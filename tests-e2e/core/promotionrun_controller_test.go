package core

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appstudiocontroller "github.com/redhat-appstudio/managed-gitops/appstudio-controller/controllers/appstudio.redhat.com"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appstudiosharedv1 "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	bindingFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/binding"
	promotionRunFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/promotionrun"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Application Promotion Run E2E Tests.", func() {
	Context("Testing Application Promotion Run Reconciler.", func() {
		var environmentProd appstudiosharedv1.Environment
		var bindingStage appstudiosharedv1.SnapshotEnvironmentBinding
		var bindingProd appstudiosharedv1.SnapshotEnvironmentBinding
		var promotionRun appstudiosharedv1.PromotionRun

		BeforeEach(func() {
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("Create Staging Environment.")
			environmentStage := buildEnvironmentResource("staging", "Staging Environment", "staging", appstudiosharedv1.EnvironmentType_POC)
			err = k8s.Create(&environmentStage, k8sClient)
			Expect(err).To(Succeed())

			By("Create Production Environment.")
			environmentProd = buildEnvironmentResource("prod", "Production Environment", "prod", appstudiosharedv1.EnvironmentType_POC)
			err = k8s.Create(&environmentProd, k8sClient)
			Expect(err).To(Succeed())

			By("Create Snapshot.")
			snapshot := buildSnapshotResource("my-snapshot", "new-demo-app", "Staging Snapshot", "Staging Snapshot", "component-a", "quay.io/jgwest-redhat/sample-workload:latest")
			err = k8s.Create(&snapshot, k8sClient)
			Expect(err).To(Succeed())

			By("Create Staging Binding.")
			bindingStage = buildSnapshotEnvironmentBindingResource("appa-staging-binding", "new-demo-app", "staging", "my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&bindingStage, k8sClient)
			Expect(err).To(Succeed())

			By("Update Status field.")
			err = buildAndUpdateBindingStatus(bindingStage.Spec.Components,
				"https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging", "resources/test-data/sample-gitops-repository/components/componentB/overlays/staging"}, &bindingStage)
			Expect(err).To(Succeed())

			By("Create Production Binding.")
			bindingProd = buildSnapshotEnvironmentBindingResource("appa-prod-binding", "new-demo-app", "prod",
				"my-snapshot", 3, []string{"component-a"})
			err = k8s.Create(&bindingProd, k8sClient)
			Expect(err).To(Succeed())

			By("Update Status field.")
			err = buildAndUpdateBindingStatus(bindingProd.Spec.Components,
				"https://github.com/redhat-appstudio/managed-gitops", "main", "fdhyqtw",
				[]string{"resources/test-data/sample-gitops-repository/components/componentA/overlays/staging", "resources/test-data/sample-gitops-repository/components/componentB/overlays/staging"}, &bindingProd)
			Expect(err).To(Succeed())

			By("Verify that Status.GitOpsDeployments field of Binding is having Component and GitOpsDeployment name.")
			gitOpsDeploymentNameStage := appstudiocontroller.GenerateBindingGitOpsDeploymentName(bindingStage, bindingStage.Spec.Components[0].Name)
			expectedGitOpsDeploymentsStage := []appstudiosharedv1.BindingStatusGitOpsDeployment{
				{ComponentName: bindingStage.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentNameStage},
			}
			Eventually(bindingStage, "3m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeploymentsStage))

			gitOpsDeploymentNameProd := appstudiocontroller.GenerateBindingGitOpsDeploymentName(bindingProd, bindingProd.Spec.Components[0].Name)
			expectedGitOpsDeploymentsProd := []appstudiosharedv1.BindingStatusGitOpsDeployment{
				{ComponentName: bindingProd.Spec.Components[0].Name, GitOpsDeployment: gitOpsDeploymentNameProd},
			}
			Eventually(bindingProd, "3m", "1s").Should(bindingFixture.HaveStatusGitOpsDeployments(expectedGitOpsDeploymentsProd))

			By("Verify that GitOpsDeployments are created.")
			gitOpsDeploymentStage := v1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentNameStage,
					Namespace: bindingStage.Namespace,
				},
			}
			err = k8s.Get(&gitOpsDeploymentStage, k8sClient)
			Expect(err).To(Succeed())

			gitOpsDeploymentProd := v1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitOpsDeploymentNameProd,
					Namespace: bindingProd.Namespace,
				},
			}
			err = k8s.Get(&gitOpsDeploymentProd, k8sClient)
			Expect(err).To(Succeed())

			By("Create PromotionRun CR.")
			promotionRun = buildPromotionRunResource("new-demo-app-manual-promotion", "new-demo-app", "my-snapshot", "prod")
		})

		It("Should create GitOpsDeployments and it should be Synced/Healthy.", func() {
			// ToDo: https://issues.redhat.com/browse/GITOPSRVCE-234
			if fixture.IsRunningAgainstKCP() {
				Skip("Skipping this test in KCP until we fix the race condition")
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			By("Create PromotionRun CR.")
			err = k8s.Create(&promotionRun, k8sClient)
			Expect(err).To(Succeed())

			now := v1.Now()
			expectedPromotionRunStatus := appstudiosharedv1.PromotionRunStatus{
				State:            appstudiosharedv1.PromotionRunState_Complete,
				CompletionResult: appstudiosharedv1.PromotionRunCompleteResult_Success,
				ActiveBindings:   []string{bindingProd.Name},
				EnvironmentStatus: []appstudiosharedv1.PromotionRunEnvironmentStatus{
					{
						Step:            1,
						EnvironmentName: environmentProd.Name,
						Status:          appstudiosharedv1.PromotionRunEnvironmentStatus_Success,
						DisplayStatus:   appstudiocontroller.StatusMessageAllGitOpsDeploymentsAreSyncedHealthy,
					},
				},
				Conditions: []appstudiosharedv1.PromotionRunCondition{
					{
						Type:               appstudiosharedv1.PromotionRunConditionErrorOccurred,
						Message:            "",
						LastProbeTime:      now,
						LastTransitionTime: &now,
						Status:             appstudiosharedv1.PromotionRunConditionStatusFalse,
						Reason:             "",
					},
				},
			}

			Eventually(promotionRun, "3m", "1s").Should(promotionRunFixture.HaveStatusComplete(expectedPromotionRunStatus))
		})

		It("Should not support Auto Promotion.", func() {

			By("Create PromotionRun CR.")
			promotionRun.Spec.ManualPromotion = appstudiosharedv1.ManualPromotionConfiguration{}
			promotionRun.Spec.AutomatedPromotion = appstudiosharedv1.AutomatedPromotionConfiguration{
				InitialEnvironment: "staging",
			}

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&promotionRun, k8sClient)
			Expect(err).To(Succeed())

			expectedPromotionRunStatusConditions := appstudiosharedv1.PromotionRunStatus{
				Conditions: []appstudiosharedv1.PromotionRunCondition{
					{
						Type:    appstudiosharedv1.PromotionRunConditionErrorOccurred,
						Message: appstudiocontroller.ErrMessageAutomatedPromotionNotSupported,
						Status:  appstudiosharedv1.PromotionRunConditionStatusTrue,
						Reason:  appstudiosharedv1.PromotionRunReasonErrorOccurred,
					},
				},
			}

			Eventually(promotionRun, "3m", "1s").Should(promotionRunFixture.HaveStatusConditions(expectedPromotionRunStatusConditions))
		})

		It("Should not support invalid value for Target Environment.", func() {

			By("Create PromotionRun CR.")
			promotionRun.Spec.ManualPromotion.TargetEnvironment = ""

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&promotionRun, k8sClient)
			Expect(err).To(Succeed())

			expectedPromotionRunStatusConditions := appstudiosharedv1.PromotionRunStatus{
				Conditions: []appstudiosharedv1.PromotionRunCondition{
					{
						Type:    appstudiosharedv1.PromotionRunConditionErrorOccurred,
						Message: appstudiocontroller.ErrMessageTargetEnvironmentHasInvalidValue,
						Status:  appstudiosharedv1.PromotionRunConditionStatusTrue,
						Reason:  appstudiosharedv1.PromotionRunReasonErrorOccurred,
					},
				},
			}

			Eventually(promotionRun, "3m", "1s").Should(promotionRunFixture.HaveStatusConditions(expectedPromotionRunStatusConditions))
		})

		It("Should reset the Status.Conditions field if error is resolved.", func() {

			By("Create PromotionRun CR with invalid value.")
			promotionRun.Spec.ManualPromotion.TargetEnvironment = ""

			k8sClient, err := fixture.GetE2ETestUserWorkspaceKubeClient()
			Expect(err).To(Succeed())

			err = k8s.Create(&promotionRun, k8sClient)
			Expect(err).To(Succeed())

			expectedPromotionRunStatusConditions := appstudiosharedv1.PromotionRunStatus{
				Conditions: []appstudiosharedv1.PromotionRunCondition{
					{
						Type:    appstudiosharedv1.PromotionRunConditionErrorOccurred,
						Message: appstudiocontroller.ErrMessageTargetEnvironmentHasInvalidValue,
						Status:  appstudiosharedv1.PromotionRunConditionStatusTrue,
						Reason:  appstudiosharedv1.PromotionRunReasonErrorOccurred,
					},
				},
			}

			By("Verify that error is updated in Status.conditions field.")
			Eventually(promotionRun, "3m", "1s").Should(promotionRunFixture.HaveStatusConditions(expectedPromotionRunStatusConditions))

			err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&promotionRun), &promotionRun)
			Expect(err).To(Succeed())

			By("Update PromotionRun CR with invalid value.")
			promotionRun.Spec.ManualPromotion.TargetEnvironment = "prod"
			err = k8s.Update(&promotionRun, k8sClient)
			Expect(err).To(Succeed())

			expectedPromotionRunStatus := appstudiosharedv1.PromotionRunStatus{
				State:            appstudiosharedv1.PromotionRunState_Complete,
				CompletionResult: appstudiosharedv1.PromotionRunCompleteResult_Success,
				ActiveBindings:   []string{bindingProd.Name},
				EnvironmentStatus: []appstudiosharedv1.PromotionRunEnvironmentStatus{
					{
						Step:            1,
						EnvironmentName: environmentProd.Name,
						Status:          appstudiosharedv1.PromotionRunEnvironmentStatus_Success,
						DisplayStatus:   appstudiocontroller.StatusMessageAllGitOpsDeploymentsAreSyncedHealthy,
					},
				},
				Conditions: []appstudiosharedv1.PromotionRunCondition{
					{
						Type:    appstudiosharedv1.PromotionRunConditionErrorOccurred,
						Message: "",
						Status:  appstudiosharedv1.PromotionRunConditionStatusFalse,
						Reason:  "",
					},
				},
			}

			By("Verify that error is removed from Status.conditions field.")
			Eventually(promotionRun, "3m", "1s").Should(promotionRunFixture.HaveStatusComplete(expectedPromotionRunStatus))
		})
	})
})

