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
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appstudioshared "github.com/redhat-appstudio/application-api/api/v1alpha1"
	apibackend "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/gitopserrors"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// If the 'appstudioLabelKey' string is present in a label of the SnapshotEnvironmentBinding, that label is copied to child GitOpsDeployments of the SnapshotEnvironmentBinding
	appstudioLabelKey = "appstudio.openshift.io"

	applicationLabelKey = appstudioLabelKey + "/application"
	componentLabelKey   = appstudioLabelKey + "/component"
	environmentLabelKey = appstudioLabelKey + "/environment"
)

// SnapshotEnvironmentBindingReconciler reconciles a SnapshotEnvironmentBinding object
type SnapshotEnvironmentBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeployments/finalizers,verbs=update

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings/finalizers,verbs=update
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=environments,verbs=get;list;watch;
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *SnapshotEnvironmentBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops).
		WithValues("name", req.Name, "namespace", req.Namespace, "component", "bindingReconcile")

	defer log.V(logutil.LogLevel_Debug).Info("Snapshot Environment Binding Reconcile() complete.")

	binding := &appstudioshared.SnapshotEnvironmentBinding{}

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	// If the Namespace is in the process of being deleted, don't handle any additional requests.
	if isNamespaceBeingDeleted, err := isRequestNamespaceBeingDeleted(ctx, req.Namespace,
		rClient, log); isNamespaceBeingDeleted || err != nil {
		return ctrl.Result{}, err
	}

	if err := rClient.Get(ctx, req.NamespacedName, binding); err != nil {
		// Binding doesn't exist: it was deleted.
		// Owner refs will ensure the GitOpsDeployments are deleted, so no work to do.
		return ctrl.Result{}, nil
	}

	// Make a copy of the original SnapshotEnvironmentBinding, so we can compare it with the updated value, to see
	// if our reconciliation changed the resource at all.
	originalBinding := *binding.DeepCopy()

	environment := appstudioshared.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      binding.Spec.Environment,
			Namespace: req.Namespace,
		},
	}
	if err := rClient.Get(ctx, client.ObjectKeyFromObject(&environment), &environment); err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, fmt.Errorf("unable to retrieve Environment '%s' referenced by Binding: %v", environment.Name, err)
		}
	}

	// Don't reconcile the binding if the application-service component indicated via the binding.status field
	// that there were issues with the GitOps repository, or if the GitOps repository isn't ready
	// yet.

	if len(binding.Status.GitOpsRepoConditions) > 0 {
		if binding.Status.GitOpsRepoConditions[len(binding.Status.GitOpsRepoConditions)-1].Status == metav1.ConditionFalse {
			// if the SnapshotEventBinding GitOps Repo Conditions status is false - return;
			// since there was an unexpected issue with refreshing/syncing the GitOps repository
			log.V(logutil.LogLevel_Debug).Info("Can not Reconcile Binding '" + binding.Name + "', since GitOps Repo Conditions status is false.")

			// Update .status.bindingConditions field EnvironmentBinding with the error
			if err := updateBindingConditionOfSEB(ctx, rClient,
				"Can not Reconcile Binding '"+binding.Name+"', since GitOps Repo Conditions status is false.", binding,
				SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred, log); err != nil {

				log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
				return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
			}

			return ctrl.Result{}, nil
		} else if binding.Status.GitOpsRepoConditions[len(binding.Status.GitOpsRepoConditions)-1].Status == metav1.ConditionTrue {

			// if the SnapshotEventBinding GitOps Repo Conditions status is true update the
			// binding condition status to false
			if err := updateBindingConditionOfSEB(ctx, rClient,
				"", binding,
				SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionFalse, SnapshotEnvironmentBindingReasonErrorOccurred, log); err != nil {

				log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
				return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
			}
		}

	} else if len(binding.Status.Components) == 0 {

		log.V(logutil.LogLevel_Debug).Info("SnapshotEventBinding Component status is required to " +
			"generate GitOps deployment, waiting for the Application Service controller to finish reconciling binding")

		// Update Status.Conditions field of environmentBinding.
		if err := updateBindingConditionOfSEB(ctx, rClient, "SnapshotEventBinding Component status is required to "+
			"generate GitOps deployment, waiting for the Application Service controller to finish reconciling binding '"+binding.Name+"'",
			binding, SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred, log); err != nil {

			log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
			return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
		}

		// Delete all existing deployments associated with this binding
		err := deleteUnmatchedDeployments(ctx, *binding, nil, rClient, log)
		if err != nil {
			return ctrl.Result{}, err
		}

		// if length of the Binding component status is 0 and there is no issue with the GitOps Repo Conditions;
		// the Application Service controller has not synced the GitOps repository yet, return and requeue.
		return ctrl.Result{}, nil
	}

	// map: componentName (string) -> expected GitOpsDeployment for that component name
	expectedDeployments := map[string]apibackend.GitOpsDeployment{}

	for _, component := range binding.Status.Components {

		// sanity test that there are no duplicate components by name
		if _, exists := expectedDeployments[component.Name]; exists {

			// Update Status.Conditions field of environmentBinding.
			if err := updateBindingConditionOfSEB(ctx, rClient, errDuplicateKeysFound+" in "+component.Name, binding, SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred, log); err != nil {
				log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
				return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
			}

			log.Error(nil, fmt.Sprintf("%s: %s", errDuplicateKeysFound, component.Name))
			return ctrl.Result{}, nil
		}

		var userDevErr gitopserrors.UserError
		expectedDeployments[component.Name], userDevErr = generateExpectedGitOpsDeployment(ctx, component, *binding, environment, rClient, log)

		// If an error occurred while generating the GitOpsDeployment, report the error back to the user
		if userDevErr != nil {

			if err := updateBindingConditionOfSEB(ctx, rClient, userDevErr.UserError(),
				binding, SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred, log); err != nil {

				log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
				return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
			}

			return ctrl.Result{RequeueAfter: time.Second * 10}, userDevErr.DevError()
		}
	}

	// Delete any existing deployments which don't have a matching component
	err := deleteUnmatchedDeployments(ctx, *binding, expectedDeployments, rClient, log)
	if err != nil {
		return ctrl.Result{}, err
	}

	var statusField []appstudioshared.BindingStatusGitOpsDeployment
	var allErrors error

	// For each deployment, check if it exists, and if it has the expected content.
	// - If not, create/update it.
	for componentName, expectedGitOpsDeployment := range expectedDeployments {

		if err := processExpectedGitOpsDeployment(ctx, expectedGitOpsDeployment, *binding, rClient, log); err != nil {

			errorMessage := fmt.Sprintf("error occurred while processing expected GitOpsDeployment '%s' for SnapshotEnvironmentBinding",
				expectedGitOpsDeployment.Name)
			log.Error(err, errorMessage)

			// Combine all errors that occurred in the loop
			if allErrors == nil {
				allErrors = fmt.Errorf("%s, error: %w", errorMessage, err)
			} else {
				allErrors = fmt.Errorf("%s.\n%s, error: %w", allErrors.Error(), errorMessage, err)
			}
		} else {
			// If no error, provide status
			deployment := apibackend.GitOpsDeployment{}
			newStatusEntry := appstudioshared.BindingStatusGitOpsDeployment{
				ComponentName:    componentName,
				GitOpsDeployment: expectedGitOpsDeployment.Name,
			}
			// gosec check doesn't allow taking the address of a loop variable. Here, reassign the loop variable.
			expectedGitOpsDeployment := expectedGitOpsDeployment
			if err := rClient.Get(ctx, client.ObjectKeyFromObject(&expectedGitOpsDeployment), &deployment); err == nil {
				newStatusEntry.GitOpsDeploymentSyncStatus = string(deployment.Status.Sync.Status)
				newStatusEntry.GitOpsDeploymentHealthStatus = string(deployment.Status.Health.Status)
				newStatusEntry.GitOpsDeploymentCommitID = deployment.Status.Sync.Revision
			} else {
				log.Error(err, "unable to get the deployment for "+componentName)
			}
			statusField = append(statusField, newStatusEntry)
		}
	}

	if allErrors != nil {
		return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("unable to process expected GitOpsDeployment: %w", allErrors)
	}

	// Update the status field with statusField vars (even if an error occurred)
	binding.Status.GitOpsDeployments = statusField
	if err := addComponentDeploymentCondition(ctx, binding, rClient, log); err != nil {
		log.Error(err, "unable to update component deployment condition for Binding "+binding.Name)
		return ctrl.Result{}, fmt.Errorf("unable to update component deployment condition for SnapshotEnvironmentBinding. Error: %w", err)
	}

	// If our update logic did not modify the binding at all, there is no need to all update.
	if reflect.DeepEqual(binding, originalBinding) {
		log.V(logutil.LogLevel_Debug).Info("Skipping update of SnapshotEnvironmentBinding, as the resource did not change.")
		return ctrl.Result{}, nil
	}

	log.Info("Updating SnapshotEnvironmentBinding status")
	if err := rClient.Status().Update(ctx, binding); err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to update SnapshotEnvironmentBinding status")
		return ctrl.Result{}, fmt.Errorf("unable to update SnapshotEnvironmentBinding status. Error: %w", err)
	}

	return ctrl.Result{}, nil
}

