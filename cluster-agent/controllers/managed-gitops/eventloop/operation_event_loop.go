package eventloop

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	operation "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	argosharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
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

// OperationEventLoop is informed of Operation resource changes (creations/modifications/deletions) by
// the Operation controller (operation_controller.go).
//
// Next, these resource changes (events) are then processed within a task retry loop.
// - Operations are how the backend informs the cluster-agent of database changes.
// - For example:
//     - The user updated a field in a GitOpsDeployment in the user's namespace
//     - The managed-gitops backend updated corresponding field in the Application database row
//     - Next: an Operation was created to inform the cluster-agent component of the Application row changed
//     - We are here: now the Operation controller of cluster-agent has been informed of the Operation.
//     - We need to: Look at the Operation, determine what database entry changed, and ensure that Argo CD
//                   is reconciled to the contents of the database entry.
//
// The overall workflow of Operations can be found in the architecture doc:
// https://docs.google.com/document/d/1e1UwCbwK-Ew5ODWedqp_jZmhiZzYWaxEvIL-tqebMzo/edit#heading=h.9vyguee8vhow
//
type OperationEventLoop struct {
	eventLoopInputChannel chan operationEventLoopEvent
}

func NewOperationEventLoop() *OperationEventLoop {
	channel := make(chan operationEventLoopEvent)

	res := &OperationEventLoop{}
	res.eventLoopInputChannel = channel

	go operationEventLoopRouter(channel)

	return res

}

type operationEventLoopEvent struct {
	request ctrl.Request
	client  client.Client
}

func (evl *OperationEventLoop) EventReceived(req ctrl.Request, client client.Client) {

	event := operationEventLoopEvent{request: req, client: client}
	evl.eventLoopInputChannel <- event
}

func operationEventLoopRouter(input chan operationEventLoopEvent) {

	ctx := context.Background()

	log := log.FromContext(ctx)

	taskRetryLoop := sharedutil.NewTaskRetryLoop("cluster-agent")

	log.Info("controllerEventLoopRouter started")

	for {
		newEvent := <-input

		mapKey := newEvent.request.Name + "-" + newEvent.request.Namespace

		// Queue a new task in the task retry loop for our event.
		task := &processOperationEventTask{
			event: operationEventLoopEvent{
				request: newEvent.request,
				client:  newEvent.client,
			},
			log: log,
		}
		taskRetryLoop.AddTaskIfNotPresent(mapKey, task, sharedutil.ExponentialBackoff{Factor: 2, Min: time.Millisecond * 200, Max: time.Second * 10, Jitter: true})

	}

}

// processOperationEventTask takes as input an Operation resource event, and processes it based on the contents of that event.
type processOperationEventTask struct {
	event operationEventLoopEvent
	log   logr.Logger
}

