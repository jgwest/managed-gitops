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
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appstudioshared "github.com/redhat-appstudio/application-api/api/v1alpha1"
	apibackend "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PromotionRunReconciler reconciles a PromotionRun object
type PromotionRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// Time limit in minutes, if GitOpsDeployments are not created/synced/Healthy in given time then cancel the Promotion.
	PromotionRunTimeOutLimit = 10

	StatusMessageAllGitOpsDeploymentsAreSyncedHealthy = "All GitOpsDeployments are Synced/Healthy"
	ErrMessageTargetEnvironmentHasInvalidValue        = "Target Environment has invalid value."
	ErrMessageAutomatedPromotionNotSupported          = "Automated promotion are not yet supported."
)

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=components,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=components/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=components/finalizers,verbs=update
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=promotionruns,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=promotionruns/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=promotionruns/finalizers,verbs=update
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshots,verbs=get;list;watch
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeployments,verbs=get;list;watch;
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=snapshotenvironmentbindings/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile

func (r *PromotionRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("name", req.Name, "namespace", req.Namespace)
	defer log.V(sharedutil.LogLevel_Debug).Info("Promotion Run Reconcile() complete.")
	promotionRun := &appstudioshared.PromotionRun{}

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	// If the Namespace is in the process of being deleted, don't handle any additional requests.
	if isNamespaceBeingDeleted, err := isRequestNamespaceBeingDeleted(ctx, req.Namespace,
		rClient, log); isNamespaceBeingDeleted || err != nil {
		return ctrl.Result{}, err
	}

	if err := rClient.Get(ctx, req.NamespacedName, promotionRun); err != nil {
		if apierr.IsNotFound(err) {
			// Nothing more to do!
			log.Error(err, "No PromotionRun exists in Namespace: "+req.Namespace)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "unable to retrieve PromotionRun from Namspace: "+req.Namespace)
			return ctrl.Result{}, fmt.Errorf("unable to retrieve PromotionRun: %v", err)
		}
	}

	if promotionRun.Status.State == appstudioshared.PromotionRunState_Complete {
		// Ignore promotion runs that have completed.
		log.V(sharedutil.LogLevel_Debug).Info("Promotion '" + promotionRun.Name + "' is already completed")
		return ctrl.Result{}, nil
	}

	if err := checkForExistingActivePromotions(ctx, *promotionRun, rClient); err != nil {
		log.Error(err, "Error occurred while checking for existing active promotions for: "+promotionRun.Name)

		// Update Status.Conditions field.
		if err = updateStatusConditions(ctx, rClient, "Error occurred while checking for existing active promotions.", promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
			appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
			log.Error(err, "unable to update PromotionRun status conditions.")
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
		}

		return ctrl.Result{}, nil
	}

	// If this is a automated promotion, ignore it for now
	if promotionRun.Spec.AutomatedPromotion.InitialEnvironment != "" {
		log.Error(nil, ErrMessageAutomatedPromotionNotSupported+" : "+promotionRun.Name)

		// Update Status.Conditions field.
		if err := updateStatusConditions(ctx, rClient, ErrMessageAutomatedPromotionNotSupported, promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
			appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
			log.Error(err, "unable to update PromotionRun status conditions.")
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
		}
		return ctrl.Result{}, nil
	}

	// If TargetEnvironment is not valid then stop the Promotion.
	if promotionRun.Spec.ManualPromotion.TargetEnvironment == "" {
		log.Error(nil, ErrMessageTargetEnvironmentHasInvalidValue+" : "+promotionRun.Name)

		// Update Status.Conditions field.
		if err := updateStatusConditions(ctx, rClient, ErrMessageTargetEnvironmentHasInvalidValue, promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
			appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
			log.Error(err, "unable to update PromotionRun status conditions.")
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
		}
		return ctrl.Result{}, nil
	}

	// 1) Locate or create the binding that this PromotionRun is targeting
	binding, err := locateOrCreateTargetManualBinding(ctx, *promotionRun, r.Client, log)
	if err != nil {
		log.Error(err, "error locating or creating Binding for PromotionRun: "+promotionRun.Name)
		return ctrl.Result{}, nil
	}

	if promotionRun.Status.State != appstudioshared.PromotionRunState_Active {
		promotionRun.Status.State = appstudioshared.PromotionRunState_Active

		if err := rClient.Status().Update(ctx, promotionRun); err != nil {
			log.Error(err, "unable to update PromotionRun state: "+promotionRun.Name)
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun state: %v", err)
		}

		sharedutil.LogAPIResourceChangeEvent(promotionRun.Namespace, promotionRun.Name, promotionRun, sharedutil.ResourceModified, log)
		log.V(sharedutil.LogLevel_Debug).Info("updated PromotionRun state" + promotionRun.Name)
	}

	// Verify: activebindings should not have a value which differs from the value specified in promotionrun.spec
	if len(promotionRun.Status.ActiveBindings) > 0 {
		for _, existingActiveBinding := range promotionRun.Status.ActiveBindings {

			if existingActiveBinding != binding.Name {
				message := "The binding changed after the PromotionRun first start. " +
					"The .spec fields of the PromotionRun are immutable, and should not be changed " +
					"after being created. old-binding: " + existingActiveBinding + ", new-binding: " + binding.Name

				// Update Status.Conditions field.
				if err = updateStatusConditions(ctx, rClient, message, promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
					appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
					log.Error(err, "unable to update PromotionRun status conditions.")
					return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
				}
				return ctrl.Result{}, nil
			}
		}
	}

	// Verify that the snapshot referred in binding.spec.snapshot actually exists, if not, throw error
	snapshot := appstudioshared.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      promotionRun.Spec.Snapshot,
			Namespace: binding.Namespace,
		},
	}

	if err := rClient.Get(ctx, client.ObjectKeyFromObject(&snapshot), &snapshot); err != nil {
		if apierr.IsNotFound(err) {
			log.Error(err, "Snapshot: "+snapshot.Name+" referred in Binding: "+binding.Name+" does not exist.")

			// Update Status.Conditions field.
			if err = updateStatusConditions(ctx, rClient, "Snapshot: "+snapshot.Name+" referred in Binding: "+binding.Name+" does not exist.",
				promotionRun, appstudioshared.PromotionRunConditionErrorOccurred, appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update PromotionRun status conditions.")
				return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
			}

			return ctrl.Result{}, nil
		} else {
			log.Error(err, "unable to retrieve Snapshot: "+snapshot.Name)

			// Update Status.Conditions field.
			if err = updateStatusConditions(ctx, rClient, "unable to retrieve Snapshot: "+snapshot.Name,
				promotionRun, appstudioshared.PromotionRunConditionErrorOccurred, appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update PromotionRun status conditions.")
				return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
			}

			return ctrl.Result{}, fmt.Errorf("unable to retrieve Snapshot: %v", err)
		}
	}

	// 2) Set the Binding to target the expected snapshot, if not already done
	if binding.Spec.Snapshot != promotionRun.Spec.Snapshot || len(promotionRun.Status.ActiveBindings) == 0 {

		binding.Spec.Snapshot = promotionRun.Spec.Snapshot

		if err := rClient.Update(ctx, &binding); err != nil {
			log.Error(err, "unable to update Binding: "+binding.Name)

			// Update Status.Conditions field.
			if err = updateStatusConditions(ctx, rClient, "unable to update Binding: "+binding.Name,
				promotionRun, appstudioshared.PromotionRunConditionErrorOccurred, appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update PromotionRun status conditions.")
				return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
			}

			return ctrl.Result{}, fmt.Errorf("unable to update Binding '%s' snapshot: %v", binding.Name, err)
		}

		sharedutil.LogAPIResourceChangeEvent(promotionRun.Namespace, promotionRun.Name, promotionRun, sharedutil.ResourceModified, log)
		log.Info("Updating Binding: " + binding.Name + " to target the Snapshot: " + promotionRun.Spec.Snapshot)

		// Set the time when of first reconcilation on a particular PromotionRun if not set already. This will be used later to check for time out of Promotion.
		if promotionRun.Status.PromotionStartTime.IsZero() {
			promotionRun.Status.PromotionStartTime = metav1.Now()
		}

		promotionRun.Status.ActiveBindings = []string{binding.Name}
		if err := rClient.Status().Update(ctx, promotionRun); err != nil {
			log.Error(err, "unable to update PromotionRun active binding: "+promotionRun.Name)

			// Update Status.Conditions field.
			if err = updateStatusConditions(ctx, rClient, "unable to update PromotionRun active binding.",
				promotionRun, appstudioshared.PromotionRunConditionErrorOccurred, appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update PromotionRun status conditions.")
				return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
			}

			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun active binding: %v", err)
		}
		sharedutil.LogAPIResourceChangeEvent(promotionRun.Namespace, promotionRun.Name, promotionRun, sharedutil.ResourceModified, log)
	}

	// 3) Wait for the environment binding to create all of the expected GitOpsDeployments
	if len(binding.Status.GitOpsDeployments) != len(binding.Spec.Components) {
		// Update Status.Environment.Status field.
		if err = updateStatusEnvironmentStatus(ctx, rClient, "Waiting for the environment binding to create all of the expected GitOpsDeployments.",
			promotionRun, appstudioshared.PromotionRunEnvironmentStatus_InProgress, log); err != nil {
			log.Error(err, "unable to update PromotionRun environment status: "+promotionRun.Name)
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun environment status %v", err)
		}
		return ctrl.Result{}, nil
	}

	// TODO: gitopsRepositoryCommitsWithSnapshot := []string{"main"} /* we need a mechanism to tell which gitops repository revision corresponds to which snapshot*/

	// 4) Wait for all the GitOpsDeployments of the binding to have the expected state
	waitingGitOpsDeployments := []string{}

	for _, gitopsDeploymentName := range binding.Status.GitOpsDeployments {
		gitopsDeployment := &apibackend.GitOpsDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      gitopsDeploymentName.GitOpsDeployment,
				Namespace: binding.Namespace,
			},
		}
		if err := rClient.Get(ctx, client.ObjectKeyFromObject(gitopsDeployment), gitopsDeployment); err != nil {
			log.Error(err, "unable to retrieve GitOpsDeployment: "+gitopsDeployment.Name)

			// Update Status.Conditions field.
			if err = updateStatusConditions(ctx, rClient, "unable to retrieve GitOpsDeployment: "+gitopsDeployment.Name,
				promotionRun, appstudioshared.PromotionRunConditionErrorOccurred, appstudioshared.PromotionRunConditionStatusTrue, appstudioshared.PromotionRunReasonErrorOccurred); err != nil {
				log.Error(err, "unable to update PromotionRun status conditions.")
				return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
			}

			return ctrl.Result{}, fmt.Errorf("unable to retrieve GitOpsDeployment '%s', %v", gitopsDeployment.Name, err)
		}

		// Must have status of Synced/Healthy
		if gitopsDeployment.Status.Sync.Status == apibackend.SyncStatusCodeSynced && gitopsDeployment.Status.Health.Status != apibackend.HeathStatusCodeHealthy {
			promotionRun.Status.State = appstudioshared.PromotionRunState_Waiting

			// Update Status.Environment.Status field.
			if err = updateStatusEnvironmentStatus(ctx, rClient, "waiting for GitOpsDeployments to get in Sync/Healthy.",
				promotionRun, appstudioshared.PromotionRunEnvironmentStatus_InProgress, log); err != nil {
				log.Error(err, "unable to update PromotionRun environment status: "+promotionRun.Name)
				return ctrl.Result{}, fmt.Errorf("unable to update promotionRun %v", err)
			}

			waitingGitOpsDeployments = append(waitingGitOpsDeployments, gitopsDeployment.Name)
			continue
		}

		// TODO: Need a mechanism to tell which gitops repository revision is supposed be deployed.
		/*// Argo CD must have deployed at least one of the commits that include the Snapshot container images
		match := false
		for _, snapshotCommit := range gitopsRepositoryCommitsWithSnapshot {
			if gitopsDeployment.Status.Sync.Revision == snapshotCommit {
				match = true
				break
			}
		}
		if !match {
			waitingGitOpsDeployments = append(waitingGitOpsDeployments, gitopsDeployment.Name)
			continue
		}*/
	}

	// Check time limit set for PromotionRun reconcilation, fail if the conditions aren't met in the given time frame.
	if err := rClient.Get(ctx, client.ObjectKeyFromObject(promotionRun), promotionRun); err != nil {
		log.Error(err, "unable to retrieve promotionRun: "+promotionRun.Name)
		return ctrl.Result{}, fmt.Errorf("unable to retrieve promotionRun '%s', %v", promotionRun.Name, err)
	}

	if !promotionRun.Status.PromotionStartTime.IsZero() && (metav1.Now().Sub(promotionRun.Status.PromotionStartTime.Time).Minutes() > PromotionRunTimeOutLimit) {

		promotionRun.Status.CompletionResult = appstudioshared.PromotionRunCompleteResult_Failure
		promotionRun.Status.State = appstudioshared.PromotionRunState_Complete

		// Update Status.Environment.Status field.
		if err = updateStatusEnvironmentStatus(ctx, rClient, fmt.Sprintf("Promotion Failed. Could not be completed in %d Minutes.", PromotionRunTimeOutLimit),
			promotionRun, appstudioshared.PromotionRunEnvironmentStatus_Failed, log); err != nil {
			log.Error(err, "unable to update PromotionRun environment status: "+promotionRun.Name)
			return ctrl.Result{}, fmt.Errorf("unable to update promotionRun %v", err)
		}
		return ctrl.Result{}, nil
	}

	if len(waitingGitOpsDeployments) > 0 {
		log.Info("Waiting for GitOpsDeployments to have expected commit/sync/health:" + strings.Join(waitingGitOpsDeployments[:], ", "))

		promotionRun.Status.State = appstudioshared.PromotionRunState_Waiting

		// Update Status.Environment.Status field.
		if err = updateStatusEnvironmentStatus(ctx, rClient, "Waiting for following GitOpsDeployments to be Synced/Healthy: "+strings.Join(waitingGitOpsDeployments[:], ", "),
			promotionRun, appstudioshared.PromotionRunEnvironmentStatus_InProgress, log); err != nil {
			log.Error(err, "unable to update PromotionRun environment status: "+promotionRun.Name)
			return ctrl.Result{}, fmt.Errorf("unable to update promotionRun %v", err)
		}

		// set ErrorOccurred condition to false:
		if err = updateStatusConditions(ctx, rClient, "", promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
			appstudioshared.PromotionRunConditionStatusFalse, ""); err != nil {
			log.Error(err, "unable to update PromotionRun status conditions.")
			return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
		}

		return ctrl.Result{RequeueAfter: time.Second * 15}, nil
	}

	// All the GitOpsDeployments are synced/healthy, and they are synced with a commit that includes the target snapshot.
	promotionRun.Status.CompletionResult = appstudioshared.PromotionRunCompleteResult_Success
	promotionRun.Status.State = appstudioshared.PromotionRunState_Complete
	promotionRun.Status.ActiveBindings = []string{binding.Name}

	// Update Status.Environment.Status field.
	if err = updateStatusEnvironmentStatus(ctx, rClient, StatusMessageAllGitOpsDeploymentsAreSyncedHealthy,
		promotionRun, appstudioshared.PromotionRunEnvironmentStatus_Success, log); err != nil {
		log.Error(err, "unable to update PromotionRun environment status: "+promotionRun.Name)
		return ctrl.Result{}, fmt.Errorf("unable to update promotionRun %v", err)
	}

	// set ErrorOccurred condition to false
	if err = updateStatusConditions(ctx, rClient, "", promotionRun, appstudioshared.PromotionRunConditionErrorOccurred,
		appstudioshared.PromotionRunConditionStatusFalse, ""); err != nil {
		log.Error(err, "unable to update PromotionRun status conditions.")
		return ctrl.Result{}, fmt.Errorf("unable to update PromotionRun status conditions %v", err)
	}

	return ctrl.Result{}, nil
}