// Delete all Deployments which are associated with the given binding but are not contained in the
// given expectedDeployments map
func deleteUnmatchedDeployments(ctx context.Context, binding appstudioshared.SnapshotEnvironmentBinding, expectedDeployments map[string]apibackend.GitOpsDeployment, k8sClient client.Client, logger logr.Logger) error {

	// Find all deployments in the binding's namespace that are labeled with the
	// binding's application and environment
	appRequirement, err := labels.NewRequirement(applicationLabelKey, selection.Equals, []string{binding.Spec.Application})
	if err != nil || appRequirement == nil {
		logger.Error(err, "error creating label selector requirement", "application", binding.Spec.Application)
		return err
	}
	envRequirement, err := labels.NewRequirement(environmentLabelKey, selection.Equals, []string{binding.Spec.Environment})
	if err != nil || envRequirement == nil {
		logger.Error(err, "error creating label selector requirement", "environment", binding.Spec.Environment)
		return err
	}
	selector := labels.NewSelector().Add(*appRequirement, *envRequirement)
	deployments := apibackend.GitOpsDeploymentList{}
	options := client.ListOptions{
		Namespace:     binding.Namespace,
		LabelSelector: selector,
	}
	if err := k8sClient.List(ctx, &deployments, &options); err != nil {
		logger.Error(err, "error retrieving list of existing deployments", "application", binding.Spec.Application, "environment", binding.Spec.Environment)
		return err
	}

	// Delete all the deployments which aren't in the expectedDeployments map
	for i := range deployments.Items {
		deployment := deployments.Items[i]
		component := deployment.Labels[componentLabelKey]

		// Sanity check: we should only delete a GitOpsDeployment if it is owned by the corresponding SnapshotEnvironmentBinding via ownerref
		if len(deployment.OwnerReferences) == 0 {
			continue
		}
		ownerRefMatch := false
		for _, ownerRef := range deployment.OwnerReferences {
			if ownerRef.UID == binding.UID {
				ownerRefMatch = true
				break
			}
		}
		if !ownerRefMatch {
			continue
		}

		// We should only delete a GitOpsDeployment that is not in our expected component list
		_, exists := expectedDeployments[component]
		if !exists {

			if err := k8sClient.Delete(ctx, &deployment); err != nil {
				logger.Error(err, "error deleting deployment", "name", deployment.Name)
				return err
			}
			logger.Info("Deleted deployment which was no longer referenced by the SnapshotEnvironmentBinding", "deploymentName", deployment.Name)

			logutil.LogAPIResourceChangeEvent(deployment.Namespace, deployment.Name, deployment, logutil.ResourceDeleted, logger)

		}
	}
	return nil
}

