package eventloop

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
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

// workspaceResourceEventLoop is responsible for handling events for API-namespaced-scoped resources, like events for RepositoryCredentials resources.
// An api-namespace-scoped resources are resources that can be used by (and reference from) multiple GitOpsDeployments.
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
	const retry, noRetry = true, false

	log.V(sharedutil.LogLevel_Debug).Info("processWorkspaceResource received message: " + string(msg.messageType))

	if msg.apiNamespaceClient == nil {
		return noRetry, fmt.Errorf("invalid namespace client")
	}

	// When the event is related to 'GitOpsDeploymentRepositoryCredential' resource, we need to process the event
	if msg.messageType == workspaceResourceLoopMessageType_processRepositoryCredential {
		req, ok := (msg.payload).(ctrl.Request)
		if !ok {
			return false, fmt.Errorf("invalid payload in processWorkspaceResourceMessage")
		}

		ctx = sharedutil.AddKCPClusterToContext(ctx, req.ClusterName)

		// Retrieve the namespace that the repository credential is contained within
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Namespace,
				Namespace: req.Namespace,
			},
		}
		if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

			if !apierr.IsNotFound(err) {
				return true, fmt.Errorf("unexpected error in retrieving repo credentials: %v", err)
			}

			log.V(sharedutil.LogLevel_Warn).Info("Received a message for a repository credential in a namespace that doesn't exist", "namespace", namespace)
			return false, nil
		}

		repoCreds := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{}

		if err := msg.apiNamespaceClient.Get(ctx, req.NamespacedName, repoCreds); err != nil {

			if !apierr.IsNotFound(err) {
				return true, fmt.Errorf("unexpected error in retrieving repo credentials: %v", err)
			}

			// The repository credentials necessarily don't exist

			// Find any existing database resources for repository credentials that previously existing with this namespace/name
			apiCRToDBMappingList := []db.APICRToDatabaseMapping{}
			if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
				req.Name, req.Namespace, string(namespace.GetUID()), db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential, &apiCRToDBMappingList); err != nil {

				return true, fmt.Errorf("unable to list APICRs for repository credentials: %v", err)
			}

			for _, item := range apiCRToDBMappingList {

				fmt.Println("STUB:", item)

				// TODO: GITOPSRVCE-96: STUB: Next steps:
				// - Delete the repository credential from the database
				// - Create the operation row, pointing to the deleted repository credentials table
				// - Delete the APICRToDatabaseMapping referenced by 'item'

			}

			// TODO: GITOPSRVCE-96:  STUB - reconcile on the missing repository credentials

			return false, fmt.Errorf("STUB: reconcile on the missing repository credentials")

		}

		// TODO: GITOPSRVCE-96: STUB: If it exists, compare it with what's in the database
		// - If it doesn't exist in the database, create it
		// - If it does exist in the database, but the values are different, update it

		return false, fmt.Errorf("STUB: not yet implemented")
	} else if msg.messageType == workspaceResourceLoopMessageType_processManagedEnvironment {

		evlMessage, ok := (msg.payload).(eventlooptypes.EventLoopMessage)
		if !ok {
			return noRetry, fmt.Errorf("invalid payload in processWorkspaceResourceMessage")
		}
		req := evlMessage.Event.Request

		ctx = sharedutil.AddKCPClusterToContext(ctx, req.ClusterName)

		// Retrieve the namespace that the managed environment is contained within
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Namespace,
				Namespace: req.Namespace,
			},
		}
		if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

			if !apierr.IsNotFound(err) {
				return retry, fmt.Errorf("unexpected error in retrieving namespace of managed env CR: %v", err)
			}

			log.V(sharedutil.LogLevel_Warn).Info("Received a message for a managed end CR in a namespace that doesn't exist", "namespace", namespace)
			return noRetry, nil
		}

		// Ask the shared resource loop to ensure the managed environment is reconciled
		_, err := sharedResourceLoop.ReconcileSharedManagedEnv(ctx, msg.apiNamespaceClient, *namespace, req.Name, req.Namespace,
			false, shared_resource_loop.DefaultK8sClientFactory{}, log)
		if err != nil {
			return retry, fmt.Errorf("unable to reconcile shared managed env: %v", err)
		}

		// Once we finish processing the managed environment, send it back to the workspace event loop, so it can be passed to GitOpsDeployments.
		// - Send it on another go routine to keep from blocking this one
		go func() {
			workspaceEventLoopInputChannel <- workspaceEventLoopMessage{
				messageType: managedEnvProcessed_Event,
				payload:     evlMessage,
			}
		}()

		return noRetry, nil

	} else {
		return noRetry, fmt.Errorf("SEVERE: unrecognized sharedResourceLoopMessageType: %s " + string(msg.messageType))
	}

	return false, nil
}

