package appstudioredhatcom

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appstudiosharedv1 "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
	apibackend "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/tests"
)

var _ = Describe("SnapshotEnvironmentBinding Reconciler Tests", func() {

	Context("Testing SnapshotEnvironmentBindingReconciler.", func() {
		var ctx context.Context
		var request reconcile.Request
		var binding *appstudiosharedv1.SnapshotEnvironmentBinding
		var bindingReconciler SnapshotEnvironmentBindingReconciler

		var environment appstudiosharedv1.Environment

		BeforeEach(func() {
			ctx = context.Background()

			scheme,
				argocdNamespace,
				kubesystemNamespace,
				apiNamespace,
				err := tests.GenericTestSetup()
			Expect(err).To(BeNil())

			// Create fake client
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(apiNamespace, argocdNamespace, kubesystemNamespace).
				Build()

			err = appstudiosharedv1.AddToScheme(scheme)
			Expect(err).To(BeNil())

			// Create placeholder environment
			environment = appstudiosharedv1.Environment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staging",
					Namespace: apiNamespace.Name,
				},
				Spec: appstudiosharedv1.EnvironmentSpec{
					DisplayName:        "my-environment",
					Type:               appstudiosharedv1.EnvironmentType_POC,
					DeploymentStrategy: appstudiosharedv1.DeploymentStrategy_AppStudioAutomated,
					ParentEnvironment:  "",
					Tags:               []string{},
					Configuration:      appstudiosharedv1.EnvironmentConfiguration{},
				},
			}
			err = k8sClient.Create(ctx, &environment)
			Expect(err).To(BeNil())

			bindingReconciler = SnapshotEnvironmentBindingReconciler{Client: k8sClient, Scheme: scheme}

			// Create SnapshotEnvironmentBinding CR.
			binding = &appstudiosharedv1.SnapshotEnvironmentBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "appa-staging-binding",
					Namespace: apiNamespace.Name,
					Labels: map[string]string{
						"appstudio.application": "new-demo-app",
						"appstudio.environment": "staging",
					},
				},
				Spec: appstudiosharedv1.SnapshotEnvironmentBindingSpec{
					Application: "new-demo-app",
					Environment: "staging",
					Snapshot:    "my-snapshot",
					Components: []appstudiosharedv1.BindingComponent{
						{
							Name: "component-a",
							Configuration: appstudiosharedv1.BindingComponentConfiguration{
								Env: []appstudiosharedv1.EnvVarPair{
									{Name: "My_STG_ENV", Value: "1000"},
								},
								Replicas: 3,
							},
						},
					},
				},
				Status: appstudiosharedv1.SnapshotEnvironmentBindingStatus{
					Components: []appstudiosharedv1.ComponentStatus{
						{
							Name: "component-a",
							GitOpsRepository: appstudiosharedv1.BindingComponentGitOpsRepository{
								URL:    "https://github.com/redhat-appstudio/managed-gitops",
								Branch: "main",
								Path:   "resources/test-data/sample-gitops-repository/components/componentA/overlays/staging",
							},
						},
					},
				},
			}

			// Create request object for Reconciler
			request = newRequest(apiNamespace.Name, binding.Name)
		})

		It("Should set the status field of Binding.", func() {
			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Check status field before calling Reconciler
			bindingFirst := &appstudiosharedv1.SnapshotEnvironmentBinding{}
			err = bindingReconciler.Client.Get(ctx, request.NamespacedName, bindingFirst)
			Expect(err).To(BeNil())
			Expect(len(bindingFirst.Status.GitOpsDeployments)).To(Equal(0))

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			// Check status field after calling Reconciler
			bindingSecond := &appstudiosharedv1.SnapshotEnvironmentBinding{}
			err = bindingReconciler.Client.Get(ctx, request.NamespacedName, bindingSecond)

			Expect(err).To(BeNil())
			Expect(len(bindingSecond.Status.GitOpsDeployments)).NotTo(Equal(0))
			Expect(bindingSecond.Status.GitOpsDeployments[0].ComponentName).To(Equal("component-a"))
			Expect(bindingSecond.Status.GitOpsDeployments[0].GitOpsDeployment).
				To(Equal(binding.Name + "-" +
					binding.Spec.Application + "-" +
					binding.Spec.Environment + "-" +
					binding.Spec.Components[0].Name))
		})

		It("Should not update GitOpsDeployment if same Binding is created again.", func() {

			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			// Fetch GitOpsDeployment object before calling Reconciler
			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			gitopsDeploymentFirst := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeploymentFirst)
			Expect(err).To(BeNil())

			// Trigger Reconciler again
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			// Fetch GitOpsDeployment object after calling Reconciler
			gitopsDeploymentSecond := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeploymentSecond)
			Expect(err).To(BeNil())

			// Reconciler should not do any change in GitOpsDeployment object.
			Expect(gitopsDeploymentFirst).To(Equal(gitopsDeploymentSecond))
		})

		It("Should revert GitOpsDeploymentObject if it's spec is different than Binding Component.", func() {
			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			// Fetch GitOpsDeployment object
			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())

			// GitOpsDeployment object spec should be same as Binding Component.
			Expect(gitopsDeployment.Spec.Source.Path).To(Equal(binding.Status.Components[0].GitOpsRepository.Path))

			// Update GitOpsDeploymentObject in cluster.
			gitopsDeployment.Spec.Source.Path = "components/componentA/overlays/dev"
			err = bindingReconciler.Update(ctx, gitopsDeployment)
			Expect(err).To(BeNil())

			// Trigger Reconciler again
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			// Fetch GitOpsDeployment object after calling Reconciler
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())

			// Reconciler should revert GitOpsDeployment object, so it will be same as old object
			Expect(gitopsDeployment.Spec.Source.Path).To(Equal(binding.Status.Components[0].GitOpsRepository.Path))
		})

		It("Should use short name for GitOpsDeployment object.", func() {
			// Update application name to exceed the limit
			binding.Spec.Application = strings.Repeat("abcde", 45)
			request = newRequest(binding.Namespace, binding.Name)

			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			// Check status field after calling Reconciler
			binding := &appstudiosharedv1.SnapshotEnvironmentBinding{}
			err = bindingReconciler.Client.Get(ctx, request.NamespacedName, binding)

			Expect(err).To(BeNil())
			Expect(len(binding.Status.GitOpsDeployments)).NotTo(Equal(0))
			Expect(binding.Status.GitOpsDeployments[0].ComponentName).To(Equal("component-a"))

			// GitOpsDeployment should have short name
			Expect(binding.Status.GitOpsDeployments[0].GitOpsDeployment).
				To(Equal(binding.Name + "-" + binding.Spec.Components[0].Name))
		})

		It("Should not return error if Status.Components is not available in Binding object.", func() {
			binding.Status.Components = []appstudiosharedv1.ComponentStatus{}
			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			checkStatusConditionOfEnvironmentBinding(ctx, bindingReconciler.Client, binding, "SnapshotEventBinding Component status is required to generate GitOps deployment, waiting for the Application Service controller to finish reconciling binding 'appa-staging-binding'")

		})

		It("Should return error if Status.GitOpsRepoConditions Status is set to False in Binding object.", func() {
			binding.Status.GitOpsRepoConditions = []metav1.Condition{
				{
					Status: metav1.ConditionFalse,
				},
			}

			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			checkStatusConditionOfEnvironmentBinding(ctx, bindingReconciler.Client, binding, "Can not Reconcile Binding 'appa-staging-binding', since GitOps Repo Conditions status is false.")
		})

		It("should not return an error if there are duplicate components in binding.Status.Components", func() {

			By("creating an SnapshotEnvironmentBinding with duplicate component names")

			binding.Status.Components = []appstudiosharedv1.ComponentStatus{
				{
					Name: "componentA",
					GitOpsRepository: appstudiosharedv1.BindingComponentGitOpsRepository{
						URL:                "https://url",
						Branch:             "branch",
						Path:               "path",
						GeneratedResources: []string{},
					},
				},
				{
					Name: "componentA",
					GitOpsRepository: appstudiosharedv1.BindingComponentGitOpsRepository{
						URL:                "https://url2",
						Branch:             "branch2",
						Path:               "path2",
						GeneratedResources: []string{},
					},
				},
			}

			// Create SnapshotEnvironmentBinding CR in cluster.
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			// Trigger Reconciler
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			checkStatusConditionOfEnvironmentBinding(ctx, bindingReconciler.Client, binding, "duplicate component keys found in status field in componentA")

		})

		It("should verify that if the Environment contains configuration information, that it is included in the generate GitOpsDeployment", func() {

			By("creating an Environment with valid configuration fields")
			environment.Spec.UnstableConfigurationFields = &appstudiosharedv1.UnstableEnvironmentConfiguration{
				KubernetesClusterCredentials: appstudiosharedv1.KubernetesClusterCredentials{
					TargetNamespace:          "my-target-namespace",
					APIURL:                   "my-api-url",
					ClusterCredentialsSecret: "secret",
				},
			}
			err := bindingReconciler.Client.Update(ctx, &environment)
			Expect(err).To(BeNil())

			By("creating default Binding")
			err = bindingReconciler.Client.Create(ctx, binding)
			Expect(err).To(BeNil())

			By("calling Reconcile")
			request = newRequest(binding.Namespace, binding.Name)
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("ensuring that the GitOpsDeployment was created using values from ConfigurationFields")

			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}
			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())

			Expect(gitopsDeployment.Spec.Destination.Namespace).To(Equal(environment.Spec.UnstableConfigurationFields.TargetNamespace))

			By("removing the field from Environment, and ensuring the GitOpsDeployment is updated")
			environment.Spec.UnstableConfigurationFields = nil
			err = bindingReconciler.Client.Update(ctx, &environment)
			Expect(err).To(BeNil())

			By("reconciling again")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.Spec.Destination.Namespace).To(Equal(""))
			Expect(gitopsDeployment.Spec.Destination.Environment).To(Equal(""))

			By("testing with a missing TargetNamespace, which should return an error")
			environment.Spec.UnstableConfigurationFields = &appstudiosharedv1.UnstableEnvironmentConfiguration{
				KubernetesClusterCredentials: appstudiosharedv1.KubernetesClusterCredentials{
					APIURL:                   "my-api-url",
					ClusterCredentialsSecret: "secret",
				},
			}
			err = bindingReconciler.Client.Update(ctx, &environment)
			Expect(err).To(BeNil())

			By("reconciling again, and expecting an TargetNamespace missing error")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).ToNot(BeNil())
			Expect(strings.Contains(err.Error(), errMissingTargetNamespace)).To(BeTrue())

		})

		It("should append ASEB label with key `appstudio.openshift.io` into the GitopsDeployment Label", func() {
			By("updating binding.ObjectMeta.Labels with appstudio.openshift.io label")
			binding.ObjectMeta.Labels[appstudioLabelKey] = "testing"

			By("creating SnapshotEnvironmentBinding CR in cluster.")
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("Fetching GitOpsDeployment object to check whether GitOpsDeployment label field has been updated")
			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(Equal(map[string]string{appstudioLabelKey: "testing"}))
		})

		It("should not append ASEB label without key appstudio.openshift.io into the GitopsDeployment Label", func() {
			By("creating SnapshotEnvironmentBinding CR in cluster.")
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("fetching GitOpsDeployment object to check whether GitOpsDeployment label field is not updated")
			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())

			Expect(gitopsDeployment.ObjectMeta.Labels).To(BeNil())
		})

		It("should update gitopsDeployment label if ASEB label gets updated", func() {
			By("updating binding.ObjectMeta.Labels with appstudio.openshift.io label")
			binding.ObjectMeta.Labels[appstudioLabelKey] = "testing"

			By("creating SnapshotEnvironmentBinding CR in cluster.")
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("fetching GitOpsDeployment object to check whether GitOpsDeployment label field has been updated")
			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(Equal(map[string]string{appstudioLabelKey: "testing"}))

			err = bindingReconciler.Get(ctx, types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}, binding)
			Expect(err).To(Succeed())

			By("updating appstudio.openshift.io label")
			binding.ObjectMeta.Labels[appstudioLabelKey] = "testing-update"

			err = bindingReconciler.Update(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("verifying GitopsDeployment is updated as binding CR is updated")
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(Equal(map[string]string{appstudioLabelKey: "testing-update"}))

			err = bindingReconciler.Get(ctx, types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}, binding)
			Expect(err).To(Succeed())

			By("removing ASEB label `appstudio.openshift.io` label and verify whether it is removed from gitopsDeployment label")
			delete(binding.ObjectMeta.Labels, appstudioLabelKey)

			err = bindingReconciler.Update(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("verifying whether gitopsDeployment.ObjectMeta.Label `appstudio.openshift.io` is removed from gitopsDeployment")
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(BeEmpty())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(Equal(map[string]string{appstudioLabelKey: "testing-update"}))

		})

		It("should update gitopsDeployment label if ASEB label gets updated, but not affect non-appstudio labels", func() {
			By("updating binding.ObjectMeta.Labels with appstudio.openshift.io label, plus a non-appstudio-label")
			binding.ObjectMeta.Labels[appstudioLabelKey] = "testing"
			binding.ObjectMeta.Labels["non-appstudio-label"] = "should-not-be-copied"

			By("creating SnapshotEnvironmentBinding")
			err := bindingReconciler.Create(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("fetching GitOpsDeployment object to check whether GitOpsDeployment label field has been updated")
			gitopsDeploymentKey := client.ObjectKey{
				Namespace: binding.Namespace,
				Name:      GenerateBindingGitOpsDeploymentName(*binding, binding.Spec.Components[0].Name),
			}

			gitopsDeployment := &apibackend.GitOpsDeployment{}
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(Equal(map[string]string{
				appstudioLabelKey: "testing",
			}), "reconciler should only copy appstudio labels to the gitops deployment")

			err = bindingReconciler.Get(ctx, types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}, binding)
			Expect(err).To(Succeed())

			By("updating appstudio.openshift.io label")
			binding.ObjectMeta.Labels[appstudioLabelKey] = "testing-update"

			err = bindingReconciler.Update(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("verifying GitopsDeployment is updated as binding CR is updated, and the non-appstudio value is unchanged")
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(Equal(map[string]string{
				appstudioLabelKey: "testing-update",
			}))

			err = bindingReconciler.Get(ctx, types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}, binding)
			Expect(err).To(Succeed())

			By("removing ASEB label `appstudio.openshift.io` label and verifying whether it is removed from gitopsDeployment label")
			delete(binding.ObjectMeta.Labels, appstudioLabelKey)

			err = bindingReconciler.Update(ctx, binding)
			Expect(err).To(BeNil())

			By("triggering Reconciler")
			_, err = bindingReconciler.Reconcile(ctx, request)
			Expect(err).To(BeNil())

			By("verifying whether gitopsDeployment.ObjectMeta.Label `appstudio.openshift.io` is removed from " +
				"gitopsDeployment, but the non-appstudio label is still present")
			err = bindingReconciler.Get(ctx, gitopsDeploymentKey, gitopsDeployment)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).To(BeEmpty())

		})

	})

	Context("verify functions that are used to ensure that GitOpsDeployments generated by the Binding controller contain "+
		"appstudio labels from the parent", func() {

		DescribeTable("test scenarios of updateMapWithExpectedAppStudioLabels()", func(actualLabels, desiredLabels, expectedResult map[string]string) {

			res := updateMapWithExpectedAppStudioLabels(actualLabels, desiredLabels)
			Expect(res).To(Equal(expectedResult))
		},
			Entry("nil check", nil, nil, nil),
			Entry("empty actual converted to nil", map[string]string{}, nil, nil),
			Entry("empty actual/desired converted to nil", map[string]string{}, map[string]string{}, nil),
			Entry("actual map without appstudio labels shouldn't be modified", map[string]string{
				"a": "b",
			}, map[string]string{}, map[string]string{"a": "b"}),
			Entry("desired appstudio labels should be added, while preserving old labels", map[string]string{
				"a": "b",
			}, map[string]string{appstudioLabelKey + "/label": "appstudio-value"},
				map[string]string{"a": "b", appstudioLabelKey + "/label": "appstudio-value"}),
			Entry("old appstudio labels should be removed, if they are not desired", map[string]string{
				"a": "b", appstudioLabelKey + "/label": "appstudio-value",
			}, map[string]string{}, map[string]string{"a": "b"}),
			Entry("existing appstudio labels should not be touched", map[string]string{
				"a": "b", appstudioLabelKey + "/label": "appstudio-value",
			}, map[string]string{appstudioLabelKey + "/label": "appstudio-value"}, map[string]string{"a": "b", appstudioLabelKey + "/label": "appstudio-value"}),
		)

		DescribeTable("test scenarios of areAppStudioLabelsEqualBetweenMaps()", func(map1, map2 map[string]string, expectedResult bool) {

			res := areAppStudioLabelsEqualBetweenMaps(map1, map2)
			Expect(res).To(Equal(expectedResult))
		},
			Entry("nil check both", nil, nil, true),
			Entry("nil check first", nil, map[string]string{}, true),
			Entry("nil check second", map[string]string{}, nil, true),
			Entry("empty maps", map[string]string{}, map[string]string{}, true),
			Entry("appstudio label in first, but not second",
				map[string]string{appstudioLabelKey + "/label": "value"}, map[string]string{}, false),
			Entry("appstudio label in second, but not first",
				map[string]string{}, map[string]string{appstudioLabelKey + "/label": "value"}, false),
			Entry("same appstudio label in both",
				map[string]string{appstudioLabelKey + "/label": "value"}, map[string]string{appstudioLabelKey + "/label": "value"}, true),
			Entry("same appstudio label in both, but mismatching non-appstudio",
				map[string]string{appstudioLabelKey + "/label": "value", "a": "b"},
				map[string]string{appstudioLabelKey + "/label": "value", "c": "d", "e": "f"}, true),

			Entry("different appstudio label in both",
				map[string]string{appstudioLabelKey + "/label1": "value"}, map[string]string{appstudioLabelKey + "/label2": "value"}, false),
		)

	})
})

// newRequest contains the information necessary to reconcile a Kubernetes object.
func newRequest(namespace, name string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func checkStatusConditionOfEnvironmentBinding(ctx context.Context, rClient client.Client, binding *appstudiosharedv1.SnapshotEnvironmentBinding, message string) {
	err := rClient.Get(ctx, client.ObjectKeyFromObject(binding), binding)
	Expect(err).To(BeNil())
	Expect(len(binding.Status.BindingConditions) > 0)

	for _, condition := range binding.Status.BindingConditions {
		if condition.Type == SnapshotEnvironmentBindingConditionErrorOccurred {
			Expect(condition.Type).To(Equal(SnapshotEnvironmentBindingConditionErrorOccurred))
			Expect(condition.Message).To(Equal(message))
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(SnapshotEnvironmentBindingReasonErrorOccurred))
		}
	}
}
