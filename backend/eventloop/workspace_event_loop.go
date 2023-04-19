package eventloop

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/application_event_loop"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// All events that occur within an individual API namespace will be received by an instance of a Workspace Event loop
// (running with a Goroutine).

// The responsibility of the workspace event loop is to:
// - Receive events for a API namespace workspace, from the controller event loop
// - Pass received events to the appropriate Application Event Loop, for the particular GitOpsDeployment/GitOpsDeploymentSync.
// - Ensure a new Application Event Loop goroutine is running for each active application (see below).
// - Look for orphaned resources that become un-orphaned. (eg A GitOpsDeploymentSyncRun is orphaned because it
//   pointed to a non-existent GitOpsDeployment, but then the GitOpsDeployment it referenced was created.)
// - If this happens, the newly un-orphaned resource is requeued, so that it can finally be processed.

// Cardinality: 1 instance of the Workspace Event Loop goroutine exists for each workspace that contains
// GitOps service API resources. (For example, if there are 10 workspaces, there will be 10 Workspace
// Event Loop instances/goroutines servicing those).

// Start a workspace event loop router go routine, which is responsible for handling API namespace events and
// then passing them to the controller loop.
func newWorkspaceEventLoopRouter(workspaceID string, vwsAPIExportName string) WorkspaceEventLoopRouterStruct {

	res := WorkspaceEventLoopRouterStruct{
		channel: make(chan workspaceEventLoopMessage),
	}

	internalStartWorkspaceEventLoopRouter(res.channel, workspaceID, defaultApplicationEventLoopFactory{
		vwsAPIExportName: vwsAPIExportName,
	})

	return res
}

func newWorkspaceEventLoopRouterWithFactory(workspaceID string, applEventLoopFactory applicationEventQueueLoopFactory) WorkspaceEventLoopRouterStruct {

	res := WorkspaceEventLoopRouterStruct{
		channel: make(chan workspaceEventLoopMessage),
	}

	internalStartWorkspaceEventLoopRouter(res.channel, workspaceID, applEventLoopFactory)

	return res
}

func (welrs *WorkspaceEventLoopRouterStruct) SendMessage(msg eventlooptypes.EventLoopMessage) {

	welrs.channel <- workspaceEventLoopMessage{
		messageType: workspaceEventLoopMessageType_Event,
		payload:     msg,
	}
}

type WorkspaceEventLoopRouterStruct struct {
	// channel chan eventlooptypes.EventLoopMessage
	channel chan workspaceEventLoopMessage
}

type workspaceEventLoopMessageType string

const (
	workspaceEventLoopMessageType_Event                     workspaceEventLoopMessageType = "event"
	workspaceEventLoopMessageType_managedEnvProcessed_Event workspaceEventLoopMessageType = "managedEnvProcessed"
	workspaceEventLoopMessageType_statusTicker              workspaceEventLoopMessageType = "statusTicker"
)

type workspaceEventLoopMessage struct {
	messageType workspaceEventLoopMessageType
	payload     any
}

// internalStartWorkspaceEventLoopRouter has the primary goal of catching panics from the workspaceEventLoopRouter, and
// recovering from them.
func internalStartWorkspaceEventLoopRouter(input chan workspaceEventLoopMessage, workspaceID string,
	applEventLoopFactory applicationEventQueueLoopFactory) {

	go func() {

		log := log.FromContext(context.Background())

		backoff := sharedutil.ExponentialBackoff{Min: time.Duration(500 * time.Millisecond), Max: time.Duration(15 * time.Second), Factor: 2, Jitter: true}

		lastFail := time.Now()

		for {
			isPanic, _ := sharedutil.CatchPanic(func() error {
				workspaceEventLoopRouter(input, workspaceID, applEventLoopFactory)
				return nil
			})

			// This really shouldn't happen, so we log it as severe.
			log.Error(nil, "SEVERE: the applicationEventLoopRouter function exited unexpectedly.", "isPanic", isPanic)

			// If the applicationEventLoopRouter somehow stops (for example, due to panic), we should restart it.
			// - if we don't restart it, we will miss any events that occur.
			//
			// However, we don't want to KEEP restarting it if it will just keep panicing over and over again,
			// so we use an expotenial backoff to ensure that failures do not consume all our CPU resources.

			// If the last failure was > 2 minutes ago, then reset the backoff back to Min.
			if time.Now().After(lastFail.Add(5 * time.Minute)) {
				backoff.Reset()
			}

			lastFail = time.Now()

			// Wait a small amount of time before we restart the application event loop
			backoff.DelayOnFail(context.Background())
		}
	}()

}

