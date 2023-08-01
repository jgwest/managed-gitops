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

	"github.com/go-logr/logr"
	applicationv1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	apierr "k8s.io/apimachinery/pkg/api/errors"
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

// DeploymentTargetClaimReconciler reconciles a DeploymentTargetClaim object
type DeploymentTargetClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargetclaims/finalizers,verbs=update
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargets,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=deploymenttargets/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Modify the Reconcile function to compare the state specified by
// the DeploymentTargetClaim object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *DeploymentTargetClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtc := applicationv1alpha1.DeploymentTargetClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
	}

	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc); err != nil {
		// Don't requeue if the requested object is not found/deleted.
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Add the deletion finalizer if it is absent.
	if addFinalizer(&dtc, applicationv1alpha1.FinalizerBinder) {
		if err := r.Client.Update(ctx, &dtc); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer %s to DeploymentTargetClaim %s in namespace %s: %v", applicationv1alpha1.FinalizerBinder, dtc.Name, dtc.Namespace, err)
		}
		log.Info("Added finalizer to DeploymentTargetClaim", "finalizer", applicationv1alpha1.FinalizerBinder)
	}

	// Handle deletion if the DTC has a deletion timestamp set.
	if dtc.GetDeletionTimestamp() != nil {
		log.Info("Handling a deleted DeploymentTargetClaim")
		// If the DTC is bound set, handle the corresponding DT
		if isDTCBound(dtc) {
			dt, err := getDTBoundByDTC(ctx, r.Client, dtc)
			if err != nil && !apierr.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			if dt != nil {
				var dtcls applicationv1alpha1.DeploymentTargetClass
				if dtcls, err = findMatchingDTClassForDT(ctx, *dt, r.Client); err != nil {
					return ctrl.Result{}, err
				}

				if dtcls.Name == "" {
					log.Error(nil, "unable to locate DeploymentTargetClass matching deploymentTargetClassName", "className", dt.Spec.DeploymentTargetClassName)
					return ctrl.Result{}, nil
				}

				// Add the deletion finalizer to the DT if it is absent.
				if addFinalizer(dt, FinalizerDT) {
					if err = r.Client.Update(ctx, dt); err != nil {
						return ctrl.Result{}, fmt.Errorf("failed to add finalizer %s to DeploymentTarget %s in namespace %s: %v", FinalizerDT, dt.Name, dt.Namespace, err)
					}
					log.Info("Added finalizer to DeploymentTarget", "finalizer", FinalizerDT)
				}

				if dtcls.Spec.ReclaimPolicy == applicationv1alpha1.ReclaimPolicy_Delete {
					log.Info("ReclaimPolicy is ReclaimPolicy_Delete")
					err = r.Client.Delete(ctx, dt)
					if err != nil {
						return ctrl.Result{}, err
					}
					log.Info("DeploymentTarget is marked to Deleted", "DeploymentTarget", dt.Name)

					logutil.LogAPIResourceChangeEvent(dt.Namespace, dt.Name, dt, logutil.ResourceDeleted, log)

				} else if dtcls.Spec.ReclaimPolicy == applicationv1alpha1.ReclaimPolicy_Retain {
					log.Info("ReclaimPolicy is ReclaimPolicy_Retain")
					if removeFinalizer(&dtc, applicationv1alpha1.FinalizerBinder) {
						if err := r.Client.Update(ctx, &dtc); err != nil {
							return ctrl.Result{}, fmt.Errorf("failed to remove finalizer %s from DeploymentTargetClaim %s in namespace %s: %v", applicationv1alpha1.FinalizerBinder, dtc.Name, dtc.Namespace, err)
						}
						log.Info("Removed finalizer from DeploymentTargetClaim", "finalizer", applicationv1alpha1.FinalizerBinder)
					}

					dt.Spec.ClaimRef = ""
					if err := r.Client.Update(ctx, dt); err != nil {
						return ctrl.Result{}, fmt.Errorf("failed to update the claimRef: %v", err)
					}

					log.Info("ClaimRef of DeploymentTarget is unset since its corresponding DeploymentTargetClaim is already deleted", "DeploymentTarget", dt.Name)

					logutil.LogAPIResourceChangeEvent(dt.Namespace, dt.Name, dt, logutil.ResourceModified, log)

					err := updateDTStatusPhase(ctx, r.Client, dt, applicationv1alpha1.DeploymentTargetPhase_Released, log)
					if err != nil {
						return ctrl.Result{}, fmt.Errorf("failed to update DeploymentTarget %s in namespace %s to Released status", dt.Name, dt.Namespace)
					}
				} else {
					log.Error(nil, "the ReclaimPolicy is neither Delete nor Retain")
				}
				return ctrl.Result{}, nil
			}
		}
		if removeFinalizer(&dtc, applicationv1alpha1.FinalizerBinder) {
			if err := r.Client.Update(ctx, &dtc); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer %s from DeploymentTargetClaim %s in namespace %s: %v", applicationv1alpha1.FinalizerBinder, dtc.Name, dtc.Namespace, err)
			}
			log.Info("Removed finalizer from DeploymentTargetClaim", "finalizer", applicationv1alpha1.FinalizerBinder)
			return ctrl.Result{}, nil
		}
	}

	// If the binding is already done, we need to check if the DTC is still bound to a DT
	// and update the status accordingly
	if isBindingCompleted(dtc) {
		if err := handleBoundedDeploymentTargetClaim(ctx, r.Client, dtc, log); err != nil {
			log.Error(err, "failed to process bounded DeploymentTargetClaim")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// If the user doesn't set the DT, check if there is a matching DT available
	// or if it needs to be dynamically provisioned.
	if dtc.Spec.TargetName == "" {
		dt, err := findMatchingDTForDTC(ctx, r.Client, dtc)
		if err != nil {
			log.Error(err, "failed to find a DeploymentTarget that matches the DeploymentTargetClaim")
			return ctrl.Result{}, err
		}

		// If a best match DT is available bind it to the current DTC.
		if dt != nil {
			log.Info("Found a matching DeploymentTarget for DeploymentTargetClaim", "DeploymentTarget", dt.Name)
			err = bindDeploymentTargetClaimToTarget(ctx, r.Client, &dtc, dt, true, log)
			if err != nil {
				log.Error(err, "failed to bind DeploymentTargetClaim to the DeploymentTarget", "DeploymentTargetName", dt.Name, "Namespace", dt.Name)
				return ctrl.Result{}, err
			}

			log.Info("DeploymentTargetClaim bound to DeploymentTarget", "DeploymentTargetName", dt.Name, "Namespace", dt.Namespace)

			return ctrl.Result{}, nil
		}

		err = handleDynamicDTCProvisioning(ctx, r.Client, &dtc, log)
		if err != nil {
			log.Error(err, "failed to handle DeploymentTargetClaim for dynamic provisioning")
			return ctrl.Result{}, err
		}
		log.Info("Waiting for the DeploymentTarget to be dynamically created by the provisioner")

		return ctrl.Result{}, nil
	}

	// Get the DT specified by the user in the DTC
	dt := applicationv1alpha1.DeploymentTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dtc.Spec.TargetName,
			Namespace: dtc.Namespace,
		},
	}

	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(&dt), &dt); err != nil {
		if apierr.IsNotFound(err) {
			log.Info("Waiting for DeploymentTarget to be created", "DeploymentTarget", dt.Name, "Namespace", dt.Namespace)

			// Update the DTC status as Pending and wait for DT to be created.
			if err := updateDTCStatusPhase(ctx, r.Client, &dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Pending, log); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err

	}

	if dt.Spec.ClaimRef != "" {
		if dt.Spec.ClaimRef == dtc.Name {
			// Both DT and DTC refer each other. So bind them together.
			err := bindDeploymentTargetClaimToTarget(ctx, r.Client, &dtc, &dt, false, log)
			if err != nil {
				log.Error(err, "failed to bind DeploymentTargetClaim to the DeploymentTarget", "DeploymentTargetName", dt.Name, "Namespace", dt.Name)
				return ctrl.Result{}, err
			}
		} else {
			log.Error(nil, "DeploymentTargetClaim wants to claim a DeploymentTarget that is already claimed", "DeploymentTarget", dt.Name)

			// Update the DTC status to Pending since the DT is not available
			if err := updateDTCStatusPhase(ctx, r.Client, &dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Pending, log); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}
	} else {
		// At this stage, DT isn't claimed by anyone. The current DTC can try to claim it.
		if err := doesDTMatchDTC(dt, dtc); err != nil {
			log.Error(err, "DeploymentTarget does not match the specified DeploymentTargetClaim")
			return ctrl.Result{}, err
		}

		err := bindDeploymentTargetClaimToTarget(ctx, r.Client, &dtc, &dt, false, log)
		if err != nil {
			log.Error(err, "failed to bind DeploymentTargetClaim to the DeploymentTarget", "DeploymentTargetName", dt.Name, "Namespace", dt.Name)
			return ctrl.Result{}, err
		}
	}

	log.Info("DeploymentTargetClaim bound to DeploymentTarget", "DeploymentTargetName", dt.Name, "Namespace", dt.Namespace)

	return ctrl.Result{}, nil
}

