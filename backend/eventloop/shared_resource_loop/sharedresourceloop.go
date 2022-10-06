package shared_resource_loop

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/operations"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The goal of the shared resource event loop is to ensure that API-namespace-scoped resources are only
// created from a single thread, preventing concurrent goroutines from stepping on each others toes.
//
// This ensures that:
//   - When multiple 'application event loop' goroutines are attempting to create workspace-scoped resources,
//     that no duplicates are created (eg it shouldn't be possible to create multiple ClusterUsers for a single user, or multiple
//     ManagedEnvironments for a single workspace)
//   - There are no race conditions on creation of workspace-scoped resources.
//
// API-namespace-scoped resources are:
// - managedenv
// - clusteraccess
// - clusteruser
// - gitopsengineinstance
// - repositorycredential
//
// Ultimately the goal of this file is to avoid this issue:
// - In the same moment of time, both these actions happen simultaneously:
//   - thread 1: creates (for example) a managed environment DB row for environment A, while processing a GitOpsDeployment targeting A
//   - thread 2: creates (for example) a managed environment DB row for environment A, while processing a different GitOpsDeployment targeting A
//   - But this is bad: the database now contains _two different_ managed environment database entries for the same environment A.
//   - Thus, without mutexes/locking, there is a race condition.
//   - However, the shared resource event loop prevents this issue, by ensuring that threads are never able to
//     concurrently create API-namespace-scoped database resources at the same time.
type SharedResourceEventLoop struct {
	inputChannel chan sharedResourceLoopMessage
}

// The bool return value is 'true' if ClusterUser is created; 'false' if it already exists in DB or in case of failure.
func (srEventLoop *SharedResourceEventLoop) GetOrCreateClusterUserByNamespaceUID(ctx context.Context, workspaceClient client.Client,
	workspaceNamespace corev1.Namespace, log logr.Logger) (*db.ClusterUser, bool, error) {

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		log:                log,
		workspaceClient:    workspaceClient,
		workspaceNamespace: workspaceNamespace,
		messageType:        sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUID,
		responseChannel:    responseChannel,
		ctx:                ctx,
	}

	srEventLoop.inputChannel <- msg

	var rawResponse any

	select {
	case rawResponse = <-responseChannel:
	case <-ctx.Done():
		return nil, false, fmt.Errorf("context cancelled in getOrCreateClusterUserByNamespaceUID")
	}

	response, ok := rawResponse.(sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDResponse)
	if !ok {
		return nil, false, fmt.Errorf("SEVERE: unexpected response type")
	}

	return response.clusterUser, response.isNewUser, response.err

}

func (srEventLoop *SharedResourceEventLoop) GetGitopsEngineInstanceById(ctx context.Context, id string, workspaceClient client.Client,
	workspaceNamespace corev1.Namespace, log logr.Logger) (*db.GitopsEngineInstance, error) {

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		log:                log,
		workspaceClient:    workspaceClient,
		workspaceNamespace: workspaceNamespace,
		messageType:        sharedResourceLoopMessage_getGitopsEngineInstanceById,
		responseChannel:    responseChannel,
		payload: sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDRequest{
			gitopsEngineInstanceID: id,
		},
		ctx: ctx,
	}

	srEventLoop.inputChannel <- msg

	var rawResponse any

	select {
	case rawResponse = <-responseChannel:
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled in getGitOpsEngineInstanceById")
	}

	response, ok := rawResponse.(sharedResourceLoopMessage_getGitopsEngineInstanceByIdResponse)
	if !ok {
		return nil, fmt.Errorf("SEVERE: unexpected response type")
	}

	return response.gitopsEngineInstance, response.err

}

// Ensure the user's workspace is configured, ensure a GitOpsEngineInstance exists that will target it, and ensure
// a cluster access exists the give the user permission to target them from the engine.
func (srEventLoop *SharedResourceEventLoop) ReconcileSharedManagedEnv(ctx context.Context,
	workspaceClient client.Client, workspaceNamespace corev1.Namespace,
	managedEnvironmentCRName string, managedEnvironmentCRNamespace string, isWorkspaceTarget bool,
	k8sClientFactory SRLK8sClientFactory, l logr.Logger) (SharedResourceManagedEnvContainer, error) {

	res := newSharedResourceManagedEnvContainer()

	if !isWorkspaceTarget && (managedEnvironmentCRName == "" || managedEnvironmentCRNamespace == "") {
		// Sanity test the parameters
		return res, fmt.Errorf("managed environment name or namespace were empty")
	}

	request := sharedResourceLoopMessage_getOrCreateSharedResourceManagedEnvRequest{
		managedEnvironmentCRName:      managedEnvironmentCRName,
		managedEnvironmentCRNamespace: managedEnvironmentCRNamespace,
		isWorkspaceTarget:             isWorkspaceTarget,
		k8sClientFactory:              k8sClientFactory,
	}

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		log:                l,
		ctx:                ctx,
		workspaceClient:    workspaceClient,
		workspaceNamespace: workspaceNamespace,
		messageType:        sharedResourceLoopMessage_getOrCreateSharedManagedEnv,
		responseChannel:    responseChannel,
		payload:            request,
	}

	srEventLoop.inputChannel <- msg

	var rawResponse any

	select {
	case rawResponse = <-responseChannel:
	case <-ctx.Done():
		return res, fmt.Errorf("context cancelled in GetOrCreateSharedManagedEnv")
	}

	response, ok := rawResponse.(sharedResourceLoopMessage_getOrCreateSharedResourcesResponse)
	if !ok {
		return res, fmt.Errorf("SEVERE: unexpected response type")
	}
	res = response.responseContainer

	return res, response.err

}

