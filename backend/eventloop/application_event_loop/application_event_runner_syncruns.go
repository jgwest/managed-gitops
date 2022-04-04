package application_event_loop

import (
	"context"
	"fmt"

	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file is responsible for processing events related to GitOpsDeploymentSyncRun CR.

func (a *applicationEventLoopRunner_Action) applicationEventRunner_handleSyncRunModified(ctx context.Context, dbQueries db.ApplicationScopedQueries) (bool, error) {

	log := a.log

	namespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Namespace: a.eventResourceNamespace, Name: a.eventResourceNamespace}, &namespace); err != nil {
		return false, fmt.Errorf("unable to retrieve namespace '%s': %v", a.eventResourceNamespace, err)
	}

	clusterUser, _, err := a.sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, a.workspaceClient, namespace)
	if err != nil {
		return false, fmt.Errorf("unable to retrieve cluster user in handleDeploymentModified, '%s': %v", string(namespace.UID), err)
	}

	// Retrieve the GitOpsDeploymentSyncRun from the namespace
	syncRunCRExists := true // True if the GitOpsDeployment resource exists in the namespace, false otherwise
	syncRunCR := &managedgitopsv1alpha1.GitOpsDeploymentSyncRun{}
	{
		syncRunKey := client.ObjectKey{Namespace: a.eventResourceNamespace, Name: a.eventResourceName}

		if err := a.workspaceClient.Get(ctx, syncRunKey, syncRunCR); err != nil {

			if apierr.IsNotFound(err) {
				syncRunCRExists = false
			} else {
				log.Error(err, "unable to locate object in handleSyncRunModified", "request", syncRunKey)
				return false, err
			}
		}
	}

	var apiCRToDBList []db.APICRToDatabaseMapping
	dbEntryExists := false
	if syncRunCRExists {
		mapping := db.APICRToDatabaseMapping{
			APIResourceType: db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentSyncRun,
			APIResourceUID:  string(syncRunCR.UID),
			DBRelationType:  db.APICRToDatabaseMapping_DBRelationType_SyncOperation,
		}

		if err := dbQueries.GetDatabaseMappingForAPICR(ctx, &mapping); err != nil {
			if db.IsResultNotFoundError(err) {
				// No corresponding entry
				dbEntryExists = false
			} else {
				// A generic error occured, so just return
				log.Error(err, "unable to resource APICRToDatabaseMapping", "uid", string(syncRunCR.UID))
				return false, err
			}
		} else {
			// Match found in database
			apiCRToDBList = append(apiCRToDBList, mapping)
		}
	} else {
		// The CR no longer exists (it was likely deleted), so instead we retrieve the UID of the SyncRun from
		// the APICRToDatabaseMapping table, by combination of (name/namespace/workspace).

		if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentSyncRun,
			a.eventResourceName, a.eventResourceNamespace, eventlooptypes.GetWorkspaceIDFromNamespaceID(namespace),
			db.APICRToDatabaseMapping_DBRelationType_SyncOperation, &apiCRToDBList); err != nil {

			log.Error(err, "unable to find API CR to DB Mapping, by API name/namespace/uid",
				"name", a.eventResourceName, "namespace", a.eventResourceNamespace, "UID", string(namespace.UID))
			return false, err
		}

		if len(apiCRToDBList) == 0 {
			// Not found: the database does not contain an entry for the GitOpsDeploymentSyncRun, and the CR doesn't exist,
			// so there is no more work for us to do.
			dbEntryExists = false
		} else {
			// Found: we were able to locate the DeplToAppMap resource for the deleted GitOpsDeployment,
			dbEntryExists = true
		}
	}

	log.Info("workspacerEventLoopRunner_handleSyncRunModified", "syncRunCRExists", syncRunCRExists, "dbEntryExists", dbEntryExists)

	if !syncRunCRExists && !dbEntryExists {
		log.Info("neither sync run CR exists, nor db entry, so our work is done.")
		// if neither exists, our work is done
		return true, nil
	}

	// The applications and gitopsengineinstance pointed to by the gitopsdeployment (if they are non-nil)
	var application *db.Application
	var gitopsEngineInstance *db.GitopsEngineInstance

	if syncRunCRExists {
		// Sanity check that the gitopsdeployment resource exists, which is referenced by the syncrun resource
		gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      syncRunCR.Spec.GitopsDeploymentName,
				Namespace: syncRunCR.Namespace,
			},
		}
		// Retrieve the GitOpsDeployment, and locate the corresponding application and gitopsengineinstance
		if err := a.workspaceClient.Get(ctx, client.ObjectKeyFromObject(gitopsDepl), gitopsDepl); err != nil {

			if apierr.IsNotFound(err) {
				// If the gitopsdepl doesn't exist, we really can't proceed any further

				err := fmt.Errorf("unable to retrieve gitopsdeployment referenced in syncrun: %v", err)
				// TODO: GITOPSRVCE-44 - ENHANCEMENT - If the gitopsDepl isn't referenced, update the status of the GitOpsDeplomentSyncRun condition as an error and return
				// TODO: GITOPSRVCE-44 - ENHANCEMENT - implement status conditions on GitOpsDeploymentSyncRun
				log.Error(err, "handleSyncRunModified error")
				return false, err

			}

			// If there was a generic error in retrieving the key, return it
			log.Error(err, "unable to retrieve gitopsdeployment referenced in syncrun")
			return false, err
		}

		// The GitopsDepl CR exists, so use the UID of the CR to retrieve the database entry, if possible
		deplToAppMapping := &db.DeploymentToApplicationMapping{Deploymenttoapplicationmapping_uid_id: string(gitopsDepl.UID)}

		if err = dbQueries.GetDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping); err != nil {
			log.Error(err, "unable to retrieve deployment to application mapping, on sync run modified", "uid", string(gitopsDepl.UID))
			return false, err
		}

		application = &db.Application{Application_id: deplToAppMapping.Application_id}
		if err := dbQueries.GetApplicationById(ctx, application); err != nil {
			log.Error(err, "unable to retrieve application, on sync run modified", "applicationId", string(deplToAppMapping.Application_id))
			return false, err
		}

		if gitopsEngineInstance, err = a.sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx, application.Engine_instance_inst_id, a.workspaceClient, namespace); err != nil {
			log.Error(err, "unable to retrieve gitopsengineinstance, on sync run modified", "instanceId", string(application.Engine_instance_inst_id))
			return false, err
		}

	}

	if syncRunCRExists && !dbEntryExists {
		// Handle create:
		// If the gitopsdeplsyncrun CR exists, but the database entry doesn't, then this is the first time we
		// have seen the GitOpsDeplSyncRun CR.
		// Create it in the DB and create the operation.

		if application == nil || gitopsEngineInstance == nil {
			err := fmt.Errorf("app or engine instance were nil in handleSyncRunModified app: %v, instance: %v", application, gitopsEngineInstance)
			log.Error(err, "unexpected nil value of required objects")
			return false, err
		}

		// createdResources is a list of database entries created in this function; if an error occurs, we delete them
		// in reverse order.
		var createdResources []db.AppScopedDisposableResource

		// Create sync operation
		syncOperation := &db.SyncOperation{
			Application_id:      application.Application_id,
			Operation_id:        "delme", // TODO: GITOPSRVCE-67 - DEBT - This field can probably be removed from the database
			DeploymentNameField: syncRunCR.Spec.GitopsDeploymentName,
			Revision:            syncRunCR.Spec.RevisionID,
			DesiredState:        db.SyncOperation_DesiredState_Running,
		}
		if err := dbQueries.CreateSyncOperation(ctx, syncOperation); err != nil {
			log.Error(err, "unable to create sync operation in database")
			return false, err
		}
		createdResources = append(createdResources, syncOperation)
		log.Info("Created a Sync Operation: " + syncOperation.SyncOperation_id)

		newApiCRToDBMapping := db.APICRToDatabaseMapping{
			APIResourceType: db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentSyncRun,
			APIResourceUID:  string(syncRunCR.UID),
			DBRelationType:  db.APICRToDatabaseMapping_DBRelationType_SyncOperation,
			DBRelationKey:   syncOperation.SyncOperation_id,

			APIResourceName:      syncRunCR.Name,
			APIResourceNamespace: syncRunCR.Namespace,
			WorkspaceUID:         eventlooptypes.GetWorkspaceIDFromNamespaceID(namespace),
		}
		if err := dbQueries.CreateAPICRToDatabaseMapping(ctx, &newApiCRToDBMapping); err != nil {
			log.Error(err, "unable to create api to db mapping in database")

			// If we were unable to retrieve the client, delete the resources we created in the previous steps
			dbutil.DisposeApplicationScopedResources(ctx, createdResources, dbQueries, log)

			return false, err
		}
		log.Info(fmt.Sprintf("Created a ApiCRToDBMapping: (APIResourceType: %s, APIResourceUID: %s, DBRelationType: %s)", newApiCRToDBMapping.APIResourceType, newApiCRToDBMapping.APIResourceUID, newApiCRToDBMapping.DBRelationType))
		createdResources = append(createdResources, &newApiCRToDBMapping)

		operationClient, err := a.getK8sClientForGitOpsEngineInstance(gitopsEngineInstance)
		if err != nil {
			log.Error(err, "unable to retrieve gitopsengine instance from handleSyncRunModified")

			// If we were unable to retrieve the client, delete the resources we created in the previous steps
			dbutil.DisposeApplicationScopedResources(ctx, createdResources, dbQueries, log)

			// Return the original error
			return false, err
		}

		dbOperationInput := db.Operation{
			Instance_id:   gitopsEngineInstance.Gitopsengineinstance_id,
			Resource_id:   syncOperation.SyncOperation_id,
			Resource_type: db.OperationResourceType_SyncOperation,
		}

		k8sOperation, dbOperation, err := createOperation(ctx, false && !a.testOnlySkipCreateOperation, dbOperationInput, clusterUser.Clusteruser_id,
			dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, operationClient, log)
		if err != nil {
			log.Error(err, "could not create operation", "namespace", dbutil.GetGitOpsEngineSingleInstanceNamespace())

			// If we were unable to create the operation, delete the resources we created in the previous steps
			dbutil.DisposeApplicationScopedResources(ctx, createdResources, dbQueries, log)

			return false, err
		}

		// TODO: GITOPSRVCE-82 - STUB - Remove the 'false' in createOperation above, once cluster agent handling of operation is implemented.
		log.Info("STUB: Not waiting for create Sync Run operation to complete, in handleNewSyncRunModified")

		if err := cleanupOperation(ctx, *dbOperation, *k8sOperation, dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, operationClient, log); err != nil {
			return false, err
		}

		return false, nil
	}

	if !syncRunCRExists && dbEntryExists {
		// Handle delete:
		// If the gitopsdeplsyncrun CR doesn't exist, but database row does, then the CR has been deleted, so handle it.

		// Deleting the CR should terminate a sync operation, if it was previously in progress.

		if len(apiCRToDBList) != 1 {
			err := fmt.Errorf("SEVERE - Update only supports one operation parameter")
			log.Error(err, err.Error())
			return false, err
		}

		// 1) Get the SyncOperation table entry pointed to by the resource
		apiCRToDBMapping := apiCRToDBList[0]

		if apiCRToDBMapping.DBRelationType != db.APICRToDatabaseMapping_DBRelationType_SyncOperation {
			err := fmt.Errorf("SEVERE - db relation type should be syncoperation")
			log.Error(err, err.Error())
			return false, err
		}

		syncOperation := db.SyncOperation{SyncOperation_id: apiCRToDBMapping.DBRelationKey}

		if err := dbQueries.GetSyncOperationById(ctx, &syncOperation); err != nil {
			log.Error(err, "unable to retrieve sync operation by id on deleted", "operationID", syncOperation.SyncOperation_id)
			return false, err
		}

		// 2) Update the state of the SyncOperation DB table to say that we want to terminate it, if it is runing
		syncOperation.DesiredState = db.SyncOperation_DesiredState_Terminated

		dbOperationInput := db.Operation{
			Instance_id:   gitopsEngineInstance.Gitopsengineinstance_id,
			Resource_id:   syncOperation.SyncOperation_id,
			Resource_type: db.OperationResourceType_SyncOperation,
		}

		// 3) Create the operation, in order to inform the cluster agent it needs to cancel the sync operation
		operationClient, err := a.getK8sClientForGitOpsEngineInstance(gitopsEngineInstance)
		if err != nil {
			log.Error(err, "unable to retrieve gitopsengine instance from handleSyncRunModified, when resource was deleted")
			return false, err
		}

		k8sOperation, dbOperation, err := createOperation(ctx, true && !a.testOnlySkipCreateOperation, dbOperationInput, clusterUser.Clusteruser_id,
			dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, operationClient, log)
		if err != nil {
			log.Error(err, "could not create operation, when resource was deleted", "namespace", dbutil.GetGitOpsEngineSingleInstanceNamespace())
			return false, err
		}

		// 4) Clean up the operation and database table entries
		if err := cleanupOperation(ctx, *dbOperation, *k8sOperation, dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, operationClient, log); err != nil {
			return false, err
		}

		// TODO: GITOPSRVCE-82 - STUB - need to implement support for sync operation in cluster agent
		log.Info("STUB: need to implement sync on cluster side")

		if _, err := dbQueries.DeleteSyncOperationById(ctx, syncOperation.SyncOperation_id); err != nil {
			log.Error(err, "could not delete sync operation, when resource was deleted", "namespace", dbutil.GetGitOpsEngineSingleInstanceNamespace())
			return false, err
		} else {
			log.Info("Sync Operation deleted with ID: ", syncOperation.SyncOperation_id)
		}

		var allErrors error

		// Remove the mappings
		for idx := range apiCRToDBList {

			apiCRToDB := apiCRToDBList[idx]

			err := a.cleanupOldSyncDBEntry(ctx, &apiCRToDB, *clusterUser, dbQueries)
			if err != nil {
				if allErrors == nil {
					allErrors = err
				} else {
					allErrors = fmt.Errorf("error: %v error: %v", err, allErrors)
				}
			}
		}

		if allErrors != nil {
			return false, allErrors
		}

		// Success: the CR no longer exists, and we have completed cleanup, so signal that the goroutine may be terminated.
		return true, nil
	}

	if syncRunCRExists && dbEntryExists {

		// Sanity checks
		if syncRunCR == nil {
			err := fmt.Errorf("SEVERE - vsync run cr is nil")
			log.Error(err, err.Error())
			return false, err
		}

		if len(apiCRToDBList) != 1 {
			err := fmt.Errorf("SEVERE - Update only supports one operation parameter")
			log.Error(err, err.Error())
			return false, err
		}

		// Get the SyncOperation table entry pointed to by the resource
		apiCRToDBMapping := apiCRToDBList[0]

		if apiCRToDBMapping.DBRelationType != db.APICRToDatabaseMapping_DBRelationType_SyncOperation {
			err := fmt.Errorf("SEVERE - db relation type should be syncoperation")
			log.Error(err, err.Error())
			return false, err
		}

		syncOperation := db.SyncOperation{SyncOperation_id: apiCRToDBMapping.DBRelationKey}

		if err := dbQueries.GetSyncOperationById(ctx, &syncOperation); err != nil {

			log.Error(err, "unable to retrieve sync operation by id on modified", "operationID", syncOperation.SyncOperation_id)
			return false, err
		}

		if syncOperation.DeploymentNameField != syncRunCR.Spec.GitopsDeploymentName {
			err := fmt.Errorf("deployment name field is immutable: changing it from its initial value is not supported")
			log.Error(err, "deployment name field change is not supported")
			return false, err
		}

		if syncOperation.Revision != syncRunCR.Spec.RevisionID {
			err := fmt.Errorf("revision change is not supported: changing it from its initial value is not supported")
			log.Error(err, "revision field change is not supported")
			return false, err
		}

		// TODO: GITOPSRVCE-67 - DEBT - Include test case to check that the various goroutines are terminated when the CR is deleted.

		return false, nil
	}

	return false, nil

}

