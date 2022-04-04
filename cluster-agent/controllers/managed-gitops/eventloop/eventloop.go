package eventloop

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	operation "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/cluster-agent/controllers"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

type ControllerEventLoop struct {
	eventLoopInputChannel chan controllerEventLoopEvent
}

func NewControllerEventLoop() *ControllerEventLoop {
	channel := make(chan controllerEventLoopEvent)

	res := &ControllerEventLoop{}
	res.eventLoopInputChannel = channel

	go controllerEventLoopRouter(channel)

	return res

}

type controllerEventLoopEvent struct {
	request ctrl.Request

	client client.Client
}

func (evl *ControllerEventLoop) EventReceived(req ctrl.Request, client client.Client) {

	event := controllerEventLoopEvent{request: req, client: client}
	evl.eventLoopInputChannel <- event
}

func controllerEventLoopRouter(input chan controllerEventLoopEvent) {

	ctx := context.Background()

	log := log.FromContext(ctx)

	taskRetryLoop := sharedutil.NewTaskRetryLoop("cluster-agent")

	log.Info("controllerEventLoopRouter started")

	for {
		newEvent := <-input

		mapKey := newEvent.request.Name + "-" + newEvent.request.Namespace

		// Call PerformTask below, when a request is received.
		task := &processEventTask{
			event: controllerEventLoopEvent{
				request: newEvent.request,
				client:  newEvent.client,
			},
			log: log,
		}
		taskRetryLoop.AddTaskIfNotPresent(mapKey, task, sharedutil.ExponentialBackoff{Factor: 2, Min: time.Millisecond * 200, Max: time.Second * 10, Jitter: true})

	}

}

type processEventTask struct {
	event controllerEventLoopEvent
	log   logr.Logger
}

func (task *processEventTask) PerformTask(taskContext context.Context) (bool, error) {
	dbQueries, err := db.NewProductionPostgresDBQueries(true)
	if err != nil {
		task.log.Error(err, "unable to instantiate database")
		return true, err
	}

	// Process the event
	dbOperation, shouldRetry, err := task.internalPerformTask(taskContext, dbQueries)

	if dbOperation != nil {

		// Don't update the status of operations that have previously completed.
		if dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed {
			return false, err
		}

		if shouldRetry {
			// Not complete, still (re)trying.
			dbOperation.State = db.OperationState_In_Progress
		} else {

			// Complete, but possibly due to a fatal error
			if err == nil {
				dbOperation.State = db.OperationState_Completed
			} else {
				dbOperation.State = db.OperationState_Failed
			}
		}
		dbOperation.Last_state_update = time.Now()

		if err != nil {
			// TODO: GITOPSRVCE-77 - SECURITY - At some point, we will likely want to sanitize the error value for users
			dbOperation.Human_readable_state = err.Error()
		}

		if err := dbQueries.UpdateOperation(taskContext, dbOperation); err != nil {
			task.log.Error(err, "unable to update operation state", "operation", dbOperation.Operation_id)
			return true, err
		} else {
			task.log.Info("Updated Operation State at: ", dbOperation.Operation_id)
		}
	}

	return shouldRetry, err

}