func (srEventLoop *SharedResourceEventLoop) ReconcileRepositoryCredential(ctx context.Context,
	workspaceClient client.Client, workspaceNamespace corev1.Namespace,
	repositoryCredentialCRName string, k8sClientFactory SRLK8sClientFactory) (*db.RepositoryCredentials, error) {

	request := sharedResourceLoopMessage_reconcileRepositoryCredentialRequest{
		repositoryCredentialCRName:      repositoryCredentialCRName,
		repositoryCredentialCRNamespace: workspaceNamespace.Name,
		k8sClientFactory:                k8sClientFactory,
	}

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		workspaceClient:    workspaceClient,
		workspaceNamespace: workspaceNamespace,
		messageType:        sharedResourceLoopMessage_reconcileRepositoryCredential,
		responseChannel:    responseChannel,
		payload:            request,
	}

	srEventLoop.inputChannel <- msg

	var rawResponse any

	select {
	case rawResponse = <-responseChannel:
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled in ReconcileRepositoryCredential")
	}

	response, ok := rawResponse.(sharedResourceLoopMessage_reconcileRepositoryCredentialResponse)
	if !ok {
		return nil, fmt.Errorf("SEVERE: unexpected response type")
	}
	return response.repositoryCredential, response.err

}

func NewSharedResourceLoop() *SharedResourceEventLoop {

	sharedResourceEventLoop := &SharedResourceEventLoop{
		inputChannel: make(chan sharedResourceLoopMessage),
	}

	go internalSharedResourceEventLoop(sharedResourceEventLoop.inputChannel)

	return sharedResourceEventLoop
}

type sharedResourceLoopMessageType string

const (
	sharedResourceLoopMessage_getOrCreateSharedManagedEnv          sharedResourceLoopMessageType = "getOrCreateSharedManagedEnv"
	sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUID sharedResourceLoopMessageType = "getOrCreateClusterUserByNamespaceUID"
	sharedResourceLoopMessage_getGitopsEngineInstanceById          sharedResourceLoopMessageType = "getGitopsEngineInstanceById"
	sharedResourceLoopMessage_reconcileRepositoryCredential        sharedResourceLoopMessageType = "reconcileRepositoryCredential"
)

type sharedResourceLoopMessage struct {
	log                logr.Logger
	ctx                context.Context
	workspaceClient    client.Client
	workspaceNamespace corev1.Namespace

	messageType     sharedResourceLoopMessageType
	responseChannel chan (any)

	// optional payload
	payload any
}

type sharedResourceLoopMessage_getGitopsEngineInstanceByIdResponse struct {
	err                  error
	gitopsEngineInstance *db.GitopsEngineInstance
}

type sharedResourceLoopMessage_getOrCreateSharedResourceManagedEnvRequest struct {
	managedEnvironmentCRName      string
	managedEnvironmentCRNamespace string
	isWorkspaceTarget             bool
	k8sClientFactory              SRLK8sClientFactory
}

type sharedResourceLoopMessage_getOrCreateSharedResourcesResponse struct {
	err               error
	responseContainer SharedResourceManagedEnvContainer
}

type sharedResourceLoopMessage_reconcileRepositoryCredentialRequest struct {
	repositoryCredentialCRName      string
	repositoryCredentialCRNamespace string
	k8sClientFactory                SRLK8sClientFactory
}

type sharedResourceLoopMessage_reconcileRepositoryCredentialResponse struct {
	err                  error
	repositoryCredential *db.RepositoryCredentials
}

func newSharedResourceManagedEnvContainer() SharedResourceManagedEnvContainer {
	return SharedResourceManagedEnvContainer{
		ClusterUser:          nil,
		IsNewUser:            false,
		ManagedEnv:           nil,
		IsNewManagedEnv:      false,
		GitopsEngineInstance: nil,
		IsNewInstance:        false,
		ClusterAccess:        nil,
		IsNewClusterAccess:   false,
		GitopsEngineCluster:  nil,
	}
}