const (
	statusCheckInterval = time.Minute * 10
)

// startStatusCheckTimer ensures that every X minutes, send a status ticker message to the workspace event loop
func startStatusCheckTicker(interval time.Duration, input chan workspaceEventLoopMessage) *time.Ticker {
	ticker := time.NewTicker(interval)

	go func() {
		for {

			// Every X minutes, send a status ticker message to the workspace event loop
			<-ticker.C

			input <- workspaceEventLoopMessage{
				messageType: workspaceEventLoopMessageType_statusTicker,
				payload:     eventlooptypes.EventLoopMessage{},
			}
		}

	}()

	return ticker
}

// workspaceEventLoopInternalState is the internal state of the workspace event loop.
// - represents the internal state of the workspace event loop
// - thus variables inside this struct should only be called from within the workspace event loop.
type workspaceEventLoopInternalState struct {

	// input is the channel that the workspace event loop will read messages from
	input chan workspaceEventLoopMessage

	// orphanedResources:
	// - key: gitops depl name
	// - value: map: name field of orphaned syncrun CR -> last event that referenced it
	orphanedResources map[string]map[string]eventlooptypes.EventLoopEvent

	// applicationMap: maintains a list of which which GitOpsDeployment resources (1-1 relationship) are handled by which Go Routines
	// - key: string, of format "(namespace UID)-(namespace name)-(GitOpsDeployment name)"
	// - value:  channel for the go routine responsible for handling events for this GitOpsDeployment
	applicationMap map[string]workspaceEventLoop_applicationEventLoopEntry

	// workspaceResourceLoop is a reference to the workspace resource loop
	workspaceResourceLoop *workspaceResourceEventLoop

	// sharedResourceEventLoop is a reference to the shared resource loop
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop

	// namespaceID is the UID of the namespace that the workspace event loop is handling
	namespaceID string

	// log is the logger to use when logging inside the workspace event loop
	log logr.Logger

	// applEventLoopFactory is the factory function to use, to create the application event loop
	applEventLoopFactory applicationEventQueueLoopFactory
}