// checkForExistingActivePromotions ensures that there are no other promotions that are active on this application.
// - only one promotion should be active at a time on a single Application
func checkForExistingActivePromotions(ctx context.Context, reconciledPromotionRun appstudioshared.PromotionRun, k8sClient client.Client) error {

	otherPromotionRuns := &appstudioshared.PromotionRunList{}
	if err := k8sClient.List(ctx, otherPromotionRuns); err != nil {
		// log me
		return err
	}

	for _, otherPromotionRun := range otherPromotionRuns.Items {
		if otherPromotionRun.Status.State == appstudioshared.PromotionRunState_Complete {
			// Ignore completed promotions (these are no longer active)
			continue
		}

		if otherPromotionRun.Spec.Application != reconciledPromotionRun.Spec.Application {
			// Ignore promotions with applications that don't match the one we are reconciling
			continue
		}

		if otherPromotionRun.ObjectMeta.CreationTimestamp.Before(&reconciledPromotionRun.CreationTimestamp) {
			return fmt.Errorf("another PromotionRun is already active. Only one waiting/active PromotionRun should exist per Application: %s", otherPromotionRun.Name)
		}
	}

	return nil
}

func locateOrCreateTargetManualBinding(ctx context.Context, promotionRun appstudioshared.PromotionRun, k8sClient client.Client, logger logr.Logger) (appstudioshared.SnapshotEnvironmentBinding, error) {

	// Locate the corresponding binding

	bindingList := appstudioshared.SnapshotEnvironmentBindingList{}
	if err := k8sClient.List(ctx, &bindingList, &client.ListOptions{Namespace: promotionRun.Namespace}); err != nil {
		return appstudioshared.SnapshotEnvironmentBinding{}, fmt.Errorf("unable to list bindings: %v", err)
	}

	for _, binding := range bindingList.Items {

		if binding.Spec.Application == promotionRun.Spec.Application && binding.Spec.Environment == promotionRun.Spec.ManualPromotion.TargetEnvironment {
			return binding, nil
		}
	}

	// Binding not found, so create it
	components := []appstudioshared.BindingComponent{}
	componentList := appstudioshared.ComponentList{}
	if err := k8sClient.List(ctx, &componentList, &client.ListOptions{Namespace: promotionRun.Namespace}); err != nil {
		return appstudioshared.SnapshotEnvironmentBinding{}, fmt.Errorf("unable to list components: %v", err)
	}
	for _, component := range componentList.Items {
		if component.Spec.Application == promotionRun.Spec.Application {
			components = append(components, appstudioshared.BindingComponent{
				Name: component.Spec.ComponentName,
			})
		}
	}
	if len(components) == 0 {
		return appstudioshared.SnapshotEnvironmentBinding{}, fmt.Errorf("unable to find components for application: %s", promotionRun.Spec.Application)
	}
	binding := appstudioshared.SnapshotEnvironmentBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      createBindingName(&promotionRun),
			Namespace: promotionRun.Namespace,
			Labels: map[string]string{
				"appstudio.application": promotionRun.Spec.Application,
				"appstudio.environment": promotionRun.Spec.ManualPromotion.TargetEnvironment,
			},
		},
		Spec: appstudioshared.SnapshotEnvironmentBindingSpec{
			Application: promotionRun.Spec.Application,
			Environment: promotionRun.Spec.ManualPromotion.TargetEnvironment,
			Snapshot:    promotionRun.Spec.Snapshot,
			Components:  components,
		},
	}
	err := k8sClient.Create(ctx, &binding)
	if err != nil {
		return appstudioshared.SnapshotEnvironmentBinding{}, err
	}
	sharedutil.LogAPIResourceChangeEvent(binding.Namespace, binding.Name, &binding, sharedutil.ResourceCreated, logger)
	logger.Info("Created SnapshotEnvironmentBinding",
		"application", promotionRun.Spec.Application,
		"environment", promotionRun.Spec.ManualPromotion.TargetEnvironment)

	return binding, nil
}