// SharedResourceManagedEnvContainer is the return value of ReconcileSharedManagedEnv, and contains the
// resources that were created by the reconciliation.
type SharedResourceManagedEnvContainer struct {
	ClusterUser          *db.ClusterUser
	IsNewUser            bool
	ManagedEnv           *db.ManagedEnvironment
	IsNewManagedEnv      bool
	GitopsEngineInstance *db.GitopsEngineInstance
	IsNewInstance        bool
	ClusterAccess        *db.ClusterAccess
	IsNewClusterAccess   bool
	GitopsEngineCluster  *db.GitopsEngineCluster
}

type sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDRequest struct {
	gitopsEngineInstanceID string
}

type sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDResponse struct {
	err         error
	clusterUser *db.ClusterUser
	isNewUser   bool
}

func internalSharedResourceEventLoop(inputChan chan sharedResourceLoopMessage) {

	ctx := context.Background()
	log := log.FromContext(ctx)
	dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
	if err != nil {
		log.Error(err, "SEVERE: internalSharedResourceEventLoop exiting before startup")
		return
	}

	for {
		msg := <-inputChan

		_, err = sharedutil.CatchPanic(func() error {
			processSharedResourceMessage(msg.ctx, msg, dbQueries, msg.log)
			return nil
		})
		if err != nil {
			log.Error(err, "unexpected error from processMessage in internalSharedResourceEventLoop")
		}
	}
}

func processSharedResourceMessage(ctx context.Context, msg sharedResourceLoopMessage, dbQueries db.DatabaseQueries, log logr.Logger) {

	log.V(sharedutil.LogLevel_Debug).Info("sharedResourceEventLoop received message: "+string(msg.messageType),
		"workspace", msg.workspaceNamespace.UID)

	if msg.messageType == sharedResourceLoopMessage_getOrCreateSharedManagedEnv {

		payload, ok := (msg.payload).(sharedResourceLoopMessage_getOrCreateSharedResourceManagedEnvRequest)
		if !ok {
			err := fmt.Errorf("SEVERE: unexpected payload")
			log.Error(err, "")
			// Reply on a separate goroutine so cancelled callers don't block the event loop
			go func() {
				msg.responseChannel <- sharedResourceLoopMessage_getOrCreateSharedResourcesResponse{
					err: err,
				}
			}()

			return
		}

		res, err := internalProcessMessage_ReconcileSharedManagedEnv(ctx, msg.workspaceClient, payload.managedEnvironmentCRName,
			payload.managedEnvironmentCRNamespace, payload.isWorkspaceTarget, msg.workspaceNamespace,
			payload.k8sClientFactory, dbQueries, log)

		response := sharedResourceLoopMessage_getOrCreateSharedResourcesResponse{
			err:               err,
			responseContainer: res,
		}

		// Reply on a separate goroutine so cancelled callers don't block the event loop
		go func() {
			msg.responseChannel <- response
		}()

	} else if msg.messageType == sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUID {

		clusterUser, isNewUser, err := internalProcessMessage_GetOrCreateClusterUserByNamespaceUID(ctx, msg.workspaceNamespace, dbQueries, log)

		response := sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDResponse{
			err:         err,
			clusterUser: clusterUser,
			isNewUser:   isNewUser,
		}

		// Reply on a separate goroutine so cancelled callers don't block the event loop
		go func() {
			msg.responseChannel <- response
		}()

	} else if msg.messageType == sharedResourceLoopMessage_getGitopsEngineInstanceById {

		var gitopsEngineInstance *db.GitopsEngineInstance
		var err error

		payload, ok := (msg.payload).(sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUIDRequest)
		if ok {
			gitopsEngineInstance, err = internalProcessMessage_GetGitopsEngineInstanceById(ctx, payload.gitopsEngineInstanceID, dbQueries)
		} else {
			err = fmt.Errorf("SEVERE - unexpected cast in internalSharedResourceEventLoop")
			log.Error(err, err.Error())
		}

		response := sharedResourceLoopMessage_getGitopsEngineInstanceByIdResponse{
			err:                  err,
			gitopsEngineInstance: gitopsEngineInstance,
		}

		// Reply on a separate goroutine so cancelled callers don't block the event loop
		go func() {
			msg.responseChannel <- response
		}()
	} else if msg.messageType == sharedResourceLoopMessage_reconcileRepositoryCredential {

		var err error
		var repositoryCredential *db.RepositoryCredentials

		payload, ok := (msg.payload).(sharedResourceLoopMessage_reconcileRepositoryCredentialRequest)
		if ok {

			repositoryCredential, err = internalProcessMessage_ReconcileRepositoryCredential(ctx,
				payload.repositoryCredentialCRName, msg.workspaceNamespace, msg.workspaceClient, payload.k8sClientFactory, dbQueries, log)

		} else {
			err = fmt.Errorf("SEVERE - unexpected cast in internalSharedResourceEventLoop")
			log.Error(err, err.Error())
		}

		response := sharedResourceLoopMessage_reconcileRepositoryCredentialResponse{
			repositoryCredential: repositoryCredential,
			err:                  err,
		}

		// Reply on a separate goroutine so cancelled callers don't block the event loop
		go func() {
			msg.responseChannel <- response
		}()

	} else {
		log.Error(nil, "SEVERE: unrecognized sharedResourceLoopMessageType: "+string(msg.messageType))
	}

}

