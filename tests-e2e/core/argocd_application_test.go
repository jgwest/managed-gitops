package core

import (
	"context"

	appv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	appFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/application"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
)

var _ = Describe("Argo CD Application", func() {
	Context("Creating GitOpsDeployment should result in an Argo CD Application", func() {
		It("Argo CD Application should have has prune, allowEmpty and selfHeal enabled", func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("create a new GitOpsDeployment CR")
			gitOpsDeployment := buildGitOpsDeploymentResource("my-gitops-depl-automated",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeployment, k8sClient)
			Expect(err).To(Succeed())

			By("GitOpsDeployment should have expected health and status")
			Eventually(gitOpsDeployment, "4m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy)))

			By("get the Application name created by the GitOpsDeployment resource")
			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).To(BeNil())

			appMapping := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: string(gitOpsDeployment.UID),
			}
			err = dbQueries.GetDeploymentToApplicationMappingByDeplId(context.Background(), appMapping)
			Expect(err).To(BeNil())

			dbApplication := &db.Application{
				Application_id: appMapping.Application_id,
			}
			err = dbQueries.GetApplicationById(context.Background(), dbApplication)
			Expect(err).To(BeNil())

			By("verify that the Argo CD Application has prune, allowEmpty and selfHeal enabled")
			app := appv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dbApplication.Name,
					Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
				},
			}
			Eventually(app, "60s", "1s").Should(appFixture.HaveAutomatedSyncPolicy(appv1alpha1.SyncPolicyAutomated{Prune: true, SelfHeal: true, AllowEmpty: true}))
		})

	})
})