func createBindingName(promotionRun *appstudioshared.PromotionRun) string {
	name := strings.ToLower(promotionRun.Spec.Application + "-" + promotionRun.Spec.ManualPromotion.TargetEnvironment + "-generated-binding")
	if len(name) > 250 {
		// 'suffix' will have a length of 64
		hash := sha256.Sum256([]byte(name))
		suffix := hex.EncodeToString(hash[:])
		name = "generated-environment-binding-" + suffix
	}
	return name
}

// SetupWithManager sets up the controller with the Manager.
func (r *PromotionRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appstudioshared.PromotionRun{}).
		Owns(&appstudioshared.SnapshotEnvironmentBinding{}).
		Complete(r)
}

// Update Status.Environment.Status field.
func updateStatusEnvironmentStatus(ctx context.Context, client client.Client, displayStatus string, promotionRun *appstudioshared.PromotionRun,
	status appstudioshared.PromotionRunEnvironmentStatusField, log logr.Logger) error {

	targetEnvIndex, targetEnvStep := -1, 0

	// Check if EnvironmentStatus for given Environment is already present.
	for i, envStatus := range promotionRun.Status.EnvironmentStatus {
		// Find the Index in array having status for given environment.
		if envStatus.EnvironmentName == promotionRun.Spec.ManualPromotion.TargetEnvironment {
			targetEnvIndex = i
			break
		}
	}

	// If given environment is not present already then create new else update existing one.
	if targetEnvIndex == -1 {

		// Find the max Step and Index available
		for _, j := range promotionRun.Status.EnvironmentStatus {
			if j.Step > targetEnvIndex {
				targetEnvStep = j.Step
			}
		}

		promotionRun.Status.EnvironmentStatus = append(promotionRun.Status.EnvironmentStatus,
			appstudioshared.PromotionRunEnvironmentStatus{
				Step:            targetEnvStep + 1,
				EnvironmentName: promotionRun.Spec.ManualPromotion.TargetEnvironment,
				DisplayStatus:   displayStatus,
				Status:          status,
			})
	} else {
		// Status for given environment exists, just update it.
		promotionRun.Status.EnvironmentStatus[targetEnvIndex].DisplayStatus = displayStatus
		promotionRun.Status.EnvironmentStatus[targetEnvIndex].Status = status
	}

	if err := client.Status().Update(ctx, promotionRun); err != nil {
		return err
	}
	sharedutil.LogAPIResourceChangeEvent(promotionRun.Namespace, promotionRun.Name, promotionRun, sharedutil.ResourceModified, log)

	return nil
}