func (a *applicationEventLoopRunner_Action) cleanupOldSyncDBEntry(ctx context.Context, apiCRToDB *db.APICRToDatabaseMapping,
	clusterUser db.ClusterUser, dbQueries db.ApplicationScopedQueries) error {

	log := a.log

	if apiCRToDB.DBRelationType != db.APICRToDatabaseMapping_DBRelationType_SyncOperation {
		err := fmt.Errorf("SEVERE: unexpected DBRelationKey, should be SyncOperation")
		log.Error(err, err.Error())
		return err
	}

	rowsDeleted, err := dbQueries.DeleteSyncOperationById(ctx, apiCRToDB.DBRelationKey)
	if err != nil {
		log.Error(err, "unable to delete sync operation db entry on sync operation delete", "key", apiCRToDB.DBRelationKey)
		return err
	} else if rowsDeleted == 0 {
		log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected number of rows deleted on sync db entry delete", "key", apiCRToDB.DBRelationKey)
	} else {
		log.Info("Sync Operation deleted with ID: " + apiCRToDB.DBRelationKey)
	}

	var operations []db.Operation
	if err := dbQueries.ListOperationsByResourceIdAndTypeAndOwnerId(ctx, apiCRToDB.DBRelationKey, apiCRToDB.DBRelationType, &operations, clusterUser.Clusteruser_id); err != nil {
		log.Error(err, "unable to retrieve operations pointing to sync operation", "key", apiCRToDB.DBRelationKey)
		return err
	} else {
		// Delete the operations that reference this SyncOperation
		for idx := range operations {
			operationId := operations[idx].Operation_id

			log := log.WithValues("operationId", operationId)

			rowsDeleted, err := dbQueries.CheckedDeleteOperationById(ctx, operationId, clusterUser.Clusteruser_id)
			if err != nil {
				log.Error(err, "unable to delete old operation")
				return err
			} else if rowsDeleted == 0 {
				log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected number of deleted rows when deleting old operation")
			} else {
				log.Info("Operation deleted with ID: " + operationId)
			}
		}
	}

	rowsDeleted, err = dbQueries.DeleteAPICRToDatabaseMapping(ctx, apiCRToDB)
	if err != nil {
		log.Error(err, "unable to delete apiCRToDBmapping", "mapping", apiCRToDB.APIResourceUID)
		return err

	} else if rowsDeleted == 0 {
		log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected number of rows deleted of apiCRToDBmapping", "mapping", apiCRToDB.APIResourceUID)
	} else {
		log.Info("Deleted APICRToDatabaseMapping")
	}

	return nil
}