// bindDeploymentTargetClaimToTarget binds the given DeploymentTarget to a DeploymentTargetClaim by
// setting the dtc.spec.targetName to the DT name and adding the "bound-by-controller" annotation to the DTC.
// It also updates the phase of the DT and DTC to bound.
func bindDeploymentTargetClaimToTarget(ctx context.Context, k8sClient client.Client, dtc *applicationv1alpha1.DeploymentTargetClaim, dt *applicationv1alpha1.DeploymentTarget, isBoundByController bool, log logr.Logger) error {

	// Add bound-by-controller annotation if we are binding a DT that was matched by the binding controller.
	if isBoundByController {
		updated := false
		if dtc.Spec.TargetName != dt.Name {
			dtc.Spec.TargetName = dt.Name
			updated = true
		}

		if dtc.Annotations == nil {
			dtc.Annotations = map[string]string{}
		}

		val, found := dtc.Annotations[applicationv1alpha1.AnnBoundByController]
		if !found && val != applicationv1alpha1.AnnBinderValueTrue {
			dtc.Annotations[applicationv1alpha1.AnnBoundByController] = applicationv1alpha1.AnnBinderValueTrue
			updated = true
		}

		if updated {
			if err := k8sClient.Update(ctx, dtc); err != nil {
				return err
			}
			logutil.LogAPIResourceChangeEvent(dtc.Namespace, dtc.Name, dtc, logutil.ResourceModified, log)

			log.Info("Added bound-by-controller annotation and/or updated the target name for DeploymentTargetClaim since the binding controller found the matching DeploymentTarget")
		}
	}

	// Set the status of DT and DTC to Bound
	err := updateDTCStatusPhase(ctx, k8sClient, dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Bound, log)
	if err != nil {
		return err
	}

	err = updateDTStatusPhase(ctx, k8sClient, dt, applicationv1alpha1.DeploymentTargetPhase_Bound, log)
	if err != nil {
		return err
	}

	if dtc.Annotations == nil {
		dtc.Annotations = map[string]string{}
	}

	// Add bind complete annotation to indicate that the binding process is complete.
	val, found := dtc.Annotations[applicationv1alpha1.AnnBindCompleted]
	if !found && val != applicationv1alpha1.AnnBinderValueTrue {
		dtc.Annotations[applicationv1alpha1.AnnBindCompleted] = applicationv1alpha1.AnnBinderValueTrue

		if err := k8sClient.Update(ctx, dtc); err != nil {
			return err
		}

		log.Info("Added bind-complete annotation since the DeploymentTargetClaim is bounded", "annotation", applicationv1alpha1.AnnBindCompleted)
		return nil
	}

	return nil
}

