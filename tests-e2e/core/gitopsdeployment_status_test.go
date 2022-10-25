package core

import (
	"context"

	appv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	appFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/application"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("GitOpsDeployment Status Tests", func() {
	Context("Status field of GitOpsDeployment is updated accurately", func() {
		It("GitOpsDeployment .status.resources field is populated with the right resources", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("create a new GitOpsDeployment resource")
			gitOpsDeploymentResource := buildGitOpsDeploymentResource("gitops-depl-test-status",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("ensuring the GitOpsDeployment status field have health, sync and resources fields populated")
			getResourceStatusList := func(name string) []managedgitopsv1alpha1.ResourceStatus {
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
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList("component-a")...)
			expectedResourceStatusList = append(expectedResourceStatusList, getResourceStatusList("component-b")...)

			Eventually(gitOpsDeploymentResource, "5m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
					gitopsDeplFixture.HaveResources(expectedResourceStatusList),
				),
			)

			By("delete the GitOpsDeployment resource")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})
	})
})

var _ = Describe("GitOpsDeployment SyncError test", func() {

	Context("Errors are set properly in Status.Sync.SyncError field of GitOpsDeployment", func() {

		It("ensures that GitOpsDeployment .status.sync.syncError field contains the syncError if Application is not synced and error type of the error is SyncError ", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("create an invalid GitOpsDeployment application")
			gitOpsDeploymentResource := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-gitops-depl",
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL: "https://github.com/managed-gitops-test-data/gitops-repositories",
						Path:    "invalid-repository",
					},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
				},
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			Eventually(gitOpsDeploymentResource, ArgoCDReconcileWaitTime, "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeOutOfSync),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				),
			)

			By("Get the Application name created by the GitOpsDeployment resource")
			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).To(BeNil())

			appMapping := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: string(gitOpsDeploymentResource.UID),
			}
			err = dbQueries.GetDeploymentToApplicationMappingByDeplId(context.Background(), appMapping)
			Expect(err).To(BeNil())

			dbApplication := &db.Application{
				Application_id: appMapping.Application_id,
			}
			err = dbQueries.GetApplicationById(context.Background(), dbApplication)
			Expect(err).To(BeNil())

			app := appv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dbApplication.Name,
					Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
				},
			}

			By("Wait for Argo CD to reconcile and set the conditions.Message field on the Application with the syncError")
			expectedConditionsOfApplication := appv1alpha1.ApplicationStatus{
				Conditions: []appv1alpha1.ApplicationCondition{
					{
						Message: "Failed sync attempt to 5ce3833a57c1582f93ea49c2947a5b4b4992ef6f: one or more synchronization tasks are not valid (retried 5 times).",
					},
				},
			}

			Eventually(app, "5m", "1s").Should(
				SatisfyAll(
					appFixture.HaveApplicationSyncError(expectedConditionsOfApplication),
				),
			)

			expectedConditions := []managedgitopsv1alpha1.GitOpsDeploymentCondition{
				{
					Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionSyncError,
					Message: "Failed sync attempt to 5ce3833a57c1582f93ea49c2947a5b4b4992ef6f: one or more synchronization tasks are not valid (retried 5 times).",
					Status:  managedgitopsv1alpha1.GitOpsConditionStatusTrue,
					Reason:  managedgitopsv1alpha1.GitopsDeploymentReasonSyncError,
				},
			}

			Eventually(gitOpsDeploymentResource, "5m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveConditions(expectedConditions),
				),
			)

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&gitOpsDeploymentResource), &gitOpsDeploymentResource)
			Expect(err).To(Succeed())

			By("Update the gitOpsDeploymentResource to fix the failure")
			gitOpsDeploymentResource.Spec.Source.RepoURL = "https://github.com/redhat-appstudio/managed-gitops"
			gitOpsDeploymentResource.Spec.Source.Path = "resources/test-data/sample-gitops-repository/environments/overlays/dev"

			err = k8s.Update(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("Wait until ArgoCD Application .conditions.Message value is cleared")
			updateExpectedConditionsOfApplication := appv1alpha1.ApplicationStatus{
				Conditions: []appv1alpha1.ApplicationCondition{
					{
						Message: "",
					},
				},
			}

			Eventually(app, "5m", "1s").Should(
				SatisfyAll(
					appFixture.HaveApplicationSyncError(updateExpectedConditionsOfApplication),
				),
			)

			By("Wait until GitopsDeployment condition statis is false")
			expectedConditionsUpdate := []managedgitopsv1alpha1.GitOpsDeploymentCondition{
				{
					Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionSyncError,
					Message: "Failed sync attempt to 5ce3833a57c1582f93ea49c2947a5b4b4992ef6f: one or more synchronization tasks are not valid (retried 5 times).",
					Status:  managedgitopsv1alpha1.GitOpsConditionStatusFalse,
					Reason:  "SyncErrorResolved",
				},
			}

			Eventually(gitOpsDeploymentResource, "5m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveConditions(expectedConditionsUpdate),
				),
			)

			By("delete the GitOpsDeployment resource")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})
	})
})
