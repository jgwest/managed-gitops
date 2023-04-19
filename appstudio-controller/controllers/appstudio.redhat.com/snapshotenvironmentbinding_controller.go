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
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// If the 'appstudioLabelKey' string is present in a label of the SnapshotEnvironmentBinding, that label is copied to child GitOpsDeployments of the SnapshotEnvironmentBinding
	appstudioLabelKey = "appstudio.openshift.io"
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

	log := log.FromContext(ctx).WithValues("name", req.Name, "namespace", req.Namespace, "component", "bindingReconcile")
	defer log.V(sharedutil.LogLevel_Debug).Info("Snapshot Environment Binding Reconcile() complete.")

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

	if len(binding.Status.GitOpsRepoConditions) > 0 &&
		binding.Status.GitOpsRepoConditions[len(binding.Status.GitOpsRepoConditions)-1].Status == metav1.ConditionFalse {
		// if the SnapshotEventBinding GitOps Repo Conditions status is false - return;
		// since there was an unexpected issue with refreshing/syncing the GitOps repository
		log.V(sharedutil.LogLevel_Debug).Info("Can not Reconcile Binding '" + binding.Name + "', since GitOps Repo Conditions status is false.")

		// Update Status.Conditions field environmentBinding.
		if err := updateStatusConditionOfEnvironmentBinding(ctx, rClient,
			"Can not Reconcile Binding '"+binding.Name+"', since GitOps Repo Conditions status is false.", binding,
			SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred); err != nil {

			log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
			return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
		}

		return ctrl.Result{}, nil

	} else if len(binding.Status.Components) == 0 {

		log.V(sharedutil.LogLevel_Debug).Info("SnapshotEventBinding Component status is required to " +
			"generate GitOps deployment, waiting for the Application Service controller to finish reconciling binding")

		// Update Status.Conditions field of environmentBinding.
		if err := updateStatusConditionOfEnvironmentBinding(ctx, rClient, "SnapshotEventBinding Component status is required to "+
			"generate GitOps deployment, waiting for the Application Service controller to finish reconciling binding '"+binding.Name+"'",
			binding, SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred); err != nil {

			log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
			return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
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
			if err := updateStatusConditionOfEnvironmentBinding(ctx, rClient, errDuplicateKeysFound+" in "+component.Name, binding, SnapshotEnvironmentBindingConditionErrorOccurred, metav1.ConditionTrue, SnapshotEnvironmentBindingReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update snapshotEnvironmentBinding status condition.")
				return ctrl.Result{}, fmt.Errorf("unable to update snapshotEnvironmentBinding status condition. %v", err)
			}

			log.Error(nil, fmt.Sprintf("%s: %s", errDuplicateKeysFound, component.Name))
			return ctrl.Result{}, nil
		}

		var err error
		expectedDeployments[component.Name], err = generateExpectedGitOpsDeployment(component, *binding, environment)
		if err != nil {
			return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("invalid target namespace: %v", err)
		}
	}

	var statusField []appstudioshared.BindingStatusGitOpsDeployment
	var allErrors error

	// For each deployment, check if it exists, and if it has the expected content.
	// - If not, create/update it.
	for componentName, expectedGitOpsDeployment := range expectedDeployments {

		if err := processExpectedGitOpsDeployment(ctx, expectedGitOpsDeployment, *binding, rClient); err != nil {

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

	// Update the status field with statusField vars (even if an error occurred)
	binding.Status.GitOpsDeployments = statusField
	if err := addComponentDeploymentCondition(ctx, binding, rClient, log); err != nil {
		log.Error(err, "unable to update component deployment condition for Binding "+binding.Name)
		return ctrl.Result{}, fmt.Errorf("unable to update component deployment condition for SnapshotEnvironmentBinding. Error: %w", err)
	}

	// If our update logic did not modify the binding at all, there is no need to all update.
	if reflect.DeepEqual(binding, originalBinding) {
		log.V(sharedutil.LogLevel_Debug).Info("Skipping update of SnapshotEnvironmentBinding, as the resource did not change.")
		return ctrl.Result{}, nil
	}

	if err := rClient.Status().Update(ctx, binding); err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to update SnapshotEnvironmentBinding status")
		return ctrl.Result{}, fmt.Errorf("unable to update SnapshotEnvironmentBinding status. Error: %w", err)
	}

	if allErrors != nil {
		return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("unable to process expected GitOpsDeployment: %w", allErrors)
	}

	return ctrl.Result{}, nil
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
	binding appstudioshared.SnapshotEnvironmentBinding, k8sClient client.Client) error {

	log := log.FromContext(ctx).WithValues("binding", binding.Name, "gitOpsDeployment", expectedGitopsDeployment.Name, "namespace", binding.Namespace)
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
		sharedutil.LogAPIResourceChangeEvent(expectedGitopsDeployment.Namespace, expectedGitopsDeployment.Name, expectedGitopsDeployment, sharedutil.ResourceCreated, log)

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
	sharedutil.LogAPIResourceChangeEvent(expectedGitopsDeployment.Namespace, expectedGitopsDeployment.Name, expectedGitopsDeployment, sharedutil.ResourceModified, log)

	return nil
}

// GenerateBindingGitOpsDeploymentName generates the name that will be used for a given GitOpsDeployment of a binding
func GenerateBindingGitOpsDeploymentName(binding appstudioshared.SnapshotEnvironmentBinding, componentName string) string {

	expectedName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + componentName

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

func generateExpectedGitOpsDeployment(component appstudioshared.BindingComponentStatus,
	binding appstudioshared.SnapshotEnvironmentBinding,
	environment appstudioshared.Environment) (apibackend.GitOpsDeployment, error) {

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

	// If the environment has a target cluster field defined, then set the destination to that managed environment
	if environment.Spec.UnstableConfigurationFields != nil {

		managedEnvironmentName := generateEmptyManagedEnvironment(environment.Name, environment.Namespace).Name

		if environment.Spec.UnstableConfigurationFields.TargetNamespace == "" {
			return apibackend.GitOpsDeployment{}, fmt.Errorf("%s: '%s'", errMissingTargetNamespace, environment.Name)
		}

		res.Spec.Destination = apibackend.ApplicationDestination{
			Environment: managedEnvironmentName,
			Namespace:   environment.Spec.UnstableConfigurationFields.TargetNamespace,
		}
	}

	res.ObjectMeta.Labels = make(map[string]string)

	// Append ASEB labels with key "appstudio.openshift.io" to the gitopsDeployment labels
	for bindingKey, bindingLabelValue := range binding.Labels {
		if strings.Contains(bindingKey, appstudioLabelKey) {
			res.ObjectMeta.Labels[bindingKey] = bindingLabelValue
		}
	}

	// Ensures that this method only adds 'appstudio.openshift.io' labels
	// - Note: If you remove this line, you need to search for other uses of 'removeNonAppStudioLabelsFromMap' in the
	// code, as you may break the logic here.
	removeNonAppStudioLabelsFromMap(res.ObjectMeta.Labels)

	res.ObjectMeta.Labels = convertToNilIfEmptyMap(res.ObjectMeta.Labels)

	return res, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SnapshotEnvironmentBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&appstudioshared.SnapshotEnvironmentBinding{}).
		Owns(&apibackend.GitOpsDeployment{}).
		Complete(r)
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