// workspaceEventLoopRouter receives all events for the namespace, and passes them to specific goroutine responsible
// for handling events for individual applications.
func workspaceEventLoopRouter(input chan workspaceEventLoopMessage, namespaceID string,
	applEventLoopFactory applicationEventQueueLoopFactory) {

	ctx := context.Background()

	log := log.FromContext(ctx).WithValues("namespaceID", namespaceID).WithName("workspace-event-loop")

	log.Info("workspaceEventLoopRouter started")
	defer log.Info("workspaceEventLoopRouter ended.")

	sharedResourceEventLoop := shared_resource_loop.NewSharedResourceLoop()

	state := workspaceEventLoopInternalState{
		sharedResourceEventLoop: sharedResourceEventLoop,
		orphanedResources:       map[string]map[string]eventlooptypes.EventLoopEvent{},
		applicationMap:          map[string]workspaceEventLoop_applicationEventLoopEntry{},
		applEventLoopFactory:    applEventLoopFactory,
		workspaceResourceLoop:   newWorkspaceResourceLoop(sharedResourceEventLoop, input),

		log:         log,
		input:       input,
		namespaceID: namespaceID,
	}

	statusTicker := startStatusCheckTicker(statusCheckInterval, input)
	defer statusTicker.Stop()

	for {
		wrapperEvent := <-input

		event := (wrapperEvent.payload).(eventlooptypes.EventLoopMessage)

		if event.Event != nil {
			// ctx = sharedutil.AddKCPClusterToContext(ctx, event.Event.Request.ClusterName)
		}

		if wrapperEvent.messageType == workspaceEventLoopMessageType_Event {
			// When the workspace event loop receive an event message, process it

			handleWorkspaceEventLoopMessage(ctx, event, wrapperEvent, state)

		} else if wrapperEvent.messageType == workspaceEventLoopMessageType_managedEnvProcessed_Event {
			// When the workspace event loop receives this message, it informs all of the applictions event loops about the event

			handleManagedEnvProcessedMessage(event, state)

		} else if wrapperEvent.messageType == workspaceEventLoopMessageType_statusTicker {
			// Every X minutes, the workspace event loop will send itself a message, causing it to check the
			// status of the application event loops it is tracking, and clean them up if needed.

			handleStatusTickerMessage(state)

		} else {
			log.Error(nil, "SEVERE: unrecognized workspace event loop message type")
		}
	}
}

func handleWorkspaceEventLoopMessage(ctx context.Context, event eventlooptypes.EventLoopMessage, wrapperEvent workspaceEventLoopMessage,
	state workspaceEventLoopInternalState) {

	log := state.log

	// First, sanity check the event
	if event.MessageType == eventlooptypes.ApplicationEventLoopMessageType_WorkComplete {
		log.Error(nil, "SEVERE: invalid message type received in workspaceEventLoopRouter")
		return
	}
	if event.Event == nil {
		log.Error(nil, "SEVERE: event was nil in workspaceEventLoopRouter")
		return
	}

	log.V(sharedutil.LogLevel_Debug).Info("workspaceEventLoop received event", "event", eventlooptypes.StringEventLoopEvent(event.Event))

	// If it's a GitOpsDeploymentSyncRun:
	//   - If the SyncRun exists, use the name that was specified
	//   - If the Syncrun doesn't exist, retrieve from the database
	//     - If it's not found in the database, it's orphaned.

	// Check GitOpsDeployment resource events that come in: if we get an event for GitOpsDeployment that has an
	// orphaned child resource depending on it, then unorphan the child resource.
	if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentTypeName {
		unorphanResourcesIfPossible(ctx, event, state.orphanedResources, state.input, log)
	}

	// Repository Credentials: Handle, then reutrn
	if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentRepositoryCredentialTypeName {
		state.workspaceResourceLoop.processRepositoryCredential(ctx, event.Event.Request, event.Event.Client)
		return
	}

	// Managed Environment: Handle, then return
	if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentManagedEnvironmentTypeName {

		// Reconcile to managed environment in the workspace resource loop, to ensure it is up-to-date (exists, or is deleted)
		state.workspaceResourceLoop.processManagedEnvironment(ctx, event, event.Event.Client)

		return
	}

	var associatedGitOpsDeploymentName string // name of the GitOpsDeployment (in this namespace) that this resource is associated with

	if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentTypeName {
		associatedGitOpsDeploymentName = event.Event.Request.Name

	} else if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentSyncRunTypeName {

		associatedGitOpsDeploymentName = checkIfOrphanedGitOpsDeploymentSyncRun(ctx, event, state.orphanedResources, log)

		if associatedGitOpsDeploymentName == "" {
			// The SyncRun no longer exists, or an unrecoverable error occurred, so just continue
			return
		}
	}

	if associatedGitOpsDeploymentName == "" {
		// Sanity check the name field is non-empty
		log.Error(nil, "SEVERE: received a resource event that we could not determine a GitOpsDeployment name for", "event", eventlooptypes.StringEventLoopEvent(event.Event))
		return
	}

	mapKey := state.namespaceID + "-" + event.Event.Request.Namespace + "-" + associatedGitOpsDeploymentName

	applicationEntryVal, exists := state.applicationMap[mapKey]
	if !exists {

		var err error
		applicationEntryVal, err = startApplicationEventQueueLoop(ctx, event.Event.Client, associatedGitOpsDeploymentName, event,
			state.sharedResourceEventLoop, state.applEventLoopFactory, log)
		if err != nil {
			// We already logged the error in startApplicationEventLoop, no need to log here
			return
		}

		// Add the loop to our map
		state.applicationMap[mapKey] = applicationEntryVal
	}

	if event.Event == nil { // Sanity check the event
		log.Error(nil, "SEVERE: event was nil in workspace_event loop")
	}

	// Send the event to the channel/go routine that handles all events for this application/gitopsdepl
	// we wait for a response from the channel (on ResponseChan) before continuing.
	syncResponseChan := make(chan application_event_loop.ResponseMessage)
	applicationEntryVal.input <- application_event_loop.RequestMessage{
		Message: eventlooptypes.EventLoopMessage{
			MessageType: eventlooptypes.ApplicationEventLoopMessageType_Event,
			Event:       event.Event,
		},
		ResponseChan: syncResponseChan,
	}

	responseMessage := <-syncResponseChan

	if !responseMessage.RequestAccepted {
		// Request was rejected: this means the application event loop has terminated.

		// Remove the terminated application event loop from the map
		delete(state.applicationMap, mapKey)

		// Requeue the message on a separate goroutine, so it will be processed again.
		go func() {
			state.input <- wrapperEvent
		}()

		return
	}

}

