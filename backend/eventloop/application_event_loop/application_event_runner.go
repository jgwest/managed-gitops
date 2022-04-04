package application_event_loop

import (
	"context"
	"fmt"
	"time"

	"github.com/redhat-appstudio/managed-gitops/backend/condition"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"

	"github.com/go-logr/logr"
	operation "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend/apis/managed-gitops/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Within the application event runner  is where the vast majority of the work takes place. This is the code that is
// actually responsible for acting on events. It is here that received events are processed, and the corresponding Argo
// CD Applications, Cluster Secrets, etc, are created.

// The event handlers are implemented in a similar style to Reconcile() methods of controllers. Just like Reconcile() methods
// of controllers, the event runners:
// - Receive a resource change event that originally originated from a K8s watch (the resource group/version/kind, name,
//	 and namespace)
// - Retrieve the corresponding resource from the namespace, based on that event
// - Reconcile that resource with the overall state of the system

// The difference between controller Reconcile() functions, vs what we are doing here, is that we are ultimately reconciling
// against a database, rather than the K8s workspace:
// - A traditional controller will receive K8s resource change events for a namespace, and (for example) update other K8s
//   resources in that namespace.
// - In contrast, our backend service will receive K8s resource change events for a namespace, and then update the resources
//   in the RDBMS database.

// For every event that is received, this is roughly what happens:
// - Looks at the event that was received. The event will contain:
//     - the type of the resource that changed: Either GitOpsDeployment or GitOpsDeploymentSyncRun (as of this writing, Feb 2022).
//     - name of the resource: for example, 'my-gitops-deployment'
//     - the namespace that contains the resource, which corresponds to the user: for example, a 'jane' namespace, for a user with user ID 'jane'.
//     - the uid of the associated GitOpsDeployment resource (from the ‘associatedGitopsDeplUID’ field).
// - Finds the correspondings K8s resource (GitOpsDeployment/GitOpsDeploymentSyncRun) of the event
// - Compares that resource contents with what's in the database
// - Updates (creates/modifies/deletes) the corresponding database entries,
//     - If the resource doesn't exist in the database, but does exist in the namespace, then create it in the database
//     - If the resource DOES exist in the database, but doesn't exist in the namespace, then delete it from the database
//     - If the resource exists in both the namespace, AND the database, then see if there are any changes required (for example, the user changed one of the fields of the resource, which means we need to update the database.)
//     - If the database was updated, inform the cluster-agent component of the changes, by creating an Operation, so that Argo CD can be updated (as described in Operation section, elsewhere).
// The backend then waits for the Operation to be marked as completed (that means the cluster-agent has finished running it).

// Cardinality: 2 instances of the Application Event Runner exist per GitOpsDeployment CR:
// - 1 ApplicationEventRunner responsible for handling events related to the GitOpsDeployment CR
// - 1 ApplicationEventRunner responsible for handling events related to the GitOpsDeploymentSyncRun CR

// For more information on how events are distributed between goroutines by event loop, see:
// https://miro.com/app/board/o9J_lgiqJAs=/?moveToWidget=3458764514216218600&cot=14

func startNewApplicationEventLoopRunner(informWorkCompleteChan chan eventlooptypes.EventLoopMessage,
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop,
	gitopsDeplUID string, workspaceID string, debugContext string) chan *eventlooptypes.EventLoopEvent {

	inputChannel := make(chan *eventlooptypes.EventLoopEvent)

	go func() {
		applicationEventLoopRunner(inputChannel, informWorkCompleteChan, sharedResourceEventLoop, gitopsDeplUID,
			workspaceID, debugContext)
	}()

	return inputChannel

}

func applicationEventLoopRunner(inputChannel chan *eventlooptypes.EventLoopEvent, informWorkCompleteChan chan eventlooptypes.EventLoopMessage,
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop, gitopsDeplUID string, workspaceID string, debugContext string) {

	outerContext := context.Background()
	log := log.FromContext(outerContext)

	log = log.WithValues("gitopsDeplUID", gitopsDeplUID, "workspaceID", workspaceID, "debugContext", debugContext)

	log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner started")

	signalledShutdown := false

	for {
		// Read from input channel: wait for an event on this application
		newEvent := <-inputChannel

		ctx, cancel := context.WithCancel(outerContext)

		defer cancel()

		// Process the event

		log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner - event received", "event", eventlooptypes.StringEventLoopEvent(newEvent))

		attempts := 1
		backoff := sharedutil.ExponentialBackoff{Min: time.Duration(100 * time.Millisecond), Max: time.Duration(15 * time.Second), Factor: 2, Jitter: true}
	inner_for:
		for {

			log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner - processing event", "event", eventlooptypes.StringEventLoopEvent(newEvent), "attempt", attempts)

			// Break if the request is cancelled, or the timeout expires
			select {
			case <-ctx.Done():
				break inner_for
			default:
			}

			_, err := sharedutil.CatchPanic(func() error {

				action := applicationEventLoopRunner_Action{
					getK8sClientForGitOpsEngineInstance: actionGetK8sClientForGitOpsEngineInstance,
					eventResourceName:                   newEvent.Request.Name,
					eventResourceNamespace:              newEvent.Request.Namespace,
					workspaceClient:                     newEvent.Client,
					sharedResourceEventLoop:             sharedResourceEventLoop,
					log:                                 log,
					workspaceID:                         workspaceID,
				}

				var err error

				dbQueriesUnscoped, err := db.NewProductionPostgresDBQueries(false)
				if err != nil {
					return fmt.Errorf("unable to access database in workspaceEventLoopRunner: %v", err)
				}

				scopedDBQueries, ok := dbQueriesUnscoped.(db.ApplicationScopedQueries)
				if !ok {
					return fmt.Errorf("SEVERE: unexpected cast failure")
				}

				if newEvent.EventType == eventlooptypes.DeploymentModified {
					// Handle all GitOpsDeployment related events
					signalledShutdown, _, _, err = action.applicationEventRunner_handleDeploymentModified(ctx, scopedDBQueries)

					// Get the GitOpsDeployment object from k8s, so we can update it if necessary
					gitopsDepl, clientError := getMatchingGitOpsDeployment(ctx, newEvent.Request.Name, newEvent.Request.Namespace, newEvent.Client)
					if clientError != nil {
						return fmt.Errorf("couldn't fetch the GitOpsDeployment instance: %v", clientError)
					}

					// Create a gitOpsDeploymentAdapter to plug any conditions
					conditionManager := condition.NewConditionManager()
					adapter := newGitOpsDeploymentAdapter(gitopsDepl, log, newEvent.Client, conditionManager, ctx)

					// Plug any conditions based on the "err" msg
					if setConditionError := adapter.setGitOpsDeploymentCondition(managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred, managedgitopsv1alpha1.GitopsDeploymentReasonErrorOccurred, err); setConditionError != nil {
						return setConditionError
					}

				} else if newEvent.EventType == eventlooptypes.SyncRunModified {
					// Handle all SyncRun related events
					signalledShutdown, err = action.applicationEventRunner_handleSyncRunModified(ctx, scopedDBQueries)

				} else if newEvent.EventType == eventlooptypes.UpdateDeploymentStatusTick {
					err = action.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, newEvent.AssociatedGitopsDeplUID, scopedDBQueries)

				} else {
					log.Error(nil, "SEVERE: Unrecognized event type", "event type", newEvent.EventType)
				}

				// TODO: GITOPSRVCE-85: Implement detection of workspace/api proxy delete, here, and handle cleanup

				return err

			})

			if err == nil {
				break inner_for
			} else {
				log.Error(err, "error from inner event handler in applicationEventLoopRunner", "event", eventlooptypes.StringEventLoopEvent(newEvent))
				backoff.DelayOnFail(ctx)
				attempts++
			}
		}

		// Inform the caller that we have completed a single unit of work
		informWorkCompleteChan <- eventlooptypes.EventLoopMessage{MessageType: eventlooptypes.ApplicationEventLoopMessageType_WorkComplete, Event: newEvent, ShutdownSignalled: signalledShutdown}

		// If the event processing logic concluded that the goroutine should shutdown, then break out of the outer for loop.
		// This is usually because the API CR no longer exists.
		if signalledShutdown {
			break
		}
	}

	log.Info("ApplicationEventLoopRunner goroutine terminated.", "signalledShutdown", signalledShutdown)
}

