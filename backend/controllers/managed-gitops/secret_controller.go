/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package managedgitops

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"

	apierr "k8s.io/apimachinery/pkg/api/errors"
)

// SecretReconciler reconciles a Secret object
type SecretReconciler struct {
	client.Client
	Scheme                       *runtime.Scheme
	PreprocessEventLoopProcessor PreprocessEventLoopProcessor
}

//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	if isFilteredOutNamespace(req) {
		return ctrl.Result{}, nil
	}

	_ = log.FromContext(ctx)

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	// list of managed envs that reference the Secret specified in req
	managedEnvsFound := []ctrl.Request{}

	// 1) Attempt to retrieve the request as a Secret
	secret := &corev1.Secret{}
	if err := rClient.Get(ctx, req.NamespacedName, secret); err == nil {

		// If the Secret exists, and is of the appropriate type, then find any ManagedEnvironments that reference that Secret in the Namespace

		// Ignore non-ManagedEnv secrets
		if secret.Type != sharedutil.ManagedEnvironmentSecretType {
			return ctrl.Result{}, err
		}

		// Locate any managed environments that reference this Secret, in the same Namespace
		managedEnvList, err := processSecret(ctx, *secret, rClient)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to process Secret resource of ManagedEnvironment: %v", err)
		}

		// For each ManagedEnvironment that was found that references the Secret,
		// add the ManagedEnv to the list of requests to process.
		for _, managedEnv := range managedEnvList {
			managedEnvReq := ctrl.Request{
				// ClusterName: req.ClusterName,
				NamespacedName: types.NamespacedName{
					Namespace: managedEnv.Namespace,
					Name:      managedEnv.Name,
				},
			}
			managedEnvsFound = append(managedEnvsFound, managedEnvReq)
		}
	} else if apierr.IsNotFound(err) {
		// For Secret not found, just ignore and return
		return ctrl.Result{}, nil
	} else {
		// For any other error besides 'not found', return and reconcile
		return ctrl.Result{}, err
	}

	// 2) If Secret is referenced by any ManagedEnvs, process those ManagedEnvs
	if len(managedEnvsFound) > 0 {
		namespace := corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: req.Namespace,
			},
		}
		if err := rClient.Get(ctx, client.ObjectKeyFromObject(&namespace), &namespace); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to retrieve namespace: %v", err)
		}

		for idx := range managedEnvsFound {
			requestToProcess := managedEnvsFound[idx]
			r.PreprocessEventLoopProcessor.callPreprocessEventLoopForManagedEnvironment(requestToProcess, rClient, namespace)
		}
	}

	return ctrl.Result{}, nil
}

func processSecret(ctx context.Context, secret corev1.Secret, k8sClient client.Client) ([]managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, error) {
	managedEnvList := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentList{}

	if err := k8sClient.List(ctx, &managedEnvList, &client.ListOptions{Namespace: secret.Namespace}); err != nil {
		return nil, fmt.Errorf("unable to list Managed Environment resources in namespace '%s': %v", secret.Namespace, err)
	}

	listOfManagedEnvCRsThatReferenceSecret := []managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{}

	for idx := range managedEnvList.Items {
		managedEnvCR := managedEnvList.Items[idx]

		if managedEnvCR.Namespace != secret.Namespace {
			// Sanity check that the managed environment resource is in the same namespace as the Secret
			continue
		}

		if managedEnvCR.Spec.ClusterCredentialsSecret == secret.Name {
			listOfManagedEnvCRsThatReferenceSecret = append(listOfManagedEnvCRsThatReferenceSecret, managedEnvCR)
		}
	}

	return listOfManagedEnvCRsThatReferenceSecret, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(filterManagedEnvSecrets()).
		Complete(r)
}

func filterManagedEnvSecrets() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(createEvent event.CreateEvent) bool {
			return isManagedEnvSecret(createEvent.Object)
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return isManagedEnvSecret(deleteEvent.Object)
		},
		GenericFunc: func(genericEvent event.GenericEvent) bool {
			return isManagedEnvSecret(genericEvent.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isManagedEnvSecret(e.ObjectNew)
		},
	}

}

func isManagedEnvSecret(o client.Object) bool {
	if os, ok := o.(*corev1.Secret); ok {
		return os.Type == sharedutil.ManagedEnvironmentSecretType
	}

	return false
}

// isFilteredOutNamespace filters out a set of namepaces that are known not to contain
// Secrets that are used/referenced by the ManagedEnvironment CR.
// - This is not for security purposes, but rather to reduce the number of K8s API requests when running on OpenShift clusters.
func isFilteredOutNamespace(req ctrl.Request) bool {

	ns := req.Namespace

	// Specifically allow the gitops-service-e2e namespace, which is used by E2E tests
	if ns == "gitops-service-e2e" {
		return false
	}

	if strings.HasPrefix(ns, "openshift-") || strings.HasPrefix(ns, "gitops-service-") || ns == "gitops" || ns == "default" ||
		ns == "kube-system" || ns == "kube-node-lease" {
		return true
	}

	return false

}