// handleBoundedDeploymentTargetClaim handles the DTCs that are already bounded i.e have the "bind-complete" annotation.
// It checks if the DTC is still bound to DTC and updates the status accordingly.
func handleBoundedDeploymentTargetClaim(ctx context.Context, k8sClient client.Client, dtc applicationv1alpha1.DeploymentTargetClaim, log logr.Logger) error {
	if !isBindingCompleted(dtc) {
		return nil
	}

	log.Info("Handling a bounded DeploymentTargetClaim")

	// If the provisioner annotation is present, check if its value still matches the DTC class name.
	provisioner, found := dtc.Annotations[applicationv1alpha1.AnnTargetProvisioner]
	if found {
		log.Info("Provisioner annotation found for DeploymentTargetClaim", "annotation", applicationv1alpha1.AnnTargetProvisioner)
		if dtc.Spec.DeploymentTargetClassName == "" {
			// If the class name doesn't exist remove the provisioner annotation
			delete(dtc.Annotations, applicationv1alpha1.AnnTargetProvisioner)
			if err := k8sClient.Update(ctx, &dtc); err != nil {
				return err
			}
			log.Info("Deleted the provisioner annotation from DeploymentTargetClaim because the class name was not set", "annotation", applicationv1alpha1.AnnTargetProvisioner)
		} else if provisioner != string(dtc.Spec.DeploymentTargetClassName) {
			// If the class name exists, but doesn't match the provisioner value
			// update the annotation with the correct provisioner value.
			dtc.Annotations[applicationv1alpha1.AnnTargetProvisioner] = string(dtc.Spec.DeploymentTargetClassName)
			if err := k8sClient.Update(ctx, &dtc); err != nil {
				return err
			}
			log.Info("Updated the provisioner annotation with the correct class name", "annotation", applicationv1alpha1.AnnTargetProvisioner, "className", string(dtc.Spec.DeploymentTargetClassName))
		}
	}

	dt, err := getDTBoundByDTC(ctx, k8sClient, dtc)
	if err != nil && !apierr.IsNotFound(err) {
		return fmt.Errorf("failed to get a DeploymentTarget for the given DeploymentTargetClaim %s", dtc.Name)
	}

	if dt == nil {
		log.Info("DeploymentTarget not found for a bounded DeploymentTargetClaim")

		// DeploymentTarget is not found for the DeploymentTargetClaim, so update the status as Lost
		err := updateDTCStatusPhase(ctx, k8sClient, &dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Lost, log)
		if err != nil {
			return err
		}

		return fmt.Errorf("DeploymentTarget not found for a bounded DeploymentTargetClaim %s in namespace %s", dtc.Name, dtc.Namespace)
	}

	log.Info("DeploymentTarget found for a bounded DeploymentTargetClaim", "DeploymentTargetName", dt.Name)

	// At this stage, the DeploymentTarget exists, so update the status to Bound.
	if err := updateDTStatusPhase(ctx, k8sClient, dt, applicationv1alpha1.DeploymentTargetPhase_Bound, log); err != nil {
		return err
	}

	return updateDTCStatusPhase(ctx, k8sClient, &dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Bound, log)

}

