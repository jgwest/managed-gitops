package application_event_loop

import (
	"context"
	"fmt"
	"time"

	"github.com/redhat-appstudio/managed-gitops/backend/condition"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"

	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop, gitopsDeplUID string, namespaceID string, debugContext string) {

	outerContext := context.Background()
	log := log.FromContext(outerContext)

	log = log.WithValues("gitopsDeplUID", gitopsDeplUID, "namespaceID", namespaceID, "debugContext", debugContext)

	log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner started")

	signalledShutdown := false

	// The name of the resource (GitOpsDeployment/SyncRun) being handled by this runner.
	var expectedResourceName string

	for {
		// Read from input channel: wait for an event on this application
		newEvent := <-inputChannel

		ctx, cancel := context.WithCancel(outerContext)

		defer cancel()

		// Process the event

		log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner - event received", "event", eventlooptypes.StringEventLoopEvent(newEvent))

		// Keep attempting the process the event until no error is returned, or the request is cancelled.
		attempts := 1
		backoff := sharedutil.ExponentialBackoff{Min: time.Duration(100 * time.Millisecond), Max: time.Duration(15 * time.Second), Factor: 2, Jitter: true}
	inner_for:
		for {

			log.V(sharedutil.LogLevel_Debug).Info("applicationEventLoopRunner - processing event", "event", eventlooptypes.StringEventLoopEvent(newEvent), "attempt", attempts)

			// Break if the context is cancelled
			select {
			case <-ctx.Done():
				break inner_for
			default:
			}

			_, err := sharedutil.CatchPanic(func() error {

				action := applicationEventLoopRunner_Action{
					getK8sClientForGitOpsEngineInstance: eventlooptypes.GetK8sClientForGitOpsEngineInstance,
					eventResourceName:                   newEvent.Request.Name,
					eventResourceNamespace:              newEvent.Request.Namespace,
					workspaceClient:                     newEvent.Client,
					sharedResourceEventLoop:             sharedResourceEventLoop,
					log:                                 log,
					workspaceID:                         namespaceID,
					k8sClientFactory:                    shared_resource_loop.DefaultK8sClientFactory{},
				}

				var err error

				dbQueriesUnscoped, err := db.NewSharedProductionPostgresDBQueries(false)
				if err != nil {
					return fmt.Errorf("unable to access database in workspaceEventLoopRunner: %v", err)
				}
				defer dbQueriesUnscoped.CloseDatabase()

				scopedDBQueries, ok := dbQueriesUnscoped.(db.ApplicationScopedQueries)
				if !ok {
					return fmt.Errorf("SEVERE: unexpected cast failure")
				}

				if newEvent.EventType == eventlooptypes.DeploymentModified {

					// Keep track of the resource name the first time we see it.
					if expectedResourceName == "" && newEvent.Request.Name != "" {
						expectedResourceName = newEvent.Request.Name
					}
					if expectedResourceName != "" && newEvent.Request.Name != expectedResourceName {
						return fmt.Errorf("SEVERE: Application event runner received. Expected: " + expectedResourceName + ", actual: " + newEvent.Request.Name)
					}

					signalledShutdown, err = handleDeploymentModified(ctx, newEvent, action, scopedDBQueries, log)

				} else if newEvent.EventType == eventlooptypes.SyncRunModified {
					// Handle all SyncRun related events
					signalledShutdown, err = action.applicationEventRunner_handleSyncRunModified(ctx, scopedDBQueries)

				} else if newEvent.EventType == eventlooptypes.UpdateDeploymentStatusTick {
					err = action.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, newEvent.AssociatedGitopsDeplUID, scopedDBQueries)

				} else if newEvent.EventType == eventlooptypes.ManagedEnvironmentModified {

					if expectedResourceName == "" {
						// If this runner hasn't processed a GitOpsDeployment yet, then no work is required for the managed environment.
						return nil
					}
					signalledShutdown, err = handleManagedEnvironmentModified(ctx, expectedResourceName, newEvent, action, dbQueriesUnscoped, log)

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

// handleManagedEnvironmentModified_shouldInformGitOpsDeployment returns true if the GitOpsDeployment CR references
// the ManagedEnvironment resource that changed, false otherwise.
func handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx context.Context, gitopsDeployment managedgitopsv1alpha1.GitOpsDeployment,
	managedEnvEvent *eventlooptypes.EventLoopEvent, dbQueries db.DatabaseQueries) (bool, error) {

	informGitOpsDeployment := false // whether or not this gitopsdeployment references the managed environment that changed

	// 1) If the GitOpsDeployment CR points to the managed environment of the event we are processing, then flag the
	// GitOpsDeployment as needed to be reconclied
	if gitopsDeployment.Spec.Destination.Environment == managedEnvEvent.Request.Name {
		informGitOpsDeployment = true
	}

	// 2) If the above check wasn't true, look at the database to see if any Application rows are pointing to the managed
	// environment that changed. If so, flag as needing to be reconciled.
	if !informGitOpsDeployment {

		// 2a) Retrieve the Namespace containing the ManagedEnvironment CR
		gitopsDeplNamespace := corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      managedEnvEvent.Request.Namespace,
				Namespace: managedEnvEvent.Request.Namespace,
			},
		}
		if err := managedEnvEvent.Client.Get(ctx, client.ObjectKeyFromObject(&gitopsDeplNamespace), &gitopsDeplNamespace); err != nil {
			if apierr.IsNotFound(err) {
				// Namespace doesn't exist, so no work to do.
				return false, nil
			} else {
				return false, fmt.Errorf("unable to retrieve namespace '%s': %v", gitopsDeplNamespace.Name, err)
			}
		}

		// 2b) Retrieve all the managed environments in the database that have had name of the req resource, in this namespace.
		managedEnvironmentDBIDs := []string{}
		{
			apiCRs := []db.APICRToDatabaseMapping{}

			// convert managed env cr to database
			if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx,
				db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
				managedEnvEvent.Request.Name, managedEnvEvent.Request.Namespace, string(gitopsDeplNamespace.UID),
				db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment, &apiCRs); err != nil {
				return false, fmt.Errorf("unable to retrieve API CRs for managed env '%s': %v", managedEnvEvent.Request.Name, err)
			}

			for _, apiCR := range apiCRs {
				managedEnvironmentDBIDs = append(managedEnvironmentDBIDs, apiCR.DBRelationKey)
			}
		}

		// 2c) Locate Applications that have been associated with the GitOpsDeployment being handled by this runner.
		// - For each of them, check if they matching any of managed environments from the previous step.
		if len(managedEnvironmentDBIDs) > 0 {
			dtams := []db.DeploymentToApplicationMapping{}

			if err := dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(ctx,
				gitopsDeployment.Name, managedEnvEvent.Request.Namespace, string(gitopsDeplNamespace.UID), &dtams); err != nil {
				return false, fmt.Errorf("unable to list DeploymentToApplicationMappings for namespace '%s': %v",
					managedEnvEvent.Request.Namespace, managedEnvEvent.Request.Name)
			}

			for _, deplToAppMapping := range dtams {

				appl := db.Application{
					Application_id: deplToAppMapping.Application_id,
				}

				if err := dbQueries.GetApplicationById(ctx, &appl); err != nil {
					if db.IsResultNotFoundError(err) {
						continue
					} else {
						return false, fmt.Errorf("unable to retrieve application '%s': %v", appl.Application_id, err)
					}
				}

				// If the Application row references the managed environment, then we need to reconcile the GitOpsDeployment
				match := false
				for _, managedEnvDBID := range managedEnvironmentDBIDs {
					if appl.Managed_environment_id == managedEnvDBID {
						match = true
						break
					}
				}
				if match {
					informGitOpsDeployment = true
					break
				}

			}
		}
	}

	return informGitOpsDeployment, nil

}

// handleManagedEnvironmentModified checks to see the GitOpsDeployment that is handled by this event runner is referencing the
// managed environment referenced in 'newEvent'. If the GitOpsDeployment DOES reference it, then we need to ensure that the
// GitOpsDeployment is reconciled, because it might need to respond to the ManagedEnvironment change.
//
// returns true if shutdown was signalled by 'handleDeploymentModified', false otherwise.
func handleManagedEnvironmentModified(ctx context.Context, expectedResourceName string, newEvent *eventlooptypes.EventLoopEvent,
	action applicationEventLoopRunner_Action, dbQueries db.DatabaseQueries, log logr.Logger) (bool, error) {

	// 1) Retrieve the GitOpsDeployment that the runner is handling events for
	gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      expectedResourceName,
			Namespace: newEvent.Request.Namespace,
		},
	}
	if err := newEvent.Client.Get(ctx, client.ObjectKeyFromObject(gitopsDeployment), gitopsDeployment); err != nil {
		if apierr.IsNotFound(err) {
			// gitopsdeployment doesn't exist; no work for us to do here.
			return false, nil
		} else {
			return false, fmt.Errorf("unable to retrieve gitopsdeployment: %v", err)
		}
	}

	informGitOpsDeployment, err := handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx, *gitopsDeployment,
		newEvent, dbQueries)
	if err != nil {
		return false, err
	}

	// If we discovered that this GitOpsDeployment CR or Application row is referencing this ManagedEnvironment,
	// then call handle deployment modified to ensure that the GitOpsDeployment is reconciled to the latest
	// contents of the managed environment.
	if informGitOpsDeployment {
		// Update the existing action, but change the 'eventResourceName' and 'eventResourceNamespace' to point
		// to the GitOpsDeployment.
		newAction := applicationEventLoopRunner_Action{
			getK8sClientForGitOpsEngineInstance: eventlooptypes.GetK8sClientForGitOpsEngineInstance,
			eventResourceName:                   gitopsDeployment.Name,
			eventResourceNamespace:              gitopsDeployment.Namespace,
			workspaceClient:                     action.workspaceClient,
			sharedResourceEventLoop:             action.sharedResourceEventLoop,
			log:                                 action.log,
			workspaceID:                         action.workspaceID,
			k8sClientFactory:                    shared_resource_loop.DefaultK8sClientFactory{},
		}

		signalledShutdown, err := handleDeploymentModified(ctx, newEvent, newAction, dbQueries, log)
		return signalledShutdown, err
	}

	return false, nil

}

