package appstudioredhatcom

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appstudiosharedv1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/tests"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Test DeploymentTargetClaimBinderController", func() {
	Context("Testing DeploymentTargetClaimBinderController", func() {

		var (
			ctx        context.Context
			k8sClient  client.Client
			reconciler DeploymentTargetClaimReconciler
		)

		BeforeEach(func() {
			ctx = context.Background()

			scheme,
				_,
				_,
				_,
				err := tests.GenericTestSetup()
			Expect(err).To(BeNil())

			err = appstudiosharedv1.AddToScheme(scheme)
			Expect(err).To(BeNil())

			testNS := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-dtc",
				},
			}

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(&testNS).Build()

			reconciler = DeploymentTargetClaimReconciler{
				Client: k8sClient,
				Scheme: scheme,
			}
		})

		Context("Test the lifecycle of a DeploymentTargetClaim", func() {

			It("should handle the deletion of a bounded DeploymentTargetClaim with class of Retain", func() {
				dt := getDeploymentTarget()
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.ObjectMeta.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
					dtc.Spec.TargetName = dt.Name
				},
				)

				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dtcls := generateDeploymentTargetClass()
				err = k8sClient.Create(ctx, &dtcls)
				Expect(err).To(BeNil())

				By("reconcile with a DT and DTC that refer each other")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

				By("check if the binding controller has set the finalizer")
				finalizerFound := false
				for _, f := range dtc.GetFinalizers() {
					if f == appstudiosharedv1.FinalizerBinder {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("delete the DTC and verify if the DT is moved to Released phase")
				err = k8sClient.Delete(ctx, &dtc)
				Expect(err).To(BeNil())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeFalse())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeTrue())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Released))

				By("verify if the claimRef of the DT is unset")
				Expect(dt.Spec.ClaimRef).Should(BeEmpty())
			})

			It("should handle the deletion of a bounded DeploymentTargetClaim with class of Delete", func() {
				dt := getDeploymentTarget()
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.ObjectMeta.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
					dtc.Spec.TargetName = dt.Name
				},
				)

				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dtcls := generateDeploymentTargetClass(func(dtcls *appstudiosharedv1.DeploymentTargetClass) {
					dtcls.Spec.ReclaimPolicy = "Delete"
				})
				err = k8sClient.Create(ctx, &dtcls)
				Expect(err).To(BeNil())

				By("reconcile with a DT and DTC that refer each other")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

				By("check if the binding controller has set the finalizer")
				finalizerFound := false
				for _, f := range dtc.GetFinalizers() {
					if f == appstudiosharedv1.FinalizerBinder {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("delete the DTC and verify if the DT is removed")
				err = k8sClient.Delete(ctx, &dtc)
				Expect(err).To(BeNil())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeFalse())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeFalse())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(apierr.IsNotFound(err)).To(BeFalse())

				By("check if the binding controller has set the finalizer")
				finalizerFound = false
				for _, f := range dt.GetFinalizers() {
					if f == FinalizerDT {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())
			})

			It("should handle the deletion of a bounded DeploymentTargetClaim with Retain ReclaimPolicy", func() {
				dt := getDeploymentTarget()
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.ObjectMeta.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
					dtc.Spec.TargetName = dt.Name
				},
				)

				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dtcls := generateDeploymentTargetClass(func(dtcls *appstudiosharedv1.DeploymentTargetClass) {
					dtcls.Spec.ReclaimPolicy = "Retain"
				})
				err = k8sClient.Create(ctx, &dtcls)
				Expect(err).To(BeNil())

				By("reconcile with a DT and DTC that refer each other")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

				By("check if the binding controller has set the finalizer")
				finalizerFound := false
				for _, f := range dtc.GetFinalizers() {
					if f == appstudiosharedv1.FinalizerBinder {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("delete the DTC and verify if the DT is removed")
				err = k8sClient.Delete(ctx, &dtc)
				Expect(err).To(BeNil())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeFalse())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Released))

				By("check if the binding controller has set the finalizer")
				finalizerFound = false
				for _, f := range dt.GetFinalizers() {
					if f == FinalizerDT {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeTrue())
			})

			It("should handle the deletion of a bounded DeploymentTargetClaim with deleted DeploymentTarget", func() {
				dt := getDeploymentTarget()
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.ObjectMeta.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
					dtc.Spec.TargetName = dt.Name
				},
				)
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile with a DT and DTC that refer each other")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

				By("check if the binding controller has set the finalizer")
				finalizerFound := false
				for _, f := range dtc.GetFinalizers() {
					if f == appstudiosharedv1.FinalizerBinder {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("delete both DTC and DT and verify if the DTC is removed")
				err = k8sClient.Delete(ctx, &dtc)
				Expect(err).To(BeNil())

				err = k8sClient.Delete(ctx, &dt)
				Expect(err).To(BeNil())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeTrue())
			})

			It("should handle the deletion of an unbounded DeploymentTargetClaim", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile with an unbounded DTC")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DTC is Pending")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Pending))

				By("check if the binding controller has set the finalizer")
				finalizerFound := false
				for _, f := range dtc.GetFinalizers() {
					if f == appstudiosharedv1.FinalizerBinder {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("delete the DTC and verify if it is removed")
				err = k8sClient.Delete(ctx, &dtc)
				Expect(err).To(BeNil())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(apierr.IsNotFound(err)).To(BeTrue())
			})
		})

		Context("Test with DeploymentTargetClaim that was previously binded by the binding controller", func() {
			It("should handle already binded DTC and DT that doesn't have the bound phase set", func() {
				By("create a DTC with binding completed annotation but in Pending phase")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted:     appstudiosharedv1.AnnBinderValueTrue,
						appstudiosharedv1.AnnBoundByController: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Pending
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("create a DT that claims the above DTC but not in bound phase")
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				By("reconcile DT and DTC with bind-complete annotation")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))
			})

			It("should return an error and set the DTC status to Lost if DT is not found", func() {
				By("create a DTC that targets a DT that doesn't exist")
				dtc := getDeploymentTargetClaim(
					func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
						dtc.Annotations = map[string]string{
							appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
						}
						dtc.Spec.TargetName = "random-dt"
						dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
					})

				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile with a bounded DT and DTC")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).NotTo(BeNil())
				expectedErr := fmt.Errorf("DeploymentTarget not found for a bounded DeploymentTargetClaim %s in namespace %s", dtc.Name, dtc.Namespace)
				Expect(err.Error()).Should(Equal(expectedErr.Error()))
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the DTC is in Lost phase")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Lost))
			})

			It("should return an error and set the DTC status to Lost if no matching DT is found", func() {
				By("creates DTC with bounded annotation")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted: appstudiosharedv1.AnnBinderValueTrue,
					}
					dtc.Status.Phase = appstudiosharedv1.DeploymentTargetClaimPhase_Bound
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("create multiple DTs that doesn't match the given DTC")
				for i := 0; i < 3; i++ {
					dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
						dt.Name = fmt.Sprintf("test-dt-%d", i)
						dt.Spec.ClaimRef = fmt.Sprintf("test-dtc-%d", i)
						dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
					})
					err = k8sClient.Create(ctx, &dt)
					Expect(err).To(BeNil())
				}

				By("reconcile with a bounded DTC and multiple DTs that are claiming other DTCs")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).NotTo(BeNil())
				expectedErr := fmt.Errorf("DeploymentTarget not found for a bounded DeploymentTargetClaim %s in namespace %s", dtc.Name, dtc.Namespace)
				Expect(err.Error()).Should(Equal(expectedErr.Error()))
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the DTC is in Lost phase")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Lost))
			})

			It("should update the provisioner annotation if it doesn't match the class name", func() {
				By("create a bounded DT and DTC with provisioner annotation that doesn't match the class name")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted:     appstudiosharedv1.AnnBinderValueTrue,
						appstudiosharedv1.AnnTargetProvisioner: "provisioner-doesn't-exist",
					}
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
				})
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				By("reconcile and check if the provisioner annotation is updated")
				req := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, req)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())

				Expect(dtc.Annotations[appstudiosharedv1.AnnTargetProvisioner]).To(Equal(string(dtc.Spec.DeploymentTargetClassName)))

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

			})

			It("should remove the provisioner annotation if the class name is not set", func() {
				By("create a bounded DT and DTC with provisioner annotation")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.DeploymentTargetClassName = ""
					dtc.Annotations = map[string]string{
						appstudiosharedv1.AnnBindCompleted:     appstudiosharedv1.AnnBinderValueTrue,
						appstudiosharedv1.AnnTargetProvisioner: "provisioner-doesn't-exist",
					}
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
				})
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				By("reconcile and check if the provisioner annotation is removed")
				req := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, req)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())

				_, found := dtc.Annotations[appstudiosharedv1.AnnTargetProvisioner]
				Expect(found).To(BeFalse())

				By("check if the status of DT and DTC is Bound")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

			})
		})

		Context("Test DeploymentTargetClaim with no target and the binding controller finds a matching target", func() {
			It("should mark the DTC for dynamic provisioning if a matching DT is not found", func() {
				By("create a DTC without specifying the DT")
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and check if it is marked for dynamic provisioning")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if the provisioner annotation is set and the DTC status is set to pending phase")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Annotations).ShouldNot(BeNil())
				Expect(dtc.Annotations[appstudiosharedv1.AnnTargetProvisioner]).To(Equal(string(dtc.Spec.DeploymentTargetClassName)))
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Pending))

				By("verify if the bound-by-controller annotation is not set")
				Expect(dtc.Annotations[appstudiosharedv1.AnnBoundByController]).ToNot(Equal(string(appstudiosharedv1.AnnBinderValueTrue)))
			})

			It("shouldn't mark the DTC for dynamic provisioning if a matching DT is found", func() {
				By("create a DTC without specifying the DT")
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("create a DT that is available and claims the above DTC")
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				By("reconcile a DTC and verify that it isn't marked for dynamic provisioning")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if both DTC and DT are bound together")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))

				By("check if the provisioner annotation is not set")
				Expect(dtc.Annotations).ShouldNot(BeNil())
				_, found := dtc.Annotations[appstudiosharedv1.AnnTargetProvisioner]
				Expect(found).To(BeFalse())

				By("check if the bound-by-controller annotation is set")
				val, found := dtc.Annotations[appstudiosharedv1.AnnBoundByController]
				Expect(found).To(BeTrue())
				Expect(val).To(Equal(appstudiosharedv1.AnnBinderValueTrue))
			})

			It("should mark the DTC as pending if the DT isn't found and DTClass is not set", func() {
				By("create a DTC without specifying the DeploymentTargetClass")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.DeploymentTargetClassName = ""
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile a DTC and verify that it isn't marked for dynamic provisioning")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if the DTC is set to pending phase")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Pending))
				// check if the provision annotation is not set
				Expect(dtc.Annotations).Should(BeNil())
			})
		})

		Context("Test DeploymentTargetClaim with a user provided DeploymentTarget", func() {
			It("should return an error and requeue if DT pointed by DTC is not found", func() {
				By("create a DTC that points to a DT that doesn't exist")
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = "random"
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and verify if it returns an error with requeue")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if the DTC is set to pending phase")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Pending))
			})

			It("should bind if the DT is found and if it refers back to the DTC", func() {
				By("create a DTC and DT that refer each other")
				dtc := getDeploymentTargetClaim()

				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc.Spec.TargetName = dt.Name
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and verify if it returns an error with requeue")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if both DTC and DT are bound together")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Annotations).ShouldNot(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))
			})

			It("should bind if the target DT is not claimed by anyone", func() {
				By("create a DT and a DTC that claims it")
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					// DT is available for claiming
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = dt.Name
				})
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and verify if it returns an error with requeue")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if both DTC and DT are bound together")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Annotations).ShouldNot(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Bound))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Bound))
			})

			It("shouldn't bind if the DT is already claimed", func() {
				By("create a DTC and DT that is already claimed")
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = "random-dtc"
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Bound
				})
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = dt.Name
				})
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and verify if the DTC is not bound")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("verify if DTC has pending state")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())
				Expect(dtc.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetClaimPhase_Pending))
			})

			It("should return an error if the DT doesn't match the DTC", func() {
				By("create a DTC and DT that refer each other but belong to different classes")
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = dt.Name
					dtc.Spec.DeploymentTargetClassName = appstudiosharedv1.DeploymentTargetClassName("random")
				})
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				By("reconcile and verify if it returns an error with requeue")
				request := newRequest(dtc.Namespace, dtc.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).ToNot(BeNil())
				expectedErr := fmt.Errorf("DeploymentTarget %s does not match DeploymentTargetClaim %s in namespace %s: deploymentTargetClassName does not match", dt.Name, dtc.Name, dtc.Namespace)
				Expect(err.Error()).To(Equal(expectedErr.Error()))
				Expect(res).To(Equal(ctrl.Result{}))
			})
		})

		Context("Test GetBoundByDTC function", func() {
			It("get the DT specified as a target in the DTC", func() {
				dt := getDeploymentTarget()
				err := k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = dt.Name
				})
				err = k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				boundedDT, err := getDTBoundByDTC(ctx, k8sClient, dtc)
				Expect(err).To(BeNil())
				Expect(boundedDT).ToNot(BeNil())
				Expect(client.ObjectKeyFromObject(boundedDT)).To(Equal(client.ObjectKeyFromObject(&dt)))
			})

			It("shouldn't return a DT if it absent", func() {
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = "random-dt"
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				boundedDT, err := getDTBoundByDTC(ctx, k8sClient, dtc)
				Expect(apierr.IsNotFound(err)).To(BeTrue())
				Expect(boundedDT).To(BeNil())
			})

			It("get the DT that refers the DTC in a claim ref", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
				})
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				// create another DT that doesn't refer the DTC
				fakedt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Name = "fake-dt"
				})
				err = k8sClient.Create(ctx, &fakedt)
				Expect(err).To(BeNil())

				boundedDT, err := getDTBoundByDTC(ctx, k8sClient, dtc)
				Expect(err).To(BeNil())
				Expect(boundedDT).ToNot(BeNil())
				Expect(client.ObjectKeyFromObject(boundedDT)).To(Equal(client.ObjectKeyFromObject(&dt)))
			})

			It("should return an error if there are multiple DTs associated with a DTC", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt1 := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Name = "test-dt-1"
					dt.Spec.ClaimRef = dtc.Name
				})
				err = k8sClient.Create(ctx, &dt1)
				Expect(err).To(BeNil())

				By("create a second DT that claims the same DTC")
				dt2 := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Name = "test-dt-2"
					dt.Spec.ClaimRef = dtc.Name
				})
				err = k8sClient.Create(ctx, &dt2)
				Expect(err).To(BeNil())

				By("check if an error is a second DT is detected and an error is returned")
				dt, err := getDTBoundByDTC(ctx, k8sClient, dtc)
				Expect(dt).To(BeNil())
				Expect(err).ToNot(BeNil())
				expectedErr := fmt.Errorf("multiple DeploymentTargets found for a bounded DeploymentTargetClaim %s", dtc.Name)
				Expect(err.Error()).To(Equal(expectedErr.Error()))

			})
		})

		Context("Test doesDTMatchDTC function", func() {
			var (
				dtc         appstudiosharedv1.DeploymentTargetClaim
				dt          appstudiosharedv1.DeploymentTarget
				mismatchErr func(string) error
				conflictErr func(string) error
			)
			BeforeEach(func() {
				dtc = getDeploymentTargetClaim()
				dt = getDeploymentTarget()
				mismatchErr = mismatchErrWrap(dt.Name, dtc.Name, dtc.Namespace)
				conflictErr = conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
			})

			It("DT and DTC match", func() {
				dt.Spec.ClaimRef = dtc.Name
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available

				err := doesDTMatchDTC(dt, dtc)
				Expect(err).To(BeNil())

				// check if it still matches without phase
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available

				err = doesDTMatchDTC(dt, dtc)
				Expect(err).To(BeNil())
			})

			It("should return an error if the classes don't match", func() {
				dt.Spec.ClaimRef = dtc.Name
				dt.Spec.DeploymentTargetClassName = "different-class"
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available

				expecterErr := mismatchErr("deploymentTargetClassName does not match")
				err := doesDTMatchDTC(dt, dtc)
				Expect(err).ToNot(BeNil())
				Expect(err.Error()).To(Equal(expecterErr.Error()))
			})

			It("should return an error if DT is not in available phase", func() {
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Bound

				expecterErr := mismatchErr("DeploymentTarget is not in Available phase")
				err := doesDTMatchDTC(dt, dtc)
				Expect(err).ToNot(BeNil())
				Expect(err.Error()).To(Equal(expecterErr.Error()))
			})

			It("should return an error if DT doesn't have cluter credentials", func() {
				dt.Spec.ClaimRef = dtc.Name
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				dt.Spec.KubernetesClusterCredentials = appstudiosharedv1.DeploymentTargetKubernetesClusterCredentials{}

				expecterErr := mismatchErr("DeploymentTarget does not have cluster credentials")
				err := doesDTMatchDTC(dt, dtc)
				Expect(err).ToNot(BeNil())
				Expect(err.Error()).To(Equal(expecterErr.Error()))
			})

			It("should return an error if there is a binding conflict", func() {
				dt.Spec.ClaimRef = dtc.Name
				dtc.Spec.TargetName = "different-dt"
				dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available

				err := doesDTMatchDTC(dt, dtc)
				conflictErr = conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
				expecterErr := conflictErr("DeploymentTargetClaim targets a DeploymentTarget that is already claimed")
				Expect(err).ToNot(BeNil())
				Expect(err.Error()).To(Equal(expecterErr.Error()))
			})

		})

		Context("Test checkForBindingConflict function", func() {
			It("should return an error if DTC targets a DT that has a claimRef to a different DTC.", func() {
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = "random-dtc"
				})

				dtc := getDeploymentTargetClaim(
					func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
						dtc.Spec.TargetName = dt.Name
					},
				)

				err := checkForBindingConflict(dtc, dt)
				Expect(err).NotTo(BeNil())
				errWrap := conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
				expectedErr := errWrap("DeploymentTarget has a claimRef to another DeploymentTargetClaim")
				Expect(err.Error()).To(Equal(expectedErr.Error()))
			})

			It("should return an error if DTC has empty target but DT has a claimRef to a different DTC.", func() {
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = "random-dtc"
				})
				dtc := getDeploymentTargetClaim()

				err := checkForBindingConflict(dtc, dt)
				Expect(err).NotTo(BeNil())
				errWrap := conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
				expectedErr := errWrap("DeploymentTarget has a claimRef to another DeploymentTargetClaim")
				Expect(err.Error()).To(Equal(expectedErr.Error()))
			})

			It("should return an error if DT has a claim ref to a DTC but the DTC targets a different DT", func() {
				dtc := getDeploymentTargetClaim(
					func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
						dtc.Spec.TargetName = "random-dt"
					},
				)
				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
				})

				err := checkForBindingConflict(dtc, dt)
				Expect(err).NotTo(BeNil())
				errWrap := conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
				expectedErr := errWrap("DeploymentTargetClaim targets a DeploymentTarget that is already claimed")
				Expect(err.Error()).To(Equal(expectedErr.Error()))
			})

			It("should return an error if DT has empty claim ref but DTC targets a different DT.", func() {
				dtc := getDeploymentTargetClaim(
					func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
						dtc.Spec.TargetName = "random-dt"
					},
				)
				dt := getDeploymentTarget()

				err := checkForBindingConflict(dtc, dt)
				errWrap := conflictErrWrapper(dtc.Name, dtc.Spec.TargetName, dt.Name, dt.Spec.ClaimRef)
				expectedErr := errWrap("DeploymentTargetClaim targets a DeploymentTarget that is already claimed")
				Expect(err.Error()).To(Equal(expectedErr.Error()))
			})

			It("should not return an error if there are no conflicts", func() {
				dtc := getDeploymentTargetClaim()
				dt := getDeploymentTarget()

				err := checkForBindingConflict(dtc, dt)
				Expect(err).To(BeNil())
			})
		})

		Context("Test findMatchingDTForDTC function", func() {
			It("should match a dynamically provisioned matching DT if found", func() {
				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Annotations = map[string]string{
						appstudiosharedv1.AnnTargetProvisioner: "sandbox-provisioner",
					}
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				expected := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err = k8sClient.Create(ctx, &expected)
				Expect(err).To(BeNil())

				dt, err := findMatchingDTForDTC(ctx, k8sClient, dtc)
				Expect(err).To(BeNil())
				Expect(client.ObjectKeyFromObject(dt)).To(Equal(client.ObjectKeyFromObject(&expected)))
			})

			It("should match a user created matching DT if found", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				expected := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Status.Phase = appstudiosharedv1.DeploymentTargetPhase_Available
				})
				err = k8sClient.Create(ctx, &expected)
				Expect(err).To(BeNil())

				dt, err := findMatchingDTForDTC(ctx, k8sClient, dtc)
				Expect(err).To(BeNil())
				Expect(client.ObjectKeyFromObject(dt)).To(Equal(client.ObjectKeyFromObject(&expected)))
			})

			It("no matching DT is found", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt, err := findMatchingDTForDTC(ctx, k8sClient, dtc)
				Expect(err).To(BeNil())
				Expect(dt).To(BeNil())
			})
		})

		Context("Test bindDeploymentTargetCliamToTarget function", func() {
			var (
				dt     appstudiosharedv1.DeploymentTarget
				dtc    appstudiosharedv1.DeploymentTargetClaim
				logger logr.Logger
			)

			BeforeEach(func() {
				dtc = getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())
				dt = getDeploymentTarget()
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				logger = log.FromContext(ctx)
			})

			It("should bind DT and DTC with bound-by-controller annotation", func() {
				err := bindDeploymentTargetClaimToTarget(ctx, k8sClient, &dtc, &dt, true, logger)
				Expect(err).To(BeNil())

				// verify if bound complete and bound-by-controller annotations are set
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())

				Expect(isBindingCompleted(dtc)).To(BeTrue())
				Expect(dtc.Annotations[appstudiosharedv1.AnnBoundByController]).To(Equal(appstudiosharedv1.AnnBinderValueTrue))
			})

			It("should bind DT and DTC without bound-by-controller annotation", func() {
				err := bindDeploymentTargetClaimToTarget(ctx, k8sClient, &dtc, &dt, false, logger)
				Expect(err).To(BeNil())

				// verify if bound complete and bound-by-controller annotations are set
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dtc), &dtc)
				Expect(err).To(BeNil())

				Expect(isBindingCompleted(dtc)).To(BeTrue())
				_, found := dtc.Annotations[appstudiosharedv1.AnnBoundByController]
				Expect(found).To(BeFalse())
			})
		})

		Context("Test findObjectsForDeploymentTarget function", func() {
			It("shouldn't return requests if the incoming DT didn't match any DTC", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt := getDeploymentTarget()

				reqs := reconciler.findObjectsForDeploymentTarget(ctx, &dt)
				Expect(reqs).To(Equal([]reconcile.Request{}))
			})

			It("should return a DTC request if the incoming DT claims it", func() {
				dtc := getDeploymentTargetClaim()
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				dt := getDeploymentTarget(func(dt *appstudiosharedv1.DeploymentTarget) {
					dt.Spec.ClaimRef = dtc.Name
				})

				reqs := reconciler.findObjectsForDeploymentTarget(ctx, &dt)
				Expect(reqs).To(Equal([]reconcile.Request{
					newRequest(dtc.Namespace, dtc.Name),
				}))
			})

			It("should return a DTC request if it targets the incoming DT", func() {
				dt := getDeploymentTarget()

				dtc := getDeploymentTargetClaim(func(dtc *appstudiosharedv1.DeploymentTargetClaim) {
					dtc.Spec.TargetName = dt.Name
				})
				err := k8sClient.Create(ctx, &dtc)
				Expect(err).To(BeNil())

				reqs := reconciler.findObjectsForDeploymentTarget(ctx, &dt)
				Expect(reqs).To(Equal([]reconcile.Request{
					newRequest(dtc.Namespace, dtc.Name),
				}))
			})

			It("shouldn't return any request if no DTC was found while handling the DT event", func() {
				dt := getDeploymentTarget()

				reqs := reconciler.findObjectsForDeploymentTarget(ctx, &dt)
				Expect(reqs).To(Equal([]reconcile.Request{}))
			})

			It("shouldn't return any request if an object of different type is passed", func() {
				dtc := getDeploymentTargetClaim()

				reqs := reconciler.findObjectsForDeploymentTarget(ctx, &dtc)
				Expect(reqs).To(Equal([]reconcile.Request{}))
			})
		})

	})
})