func (task *processEventTask) internalPerformTask(taskContext context.Context, dbQueries db.DatabaseQueries) (*db.Operation, bool, error) {
	log := log.FromContext(taskContext)

	eventClient := task.event.client

	// 1) Retrieve an up-to-date copy of the Operation CR that we want to process.
	operationCR := &operation.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      task.event.request.Name,
			Namespace: task.event.request.Namespace,
		},
	}
	if err := eventClient.Get(taskContext, client.ObjectKeyFromObject(operationCR), operationCR); err != nil {
		if apierr.IsNotFound(err) {
			// work is complete
			log.V(sharedutil.LogLevel_Debug).Info("Received a request for an operation CR that doesn't exist: " + operationCR.Namespace + "/" + operationCR.Name)
			return nil, false, nil

		} else {
			// generic error
			log.Error(err, err.Error())
			return nil, true, err
		}
	}

	// 2) Retrieve the database entry that corresponds to the Operation CR.
	dbOperation := db.Operation{
		Operation_id: operationCR.Spec.OperationID,
	}
	if err := dbQueries.GetOperationById(taskContext, &dbOperation); err != nil {

		if db.IsResultNotFoundError(err) {
			// no corresponding db operation, so no work to do
			log.V(sharedutil.LogLevel_Warn).Info("Received operation requested for operation DB entry that doesn't exist: " + dbOperation.Operation_id)
			return nil, false, nil
		} else {
			// some other generic error
			log.Error(err, "Unable to retrieve operation due to generic error: "+dbOperation.Operation_id)
			return nil, true, err
		}
	}

	// If the operation has already completed (we previously ran it), then just ignore it and return
	if dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed {
		return &dbOperation, false, nil
	}

	// 3) Find the Argo CD instance that is targeted by this operation.
	dbGitopsEngineInstance := &db.GitopsEngineInstance{
		Gitopsengineinstance_id: dbOperation.Instance_id,
	}
	if err := dbQueries.GetGitopsEngineInstanceById(taskContext, dbGitopsEngineInstance); err != nil {

		if db.IsResultNotFoundError(err) {
			// log as warning
			log.Error(err, "Receive operation on gitops engine instance that doesn't exist: "+dbOperation.Operation_id)

			// no corresponding db operation, so no work to do
			return &dbOperation, false, nil
		} else {
			// some other generic error
			log.Error(err, "Unexpected error on retrieving GitOpsEngineInstance in internalPerformTask")
			return &dbOperation, true, err
		}
	}

	// Sanity test: find the gitops engine cluster, by kube-system, and ensure that the
	// gitopsengineinstance matches the gitops engine cluster we are running on.
	kubeSystemNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", Namespace: "kube-system"}}
	if err := eventClient.Get(taskContext, client.ObjectKeyFromObject(kubeSystemNamespace), kubeSystemNamespace); err != nil {
		log.Error(err, "SEVERE: Unable to retrieve kube-system namespace")
		return &dbOperation, true, fmt.Errorf("unable to retrieve kube-system namespace in internalPerformTask")
	}
	if thisCluster, err := dbutil.GetGitopsEngineClusterByKubeSystemNamespaceUID(taskContext, string(kubeSystemNamespace.UID), dbQueries, log); err != nil {
		log.Error(err, "Unable to retrieve GitOpsEngineCluster when processing Operation")
		return &dbOperation, true, err
	} else if thisCluster == nil {
		log.Error(err, "GitOpsEngineCluster could not be found when processing Operation")
		return &dbOperation, true, nil
	} else if thisCluster.Gitopsenginecluster_id != dbGitopsEngineInstance.EngineCluster_id {
		log.Error(nil, "SEVERE: The gitops engine cluster that the cluster-agent is running on did not match the operation's target argo cd instance id.")
		return &dbOperation, true, nil
	}

	// 4) Find the namespace for the targeted Argo CD instance
	argoCDNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbGitopsEngineInstance.Namespace_name,
			Namespace: dbGitopsEngineInstance.Namespace_name,
		},
	}
	if err := eventClient.Get(taskContext, client.ObjectKeyFromObject(argoCDNamespace), argoCDNamespace); err != nil {
		if db.IsResultNotFoundError(err) {
			log.Error(err, "Argo CD namespace doesn't exist")
			// no corresponding db operation, so no work to do
			return &dbOperation, false, nil
		} else {
			log.Error(err, "unexpected error on retrieve Argo CD namespace")
			// some other generic error
			return &dbOperation, true, err
		}
	}
	if string(argoCDNamespace.UID) != dbGitopsEngineInstance.Namespace_uid {
		log.Error(nil, "SEVERE: Engine instance did not match Argo CD namespace uid, while processing operation")
		return &dbOperation, false, nil
	}

	// Finally, call the corresponding method for processing the particular type of Operation.

	if dbOperation.Resource_type == db.OperationResourceType_Application {
		shouldRetry, err := processOperation_Application(taskContext, dbOperation, *operationCR, dbQueries, *argoCDNamespace, eventClient, log)

		if err != nil {
			log.Error(err, "error occurred on processing the application operation")
		}

		return &dbOperation, shouldRetry, err

	} else {
		log.Error(nil, "SEVERE: unrecognized resource type: "+dbOperation.Resource_type)
		return &dbOperation, false, nil
	}

}

