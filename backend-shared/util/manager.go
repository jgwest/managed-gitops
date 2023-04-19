package util

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	gitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

const (
	// RunningAgainstKCPEnv is an env variable that is set when GitOps service is running against a KCP environment
	RunningAgainstKCPEnv = "GITOPS_IN_KCP"
)

func IsKCPVirtualWorkspaceDisabled() bool {
	return strings.EqualFold(os.Getenv("DISABLE_KCP_VIRTUAL_WORKSPACE"), "true")
}

func IsRunningAgainstKCP() bool {
	return strings.EqualFold(os.Getenv("GITOPS_IN_KCP"), "true")
}

// GetControllerManager returns a manager for running controllers
func GetControllerManager(ctx context.Context, cfg *rest.Config, log *logr.Logger, apiExportName string, opts ctrl.Options) (ctrl.Manager, error) {

	scheme := runtime.NewScheme()

	apiExportClient, err := client.New(cfg, client.Options{Scheme: scheme})
	apiExportClient = IfEnabledSimulateUnreliableClient(apiExportClient)
	if err != nil {
		return nil, fmt.Errorf("error creating APIExport client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}

	return getControllerManager(ctx, cfg, apiExportClient, discoveryClient, log, apiExportName, opts)
}

// getControllerManager returns a standard controller-runtime manager in a non-KCP environment and a KCP workspace aware manager in the presence of KCP APIs
func getControllerManager(ctx context.Context, restConfig *rest.Config, apiExportClient client.Client,
	discoveryClient discovery.DiscoveryInterface, log *logr.Logger, apiExportName string,
	opts ctrl.Options) (ctrl.Manager, error) {

	var mgr ctrl.Manager

	isKCPVirtualWorkspaceEnvironment, err := kcpAPIsGroupPresent(discoveryClient)
	if err != nil {
		return nil, err
	}

	// If the service is running on KCP, BUT this environment variable is set, then
	// disable virtual workspace support.
	if IsKCPVirtualWorkspaceDisabled() {
		isKCPVirtualWorkspaceEnvironment = false
	}

	if isKCPVirtualWorkspaceEnvironment {

		var cfg *rest.Config

		// Try up to 120 seconds for the virtual workspace URL to become available.
		err := wait.PollImmediate(time.Second, time.Second*120, func() (bool, error) {
			var err error
			log.Info("Looking up virtual workspace URL")
			cfg, err = restConfigForAPIExport(ctx, restConfig, apiExportClient, apiExportName)
			if err != nil {
				fmt.Printf("error looking up virtual workspace URL: '%v', retrying in 1 second.\n", err)
				return false, nil
			}
			return true, nil
		})

		if err != nil {
			return nil, fmt.Errorf("virtual workspace URL never became available")
		}

		log.Info("Using virtual workspace URL", "url", cfg.Host)

		// opts.LeaderElectionConfig = restConfig
		// mgr, err = kcp.NewClusterAwareManager(cfg, opts)
		// if err != nil {
		// 	return nil, fmt.Errorf("unable to start cluster aware manager: %w", err)
		// }

	} else {
		log.Info("The apis.kcp.dev group is not present - creating standard manager")
		mgr, err = ctrl.NewManager(restConfig, opts)
		if err != nil {
			return nil, fmt.Errorf("unable to start manager: %w", err)
		}
	}

	return mgr, nil
}

// restConfigForAPIExport returns a *rest.Config properly configured to communicate with the endpoint for the
// APIExport's virtual workspace.
func restConfigForAPIExport(ctx context.Context, cfg *rest.Config, apiExportClient client.Client, apiExportName string) (*rest.Config, error) {

	// var apiExport apisv1alpha1.APIExport

	// if apiExportName != "" {
	// 	if err := apiExportClient.Get(ctx, types.NamespacedName{Name: apiExportName}, &apiExport); err != nil {
	// 		return nil, fmt.Errorf("error getting APIExport %q: %w", apiExportName, err)
	// 	}
	// } else {
	// 	exports := &apisv1alpha1.APIExportList{}
	// 	if err := apiExportClient.List(ctx, exports); err != nil {
	// 		return nil, fmt.Errorf("error listing APIExports: %w", err)
	// 	}
	// 	if len(exports.Items) == 0 {
	// 		return nil, fmt.Errorf("no APIExport found")
	// 	}
	// 	if len(exports.Items) > 1 {
	// 		return nil, fmt.Errorf("more than one APIExport found")
	// 	}
	// 	apiExport = exports.Items[0]
	// }

	// if len(apiExport.Status.VirtualWorkspaces) < 1 {
	// 	return nil, fmt.Errorf("APIExport %q status.virtualWorkspaces is empty", apiExportName)
	// }

	cfg = rest.CopyConfig(cfg)
	// // TODO: GITOPSRVCE-204 - implement sharding of virtual workspaces
	// cfg.Host = apiExport.Status.VirtualWorkspaces[0].URL

	return cfg, nil
}

func kcpAPIsGroupPresent(discoveryClient discovery.DiscoveryInterface) (bool, error) {
	// apiGroupList, err := discoveryClient.ServerGroups()
	// if err != nil {
	// 	return false, fmt.Errorf("failed to get server groups: %w", err)
	// }

	// for _, group := range apiGroupList.Groups {
	// 	// if group.Name == apisv1alpha1.SchemeGroupVersion.Group {
	// 	// 	for _, version := range group.Versions {
	// 	// 		if version.Version == apisv1alpha1.SchemeGroupVersion.Version {
	// 	// 			return true, nil
	// 	// 		}
	// 	// 	}
	// 	// }
	// }
	return false, nil
}

// NewVirtualWorkspaceClient returns a client that can access a workspace pointed by KUBECONFIG via Virtual Workspace
func NewVirtualWorkspaceClient(apiExportName string) (client.Client, error) {
	restConfig, err := GetRESTConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	if err := registerSchemes(scheme); err != nil {
		return nil, fmt.Errorf("failed to register scheme: %v", err)
	}

	apiExportClient, err := newClusterAwareClient(restConfig, scheme, false)
	if err != nil {
		return nil, fmt.Errorf("error creating APIExport client: %w", err)
	}

	// Try up to 120 seconds for the virtual workspace URL to become available.
	err = wait.PollImmediate(time.Second, time.Second*120, func() (bool, error) {
		var err error
		restConfig, err = restConfigForAPIExport(context.Background(), restConfig, apiExportClient, apiExportName)
		if err != nil {
			fmt.Printf("error looking up virtual workspace URL: '%v', retrying in 1 second.\n", err)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return nil, fmt.Errorf("virtual workspace URL never became available")
	}

	return newClusterAwareClient(restConfig, scheme, true)
}

func newClusterAwareClient(restConfig *rest.Config, scheme *runtime.Scheme, withMapper bool) (client.Client, error) {
	// httpClient, err := kcp.ClusterAwareHTTPClient(restConfig)
	// if err != nil {
	// 	fmt.Printf("error creating HTTP client: %v\n", err)
	// 	return nil, err
	// }

	// var restMapper meta.RESTMapper
	// if withMapper {
	// 	restMapper, err = kcp.NewClusterAwareMapperProvider(restConfig)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("error creating Cluster Aware Mapper: %v", err)
	// 	}
	// }

	return client.New(restConfig, client.Options{
		Scheme: scheme,
		// HTTPClient: httpClient,
		// Mapper:     restMapper,
	})
}

func registerSchemes(scheme *runtime.Scheme) error {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add client go to scheme: %v", err)
	}

	// if err := tenancyv1alpha1.AddToScheme(scheme); err != nil {
	// 	return fmt.Errorf("failed to add %s to scheme: %v", tenancyv1alpha1.SchemeGroupVersion, err)
	// }

	// if err := datav1alpha1.AddToScheme(scheme); err != nil {
	// 	return fmt.Errorf("failed to add %s to scheme: %v", datav1alpha1.GroupVersion, err)
	// }

	// if err := apisv1alpha1.AddToScheme(scheme); err != nil {
	// 	return fmt.Errorf("failed to add %s to scheme: %v", apisv1alpha1.SchemeGroupVersion, err)
	// }

	if err := gitopsv1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("failed to add %s to scheme: %v", gitopsv1alpha1.GroupVersion, err)
	}

	return nil
}

// AddKCPClusterToContext injects the cluster name into the context when running in a KCP environment with virtual workspaces enabled
func AddKCPClusterToContext(ctx context.Context, clusterName string) context.Context {
	// if clusterName != "" && !IsKCPVirtualWorkspaceDisabled() {
	// 	ctx = logicalcluster.WithCluster(ctx, logicalcluster.New(clusterName))
	// }
	return ctx
}

// RemoveKCPClusterFromContext removes the cluster name from the context when running in a KCP environment with virtual workspaces enabled
func RemoveKCPClusterFromContext(ctx context.Context) context.Context {
	if !IsKCPVirtualWorkspaceDisabled() {
		// clusterName, exists := logicalcluster.ClusterFromContext(ctx)
		// if exists {
		// 	ctx = context.WithValue(ctx, logicalcluster.New(clusterName.String()), nil)
		// }
	}
	return ctx
}