// PerformTask takes as input an Operation resource event, and processes it based on the contents of that event.
//
// Returns bool (true if the task should be retried, for example because it failed, false otherwise),
// and error (an error to log on failure).
//
// NOTE: 'error' value does not affect whether the task will be retried, this error is only used for
// error reporting.
func (task *processOperationEventTask) PerformTask(taskContext context.Context) (bool, error) {
	dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
	if err != nil {
		return true, fmt.Errorf("unable to instantiate database in operation controller loop: %v", err)
	}
	defer dbQueries.CloseDatabase()

	// Process the event (this is where most of the work is done)
	dbOperation, shouldRetry, err := task.internalPerformTask(taskContext, dbQueries)

	if dbOperation != nil {

		// After the event is processed, update the status in the database

		// Don't update the status of operations that have previously completed.
		if dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed {
			return false, err
		}

		// If the task failed and thus should be retried...
		if shouldRetry {
			// Not complete, still (re)trying.
			dbOperation.State = db.OperationState_In_Progress
		} else {

			// Complete (but complete doesn't mean successful: it could be complete due to a fatal error)
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

		// Update the Operation row of the database, based on the new state.
		if err := dbQueries.UpdateOperation(taskContext, dbOperation); err != nil {
			task.log.Error(err, "unable to update operation state", "operation", dbOperation.Operation_id)
			return true, err
		}
		task.log.Info("Updated Operation state for '" + dbOperation.Operation_id + "', state is: " + string(dbOperation.State))
	}

	return shouldRetry, err

}

func (task *processOperationEventTask) internalPerformTask(taskContext context.Context, dbQueries db.DatabaseQueries) (*db.Operation, bool, error) {
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
			// If the resource doesn't exist, our job is done.
			log.V(sharedutil.LogLevel_Debug).Info("Received a request for an operation CR that doesn't exist: " + operationCR.Namespace + "/" + operationCR.Name)
			return nil, false, nil

		} else {
			// generic error
			return nil, true, fmt.Errorf("unable to retrieve operation CR: %v", err)
		}
	}

	log = log.WithValues("operationID", operationCR.Spec.OperationID)

	// 2) Retrieve the database entry that corresponds to the Operation CR.
	dbOperation := db.Operation{
		Operation_id: operationCR.Spec.OperationID,
	}
	if err := dbQueries.GetOperationById(taskContext, &dbOperation); err != nil {

		if db.IsResultNotFoundError(err) {
			// no corresponding db operation, so no work to do
			log.V(sharedutil.LogLevel_Warn).Info("Received operation requested for operation DB entry that doesn't exist")
			return nil, false, nil
		} else {
			// some other generic error
			log.Error(err, "Unable to retrieve operation due to generic error")
			return nil, true, err
		}
	}

	// If the operation has already completed (e.g. we previously ran it), then just ignore it and return
	if dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed {
		log.V(sharedutil.LogLevel_Debug).Info("Skipping Operation with state of completed/failed")
		return &dbOperation, false, nil
	}

	// If the operation is in waiting state, update it to in-progress before we start processing it.
	if dbOperation.State == db.OperationState_Waiting {
		log.V(sharedutil.LogLevel_Debug).Info("Updating OperationState to InProgress")
		dbOperation.State = db.OperationState_In_Progress

		if err := dbQueries.UpdateOperation(taskContext, &dbOperation); err != nil {
			log.Error(err, "unable to update Operation state")
			return nil, true, fmt.Errorf("unable to update Operation '%s' state: %v", dbOperation.Resource_id, err)
		}
	}

	// 3) Find the Argo CD instance that is targeted by this operation.
	dbGitopsEngineInstance := &db.GitopsEngineInstance{
		Gitopsengineinstance_id: dbOperation.Instance_id,
	}
	if err := dbQueries.GetGitopsEngineInstanceById(taskContext, dbGitopsEngineInstance); err != nil {

		if db.IsResultNotFoundError(err) {
			// log as warning
			log.Error(err, "Receive operation on gitops engine instance that doesn't exist")

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
		if apierr.IsNotFound(err) {
			log.Error(err, "Argo CD namespace doesn't exist: "+argoCDNamespace.Name)
			// Return retry as true, as it's possible it may exist in the future.
			return &dbOperation, true, fmt.Errorf("argo CD namespace doesn't exist: %s", argoCDNamespace.Name)
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

	} else if dbOperation.Resource_type == db.OperationResourceType_ManagedEnvironment {
		shouldRetry, err := processOperation_ManagedEnvironment(taskContext, dbOperation, *operationCR, dbQueries, *argoCDNamespace, eventClient, log)

		if err != nil {
			log.Error(err, "error occurred on processing the application operation")
		}

		return &dbOperation, shouldRetry, err

	} else if dbOperation.Resource_type == db.OperationResourceType_RepositoryCredentials {
		shouldRetry, err := processOperation_RepositoryCredentials(taskContext, dbOperation, *operationCR, dbQueries, *argoCDNamespace, eventClient, log)

		if err != nil {
			log.Error(err, "error occurred on processing the repository credentials operation")
		}

		return &dbOperation, shouldRetry, err

	} else {
		log.Error(nil, "SEVERE: unrecognized resource type: "+dbOperation.Resource_type)
		return &dbOperation, false, nil
	}

}

// processOoperation_ManagedEnvironment handles an Operation that targets an Application.
// Returns true if the task should be retried (eg due to failure), false otherwise.
func processOperation_ManagedEnvironment(ctx context.Context, dbOperation db.Operation, crOperation operation.Operation,
	dbQueries db.DatabaseQueries, argoCDNamespace corev1.Namespace, eventClient client.Client, log logr.Logger) (bool, error) {

	// The only operation we currently support for managed environment is deletion (creation is handled by Application operations).
	// Thus, we expect the ManagedEnvironment database entry here to not be found.

	// 1) Make sure the managed environment db entry DOESN'T exist (see above)
	{
		managedEnv := &db.ManagedEnvironment{
			Managedenvironment_id: dbOperation.Resource_id, // managed env id referencing managed env row
		}
		if err := dbQueries.GetManagedEnvironmentById(ctx, managedEnv); err != nil {
			if !db.IsResultNotFoundError(err) {
				return true, fmt.Errorf("an unexpected error occcurred on retrieving managed env: %v", err)
			}
		} else {
			// The database entry still exists, so return an error
			return false, fmt.Errorf("managed environment still exists in the database")
		}
	}

	// 2) Delete the Argo CD cluster secret corresponding to the managed environment.
	// - the cluster secret has a specific name format, so it is easy to locate by the name.

	// We have confirmed the database entry doesn't exist, now locate the Argo CD Cluster Secret that
	// corresponds to the managed environment.
	expectedSecretName := argosharedutil.GenerateArgoCDClusterSecretName(db.ManagedEnvironment{Managedenvironment_id: dbOperation.Resource_id})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      expectedSecretName,
			Namespace: argoCDNamespace.Name,
		},
	}

	if err := eventClient.Delete(ctx, secret); err != nil {
		if apierr.IsNotFound(err) {
			// The cluster secret doesn't exist, so our work is done.
			return false, nil
		} else {
			return true, fmt.Errorf("unable to retrieve Argo CD Cluster Secret '%s' in '%s': %v", secret.Name, secret.Namespace, err)
		}
	}

	// Argo CD cluster secret corresponding to environment is successfully deleted.
	return false, nil
}