func getDeploymentTargetClaim(ops ...func(dtc *appstudiosharedv1.DeploymentTargetClaim)) appstudiosharedv1.DeploymentTargetClaim {
	dtc := appstudiosharedv1.DeploymentTargetClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-dtc",
			Namespace:   "test-ns",
			Annotations: map[string]string{},
		},
		Spec: appstudiosharedv1.DeploymentTargetClaimSpec{
			DeploymentTargetClassName: appstudiosharedv1.DeploymentTargetClassName("test-sandbox-class"),
		},
	}

	for _, o := range ops {
		o(&dtc)
	}

	return dtc
}

func getDeploymentTarget(ops ...func(dt *appstudiosharedv1.DeploymentTarget)) appstudiosharedv1.DeploymentTarget {
	dt := appstudiosharedv1.DeploymentTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-dt",
			Namespace:   "test-ns",
			Annotations: map[string]string{},
		},
		Spec: appstudiosharedv1.DeploymentTargetSpec{
			DeploymentTargetClassName: appstudiosharedv1.DeploymentTargetClassName("test-sandbox-class"),
			KubernetesClusterCredentials: appstudiosharedv1.DeploymentTargetKubernetesClusterCredentials{
				DefaultNamespace:         "test-ns",
				APIURL:                   "https://api-url/api",
				ClusterCredentialsSecret: "test-secret",
			},
		},
	}

	for _, o := range ops {
		o(&dt)
	}

	return dt
}

func generateDeploymentTargetClass(ops ...func(dtc *appstudiosharedv1.DeploymentTargetClass)) appstudiosharedv1.DeploymentTargetClass {
	dtcls := appstudiosharedv1.DeploymentTargetClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox-class",
			Annotations: map[string]string{},
		},
		Spec: appstudiosharedv1.DeploymentTargetClassSpec{
			Provisioner:   appstudiosharedv1.Provisioner_Devsandbox,
			ReclaimPolicy: "Retain",
		},
	}

	for _, o := range ops {
		o(&dtcls)
	}

	return dtcls
}