// handleManagedEnvProcessedMessage: when the workspace event loop receives this message, it informs all of
// the applictions event loops about the event.
func handleManagedEnvProcessedMessage(event eventlooptypes.EventLoopMessage, state workspaceEventLoopInternalState) {

	state.log.V(sharedutil.LogLevel_Debug).Info(fmt.Sprintf("received ManagedEnvironment event, passed event to %d applications",
		len(state.applicationMap)))

	if event.Event == nil { // Sanity check the event
		state.log.Error(nil, "SEVERE: event was nil in handleManagedEnvProcessedMessage")
	}

	// Send a message about this ManagedEnvironment to all of the goroutines currently processing GitOpsDeployment/SyncRuns
	for key := range state.applicationMap {
		applicationEntryVal := state.applicationMap[key]

		go func() {

			applicationEntryVal.input <- application_event_loop.RequestMessage{
				Message: eventlooptypes.EventLoopMessage{
					MessageType: eventlooptypes.ApplicationEventLoopMessageType_Event,
					Event:       event.Event,
				},
				ResponseChan: nil,
			}
		}()
	}

}

// handleStatusTickerMessage: every X minutes, the workspace event loop will send itself a message, causing it to
// check the status of the application event loops it is tracking, and clean them up if needed.
func handleStatusTickerMessage(state workspaceEventLoopInternalState) {

	// For each of the Application Event Loops (GitOpsDeployments in the namespace) that we are tracking...
	for key := range state.applicationMap {
		applicationEntryVal := state.applicationMap[key]

		// Send a synchronous message to each Application Event Loop, asking if it is still active
		syncResponseChan := make(chan application_event_loop.ResponseMessage)

		applicationEntryVal.input <- application_event_loop.RequestMessage{
			Message: eventlooptypes.EventLoopMessage{
				MessageType: eventlooptypes.ApplicationEventLoopMessageType_StatusCheck,
				Event:       nil,
			},
			ResponseChan: syncResponseChan,
		}
		responseMessage := <-syncResponseChan

		// If the Application Event Loop is not active (it is terminating), then the remove it from
		// the list of active applications.
		// - This allows us to clean up old appliction event loops.
		if !responseMessage.RequestAccepted {
			// Request was rejected: this means the application event loop has terminated.
			// Remove the terminated application event loop from the map
			delete(state.applicationMap, key)
		}
	}

}