func buildEnvironmentResource(name, displayName, parentEnvironment string, envType appstudiosharedv1.EnvironmentType) appstudiosharedv1.Environment {
	environment := appstudiosharedv1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: appstudiosharedv1.EnvironmentSpec{
			DisplayName:        displayName,
			Type:               envType,
			DeploymentStrategy: appstudiosharedv1.DeploymentStrategy_AppStudioAutomated,
			ParentEnvironment:  parentEnvironment,
			Tags:               []string{name},
			Configuration: appstudiosharedv1.EnvironmentConfiguration{
				Env: []appstudiosharedv1.EnvVarPair{},
			},
		},
	}

	return environment
}

func buildSnapshotResource(name, appName, displayName, displayDescription, componentName, containerImage string) appstudiosharedv1.Snapshot {
	snapshot := appstudiosharedv1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: appstudiosharedv1.SnapshotSpec{
			Application:        appName,
			DisplayName:        displayName,
			DisplayDescription: displayDescription,
			Components: []appstudiosharedv1.SnapshotComponent{
				{
					Name:           componentName,
					ContainerImage: containerImage,
				},
			},
		},
	}
	return snapshot
}

func buildPromotionRunResource(name, appName, snapshotName, targetEnvironment string) appstudiosharedv1.PromotionRun {

	promotionRun := appstudiosharedv1.PromotionRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fixture.GitOpsServiceE2ENamespace,
		},
		Spec: appstudiosharedv1.PromotionRunSpec{
			Snapshot:    snapshotName,
			Application: appName,
			ManualPromotion: appstudiosharedv1.ManualPromotionConfiguration{
				TargetEnvironment: targetEnvironment,
			},
		},
	}
	return promotionRun
}