const (
	ArgoCDSecretTypeKey                 = "argocd.argoproj.io/secret-type"
	ArgoCDSecretTypeValue_ClusterSecret = "cluster"
)

// processOperation_Application handles an Operation that targets an Application.
// Returns true if the task should be retried (eg due to failure), false otherwise.
func processOperation_Application(ctx context.Context, dbOperation db.Operation, crOperation operation.Operation, dbQueries db.DatabaseQueries,
	argoCDNamespace corev1.Namespace, eventClient client.Client, log logr.Logger) (bool, error) {

	// Sanity check
	if dbOperation.Resource_id == "" {
		return true, fmt.Errorf("resource id was nil while processing operation: " + crOperation.Name)
	}

	dbApplication := &db.Application{
		Application_id: dbOperation.Resource_id,
	}

	log = log.WithValues("applicationRow", dbApplication.Application_id)

	if err := dbQueries.GetApplicationById(ctx, dbApplication); err != nil {

		if db.IsResultNotFoundError(err) {
			// The application db entry no longer exists, so delete the corresponding Application CR

			// Find the Application that has the corresponding databaseID label
			list := appv1.ApplicationList{}
			labelSelector := labels.NewSelector()
			req, err := labels.NewRequirement(controllers.ArgoCDApplicationDatabaseIDLabel, selection.Equals, []string{dbApplication.Application_id})
			if err != nil {
				log.Error(err, "SEVERE: invalid label requirement")
				return false, err
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
				log.Error(nil, "SEVERE: unexpected number of items in list", "length", len(list.Items))
			}

			var firstDeletionErr error
			for _, item := range list.Items {

				log := log.WithValues("argoCDApplicationName", item.Name, "argoCDApplicationNmespace", item.Namespace)

				log.Info("Deleting Argo CD Application that is missing a DB Entry")

				// Delete all Argo CD applications with the corresponding database label (but, there should be only one)
				err := controllers.DeleteArgoCDApplication(ctx, item, eventClient, log)
				if err != nil {
					log.Error(err, "error on deleting Argo CD Application")

					if firstDeletionErr == nil {
						firstDeletionErr = err
					}
				}
			}

			if firstDeletionErr != nil {
				log.Error(firstDeletionErr, "Deletion of at least one Argo CD application failed.", "firstError", firstDeletionErr)
				return true, firstDeletionErr
			}

			// success
			return false, nil

		} else {
			log.Error(err, "Unable to retrieve database Application row from database")
			return true, err
		}
	}

	app := &appv1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbApplication.Name,
			Namespace: argoCDNamespace.Name,
		},
	}

	// Retrieve the Argo CD Application from the namespace
	log = log.WithValues("argoCDApplicationName", app.Name)
	if err := eventClient.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {

		if apierr.IsNotFound(err) {
			// The Application CR doesn't exist, so we need to create it

			// Copy the contents of the Spec_field database column, into the Spec field of the Argo CD Application CR
			if err := yaml.Unmarshal([]byte(dbApplication.Spec_field), app); err != nil {
				log.Error(err, "SEVERE: unable to unmarshal application spec field on creating Application CR: "+app.Name)
				// We return nil here, because there's likely nothing else that can be done to fix this.
				// Thus there is no need to keep retrying.
				return false, nil
			}

			// Add databaseID label
			app.ObjectMeta.Labels = map[string]string{controllers.ArgoCDApplicationDatabaseIDLabel: dbApplication.Application_id}

			// Before we create the application, make sure that the managed environment exists that the application points to
			if app.Spec.Destination.Name != argosharedutil.ArgoCDDefaultDestinationInCluster {
				if err := ensureManagedEnvironmentExists(ctx, *dbApplication, dbQueries, argoCDNamespace, eventClient, log); err != nil {
					log.Error(err, "unable to ensure that managed environment exists")
					return true, err
				}
			}

			if err := eventClient.Create(ctx, app, &client.CreateOptions{}); err != nil {
				log.Error(err, "unable to create Argo CD Application CR: "+app.Name)
				// This may or may not be salvageable depending on the error; ultimately we should figure out which
				// error messages mean unsalvageable, and not wait for them.
				return true, err
			}

			log.Info("Created Argo CD Application CR: " + app.Name)

			// Success
			return false, nil

		} else {
			// If another error occurred, retry.
			log.Error(err, "Unexpected error when attempting to retrieve Argo CD Application CR")
			return true, err
		}

	}

	// The application CR exists, and the database entry exists, so check if there is any
	// difference between them.

	// Before we create the application, make sure that the managed environment that the application points to exists

	specFieldApp := &appv1.Application{}

	if err := yaml.Unmarshal([]byte(dbApplication.Spec_field), specFieldApp); err != nil {
		log.Error(err, "SEVERE: unable to unmarshal DB application spec field, on updating existing Application cR: "+app.Name)
		// We return nil here, with no retry, because there's likely nothing else that can be done to fix this.
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
	} else if !reflect.DeepEqual(specFieldApp.Spec.SyncPolicy, app.Spec.SyncPolicy) {
		specDiff = "sync policy fields differ"
	}

	if specDiff != "" {
		app.Spec.Destination = specFieldApp.Spec.Destination
		app.Spec.Source = specFieldApp.Spec.Source
		app.Spec.Project = specFieldApp.Spec.Project
		app.Spec.SyncPolicy = specFieldApp.Spec.SyncPolicy

		if err := eventClient.Update(ctx, app); err != nil {
			log.Error(err, "unable to update application after difference detected: "+app.Name)
			// Retry if we were unable to update the Application, for example due to a conflict
			return true, err
		}

		log.Info("Updated Argo CD Application CR: " + app.Name + ", diff was: " + specDiff)

	} else {
		log.Info("no changes detected in application, so no update needed")
	}

	// Finally, ensure that the managed-environment secret is still up to date
	if app.Spec.Destination.Name != argosharedutil.ArgoCDDefaultDestinationInCluster {
		if err := ensureManagedEnvironmentExists(ctx, *dbApplication, dbQueries, argoCDNamespace, eventClient, log); err != nil {
			log.Error(err, "unable to ensure that managed environment exists")
			return true, err
		}
	}

	return false, nil
}

