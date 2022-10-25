package core

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"

	appstudiocontroller "github.com/redhat-appstudio/managed-gitops/appstudio-controller/controllers/appstudio.redhat.com"
	appstudiosharedv1 "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	bindingFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/binding"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"

	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("SnapshotEnvironmentBinding Reconciler E2E tests", func() {

	Context("Testing SnapshotEnvironmentBinding Reconciler.", func() {

		var environment appstudiosharedv1.Environment
		BeforeEach(func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating the 'staging' Environment")
			environment = appstudiosharedv1.Environment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staging",
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Spec: appstudiosharedv1.EnvironmentSpec{
					Type:               appstudiosharedv1.EnvironmentType_POC,
					DisplayName:        "my-environment",
					DeploymentStrategy: appstudiosharedv1.DeploymentStrategy_AppStudioAutomated,
					ParentEnvironment:  "",
					Tags:               []string{},
					Configuration: appstudiosharedv1.EnvironmentConfiguration{
						Env: []appstudiosharedv1.EnvVarPair{},
					},
				},
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&environment, k8sClient)
			Expect(err).To(Succeed())

		})

		// This test is to verify the scenario when a user creates an SnapshotEnvironmentBinding CR in Cluster.
		// Then GitOps-Service should create GitOpsDeployment CR based on data given in Binding and update details of GitOpsDeployment in Status field of Binding.
		It("Should update Status of Binding and create new GitOpsDeployment CR.", func() {

			By("Create Binding CR in Cluster and it requires to update the Status field of Binding, because it is not updated while creating object.")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a", "component-b"})

			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update Status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				"https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging", "resources/test-data/sample-gitops-repository/components/componentB/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")

			gitOpsDeploymentNameFirst := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)
			gitOpsDeploymentNameSecond := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[1].Name)

			// gitOpsDeploymentNameFirst := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[0].Name
			// gitOpsDeploymentNameSecond := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[1].Name

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{
				{ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentNameFirst},
				{ComponentName: binding.Spec.Components[1].Name, GitOpsDeployment: gitOpsDeploymentNameSecond},
			}

			Eventually(binding, "3m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			//====================================================
			By("Verify that GitOpsDeployment CR created by GitOps-Service is having spec source as given in Binding.")

			gitOpsDeploymentFirst := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentNameFirst, binding.Namespace)
			Eventually(gitOpsDeploymentFirst, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))

			gitOpsDeploymentSecond := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentNameSecond, binding.Namespace)
			Eventually(gitOpsDeploymentSecond, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[1].GitOpsRepository.URL,
				Path:           binding.Status.Components[1].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[1].GitOpsRepository.Branch,
			}))

			//====================================================
			By("Verify that GitOpsDeployment CR created by GitOps-Service is having ownerReference according to Binding.")
			err = k8s.Get(&binding, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentFirst, k8sClient)
			Expect(err).To(Succeed())
			Expect(gitOpsDeploymentFirst.OwnerReferences[0].Name).To(Equal(binding.Name))
			Expect(gitOpsDeploymentFirst.OwnerReferences[0].UID).To(Equal(binding.UID))

			err = k8s.Get(&gitOpsDeploymentSecond, k8sClient)
			Expect(err).To(Succeed())
			Expect(gitOpsDeploymentSecond.OwnerReferences[0].Name).To(Equal(binding.Name))
			Expect(gitOpsDeploymentSecond.OwnerReferences[0].UID).To(Equal(binding.UID))
		})

		//This test is to verify the scenario when a user creates an SnapshotEnvironmentBinding CR in Cluster and after GitOpsDeployment CR is created by GitOps-Service,
		// user does modification in Binding CR. In this case GitOps-Service should also update GitOpsDeployment CR accordingly.
		It("Should update GitOpsDeployment CR if Binding CR is updated.", func() {

			By("Create Binding CR in Cluster and it requires to update the Status field of Binding, because it is not updated while creating object.")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update Status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")

			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)
			// gitOpsDeploymentName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[0].Name

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			//====================================================
			By("Verify that GitOpsDeployment CR created, is having Spec.Source as given in Binding.")

			gitOpsDeployment := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)

			Eventually(gitOpsDeployment, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))

			//====================================================
			By("Verify that GitOpsDeployment CR is updated by GitOps-Service as Binding is updated.")

			err = k8s.Get(&binding, k8sClient)
			Expect(err).To(Succeed())
			binding.Status.Components[0].GitOpsRepository.Path = "resources/test-data/sample-gitops-repository/components/componentA/overlays/dev"
			err = k8s.Update(&binding, k8sClient)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			//====================================================
			By("Verify that GitOpsDeployment CR updated by GitOps-Service is having Spec.Source as given in Binding.")

			err = k8s.Get(&gitOpsDeployment, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeployment, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))
		})

		// This test is to verify the scenario when a user creates an SnapshotEnvironmentBinding CR in Cluster and then GitOps-Service creates GitOpsDeployment CR,
		// but the user does modification directly in the GitOpsDeployment CR. In this case GitOps-Service service should revert changes done by the user in GitOpsDeployment CR.
		It("Should revert GitOpsDeployment, if modification are done directly for it, without updating Binding CR.", func() {

			By("Create Binding CR in Cluster and it requires to update the Status field of Binding, because it is not updated while creating object.")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update the Status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")

			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)
			// gitOpsDeploymentName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[0].Name

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			//====================================================
			By("Verify that GitOpsDeployment CR created, is having Spec.Source as given in Binding.")

			gitOpsDeploymentBefore := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)

			Eventually(gitOpsDeploymentBefore, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))

			//====================================================
			By("Update GitOpsDeployment CR, but dont change anything is in Binding CR.")

			err = bindingFixture.UpdateStatusWithFunction(&binding, func(bindingStatus *appstudiosharedv1.SnapshotEnvironmentBindingStatus) {
				bindingStatus.Components[0].GitOpsRepository.Path = "resources/test-data/sample-gitops-repository/components/componentA/overlays/dev"
			})
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that GitOpsDeployment CR is reverted by GitOps-Service is having same Spec.Source as given in Binding.")

			gitOpsDeploymentAfter := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)

			Eventually(gitOpsDeploymentAfter, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))
		})

		// This test is to verify the scenario when a user creates an SnapshotEnvironmentBinding CR in Cluster and then GitOps-Service creates GitOpsDeployment CR.
		// Now user deletes GitOpsDeployment from Cluster but Binding is still present in Cluster. In this case GitOps-Service should recreate GitOpsDeployment CR as given in Binding.
		It("Should recreate GitOpsDeployment, if Binding still exists but GitOpsDeployment is deleted.", func() {

			By("Create Binding CR in Cluster and it requires to update the Status field of Binding, because it is not updated while creating object.")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update the Status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops",
				"main", "fdhyqtw", []string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")

			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)
			// gitOpsDeploymentName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[0].Name

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			//====================================================
			By("Verify that GitOpsDeployment CR created, is having Spec.Source as given in Binding.")

			gitOpsDeploymentBefore := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)

			Eventually(gitOpsDeploymentBefore, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))

			//====================================================
			By("Delete GitOpsDeployment CR created by GitOps-Service, but not the Binding.")

			err = k8s.Delete(&gitOpsDeploymentBefore, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentBefore, k8sClient)
			Expect(err).NotTo(Succeed())
			Expect(apierr.IsNotFound(err)).To(BeTrue())

			//====================================================
			By("Verify that GitOpsDeployment CR is recreated by GitOps-Service.")

			// Update any value in Binding just to trigger Reconciler.
			err = k8s.Get(&binding, k8sClient)
			Expect(err).To(Succeed())
			binding.Spec.Components[0].Configuration.Replicas = 2
			err = k8s.Update(&binding, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentAfter := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)
			err = k8s.Get(&gitOpsDeploymentAfter, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentAfter, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))
		})

		// This test is to verify the scenario when a user creates an SnapshotEnvironmentBinding CR in Cluster but GitOpsDeployment CR default name is too long.
		// By default the naming convention used for GitOpsDeployment is <Binding Name>-<Application Name>-<Environment Name>-<Components Name> and If name exceeds the max limit then GitOps-Service should follow short name <Binding Name>-<Components Name>.
		// In this test GitOps-Service should use short naming convention instead of default one.
		It("Should use short name for GitOpsDeployment, if Name field length is more than max length.", func() {

			By("Create Binding CR in Cluster and it requires to update the Status field of Binding, because it is not updated while creating object.")

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			binding.Spec.Application = strings.Repeat("abcde", 45)
			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update the status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			//====================================================
			By("Verify that short name is used for GitOpsDeployment CR instead.")

			gitOpsDeploymentName := binding.Name + "-" + binding.Spec.Application + "-" + binding.Spec.Environment + "-" + binding.Spec.Components[0].Name

			// Check no GitOpsDeployment CR found with default name (longer name).
			gitOpsDeployment := buildGitOpsDeploymentObjectMeta(gitOpsDeploymentName, binding.Namespace)
			err = k8s.Get(&gitOpsDeployment, k8sClient)
			Expect(apierr.IsNotFound(err)).To(BeTrue())

			// Check GitOpsDeployment is created with short name).
			gitOpsDeployment.Name = binding.Name + "-" + binding.Spec.Components[0].Name
			err = k8s.Get(&gitOpsDeployment, k8sClient)
			Expect(err).To(Succeed())

			// Check GitOpsDeployment is having repository data as given in Binding.
			Eventually(gitOpsDeployment, "2m", "1s").Should(gitopsDeplFixture.HaveSpecSource(managedgitopsv1alpha1.ApplicationSource{
				RepoURL:        binding.Status.Components[0].GitOpsRepository.URL,
				Path:           binding.Status.Components[0].GitOpsRepository.Path,
				TargetRevision: binding.Status.Components[0].GitOpsRepository.Branch,
			}))
		})

		It("should create a GitOpsDeployment that references cluster credentials specified in Environment", func() {

			// ToDo: solve GITOPSRVC-217, and remove this constraint
			if fixture.IsRunningAgainstKCP() {
				Skip("Skipping this test because of race condition when running on KCP based env")
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("creating second managed environment Secret")
			secret := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-secret",
					Namespace: environment.Namespace,
				},
				Type: sharedutil.ManagedEnvironmentSecretType,
				Data: map[string][]byte{
					"kubeconfig": ([]byte)("{}"),
				},
			}
			err = k8s.Create(&secret, k8sClient)
			Expect(err).To(BeNil())

			err = k8s.Get(&environment, k8sClient)
			Expect(err).To(BeNil())

			environment.Spec.UnstableConfigurationFields = &appstudiosharedv1.UnstableEnvironmentConfiguration{
				KubernetesClusterCredentials: appstudiosharedv1.KubernetesClusterCredentials{
					TargetNamespace:          fixture.GitOpsServiceE2ENamespace,
					APIURL:                   "https://api-url",
					ClusterCredentialsSecret: "my-secret",
				},
			}

			err = k8s.Update(&environment, k8sClient)
			Expect(err).To(BeNil())

			By("generating the Binding, and waiting for the corresponding GitOpsDeployment to exist")

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(BeNil())

			// Update the status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			By("waiting for the the controller to Reconcile the GitOpsDeplyoment")
			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)

			gitopsDeployment := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentName,
					Namespace: binding.Namespace,
				},
			}

			Eventually(&gitopsDeployment, "60s", "1s").Should(k8s.ExistByName(k8sClient))

			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())

			Expect(gitopsDeployment.Spec.Destination.Environment).To(Equal("managed-environment-"+environment.Name),
				"the destination should be the environment")
			Expect(gitopsDeployment.Spec.Destination.Namespace).
				To(Equal(environment.Spec.UnstableConfigurationFields.KubernetesClusterCredentials.TargetNamespace),
					"the namespace of the GitOpsDeployment should come from the Environment")

		})

		It("Should append ASEB labels with key `appstudio.openshift.io` to GitopsDeployment label", func() {
			By("Create SnapshotEnvironmentBindingResource")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			binding.ObjectMeta.Labels = map[string]string{"appstudio.openshift.io": "testing"}
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			By("Update Status field of SnapshotEnvironmentBindingResource")
			// Update the status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")
			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			By("Verify whether `gitopsDeployment.ObjectMeta.Labels` is updated with ASEB labels")
			gitopsDeployment := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentName,
					Namespace: binding.Namespace,
				},
			}

			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).To(Equal("testing"))
		})

		It("Should not append ASEB label without appstudio.openshift.io label into the GitopsDeployment Label", func() {
			By("Create SnapshotEnvironmentBindingResource")

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			By("Update Status field of SnapshotEnvironmentBindingResource")
			// Update the status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")
			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			By("Verify whether `gitopsDeployment.ObjectMeta.Labels` is not updated with ASEB labels")
			gitopsDeployment := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentName,
					Namespace: binding.Namespace,
				},
			}

			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).ToNot(Equal("testing"))
		})

		It("Should update gitopsDeployment label if ASEB label gets updated", func() {
			By("Create SnapshotEnvironmentBindingResource")
			binding := buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			binding.ObjectMeta.Labels = map[string]string{"appstudio.openshift.io": "testing"}
			err = k8s.Create(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update the status field
			err = buildAndUpdateBindingStatus(binding.Spec.Components,
				https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging"}, &binding)
			Expect(err).To(Succeed())

			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")
			gitOpsDeploymentName := appstudiocontroller.GenerateBindingGitOpsDeploymentName(binding, binding.Spec.Components[0].Name)

			expectedGitOpsDeployments := []appstudiosharedv1.BindingStatusGitOpsDeployment{{
				ComponentName: binding.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentName,
			}}

			Eventually(binding, "2m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeployments))

			By("Verify whether `gitopsDeployment.ObjectMeta.Labels` is not updated with ASEB labels")
			gitopsDeployment := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentName,
					Namespace: binding.Namespace,
				},
			}

			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).To(Equal("testing"))

			err = k8s.Get(&binding, k8sClient)
			Expect(err).To(Succeed())

			// Update binding label
			binding.ObjectMeta.Labels["appstudio.openshift.io"] = "testing-update"
			err = k8s.Update(&binding, k8sClient)
			Expect(err).To(Succeed())

			By("Verify whether `gitopsDeployment.ObjectMeta.Labels` is updated with ASEB labels")
			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels).ToNot(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).ToNot(Equal("testing"))
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).To(Equal("testing-update"))

			By("Remove ASEB label `appstudio.openshift.io` label and verify whether it is removed from gitopsDeployment label")
			delete(binding.ObjectMeta.Labels, "appstudio.openshift.io")
			err = k8s.Update(&binding, k8sClient)
			Expect(err).To(Succeed())

			By("Verify whether gitopsDeployment.ObjectMeta.Label `appstudio.openshift.io` is removed from gitopsDeployment")
			err = k8s.Get(&gitopsDeployment, k8sClient)
			Expect(err).To(BeNil())
			Expect(gitopsDeployment.ObjectMeta.Labels["appstudio.openshift.io"]).ToNot(Equal("testing-update"))
		})

	})

})