// Don't call this directly: call it via workspaceEventLoopRunner_Action
func actionGetK8sClientForGitOpsEngineInstance(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {

	// TODO: GITOPSRVCE-73: When we support multiple Argo CD instances (and multiple instances on separate clusters), this logic should be updated.

	config, err := sharedutil.GetRESTConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	err = managedgitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = operation.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return k8sClient, nil

}

// applicationEventLoopRunner_Action is a short-lived struct containing data required to perform an action
// on the database, and/or on gitops engine cluster.
type applicationEventLoopRunner_Action struct {

	// logger to use
	log logr.Logger

	// The Name field of the K8s object (for example, a GitOpsDeploymentRun with a name of 'my-app-deployment')
	eventResourceName string

	// The namespace (for example, a GitOpsDeploymentRun in namespace 'my-deployments')
	eventResourceNamespace string

	// The K8s client that can be used to read/write objects on the workspace cluster
	workspaceClient client.Client

	// The UID of the API namespace (namespace containing GitOps API types)
	workspaceID string

	// getK8sClientForGitOpsEngineInstance returns the K8s client that corresponds to the gitops engine instance.
	// As of this writing, only one Argo CD instance is supported, so this is trivial, but should have
	// more complex logic in the future.
	getK8sClientForGitOpsEngineInstance func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error)

	// sharedResourceEventLoop can be used to invoke the shared resource event loop, in order to
	// create or retrieve shared database resources.
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop

	// testOnlySkipCreateOperation: for unit testing purposes only, skip creation of an operation when
	// processing the action. This allows us to unit test application event functions without needing to
	// have a cluster-agent running alongside it.
	testOnlySkipCreateOperation bool
}