func deleteRepositoryCredentialLeftovers(ctx context.Context, apiNamespaceClient client.Client, clusterUser db.ClusterUser, repositoryCredentialCRName string, repositoryCredentialCRNamespace corev1.Namespace, dbQueries db.DatabaseQueries, l logr.Logger) (bool, error) {

	ns := repositoryCredentialCRNamespace.Name

	// Find any existing database resources for repository credentials that previously existing with this namespace/name
	var repositoryCredentialsList []db.APICRToDatabaseMapping
	if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
		repositoryCredentialCRName, ns, string(repositoryCredentialCRNamespace.GetUID()),
		db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential, &repositoryCredentialsList); err != nil {

		return false, fmt.Errorf("unable to list APICRs for repository credentials: %v", err)
	}

	if len(repositoryCredentialsList) > 0 { // if there are any Repository Credentials in the database (leftovers)
		l.V(sharedutil.LogLevel_Debug).Info("Deleting Repository Credentials from the database ...")

		// ----
		for _, apiCRToDBMapping := range repositoryCredentialsList {

			// Fetch the repocred from the database
			dbRepoCred, reconciledResult, err := getDBCred(ctx, dbQueries, apiCRToDBMapping, l)
			if err != nil {
				return reconciledResult, err
			}

			// Delete the repocred from the database
			if reconciledResult, err := deleteRepoCredFromDB(ctx, dbQueries, apiCRToDBMapping, l); err != nil {
				return reconciledResult, err
			}

			// Create an operation to delete the repocred from the cluster
			err = createRepoCredOperation(ctx, l, *dbRepoCred, &clusterUser, ns, dbQueries, apiNamespaceClient)
			if err != nil {
				return false, err
			}

			// Delete the repocred APICRToDatabaseMapping from the shared resource loop
			deleteAPICRToDatabaseMapping(ctx, repositoryCredentialsList, dbQueries, l)
		}
		// ---

		//for _, item := range repositoryCredentialsList {
		//	fetchRow := db.APICRToDatabaseMapping{
		//		APIResourceType: item.APIResourceType,
		//		APIResourceUID:  item.APIResourceUID,
		//		DBRelationKey:   item.DBRelationKey,
		//		DBRelationType:  item.DBRelationType,
		//	}
		//
		//	if err := dbQueries.GetDatabaseMappingForAPICR(ctx, &fetchRow); err != nil {
		//		l.Error(err, "unable to fetch database mapping for APICR")
		//		continue
		//	}
		//
		//	// 2b) Delete the corresponding database resources
		//	rowsAffected, err := dbQueries.DeleteRepositoryCredentialsByID(ctx, fetchRow.DBRelationKey)
		//	if err != nil {
		//		// Warn, but continue
		//		l.V(sharedutil.LogLevel_Warn).Info("Unable to delete RepositoryCredentials DB Row", "item", item.APIResourceUID)
		//	}
		//	if rowsAffected != 1 {
		//		// Warn, but continue.
		//		l.V(sharedutil.LogLevel_Warn).Info("unexpected number of rows deleted for RepositoryCredentials", "mapping", item.APIResourceUID)
		//	}

		// repositoryCredentialPrimaryKey := item.DBRelationKey

		//TODO: GITOPSRVCE-96: STUB: Next steps:
		//- Create the operation row, pointing to the deleted repository credentials table

	} else {
		l.V(sharedutil.LogLevel_Debug).Info("No APICRToDatabaseMapping found for the deleted GitOpsDeploymentRepositoryCredential CR")
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

const (
	errCRNotFound       = "CR no longer exists in the cluster"
	errGenericCR        = "unable to retrieve CR from the cluster"
	errBug              = "unexpected error"
	errUpdateDBRepoCred = "unable to update repository credential in the database"
	errCreateDBRepoCred = "unable to create repository credential in the database"
)

func internalProcessMessage_ReconcileRepositoryCredential(ctx context.Context,
	repositoryCredentialCRName string,
	repositoryCredentialCRNamespace corev1.Namespace,
	apiNamespaceClient client.Client,
	k8sClientFactory SRLK8sClientFactory,
	dbQueries db.DatabaseQueries, l logr.Logger) (*db.RepositoryCredentials, error) {

	ns := repositoryCredentialCRNamespace.Name
	cr := repositoryCredentialCRName

	// ----
	clusterUser, _, err := internalGetOrCreateClusterUserByNamespaceUID(ctx, string(repositoryCredentialCRNamespace.UID), dbQueries, l)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve cluster user in processMessage, '%s': %v", string(repositoryCredentialCRNamespace.UID), err)
	}
	// ----
	gitopsEngineInstance, _, _, err := internalDetermineGitOpsEngineInstanceForNewApplication(ctx, *clusterUser, apiNamespaceClient, dbQueries, l)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve gitops engine instance")
	}
	// ----

	// Allocate a struct pointer in memory for receiving the kubernetes CR object
	repoCreds := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{}

	// 2) Attempt to get the CR
	if err := apiNamespaceClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cr}, repoCreds); err != nil {

		// If the CR is not found, then we can assume it was deleted
		if apierr.IsNotFound(err) {
			if repoCreds == nil {
				// Delete the related leftovers in the database
				l.Error(err, errCRNotFound, "CR Name:", cr, "Namespace", ns)
				_, err = deleteRepositoryCredentialLeftovers(ctx, apiNamespaceClient, *clusterUser, cr, repositoryCredentialCRNamespace, dbQueries, l)
				if err != nil {
					return nil, err
				}
			} else {
				l.Error(err, errBug, "CR Name:", cr, "Namespace", ns)
				return nil, nil
			}
		}

		// Something went wrong, retry
		l.Error(err, errGenericCR, "CR Name", cr, "Namespace", ns)
		return nil, fmt.Errorf("unexpected error in retrieving repo credentials: %v", err)
	}

	// 3. If CR exists in the cluster, check the DB to see if it exists there

	var privateURL, authUsername, authPassword, authSSHKey, secretObj string
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repoCreds.Spec.Secret,
			Namespace: ns, // we assume the secret is in the same namespace as the CR
		},
	}

	if err := apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		if apierr.IsNotFound(err) {
			l.Info("secret not found", "secret", repoCreds.Spec.Secret, "namespace", ns)
			return nil, fmt.Errorf("secret not found: %v", err)
		} else {
			// Something went wrong, retry
			l.Error(err, "error retrieving secret", "secret", repoCreds.Spec.Secret, "namespace", ns)
			return nil, fmt.Errorf("error retrieving secret: %v", err)
		}
	} else {
		// Secret exists, so get its data
		privateURL = string(secret.Data["url"]) // is this supposed to the same as the CR's Repository?
		authUsername = string(secret.Data["username"])
		authPassword = string(secret.Data["password"])
		authSSHKey = string(secret.Data["sshPrivateKey"])
		secretObj = secret.Name
	}

	dbRepoCred, err := dbQueries.GetRepositoryCredentialsByID(ctx, string(repoCreds.UID))
	if err != nil {

		if db.IsResultNotFoundError(err) {
			// If the CR exists in the cluster but not in the DB, then create it in the DB and create an Operation.
			err = dbQueries.CreateRepositoryCredentials(ctx, &db.RepositoryCredentials{
				RepositoryCredentialsID: string(repoCreds.UID),
				UserID:                  clusterUser.Clusteruser_id, // comply with the constraint 'fk_clusteruser_id'
				PrivateURL:              privateURL,                 // or repoCreds.Spec.Repository?
				AuthUsername:            authUsername,
				AuthPassword:            authPassword,
				AuthSSHKey:              authSSHKey,
				SecretObj:               secretObj,
				EngineClusterID:         gitopsEngineInstance.Gitopsengineinstance_id, // comply with the constraint 'fk_gitopsengineinstance_id',
			})
			if err != nil {
				l.Error(err, errCreateDBRepoCred, "CR Name", cr, "Namespace", ns)
				return nil, fmt.Errorf("unable to create repository credential in the database: %v", err)
			}

			err = createRepoCredOperation(ctx, l, dbRepoCred, clusterUser, ns, dbQueries, apiNamespaceClient)
			if err != nil {
				return nil, err
			}
		} else {
			l.Error(err, "error retrieving repository credentials from DB", "repositoryCredentialsID", repoCreds.UID)
			return nil, fmt.Errorf("error retrieving repository credentials from DB: %v", err)
		}
	} else {
		// If the CR exists in the cluster and in the DB, then check if the data is the same and create an Operation
		isUpdateNeeded := compareClusterResourceWithDatabaseRow(*repoCreds, &dbRepoCred, l)
		if isUpdateNeeded {
			l.Info("Syncing with database...")
			if err = dbQueries.UpdateRepositoryCredentials(ctx, &dbRepoCred); err != nil {
				l.Error(err, errUpdateDBRepoCred)
				return nil, err
			}

			err = createRepoCredOperation(ctx, l, dbRepoCred, clusterUser, ns, dbQueries, apiNamespaceClient)
			if err != nil {
				return nil, err
			}
		}
	}

	//// This is a best effort operation, so we don't return an error if it fails.
	//
	//// 2a) Find any existing database resources for repository credentials that previously existing with this namespace/name
	//var apiCRToDBMappingList []db.APICRToDatabaseMapping
	//if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
	//	cr, ns, string(repositoryCredentialCRNamespace.GetUID()),
	//	db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential, &apiCRToDBMappingList); err != nil {
	//
	//	return nil, fmt.Errorf("unable to list APICRs for repository credentials: %v", err)
	//}
	//
	//for _, item := range apiCRToDBMappingList {
	//
	//	fmt.Println("Type", item.APIResourceType)
	//	fmt.Println("UID", item.APIResourceUID)
	//	fmt.Println("Namespace", item.APIResourceNamespace)
	//	fmt.Println("Name", item.APIResourceName)
	//	fmt.Println("DB Relation Type", item.DBRelationType)
	//	fmt.Println("DB Relation Key", item.DBRelationKey)
	//
	//	fetchRow := db.APICRToDatabaseMapping{
	//		APIResourceType: item.APIResourceType,
	//		APIResourceUID:  item.APIResourceUID,
	//		DBRelationKey:   item.DBRelationKey,
	//		DBRelationType:  item.DBRelationType,
	//	}
	//
	//	if err = dbQueries.GetDatabaseMappingForAPICR(ctx, &fetchRow); err != nil {
	//		log.Error(err, "unable to fetch database mapping for APICR")
	//		continue
	//	}
	//
	//	// 2b) Delete the corresponding database resources
	//	rowsAffected, err := dbQueries.DeleteRepositoryCredentialsByID(ctx, fetchRow.DBRelationKey)
	//	if err != nil {
	//		// Warn, but continue
	//		log.V(sharedutil.LogLevel_Warn).Info("Unable to delete RepositoryCredentials DB Row", "item", item.APIResourceUID)
	//	}
	//	if rowsAffected != 1 {
	//		// Warn, but continue.
	//		log.V(sharedutil.LogLevel_Warn).Info("unexpected number of rows deleted for RepositoryCredentials", "mapping", item.APIResourceUID)
	//	}
	//
	//	// repositoryCredentialPrimaryKey := item.DBRelationKey
	//
	//	fmt.Println("STUB:", item)

	// TODO: GITOPSRVCE-96: STUB: Next steps:
	// - Delete the repository credential from the database
	// - Create the operation row, pointing to the deleted repository credentials table
	// - Delete the APICRToDatabaseMapping referenced by 'item'

	// Success!
	// return nil, nil

	// }

	// TODO: GITOPSRVCE-96: We should probably rename this to internalDetermineGitOpsEngineInstance, given how it is being used :P

	// TODO: GITOPSRVCE-96: STUB:
	// - If the GitOpsDeploymentRepositoryCredential does exist, but not in the DB, then create a RepositoryCredential DB entry
	// - If the GitOpsDeploymentRepositoryCredential does exist, and also in the DB, then compare and change the RepositoryCredential DB entry
	// Then, in both cases, create an Operation to update the cluster-agent

	return nil, fmt.Errorf("STUB!")

}

