package eventloop

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/application_event_loop"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// workspaceResourceEventLoop is responsible for handling events for API-namespaced-scoped resources, like events for RepositoryCredentials resources.
// An api-namespace-scoped resources is a resource that can be used by (and reference from) multiple GitOpsDeployments at the same time.
//
// For example, a ManagedEnvironment could be referenced by 2 separate GitOpsDeployments in a namespace.
//
// NOTE: workspaceResourceEventLoop should only be called from workspace_event_loop.go.
type workspaceResourceEventLoop struct {
	inputChannel chan workspaceResourceLoopMessage
}

type workspaceResourceLoopMessage struct {
	apiNamespaceClient client.Client

	messageType workspaceResourceLoopMessageType

	// optional payload
	payload any
}

type workspaceResourceLoopMessageType string

const (
	workspaceResourceLoopMessageType_processRepositoryCredential workspaceResourceLoopMessageType = "processRepositoryCredential"
	workspaceResourceLoopMessageType_processManagedEnvironment   workspaceResourceLoopMessageType = "processManagedEnvironment"

	retry   = true
	noRetry = false
)

func (werl *workspaceResourceEventLoop) processRepositoryCredential(ctx context.Context, req ctrl.Request, apiNamespaceClient client.Client) {

	msg := workspaceResourceLoopMessage{
		apiNamespaceClient: apiNamespaceClient,
		messageType:        workspaceResourceLoopMessageType_processRepositoryCredential,
		payload:            req,
	}

	werl.inputChannel <- msg

	// This function is async: we don't wait for a return value from the loop.
}

func (werl *workspaceResourceEventLoop) processManagedEnvironment(ctx context.Context, eventLoopMessage eventlooptypes.EventLoopMessage,
	apiNamespaceClient client.Client) {

	msg := workspaceResourceLoopMessage{
		apiNamespaceClient: apiNamespaceClient,
		messageType:        workspaceResourceLoopMessageType_processManagedEnvironment,
		payload:            eventLoopMessage,
	}

	werl.inputChannel <- msg

	// This function is async: we don't wait for a return value from the loop.
}

func newWorkspaceResourceLoop(sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop,
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage, namespaceName string,
	namespaceUID string) *workspaceResourceEventLoop {

	workspaceResourceEventLoop := &workspaceResourceEventLoop{
		inputChannel: make(chan workspaceResourceLoopMessage),
	}

	go internalWorkspaceResourceEventLoop(workspaceResourceEventLoop.inputChannel, sharedResourceLoop, workspaceEventLoopInputChannel, shared_resource_loop.DefaultK8sClientFactory{}, namespaceName, namespaceUID)

	return workspaceResourceEventLoop
}

func newWorkspaceResourceLoopWithFactory(sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop,
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage, k8sClientFactory shared_resource_loop.SRLK8sClientFactory, namespaceName string, namespaceUID string) *workspaceResourceEventLoop {

	workspaceResourceEventLoop := &workspaceResourceEventLoop{
		inputChannel: make(chan workspaceResourceLoopMessage),
	}

	go internalWorkspaceResourceEventLoop(workspaceResourceEventLoop.inputChannel, sharedResourceLoop, workspaceEventLoopInputChannel, k8sClientFactory, namespaceName, namespaceUID)

	return workspaceResourceEventLoop
}

