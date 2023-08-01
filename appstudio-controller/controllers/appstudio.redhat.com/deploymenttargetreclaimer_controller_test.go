package appstudioredhatcom

import (
	"context"
	"time"

	codereadytoolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appstudiosharedv1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/tests"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Test DeploymentTargetReclaimController", func() {
	Context("Testing DeploymentTargetReclaimController", func() {

		var (
			ctx        context.Context
			k8sClient  client.Client
			reconciler DeploymentTargetReconciler
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
			err = codereadytoolchainv1alpha1.AddToScheme(scheme)
			Expect(err).To(BeNil())

			testNS := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-namespace",
				},
			}

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(&testNS).Build()

			reconciler = DeploymentTargetReconciler{
				Client: k8sClient,
				Scheme: scheme,
			}
		})

		Context("Test the creation of a DeploymentTarget", func() {

			When("A DeploymentTarget which is not referenced by any other CRs is created", func() {
				It("should set the finalizer on the DeploymentTarget", func() {

					By("creating a default DeploymentTargetClass")
					dtcls := generateDeploymentTargetClass(func(dtc *appstudiosharedv1.DeploymentTargetClass) {
						dtc.Spec.ReclaimPolicy = appstudiosharedv1.ReclaimPolicy_Delete
					})
					err := k8sClient.Create(ctx, &dtcls)
					Expect(err).To(BeNil())

					By("creating a default DeploymentTarget")
					dt := generateReclaimDeploymentTarget()
					err = k8sClient.Create(ctx, &dt)
					Expect(err).To(BeNil())

					By("reconcile with a DeploymentTarget")
					request := newRequest(dt.Namespace, dt.Name)
					res, err := reconciler.Reconcile(ctx, request)
					Expect(err).To(BeNil())
					Expect(res).To(Equal(ctrl.Result{}))

					By("check if the DT has set the finalizer")
					err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
					Expect(err).To(BeNil())
					finalizerFound := false
					for _, f := range dt.GetFinalizers() {
						if f == FinalizerDT {
							finalizerFound = true
							break
						}
					}
					Expect(finalizerFound).To(BeTrue())

				})
			})
		})

		Context("Test the reclaim of a DeploymentTarget", func() {

			It("Failed to delete SpaceRequest and DeploymentTarget", func() {

				By("creating a default DeploymentTargetClass")
				dtcls := generateDeploymentTargetClass(func(dtc *appstudiosharedv1.DeploymentTargetClass) {
					dtc.Spec.ReclaimPolicy = appstudiosharedv1.ReclaimPolicy_Delete
				})
				err := k8sClient.Create(ctx, &dtcls)
				Expect(err).To(BeNil())

				By("creating a default DeploymentTarget")
				dt := generateReclaimDeploymentTarget()
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())

				By("creating a SpaceRequest")
				sr := generateReclaimSpaceRequest(func(sr *codereadytoolchainv1alpha1.SpaceRequest) {
					sr.Spec.TierName = "appstudio-env"
					sr.Status.Conditions[0].Status = corev1.ConditionFalse
				})
				err = k8sClient.Create(ctx, &sr)
				Expect(err).To(BeNil())

				By("reconcile with a DeploymentTarget")
				request := newRequest(dt.Namespace, dt.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				By("check if the DT has set the finalizer")
				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				finalizerFound := false
				for _, f := range dt.GetFinalizers() {
					if f == FinalizerDT {
						finalizerFound = true
						break
					}
				}
				Expect(finalizerFound).To(BeTrue())

				By("deleting the DT")
				err = k8sClient.Delete(ctx, &dt)
				Expect(err).To(BeNil())

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{Requeue: true}), "should requeue after deleting the SpaceRequest")

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{RequeueAfter: 30 * time.Second}),
					"should requeue because the SpaceRequest hasn't been cleaned up yet")

				err = k8sClient.Delete(ctx, &sr)
				Expect(err).To(BeNil())

				// Use a mock clock and forward the time by two minutes.
				reconciler.Clock = sharedutil.NewMockClock(time.Now().Add(2 * time.Minute))

				res, err = reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Status.Phase).To(Equal(appstudiosharedv1.DeploymentTargetPhase_Failed))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&sr), &sr)
				Expect(apierr.IsNotFound(err)).To(BeFalse())
			})

			It("should unset the claimRef of DT if its corresponding DTC is deleted with Retain policy", func() {
				By("creating a default DeploymentTargetClass with Retain policy")
				dtcls := generateDeploymentTargetClass(func(dtc *appstudiosharedv1.DeploymentTargetClass) {
					dtc.Spec.ReclaimPolicy = appstudiosharedv1.ReclaimPolicy_Retain
				})
				err := k8sClient.Create(ctx, &dtcls)
				Expect(err).To(BeNil())

				By("creating a default DeploymentTarget pointing to a non-existing DTC")
				dt := generateReclaimDeploymentTarget()
				err = k8sClient.Create(ctx, &dt)
				Expect(err).To(BeNil())
				Expect(dt.Spec.ClaimRef).To(Equal("test-dtc"))

				By("reconcile and verify if the claimRef field is unset")
				request := newRequest(dt.Namespace, dt.Name)
				res, err := reconciler.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(res).To(Equal(ctrl.Result{}))

				err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
				Expect(err).To(BeNil())
				Expect(dt.Spec.ClaimRef).To(Equal(""))
			})

			When("a DT is deleted, but the SpaceRequest no longer exists", func() {

				It("should unset the finalizer from the DT", func() {

					By("creating a default DeploymentTargetClass with Delete policy")
					dtcls := generateDeploymentTargetClass(func(dtc *appstudiosharedv1.DeploymentTargetClass) {
						dtc.Spec.ReclaimPolicy = appstudiosharedv1.ReclaimPolicy_Delete
					})
					err := k8sClient.Create(ctx, &dtcls)
					Expect(err).To(BeNil())

					By("creating a default DeploymentTarget pointing to a non-existing DTC")
					dt := generateReclaimDeploymentTarget()
					dt.Finalizers = append(dt.Finalizers, FinalizerDT)
					now := metav1.Now()
					dt.DeletionTimestamp = &now
					err = k8sClient.Create(ctx, &dt)
					Expect(err).To(BeNil())

					By("creating a DTClaim that matches the DeploymentTarget")
					dtc := &appstudiosharedv1.DeploymentTargetClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-dtc",
							Namespace: "test-deployment",
						},
					}
					Expect(k8sClient.Create(ctx, dtc)).To(Succeed())

					By("not creating a SpaceRequest for the DT/DTC to reference")

					Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)).To(Succeed())

					request := newRequest(dt.Namespace, dt.Name)
					res, err := reconciler.Reconcile(ctx, request)
					Expect(err).To(BeNil())
					Expect(res).To(Equal(ctrl.Result{}))

					err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
					Expect(apierr.IsNotFound(err)).To(BeTrue(), "the finalizer should have been removed, since the SpaceRequest doesn't exist")
				})
			})

			When("a DT is deleted, but the DeploymentTargetClass is retain", func() {

				It("should unset the finalizer from the DT", func() {

					By("creating a default DeploymentTargetClass with Retain policy")
					dtcls := generateDeploymentTargetClass(func(dtc *appstudiosharedv1.DeploymentTargetClass) {
						dtc.Spec.ReclaimPolicy = appstudiosharedv1.ReclaimPolicy_Retain
					})
					err := k8sClient.Create(ctx, &dtcls)
					Expect(err).To(BeNil())

					By("creating a default DeploymentTarget pointing to a non-existing DTC")
					dt := generateReclaimDeploymentTarget()
					dt.Finalizers = append(dt.Finalizers, FinalizerDT)
					now := metav1.Now()
					dt.DeletionTimestamp = &now
					err = k8sClient.Create(ctx, &dt)
					Expect(err).To(BeNil())

					By("creating a DeploymentTargetClaim that is referenced by the DT")
					dtc := &appstudiosharedv1.DeploymentTargetClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-dtc",
							Namespace: "test-deployment",
						},
					}
					Expect(k8sClient.Create(ctx, dtc)).To(Succeed())

					By("creating a SpaceRequest that references the DTC")
					spaceRequest := &codereadytoolchainv1alpha1.SpaceRequest{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "my-sr",
							Namespace: dtc.Namespace,
							Labels: map[string]string{
								deploymentTargetClaimLabel: dtc.Name,
							},
						},
					}
					Expect(k8sClient.Create(context.Background(), spaceRequest)).To(Succeed())

					Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)).To(Succeed())

					request := newRequest(dt.Namespace, dt.Name)
					res, err := reconciler.Reconcile(ctx, request)
					Expect(err).To(BeNil())
					Expect(res).To(Equal(ctrl.Result{}))

					err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&dt), &dt)
					Expect(apierr.IsNotFound(err)).To(BeTrue(), "the finalizer should have been removed, since the policy is Retain")

				})
			})

		})

		Context("Test findDeploymentTargetsForSpaceRequests", func() {

			When("there is no matching DT for the space request", func() {
				It("should return an empty set", func() {

					spaceRequest := &codereadytoolchainv1alpha1.SpaceRequest{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "my-sr",
							Namespace: "test-namespce",
						},
					}
					Expect(k8sClient.Create(context.Background(), spaceRequest)).To(Succeed())

					req := reconciler.findDeploymentTargetsForSpaceRequests(context.Background(), spaceRequest)
					Expect(req).To(BeEmpty())
				})
			})

			When("an invalid object is passed", func() {
				It("should return an empty set", func() {

					notASpaceRequest := &appstudiosharedv1.DeploymentTarget{}

					req := reconciler.findDeploymentTargetsForSpaceRequests(context.Background(), notASpaceRequest)
					Expect(req).To(BeEmpty())
				})
			})

			When("there is a matching DT for the space request", func() {

				It("should return the matching DT", func() {

					dtc := &appstudiosharedv1.DeploymentTargetClaim{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "my-dtc",
							Namespace: "test-namespace",
						},
					}
					Expect(k8sClient.Create(context.Background(), dtc)).To(Succeed())

					spaceRequest := &codereadytoolchainv1alpha1.SpaceRequest{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "my-sr",
							Namespace: dtc.Namespace,
							Labels: map[string]string{
								deploymentTargetClaimLabel: dtc.Name,
							},
						},
					}
					Expect(k8sClient.Create(context.Background(), spaceRequest)).To(Succeed())

					dt := &appstudiosharedv1.DeploymentTarget{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "my-dt",
							Namespace: spaceRequest.Namespace,
						},
						Spec: appstudiosharedv1.DeploymentTargetSpec{
							ClaimRef: dtc.Name,
						},
					}
					Expect(k8sClient.Create(context.Background(), dt)).To(Succeed())

					req := reconciler.findDeploymentTargetsForSpaceRequests(context.Background(), spaceRequest)
					Expect(req).To(HaveLen(1))
					Expect(req[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: dt.Namespace, Name: dt.Name}))

				})

			})
		})

	})
})

