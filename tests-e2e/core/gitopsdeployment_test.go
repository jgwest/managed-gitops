package core

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	fixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	typed "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	name    = "my-gitops-depl"
	repoURL = "https://github.com/redhat-appstudio/managed-gitops"

	// ArgoCDReconcileWaitTime is the length of time to watch for Argo CD/GitOps Service to deploy the resources
	// of an Application (e.g. to reconcile the Application resource)
	// We set this to be at least twice the default reconciliation time (3 minutes) to avoid a race condition in updating ConfigMaps
	// in Argo CD. This is an imperfect solution.
	ArgoCDReconcileWaitTime = "7m"
)

var _ = Describe("GitOpsDeployment E2E tests", func() {

	Context("Create, Update and Delete a GitOpsDeployment ", func() {

		// this assumes that service is running on non aware kcp client
		config, err := fixture.GetSystemKubeConfig()
		Expect(err).To(BeNil())

		k8sClient, err := fixture.GetKubeClient(config)
		Expect(err).To(BeNil())
		ctx := context.Background()

		AfterEach(func() {
			// At end of a test, output the state of Argo CD Application, for post-mortem debuggign
			err := fixture.ReportRemainingArgoCDApplications(k8sClient)
			Expect(err).To(BeNil())
		})

		// Generate expected resources for "https://github.com/redhat-appstudio/managed-gitops"
		getResourceStatusList_GitOpsRepositoryTemplateRepo := func(name string) []managedgitopsv1alpha1.ResourceStatus {
			return []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "apps",
					Version:   "v1",
					Kind:      "Deployment",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      name,
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
					Health: &managedgitopsv1alpha1.HealthStatus{
						Status: managedgitopsv1alpha1.HeathStatusCodeHealthy,
					},
				},
				{
					Group:     "route.openshift.io",
					Version:   "v1",
					Kind:      "Route",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      name,
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
					Health: &managedgitopsv1alpha1.HealthStatus{
						Status:  managedgitopsv1alpha1.HeathStatusCodeHealthy,
						Message: "Route is healthy",
					},
				},
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Service",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      name,
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
					Health: &managedgitopsv1alpha1.HealthStatus{
						Status: managedgitopsv1alpha1.HeathStatusCodeHealthy,
					},
				},
			}
		}

		// Generate  expected resource from "https://github.com/managed-gitops-test-data/deployment-permutations-*" using name
		getResourceStatusList_deploymentPermutations := func(name string) []managedgitopsv1alpha1.ResourceStatus {
			return []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "ConfigMap",
					Name:      name,
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				},
			}
		}

		// Generate expected identifier configmap for "https://github.com/managed-gitops-test-data/deployment-permutations-*"
		createResourceStatusListFunction_deploymentPermutations := func() []managedgitopsv1alpha1.ResourceStatus {
			return []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "ConfigMap",
					Name:      "identifier",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				},
			}
		}

		// Returns true if the all param resources no longer exist (have been deleted), and false otherwise.
		expectAllResourcesToBeDeleted := func(expectedResourceStatusList []managedgitopsv1alpha1.ResourceStatus) {

			Eventually(func() bool {

				k8sclient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
				Expect(err).To(Succeed())

				for _, resourceValue := range expectedResourceStatusList {
					ns := typed.NamespacedName{
						Namespace: resourceValue.Namespace,
						Name:      resourceValue.Name,
					}

					var obj client.Object

					if resourceValue.Kind == "ConfigMap" {
						obj = &corev1.ConfigMap{}
					} else if resourceValue.Kind == "Deployment" {
						obj = &appsv1.Deployment{}
					} else if resourceValue.Kind == "Route" {
						obj = &routev1.Route{}
					} else if resourceValue.Kind == "Service" {
						obj = &corev1.Service{}
					} else {
						GinkgoWriter.Println("unrecognize kind:", resourceValue.Kind)
						return false
					}
					err := k8sclient.Get(context.Background(), ns, obj)
					if !apierr.IsNotFound(err) {
						return false
					}
				}

				// If all the resources were found, return true
				return true
			}, "2m", "1s").Should(BeTrue())

		}

		It("Should ensure succesful creation of GitOpsDeployment, by creating the GitOpsDeployment", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())
			gitOpsDeploymentResource := buildGitOpsDeploymentResource(name,
				repoURL, "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)
			gitOpsDeploymentResource.Spec.Destination.Environment = ""
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)
			expectedResourceStatusList := []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "ConfigMap",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      "environment-config-map",
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				},
			}
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-a")...)
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-b")...)

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
				),
			)

			By("deleting the GitOpsDeployment resource and waiting for the resources to be deleted")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)

		})

		It("Should ensure synchronicity of create and update of GitOpsDeployment, by ensurng no updates are done in existing deployment, if CR is submitted again without any changes", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			gitOpsDeploymentResource := buildGitOpsDeploymentResource(name,
				repoURL, "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)
			gitOpsDeploymentResource.Spec.Destination.Environment = ""
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource.Spec.Source.RepoURL = repoURL
			gitOpsDeploymentResource.Spec.Source.Path = "resources/test-data/sample-gitops-repository/environments/overlays/dev"

			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).To(BeNil())
			Expect(gitOpsDeploymentResource.Spec.Source.RepoURL).To(Equal(repoURL))
			Expect(gitOpsDeploymentResource.Spec.Source.Path).To(Equal("resources/test-data/sample-gitops-repository/environments/overlays/dev"))

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
				),
			)
			Consistently(gitOpsDeploymentResource, "20s", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
				),
			)

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})

		It("Should ensure synchronicity of create and update of GitOpsDeployment, by ensuring GitOpsDeployment should update successfully on changing value(s) within Spec", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())
			gitOpsDeploymentResource := buildGitOpsDeploymentResource(name,
				repoURL, "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource.Spec.Source.Path = "resources/test-data/sample-gitops-repository/environments/overlays/staging"

			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).To(BeNil())
			Expect(gitOpsDeploymentResource.Spec.Source.Path).To(Equal("resources/test-data/sample-gitops-repository/environments/overlays/staging"))

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})

		It("Checks whether a change in repo URL is reflected within the cluster", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("creating a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
				"https://github.com/managed-gitops-test-data/deployment-permutations-a", "pathB", "branchA",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			createResourceStatusList_deploymentPermutations := createResourceStatusListFunction_deploymentPermutations()

			createResourceStatusList_deploymentPermutations = append(createResourceStatusList_deploymentPermutations,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathb")...)
			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(createResourceStatusList_deploymentPermutations),
				),
			)

			By("ensuring changes in repo URL deploys the resources that are a part of the latest modification")

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource.Spec.Source.RepoURL = "https://github.com/managed-gitops-test-data/deployment-permutations-b"

			//updating the CR with changes in repoURL
			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			expectedResourceStatusList = append(expectedResourceStatusList,
				getResourceStatusList_deploymentPermutations("deployment-permutations-b-brancha-pathb")...)

			// There is an Argo CD race condition where the ConfigMaps are updated before Argo CD can detect the event, which causes
			// the Argo CD sync to hang. Thus we need to use this large '10m' wait time here - jgwest, Jul 2022.
			Eventually(gitOpsDeploymentResource, "10m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
				),
			)
			By("delete the GitOpsDeployment resource")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)

		})

		It("Checks whether a change in source path is reflected within the cluster", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("creating a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
				"https://github.com/managed-gitops-test-data/deployment-permutations-a", "pathB", "branchA",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)
			gitOpsDeploymentResource.Spec.Destination.Environment = ""
			gitOpsDeploymentResource.Spec.Destination.Namespace = fixture.GitOpsServiceE2ENamespace

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			createResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			createResourceStatusList = append(createResourceStatusList,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathb")...)
			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Branch:  gitOpsDeploymentResource.Spec.Source.TargetRevision,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(createResourceStatusList),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
				),
			)

			By("ensuring changes in repo URL deploys the resources that are a part of the latest modification")

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource.Spec.Source.Path = "pathC"

			//updating the CR with changes in repoURL
			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			expectedResourceStatusList = append(expectedResourceStatusList,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathc")...)

			expectedReconciledStateField = managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Branch:  gitOpsDeploymentResource.Spec.Source.TargetRevision,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: gitOpsDeploymentResource.Spec.Destination.Namespace,
				},
			}

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
					gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
				),
			)
			By("delete the GitOpsDeployment resource")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)

		})

		It("Checks whether a change in target revision is reflected within the cluster", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
				"https://github.com/managed-gitops-test-data/deployment-permutations-a", "pathB", "branchA",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			createResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			createResourceStatusList = append(createResourceStatusList, getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathb")...)
			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(createResourceStatusList),
				),
			)

			By("ensuring changes in repo URL deploys the resources that are a part of the latest modification")

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource.Spec.Source.TargetRevision = "branchB"

			//updating the CR with changes in repoURL
			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			expectedResourceStatusList = append(expectedResourceStatusList,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-branchb-pathb")...)

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
				),
			)
			By("deleting the GitOpsDeployment resource")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)

		})

		It("Checks whether resources are created in target revision and reflected within the cluster", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildTargetRevisionGitOpsDeploymentResource("gitops-depl-test-status",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev", "xyz",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("ensuring changes in target revision deploys the resources that are a part of the latest modification")

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
			gitOpsDeploymentResource.Spec.Source.TargetRevision = "HEAD"

			//updating the CR with changes in target revision
			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedResourceStatusList_GitOpsRepositoryTemplateRepo := []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "ConfigMap",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      "environment-config-map",
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				},
			}
			expectedResourceStatusList_GitOpsRepositoryTemplateRepo = append(expectedResourceStatusList_GitOpsRepositoryTemplateRepo, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-a")...)
			expectedResourceStatusList_GitOpsRepositoryTemplateRepo = append(expectedResourceStatusList_GitOpsRepositoryTemplateRepo, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-b")...)

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList_GitOpsRepositoryTemplateRepo),
				),
			)
			By("delete the GitOpsDeployment resource")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList_GitOpsRepositoryTemplateRepo)

		})

		It("Checks whether deleting the GitOpsDeployment deletes all the resources deployed as part of it", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildGitOpsDeploymentResource("gitops-depl-test-status",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("populating the resource list")

			expectedResourceStatusList := []managedgitopsv1alpha1.ResourceStatus{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "ConfigMap",
					Namespace: fixture.GitOpsServiceE2ENamespace,
					Name:      "environment-config-map",
					Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				},
			}
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-a")...)
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList_GitOpsRepositoryTemplateRepo("component-b")...)

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
				),
			)
			By("delete the GitOpsDeployment resource")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(Succeed())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)
		})

		It("Checks for failure of deployment when an invalid input is provided", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("ensuring GitOpsDeployment Fails to create and deploy application when an invalid field input is passed")
			gitOpsDeploymentResource := buildGitOpsDeploymentResource("test-should-fail",
				"invalid-url", "path/path/path",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").ShouldNot(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)
			Consistently(gitOpsDeploymentResource, "20s", "1s").ShouldNot(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)

		})

	})
})