func internalWorkspaceResourceEventLoop(inputChan chan workspaceResourceLoopMessage,
	sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop,
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage, k8sClientFactory shared_resource_loop.SRLK8sClientFactory, namespaceName string, namespaceUID string) {

	ctx := context.Background()
	l := log.FromContext(ctx).
		WithName(logutil.LogLogger_managed_gitops).
		WithValues(logutil.Log_Component, logutil.Log_Component_Backend_WorkspaceResourceEventLoop)

	dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
	if err != nil {
		l.Error(err, "SEVERE: internalSharedResourceEventLoop exiting before startup")
		return
	}

	taskRetryLoop := sharedutil.NewTaskRetryLoop("workspace-resource-event-retry-loop" + namespaceName + "-" + namespaceUID)

	for {
		msg := <-inputChan

		var mapKey string

		if msg.messageType == workspaceResourceLoopMessageType_processRepositoryCredential {

			repoCred, ok := (msg.payload).(ctrl.Request)
			if !ok {
				l.Error(nil, "SEVERE: Unexpected payload type in workspace resource event loop")
				continue
			}

			mapKey = "repo-cred-" + repoCred.Namespace + "-" + repoCred.Name

		} else if msg.messageType == workspaceResourceLoopMessageType_processManagedEnvironment {

			evlMsg, ok := (msg.payload).(eventlooptypes.EventLoopMessage)
			if !ok {
				l.Error(nil, "SEVERE: Unexpected payload type in workspace resource event loop")
				continue
			}

			mapKey = "managed-env-" + evlMsg.Event.Request.Namespace + "-" + evlMsg.Event.Request.Name

		} else {
			l.Error(nil, "SEVERE: Unexpected message type: "+string(msg.messageType))
			continue
		}

		// TODO: GITOPSRVCE-68 - PERF - Use a more memory efficient key

		// Pass the event to the retry loop, for processing
		task := &workspaceResourceEventTask{
			msg:                            msg,
			dbQueries:                      dbQueries,
			log:                            l,
			sharedResourceLoop:             sharedResourceLoop,
			workspaceEventLoopInputChannel: workspaceEventLoopInputChannel,
			k8sClientFactory:               k8sClientFactory,
		}

		taskRetryLoop.AddTaskIfNotPresent(mapKey, task, sharedutil.ExponentialBackoff{Factor: 2, Min: time.Millisecond * 200, Max: time.Second * 10, Jitter: true})
	}
}

type workspaceResourceEventTask struct {
	msg                            workspaceResourceLoopMessage
	dbQueries                      db.DatabaseQueries
	log                            logr.Logger
	sharedResourceLoop             *shared_resource_loop.SharedResourceEventLoop
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage
	k8sClientFactory               shared_resource_loop.SRLK8sClientFactory
}

// Returns true if the task should be retried, false otherwise, plus an error
func (wert *workspaceResourceEventTask) PerformTask(taskContext context.Context) (bool, error) {

	retry, err := internalProcessWorkspaceResourceMessage(taskContext, wert.msg, wert.sharedResourceLoop, wert.workspaceEventLoopInputChannel, wert.dbQueries, wert.k8sClientFactory, wert.log)

	// If we recognize this error is a connection error due to the user providing us invalid credentials, don't bother to log it.
	if application_event_loop.IsManagedEnvironmentConnectionUserError(err, wert.log) {
		wert.log.Info(fmt.Sprintf("user cluster credentials for URL are invalid: %v", err))
		// PerformTask only uses the error for logging, so we log here then supress the return value
		return retry, nil
	}

	if err != nil {
		wert.log.Error(err, "unable to process workspace resource message")
	}

	return retry, nil // error return is only used for logging: we don't need to log in the calling function, because we are logging above
}

// Returns true if the task should be retried, false otherwise, plus an error
func internalProcessWorkspaceResourceMessage(ctx context.Context, msg workspaceResourceLoopMessage,
	sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop, workspaceEventLoopInputChannel chan workspaceEventLoopMessage,
	dbQueries db.DatabaseQueries, k8sClientFactory shared_resource_loop.SRLK8sClientFactory, log logr.Logger) (bool, error) {

	log.V(logutil.LogLevel_Debug).Info("processWorkspaceResource received message: " + string(msg.messageType))

	if msg.apiNamespaceClient == nil {
		return noRetry, fmt.Errorf("invalid namespace client")
	}

	// When the event is related to 'GitOpsDeploymentRepositoryCredential' resource, we need to process the event
	if msg.messageType == workspaceResourceLoopMessageType_processRepositoryCredential {
		shouldRetry, err := handleResourceLoopRepositoryCredential(ctx, msg, sharedResourceLoop, k8sClientFactory, log)
		if err != nil {
			return shouldRetry, fmt.Errorf("failed to process workspace resource message: %v", err)
		}

		return noRetry, nil
	} else if msg.messageType == workspaceResourceLoopMessageType_processManagedEnvironment {

		shouldRetry, err := handleResourceLoopManagedEnvironment(ctx, msg, sharedResourceLoop, k8sClientFactory, workspaceEventLoopInputChannel, log)
		if err != nil {
			return shouldRetry, fmt.Errorf("failed to process workspace resource message: %v", err)
		}

		return noRetry, nil

	}
	return noRetry, fmt.Errorf("SEVERE: unrecognized sharedResourceLoopMessageType: %s", msg.messageType)

}

