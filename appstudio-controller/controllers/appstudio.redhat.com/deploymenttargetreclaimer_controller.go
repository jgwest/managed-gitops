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

package appstudioredhatcom

import (
	"context"
	"fmt"
	"time"

	codereadytoolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	"github.com/codeready-toolchain/toolchain-common/pkg/condition"
	applicationv1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DeploymentTargetReconciler reconciles a DeploymentTarget object
type DeploymentTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Clock  sharedutil.Clock
}

const (
	FinalizerDT = "dt.appstudio.redhat.com/finalizer"
)

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims/finalizers,verbs=update
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargets,verbs=get;list;watch;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargets/finalizers,verbs=update
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=spacerequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=toolchain.dev.openshift.com,resources=spacerequests/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Modify the Reconcile function to compare the state specified by
// the DeploymentTargetClaim object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *DeploymentTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := log.FromContext(ctx).WithValues("name", req.Name, "namespace", req.Namespace, "component", "deploymentTargetReclaimer")

	dt := applicationv1alpha1.DeploymentTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(&dt), &dt); err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Add the deletion finalizer if it is absent.
	if addFinalizer(&dt, FinalizerDT) {
		if err := r.Client.Update(ctx, &dt); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer %s to DeploymentTarget %s in namespace %s: %v", FinalizerDT, dt.Name, dt.Namespace, err)
		}
		log.Info("Added finalizer to DeploymentTarget", "finalizer", FinalizerDT)
	}

	// Retrieve and sanity check the DeploymentTargetClass of the DT
	dtClass, err := findMatchingDTClassForDT(ctx, dt, r.Client)
	if err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if dtClass.Spec.ReclaimPolicy != applicationv1alpha1.ReclaimPolicy_Delete &&
		dtClass.Spec.ReclaimPolicy != applicationv1alpha1.ReclaimPolicy_Retain {

		log.Error(nil, "unexpected reclaim policy value on DTClass", "reclaimPolicy", dtClass.Spec.ReclaimPolicy)
		return ctrl.Result{}, nil
	}

	// If the DeploymentTarget is not deleted, verify if it has a corresponding DTC
	if dt.Spec.ClaimRef != "" && dt.DeletionTimestamp == nil {
		dtc := &applicationv1alpha1.DeploymentTargetClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dt.Spec.ClaimRef,
				Namespace: dt.Namespace,
			},
		}

		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(dtc), dtc); err != nil {
			if !apierr.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// If the DTC is already deleted and the reclaim policy is Retain, unset the claimRef field of the DeploymentTarget.
			if dtClass.Spec.ReclaimPolicy == applicationv1alpha1.ReclaimPolicy_Retain {
				dt.Spec.ClaimRef = ""
				if err := r.Client.Update(ctx, &dt); err != nil {
					return ctrl.Result{}, err
				}

				log.Info("ClaimRef of DeploymentTarget is unset since its corresponding DeploymentTargetClaim is already deleted", "DeploymentTarget", dt.Name)

				logutil.LogAPIResourceChangeEvent(dt.Namespace, dt.Name, dt, logutil.ResourceModified, log)
			}
		}
	}

	if dt.DeletionTimestamp == nil {
		// The DeploymentTarget is not currently being deleted, so no more work to do.
		return ctrl.Result{}, nil
	}

	// From this point on in this function, the DeletionTimestamp is necessarily set

	// Handle deletion if the DT has a deletion timestamp set.
	var sr *codereadytoolchainv1alpha1.SpaceRequest
	if sr, err = findMatchingSpaceRequestForDT(ctx, r.Client, &dt); err != nil {
		if !apierr.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// If the SpaceRequest no longer exists, OR if the class has a Retain policy, then there is no more work to do.
	if sr == nil || dtClass.Spec.ReclaimPolicy == applicationv1alpha1.ReclaimPolicy_Retain {
		if removeFinalizer(&dt, FinalizerDT) {
			if err := r.Client.Update(ctx, &dt); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer %s from DeploymentTarget %s in namespace %s: %v", FinalizerDT, dt.Name, dt.Namespace, err)
			}
			log.Info("Removed finalizer from DeploymentTarget", "finalizer", FinalizerDT)
		}
		return ctrl.Result{}, nil
	}

	if dtClass.Spec.ReclaimPolicy != applicationv1alpha1.ReclaimPolicy_Delete {
		log.Error(nil, "Unexpected reclaimPolicy: neither Retain nor Delete.")
		return ctrl.Result{}, nil
	}

	// The ReclaimPolicy is necessarily equal to 'Delete', from this point on in the function

	if addFinalizer(sr, codereadytoolchainv1alpha1.FinalizerName) {
		if err := r.Client.Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer %s for SpaceRequest %s in namespace %s: %v", codereadytoolchainv1alpha1.FinalizerName, sr.Name, sr.Namespace, err)
		}
		log.Info("Added finalizer for SpaceRequest", "finalizer", codereadytoolchainv1alpha1.FinalizerName)
	}

	var readyCond codereadytoolchainv1alpha1.Condition
	var found bool
	if readyCond, found = condition.FindConditionByType(sr.Status.Conditions, codereadytoolchainv1alpha1.ConditionReady); !found {
		return ctrl.Result{}, fmt.Errorf("failed to find ConditionReady for SpaceRequest %s from %s", sr.Name, sr.Namespace)
	}

	// Delete the SpaceRequest if it has not been deleted
	if sr.DeletionTimestamp == nil {

		log.Info("Deleting SpaceRequest", "spaceRequest", sr)
		if err := r.Client.Delete(ctx, sr); err != nil {
			return ctrl.Result{}, err
		}
		logutil.LogAPIResourceChangeEvent(sr.Namespace, sr.Name, sr, logutil.ResourceDeleted, log)

		return ctrl.Result{Requeue: true}, nil
	}

	if dt.Status.Phase == applicationv1alpha1.DeploymentTargetPhase_Failed {
		// No more work: the DT is already reported as failed
		return ctrl.Result{}, nil
	}

	// Initialize the clock if it isn't already to avoid panic.
	if r.Clock == nil {
		r.Clock = sharedutil.NewClock()
	}

	// If SpaceRequest still exists after 2 minutes and has a condition reason of UnableToTerminate...
	if r.Clock.Now().After(sr.GetDeletionTimestamp().Add(time.Minute*2)) &&
		readyCond.Reason == codereadytoolchainv1alpha1.SpaceTerminatingFailedReason {

		dt.Status.Phase = applicationv1alpha1.DeploymentTargetPhase_Failed
		if err := r.Client.Status().Update(ctx, &dt); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("The status of DT is updated to Failed", "dtName", dt.Name, "dtNamespace", dt.Namespace)
		return ctrl.Result{}, nil
	}

	log.Info("Requeuing DeploymentTarget, since SpaceRequest is still terminating", "spaceRequestStatusPhase", dt.Status.Phase, "deletionTimestamp", sr.GetDeletionTimestamp())

	// OTOH, if the SpaceRequest has not timed out yet, then requeue
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil

}

