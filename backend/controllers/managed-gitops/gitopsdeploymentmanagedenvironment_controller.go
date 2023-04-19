/*
Copyright 2022, 2023

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/preprocess_event_loop"
)

// GitOpsDeploymentManagedEnvironmentReconciler reconciles a GitOpsDeploymentManagedEnvironment object
type GitOpsDeploymentManagedEnvironmentReconciler struct {
	client.Client
	Scheme                       *runtime.Scheme
	PreprocessEventLoopProcessor PreprocessEventLoopProcessor
}

//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentmanagedenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentmanagedenvironments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentmanagedenvironments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *GitOpsDeploymentManagedEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	_ = log.FromContext(ctx)

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	// The Reconcile function receives events for both ManagedEnv and Secrets.
	// Since the 'req' object doesn't tell us the type resource type (ManagedEnv or Secret), we need to check both cases.

	namespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Namespace,
		},
	}
	if err := rClient.Get(ctx, client.ObjectKeyFromObject(&namespace), &namespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to retrieve namespace: %v", err)
	}

	r.PreprocessEventLoopProcessor.callPreprocessEventLoopForManagedEnvironment(req, rClient, namespace)

	return ctrl.Result{}, nil
}

type PreprocessEventLoopProcessor interface {
	callPreprocessEventLoopForManagedEnvironment(requestToProcess ctrl.Request, k8sClient client.Client, namespace corev1.Namespace)
}

func NewDefaultPreProcessEventLoopProcessor(preprocessEventLoop *preprocess_event_loop.PreprocessEventLoop) PreprocessEventLoopProcessor {
	return &DefaultPreProcessEventLoopProcessor{
		PreprocessEventLoop: preprocessEventLoop,
	}
}

var _ PreprocessEventLoopProcessor = &DefaultPreProcessEventLoopProcessor{}

type DefaultPreProcessEventLoopProcessor struct {
	PreprocessEventLoop *preprocess_event_loop.PreprocessEventLoop
}

func (dppelp *DefaultPreProcessEventLoopProcessor) callPreprocessEventLoopForManagedEnvironment(requestToProcess ctrl.Request, k8sClient client.Client, namespace corev1.Namespace) {
	dppelp.PreprocessEventLoop.EventReceived(requestToProcess, eventlooptypes.GitOpsDeploymentManagedEnvironmentTypeName,
		k8sClient,
		eventlooptypes.ManagedEnvironmentModified, string(namespace.UID))
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitOpsDeploymentManagedEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Watches(&source.Kind{Type: &corev1.Secret{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