func startApplicationEventQueueLoop(ctx context.Context, k8sClient client.Client, associatedGitOpsDeploymentName string, event eventlooptypes.EventLoopMessage,
	sharedResourceEventLoop *shared_resource_loop.SharedResourceEventLoop,
	applEventLoopFactory applicationEventQueueLoopFactory, log logr.Logger) (workspaceEventLoop_applicationEventLoopEntry, error) {

	// Start the application event queue go-routine

	aeqlParam := application_event_loop.ApplicationEventQueueLoop{
		GitopsDeploymentName:      associatedGitOpsDeploymentName,
		GitopsDeploymentNamespace: event.Event.Request.Namespace,
		WorkspaceID:               event.Event.WorkspaceID,
		SharedResourceEventLoop:   sharedResourceEventLoop,
		VwsAPIExportName:          "", // this value will be replaced in startApplicationEventQueueLoop, so is blank
		InputChan:                 make(chan application_event_loop.RequestMessage),
		Client:                    k8sClient,
	}

	// Start the application event loop's goroutine
	if err := applEventLoopFactory.startApplicationEventQueueLoop(ctx, aeqlParam); err != nil {
		log.Error(err, "SEVERE: an error occurred when attempting to start application event queue loop")
		return workspaceEventLoop_applicationEventLoopEntry{}, err
	}

	applicationEntryVal := workspaceEventLoop_applicationEventLoopEntry{
		input: aeqlParam.InputChan,
	}

	return applicationEntryVal, nil
}

type workspaceEventLoop_applicationEventLoopEntry struct {
	// input is the channel used to communicate with an application event loop goroutine.
	input chan application_event_loop.RequestMessage
}

// applicationEventQueueLoopFactory is used to start the application event queue. It is a lightweight wrapper
// around the 'application_event_loop.StartApplicationEventQueueLoop' function.
//
// The defaultApplicationEventLoopFactory should be used in all cases, except for when writing mocks for unit tests.
type applicationEventQueueLoopFactory interface {
	startApplicationEventQueueLoop(ctx context.Context, aeqlParam application_event_loop.ApplicationEventQueueLoop) error
}

type defaultApplicationEventLoopFactory struct {
	vwsAPIExportName string
}

// The default implementation of startApplicationEventQueueLoop is just a simple wrapper around a call to
// StartApplicationEventQueueLoop
func (d defaultApplicationEventLoopFactory) startApplicationEventQueueLoop(ctx context.Context, aeqlParam application_event_loop.ApplicationEventQueueLoop) error {

	if aeqlParam.VwsAPIExportName != "" {
		return fmt.Errorf("vwsAPIExportName must be empty in defaultApplicationEventLoopFactory")
	}

	// We always use the vwsAPIExportName value from the factory
	aeqlParam.VwsAPIExportName = d.vwsAPIExportName

	application_event_loop.StartApplicationEventQueueLoop(ctx, aeqlParam)

	return nil
}

var _ applicationEventQueueLoopFactory = defaultApplicationEventLoopFactory{}