func addComponentDeploymentCondition(ctx context.Context, binding *appstudioshared.SnapshotEnvironmentBinding, c client.Client, log logr.Logger) error {
	total := len(binding.Status.GitOpsDeployments)
	synced := 0
	for _, deploymentStatus := range binding.Status.GitOpsDeployments {
		deployment := apibackend.GitOpsDeployment{}
		err := c.Get(ctx, types.NamespacedName{Name: deploymentStatus.GitOpsDeployment, Namespace: binding.Namespace}, &deployment)
		if err != nil {
			return err
		}
		if deployment.Status.Sync.Status == apibackend.SyncStatusCodeSynced {
			synced++
		}
	}

	ctype := appstudioshared.ComponentDeploymentConditionAllComponentsDeployed
	status := metav1.ConditionFalse
	reason := appstudioshared.ComponentDeploymentConditionCommitsUnsynced
	if synced == total {
		status = metav1.ConditionTrue
		reason = appstudioshared.ComponentDeploymentConditionCommitsSynced
	}
	message := fmt.Sprintf("%d of %d components deployed", synced, total)

	if len(binding.Status.ComponentDeploymentConditions) > 1 {
		// this should never happen, log and fix it
		log.Error(nil, "snapshot environment binding has multiple component deployment conditions",
			"binding name", binding.Name, "binding namespace", binding.Namespace)
		binding.Status.ComponentDeploymentConditions = []metav1.Condition{}
	}
	if len(binding.Status.ComponentDeploymentConditions) == 0 {
		binding.Status.ComponentDeploymentConditions = append(binding.Status.ComponentDeploymentConditions, metav1.Condition{})
	}

	condition := &binding.Status.ComponentDeploymentConditions[0]
	if condition.Type != ctype || condition.Status != status || condition.Reason != reason || condition.Message != message {
		condition.Type = ctype
		condition.Status = status
		condition.Reason = reason
		condition.Message = message
		condition.LastTransitionTime = metav1.Now()
	}

	return nil
}

