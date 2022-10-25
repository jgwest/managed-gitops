package core

import (
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("GitOpsDeployment E2E tests", func() {

	Context("Create a new GitOpsDeployment", func() {

		It("should be healthy and have synced status, and resources should be deployed", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("creating the GitOpsDeployment")

			gitOpsDeploymentResource := buildGitOpsDeploymentResource("my-gitops-depl",
				"https://github.com/redhat-appstudio/managed-gitops", "resources/test-data/sample-gitops-repository/environments/overlays/dev",
				managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated)

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("ensuring GitOpsDeployment should have expected health and status")

			Eventually(gitOpsDeploymentResource, "2m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveSyncStatusCode(managedgitopsv1alpha1.SyncStatusCodeSynced),
					gitopsDeplFixture.HaveHealthStatusCode(managedgitopsv1alpha1.HeathStatusCodeHealthy)))

			By("ensuring the resources of the GitOps repo are successfully deployed")

			componentADepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-a", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			componentBDepl := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "component-b", Namespace: fixture.GitOpsServiceE2ENamespace},
			}
			Eventually(componentADepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))
			Eventually(componentBDepl, "60s", "1s").Should(k8s.ExistByName(k8sClient))

			By("deleting the GitOpsDeployment")

			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			By("ensuring the resources of the GitOps repo are successfully deleted")

			Eventually(componentADepl, "60s", "1s").ShouldNot(k8s.ExistByName(k8sClient))
			Eventually(componentBDepl, "60s", "1s").ShouldNot(k8s.ExistByName(k8sClient))

		})
	})
})

// buildGitOpsDeploymentResource builds a GitOpsDeployment with 'opinionated' default values, which is self-contained to
// the GitGitOpsServiceE2ENamespace. This makes it easy to clean up after tests using EnsureCleanSlate.
// - Defaults to creation in GitOpsServiceE2ENamespace
// - Defaults to deployment of K8s resources to GitOpsServiceE2ENamespace
func buildGitOpsDeploymentResource(name, repoURL, path, deploymentSpecType string) managedgitopsv1alpha1.GitOpsDeployment {

	gitOpsDeploymentResource := managedgitopsv1alpha1.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
			Source: managedgitopsv1alpha1.ApplicationSource{
				RepoURL: repoURL,
				Path:    path,
			},
			Destination: managedgitopsv1alpha1.ApplicationDestination{},
			Type:        deploymentSpecType,
		},
	}

	return gitOpsDeploymentResource
}

func buildTargetRevisionGitOpsDeploymentResource(name, repoURL, path, target, deploymentSpecType string) managedgitopsv1alpha1.GitOpsDeployment {
	gitOpsDeploymentResource := buildGitOpsDeploymentResource(name, repoURL, path, deploymentSpecType)
	gitOpsDeploymentResource.Spec.Source.TargetRevision = target
	return gitOpsDeploymentResource
}