// Processes events related to GitOpsDeploymentSyncRuns: determines whether the referenced GitOpsDeployment exists.
// - If the GitOpsDepl exists, return the name
// - Otherwise, add the SyncRun resource to orphaned list (why? because it is a gitopsdeplsyncrun that refers to a gitopsdepl that doesn't exist)
//
// If the resource is orphaned, or an error occurred, "" is returned.
//
//	See https://docs.google.com/document/d/1e1UwCbwK-Ew5ODWedqp_jZmhiZzYWaxEvIL-tqebMzo/edit#heading=h.8tiycl1h7rns for details.
func checkIfOrphanedGitOpsDeploymentSyncRun(ctx context.Context, event eventlooptypes.EventLoopMessage,
	orphanedResources map[string]map[string]eventlooptypes.EventLoopEvent, log logr.Logger) string {

	if event.Event.ReqResource == eventlooptypes.GitOpsDeploymentSyncRunTypeName {

		// 1) Retrieve the SyncRun
		syncRunCR := &v1alpha1.GitOpsDeploymentSyncRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      event.Event.Request.Name,
				Namespace: event.Event.Request.Namespace,
			},
		}
		if err := event.Event.Client.Get(ctx, client.ObjectKeyFromObject(syncRunCR), syncRunCR); err != nil {
			if apierr.IsNotFound(err) {
				log.V(sharedutil.LogLevel_Debug).Info("skipping potentially orphaned resource that could no longer be found:", "resource", syncRunCR.ObjectMeta)

				// The SyncRun CR doesn't exist, so try fetching the SyncOperation from the database and extract the name of GitOpsDeployment
				dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
				if err != nil {
					log.Error(err, "failed to get a connection to the databse")
					return ""
				}

				syncOperation, err := getDBSyncOperationFromAPIMapping(ctx, dbQueries, event.Event.Client, syncRunCR)
				if err != nil {
					log.Error(err, "failed to get SyncOperation using APICRToDBMapping")
					return ""
				}

				return syncOperation.DeploymentNameField
			} else {
				log.Error(err, "unexpected client error on retrieving syncrun object", "resource", syncRunCR.ObjectMeta)
				return ""
			}
		}

		// 2) Retrieve the GitOpsDeployment, referenced by the SyncRun, to see if the SyncRun is orphaned
		gitopsDeplCR := &v1alpha1.GitOpsDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      syncRunCR.Spec.GitopsDeploymentName,
				Namespace: event.Event.Request.Namespace,
			},
		}
		if err := event.Event.Client.Get(ctx, client.ObjectKeyFromObject(gitopsDeplCR), gitopsDeplCR); err != nil {

			if !apierr.IsNotFound(err) {
				log.Error(err, "unexpected client error on retrieving GitOpsDeployment references by GitOpsDeploymentSyncRun", "resource", syncRunCR.ObjectMeta)
				return ""
			}

			log.V(sharedutil.LogLevel_Debug).Info("was unable to locate GitOpsDeployment referenced by GitOpsDeploymentSyncRun, so the SyncRun is still orphaned.")

			// Otherwise, the GitOpsDeployment couldn't be found: therefore the SyncRun is orphaned, so continue.

		} else {
			// The resource is not orphaned, so our work is done: return the GitOpsDeployment referenced by the GitOpsDeploymentSyncRun
			return gitopsDeplCR.Name
		}

		// At this point, the SyncRun exists, but the GitOpsDeployment referenced by it doesn't

		// 3) Retrieve the list of orphaned resources that are depending on the GitOpsDeployment name referenced in the spec
		// field of the SyncRun.
		gitopsDeplMap, exists := orphanedResources[syncRunCR.Spec.GitopsDeploymentName]
		if !exists {
			gitopsDeplMap = map[string]eventlooptypes.EventLoopEvent{}
			orphanedResources[syncRunCR.Spec.GitopsDeploymentName] = gitopsDeplMap
		}

		// 4) Add this resource event to that list
		log.V(sharedutil.LogLevel_Debug).Info("Adding syncrun CR to orphaned resources list, name: " + syncRunCR.Name + ", missing gitopsdepl name: " + syncRunCR.Spec.GitopsDeploymentName)
		gitopsDeplMap[syncRunCR.Name] = *event.Event

		return ""

	} else {

		log.Error(nil, "SEVERE: unexpected event resource type in handleOrphaned")
		return ""
	}

}

