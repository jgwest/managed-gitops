package eventlooptypes

import (
	"context"
	"fmt"

	gitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type EventLoopEventType string

const (
	DeploymentModified           EventLoopEventType = "DeploymentModified"
	RepositoryCredentialModified EventLoopEventType = "RepositoryCredentialModified"
	ManagedEnvironmentModified   EventLoopEventType = "ManagedEnvironmentModified"
	SyncRunModified              EventLoopEventType = "SyncRunModified"
	UpdateDeploymentStatusTick   EventLoopEventType = "UpdateDeploymentStatusTick"
)

const KubeSystemNamespace = "kube-system"

// EventLoopEvent tracks an event received from the controllers in the apis/managed-gitops/v1alpha1 package.
// For example, when a GitOpsDeployment is created/modified/deleted, an EventLoopEvent is created and
// is then processed by the event loops.
type EventLoopEvent struct {

	// EventType indicates the type of event, usually the modification of a resource
	EventType EventLoopEventType

	// Request from the event context
	Request ctrl.Request

	// Client from the event context
	Client client.Client

	// ReqResource indicates whether the event is for a GitOpsDeployment, or DeploymentSyncRun (or other resources)
	ReqResource GitOpsResourceType

	// WorkspaceID is the UID of the namespace that contains the request
	WorkspaceID string
}

// GetReqResourceAsClientObject converts the resource into a simple client.Object: it will be of
// the expected type (GitOpsDeployment/SyncRun/etc), but only contain the name and namespace.
func (ele *EventLoopEvent) GetReqResourceAsSimpleClientObject() (client.Object, error) {

	var resource client.Object

	if ele.ReqResource == GitOpsDeploymentTypeName {
		resource = &gitopsv1alpha1.GitOpsDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ele.Request.Name,
				Namespace: ele.Request.Namespace,
			},
		}
	} else if ele.ReqResource == GitOpsDeploymentSyncRunTypeName {
		resource = &gitopsv1alpha1.GitOpsDeploymentSyncRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ele.Request.Name,
				Namespace: ele.Request.Namespace,
			},
		}
	} else if ele.ReqResource == GitOpsDeploymentRepositoryCredentialTypeName {
		resource = &gitopsv1alpha1.GitOpsDeploymentRepositoryCredential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ele.Request.Name,
				Namespace: ele.Request.Namespace,
			},
		}
	} else {
		return nil, fmt.Errorf("SEVERE - unexpected request resource type: %v", string(ele.ReqResource))
	}

	return resource, nil
}

// Packages an EventLoopEvent as a message between event loop channels.
// - The type of messages depends on the MessageType
type EventLoopMessage struct {
	MessageType EventLoopMessageType
	Event       *EventLoopEvent

	// ShutdownSignalled is included as part of workComplete message, to indicate that the goroutine has succesfully shut down.
	ShutdownSignalled bool
}

type EventLoopMessageType int

const (
	// ApplicationEventLoopMessageType_WorkComplete indicates the message indicates that a particular task has completed.
	// For example:
	ApplicationEventLoopMessageType_WorkComplete EventLoopMessageType = iota

	// ApplicationEventLoopMessageType_Event indicates that the message contains an event
	ApplicationEventLoopMessageType_Event
)

// eventlooptypes.StringEventLoopEvent is a utility function for debug purposes.
func StringEventLoopEvent(obj *EventLoopEvent) string {
	if obj == nil {
		return "(nil)"
	}

	return fmt.Sprintf("[%s] %s/%s/%s, for workspace '%s'", obj.EventType, obj.Request.Namespace,
		obj.Request.Name, string(obj.ReqResource), obj.WorkspaceID)

}

type GitOpsResourceType string

const (
	GitOpsDeploymentTypeName                     GitOpsResourceType = "GitOpsDeployment"
	GitOpsDeploymentSyncRunTypeName              GitOpsResourceType = "GitOpsDeploymentSyncRun"
	GitOpsDeploymentRepositoryCredentialTypeName GitOpsResourceType = "GitOpsDeploymentRepositoryCredential"
	GitOpsDeploymentManagedEnvironmentTypeName   GitOpsResourceType = "GitOpsDeploymentManagedEnvironmentTypeName"
)

func GetWorkspaceIDFromNamespaceID(namespace corev1.Namespace) string {
	// Here we assume that the namespace UID is the same as the workspace UID. If/when that changes, this should be updated.
	return string(namespace.UID)
}

// GetK8sClientForGitOpsEngineInstance returns a client for accessing resources from a GitOpsEngine cluster based on the environment.
// KCP environment: return a client that targets a workload cluster where the GitOpsEngine instance is deployed.
// non-KCP environment: return a normal client that targets the same cluster as backend.
func GetK8sClientForGitOpsEngineInstance(ctx context.Context, gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {

	// TODO: GITOPSRVCE-73: When we support multiple Argo CD instances (and multiple instances on separate clusters), this logic should be updated.
	serviceClient, err := GetK8sClientForServiceWorkspace()
	if err != nil {
		return nil, err
	}

	if !sharedutil.IsRunningAgainstKCP() {
		return serviceClient, nil
	}

	return getGitOpsEngineWorkloadClient(ctx, serviceClient, gitopsEngineInstance)
}

func getGitOpsEngineWorkloadClient(ctx context.Context, serviceClient client.Client, gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {

	if !sharedutil.IsRunningAgainstKCP() {
		return nil, fmt.Errorf("use a service provider client in a non-KCP environment")
	}

	// In a KCP environment, Argo CD will be installed on a workload cluster. We need to retrieve the credentials required to connect to the Argo CD instance, which is stored in the secret.
	clusterCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gitops-engine-cluster",
			Namespace: "gitops",
		},
	}
	err := serviceClient.Get(ctx, client.ObjectKeyFromObject(clusterCreds), clusterCreds)
	if err != nil {
		return nil, fmt.Errorf("unable to find cluster credentials for GitOpsEngine cluster: %v", err)
	}

	host, ok := clusterCreds.Data["host"]
	if !ok {
		return nil, fmt.Errorf("missing host API URL in the GitOpsEngine cluster secret")
	}
	bearerToken, ok := clusterCreds.Data["bearer_token"]
	if !ok {
		return nil, fmt.Errorf("missing bearer token in the GitOpsEngine cluster secret")
	}

	// create a new client with the host and bearer token of the GitOpsEngine cluster.
	config := &rest.Config{
		Host:        string(host),
		BearerToken: string(bearerToken),
		TLSClientConfig: rest.TLSClientConfig{
			Insecure:   true,
			ServerName: "",
		},
	}

	scheme := runtime.NewScheme()
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = gitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	return client.New(config, client.Options{Scheme: scheme})
}

// GetK8sClientForServiceWorkspace returns a client for service provider workspace
func GetK8sClientForServiceWorkspace() (client.Client, error) {
	config, err := sharedutil.GetRESTConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	err = gitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	// TODO: Places like this, where we are calling client.New, should also be wrapped with a ChaosClient
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	k8sClient = sharedutil.IfEnabledSimulateUnreliableClient(k8sClient)
	if err != nil {
		return nil, err
	}

	return k8sClient, nil

}