const (
	errDuplicateKeysFound     = "duplicate component keys found in status field"
	errMissingTargetNamespace = "TargetNamespace field of Environment was empty"
)

// processExpectedGitOpsDeployment processed the GitOpsDeployment that is expected for a particular Component
func processExpectedGitOpsDeployment(ctx context.Context, expectedGitopsDeployment apibackend.GitOpsDeployment,
	binding appstudioshared.SnapshotEnvironmentBinding, k8sClient client.Client, l logr.Logger) error {

	log := l.WithValues("binding", binding.Name, "gitOpsDeployment", expectedGitopsDeployment.Name, "bindingNamespace", binding.Namespace)

	actualGitOpsDeployment := apibackend.GitOpsDeployment{}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&expectedGitopsDeployment), &actualGitOpsDeployment); err != nil {

		// A) If the GitOpsDeployment doesn't exist, create it
		if !apierr.IsNotFound(err) {
			log.Error(err, "expectedGitopsDeployment: "+expectedGitopsDeployment.Name+" not found for Binding "+binding.Name)
			return fmt.Errorf("expectedGitopsDeployment: %s not found for Binding: %s: Error: %w", expectedGitopsDeployment.Name, binding.Name, err)
		}
		if err := k8sClient.Create(ctx, &expectedGitopsDeployment); err != nil {
			log.Error(err, "unable to create expectedGitopsDeployment: '"+expectedGitopsDeployment.Name+"' for Binding: '"+binding.Name+"'")
			return err
		}
		logutil.LogAPIResourceChangeEvent(expectedGitopsDeployment.Namespace, expectedGitopsDeployment.Name, expectedGitopsDeployment, logutil.ResourceCreated, log)

		return nil
	}

	// GitOpsDeployment already exists, so compare it with what we expect
	if reflect.DeepEqual(expectedGitopsDeployment.Spec, actualGitOpsDeployment.Spec) &&
		areAppStudioLabelsEqualBetweenMaps(expectedGitopsDeployment.ObjectMeta.Labels, actualGitOpsDeployment.ObjectMeta.Labels) {
		// B) The GitOpsDeployment is exactly as expected, so return
		return nil
	}

	// C) The GitOpsDeployment is not the same, so it should be updated to be consistent with what we expect
	actualGitOpsDeployment.Spec = expectedGitopsDeployment.Spec

	// Ensure that the appstudio labels in the GitOpsDeployment are the same as in the binding, while
	// not affecting any of the other user-added, non-appstudio labels on the GitOpDeployment
	actualGitOpsDeployment.Labels = updateMapWithExpectedAppStudioLabels(actualGitOpsDeployment.Labels, expectedGitopsDeployment.Labels)

	if err := k8sClient.Update(ctx, &actualGitOpsDeployment); err != nil {
		log.Error(err, "unable to update actualGitOpsDeployment: "+actualGitOpsDeployment.Name+" for Binding: "+binding.Name)
		return fmt.Errorf("unable to update actualGitOpsDeployment '%s', for Binding:%s, Error: %w", actualGitOpsDeployment.Name, binding.Name, err)
	}
	logutil.LogAPIResourceChangeEvent(expectedGitopsDeployment.Namespace, expectedGitopsDeployment.Name, expectedGitopsDeployment, logutil.ResourceModified, log)

	return nil
}