func createRepoCredOperation(ctx context.Context, l logr.Logger, dbRepoCred db.RepositoryCredentials, clusterUser *db.ClusterUser, ns string, dbQueries db.DatabaseQueries, apiNamespaceClient client.Client) error {
	l.Info("Creating operation...")
	dbOperationInput := db.Operation{
		Instance_id:             dbRepoCred.EngineClusterID,
		Resource_id:             dbRepoCred.RepositoryCredentialsID,
		Resource_type:           db.OperationResourceType_RepositoryCredentials,
		State:                   db.OperationState_Waiting,
		Operation_owner_user_id: clusterUser.Clusteruser_id,
	}

	operationCR, operationDB, err := operations.CreateOperation(ctx, false, dbOperationInput, clusterUser.Clusteruser_id, ns, dbQueries, apiNamespaceClient, l)
	if err != nil {
		return fmt.Errorf("unable to create operation: %v", err)
	}

	l.Info("operationCR", "operationCR", operationCR)
	l.Info("operationDB", "operationDB", operationDB)

	return nil
}

func compareClusterResourceWithDatabaseRow(cr managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential, dbr *db.RepositoryCredentials, l logr.Logger) bool {
	var isSecretUpdateNeeded bool
	if cr.Spec.Secret != dbr.SecretObj {
		l.Info("Secret name changed", "old", dbr.SecretObj, "new", cr.Spec.Secret)
		dbr.SecretObj = cr.Spec.Secret
		isSecretUpdateNeeded = true
	}

	var isRepoUpdateNeeded bool
	if cr.Spec.Repository != dbr.PrivateURL {
		l.Info("Repository URL changed", "old", dbr.PrivateURL, "new", cr.Spec.Repository)
		dbr.PrivateURL = cr.Spec.Repository
		isSecretUpdateNeeded = true
	}

	return isSecretUpdateNeeded || isRepoUpdateNeeded
}