// ensureManagedEnvironmentExists ensures that the managed environment described by 'application' is defined as an Argo CD
// cluster secret, in the Argo CD namespace.
func ensureManagedEnvironmentExists(ctx context.Context, application db.Application, dbQueries db.DatabaseQueries,
	argoCDNamespace corev1.Namespace, eventClient client.Client, log logr.Logger) error {

	if application.Managed_environment_id == "" {
		// No work to do
		return nil
	}

	expectedSecret, shouldDeleteSecret, err := generateExpectedClusterSecret(ctx, application, dbQueries, argoCDNamespace, eventClient, log)
	if err != nil {
		return fmt.Errorf("unable to generate expected cluster secret: %v", err)
	}

	// If we detected that the managed environment row was deleted, ensure the secret is deleted.
	if shouldDeleteSecret {
		secretName := argosharedutil.GenerateArgoCDClusterSecretName(db.ManagedEnvironment{Managedenvironment_id: application.Managed_environment_id})
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: argoCDNamespace.Name,
			},
		}
		if err := eventClient.Delete(ctx, secret); err != nil {
			if apierr.IsNotFound(err) {
				// The secret doesn't exist, so no more work to do.
				return nil
			} else {
				return fmt.Errorf("unable to delete secret of deleted managed environment: %v", err)
			}
		}

		return nil
	}

	// If the secret is otherwise empty, no work is required
	if expectedSecret.Name == "" {
		return nil
	}

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      expectedSecret.Name,
			Namespace: expectedSecret.Namespace,
		},
	}
	if err := eventClient.Get(ctx, client.ObjectKeyFromObject(existingSecret), existingSecret); err != nil {
		if !apierr.IsNotFound(err) {
			return fmt.Errorf("unable to retrieve existing Argo CD Cluster secret '%s' in '%s'", existingSecret.Name, existingSecret.Namespace)
		}

		// A) Secret doesn't exist, so create it
		if err := eventClient.Create(ctx, &expectedSecret); err != nil {
			return fmt.Errorf("unable to create expected Argo CD Cluster secret '%s' in '%s'", expectedSecret.Name, expectedSecret.Namespace)
		}

		return nil
	}

	// B) Secret already exists, so compare
	if reflect.DeepEqual(existingSecret.Data, expectedSecret.Data) {
		// No work required, so exit.
		return nil
	}
	existingSecret.Data = expectedSecret.Data

	// C) Secret exists, but is different from what is expected, so update it.
	if err := eventClient.Update(ctx, existingSecret); err != nil {
		return fmt.Errorf("unable to update existing secret '%s' in '%s'", existingSecret.Name, existingSecret.Namespace)
	}

	return nil

}

