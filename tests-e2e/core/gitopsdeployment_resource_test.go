package core

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	gitopsDeplFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/gitopsdeployment"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("GitOpsDeployment Condition Tests", func() {

	Context("Errors are set properly in Status.Conditions field of GitOpsDeployment", func() {

		It("ensures that GitOpsDeployment .status.condition field is populated with the corrrect error when an illegal GitOpsDeployment is created", func() {

			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("create an invalid GitOpsDeployment application")
			gitOpsDeploymentResource := managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "managed-environment-gitops-depl",
					Namespace: fixture.GitOpsServiceE2ENamespace,
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL: "https://github.com/redhat-appstudio/managed-gitops",
						Path:    "resources/test-data/sample-gitops-repository/environments/overlays/dev",
					},
					Destination: managedgitopsv1alpha1.ApplicationDestination{
						Environment: "does-not-exist", // This is the invalid value, which should return a condition error
						Namespace:   "",
					},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
				},
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())

			expectedConditions := []managedgitopsv1alpha1.GitOpsDeploymentCondition{
				{
					Type:    managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred,
					Message: "an unknown error occurred",
					Status:  managedgitopsv1alpha1.GitOpsConditionStatusTrue,
					Reason:  managedgitopsv1alpha1.GitopsDeploymentReasonErrorOccurred,
				},
			}

			Eventually(gitOpsDeploymentResource, "5m", "1s").Should(
				SatisfyAll(
					gitopsDeplFixture.HaveConditions(expectedConditions),
				),
			)

			By("delete the GitOpsDeployment resource")
			err = k8s.Delete(&gitOpsDeploymentResource, k8sClient)
			Expect(err).To(Succeed())
		})
	})
})
