package util

import (
	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("ControllerManager test", func() {

	// Context("GetControllerManager should return the appropriate manager for KCP/non-KCP envs", func() {

	// 	ctx := context.Background()
	// 	log := log.FromContext(ctx)
	// 	apiExportName := "test-kcp"
	// 	var fakeClient client.Client
	// 	var fakeDiscovery *fakediscovery.FakeDiscovery
	// 	var fakeServer *httptest.Server
	// 	var cfg *rest.Config
	// 	var opts ctrl.Options

	// 	BeforeEach(func() {

	// 		fakeClient = fake.NewClientBuilder().WithScheme(scheme).Build()

	// 		fakeDiscovery = &fakediscovery.FakeDiscovery{
	// 			Fake: &testing.Fake{
	// 				Resources: []*metav1.APIResourceList{
	// 					{
	// 						GroupVersion: "v1",
	// 					},
	// 				},
	// 			},
	// 		}

	// 		fakeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
	// 			var obj interface{}
	// 			switch req.URL.Path {
	// 			case "/api":
	// 				obj = &metav1.APIVersions{
	// 					Versions: []string{
	// 						"v1",
	// 					},
	// 				}
	// 			default:
	// 				w.WriteHeader(http.StatusNotFound)
	// 				return
	// 			}
	// 			output, err := json.Marshal(obj)
	// 			if err != nil {
	// 				GinkgoT().Fatalf("unexpected encoding error: %v", err)
	// 				return
	// 			}
	// 			w.Header().Set("Content-Type", "application/json")
	// 			w.WriteHeader(http.StatusOK)
	// 			_, err = w.Write(output)
	// 			if err != nil {
	// 				http.Error(w, err.Error(), http.StatusInternalServerError)
	// 			}
	// 		}))

	// 		cfg = &rest.Config{
	// 			Host:     fakeServer.URL,
	// 			Username: "fake",
	// 			Password: "fake",
	// 		}

	// 		opts = ctrl.Options{
	// 			MetricsBindAddress: "0",
	// 		}
	// 	})

	// 	AfterEach(func() {
	// 		fakeServer.Close()
	// 	})

	// 	When("KCP API groups are absent", func() {
	// 		It("Should return a standard manager", func() {
	// 			gotMgr, err := getControllerManager(ctx, cfg, fakeClient, fakeDiscovery, &log, apiExportName, opts)
	// 			Expect(err).To(BeNil())

	// 			expectedMgr, err := ctrl.NewManager(cfg, opts)
	// 			Expect(err).To(BeNil())
	// 			Expect(gotMgr.GetConfig()).Should(Equal(expectedMgr.GetConfig()))
	// 		})
	// 	})

	// 	When("KCP API groups are present", func() {
	// 		It("Should return a manager that is KCP aware", func() {
	// 			// apiExport := apisv1alpha1.APIExport{
	// 			// 	ObjectMeta: metav1.ObjectMeta{
	// 			// 		Name: "test-kcp",
	// 			// 	},
	// 			// 	Spec: apisv1alpha1.APIExportSpec{},
	// 			// 	Status: apisv1alpha1.APIExportStatus{
	// 			// 		VirtualWorkspaces: []apisv1alpha1.VirtualWorkspace{
	// 			// 			{
	// 			// 				URL: fakeServer.URL,
	// 			// 			},
	// 			// 		},
	// 			// 	},
	// 			// }

	// 			// err := fakeClient.Create(ctx, &apiExport)
	// 			// Expect(err).To(BeNil())

	// 			fakeDiscovery = &fakediscovery.FakeDiscovery{
	// 				Fake: &testing.Fake{
	// 					Resources: []*metav1.APIResourceList{
	// 						{
	// 							GroupVersion: "apis.kcp.dev/v1alpha1",
	// 						},
	// 					},
	// 				},
	// 			}

	// 			// gotMgr, err := getControllerManager(ctx, cfg, fakeClient, fakeDiscovery, &log, apiExportName, opts)
	// 			// Expect(err).To(BeNil())

	// 			// expectedMgr, err := kcp.NewClusterAwareManager(cfg, opts)
	// 			// Expect(err).To(BeNil())
	// 			// Expect(gotMgr.GetConfig()).Should(Equal(expectedMgr.GetConfig()))

	// 			// err = fakeClient.Delete(ctx, &apiExport)
	// 			// Expect(err).To(BeNil())
	// 		})
	// 	})
	// })
})