// cleanupOperation cleans up the database entry and (optionally) the CR, once an operation has concluded.
func cleanupOperation(ctx context.Context, dbOperation db.Operation, k8sOperation operation.Operation, operationNamespace string,
	dbQueries db.ApplicationScopedQueries, gitopsEngineClient client.Client, log logr.Logger) error {

	log = log.WithValues("operation", dbOperation.Operation_id, "namespace", operationNamespace)

	// // Delete the database entry
	// rowsDeleted, err := dbQueries.DeleteOperationById(ctx, dbOperation.Operation_id)
	// if err != nil {
	// 	return err
	// }
	// if rowsDeleted != 1 {
	// 	log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected number of operation rows deleted", "operation-id", dbOperation.Operation_id, "rows", rowsDeleted)
	// }

	log.V(sharedutil.LogLevel_Debug).Info("Deleting operation CR: " + k8sOperation.Name)

	// Optional: Delete the Operation CR
	if err := gitopsEngineClient.Delete(ctx, &k8sOperation); err != nil {
		if !apierr.IsNotFound(err) {
			// Log the error, but don't return it: it's the responsibility of the cluster agent to delete the operation cr
			log.Error(err, "Unable to delete operation")
		}
	}

	return nil

}

func createOperation(ctx context.Context, waitForOperation bool, dbOperationParam db.Operation, clusterUserID string,
	operationNamespace string, dbQueries db.ApplicationScopedQueries, gitopsEngineClient client.Client, log logr.Logger) (*operation.Operation, *db.Operation, error) {

	var err error
	dbOperation := db.Operation{
		Instance_id:             dbOperationParam.Instance_id,
		Resource_id:             dbOperationParam.Resource_id,
		Resource_type:           dbOperationParam.Resource_type,
		Operation_owner_user_id: clusterUserID,
		Created_on:              time.Now(),
		Last_state_update:       time.Now(),
		State:                   db.OperationState_Waiting,
		Human_readable_state:    "",
	}

	log.Info("Creating database operation", "operation", dbOperation.ShortString())

	if err := dbQueries.CreateOperation(ctx, &dbOperation, clusterUserID); err != nil {
		log.Error(err, "unable to create operation", "operation", dbOperation.LongString())
		return nil, nil, err
	} else {
		log.Info("Operation Created with ID: " + dbOperation.Operation_id)
	}

	// Create K8s operation
	operation := operation.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "operation-" + dbOperation.Operation_id,
			Namespace: operationNamespace,
		},
		Spec: operation.OperationSpec{
			OperationID: dbOperation.Operation_id,
		},
	}

	log.Info("Creating K8s Operation CR", "operation", fmt.Sprintf("%v", operation.Spec.OperationID))

	if err := gitopsEngineClient.Create(ctx, &operation, &client.CreateOptions{}); err != nil {
		log.Error(err, "unable to create K8s Operation in namespace", "operation", dbOperation.Operation_id, "namespace", operation.Namespace)
		return nil, nil, err
	}

	// Wait for operation to complete.

	if waitForOperation {
		log.Info("Waiting for Operation to complete", "operation", fmt.Sprintf("%v", operation.Spec.OperationID))

		if err = waitForOperationToComplete(ctx, &dbOperation, dbQueries, log); err != nil {
			log.Error(err, "operation did not complete", "operation", dbOperation.Operation_id, "namespace", operation.Namespace)
			return nil, nil, err
		}

		log.Info("Operation completed", "operation", fmt.Sprintf("%v", operation.Spec.OperationID))
	}

	return &operation, &dbOperation, nil

}

// waitForOperationToComplete waits for an Operation database entry to have 'Completed' or 'Failed' status.
//
func waitForOperationToComplete(ctx context.Context, dbOperation *db.Operation, dbQueries db.ApplicationScopedQueries, log logr.Logger) error {

	backoff := sharedutil.ExponentialBackoff{Factor: 2, Min: time.Duration(100 * time.Millisecond), Max: time.Duration(10 * time.Second), Jitter: true}

	for {

		err := dbQueries.GetOperationById(ctx, dbOperation)
		if err != nil {
			// Either the operation couldn't be found (which shouldn't happen here), or some other issue, so return it
			return err
		}

		if err == nil && (dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed) {
			break
		}

		backoff.DelayOnFail(ctx)

		// Break if the request is cancelled, or the timeout expires
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation context is Done() in waitForOperationToComplete")
		default:
		}

	}

	return nil
}
