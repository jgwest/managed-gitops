package eventloop

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// workspaceResourceEventLoop is responsible for handling workspace-scoped events, like events for RepositoryCredentials resources.
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
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage) *workspaceResourceEventLoop {

	workspaceResourceEventLoop := &workspaceResourceEventLoop{
		inputChannel: make(chan workspaceResourceLoopMessage),
	}

	go internalWorkspaceResourceEventLoop(workspaceResourceEventLoop.inputChannel, sharedResourceLoop, workspaceEventLoopInputChannel)

	return workspaceResourceEventLoop
}

func internalWorkspaceResourceEventLoop(inputChan chan workspaceResourceLoopMessage,
	sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop,
	workspaceEventLoopInputChannel chan workspaceEventLoopMessage) {

	ctx := context.Background()
	log := log.FromContext(ctx).WithValues("component", "workspace_resource_event_loop")

	dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
	if err != nil {
		log.Error(err, "SEVERE: internalSharedResourceEventLoop exiting before startup")
		return
	}

	taskRetryLoop := sharedutil.NewTaskRetryLoop("workspace-resource-event-retry-loop")

	for {
		msg := <-inputChan

		var mapKey string

		if msg.messageType == workspaceResourceLoopMessageType_processRepositoryCredential {

			repoCred, ok := (msg.payload).(ctrl.Request)
			if !ok {
				log.Error(nil, "SEVERE: Unexpected payload type in workspace resource event loop")
				continue
			}

			mapKey = "repo-cred-" + repoCred.Namespace + "-" + repoCred.Name

		} else if msg.messageType == workspaceResourceLoopMessageType_processManagedEnvironment {

			evlMsg, ok := (msg.payload).(eventlooptypes.EventLoopMessage)
			if !ok {
				log.Error(nil, "SEVERE: Unexpected payload type in workspace resource event loop")
				continue
			}

			mapKey = "managed-env-" + evlMsg.Event.Request.Namespace + "-" + evlMsg.Event.Request.Name

		} else {
			log.Error(nil, "SEVERE: Unexpected message type: "+string(msg.messageType))
			continue
		}

		// TODO: GITOPSRVCE-68 - PERF - Use a more memory efficient key

		// Pass the event to the retry loop, for processing
		task := &workspaceResourceEventTask{
			msg:                            msg,
			dbQueries:                      dbQueries,
			log:                            log,
			sharedResourceLoop:             sharedResourceLoop,
			workspaceEventLoopInputChannel: workspaceEventLoopInputChannel,
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
}

// Returns true if the task should be retried, false otherwise, plus an error
func (wert *workspaceResourceEventTask) PerformTask(taskContext context.Context) (bool, error) {

	retry, err := internalProcessWorkspaceResourceMessage(taskContext, wert.msg, wert.sharedResourceLoop, wert.workspaceEventLoopInputChannel, wert.dbQueries, wert.log)

	return retry, err
}

// Returns true if the task should be retried, false otherwise, plus an error
func internalProcessWorkspaceResourceMessage(ctx context.Context, msg workspaceResourceLoopMessage,
	sharedResourceLoop *shared_resource_loop.SharedResourceEventLoop, workspaceEventLoopInputChannel chan workspaceEventLoopMessage,
	dbQueries db.DatabaseQueries, log logr.Logger) (bool, error) {

	const retry_true, noRetry_false = true, false

	log.V(sharedutil.LogLevel_Debug).Info("processWorkspaceResource received message: " + string(msg.messageType))

	if msg.apiNamespaceClient == nil {
		return noRetry_false, fmt.Errorf("invalid namespace client")
	}

	if msg.messageType == workspaceResourceLoopMessageType_processRepositoryCredential {

		req, ok := (msg.payload).(ctrl.Request)
		if !ok {
			return noRetry_false, fmt.Errorf("invalid payload in processWorkspaceResourceMessage")
		}

		// Retrieve the namespace that the repository credential is contained within
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Namespace,
				Namespace: req.Namespace,
			},
		}
		if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

			if !apierr.IsNotFound(err) {
				return retry_true, fmt.Errorf("unexpected error in retrieving repo credentials: %v", err)
			}

			log.V(sharedutil.LogLevel_Warn).Info("Received a message for a repository credential in a namepace that doesn't exist", "namespace", namespace)
			return noRetry_false, nil
		}

		// Request that the shared resource loop handle the GitOpsDeploymentRepositoryCredential resource:
		// - If the GitOpsDeploymentRepositoryCredential doesn't exist, delete the corsponding database table
		// - If the GitOpsDeploymentRepositoryCredential does exist, but not in the DB, then create a RepositoryCredential DB entry
		// - If the GitOpsDeploymentRepositoryCredential does exist, and also in the DB, then compare and change a RepositoryCredential DB entry
		// Then, in all 3 cases, create an Operation to update the cluster-agent
		_, err := sharedResourceLoop.ReconcileRepositoryCredential(ctx, msg.apiNamespaceClient, *namespace, req.Name, shared_resource_loop.DefaultK8sClientFactory{})

		if err != nil {
			return retry_true, fmt.Errorf("unable to reconcile repository credential")
		}

		return noRetry_false, nil

	} else if msg.messageType == workspaceResourceLoopMessageType_processManagedEnvironment {

		evlMessage, ok := (msg.payload).(eventlooptypes.EventLoopMessage)
		if !ok {
			return noRetry_false, fmt.Errorf("invalid payload in processWorkspaceResourceMessage")
		}
		req := evlMessage.Event.Request

		// Retrieve the namespace that the managed environment is contained within
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Namespace,
				Namespace: req.Namespace,
			},
		}
		if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

			if !apierr.IsNotFound(err) {
				return retry_true, fmt.Errorf("unexpected error in retrieving namespace of managed env CR: %v", err)
			}

			log.V(sharedutil.LogLevel_Warn).Info("Received a message for a managed end CR in a namepace that doesn't exist", "namespace", namespace)
			return noRetry_false, nil
		}

		// Ask the shared resource loop to ensure the managed environment is reconciled
		_, err := sharedResourceLoop.ReconcileSharedManagedEnv(ctx, msg.apiNamespaceClient, *namespace, req.Name, req.Namespace, false, shared_resource_loop.DefaultK8sClientFactory{})
		if err != nil {
			return retry_true, fmt.Errorf("unable to reconcile shared managed env: %v", err)
		}

		// Once we finish processing the managed environment, send it back to the workspace event loop, so it can be passed to GitOpsDeployments.
		// - Send it on another go routine to keep from blocking this one
		go func() {
			workspaceEventLoopInputChannel <- workspaceEventLoopMessage{
				messageType: managedEnvProcessed_Event,
				payload:     evlMessage,
			}
		}()

		return noRetry_false, nil

	} else {
		return noRetry_false, fmt.Errorf("SEVERE: unrecognized sharedResourceLoopMessageType: %s " + string(msg.messageType))
	}

}