//func getRepoCredFromDB(ctx context.Context, dbQueries db.DatabaseQueries, req ctrl.Request, namespace *corev1.Namespace) ([]db.APICRToDatabaseMapping, bool, error) {
//	const retry, noRetry = true, false
//
//	var apiCRToDBMappingList []db.APICRToDatabaseMapping
//	if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
//		req.Name, req.Namespace, string(namespace.GetUID()), db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential, &apiCRToDBMappingList); err != nil {
//
//		// We don't know wha t happened, so we'll retry
//		return nil, retry, fmt.Errorf("unable to list API CRs for repository credentials: %v", err)
//	}
//
//	// Return the list of repository credentials
//	return apiCRToDBMappingList, noRetry, nil
//}

func internalProcessMessage_GetGitopsEngineInstanceById(ctx context.Context, id string, dbq db.DatabaseQueries) (*db.GitopsEngineInstance, error) {

	gitopsEngineInstance := db.GitopsEngineInstance{
		Gitopsengineinstance_id: id,
	}

	err := dbq.GetGitopsEngineInstanceById(ctx, &gitopsEngineInstance)
	return &gitopsEngineInstance, err
}

// The bool return value is 'true' if ClusterUser is created; 'false' if it already exists in DB or in case of failure.
func internalProcessMessage_GetOrCreateClusterUserByNamespaceUID(ctx context.Context, workspaceNamespace corev1.Namespace,
	dbq db.DatabaseQueries, log logr.Logger) (*db.ClusterUser, bool, error) {
	isNewUser := false

	// TODO: GITOPSRVCE-19 - KCP support: for now, we assume that the namespace UID that the request occurred in is the user id.
	clusterUser := db.ClusterUser{User_name: string(workspaceNamespace.UID)}

	// TODO: GITOPSRVCE-41 - We are assuming that user namespace uid == username, which is messy. We should add a new field for unique user id, and username should be human readable and not used for security, etc.
	err := dbq.GetClusterUserByUsername(ctx, &clusterUser)
	if err != nil {
		if db.IsResultNotFoundError(err) {
			isNewUser = true

			if err := dbq.CreateClusterUser(ctx, &clusterUser); err != nil {
				log.Error(err, "Unable to create ClusterUser with User ID: "+clusterUser.Clusteruser_id, clusterUser.GetAsLogKeyValues()...)
				return nil, false, err
			}
			log.Info("Created Cluster User with User ID: "+clusterUser.Clusteruser_id, clusterUser.GetAsLogKeyValues()...)

		} else {
			return nil, false, err
		}
	}

	return &clusterUser, isNewUser, nil
}