// Handle events originating from the GitOpsDeployment controller
func handleDeploymentModified(ctx context.Context, newEvent *eventlooptypes.EventLoopEvent, action applicationEventLoopRunner_Action,
	scopedDBQueries db.ApplicationScopedQueries, log logr.Logger) (bool, error) {

	// Handle all GitOpsDeployment related events
	signalledShutdown, _, _, _, err := action.applicationEventRunner_handleDeploymentModified(ctx, scopedDBQueries)

	// Get the GitOpsDeployment object from k8s, so we can update it if necessary
	gitopsDepl, clientError := getMatchingGitOpsDeployment(ctx, newEvent.Request.Name, newEvent.Request.Namespace, newEvent.Client)
	if clientError != nil {
		if !apierr.IsNotFound(clientError) {
			return false, fmt.Errorf("couldn't fetch the GitOpsDeployment instance: %v", clientError)
		}

		// For IsNotFound error, no more we need to do, so return nil.
		return false, nil
	}

	// Create a gitOpsDeploymentAdapter to plug any conditions
	conditionManager := condition.NewConditionManager()
	adapter := newGitOpsDeploymentAdapter(gitopsDepl, log, newEvent.Client, conditionManager, ctx)

	// Plug any conditions based on the "err" msg
	if setConditionError := adapter.setGitOpsDeploymentCondition(managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred,
		managedgitopsv1alpha1.GitopsDeploymentReasonErrorOccurred, err); setConditionError != nil {
		return false, setConditionError
	}

	if err == nil {
		return signalledShutdown, nil
	} else {
		return signalledShutdown, err.DevError()
	}

}

// applicationEventLoopRunner_Action is a short-lived struct containing data required to perform an action
// on the database, and/or on gitops engine cluster.
type applicationEventLoopRunner_Action struct {

	// logger to use
	log logr.Logger

	// The Name field of the K8s object (for example, a GitOpsDeployment with a name of 'my-app-deployment')
	eventResourceName string

	// The namespace containing the API resource (for example, a GitOpsDeployment in namespace 'my-deployments')
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

	k8sClientFactory shared_resource_loop.SRLK8sClientFactory
}