func buildAndUpdateBindingStatus(components []appstudiosharedv1.BindingComponent, url,
	branch, commitID string, path []string, binding *appstudiosharedv1.SnapshotEnvironmentBinding) error {

	By(fmt.Sprintf("updating Status field of SnapshotEnvironmentBindingResource for '%s' of '%s' in '%v'", url, branch, path))

	return bindingFixture.UpdateStatusWithFunction(binding, func(bindingStatus *appstudiosharedv1.SnapshotEnvironmentBindingStatus) {

		// Update the binding status
		*bindingStatus = buildSnapshotEnvironmentBindingStatus(components,
			url, branch, commitID, path)

	})
}

func buildSnapshotEnvironmentBindingResource(name, appName, envName, snapShotName string, replica int, componentNames []string) appstudiosharedv1.SnapshotEnvironmentBinding {
	// Create SnapshotEnvironmentBinding CR.
	binding := appstudiosharedv1.SnapshotEnvironmentBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: appstudiosharedv1.SnapshotEnvironmentBindingSpec{
			Application: appName,
			Environment: envName,
			Snapshot:    snapShotName,
		},
	}

	components := []appstudiosharedv1.BindingComponent{}
	for _, name := range componentNames {
		components = append(components, appstudiosharedv1.BindingComponent{
			Name:          name,
			Configuration: appstudiosharedv1.BindingComponentConfiguration{Replicas: replica},
		})
	}

	binding.Spec.Components = components
	return binding
}

func buildSnapshotEnvironmentBindingStatus(components []appstudiosharedv1.BindingComponent, url,
	branch, commitID string, path []string) appstudiosharedv1.SnapshotEnvironmentBindingStatus {

	// Create SnapshotEnvironmentBindingStatus object.
	status := appstudiosharedv1.SnapshotEnvironmentBindingStatus{}

	componentStatus := []appstudiosharedv1.ComponentStatus{}

	for i, component := range components {
		componentStatus = append(componentStatus, appstudiosharedv1.ComponentStatus{
			Name: component.Name,
			GitOpsRepository: appstudiosharedv1.BindingComponentGitOpsRepository{
				URL: url, Branch: branch, Path: path[i], GeneratedResources: []string{}, CommitID: commitID,
			},
		})
	}

	status.Components = componentStatus
	return status
}

func buildGitOpsDeploymentObjectMeta(name, namespace string) managedgitopsv1alpha1.GitOpsDeployment {
	// Create GitOpsDeployment object only with ObjectMeta.
	gitOpsDeployment := managedgitopsv1alpha1.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	return gitOpsDeployment
}