// Update Status.Conditions field.
func updateStatusConditions(ctx context.Context, client client.Client, message string,
	promotionRun *appstudioshared.PromotionRun, conditionType appstudioshared.PromotionRunConditionType,
	status appstudioshared.PromotionRunConditionStatus, reason appstudioshared.PromotionRunReasonType) error {

	// Check if condition with same type is already set, if Yes then check if content is same,
	// if Yes then update only LastProbeTime else update all fields in existing element.
	// If element with same type is not present then append new element.
	index := -1
	for i, Condition := range promotionRun.Status.Conditions {
		if Condition.Type == conditionType {
			index = i
			break
		}
	}

	now := metav1.Now()

	if index == -1 {
		promotionRun.Status.Conditions = append(promotionRun.Status.Conditions,
			appstudioshared.PromotionRunCondition{
				Type:               conditionType,
				Message:            message,
				LastProbeTime:      now,
				LastTransitionTime: &now,
				Status:             status,
				Reason:             reason,
			})
	} else {
		if promotionRun.Status.Conditions[index].Message == message &&
			promotionRun.Status.Conditions[index].Reason == reason &&
			promotionRun.Status.Conditions[index].Status == status {

			promotionRun.Status.Conditions[index].LastProbeTime = now
		} else {
			promotionRun.Status.Conditions[index].Reason = reason
			promotionRun.Status.Conditions[index].Message = message
			promotionRun.Status.Conditions[index].LastProbeTime = now
			promotionRun.Status.Conditions[index].LastTransitionTime = &now
			promotionRun.Status.Conditions[index].Status = status
		}
	}

	if err := client.Status().Update(ctx, promotionRun); err != nil {
		return err
	}

	return nil
}