// processOperation_Application handles an Operation that targets an Application. Returns true if the task should be retried (eg due to failure).
func processOperation_Application(ctx context.Context, dbOperation db.Operation, crOperation operation.Operation, dbQueries db.DatabaseQueries,
	argoCDNamespace corev1.Namespace, eventClient client.Client, log logr.Logger) (bool, error) {

	// Sanity check
	if dbOperation.Resource_id == "" {
		return true, fmt.Errorf("resource id was nil while processing operation: " + crOperation.Name)
	}

	dbApplication := &db.Application{
		Application_id: dbOperation.Resource_id,
	}

	if err := dbQueries.GetApplicationById(ctx, dbApplication); err != nil {

		if db.IsResultNotFoundError(err) {
			// The application db entry no longer exists, so delete the corresponding application CR

			// Find the Application that has the corresponding databaseID label
			list := appv1.ApplicationList{}
			labelSelector := labels.NewSelector()
			req, err := labels.NewRequirement("databaseID", selection.Equals, []string{dbApplication.Application_id})
			if err != nil {
				log.Error(err, "invalid label requirement")
				return true, err
			}
			labelSelector = labelSelector.Add(*req)
			if err := eventClient.List(ctx, &list, &client.ListOptions{
				Namespace:     argoCDNamespace.Name,
				LabelSelector: labelSelector,
			}); err != nil {
				log.Error(err, "unable to complete Argo CD Application list")
				return true, err
			}

			if len(list.Items) > 1 {
				// Sanity test: should really only ever be 0 or 1
				log.Error(nil, "unexpected number of items in list", "length", len(list.Items))
			}

			var firstDeletionErr error
			for _, item := range list.Items {
				// Delete all Argo CD applications with the corresponding database label (but, there should be only one)
				err := controllers.DeleteArgoCDApplication(ctx, item, eventClient, log)
				if err != nil {
					log.Error(err, "error on deleting Argo CD Application: %v", item.Name)

					if firstDeletionErr == nil {
						firstDeletionErr = err
					}
				}
			}

			if firstDeletionErr != nil {
				log.Error(firstDeletionErr, "Deletion of at least one Argo CD application failed. First error was: %v", firstDeletionErr)
				return true, firstDeletionErr
			}

			return false, nil

		} else {
			log.Error(err, "An error occurred while attempting to retrieve Argo CD Application cR")
			return true, err
		}
	}

	app := &appv1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbApplication.Name,
			Namespace: argoCDNamespace.Name,
		},
	}

	log = log.WithValues("app.Name", app.Name)

	if err := eventClient.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {

		if apierr.IsNotFound(err) {
			// The Application CR doesn't exist, so we need to create it

			if err := yaml.Unmarshal([]byte(dbApplication.Spec_field), app); err != nil {
				log.Error(err, "SEVERE: unable to unmarshal application spec field on creating Application CR: "+app.Name)
				// We return nil here, because there's likely nothing else that can be done to fix this.
				// Thus there is no need to keep retrying.
				return false, nil
			}

			app.ObjectMeta.Labels = map[string]string{"databaseID": dbApplication.Application_id}

			if err := eventClient.Create(ctx, app, &client.CreateOptions{}); err != nil {
				log.Error(err, "unable to create Argo CD Application CR: "+app.Name)
				// This may or may not be salvageable depending on the error; ultimately we should figure out which
				// error messages mean unsalvageable, and not wait for them.
				return true, err
			}

			log.Info("Created Argo CD Application CR: " + app.Name)

			return false, nil

		} else {
			log.Error(err, "Unexpected error when attempting to retrieve Argo CD Application CR")
			return false, err
		}

	}

	// The application CR exists, and the database entry exists, so check if there is any
	// difference between them.

	specFieldApp := &appv1.Application{}

	if err := yaml.Unmarshal([]byte(dbApplication.Spec_field), specFieldApp); err != nil {
		log.Error(err, "SEVERE: unable to unmarshal DB application spec field, on updating existing Application cR: "+app.Name)
		// We return nil here, because there's likely nothing else that can be done to fix this.
		// Thus there is no need to keep retrying.
		return false, nil
	}

	var specDiff string
	if !reflect.DeepEqual(specFieldApp.Spec.Source, app.Spec.Source) {
		specDiff = "spec.source fields differ"
	} else if !reflect.DeepEqual(specFieldApp.Spec.Destination, app.Spec.Destination) {
		specDiff = "spec.destination fields differ"
	} else if specFieldApp.Spec.Project != app.Spec.Project {
		specDiff = "spec project fields differ"
	} else if specFieldApp.Spec.SyncPolicy != app.Spec.SyncPolicy {
		specDiff = "sync policy fields differ"
	}

	if specDiff != "" {
		app.Spec.Destination = specFieldApp.Spec.Destination
		app.Spec.Source = specFieldApp.Spec.Source
		app.Spec.Project = specFieldApp.Spec.Project

		if err := eventClient.Update(ctx, app); err != nil {
			log.Error(err, "unable to update application after difference detected: "+app.Name)
			return false, err
		}

		log.Info("Updated Argo CD Application CR: " + app.Name + ", diff was: " + specDiff)

	} else {
		log.Info("no changes detected in application, so no update needed")
	}

	return false, nil
}