func handleResourceLoopRepositoryCredential(ctx context.Context, msg workspaceResourceLoopMessage, sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop, k8sClientFactory shared_resource_loop.SRLK8sClientFactory, log logr.Logger) (bool, error) {

	req, ok := (msg.payload).(ctrl.Request)
	if !ok {
		return noRetry, fmt.Errorf("invalid RepositoryCredential payload in processWorkspaceResourceMessage")
	}

	// Retrieve the namespace that the repository credential is contained within
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Namespace,
		},
	}
	if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

		if !apierr.IsNotFound(err) {
			return retry, fmt.Errorf("unexpected error in retrieving repo credentials: %v", err)
		}

		log.V(logutil.LogLevel_Warn).Info("Received a message for a repository credential in a namepace that doesn't exist")
		return noRetry, nil
	}

	// Request that the shared resource loop handle the GitOpsDeploymentRepositoryCredential resource:
	// - If the GitOpsDeploymentRepositoryCredential doesn't exist, delete the corresponding database table
	// - If the GitOpsDeploymentRepositoryCredential does exist, but not in the DB, then create a RepositoryCredential DB entry
	// - If the GitOpsDeploymentRepositoryCredential does exist, and also in the DB, then compare and change a RepositoryCredential DB entry
	// Then, in all 3 cases, create an Operation to update the cluster-agent
	_, err := sharedResourceLoop.ReconcileRepositoryCredential(ctx, msg.apiNamespaceClient, *namespace, req.Name, k8sClientFactory, log)

	if err != nil {
		return retry, fmt.Errorf("unable to reconcile repository credential. Error: %v", err)
	}

	return noRetry, nil
}

func handleResourceLoopManagedEnvironment(ctx context.Context, msg workspaceResourceLoopMessage, sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop, k8sClientFactory shared_resource_loop.SRLK8sClientFactory, workspaceEventLoopInputChannel chan workspaceEventLoopMessage, log logr.Logger) (bool, error) {

	evlMessage, ok := (msg.payload).(eventlooptypes.EventLoopMessage)
	if !ok {
		return noRetry, fmt.Errorf("invalid ManagedEnvironment payload in processWorkspaceResourceMessage")
	}

	if evlMessage.Event == nil { // Sanity test the message
		log.Error(nil, "SEVERE: process managed env event is nil")
	}

	req := evlMessage.Event.Request

	// Retrieve the namespace that the managed environment is contained within
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Namespace,
		},
	}
	if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

		if !apierr.IsNotFound(err) {
			return retry, fmt.Errorf("unexpected error in retrieving namespace of managed env CR: %v", err)
		}

		log.V(logutil.LogLevel_Warn).Info("Received a message for a managed env CR in a namespace that doesn't exist")
		return noRetry, nil
	}

	// Ask the shared resource loop to ensure the managed environment is reconciled
	_, isUserErr, err := sharedResourceLoop.ReconcileSharedManagedEnv(ctx, msg.apiNamespaceClient, *namespace, req.Name, req.Namespace,
		false, k8sClientFactory, log)

	if err != nil {

		if isUserErr {
			log.Info("user error: user specified invalid managed environment parameters", "error", err)
			// A user error indicates that the user specified invalid parameters, for example, they specified a Secret that doesn't exist.
			// We do not need to retry in this case, as the user needs to make a change before their resource is valid.
			return noRetry, nil
		} else {
			return retry, fmt.Errorf("unable to reconcile shared managed env: %v", err)
		}

	}

	// Once we finish processing the managed environment, send it back to the workspace event loop, so it can be passed to GitOpsDeployments.
	// - Send it on another go routine to keep from blocking this one
	go func() {
		workspaceEventLoopInputChannel <- workspaceEventLoopMessage{
			messageType: workspaceEventLoopMessageType_managedEnvProcessed_Event,
			payload:     evlMessage,
		}
	}()

	return noRetry, nil
}