// GenerateBindingGitOpsDeploymentName generates the name that will be used for a given GitOpsDeployment of a binding
func GenerateBindingGitOpsDeploymentName(binding appstudioshared.SnapshotEnvironmentBinding, componentName string) string {

	expectedName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + componentName

	// The application name, environment name and component name are each limited to be at most 63 characters.
	// If the length of the GitOpsDeployment exceeds the K8s maximum, shorten it to just binding+component
	if len(expectedName) > 250 {
		expectedShortName := binding.Name + "-" + componentName

		// If the length is still > 250
		if len(expectedShortName) > 250 {
			hashValue := sha256.Sum256([]byte(expectedName))
			hashString := fmt.Sprintf("%x", hashValue)
			return expectedShortName[0:180] + "-" + hashString
		}
		return expectedShortName
	}

	return expectedName

}

func generateExpectedGitOpsDeployment(ctx context.Context, component appstudioshared.BindingComponentStatus,
	binding appstudioshared.SnapshotEnvironmentBinding,
	environment appstudioshared.Environment,
	k8sClient client.Client, logger logr.Logger) (apibackend.GitOpsDeployment, gitopserrors.UserError) {

	res := apibackend.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateBindingGitOpsDeploymentName(binding, component.Name),
			Namespace: binding.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         binding.APIVersion,
					Kind:               binding.Kind,
					Name:               binding.Name,
					UID:                binding.UID,
					BlockOwnerDeletion: pointer.Bool(true),
					Controller:         pointer.Bool(true),
				},
			},
		},
		Spec: apibackend.GitOpsDeploymentSpec{
			Source: apibackend.ApplicationSource{
				RepoURL:        component.GitOpsRepository.URL,
				Path:           component.GitOpsRepository.Path,
				TargetRevision: component.GitOpsRepository.Branch,
			},
			Type:        apibackend.GitOpsDeploymentSpecType_Automated, // Default to automated, for now
			Destination: apibackend.ApplicationDestination{},           // Default to same namespace, for now
		},
	}

	// If the Environment references a DeploymentTargetClaim...
	if environment.Spec.Configuration.Target.DeploymentTargetClaim.ClaimName != "" {

		// Retrieve the DTC
		dtc := appstudioshared.DeploymentTargetClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      environment.Spec.Configuration.Target.DeploymentTargetClaim.ClaimName,
				Namespace: environment.Namespace,
			},
		}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc); err != nil {

			devErr := fmt.Errorf("error on retrieving DeploymentTargetClaim '%s': %v", dtc.Name, err)

			if apierr.IsNotFound(err) {
				return apibackend.GitOpsDeployment{}, gitopserrors.NewUserDevError("DeploymentTargetClaim referenced by Environment does not exist.", devErr)
			} else {
				return apibackend.GitOpsDeployment{}, gitopserrors.NewDevOnlyError(devErr)
			}
		}

		// Locate the corresponding DT, so we can pull the credentials from the DT
		dt, err := getDTBoundByDTC(ctx, k8sClient, dtc)
		if dt == nil || err != nil {
			devErr := fmt.Errorf("unable to locate DeploymentTarget of DeploymentTargetClaim '%s'. %v", dtc.Name, err)

			return apibackend.GitOpsDeployment{}, gitopserrors.NewUserDevError("unable to locate DeploymentTarget the references DeploymentTargetClaim", devErr)
		}

		// Update the .spec.destination field of the GitOpsDeployment to point to the managed environment and Namespace from the DT
		managedEnvironmentName := generateEmptyManagedEnvironment(environment.Name, environment.Namespace).Name

		res.Spec.Destination = apibackend.ApplicationDestination{
			Environment: managedEnvironmentName,
			Namespace:   dt.Spec.KubernetesClusterCredentials.DefaultNamespace, // Note: this might be empty
		}

	} else if environment.Spec.UnstableConfigurationFields != nil {
		// If the environment has a target cluster field defined, then set the destination to that managed environment

		if environment.Spec.UnstableConfigurationFields.TargetNamespace == "" {
			devErr := fmt.Errorf("invalid target namespace: %s: '%s'", errMissingTargetNamespace, environment.Name)
			return apibackend.GitOpsDeployment{}, gitopserrors.NewUserDevError("Environment is missing a TargetNamespace field", devErr)
		}

		managedEnvironmentName := generateEmptyManagedEnvironment(environment.Name, environment.Namespace).Name

		res.Spec.Destination = apibackend.ApplicationDestination{
			Environment: managedEnvironmentName,
			Namespace:   environment.Spec.UnstableConfigurationFields.TargetNamespace,
		}
	}
	// Else if neither of the above is true, it's necessarily just an Environment with no credentials specified,
	// which means we should just deploy to the same Namespace as the Environment CR itself.

	res.ObjectMeta.Labels = make(map[string]string)

	// Append ASEB labels with key "appstudio.openshift.io" to the gitopsDeployment labels
	for bindingKey, bindingLabelValue := range binding.Labels {
		if strings.Contains(bindingKey, appstudioLabelKey) {
			res.ObjectMeta.Labels[bindingKey] = bindingLabelValue
		}
	}

	// Append labels to identify the Application, Component and Environment associated with this GitOpsDeployment.
	// Label values are limited to a maximum of 63 characters, while resource names by default can be up to 253.
	// AppStudio has webhooks to artificially limits these names to 63 characters, but to be safe we
	// error out if they are too long.
	if err := setLabel(&res, applicationLabelKey, binding.Spec.Application); err != nil {
		return apibackend.GitOpsDeployment{}, gitopserrors.NewDevOnlyError(err)
	}
	if err := setLabel(&res, componentLabelKey, component.Name); err != nil {
		return apibackend.GitOpsDeployment{}, gitopserrors.NewDevOnlyError(err)
	}
	if err := setLabel(&res, environmentLabelKey, binding.Spec.Environment); err != nil {
		return apibackend.GitOpsDeployment{}, gitopserrors.NewDevOnlyError(err)
	}

	// Ensures that this method only adds 'appstudio.openshift.io' labels
	// - Note: If you remove this line, you need to search for other uses of 'removeNonAppStudioLabelsFromMap' in the
	// code, as you may break the logic here.
	removeNonAppStudioLabelsFromMap(res.ObjectMeta.Labels)

	res.ObjectMeta.Labels = convertToNilIfEmptyMap(res.ObjectMeta.Labels)

	return res, nil
}