const (
	serviceAccountNamespaceKubeSystem = "kube-system"
)

// Ensure the user's workspace is configured, ensure a GitOpsEngineInstance exists that will target it, and ensure
// a cluster access exists the give the user permission to target them from the engine.
// The bool return value is 'true' if respective resource is created; 'false' if it already exists in DB or in case of failure.
func internalProcessMessage_GetOrCreateSharedResources(ctx context.Context, workspaceClient client.Client,
	workspaceNamespace corev1.Namespace, dbQueries db.DatabaseQueries,
	log logr.Logger) (SharedResourceManagedEnvContainer, error) {

	clusterUser, isNewUser, err := internalGetOrCreateClusterUserByNamespaceUID(ctx, string(workspaceNamespace.UID), dbQueries, log)
	if err != nil {
		return SharedResourceManagedEnvContainer{},
			fmt.Errorf("unable to retrieve cluster user in processMessage, '%s': %v", string(workspaceNamespace.UID), err)
	}

	managedEnv, isNewManagedEnv, err := dbutil.GetOrCreateManagedEnvironmentByNamespaceUID(ctx, workspaceNamespace, dbQueries, log)
	if err != nil {
		return SharedResourceManagedEnvContainer{},
			fmt.Errorf("unable to get or created managed env on deployment modified event: %v", err)
	}

	engineInstance, isNewInstance, gitopsEngineCluster, err := internalDetermineGitOpsEngineInstanceForNewApplication(ctx, *clusterUser, workspaceClient, dbQueries, log)
	if err != nil {
		return SharedResourceManagedEnvContainer{}, fmt.Errorf("unable to determine gitops engine instance: %v", err)
	}

	// Create the cluster access object, to allow us to interact with the GitOpsEngine and ManagedEnvironment on the user's behalf
	ca := db.ClusterAccess{
		Clusteraccess_user_id:                   clusterUser.Clusteruser_id,
		Clusteraccess_managed_environment_id:    managedEnv.Managedenvironment_id,
		Clusteraccess_gitops_engine_instance_id: engineInstance.Gitopsengineinstance_id,
	}

	err, isNewClusterAccess := internalGetOrCreateClusterAccess(ctx, &ca, dbQueries, log)
	if err != nil {
		return SharedResourceManagedEnvContainer{}, fmt.Errorf("unable to create cluster access: %v", err)
	}

	return SharedResourceManagedEnvContainer{
		ClusterUser:          clusterUser,
		IsNewUser:            isNewUser,
		ManagedEnv:           managedEnv,
		IsNewManagedEnv:      isNewManagedEnv,
		GitopsEngineInstance: engineInstance,
		IsNewInstance:        isNewInstance,
		ClusterAccess:        &ca,
		IsNewClusterAccess:   isNewClusterAccess,
		GitopsEngineCluster:  gitopsEngineCluster,
	}, nil

}