// handleDynamicDTCProvisioning processes the DeploymentTargetClaim for dynamic provisioning.
func handleDynamicDTCProvisioning(ctx context.Context, k8sClient client.Client, dtc *applicationv1alpha1.DeploymentTargetClaim, log logr.Logger) error {
	log.Info("Handling the DeploymentTargetClaim for dynamic provisioning")

	// If DTC is not configured with a class name, update its status as Pending and return.
	if dtc.Spec.DeploymentTargetClassName == "" {
		log.Info("DeploymentTargetClaim cannot be dynamically provisioned since DeploymentTargetClassName is not set")
		return updateDTCStatusPhase(ctx, k8sClient, dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Pending, log)
	}

	// DTC is configured with a class name. So mark the DTC for dynamic provisioning.
	if dtc.Annotations == nil {
		dtc.Annotations = map[string]string{}
	}

	dtc.Annotations[applicationv1alpha1.AnnTargetProvisioner] = string(dtc.Spec.DeploymentTargetClassName)

	if err := k8sClient.Update(ctx, dtc); err != nil {
		return err
	}

	log.Info("Added the provisioner annotation to the DeploymentTargetClaim", "annotation", applicationv1alpha1.AnnTargetProvisioner)

	// set the DTC to Pending phase and wait for the Provisioner to create a DT
	return updateDTCStatusPhase(ctx, k8sClient, dtc, applicationv1alpha1.DeploymentTargetClaimPhase_Pending, log)
}

// findMatchingDTForDTC tries to find a DT that matches the given DTC in a namespace.
// NOTE:
// - this function will only return DT that are 'Available' in .status.phase
// - this function returns nil if no matching DT was found.
func findMatchingDTForDTC(ctx context.Context, k8sClient client.Client, dtc applicationv1alpha1.DeploymentTargetClaim) (*applicationv1alpha1.DeploymentTarget, error) {
	dtList := applicationv1alpha1.DeploymentTargetList{}
	if err := k8sClient.List(ctx, &dtList, &client.ListOptions{Namespace: dtc.Namespace}); err != nil {
		return nil, err
	}

	var matcher func(dt applicationv1alpha1.DeploymentTarget) bool
	if isMarkedForDynamicProvisioning(dtc) {
		// Check if there is a matching DT created by the provisioner
		matcher = func(dt applicationv1alpha1.DeploymentTarget) bool {
			return dt.Spec.ClaimRef == dtc.Name && doesDTMatchDTC(dt, dtc) == nil
		}
	} else {
		// Check if there is a matching DT created by the user
		matcher = func(dt applicationv1alpha1.DeploymentTarget) bool {
			return doesDTMatchDTC(dt, dtc) == nil
		}
	}

	var dt *applicationv1alpha1.DeploymentTarget
	for i, d := range dtList.Items {
		if matcher(d) {
			dt = &dtList.Items[i]
			break
		}
	}

	return dt, nil
}