// generateExpectedClusterSecret generates (but does apply) an Argo CD cluster secret for the environment of the application.
// returns:
// - argo cd cluster secret based on managed environment
// - bool: true if secret should be deleted false otherwise
// - error
func generateExpectedClusterSecret(ctx context.Context, application db.Application, dbQueries db.DatabaseQueries,
	argoCDNamespace corev1.Namespace, eventClient client.Client, log logr.Logger) (corev1.Secret, bool, error) {

	const (
		deleteSecret_true  = true
		deleteSecret_false = false
	)

	managedEnv := &db.ManagedEnvironment{
		Managedenvironment_id: application.Managed_environment_id,
	}

	if err := dbQueries.GetManagedEnvironmentById(ctx, managedEnv); err != nil {
		if db.IsResultNotFoundError(err) {
			// Application refers to a managed environment that doesn't exist: no more work to do.
			// Return true to indicate that the managed environment cluster secret should be deleted.
			return corev1.Secret{}, deleteSecret_true, nil
		} else {
			return corev1.Secret{}, deleteSecret_false,
				fmt.Errorf("unable to get managed environment '%s': %v", managedEnv.Managedenvironment_id, err)
		}
	}

	clusterCredentials := &db.ClusterCredentials{
		Clustercredentials_cred_id: managedEnv.Clustercredentials_id,
	}

	if err := dbQueries.GetClusterCredentialsById(ctx, clusterCredentials); err != nil {
		if db.IsResultNotFoundError(err) {
			// Managed environment refers to cluster credentials which no longer exist: no more work to do.
			// Return true to indicate that the managed environment cluster secret should be deleted.
			return corev1.Secret{}, deleteSecret_true, nil
		} else {
			return corev1.Secret{}, deleteSecret_false,
				fmt.Errorf("unable to get cluster credentials '%s': %v", clusterCredentials.Clustercredentials_cred_id, err)
		}
	}

	bearerToken := clusterCredentials.Serviceaccount_bearer_token

	name := argosharedutil.GenerateArgoCDClusterSecretName(*managedEnv)

	clusterSecretConfigJSON := ClusterSecretConfigJSON{
		BearerToken: bearerToken,
		TLSClientConfig: ClusterSecretTLSClientConfigJSON{
			Insecure: true, // TODO: GITOPSRVCE-178: Once TLS certification validation configuration is implmented, this value should be updated.
		},
	}

	jsonString, err := json.Marshal(clusterSecretConfigJSON)
	if err != nil {
		return corev1.Secret{}, deleteSecret_false, fmt.Errorf("SEVERE: unable to marshal JSON")
	}

	managedEnvironmentSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: argoCDNamespace.Name,
			Labels: map[string]string{
				ArgoCDSecretTypeKey:                            ArgoCDSecretTypeValue_ClusterSecret,
				controllers.ArgoCDClusterSecretDatabaseIDLabel: managedEnv.Managedenvironment_id,
			},
		},
		Data: map[string][]byte{
			"name":   ([]byte)(name),
			"server": ([]byte)(clusterCredentials.Host),
			"config": ([]byte)(string(jsonString)),
		},
	}

	return managedEnvironmentSecret, deleteSecret_false, nil

}

type ClusterSecretConfigJSON struct {
	BearerToken     string                           `json:"bearerToken"`
	TLSClientConfig ClusterSecretTLSClientConfigJSON `json:"tlsClientConfig"`
}

type ClusterSecretTLSClientConfigJSON struct {
	Insecure bool `json:"insecure"`
}