// Whenever a new Argo CD Application needs to be created, we need to find an Argo CD instance
// that is available to use it. In the future, when we have multiple instances, there would
// be an algorithm that intelligently places applications -> instances, to ensure that
// there are no Argo CD instances that are overloaded (have too many users).
//
// However, at the moment we are using a single shared Argo CD instnace, so we will
// just return that.
//
// The bool return value is 'true' if GitOpsEngineInstance is created; 'false' if it already exists in DB or in case of failure.
//
// This logic would be improved by https://issues.redhat.com/browse/GITOPSRVCE-73 (and others)
func internalDetermineGitOpsEngineInstanceForNewApplication(ctx context.Context, user db.ClusterUser,
	k8sClient client.Client, dbq db.DatabaseQueries, log logr.Logger) (*db.GitopsEngineInstance, bool, *db.GitopsEngineCluster, error) {

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dbutil.GetGitOpsEngineSingleInstanceNamespace(), Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace()}}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
		return nil, false, nil, fmt.Errorf("unable to retrieve gitopsengine namespace in determineGitOpsEngineInstanceForNewApplication: %v", err)
	}

	kubeSystemNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", Namespace: "kube-system"}}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(kubeSystemNamespace), kubeSystemNamespace); err != nil {
		return nil, false, nil, fmt.Errorf("unable to retrieve kube-system namespace in determineGitOpsEngineInstanceForNewApplication: %v", err)
	}

	gitopsEngineInstance, isNewInstance, gitopsEngineCluster, err := dbutil.GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID(ctx, *namespace, string(kubeSystemNamespace.UID), dbq, log)
	if err != nil {
		return nil, false, nil, fmt.Errorf("unable to get or create engine instance for new application: %v", err)
	}

	// When we support multiple Argo CD instance, the algorithm would be:
	//
	// algorithm input:
	// - user
	// - API namespace (this assumes that all gitopsdeployment* APIs in a single namespace will share the same Argo CD instance)
	//
	// output:
	// - gitops engine instance
	//
	// In a way that ensures that applications are balanced between instances.
	// Preliminary thoughts: https://docs.google.com/document/d/15E8d5frNuTFEdCHMlNSk0LQr6DI7BtiypxIC2AnW-OQ/edit#

	return gitopsEngineInstance, isNewInstance, gitopsEngineCluster, nil
}

// The bool return value is 'true' if ClusterAccess is created; 'false' if it already exists in DB or in case of failure.
func internalGetOrCreateClusterAccess(ctx context.Context, ca *db.ClusterAccess, dbq db.DatabaseQueries, log logr.Logger) (error, bool) {

	if err := dbq.GetClusterAccessByPrimaryKey(ctx, ca); err != nil {

		if !db.IsResultNotFoundError(err) {
			return err, false
		}
	} else {
		return nil, false
	}

	if err := dbq.CreateClusterAccess(ctx, ca); err != nil {
		log.Error(err, "Unable to create ClusterAccess", ca.GetAsLogKeyValues()...)

		return err, false
	}
	log.Info(fmt.Sprintf("Created ClusterAccess for UserID: %s, for ManagedEnvironment: %s", ca.Clusteraccess_user_id,
		ca.Clusteraccess_managed_environment_id), ca.GetAsLogKeyValues()...)

	return nil, true
}

// The bool return value is 'true' if ClusterUser is created; 'false' if it already exists in DB or in case of failure.
func internalGetOrCreateClusterUserByNamespaceUID(ctx context.Context, namespaceUID string, dbq db.DatabaseQueries, log logr.Logger) (*db.ClusterUser, bool, error) {
	isNewUser := false

	// TODO: GITOPSRVCE-19 - KCP support: for now, we assume that the namespace UID that the request occurred in is the user id.
	clusterUser := db.ClusterUser{User_name: namespaceUID}

	err := dbq.GetClusterUserByUsername(ctx, &clusterUser)
	if err != nil {
		if db.IsResultNotFoundError(err) {
			isNewUser = true

			if err := dbq.CreateClusterUser(ctx, &clusterUser); err != nil {
				log.Error(err, "Unable to create ClusterUser", clusterUser.GetAsLogKeyValues()...)
				return nil, false, err
			}
			log.Info("Created ClusterUser", clusterUser.GetAsLogKeyValues()...)

		} else {
			return nil, false, err
		}
	}

	return &clusterUser, isNewUser, nil
}