// findMatchingSpaceRequestForDT tries to find a SpaceRequest that matches the given DT in a namespace.
// The function will return only the SpaceRequest that matches the expected environment Tier name
func findMatchingSpaceRequestForDT(ctx context.Context, k8sClient client.Client, dt *applicationv1alpha1.DeploymentTarget) (*codereadytoolchainv1alpha1.SpaceRequest, error) {

	spaceRequestList := codereadytoolchainv1alpha1.SpaceRequestList{}

	opts := []client.ListOption{
		client.InNamespace(dt.Namespace),
		client.MatchingLabels{
			deploymentTargetClaimLabel: dt.Spec.ClaimRef,
		},
	}

	if err := k8sClient.List(ctx, &spaceRequestList, opts...); err != nil {
		return nil, err
	}

	if len(spaceRequestList.Items) > 0 {
		var spaceRequest *codereadytoolchainv1alpha1.SpaceRequest
		for i, s := range spaceRequestList.Items {
			if s.Spec.TierName == environmentTierName {
				spaceRequest = &spaceRequestList.Items[i]
				break
			}
		}
		return spaceRequest, nil
	}

	return nil, nil
}

// findMatchingDTClassForDT tries to find a DTCLS that matches the given DT in a namespace.
func findMatchingDTClassForDT(ctx context.Context, dt applicationv1alpha1.DeploymentTarget, k8sClient client.Client) (applicationv1alpha1.DeploymentTargetClass, error) {

	// Retrieve and validate the DeploymentTargetClass of the DT
	dtClass := applicationv1alpha1.DeploymentTargetClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(dt.Spec.DeploymentTargetClassName),
		},
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtClass), &dtClass); err != nil {
		return applicationv1alpha1.DeploymentTargetClass{}, fmt.Errorf("unable to retrieve DeploymentTargetClass '%s' referenced by DeploymentTarget", dtClass.Name)
	}

	return dtClass, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	manager := ctrl.NewControllerManagedBy(mgr).
		For(&applicationv1alpha1.DeploymentTarget{}).
		Watches(
			&codereadytoolchainv1alpha1.SpaceRequest{},
			handler.EnqueueRequestsFromMapFunc(r.findDeploymentTargetsForSpaceRequests))

	return manager.Complete(r)
}

func (r *DeploymentTargetReconciler) findDeploymentTargetsForSpaceRequests(ctx context.Context, sr client.Object) []reconcile.Request {

	srObj, isOk := sr.(*codereadytoolchainv1alpha1.SpaceRequest)
	if !isOk {
		ctrl.Log.Error(nil, "SEVERE: type mismatch in mapping function. Expected an event for SpaceRequest object")
		return []reconcile.Request{}
	}

	dt, err := findMatchingDTForSpaceRequest(context.Background(), r.Client, srObj)
	if err != nil {
		ctrl.Log.Error(err, "unable to find matching DT for space request", "spacerequest", srObj.Name, "namespace", srObj.Namespace)
		return []reconcile.Request{}
	}

	if dt == nil {
		return []reconcile.Request{}
	}

	return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(dt)}}

}