// A DT matches a given DTC if it satisfies the below conditions
// 1. Both DT and DTC belong to the same class.
// 2. DT should be in Available phase and should not have a different claim ref.
// 3. DT should have the cluster credentials.
func doesDTMatchDTC(dt applicationv1alpha1.DeploymentTarget, dtc applicationv1alpha1.DeploymentTargetClaim) (err error) {
	mismatchErr := mismatchErrWrap(dt.Name, dtc.Name, dtc.Namespace)

	if dt.Spec.ClaimRef != "" && dt.Spec.ClaimRef != dtc.Name {
		return mismatchErr("DeploymentTarget already references another DeploymentTargetClaim via DeploymentTarget's .spec.claimRef field")
	}

	if dt.Spec.DeploymentTargetClassName != dtc.Spec.DeploymentTargetClassName {
		return mismatchErr("deploymentTargetClassName does not match")
	}

	if dt.Status.Phase != "" && dt.Status.Phase != applicationv1alpha1.DeploymentTargetPhase_Available {
		return mismatchErr("DeploymentTarget is not in Available phase")
	}

	if dt.Spec.KubernetesClusterCredentials == (applicationv1alpha1.DeploymentTargetKubernetesClusterCredentials{}) {
		return mismatchErr("DeploymentTarget does not have cluster credentials")
	}

	if err := checkForBindingConflict(dtc, dt); err != nil {
		return err
	}

	return nil
}

func mismatchErrWrap(dtName, dtcName, ns string) func(string) error {
	return func(msg string) error {
		return fmt.Errorf("DeploymentTarget %s does not match DeploymentTargetClaim %s in namespace %s: %s", dtName, dtcName, ns, msg)
	}
}

func updateDTCStatusPhase(ctx context.Context, k8sClient client.Client, dtc *applicationv1alpha1.DeploymentTargetClaim, targetPhase applicationv1alpha1.DeploymentTargetClaimPhase, log logr.Logger) error {
	if dtc.Status.Phase == targetPhase {
		return nil
	}

	dtc.Status.Phase = targetPhase

	if err := k8sClient.Status().Update(ctx, dtc); err != nil {
		return err
	}

	log.Info("Updated the status of DeploymentTargetClaim to phase", "phase", dtc.Status.Phase)
	return nil
}

func updateDTStatusPhase(ctx context.Context, k8sClient client.Client, dt *applicationv1alpha1.DeploymentTarget, targetPhase applicationv1alpha1.DeploymentTargetPhase, log logr.Logger) error {
	if dt.Status.Phase == targetPhase {
		return nil
	}

	dt.Status.Phase = targetPhase

	if err := k8sClient.Status().Update(ctx, dt); err != nil {
		return err
	}

	log.Info("Updated the status of DeploymentTarget to phase", "DeploymentTarget", dt.Name, "phase", dt.Status.Phase)
	return nil
}

func isBindingCompleted(dtc applicationv1alpha1.DeploymentTargetClaim) bool {
	if dtc.Annotations == nil {
		return false
	}

	_, found := dtc.Annotations[applicationv1alpha1.AnnBindCompleted]
	return found
}

func isDTCBound(dtc applicationv1alpha1.DeploymentTargetClaim) bool {
	return isBindingCompleted(dtc) && dtc.Status.Phase == applicationv1alpha1.DeploymentTargetClaimPhase_Bound
}

func isMarkedForDynamicProvisioning(dtc applicationv1alpha1.DeploymentTargetClaim) bool {
	if dtc.Annotations == nil {
		return false
	}

	_, found := dtc.Annotations[applicationv1alpha1.AnnTargetProvisioner]
	return found
}

func addFinalizer(obj client.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	for _, f := range finalizers {
		if f == finalizer {
			return false
		}
	}

	finalizers = append(finalizers, finalizer)
	obj.SetFinalizers(finalizers)
	return true
}

func removeFinalizer(obj client.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	for i, f := range finalizers {
		if f == finalizer {
			finalizers = append(finalizers[:i], finalizers[i+1:]...)
			obj.SetFinalizers(finalizers)
			return true
		}
	}
	return false
}

