package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	routev1 "github.com/openshift/api/route/v1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/db/util"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
	sharedresourceloop "github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	fixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	appProjectFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/appproject"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typed "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (

	// ArgoCDReconcileWaitTime is the length of time to watch for Argo CD/GitOps Service to deploy the resources
	// of an Application (e.g. to reconcile the Application resource)
	// We set this to be at least twice the default reconciliation time (3 minutes) to avoid a race condition in updating ConfigMaps
	// in Argo CD. This is an imperfect solution.
	ArgoCDReconcileWaitTime = "7m"
)

var _ = Describe("GitOpsDeployment E2E tests", func() {
	Context("Create, Update and Delete a GitOpsDeployment ", func() {

		var config *rest.Config
		var k8sClient client.Client
		ctx := context.Background()

		// this assumes that service is running on non aware kcp client
		BeforeEach(func() {
			var err error
			config, err = fixture.GetSystemKubeConfig()
			Expect(err).ToNot(HaveOccurred())

			k8sClient, err = fixture.GetKubeClient(config)
			Expect(err).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			// At end of a test, output the state of Argo CD Application, for post-mortem debuggign
			err := fixture.ReportRemainingArgoCDApplications(k8sClient)
			Expect(err).ToNot(HaveOccurred())
		})

		// Generate expected resources for fixture.RepoURL
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
				// {
				// 	Group:     "route.openshift.io",
				// 	Version:   "v1",
				// 	Kind:      "Route",
				// 	Namespace: fixture.GitOpsServiceE2ENamespace,
				// 	Name:      name,
				// 	Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
				// 	Health: &managedgitopsv1alpha1.HealthStatus{
				// 		Status:  managedgitopsv1alpha1.HeathStatusCodeHealthy,
				// 		Message: "Route is healthy",
				// 	},
				// },
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

		It("Should ensure successful creation of GitOpsDeployment, by creating the GitOpsDeployment and ensure that appProjectRepository row has been created referencing to gitopsDeployment", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
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
					Namespace: fixture.GitOpsServiceE2ENamespace,
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

			By("verify whether appProjectRepository row pointing to GitopsDeployment has been created in database ")

			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).ToNot(HaveOccurred())

			namespace := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: gitOpsDeploymentResource.Namespace,
				},
			}

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace)
			Expect(err).ToNot(HaveOccurred())

			clusterUser := db.ClusterUser{
				User_name: string(namespace.UID),
			}

			err = dbQueries.GetClusterUserByUsername(ctx, &clusterUser)
			Expect(err).ToNot(HaveOccurred())

			appProjectRepositoryDB := db.AppProjectRepository{
				Clusteruser_id: clusterUser.Clusteruser_id,
				RepoURL:        sharedresourceloop.NormalizeGitURL(gitOpsDeploymentResource.Spec.Source.RepoURL),
			}

			err = dbQueries.GetAppProjectRepositoryByClusterUserAndRepoURL(ctx, &appProjectRepositoryDB)
			Expect(err).ToNot(HaveOccurred())

			By("Ensure AppProject resource has been created")
			appProject := &appv1.AppProject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app-project-" + appProjectRepositoryDB.Clusteruser_id,
					Namespace: "gitops-service-argocd",
				},
			}

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())
			app := &appv1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      argocd.GenerateArgoCDApplicationName(string(gitOpsDeploymentResource.UID)),
					Namespace: "gitops-service-argocd",
				},
			}
			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(app), app)
			Expect(err).ToNot(HaveOccurred())
			if util.AppProjectIsolationEnabled() {
				By("verifying whether Argo CD Application CR references AppProject via the .spec.project")
				Expect(app.Spec.Project).To(Equal(appProject.Name))
			} else {
				By("verifying whether Argo CD Application CR uses default AppProject")
				Expect(app.Spec.Project).To(Equal("default"))
			}

			By("deleting the GitOpsDeployment resource and waiting for the resources to be deleted")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
			expectAllResourcesToBeDeleted(expectedResourceStatusList)

			By("Ensure the AppProject doesn't exist.")
			Eventually(func() bool {
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(appProject), appProject)
				return apierr.IsNotFound(err)
			}, time.Minute, time.Second*5).Should(BeTrue())

		})

		It("should ensure that when 2 GitOpsDeployments are created and point to the same repo url, and one is deleted, the AppProjectRepository for the other still exists", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())
			gitOpsDeploymentResource1 := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource1, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentResource2 := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName+"2",
				fixture.RepoURL, "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			err = k8s.Create(&gitOpsDeploymentResource2, k8sClient)
			Expect(err).To(Succeed())

			By("verify whether appProjectRepository row pointing to GitopsDeployment has been created in database ")

			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).ToNot(HaveOccurred())

			namespace := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: gitOpsDeploymentResource1.Namespace,
				},
			}

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace)
			Expect(err).ToNot(HaveOccurred())

			clusterUser := db.ClusterUser{
				User_name: string(namespace.UID),
			}

			err = dbQueries.GetClusterUserByUsername(ctx, &clusterUser)
			Expect(err).ToNot(HaveOccurred())

			By("ensuring AppProject resource has been created")
			appProject := &appv1.AppProject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app-project-" + clusterUser.Clusteruser_id,
					Namespace: "gitops-service-argocd",
				},
			}

			Eventually(appProject, "60s", "1s").Should(k8s.ExistByName(k8sClient))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)).Error().ToNot(HaveOccurred())

			Expect(appProject.Spec.SourceRepos).Should(ContainElement(fixture.RepoURL))

			By("deleting the second GitOpsDeployment targeting the git repository")
			Expect(k8sClient.Delete(ctx, &gitOpsDeploymentResource2)).Error().ToNot(HaveOccurred())

			Consistently(appProject, "20s", "1s").Should(k8s.ExistByName(k8sClient), "the AppProject should not be deleted, because there still exists a GitOpsDeployment that is using it")

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)).Error().ToNot(HaveOccurred())

			Expect(appProject.Spec.SourceRepos).Should(ContainElement(fixture.RepoURL), "it should still reference the repoURL from the GitOpsDeployment")

			By("deleting the first GitOpsDeployment targeting the git repository")
			Expect(k8sClient.Delete(ctx, &gitOpsDeploymentResource1)).Error().ToNot(HaveOccurred())

			Eventually(appProject, "20s", "1s").ShouldNot(k8s.ExistByName(k8sClient), "the AppProject should be deleted now that the final reference has been removed")

		})

		It("Should ensure synchronicity of create and update of GitOpsDeployment, by ensurng no updates are done in existing deployment, if CR is submitted again without any changes", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
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

			gitOpsDeploymentResource.Spec.Source.RepoURL = fixture.RepoURL
			gitOpsDeploymentResource.Spec.Source.Path = fixture.GitopsDeploymentPath

			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).ToNot(HaveOccurred())
			Expect(gitOpsDeploymentResource.Spec.Source.RepoURL).To(Equal(fixture.RepoURL))
			Expect(gitOpsDeploymentResource.Spec.Source.Path).To(Equal(fixture.GitopsDeploymentPath))

			expectedReconciledStateField := managedgitopsv1alpha1.ReconciledState{
				Source: managedgitopsv1alpha1.GitOpsDeploymentSource{
					RepoURL: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Path:    gitOpsDeploymentResource.Spec.Source.Path,
				},
				Destination: managedgitopsv1alpha1.GitOpsDeploymentDestination{
					Name:      gitOpsDeploymentResource.Spec.Destination.Environment,
					Namespace: fixture.GitOpsServiceE2ENamespace,
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
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
			Expect(err).ToNot(HaveOccurred())
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
				"https://github.com/managed-gitops-test-data/deployment-permutations-a", "pathB", "branchA",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

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
					Namespace: fixture.GitOpsServiceE2ENamespace,
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
					Namespace: fixture.GitOpsServiceE2ENamespace,
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildTargetRevisionGitOpsDeploymentResource("gitops-depl-test-status",
				fixture.RepoURL, fixture.GitopsDeploymentPath, "xyz",
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
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource("gitops-depl-test-status",
				fixture.RepoURL, fixture.GitopsDeploymentPath,
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

		It("Checks whether the GitOpsDeployment can be deleted in the presence of a finalizer", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment resource with the deletion finalizer")
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource("gitops-depl-test-status",
				fixture.RepoURL, fixture.GitopsDeploymentPath,
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			gitOpsDeploymentResource.Finalizers = append(gitOpsDeploymentResource.Finalizers, managedgitopsv1alpha1.DeletionFinalizer)

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

			// We add a finalizer to one of the resources to prevent a race condition where the GitOpsDeployment and
			// all the dependent resources might get deleted before the test can check if the finalizer has prevented the deletion.
			testFinalizer := "kubernetes"
			resStatus := expectedResourceStatusList[0]

			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resStatus.Name,
					Namespace: resStatus.Namespace,
				},
			}

			err = k8s.UpdateWithoutConflict(cm, k8sClient, func(o client.Object) {
				o.SetFinalizers([]string{testFinalizer})
			})
			Expect(err).ToNot(HaveOccurred())

			By("delete the GitOpsDeployment resource")

			err = k8sClient.Delete(ctx, &gitOpsDeploymentResource)
			Expect(err).To(Succeed())

			By("verify if the finalizer has prevented the GitOpsDeployment from deletion")
			err = k8s.Get(&gitOpsDeploymentResource, k8sClient)
			Expect(err).ToNot(HaveOccurred())

			Eventually(&gitOpsDeploymentResource, "60s", "1s").Should(k8s.HasNonNilDeletionTimestamp(k8sClient))

			// check if the GitOps Service has not removed the finalizer before the dependent resources are deleted.
			Consistently(&gitOpsDeploymentResource, "30s", "1s").Should(k8s.ExistByName(k8sClient))

			// Remove the test finalizer and verify if all the dependent resources are deleted
			err = k8s.UpdateWithoutConflict(cm, k8sClient, func(o client.Object) {
				o.SetFinalizers([]string{})
			})
			Expect(err).ToNot(HaveOccurred())

			expectAllResourcesToBeDeleted(expectedResourceStatusList)

			By("verify if the GitOpsDeployment is deleted after all the dependencies are removed")
			Eventually(&gitOpsDeploymentResource, "30s", "1s").Should(k8s.NotExist(k8sClient))
		})

		It("Checks for failure of deployment when an invalid input is provided", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("ensuring GitOpsDeployment Fails to create and deploy application when an invalid field input is passed")
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource("test-should-fail",
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

		It("Create and delete the same GitOpsDeployment 3 times in a row, and make sure each works", func() {

			// The goal of this test is to make sure the GitOpsService properly handles the deletion and
			// recreation of GitOpsDeployments with the same name/namespace/
			// - An example of a bug that would be caught by this test: what if the application event loop
			//   runner failed to cleanup up from an old GitOpsDeployment, and thus the event loop failed
			//   to process events for new GitOpsDeployments with the same name, in that namesapace.

			for count := 0; count < 3; count++ {

				Expect(fixture.EnsureCleanSlate()).To(Succeed())
				gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
					fixture.RepoURL, fixture.GitopsDeploymentPath,
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
						// gitopsDeplFixture.HaveReconciledState(expectedReconciledStateField),
					),
				)

				By("deleting the GitOpsDeployment resource and waiting for the resources to be deleted")

				err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
				Expect(err).To(Succeed())

				expectAllResourcesToBeDeleted(expectedResourceStatusList)

			}

		})

		It("Verify whether AppProject is pointing to the non-default AppProject and it includes the git repository referenced in the gitOpsDeployment repo", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("create a new GitOpsDeployment CR")
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("GitOpsDeployment should have expected health and status")
			Eventually(gitOpsDeploymentResource, "4m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy)))

			By("get the Application name created by the GitOpsDeployment resource")
			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).ToNot(HaveOccurred())

			appMapping := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: string(gitOpsDeploymentResource.UID),
			}
			err = dbQueries.GetDeploymentToApplicationMappingByDeplId(context.Background(), appMapping)
			Expect(err).ToNot(HaveOccurred())

			dbApplication := &db.Application{
				Application_id: appMapping.Application_id,
			}
			err = dbQueries.GetApplicationById(context.Background(), dbApplication)
			Expect(err).ToNot(HaveOccurred())

			By("verify that the Argo CD Application has been created")
			app := &appv1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dbApplication.Name,
					Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
				},
			}

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(app), app)
			Expect(err).ToNot(HaveOccurred())

			By("get namespace to fetch cluster user id")
			namespace := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: gitOpsDeploymentResource.Namespace,
				},
			}

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace)
			Expect(err).ToNot(HaveOccurred())

			clusterUser := db.ClusterUser{
				User_name: string(namespace.UID),
			}

			err = dbQueries.GetClusterUserByUsername(ctx, &clusterUser)
			Expect(err).ToNot(HaveOccurred())

			By("verify that AppProject has been created")
			appProject := &appv1.AppProject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app-project-" + clusterUser.Clusteruser_id,
					Namespace: "gitops-service-argocd",
				},
			}

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())

			By("verify whether AppProject is pointing to non-default project value")
			if util.AppProjectIsolationEnabled() {
				Expect(app.Spec.Project).To(Equal(appProject.Name))
			} else {
				Expect(app.Spec.Project).To(Equal("default"))
			}

			By("Verify whether AppProject includes the git repository referenced in the repo")

			Eventually(appProject, "30s", "1s").Should(
				appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
					SourceRepos: []string{gitOpsDeploymentResource.Spec.Source.RepoURL},
				}), "the repo URL of the AppProject should update to the value specified in the GitOpsDeployment")

			By("Create GitOpsDeploymentRepositoryCredential referencing to the the above repo")
			gitopsRepoCred := managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{
				ObjectMeta: metav1.ObjectMeta{
					Name:      repoCredCRToken,
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredentialSpec{
					Repository: gitOpsDeploymentResource.Spec.Source.RepoURL,
					Secret:     secretToken,
				},
			}

			err = k8s.Create(&gitopsRepoCred, k8sClient)
			Expect(err).To(Succeed())

			Eventually(appProject, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource.Spec.Source.RepoURL},
					}),
				),
			)

			Consistently(appProject, "20s", "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource.Spec.Source.RepoURL},
					}),
				),
			)

			By("Delete GitOpsDeploymentRepositoryCredential")
			err = k8s.Delete(&gitopsRepoCred, k8sClient)
			Expect(err).To(Succeed())

			By("Verify that AppProject should still include the git repository after 30 seconds")
			Eventually(appProject, "30s", "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource.Spec.Source.RepoURL},
					}),
				),
			)

			By("Delete GitOpsDeployment Resource")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

		})

		It("Validate the expected behavior of GitOps deployments and their impact on the AppProject configuration", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment resource targeting a repo A")
			gitOpsDeploymentResource1 := gitopsDeplFixture.BuildTargetRevisionGitOpsDeploymentResource("gitops-depl-test",
				"https://github.com/managed-gitops-test-data/deployment-permutations-a", "pathB", "branchA",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			err := k8s.Create(&gitOpsDeploymentResource1, k8sClient)
			Expect(err).To(Succeed())

			createResourceStatusList_deploymentPermutations := createResourceStatusListFunction_deploymentPermutations()

			createResourceStatusList_deploymentPermutations = append(createResourceStatusList_deploymentPermutations,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathb")...)
			err = k8s.Get(&gitOpsDeploymentResource1, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource1, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(createResourceStatusList_deploymentPermutations),
				),
			)

			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).ToNot(HaveOccurred())

			By("Retrieve namespace to get cluster user id")
			namespace := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: gitOpsDeploymentResource1.Namespace,
				},
			}

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace)
			Expect(err).ToNot(HaveOccurred())

			clusterUser := db.ClusterUser{
				User_name: string(namespace.UID),
			}

			err = dbQueries.GetClusterUserByUsername(ctx, &clusterUser)
			Expect(err).ToNot(HaveOccurred())

			By("Verify whether AppProject includes the git repository referenced in the repo A")
			appProject := &appv1.AppProject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app-project-" + clusterUser.Clusteruser_id,
					Namespace: "gitops-service-argocd",
				},
			}

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())

			Eventually(appProject, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource1.Spec.Source.RepoURL},
					}),
				),
			)

			By("Creating a second gitopsdeployment targetting a different repo")
			gitOpsDeploymentResource2 := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			err = k8s.Create(&gitOpsDeploymentResource2, k8sClient)
			Expect(err).To(Succeed())

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

			err = k8s.Get(&gitOpsDeploymentResource2, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource2, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
				),
			)

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())

			Eventually(appProject, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource1.Spec.Source.RepoURL,
							gitOpsDeploymentResource2.Spec.Source.RepoURL},
					}),
				),
			)

			By("Delete gitopdeployment pointing to repo A")
			err = k8s.Delete(&gitOpsDeploymentResource1, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource1, k8sClient)
			Expect(err).ToNot(Succeed())

			By("Ensure the AppProject now only contains repo B")

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())

			Eventually(appProject, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
						SourceRepos: []string{gitOpsDeploymentResource2.Spec.Source.RepoURL},
					}),
				),
			)

			By("Delete gitopdeployment pointing to repo B")
			err = k8s.Delete(&gitOpsDeploymentResource2, k8sClient)
			Expect(err).To(Succeed())

			err = k8s.Get(&gitOpsDeploymentResource2, k8sClient)
			Expect(err).ToNot(Succeed())

			By("Ensure the AppProject doesn't exist.")
			Eventually(func() bool {
				err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(appProject), appProject)
				return apierr.IsNotFound(err)
			}, time.Minute, time.Second*5).Should(BeTrue())
		})

		It("Verify that updating the Git repo URL of a GitOpsDeployment causes the corresponding AppProject to be updated", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating a new GitOpsDeployment CR")
			gitOpsDeploymentResource := gitopsDeplFixture.BuildGitOpsDeploymentResource(fixture.GitopsDeploymentName,
				fixture.RepoURL, fixture.GitopsDeploymentPath,
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("ensuring GitOpsDeployment should have expected health and status")
			Eventually(gitOpsDeploymentResource, "4m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy)))

			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).ToNot(HaveOccurred())

			By("retrieving namespace to fetch cluster user id")
			namespace := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: gitOpsDeploymentResource.Namespace,
				},
			}

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace)
			Expect(err).ToNot(HaveOccurred())

			clusterUser := db.ClusterUser{
				User_name: string(namespace.UID),
			}

			err = dbQueries.GetClusterUserByUsername(ctx, &clusterUser)
			Expect(err).ToNot(HaveOccurred())

			By("verifying that AppProject has been created")
			appProject := &appv1.AppProject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app-project-" + clusterUser.Clusteruser_id,
					Namespace: "gitops-service-argocd",
				},
			}

			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(appProject), appProject)
			Expect(err).ToNot(HaveOccurred())

			By("verifying whether AppProject includes the git repository referenced in the repo")

			Eventually(appProject, "30s", "1s").Should(
				appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
					SourceRepos: []string{gitOpsDeploymentResource.Spec.Source.RepoURL},
				}), "the repo URL of the AppProject should update to the value specified in the GitOpsDeployment")

			By("updating the GitOpsDeployment to point to a new URL")

			newRepoURL := "https://github.com/redhat-appstudio/a-different-url" // it doesn't need to be a real git repo for this test to pass

			Expect(gitopsDeplFixture.UpdateDeploymentWithFunction(&gitOpsDeploymentResource, func(gitopsDepl *managedgitopsv1alpha1.GitOpsDeployment) {

				gitopsDepl.Spec.Source.RepoURL = newRepoURL

			})).Error().To(Succeed())

			Eventually(appProject, "30s", "1s").Should(
				appProjectFixture.HaveAppProjectSourceRepos(appv1.AppProjectSpec{
					SourceRepos: []string{newRepoURL},
				}), "the repo URL of the AppProject should update to the NEW value specified in the GitOpsDeployment, and it should not contain the old value")

		})

	})

	Context("Simulates simple interactions of X active users interacting with the service", func() {

		// Number of users interacting with GitOps service.
		const (
			numberToSimulate = 10
		)

		var k8sClient client.Client

		BeforeEach(func() {
			var config *rest.Config

			var err error
			config, err = fixture.GetSystemKubeConfig()
			Expect(err).ToNot(HaveOccurred())

			k8sClient, err = fixture.GetKubeClient(config)
			Expect(err).ToNot(HaveOccurred())

			// Ensure that all user-* namespaces are deleted at start of tests
			for i := 1; i <= numberToSimulate; i++ {

				namespace := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("user-%d", i),
					},
				}

				if fixture.EnableNamespaceBackedArgoCD {
					namespace.Labels = map[string]string{
						"argocd.argoproj.io/managed-by": "gitops-service-argocd",
					}
				}

				if err = k8sClient.Delete(context.Background(), namespace); err != nil {
					Expect(apierr.IsNotFound(err)).To(BeTrue())
				}

				Eventually(namespace, "2m", "1s").ShouldNot(k8s.ExistByName(k8sClient))
			}

		})

		AfterEach(func() {
			// At end of a test, output the state of Argo CD Application, for post-mortem debuggign
			err := fixture.ReportRemainingArgoCDApplications(k8sClient)
			Expect(err).ToNot(HaveOccurred())
		})

		config, err := fixture.GetSystemKubeConfig()
		Expect(err).ToNot(HaveOccurred())

		// testPrintln outputs a log statement along with the particular test #
		testPrintln := func(str string, i int) {
			fmt.Printf("%v - %d) %s\n", time.Now(), i, str)
		}

		// runTestInner returns error if the test fail, nil otherwise.
		runTestInner := func(i int) error {

			testPrintln("Create new namespace", i)
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("user-%d", i),
				},
			}

			if fixture.EnableNamespaceBackedArgoCD {
				namespace.Labels = map[string]string{
					"argocd.argoproj.io/managed-by": "gitops-service-argocd",
				}
			}

			if err := k8s.Create(namespace, k8sClient); err != nil {
				return err
			}

			// Generate  expected resource from "https://github.com/managed-gitops-test-data/deployment-permutations-*" using name
			getResourceStatusList_deploymentPermutations := func(name string) []managedgitopsv1alpha1.ResourceStatus {
				return []managedgitopsv1alpha1.ResourceStatus{
					{
						Group:     "",
						Version:   "v1",
						Kind:      "ConfigMap",
						Name:      name,
						Namespace: namespace.ObjectMeta.Name,
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
						Namespace: namespace.ObjectMeta.Name,
						Status:    managedgitopsv1alpha1.SyncStatusCodeSynced,
					},
				}
			}

			testPrintln("Create GitopsDeploymentResource", i)
			gitOpsDeploymentResource := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("my-gitops-depl-%d", i),
					Namespace: namespace.ObjectMeta.Name,
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL:        "https://github.com/managed-gitops-test-data/deployment-permutations-a",
						Path:           "pathB",
						TargetRevision: "branchA",
					},
					Destination: managedgitopsv1alpha1.ApplicationDestination{},
					Type:        managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
				},
			}

			if err := k8s.Create(gitOpsDeploymentResource, k8sClient); err != nil {
				return err
			}

			testPrintln("Make sure it deploys the target resources", i)
			createResourceStatusList_deploymentPermutations := createResourceStatusListFunction_deploymentPermutations()

			createResourceStatusList_deploymentPermutations = append(createResourceStatusList_deploymentPermutations,
				getResourceStatusList_deploymentPermutations("deployment-permutations-a-brancha-pathb")...)
			if err := k8s.Get(gitOpsDeploymentResource, k8sClient); err != nil {
				return err
			}

			if err := wait.PollImmediate(time.Second*1, time.Minute*10, func() (done bool, err error) {

				if !gitopsDeplFixture.HaveSyncStatusCodeFunc(managedgitopsv1alpha1.SyncStatusCodeSynced, *gitOpsDeploymentResource) {
					return false, nil
				}

				if !gitopsDeplFixture.HaveHealthStatusCodeFunc(managedgitopsv1alpha1.HeathStatusCodeHealthy, *gitOpsDeploymentResource) {
					return false, nil
				}

				if !gitopsDeplFixture.HaveResourcesFunc(createResourceStatusList_deploymentPermutations, *gitOpsDeploymentResource) {
					return false, nil
				}

				return true, nil

			}); err != nil {
				return err
			}

			testPrintln("Modify the gitops deployment to a new repoURL", i)
			if err := k8s.Get(gitOpsDeploymentResource, k8sClient); err != nil {
				return err
			}

			gitOpsDeploymentResource.Spec.Source.RepoURL = "https://github.com/managed-gitops-test-data/deployment-permutations-b"

			testPrintln("updating the CR with changes in repoURLL", i)
			if err := k8s.Update(gitOpsDeploymentResource, k8sClient); err != nil {
				return err
			}

			expectedResourceStatusList := createResourceStatusListFunction_deploymentPermutations()
			expectedResourceStatusList = append(expectedResourceStatusList,
				getResourceStatusList_deploymentPermutations("deployment-permutations-b-brancha-pathb")...)

			if err := wait.PollImmediate(time.Second*1, time.Minute*10, func() (done bool, err error) {

				if !gitopsDeplFixture.HaveSyncStatusCodeFunc(managedgitopsv1alpha1.SyncStatusCodeSynced, *gitOpsDeploymentResource) {
					return false, nil
				}

				if !gitopsDeplFixture.HaveHealthStatusCodeFunc(managedgitopsv1alpha1.HeathStatusCodeHealthy, *gitOpsDeploymentResource) {
					return false, nil
				}

				if !gitopsDeplFixture.HaveResourcesFunc(expectedResourceStatusList, *gitOpsDeploymentResource) {
					return false, nil
				}

				return true, nil

			}); err != nil {
				return err
			}

			testPrintln("delete the GitOpsDeployment resource", i)
			if err := k8s.Delete(gitOpsDeploymentResource, k8sClient); err != nil {
				return err
			}

			testPrintln("delete namespace", i)
			if err := fixture.DeleteNamespace(namespace.ObjectMeta.Name, config); err != nil {
				return err
			}

			return nil
		}

		// runTests calls the inner test function, and returns the result on the channel
		runTest := func(i int, resChannel chan<- error, wg *sync.WaitGroup) {

			result := runTestInner(i)
			wg.Done()
			resChannel <- result
		}

		It("Simulates simple interactions of X active users interacting with the service ", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			resultChan := make(chan error)
			var wg sync.WaitGroup

			wg.Add(numberToSimulate)

			for i := 1; i <= numberToSimulate; i++ {
				go runTest(i, resultChan, &wg)
			}

			fmt.Println("Waiting for goroutines to finish...")
			wg.Wait()

			for j := 1; j <= numberToSimulate; j++ {
				res := <-resultChan
				fmt.Println("read from channel ", res)
				Expect(res).ToNot(HaveOccurred())
			}

			fmt.Println("Done!")
		})

	})

})

// Returns true if the all param resources no longer exist (have been deleted), and false otherwise.
func expectAllResourcesToBeDeleted(expectedResourceStatusList []managedgitopsv1alpha1.ResourceStatus) {

	Eventually(func() bool {

		k8sclient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
		Expect(err).To(Succeed())

		for _, resourceValue := range expectedResourceStatusList {
			resourceName := typed.NamespacedName{
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

			// The object should not exist: a NotFound error should be returned, otherwise return false.
			if err := k8sclient.Get(context.Background(), resourceName, obj); err == nil || !apierr.IsNotFound(err) {
				fmt.Println("Waiting for resource to be deleted:", resourceName)
				return false
			}
		}

		// If all the resources were found, return true
		return true
	}, ArgoCDReconcileWaitTime, "1s").Should(BeTrue(), "all resources should be deleted")

}