func generateReclaimSpaceRequest(ops ...func(spacerequest *codereadytoolchainv1alpha1.SpaceRequest)) codereadytoolchainv1alpha1.SpaceRequest {
	spacerequest := codereadytoolchainv1alpha1.SpaceRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-spacerequest",
			Namespace: "test-deployment",
			Labels: map[string]string{
				"appstudio.openshift.io/dtc": "test-dtc",
			},
		},
		Spec: codereadytoolchainv1alpha1.SpaceRequestSpec{
			TierName:           "appstudio-env",
			TargetClusterRoles: []string{"test-role"},
		},
		Status: codereadytoolchainv1alpha1.SpaceRequestStatus{
			TargetClusterURL: "https://api-url/test-api",
			NamespaceAccess: []codereadytoolchainv1alpha1.NamespaceAccess{
				{
					Name:      "test-ns",
					SecretRef: "test-secret",
				},
			},
			Conditions: []codereadytoolchainv1alpha1.Condition{
				{
					Type:   "Ready",
					Status: corev1.ConditionTrue,
					Reason: "UnableToTerminate",
				},
			},
		},
	}
	for _, o := range ops {
		o(&spacerequest)
	}

	return spacerequest
}

func generateReclaimDeploymentTarget(ops ...func(dt *appstudiosharedv1.DeploymentTarget)) appstudiosharedv1.DeploymentTarget {
	dt := appstudiosharedv1.DeploymentTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-dt",
			Namespace:   "test-deployment",
			Annotations: map[string]string{},
		},
		Spec: appstudiosharedv1.DeploymentTargetSpec{
			ClaimRef:                  "test-dtc",
			DeploymentTargetClassName: appstudiosharedv1.DeploymentTargetClassName("test-sandbox-class"),
			KubernetesClusterCredentials: appstudiosharedv1.DeploymentTargetKubernetesClusterCredentials{
				DefaultNamespace:         "test-ns",
				APIURL:                   "https://api-url/api",
				ClusterCredentialsSecret: "test-secret",
			},
		},
		Status: appstudiosharedv1.DeploymentTargetStatus{
			Phase: appstudiosharedv1.DeploymentTargetPhase_Bound,
		},
	}

	for _, o := range ops {
		o(&dt)
	}

	return dt
}
