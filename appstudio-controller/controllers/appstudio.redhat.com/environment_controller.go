/*
Copyright 2022.

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

package appstudioredhatcom

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"

	appstudioshared "github.com/redhat-appstudio/application-api/api/v1alpha1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// EnvironmentReconciler reconciles a Environment object
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Managed Environment secret label is added to the secrets created by the Environment controller.
	// It is used to identify the Environment that is associated with the secret.
	// #nosec G101
	managedEnvironmentSecretLabel = "appstudio.openshift.io/environment-secret"
)

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments/finalizers,verbs=update
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentmanagedenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentmanagedenvironments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;delete;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops).
		WithValues("request", req)

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	// If the Namespace is in the process of being deleted, don't handle any additional requests.
	if isNamespaceBeingDeleted, err := isRequestNamespaceBeingDeleted(ctx, req.Namespace,
		rClient, log); isNamespaceBeingDeleted || err != nil {
		return ctrl.Result{}, err
	}

	// The goal of this function is to ensure that if an Environment exists, and that Environment
	// has the 'kubernetesCredentials' field defined, that a corresponding
	// GitOpsDeploymentManagedEnvironment exists (and is up-to-date).
	environment := &appstudioshared.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	if err := rClient.Get(ctx, client.ObjectKeyFromObject(environment), environment); err != nil {

		if !apierr.IsNotFound(err) {
			// If a generic error occurred, return it
			log.Error(err, "unable to retrieve Environment resource")
			return ctrl.Result{}, fmt.Errorf("unable to retrieve Environment resource: %v", err)
		}

		// If the Environment resource no longer exists...

		gitOpsDeplManagedEnv := generateEmptyManagedEnvironment(environment.Name, environment.Namespace)

		// A) The Environment resource could not be found: As the environment resource no longer exists, the
		// corresponding GitOpsDeploymentManagedEnvironment should be deleted.
		if err := rClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeplManagedEnv), &gitOpsDeplManagedEnv); err != nil {

			if apierr.IsNotFound(err) {
				// The GitOpsDeploymentManagedEnvironment no longer exists, so no more work to do
				return ctrl.Result{}, nil
			}

			log.Error(err, "unable to retrieve GitOpsDeploymentManagedEnvironment")
			return ctrl.Result{}, fmt.Errorf("unable to retrieve GitOpsDeploymentManagedEnvironment: %v", err)
		}

		// The GitOpsDeploymentManagedEnvironment exists, so delete it....
		if err := rClient.Delete(ctx, &gitOpsDeplManagedEnv); err != nil {

			if !apierr.IsNotFound(err) {
				log.Error(err, "Unable to delete GitOpsDeploymentManagedEnvironment")
				return ctrl.Result{}, fmt.Errorf("unable to delete GitOpsDeploymentMangedEnvironment resource: %v", err)
			}

			// Otherwise, our work is done, as it no longer exists.
			return ctrl.Result{}, nil
		}

		logutil.LogAPIResourceChangeEvent(gitOpsDeplManagedEnv.Namespace, gitOpsDeplManagedEnv.Name, gitOpsDeplManagedEnv, logutil.ResourceDeleted, log)

		log.Info("The GitOpsDeploymentManagedEnvironment corresponding to the Environment resource has been deleted.")

		return ctrl.Result{}, nil

	}

	// Reconcile the Environment into a ManagedEnvironment, based on Environment CR contents
	// - 'res' and 'err' work as normal for a Reconcile function
	// - 'statusUpdated' is true if an error occurred during reconciliation, which caused the .status.Conditions[EnvironmentConditionErrorOccurred] field to be set. It is false otherwise.
	res, err, statusUpdated := reconcileEnvironment(ctx, *environment, rClient, log)

	// This allows us to tell the difference between two different cases:
	// - User input error: API consumer specified an invalid value to our API, such as a missing Secret. We should not reconcile in this case.
	// - Generic error: Any other generic error where it is not related to user input. We SHOULD re-reconcile in these cases.
	// This allows us to avoid constantly reconciling on known user input errors.

	if err == nil && !statusUpdated {
		// If no error occurred, and the .status.Conditions[EnvironmentConditionErrorOccurred] was not set, then reset
		// the condition as resolved.

		// Re-retrieve the Environment contents
		if err := rClient.Get(ctx, client.ObjectKeyFromObject(environment), environment); err != nil {

			if apierr.IsNotFound(err) {
				log.Info("Environment resource no longer exists")
				// A) The Environment resource could not be found: the owner reference on the GitOpsDeploymentManagedEnvironment
				// should ensure that it is cleaned up, so no more work is required.
				return ctrl.Result{}, nil
			}

			return ctrl.Result{}, fmt.Errorf("unable to retrieve Environment: %v", err)
		}

		if err := updateConditionErrorAsResolved(ctx, rClient, "", environment, EnvironmentConditionErrorOccurred, metav1.ConditionFalse, EnvironmentReasonErrorOccurred, log); err != nil {
			return ctrl.Result{}, err
		}
	}

	return res, err
}

// reconcileEnvironment reconciles the Environment into a ManagedEnvironment, based on Environment CR contents
// - 'res' and 'err' work as normal for a Reconcile function
// - 'statusUpdated' is true if an error occurred during reconciliation, which caused the .status.Conditions[EnvironmentConditionErrorOccurred] field to be set. It is false otherwise.
func reconcileEnvironment(ctx context.Context, environment appstudioshared.Environment, rClient client.Client, log logr.Logger) (ctrl.Result, error, bool) {

	// Utility constants: true if .status.Conditions[EnvironmentConditionErrorOccurred] was set, false otherwise.
	const (
		errorConditionSet_true  = true
		errorConditionSet_false = false
	)

	if environment.GetDeploymentTargetClaimName() != "" && environment.Spec.UnstableConfigurationFields != nil {
		log.Error(nil, "Environment is invalid since it cannot have both DeploymentTargetClaim and credentials configuration set")

		// Update Status.Conditions field of Environment.
		if err := updateStatusConditionOfEnvironment(ctx, rClient,
			"Environment is invalid since it cannot have both DeploymentTargetClaim and credentials configuration set", &environment,
			EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

			return ctrl.Result{}, fmt.Errorf("unable to update environment status condition. %v", err), errorConditionSet_false

		}
		return ctrl.Result{}, nil, errorConditionSet_true
	}

	// generateDesiredResource will return two types of error:
	// - err != nil: a serious error that should be re-reconciled
	// - err == nil && semanticErrOccurred_dontContinue = true - a error in user input; this does not require re-reconciliation, so we can just exit without requeing
	desiredManagedEnv, semanticErrOccurred_dontContinue, err := generateDesiredResource(ctx, environment, rClient, log)

	// A serious error occurred, then reconcile
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to generate expected GitOpsDeploymentManagedEnvironment resource: %v", err), errorConditionSet_true

	} else if semanticErrOccurred_dontContinue {
		// If a user error occurred, but reconciling will not fix it, then we should not re-reconcile: we just exit without continuing
		return ctrl.Result{}, nil, errorConditionSet_true
	}

	if desiredManagedEnv == nil {
		return ctrl.Result{}, nil, errorConditionSet_false
	}

	currentManagedEnv := generateEmptyManagedEnvironment(environment.Name, environment.Namespace)
	if err := rClient.Get(ctx, client.ObjectKeyFromObject(&currentManagedEnv), &currentManagedEnv); err != nil {

		if apierr.IsNotFound(err) {
			// B) The GitOpsDeploymentManagedEnvironment doesn't exist, so needs to be created.

			log.Info("Creating GitOpsDeploymentManagedEnvironment", "managedEnv", desiredManagedEnv.Name)
			if err := rClient.Create(ctx, desiredManagedEnv); err != nil {
				return ctrl.Result{}, fmt.Errorf("unable to create new GitOpsDeploymentManagedEnvironment: %v", err), errorConditionSet_false
			}
			logutil.LogAPIResourceChangeEvent(desiredManagedEnv.Namespace, desiredManagedEnv.Name, desiredManagedEnv, logutil.ResourceCreated, log)

			// Success: the resource has been created.
			return ctrl.Result{}, nil, errorConditionSet_false

		} else {
			// For any other error, return it
			return ctrl.Result{}, fmt.Errorf("unable to retrieve existing GitOpsDeploymentManagedEnvironment '%s': %v",
				currentManagedEnv.Name, err), errorConditionSet_false
		}
	}

	// C) The GitOpsDeploymentManagedEnvironment already exists, so compare it with the desired state, and update it if different.
	if reflect.DeepEqual(currentManagedEnv.Spec, desiredManagedEnv.Spec) {

		// If the spec field is the same, no more work is needed.
		return ctrl.Result{}, nil, errorConditionSet_false
	}

	log.Info("Updating GitOpsDeploymentManagedEnvironment as a change was detected", "managedEnv", desiredManagedEnv.Name)

	// Update the current object to the desired state
	currentManagedEnv.Spec = desiredManagedEnv.Spec

	if err := rClient.Update(ctx, &currentManagedEnv); err != nil {
		return ctrl.Result{},
			fmt.Errorf("unable to update existing GitOpsDeploymentManagedEnvironment '%s': %v", currentManagedEnv.Name, err), errorConditionSet_false
	}
	logutil.LogAPIResourceChangeEvent(currentManagedEnv.Namespace, currentManagedEnv.Name, currentManagedEnv, logutil.ResourceModified, log)

	return ctrl.Result{}, nil, errorConditionSet_false
}

const (
	SnapshotEnvironmentBindingConditionErrorOccurred = "ErrorOccurred"
	SnapshotEnvironmentBindingReasonErrorOccurred    = "ErrorOccurred"
	EnvironmentConditionErrorOccurred                = "ErrorOccurred"
	EnvironmentReasonErrorOccurred                   = "ErrorOccurred"
)

// Update .status.conditions field of Environment
func updateStatusConditionOfEnvironment(ctx context.Context, client client.Client, message string,
	environment *appstudioshared.Environment, conditionType string,
	status metav1.ConditionStatus, reason string, log logr.Logger) error {

	newCondition := metav1.Condition{
		Type:    conditionType,
		Message: message,
		Status:  status,
		Reason:  reason,
	}

	changed, newConditions := insertOrUpdateConditionsInSlice(newCondition, environment.Status.Conditions)

	if changed {
		environment.Status.Conditions = newConditions

		if err := client.Status().Update(ctx, environment); err != nil {
			log.Error(err, "unable to update environment status condition.")
			return err
		}
	}

	return nil

}

func updateConditionErrorAsResolved(ctx context.Context, client client.Client, message string,
	environment *appstudioshared.Environment, conditionType string,
	status metav1.ConditionStatus, reason string, log logr.Logger) error {

	cond, present := findCondition(environment.Status.Conditions, EnvironmentConditionErrorOccurred)

	if !present {
		return nil
	}

	reason = reason + "Resolved"

	// Check the condition and mark it as resolved, if it's resolved
	if cond.Reason != reason {
		if err := updateStatusConditionOfEnvironment(ctx, client,
			"", environment, EnvironmentConditionErrorOccurred, metav1.ConditionFalse, reason, log); err != nil {
			return fmt.Errorf("unable to update status of Environment: %v", err)
		}
	}

	return nil
}

// findCondition finds the suitable Condition object by looking into the conditions list and returns true if already exists
// but, if none exists, it appends one and returns false
func findCondition(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for i, condition := range conditions {
		if condition.Type == conditionType {
			return conditions[i], true
		}
	}

	return metav1.Condition{}, false
}

// generateDesiredResource will return two types of error:
// - semanticErrOccurred_dontContinue = true - a error in user input; this does not require re-reconcilition
// - err != nil - any other error which does require reconciliation
func generateDesiredResource(ctx context.Context, env appstudioshared.Environment, k8sClient client.Client, log logr.Logger) (*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, bool, error) {

	// Utility constants: return true if .status.conditions[EnvironmentConditionErrorOccurred] was set, false otherwise.
	const (
		errorConditionSet_true  = true
		errorConditionSet_false = false
	)

	var managedEnvDetails managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec
	// If the Environment has a reference to the DeploymentTargetClaim, use the credential secret
	// from the bounded DeploymentTarget.
	claimName := env.GetDeploymentTargetClaimName()
	if claimName != "" {
		log.Info("Environment is configured with a DeploymentTargetClaim")
		dtc := &appstudioshared.DeploymentTargetClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      claimName,
				Namespace: env.Namespace,
			},
		}

		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(dtc), dtc); err != nil {
			if apierr.IsNotFound(err) {
				log.Error(err, "DeploymentTargetClaim not found while generating the desired Environment resource", "expectedDTC", dtc)

				// Update Status.Conditions field of Environment.
				if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
					"DeploymentTargetClaim not found while generating the desired Environment resource", &env,
					EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

					return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
				}

				return nil, errorConditionSet_true, nil
			}

			// Update Status.Conditions field of Environment.
			if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
				"Unable to find DeploymentTarget for DeploymentTargetClaim", &env,
				EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

				return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
			}

			return nil, errorConditionSet_true, err
		}

		// If the DeploymentTargetClaim is not in bounded phase, return and wait
		// until it reaches bounded phase.
		if dtc.Status.Phase != appstudioshared.DeploymentTargetClaimPhase_Bound {
			log.Info("Waiting until the DeploymentTargetClaim associated with Environment reaches Bounded phase", "DeploymentTargetClaim", dtc.Name)
			return nil, errorConditionSet_false, nil
		}

		// If the DeploymentTargetClaim is bounded, find the corresponding DeploymentTarget.
		dt, err := getDTBoundByDTC(ctx, k8sClient, *dtc)
		if err != nil {
			if apierr.IsNotFound(err) {
				log.Error(err, "DeploymentTargetClaim references a DeploymentTarget that does not exist", "DeploymentTargetClaim", dtc.Name)

				// Update Status.Conditions field of Environment.
				if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
					"DeploymentTargetClaim references a DeploymentTarget that does not exist", &env,
					EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

					return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
				}

				return nil, errorConditionSet_true, nil
			}

			// Update Status.Conditions field of Environment.
			if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
				"Unable to find the DeploymentTarget for DeploymentTargetClaim", &env,
				EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

				return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
			}

			// A generic error occurred
			return nil, errorConditionSet_true, err
		}

		if dt == nil {
			log.Error(nil, "DeploymentTarget not found for DeploymentTargetClaim", "DeploymentTargetClaim", dtc.Name)

			// Update Status.Conditions field of Environment.
			if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
				"DeploymentTarget not found for DeploymentTargetClaim", &env,
				EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

				return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
			}

			return nil, errorConditionSet_true, nil
		}

		var namespacesField []string

		if dt.Spec.KubernetesClusterCredentials.DefaultNamespace != "" {
			namespacesField = append(namespacesField, dt.Spec.KubernetesClusterCredentials.DefaultNamespace)
		}

		log.Info("Using the cluster credentials from the DeploymentTarget", "DeploymentTarget", dt.Name)
		managedEnvDetails = managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
			APIURL:                     dt.Spec.KubernetesClusterCredentials.APIURL,
			ClusterCredentialsSecret:   dt.Spec.KubernetesClusterCredentials.ClusterCredentialsSecret,
			AllowInsecureSkipTLSVerify: dt.Spec.KubernetesClusterCredentials.AllowInsecureSkipTLSVerify,
			Namespaces:                 namespacesField,
		}

	} else if env.Spec.UnstableConfigurationFields != nil {
		log.Info("Using the cluster credentials specified in the Environment")
		managedEnvDetails = managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
			APIURL:                     env.Spec.UnstableConfigurationFields.KubernetesClusterCredentials.APIURL,
			ClusterCredentialsSecret:   env.Spec.UnstableConfigurationFields.ClusterCredentialsSecret,
			AllowInsecureSkipTLSVerify: env.Spec.UnstableConfigurationFields.KubernetesClusterCredentials.AllowInsecureSkipTLSVerify,
		}
	} else {
		// Don't process the Environment configuration fields if they are empty:
		// - This is not an error, this just means the Environment is deploying to the same namespace that the Environment CR itself is defined in. In this case, we don't need a ManagedEnvironment, and thus our work is done.
		log.Info("Environment neither has cluster credentials nor DeploymentTargetClaim configured")
		return nil, errorConditionSet_false, nil
	}

	if env.Spec.UnstableConfigurationFields != nil {
		managedEnvDetails.ClusterResources = env.Spec.UnstableConfigurationFields.ClusterResources

		// Make a copy of the Environment's namespaces field
		size := len(env.Spec.UnstableConfigurationFields.Namespaces)
		managedEnvDetails.Namespaces = append(make([]string, 0, size), env.Spec.UnstableConfigurationFields.Namespaces...)
	}

	// 1) Retrieve the secret that the Environment is pointing to
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedEnvDetails.ClusterCredentialsSecret,
			Namespace: env.Namespace,
		},
	}

	managedEnvSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateManagedEnvSecretName(env.Name),
			Namespace: secret.Namespace,
			Labels: map[string]string{
				managedEnvironmentSecretLabel: env.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         env.APIVersion,
					Kind:               env.Kind,
					Name:               env.Name,
					UID:                env.UID,
					BlockOwnerDeletion: pointer.Bool(true),
					Controller:         pointer.Bool(true),
				},
			},
		},
		Type: sharedutil.ManagedEnvironmentSecretType,
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		if apierr.IsNotFound(err) {

			log.Info("the secret referenced by the Environment resource was not found:", "secretName", secret.Name)

			// Update Status.Conditions field of Environment.
			if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
				"the secret "+secret.Name+" referenced by the Environment resource was not found", &env,
				EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

				return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
			}

			// Delete the managed Environment secret if the orginal secret is not found.
			if err := k8sClient.Delete(ctx, &managedEnvSecret); err != nil {
				if !apierr.IsNotFound(err) {
					log.Error(err, "unexpected error occurred on deleting the managedenv secret")
					return nil, errorConditionSet_false, fmt.Errorf("unable to delete the secret for managed Environment: %s", env.Name)
				}

				// The managed env secret also doesn't exist, so exit
				return nil, errorConditionSet_true, nil
			}

			logutil.LogAPIResourceChangeEvent(managedEnvSecret.Namespace, managedEnvSecret.Name, managedEnvSecret, logutil.ResourceDeleted, log)

			return nil, errorConditionSet_true, nil
		}

		// Update Status.Conditions field of Environment.
		if err := updateStatusConditionOfEnvironment(ctx, k8sClient,
			"Secret referenced by the Environment resource was not found", &env,
			EnvironmentConditionErrorOccurred, metav1.ConditionTrue, EnvironmentReasonErrorOccurred, log); err != nil {

			return nil, errorConditionSet_false, fmt.Errorf("unable to update environment status condition. %v", err)
		}

		return nil, errorConditionSet_true, err
	}

	managedEnv := generateEmptyManagedEnvironment(env.Name, env.Namespace)

	// We only want to reconcile managed environment secrets for secrets coming from SpaceRequest.
	// Skip reconciling if the secret is already of type ManagedEnvironment.
	if claimName != "" && secret.Type != sharedutil.ManagedEnvironmentSecretType {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&managedEnvSecret), &managedEnvSecret); err != nil {
			if !apierr.IsNotFound(err) {
				return nil, errorConditionSet_false, fmt.Errorf("failed to fetch the secret %s for managed Environment %s: %v", managedEnvSecret.Name, managedEnv.Name, err)
			}

			// Create a new managed environment secret if it is not found
			managedEnvSecret.Data = secret.Data
			if err := k8sClient.Create(ctx, &managedEnvSecret); err != nil {
				return nil, errorConditionSet_false, fmt.Errorf("failed to create a secret for managed Environment %s: %v", managedEnv.Name, err)
			}

			logutil.LogAPIResourceChangeEvent(managedEnvSecret.Namespace, managedEnvSecret.Name, managedEnvSecret, logutil.ResourceCreated, log)
		} else {
			// The managed Environment secret is found. Compare it with the original secret and update if required.
			if !reflect.DeepEqual(secret.Data, managedEnvSecret.Data) {
				managedEnvSecret.Data = secret.Data
				if err := k8sClient.Update(ctx, &managedEnvSecret); err != nil {
					return nil, errorConditionSet_false, fmt.Errorf("failed to update the secret for managed Environment %s: %v", managedEnv.Name, err)
				}

				logutil.LogAPIResourceChangeEvent(managedEnvSecret.Namespace, managedEnvSecret.Name, managedEnvSecret, logutil.ResourceModified, log)
			}
		}
		managedEnvDetails.ClusterCredentialsSecret = managedEnvSecret.Name
	}

	// 2) Generate (but don't apply) the corresponding GitOpsDeploymentManagedEnvironment resource
	managedEnv.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: managedgitopsv1alpha1.GroupVersion.Group + "/" + managedgitopsv1alpha1.GroupVersion.Version,
			Kind:       "Environment",
			Name:       env.Name,
			UID:        env.UID,
		},
	}
	managedEnv.Spec = managedEnvDetails

	return &managedEnv, errorConditionSet_false, nil
}

func generateManagedEnvSecretName(envName string) string {
	return fmt.Sprintf("managed-environment-secret-%s", envName)
}

func generateEmptyManagedEnvironment(environmentName string, environmentNamespace string) managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment {
	res := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "managed-environment-" + environmentName,
			Namespace: environmentNamespace,
		},
	}
	return res
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appstudioshared.Environment{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForSecret),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}), // secret doesn't have a generation, so we use RV
		).
		Watches(
			&appstudioshared.DeploymentTargetClaim{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForDeploymentTargetClaim),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&appstudioshared.DeploymentTarget{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForDeploymentTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForGitOpsDeploymentManagedEnvironment),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Complete(r)
}

// findObjectsForGitOpsDeploymentManagedEnvironment maps an incoming GitOpsDeploymentManagedEnvironment event to the
// corresponding Environment request.
func (r *EnvironmentReconciler) findObjectsForGitOpsDeploymentManagedEnvironment(ctx context.Context, managedEnv client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	managedEnv, ok := managedEnv.(*managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the Environment mapping function, expected a GitOpsDeploymentManagedEnvironment")
		return []reconcile.Request{}
	}

	envRequests := []reconcile.Request{}

	// Only queue based on ManagedEnvironments that have an ownerref to an appstudio Environment
	for _, ownerRef := range managedEnv.GetOwnerReferences() {
		if ownerRef.Kind == "Environment" &&
			ownerRef.APIVersion == managedgitopsv1alpha1.GroupVersion.Group+"/"+managedgitopsv1alpha1.GroupVersion.Version {

			envRequests = append(envRequests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: managedEnv.GetNamespace(), Name: ownerRef.Name},
			})
		}
	}

	return envRequests

}

// findObjectsForDeploymentTargetClaim maps an incoming DTC event to the corresponding Environment request.
func (r *EnvironmentReconciler) findObjectsForDeploymentTargetClaim(ctx context.Context, dtc client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtc, ok := dtc.(*appstudioshared.DeploymentTargetClaim)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the Environment mapping function, expected a DeploymentTargetClaim")
		return []reconcile.Request{}
	}

	envList := &appstudioshared.EnvironmentList{}
	if err := r.Client.List(context.Background(), envList, &client.ListOptions{Namespace: dtc.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list Environments in the Environment mapping function")
		return []reconcile.Request{}
	}

	envRequests := []reconcile.Request{}
	for i := 0; i < len(envList.Items); i++ {
		env := envList.Items[i]
		if env.GetDeploymentTargetClaimName() == dtc.GetName() {
			envRequests = append(envRequests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&env),
			})
		}
	}
	return envRequests
}

// findObjectsForDeploymentTarget maps an incoming DT event to the corresponding Environment request.
// We should reconcile Environments if the DT credentials get updated.
func (r *EnvironmentReconciler) findObjectsForDeploymentTarget(ctx context.Context, dt client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtObj, ok := dt.(*appstudioshared.DeploymentTarget)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the Environment mapping function, expected a DeploymentTarget")
		return []reconcile.Request{}
	}

	// 1. Find all DeploymentTargetClaims that are associated with this DeploymentTarget.
	dtcList := appstudioshared.DeploymentTargetClaimList{}
	err := r.List(ctx, &dtcList, &client.ListOptions{Namespace: dt.GetNamespace()})
	if err != nil {
		handlerLog.Error(err, "failed to list DeploymentTargetClaims in the mapping function")
		return []reconcile.Request{}
	}

	dtcs := []appstudioshared.DeploymentTargetClaim{}
	for _, d := range dtcList.Items {
		dtc := d
		// We only want to reconcile for DTs that have a corresponding DTC.
		if dtc.Spec.TargetName == dt.GetName() || dtObj.Spec.ClaimRef == dtc.Name {
			dtcs = append(dtcs, dtc)
		}
	}

	// 2. Find all Environments that are associated with this DeploymentTargetClaim.
	envList := &appstudioshared.EnvironmentList{}
	err = r.Client.List(context.Background(), envList, &client.ListOptions{Namespace: dt.GetNamespace()})
	if err != nil {
		handlerLog.Error(err, "failed to list Environments in the Environment mapping function")
		return []reconcile.Request{}
	}

	envRequests := []reconcile.Request{}
	for i := 0; i < len(envList.Items); i++ {
		env := envList.Items[i]
		for _, dtc := range dtcs {
			if env.GetDeploymentTargetClaimName() == dtc.GetName() {
				envRequests = append(envRequests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&env),
				})
			}
		}
	}

	return envRequests
}

// findObjectsForSecret finds all the Environment objects that are using this incoming secret.
// There are two types of secrets that we want to reconcile:
// 1. Secret created by the SpaceRequest controller
// 2. Secret created for the managed Environment
func (r *EnvironmentReconciler) findObjectsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	secretObj, ok := secret.(*corev1.Secret)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the Environment mapping function, expected a Secret")
		return []reconcile.Request{}
	}

	// Filter secrets to avoid unnecessary API calls on them.
	if secretObj.Type != corev1.SecretTypeOpaque && secretObj.Type != sharedutil.ManagedEnvironmentSecretType {
		return []reconcile.Request{}
	}

	// Check if the secret is created by the Environment controller
	if secretObj.Type == sharedutil.ManagedEnvironmentSecretType {
		envName := secretObj.GetLabels()[managedEnvironmentSecretLabel]
		if envName != "" {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      envName,
						Namespace: secret.GetNamespace(),
					},
				},
			}
		}
		return []reconcile.Request{}
	}

	// If the secret is created by the SpaceRequest controller, find the corresponding Environment.
	envList := &appstudioshared.EnvironmentList{}
	err := r.Client.List(context.Background(), envList, &client.ListOptions{Namespace: secret.GetNamespace()})
	if err != nil {
		handlerLog.Error(err, "failed to list Environments in the Environment mapping function")
		return []reconcile.Request{}
	}

	dtList := appstudioshared.DeploymentTargetList{}
	err = r.Client.List(ctx, &dtList, &client.ListOptions{Namespace: secret.GetNamespace()})
	if err != nil {
		handlerLog.Error(err, "failed to list DeploymentTargets in the mapping function")
		return []reconcile.Request{}
	}

	envRequests := []reconcile.Request{}
	for i := 0; i < len(envList.Items); i++ {
		env := envList.Items[i]

		// 1. Find the DTC that is associated with the Environment
		dtcName := env.GetDeploymentTargetClaimName()
		if dtcName == "" {
			continue
		}

		dtc := appstudioshared.DeploymentTargetClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dtcName,
				Namespace: env.Namespace,
			},
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc); err != nil {
			handlerLog.Error(err, "failed to get the DeploymentTargetClaim in the Environment mapping function")
			return []reconcile.Request{}
		}

		// 2. Find the corresponding DT for the DTC
		dt := appstudioshared.DeploymentTarget{}
		for _, d := range dtList.Items {
			if dtc.Spec.TargetName == d.Name || d.Spec.ClaimRef == dtc.Name {
				dt = d
				break
			}
		}

		// 3. We only want to reconcile for secrets that are part of the DT configured for a given Environment.
		if dt.Spec.KubernetesClusterCredentials.ClusterCredentialsSecret == secret.GetName() {
			envRequests = append(envRequests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&env),
			})
		}
	}

	return envRequests
}