func getDBSyncOperationFromAPIMapping(ctx context.Context, dbQueries db.DatabaseQueries, k8sclient client.Client, syncRunCR *v1alpha1.GitOpsDeploymentSyncRun) (db.SyncOperation, error) {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: syncRunCR.Namespace,
		},
	}
	if err := k8sclient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
		return db.SyncOperation{}, fmt.Errorf("failed to get namespace %s: %v", namespace.Name, err)
	}

	apiCRToDBMappingList := []db.APICRToDatabaseMapping{}
	if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentSyncRun, syncRunCR.Name, syncRunCR.Namespace, string(namespace.UID), db.APICRToDatabaseMapping_DBRelationType_SyncOperation, &apiCRToDBMappingList); err != nil {
		return db.SyncOperation{}, fmt.Errorf("failed to list APICRToDBMapping by namespace and name: %v", err)
	}

	if len(apiCRToDBMappingList) != 1 {
		return db.SyncOperation{}, fmt.Errorf("SEVERE: unexpected number of APICRToDBMappings for GitOpsDeploymentSyncRun %s: %d", syncRunCR.Name, len(apiCRToDBMappingList))
	}

	apiCRToDBMapping := apiCRToDBMappingList[0]

	if apiCRToDBMapping.DBRelationType != db.APICRToDatabaseMapping_DBRelationType_SyncOperation {
		return db.SyncOperation{}, fmt.Errorf("expected db relation type to be 'SyncOperation', but found '%s'", apiCRToDBMapping.DBRelationType)
	}

	syncOperation := db.SyncOperation{SyncOperation_id: apiCRToDBMapping.DBRelationKey}
	if err := dbQueries.GetSyncOperationById(ctx, &syncOperation); err != nil {
		return db.SyncOperation{}, fmt.Errorf("failed to retrieve syncoperation by id: %v", err)
	}

	return syncOperation, nil
}

func unorphanResourcesIfPossible(ctx context.Context, event eventlooptypes.EventLoopMessage,
	orphanedResources map[string]map[string]eventlooptypes.EventLoopEvent,
	input chan workspaceEventLoopMessage, log logr.Logger) {

	// If there exists an orphaned resource that is waiting for this gitopsdepl (by name)
	if gitopsDeplMap, exists := orphanedResources[event.Event.Request.Name]; exists {

		// Retrieve the gitopsdepl
		gitopsDeplCR := &v1alpha1.GitOpsDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      event.Event.Request.Name,
				Namespace: event.Event.Request.Namespace,
			},
		}

		if err := event.Event.Client.Get(ctx, client.ObjectKeyFromObject(gitopsDeplCR), gitopsDeplCR); err != nil {
			// log as warning, but continue.
			log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected client error on retrieving gitopsdepl")

		} else {
			// Copy the events to a new slice, and remove the events from the orphanedResourced map
			requeueEvents := []eventlooptypes.EventLoopMessage{}
			for index := range gitopsDeplMap {

				orphanedResourceEvent := gitopsDeplMap[index]

				requeueEvents = append(requeueEvents, eventlooptypes.EventLoopMessage{
					MessageType: eventlooptypes.ApplicationEventLoopMessageType_Event,
					Event:       &orphanedResourceEvent,
				})

				log.V(sharedutil.LogLevel_Debug).Info("found parent: " + gitopsDeplCR.Name + " (" + string(gitopsDeplCR.UID) + "), of orphaned resource: " + orphanedResourceEvent.Request.Name)
			}
			delete(orphanedResources, event.Event.Request.Name)

			// Requeue the orphaned events from a separate goroutine (to prevent us from blocking this goroutine).
			// The orphaned events will be reprocessed after the gitopsdepl is processed.
			go func() {
				for _, eventToRequeue := range requeueEvents {
					log.V(sharedutil.LogLevel_Debug).Info("requeueing orphaned resource: " + eventToRequeue.Event.Request.Name +
						", for parent: " + gitopsDeplCR.Name + " in namespace " + gitopsDeplCR.Namespace)
					input <- workspaceEventLoopMessage{
						messageType: workspaceEventLoopMessageType_Event,
						payload:     eventToRequeue,
					}
				}
			}()
		}
	}
}