// getDTBoundByDTC will get the DT that is bound to a given DTC.
// It returns the DT targeted by DTC if DTC.Spec.TargetName is set.
// Else it will fetch the DT that is claiming the current DTC.
func getDTBoundByDTC(ctx context.Context, k8sClient client.Client, dtc applicationv1alpha1.DeploymentTargetClaim) (*applicationv1alpha1.DeploymentTarget, error) {
	if dtc.Spec.TargetName != "" {
		dt := &applicationv1alpha1.DeploymentTarget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dtc.Spec.TargetName,
				Namespace: dtc.Namespace,
			},
		}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(dt), dt); err != nil {
			return nil, err
		}

		if err := checkForBindingConflict(dtc, *dt); err != nil {
			return nil, err
		}

		return dt, nil
	}

	dtList := applicationv1alpha1.DeploymentTargetList{}
	if err := k8sClient.List(ctx, &dtList, &client.ListOptions{Namespace: dtc.Namespace}); err != nil {
		return nil, err
	}

	var dt *applicationv1alpha1.DeploymentTarget
	for i, d := range dtList.Items {
		if d.Spec.ClaimRef == dtc.Name {
			if dt == nil {
				dt = &dtList.Items[i]
			} else {
				// There shouldn't be multiple DTs bounded to a single DTC since there is
				// a 1:1 relationship between them.
				return nil, fmt.Errorf("multiple DeploymentTargets found for a bounded DeploymentTargetClaim %s", dtc.Name)
			}
		}
	}

	return dt, nil
}

// checkForBindingConflict checks for the following conflicts:
// 1. DTC targets a DT that has a claimRef to a different DTC.
// 2. DTC has empty target but DT has a claimRef to a different DTC.
// 3. DT has a claim ref to a DTC but the DTC targets a different DT.
// 4. DT has empty claim ref but DTC targets a different DT.
func checkForBindingConflict(dtc applicationv1alpha1.DeploymentTargetClaim, dt applicationv1alpha1.DeploymentTarget) error {
	conflictErr := conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)

	if dtc.Spec.TargetName == dt.Name || dtc.Spec.TargetName == "" {
		if dt.Spec.ClaimRef != "" && dt.Spec.ClaimRef != dtc.Name {
			return conflictErr("DeploymentTarget has a claimRef to another DeploymentTargetClaim")
		}
	}

	if dt.Spec.ClaimRef == dtc.Name || dt.Spec.ClaimRef == "" {
		if dtc.Spec.TargetName != "" && dtc.Spec.TargetName != dt.Name {
			return conflictErr("DeploymentTargetClaim targets a DeploymentTarget that is already claimed")
		}
	}

	return nil
}

func conflictErrWrapper(dtcName, target, dtName, claimRef string) func(string) error {
	return func(msg string) error {
		return fmt.Errorf("cannot bind DeploymentTargetClaim %s with target %q to DeploymentTarget %s with claim ref %q: %s", dtcName, target, dtName, claimRef, msg)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentTargetClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&applicationv1alpha1.DeploymentTargetClaim{}).
		Watches(
			&applicationv1alpha1.DeploymentTarget{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForDeploymentTarget),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Map all incoming DT events to corresponding DTC requests to be handled by the Reconciler.
func (r *DeploymentTargetClaimReconciler) findObjectsForDeploymentTarget(ctx context.Context, dt client.Object) []reconcile.Request {

	handlerLog := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops)

	dtObj, isOk := dt.(*applicationv1alpha1.DeploymentTarget)
	if !isOk {
		handlerLog.Error(nil, "SEVERE: type mismatch in mapping function. Expected an event for DeploymentTarget object")
		return []reconcile.Request{}
	}

	dtcList := applicationv1alpha1.DeploymentTargetClaimList{}
	err := r.List(ctx, &dtcList, &client.ListOptions{Namespace: dt.GetNamespace()})
	if err != nil {
		handlerLog.Error(err, "failed to list DeploymentTargetClaims in the mapping function")
		return []reconcile.Request{}
	}

	requests := []reconcile.Request{}
	for _, d := range dtcList.Items {
		dtc := d
		// We only want to reconcile for DTs that have a corresponding DTC.
		if dtc.Spec.TargetName == dt.GetName() || dtObj.Spec.ClaimRef == dtc.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&dtc),
			})
		}
	}

	return requests
}
