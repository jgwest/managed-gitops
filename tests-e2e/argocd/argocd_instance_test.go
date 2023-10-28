package argocd

import (
	"context"
	"time"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/gitops-engine/pkg/health"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	argocdv1 "github.com/redhat-appstudio/managed-gitops/cluster-agent/utils"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture"
	appFixture "github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/application"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var _ = Describe("Standalone ArgoCD instance E2E tests", func() {

	const (
		argocdNamespace      = fixture.NewArgoCDInstanceNamespace
		argocdCRName         = "argocd"
		destinationNamespace = fixture.NewArgoCDInstanceDestNamespace
	)

	Context("Create a Standalone ArgoCD instance", func() {

		BeforeEach(func() {

			By("Delete old namespaces, and kube-system resources")
			Expect(fixture.EnsureCleanSlate()).To(Succeed())

			By("deleting the namespace before the test starts, so that the code can create it")
			config, err := fixture.GetSystemKubeConfig()
			if err != nil {
				panic(err)
			}
			err = fixture.DeleteNamespace(argocdNamespace, config)
			Expect(err).ToNot(HaveOccurred())

		})

		testStandaloneArgoCD := func(testUpdate bool) {

			fixture.SkipIfArgoCDOperandRequired()

			By("creating ArgoCD resource")
			ctx := context.Background()
			log := log.FromContext(ctx)

			config, err := fixture.GetSystemKubeConfig()
			Expect(err).ToNot(HaveOccurred())
			apiHost := config.Host

			k8sClient, err := fixture.GetKubeClient(config)
			Expect(err).ToNot(HaveOccurred())

			err = argocdv1.ReconcileNamespaceScopedArgoCD(ctx, argocdCRName, argocdNamespace, k8sClient, log)
			Expect(err).ToNot(HaveOccurred())

			By("ensuring ArgoCD service resource exists")
			argocdInstance := &apps.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: argocdCRName + "-server", Namespace: argocdNamespace},
			}

			Eventually(argocdInstance, "60s", "5s").Should(k8s.ExistByName(k8sClient))
			Expect(err).ToNot(HaveOccurred())

			By("ensuring ArgoCD resource exists in kube-system namespace")
			err = argocdv1.SetupArgoCD(ctx, apiHost, argocdNamespace, k8sClient, log)
			Expect(err).ToNot(HaveOccurred())

			if testUpdate {
				By("ensuring the SetupArgoCD function works even if resources already exist in kube-system namespace")
				err = argocdv1.SetupArgoCD(ctx, apiHost, argocdNamespace, k8sClient, log)
				Expect(err).ToNot(HaveOccurred())
			}

			destinationNamespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fixture.NewArgoCDInstanceDestNamespace,
				},
			}

			if fixture.EnableNamespaceBackedArgoCD {
				destinationNamespace.Labels = map[string]string{
					"argocd.argoproj.io/managed-by": argocdNamespace,
				}
			}

			err = k8sClient.Create(ctx, destinationNamespace)
			Expect(err).ToNot(HaveOccurred())

			By("creating ArgoCD application")
			app := appv1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "argo-app-6",
					Namespace: argocdNamespace,
				},
				Spec: appv1.ApplicationSpec{
					Source: &appv1.ApplicationSource{
						RepoURL:        "https://github.com/redhat-appstudio/managed-gitops",
						Path:           "resources/test-data/sample-gitops-repository/environments/overlays/dev",
						TargetRevision: "HEAD",
					},
					Destination: appv1.ApplicationDestination{
						Name:      argocdv1.ClusterSecretName,
						Namespace: destinationNamespace.Name,
					},
				},
			}

			err = k8s.Create(&app, k8sClient)
			Expect(err).ToNot(HaveOccurred())

			cs := argocdv1.NewCredentialService(nil, true)
			Expect(cs).ToNot(BeNil())

			By("calling AppSync and waiting for it to return with no error")
			Eventually(func() bool {
				GinkgoWriter.Println("Attempting to sync application: ", app.Name)
				err := argocdv1.AppSync(context.Background(), app.Name, "", app.Namespace, k8sClient, cs, true)
				GinkgoWriter.Println("- AppSync result: ", err)
				return err == nil
			}).WithTimeout(time.Minute * 4).WithPolling(time.Second * 1).Should(BeTrue())

			Eventually(app, "2m", "1s").Should(
				SatisfyAll(
					appFixture.HaveSyncStatusCode(appv1.SyncStatusCodeSynced),
					appFixture.HaveHealthStatusCode(health.HealthStatusHealthy),
				))
		}

		It("should create ArgoCD resource and application, wait for it to be installed and synced", func() {
			testStandaloneArgoCD(false)
		})

		It("should update existing ArgoCD resources, create an application, wait for it to be installed and synced", func() {
			testStandaloneArgoCD(true)
		})
	})
})