// Sets the given label on the given GitopsDeployment.  Returns an error if the length of the label value
// is greater than the limit of 63 characters, else returns nil
func setLabel(deployment *apibackend.GitOpsDeployment, key, value string) error {
	if len(value) > 63 {
		err := fmt.Errorf("unable to set label %s on deployment %s: value '%s' for label is longer than 63 characters", key, deployment.Name, value)
		return err
	}
	deployment.ObjectMeta.Labels[key] = value
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SnapshotEnvironmentBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appstudioshared.SnapshotEnvironmentBinding{}).
		Owns(&apibackend.GitOpsDeployment{}).
		Watches(
			&appstudioshared.Environment{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForEnvironment),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&appstudioshared.DeploymentTarget{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForDeploymentTarget),
			// When/if we start using DT's .status field, switch this ResourceVersionChangedPredicate:
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&appstudioshared.DeploymentTargetClaim{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForDeploymentTargetClaim),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).Complete(r)
}

// findObjectsForDeploymentTarget maps an incoming DT event to the corresponding Environment request.
// We should reconcile Environments if the DT credentials get updated.
func (r *SnapshotEnvironmentBindingReconciler) findObjectsForDeploymentTargetClaim(ctx context.Context, dtc client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtcObj, ok := dtc.(*appstudioshared.DeploymentTargetClaim)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the SEB DeploymentTargetClaim mapping function, expected a DeploymentTargetClaim")
		return []reconcile.Request{}
	}

	// 1. Find all Environments that are associated with this DeploymentTargetClaim.
	envList := &appstudioshared.EnvironmentList{}
	if err := r.Client.List(context.Background(), envList, &client.ListOptions{Namespace: dtcObj.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list Environments in the SEB DeploymentTargetClaim mapping function")
		return []reconcile.Request{}
	}

	environmentByName := map[string]bool{} // The keys of this map contain Environments that are referenced by the DTC (which references the DTC)
	for _, env := range envList.Items {

		// If the DTCs match the Environment's claim name, it's a match
		if env.Spec.Configuration.Target.DeploymentTargetClaim.ClaimName == dtcObj.Name {
			environmentByName[env.Name] = true
		}
	}

	// 2. Find SnapshotEnvironmentBindings in the namespace that match one of the Enviroments inside the 'environmentByName' map
	snapshotEnvList := &appstudioshared.SnapshotEnvironmentBindingList{}
	if err := r.Client.List(context.Background(), snapshotEnvList, &client.ListOptions{Namespace: dtcObj.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list SnapshotEnvironmentBindings in the SEB DeploymentTargetClaim mapping function")
		return []reconcile.Request{}
	}
	snapshotRequests := []reconcile.Request{}
	for index := range snapshotEnvList.Items {
		snapshotEnvBinding := snapshotEnvList.Items[index]

		if _, exists := environmentByName[snapshotEnvBinding.Spec.Environment]; exists {
			snapshotRequests = append(snapshotRequests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&snapshotEnvBinding),
			})
		}
	}

	return snapshotRequests
}