func getDBCred(ctx context.Context, dbQueries db.DatabaseQueries, apiCRToDBMapping db.APICRToDatabaseMapping, log logr.Logger) (*db.RepositoryCredentials, bool, error) {
	const retry, noRetry = true, false
	dbCred, err := dbQueries.GetRepositoryCredentialsByID(ctx, apiCRToDBMapping.APIResourceUID)

	if err != nil {
		if db.IsResultNotFoundError(err) {
			log.V(sharedutil.LogLevel_Warn).Info("Received a message for a repository credential database entry that doesn't exist", "error", err, "ID", apiCRToDBMapping.APIResourceUID)

			return nil, noRetry, err
		} else {
			// Looks like random glitch in the database, so we should retry

			return nil, retry, fmt.Errorf("unexpected error in retrieving repo credentials with ID '%v' from the database: %v", err, apiCRToDBMapping.APIResourceUID)
		}
	}

	log.V(sharedutil.LogLevel_Warn).Info("The repository credential database entry has been received", "ID", dbCred.RepositoryCredentialsID)

	return &dbCred, noRetry, nil
}

func deleteAPICRToDatabaseMapping(ctx context.Context, apiCRToDBMappingList []db.APICRToDatabaseMapping, dbQueries db.DatabaseQueries, log logr.Logger) {
	if len(apiCRToDBMappingList) > 0 {
		for _, item := range apiCRToDBMappingList {
			rowsDeleted, err := dbQueries.DeleteAPICRToDatabaseMapping(ctx, &item)
			if err != nil {
				// Warn, but continue.
				log.V(sharedutil.LogLevel_Warn).Info("Unable to delete APICRToDatabaseMapping", "item", item.APIResourceUID)
			}
			if rowsDeleted != 1 {
				// Warn, but continue.
				log.V(sharedutil.LogLevel_Warn).Info("unexpected number of rows deleted for APICRToDatabaseMapping", "mapping", item.APIResourceUID)
			}

		}
	} else {
		log.V(sharedutil.LogLevel_Debug).Info("No APICRToDatabaseMapping found for the deleted GitOpsDeploymentRepositoryCredential CR")
	}

	log.V(sharedutil.LogLevel_Debug).Info("APICRToDatabaseMapping deleted successfully")
}

func deleteRepoCredFromDB(ctx context.Context, dbQueries db.DatabaseQueries, apiCRToDBMapping db.APICRToDatabaseMapping, log logr.Logger) (bool, error) {
	const retry, noRetry = true, false
	rowsDeleted, err := dbQueries.DeleteRepositoryCredentialsByID(ctx, apiCRToDBMapping.APIResourceUID)

	if err != nil {
		// Log the error and retry
		log.V(sharedutil.LogLevel_Debug).Info("Error deleting Repository Credential from the database", "error", err)
		return retry, err
	}

	if rowsDeleted == 0 {
		// Log the error, but continue to delete the other Repository Credentials (this looks morel like a bug in our code)
		log.V(sharedutil.LogLevel_Warn).Error(nil, "No rows deleted from the database", "rowsDeleted", rowsDeleted, "repocred id", apiCRToDBMapping.APIResourceUID)
		return noRetry, err
	}

	// meaning: err == nil && rowsDeleted > 0
	log.V(sharedutil.LogLevel_Debug).Info("Deleted Repository Credential from the database", "repocred id", apiCRToDBMapping.APIResourceUID)
	return noRetry, nil
}

func getCR(ctx context.Context, msg workspaceResourceLoopMessage, req ctrl.Request, log logr.Logger) (*managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential, bool, error) {
	const retry, noRetry = true, false
	repoCreds := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{}

	if err := msg.apiNamespaceClient.Get(ctx, req.NamespacedName, repoCreds); err != nil {

		if !apierr.IsNotFound(err) {
			// We don't know what happened, so we'll retry
			return nil, retry, fmt.Errorf("unexpected error in retrieving repo credentials CR: %v", err)
		}

		// The CR was deleted, (Note: this is not an error)
		log.V(sharedutil.LogLevel_Warn).Info("Received a message for a repository credential CR that doesn't exist", "CR", repoCreds)
		return nil, noRetry, nil
	}

	// The CR exists
	return repoCreds, noRetry, nil
}

func getNamespace(ctx context.Context, msg workspaceResourceLoopMessage, req ctrl.Request, log logr.Logger) (*corev1.Namespace, bool, error) {
	const retry, noRetry = true, false
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Namespace,
			Namespace: req.Namespace,
		},
	}

	if err := msg.apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {

		if !apierr.IsNotFound(err) {
			// We don't know if the namespace exists or not, so we need to retry
			return namespace, retry, fmt.Errorf("unexpected error in retrieving repo credentials namespace: %v", err)
		}

		// Namespace doesn't exist (Note: this is not an error)
		log.V(sharedutil.LogLevel_Warn).Info("Received a message for a repository credential in a namespace that doesn't exist", "namespace", namespace)
		return namespace, noRetry, nil
	}

	// The namespace exists
	return namespace, noRetry, nil
}