// findObjectsForDeploymentTarget maps an incoming DT event to the corresponding Environment request.
// We should reconcile Environments if the DT credentials get updated.
func (r *SnapshotEnvironmentBindingReconciler) findObjectsForDeploymentTarget(ctx context.Context, dt client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtObj, ok := dt.(*appstudioshared.DeploymentTarget)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the SEB DeploymentTarget mapping function, expected a DeploymentTarget")
		return []reconcile.Request{}
	}

	// 1. Find all DeploymentTargetClaims that are associated with this DeploymentTarget.
	dtcList := appstudioshared.DeploymentTargetClaimList{}
	if err := r.List(ctx, &dtcList, &client.ListOptions{Namespace: dt.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list DeploymentTargetClaims in the SEB DT mapping function")
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
	if err := r.Client.List(context.Background(), envList, &client.ListOptions{Namespace: dt.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list Environments in the SEB DeploymentTarget mapping function")
		return []reconcile.Request{}
	}
	environmentByName := map[string]bool{} // The keys of this map contain Environments that are referenced by the DTC (which references the DTC)
	for _, env := range envList.Items {

		matchFound := false
		for _, dtc := range dtcs {
			// If at least one of the DTCs match the Environment's claim name, it's a match
			if env.Spec.Configuration.Target.DeploymentTargetClaim.ClaimName == dtc.Name {
				matchFound = true
				break
			}
		}

		if matchFound { // at least one match: the Environment is bound to the DTC
			environmentByName[env.Name] = true
		}
	}

	// 3. Find SnapshotEnvironmentBindings in the namespace that match one of the Enviroments inside the 'environmentByName' map
	snapshotEnvList := &appstudioshared.SnapshotEnvironmentBindingList{}
	if err := r.Client.List(context.Background(), snapshotEnvList, &client.ListOptions{Namespace: dt.GetNamespace()}); err != nil {
		handlerLog.Error(err, "failed to list SnapshotEnvironmentBindings in the SEB DeploymentTarget mapping function")
		return []reconcile.Request{}
	}
	snapshotRequests := []reconcile.Request{}
	for index := range snapshotEnvList.Items {
		snapshotEnvBinding := snapshotEnvList.Items[index]

		if _, exists := environmentByName[snapshotEnvBinding.Spec.Environment]; exists {
			snapshotRequests = append(snapshotRequests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&snapshotEnvBinding),
			})
		}
	}

	return snapshotRequests
}

// findObjectsForEnvironment will reconcile on any Environments that are (indirectly) referenced by SEBs
func (r *SnapshotEnvironmentBindingReconciler) findObjectsForEnvironment(ctx context.Context, envParam client.Object) []reconcile.Request {
	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	// 1) Cast the Environment obj
	envObj, ok := envParam.(*appstudioshared.Environment)
	if !ok {
		handlerLog.Error(nil, "incompatible object in the Environment mapping function, expected a Secret")
		return []reconcile.Request{}
	}

	// 2) Retrieve all the SEBs in the same Namespace as the Environment
	var snapshotEnvBindings appstudioshared.SnapshotEnvironmentBindingList
	if err := r.Client.List(context.Background(), &snapshotEnvBindings, &client.ListOptions{Namespace: envObj.Namespace}); err != nil {
		handlerLog.Error(err, "failed to get SnapshotEnvironmentBindings list in the Environment mapping function")
		return []reconcile.Request{}
	}

	// 3) Reconcile any SEBs that reference this Environment
	var res []reconcile.Request
	for idx := range snapshotEnvBindings.Items {
		seb := snapshotEnvBindings.Items[idx]
		if seb.Spec.Environment == envObj.Name {
			res = append(res, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&seb),
			})
		}
	}

	return res
}

// updateMapWithExpectedAppStudioLabels ensures that the appstudio labels in the generated GitOpsDeployment are the same as defined in
// the parent binding, while not affecting any other non-appstudio labels that are also on the generated GitOpDeployment
func updateMapWithExpectedAppStudioLabels(actualLabelsParam map[string]string, expectedLabelsParam map[string]string) map[string]string {

	// 1) Clone the maps so we don't mutate the parameter values
	actualLabels := cloneMap(actualLabelsParam)
	expectedLabels := cloneMap(expectedLabelsParam)

	// 2) Remove '*appstudioLabelKey*' labels, we will add them back in the next tep
	removeAppStudioLabelsFromMap(actualLabels)

	// 3) Add back the appStudio expectedLabels
	for expectedLabelKey, expectedLabelValue := range expectedLabels {
		actualLabels[expectedLabelKey] = expectedLabelValue
	}

	// 4) Convert the map to nil, if it's empty, so that we don't have an empty labels map in the K8s object
	actualLabels = convertToNilIfEmptyMap(actualLabels)

	return actualLabels
}

// areAppStudioLabelsEqualBetweenMaps looks only at the map keys that contain appstudioLabelKey:
// returns true if they are all equal, false otherwise.
func areAppStudioLabelsEqualBetweenMaps(x map[string]string, y map[string]string) bool {
	newX := cloneMap(x)
	newY := cloneMap(y)

	removeNonAppStudioLabelsFromMap(newX)
	removeNonAppStudioLabelsFromMap(newY)

	return reflect.DeepEqual(newX, newY)

}

func convertToNilIfEmptyMap(m map[string]string) map[string]string {
	res := m
	if len(res) == 0 {
		res = nil
	}
	return res
}

func cloneMap(m map[string]string) map[string]string {
	res := map[string]string{}

	if m == nil {
		return res
	}

	for k, v := range m {
		res[k] = v
	}

	return res
}

func removeNonAppStudioLabelsFromMap(m map[string]string) {

	for key := range m {

		if !strings.Contains(key, appstudioLabelKey) {
			delete(m, key)
		}
	}
}

func removeAppStudioLabelsFromMap(m map[string]string) {

	for key := range m {

		if strings.Contains(key, appstudioLabelKey) {
			delete(m, key)
		}
	}
}

// isRequestNamespaceBeingDeleted returns true if the Namespace of the resource is being deleted, false otherwise.
func isRequestNamespaceBeingDeleted(ctx context.Context, namespaceName string, k8sClient client.Client, log logr.Logger) (bool, error) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
		if apierr.IsNotFound(err) {
			return false, nil
		}
		log.Error(err, "Unexpected error occurred on retrieving Namespace")
		return false, err

	} else if namespace.DeletionTimestamp != nil {
		log.Info("Ignoring request to Namespace that is being deleted (has non-nil deletionTimestamp)")
		return true, nil
	}

	return false, nil

}

// Update .status.bindingConditions field of SnapshotEnvironmentBinding
func updateBindingConditionOfSEB(ctx context.Context, client client.Client, message string,
	binding *appstudioshared.SnapshotEnvironmentBinding, conditionType string,
	status metav1.ConditionStatus, reason string, log logr.Logger) error {

	newCondition := metav1.Condition{
		Type:    conditionType,
		Message: message,
		Status:  status,
		Reason:  reason,
	}

	changed, newConditions := insertOrUpdateConditionsInSlice(newCondition, binding.Status.BindingConditions)

	if changed {
		binding.Status.BindingConditions = newConditions

		if err := client.Status().Update(ctx, binding); err != nil {
			log.Error(err, "unable to update .status.bindingCondition of SEB")
			return err
		}
		log.Info("updated .status.bindingCondition of SnapshotEnvironmentBinding")
	}

	return nil
}

// insertOrUpdateConditionsInSlice is a generic function for inserting/updating metav1.Condition into a slice of []metav1.Condition
func insertOrUpdateConditionsInSlice(newCondition metav1.Condition, existingConditions []metav1.Condition) (bool, []metav1.Condition) {

	// Check if condition with same type is already set, if Yes then check if content is same,
	// If content is not same update LastTransitionTime

	index := -1
	for i, Condition := range existingConditions {
		if Condition.Type == newCondition.Type {
			index = i
			break
		}
	}

	now := metav1.Now()

	changed := false

	if index == -1 {
		newCondition.LastTransitionTime = now
		existingConditions = append(existingConditions, newCondition)
		changed = true

	} else if existingConditions[index].Message != newCondition.Message ||
		existingConditions[index].Reason != newCondition.Reason ||
		existingConditions[index].Status != newCondition.Status {

		newCondition.LastTransitionTime = now
		existingConditions[index] = newCondition
		changed = true
	}

	return changed, existingConditions

}
