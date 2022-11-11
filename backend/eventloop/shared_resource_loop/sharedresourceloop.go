package shared_resource_loop

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
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
	workspaceNamespace corev1.Namespace, l logr.Logger) (*db.ClusterUser, bool, error) {

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		log:                l,
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
	workspaceNamespace corev1.Namespace, l logr.Logger) (*db.GitopsEngineInstance, error) {

	responseChannel := make(chan any)

	msg := sharedResourceLoopMessage{
		log:                l,
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
	l := log.FromContext(ctx)
	dbQueries, err := db.NewSharedProductionPostgresDBQueries(false)
	if err != nil {
		l.Error(err, "SEVERE: internalSharedResourceEventLoop exiting before startup")
		return
	}

	for {
		msg := <-inputChan

		_, err = sharedutil.CatchPanic(func() error {
			processSharedResourceMessage(msg.ctx, msg, dbQueries, msg.log)
			return nil
		})
		if err != nil {
			l.Error(err, "unexpected error from processMessage in internalSharedResourceEventLoop")
		}
	}
}

func processSharedResourceMessage(ctx context.Context, msg sharedResourceLoopMessage, dbQueries db.DatabaseQueries, l logr.Logger) {

	l.V(sharedutil.LogLevel_Debug).Info("sharedResourceEventLoop received message: "+string(msg.messageType),
		"workspace", msg.workspaceNamespace.UID)

	if msg.messageType == sharedResourceLoopMessage_getOrCreateSharedManagedEnv {

		payload, ok := (msg.payload).(sharedResourceLoopMessage_getOrCreateSharedResourceManagedEnvRequest)
		if !ok {
			err := fmt.Errorf("SEVERE: unexpected payload")
			l.Error(err, "")
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
			payload.k8sClientFactory, dbQueries, l)

		response := sharedResourceLoopMessage_getOrCreateSharedResourcesResponse{
			err:               err,
			responseContainer: res,
		}

		// Reply on a separate goroutine so cancelled callers don't block the event loop
		go func() {
			msg.responseChannel <- response
		}()

	} else if msg.messageType == sharedResourceLoopMessage_getOrCreateClusterUserByNamespaceUID {

		clusterUser, isNewUser, err := internalProcessMessage_GetOrCreateClusterUserByNamespaceUID(ctx, msg.workspaceNamespace, dbQueries, l)

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
			l.Error(err, err.Error())
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
				payload.repositoryCredentialCRName, msg.workspaceNamespace, msg.workspaceClient, payload.k8sClientFactory, dbQueries, l)

		} else {
			err = fmt.Errorf("SEVERE - unexpected cast in internalSharedResourceEventLoop")
			l.Error(err, err.Error())
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
		l.Error(nil, "SEVERE: unrecognized sharedResourceLoopMessageType: "+string(msg.messageType))
	}

}

func deleteRepoCredFromDB(ctx context.Context, dbQueries db.DatabaseQueries, ID string, l logr.Logger) (bool, error) {
	const retry, noRetry = true, false
	rowsDeleted, err := dbQueries.DeleteRepositoryCredentialsByID(ctx, ID)

	if err != nil {
		// Log the error and retry
		l.V(sharedutil.LogLevel_Debug).Info("Error deleting Repository Credential from the database", "error", err)
		return retry, err
	}

	if rowsDeleted == 0 {
		// Log the error, but continue to delete the other Repository Credentials (this looks morel like a bug in our code)
		l.V(sharedutil.LogLevel_Warn).Error(nil, "No rows deleted from the database", "rowsDeleted", rowsDeleted, "repocred id", ID)
		return noRetry, err
	}

	// meaning: err == nil && rowsDeleted > 0
	l.V(sharedutil.LogLevel_Debug).Info("Deleted Repository Credential from the database", "repocred id", ID)
	return noRetry, nil
}

const (
	errGenericCR        = "unable to retrieve CR from the cluster"
	errUpdateDBRepoCred = "unable to update repository credential in the database"
	errCreateDBRepoCred = "unable to create repository credential in the database"
)

func internalProcessMessage_ReconcileRepositoryCredential(ctx context.Context,
	repositoryCredentialCRName string,
	repositoryCredentialCRNamespace corev1.Namespace,
	apiNamespaceClient client.Client,
	k8sClientFactory SRLK8sClientFactory,
	dbQueries db.DatabaseQueries, l logr.Logger) (*db.RepositoryCredentials, error) {

	resourceNS := repositoryCredentialCRNamespace.Name

	clusterUser, _, err := internalGetOrCreateClusterUserByNamespaceUID(ctx, string(repositoryCredentialCRNamespace.UID), dbQueries, l)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve cluster user while processing GitOpsRepositoryCredentials: '%s' in namespace: '%s': %v",
			repositoryCredentialCRName, string(repositoryCredentialCRNamespace.UID), err)
	}

	managedEnv, _, err := dbutil.GetOrCreateManagedEnvironmentByNamespaceUID(ctx, repositoryCredentialCRNamespace, dbQueries, l)
	if err != nil {
		return nil, fmt.Errorf("unable to get or created managed env on deployment modified event: %v", err)
	}

	gitopsEngineInstance, _, _, err := internalDetermineGitOpsEngineInstanceForNewApplication(ctx, *clusterUser, *managedEnv, apiNamespaceClient, dbQueries, l)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve cluster user while processing GitOpsRepositoryCredentials: '%s' in namespace: '%s': Error: %v",
			repositoryCredentialCRName, string(repositoryCredentialCRNamespace.UID), err)
	}

	// Note: this may be nil in some if-else branches
	gitopsDeploymentRepositoryCredentialCR := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{}

	// 1) Attempt to get the gitopsDeploymentRepositoryCredentialCR from the namespace
	l.Info("Attempting to get the CR")
	if err := apiNamespaceClient.Get(ctx, types.NamespacedName{Namespace: resourceNS, Name: repositoryCredentialCRName},
		gitopsDeploymentRepositoryCredentialCR); err != nil {

		l.Info("Error getting the CR")

		if apierr.IsNotFound(err) {
			l.Info("CR not found")
			gitopsDeploymentRepositoryCredentialCR = nil

		} else {
			l.Info("Some other error with the CR")

			// Something went wrong, retry
			l.WithValues("Error", err, "DebugErr", errGenericCR, "CR Name", repositoryCredentialCRName, "Namespace", resourceNS)
			// TODO: PANOS - The l is updated here, but it's not being used for anything. l.WithValues() doesn't log anything.

			vErr := fmt.Errorf("unexpected error in retrieving repository credentials: %v", err)
			return nil, vErr

		}
	}

	// 2) Look for any APICRToDBMappings that point(ed) to a K8s resource with the same name and namespace
	// as this GitOpsDeploymentRespositoryCredential
	l.Info("Look for DB Row if this exists")

	var apiCRToDBMappingList []db.APICRToDatabaseMapping
	if err := dbQueries.ListAPICRToDatabaseMappingByAPINamespaceAndName(
		ctx, db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
		repositoryCredentialCRName,
		repositoryCredentialCRNamespace.Name,
		string(repositoryCredentialCRNamespace.GetUID()),
		db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential,
		&apiCRToDBMappingList); err != nil {

		return nil, fmt.Errorf("unable to list APICRs for repository credentials: %v", err)
	}

	// APICRToDBMapping that matches the current resource UID (or nil if the the current resource UID doesn't exist)
	var currentAPICRToDBMapping *db.APICRToDatabaseMapping

	// old APICRToDBMappings that don't match the current resource UID (pointing to previously deleted GitOpsDeploymentRepositoryCredentials)
	oldAPICRToDBMappings := []db.APICRToDatabaseMapping{}

	// 3) Identify APICRToDBMappings that refer to K8s resources that no longer exist, and find the DTAM that
	// refers to the resource we got the request for.
	for idx := range apiCRToDBMappingList {

		apiCRToDBMapping := apiCRToDBMappingList[idx]

		if gitopsDeploymentRepositoryCredentialCR != nil &&
			apiCRToDBMapping.APIResourceUID == string(gitopsDeploymentRepositoryCredentialCR.UID) {

			currentAPICRToDBMapping = &apiCRToDBMapping
		} else {
			oldAPICRToDBMappings = append(oldAPICRToDBMappings, apiCRToDBMapping)
		}
	}

	// 4) Clean up any old GitOpsDeploymentRepositoryCredential DB rows that have the same name/namespace as this resource,
	// but that no longer exist
	if len(oldAPICRToDBMappings) > 0 {

		for _, oldAPICRToDBMapping := range oldAPICRToDBMappings {

			repositoryCredentialPrimaryKey := oldAPICRToDBMapping.DBRelationKey

			// 4a) Delete the  RepositoryCredential DB row if it exists (and create an operation for it if needed)
			if dbRepoCred, err := dbQueries.GetRepositoryCredentialsByID(ctx, repositoryCredentialPrimaryKey); err != nil {

				if db.IsResultNotFoundError(err) {
					l.Info("DB entry not found")
					// The CR is not found in the database, so it is already deleted
				} else {
					// It's a glitch, so return the error
					l.Info("Error getting repo cred from DB")
					return nil, fmt.Errorf("unable to retrieve repository credential from the database: %v", err)
				}

			} else {
				l.Info("[Leftover] RepositoryCredential row found in DB")
				if _, err := deleteRepoCredFromDB(ctx, dbQueries, repositoryCredentialPrimaryKey, l); err != nil {
					return nil, err
				}
				l.Info("RepositoryCredential row deleted from DB")

				// We need to fire-up an Operation as well
				l.Info("Creating an Operation for the deleted RepositoryCredential DB row")
				if err := createRepoCredOperation(ctx, dbRepoCred, clusterUser, resourceNS, dbQueries,
					apiNamespaceClient, l); err != nil {

					l.Info("Error creating an Operation for the deleted RepositoryCredential DB row")
					return nil, err
				}
				l.Info("Operation created for the deleted RepositoryCredential DB row")

				// TODO: PANOS - need a call to operations.CleanupOperation() here, once the operation is done.
			}

			// 4b) Next, we delete the APICRToDBMapping that pointed to the deleted RepositoryCredential DB row

			// Delete the APICRToDatabaseMapping referenced by 'item'
			if rowsDeleted, err := dbQueries.DeleteAPICRToDatabaseMapping(ctx, &oldAPICRToDBMapping); err != nil {
				l.Info("unable to delete apiCRToDBmapping", "mapping", oldAPICRToDBMapping.APIResourceUID)
				return nil, err
			} else if rowsDeleted == 0 {
				l.Info("unexpected number of rows deleted of apiCRToDBmapping", "mapping", oldAPICRToDBMapping.APIResourceUID)
			} else {
				l.Info("deleted APICRToDatabaseMapping")
			}
		}

		// We've completed cleanup of all the old repo cred CRs
	}

	if gitopsDeploymentRepositoryCredentialCR == nil {
		// If the GitOpsDeploymentRepositoryCredential doesn't exist, then our work is done.
		return nil, nil
	}

	l.Info("CR found", "CR Name", repositoryCredentialCRName, "Namespace", resourceNS)
	l.Info("gitopsDeploymentRepositoryCredentialCR", "gitopsDeploymentRepositoryCredentialCR", gitopsDeploymentRepositoryCredentialCR)
	l.Info("UID", "UID", gitopsDeploymentRepositoryCredentialCR.GetUID())

	// 5) If gitopsDeploymentRepositoryCredentialCR exists in the cluster, check the DB to see if the related RepositoryCredential row exists as well

	// TODO: PANOS - Sanity test for gitopsDeploymentRepositoryCredentialCR.Spec.Secret to be non-empty value

	var privateURL, authUsername, authPassword, authSSHKey, secretObj string
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gitopsDeploymentRepositoryCredentialCR.Spec.Secret,
			Namespace: resourceNS, // we assume the secret is in the same namespace as the CR
		},
	}

	privateURL = gitopsDeploymentRepositoryCredentialCR.Spec.Repository

	if err := apiNamespaceClient.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		if apierr.IsNotFound(err) {
			l.WithValues("secret not found", err, "secret", gitopsDeploymentRepositoryCredentialCR.Spec.Secret, "namespace", resourceNS)
			return nil, fmt.Errorf("secret not found: %v", err)
		} else {
			// Something went wrong, retry
			l.WithValues("Error", err, "secret", gitopsDeploymentRepositoryCredentialCR.Spec.Secret, "namespace", resourceNS)
			return nil, fmt.Errorf("error retrieving secret: %v", err)
		}
	} else {
		// Secret exists, so get its data

		// TODO: PANOS - we had a slack conversation about whether the 'url' field should match the secret, but I forget what we decided the behaviour should be....

		// privateURL = string(secret.Data["url"]) // is this supposed to the same as the CR's Repository?
		authUsername = string(secret.Data["username"])
		authPassword = string(secret.Data["password"])
		authSSHKey = string(secret.Data["sshPrivateKey"])
		secretObj = secret.Name
	}

	// 6) If there is no existing APICRToDBMapping for this CR, then it is a create
	if currentAPICRToDBMapping == nil {

		l.Info("Error getting mapping for GitOpsDeploymentRepositoryCredential")
		l.Info("err", "error", err)

		l.Info("Since DB was not found, we create the DB Row now")

		// If the CR exists in the cluster but not in the DB, then create it in the DB and create an Operation.
		dbRepoCred := db.RepositoryCredentials{
			UserID:          clusterUser.Clusteruser_id, // comply with the constraint 'fk_clusteruser_id'
			PrivateURL:      privateURL,                 // or gitopsDeploymentRepositoryCredentialCR.Spec.Repository?
			AuthUsername:    authUsername,
			AuthPassword:    authPassword,
			AuthSSHKey:      authSSHKey,
			SecretObj:       secretObj,
			EngineClusterID: gitopsEngineInstance.Gitopsengineinstance_id, // comply with the constraint 'fk_gitopsengineinstance_id',
		}

		err = dbQueries.CreateRepositoryCredentials(ctx, &dbRepoCred)

		if err != nil {
			// TODO: PANOS - The l is updated here, but it's not being used for anything. l.WithValues() doesn't log anything.
			l = l.WithValues("Error", err, "DebugErr", errCreateDBRepoCred, "CR Name", repositoryCredentialCRName, "Namespace", resourceNS)
			return nil, fmt.Errorf("unable to create repository credential in the database: %v", err)
		} else {
			l.Info("Created RepositoryCredential in the DB", "repositoryCredential", dbRepoCred)
		}

		// Create the mapping
		newApiCRToDBMapping := db.APICRToDatabaseMapping{
			APIResourceType:      db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentRepositoryCredential,
			APIResourceUID:       string(gitopsDeploymentRepositoryCredentialCR.UID),
			APIResourceName:      repositoryCredentialCRName,
			APIResourceNamespace: resourceNS,
			NamespaceUID:         string(repositoryCredentialCRNamespace.GetUID()),

			DBRelationType: db.APICRToDatabaseMapping_DBRelationType_RepositoryCredential,
			DBRelationKey:  dbRepoCred.RepositoryCredentialsID,
		}

		if err := dbQueries.CreateAPICRToDatabaseMapping(ctx, &newApiCRToDBMapping); err != nil {
			l.Info("Error", "error", err, "Debug:", "unable to create api to db mapping in database")

			// If we were unable to retrieve the client, delete the resources we created in the previous steps

			return nil, err
		}
		l.Info(fmt.Sprintf("Created a ApiCRToDBMapping: (APIResourceType: %s, APIResourceUID: %s, DBRelationType: %s)", newApiCRToDBMapping.APIResourceType, newApiCRToDBMapping.APIResourceUID, newApiCRToDBMapping.DBRelationType))

		err = createRepoCredOperation(ctx, dbRepoCred, clusterUser, resourceNS, dbQueries, apiNamespaceClient, l)
		if err != nil {
			return nil, err
		}
		l.Info("Created Operation for RepositoryCredential")

		// TODO: PANOS - need a call to operations.CleanupOperation() here, once the operation is done.

		return &dbRepoCred, nil
	}

	// 7) If the APICRToDBMapping already exists in the database, and already points to the CR, then it is instead an update

	// Match found in database

	repositoryCredentialPrimaryKey := currentAPICRToDBMapping.DBRelationKey

	l.Info("Primary Key", "key", repositoryCredentialPrimaryKey)

	if dbRepoCred, err := dbQueries.GetRepositoryCredentialsByID(ctx, repositoryCredentialPrimaryKey); err != nil {

		l.Info("Error getting repo cred from DB")

		if db.IsResultNotFoundError(err) {
			// If the APICRToDBMapping points to a RepositoryCredential that doesn't exist, delete the APICRToDBMapping
			// and return an error.

			l.Info("Deleting APICRToDBMapping that points to an invalid repository credential")

			// Delete the APICRToDatabaseMapping referenced by 'item'
			if rowsDeleted, err := dbQueries.DeleteAPICRToDatabaseMapping(ctx, currentAPICRToDBMapping); err != nil {
				l.Info("unable to delete apiCRToDBmapping", "mapping", currentAPICRToDBMapping.APIResourceUID)
				return nil, err
			} else if rowsDeleted == 0 {
				l.Info("unexpected number of rows deleted of apiCRToDBmapping", "mapping", currentAPICRToDBMapping.APIResourceUID)
			} else {
				l.Info("deleted APICRToDatabaseMapping")
			}
			return nil, fmt.Errorf("APICRToDBMapping pointed to a RepositoryCredential that didn't exist")
		}

		// TODO: PANOS - The l is updated here, but it's not being used for anything. l.WithValues() doesn't log anything.
		l = l.WithValues("Error retrieving repository credentials from DB", err, "repositoryCredentialsID", gitopsDeploymentRepositoryCredentialCR.UID)
		err = fmt.Errorf("error retrieving repository credentials from DB: %v", err)
		return nil, err

	} else {

		l.Info("the CR exists in the cluster and in the DB, then check if the data is the same and create an Operation")

		// If the CR exists in the cluster and in the DB, then check if the data is the same and create an Operation
		isUpdateNeeded := compareAndModifyClusterResourceWithDatabaseRow(*gitopsDeploymentRepositoryCredentialCR, &dbRepoCred, secret, l)
		if isUpdateNeeded {
			l.Info("Syncing with database...")
			if err := dbQueries.UpdateRepositoryCredentials(ctx, &dbRepoCred); err != nil {
				// TODO: PANOS - The l is updated here, but it's not being used for anything. l.WithValues() doesn't log anything.
				l = l.WithValues("Error", err, "ErrDebug", errUpdateDBRepoCred)
				return nil, err
			}

			if err := createRepoCredOperation(ctx, dbRepoCred, clusterUser, resourceNS, dbQueries, apiNamespaceClient, l); err != nil {
				return nil, err
			}

			return &dbRepoCred, nil
			// TODO: PANOS - need a call to operations.CleanupOperation() here, once the operation is done.

		} else {
			return &dbRepoCred, nil
		}
	}
}

func createRepoCredOperation(ctx context.Context, dbRepoCred db.RepositoryCredentials, clusterUser *db.ClusterUser, ns string,
	dbQueries db.DatabaseQueries, apiNamespaceClient client.Client, l logr.Logger) error {

	l.Info("Creating operation...")

	dbOperationInput := db.Operation{
		Instance_id:             dbRepoCred.EngineClusterID,
		Resource_id:             dbRepoCred.RepositoryCredentialsID,
		Resource_type:           db.OperationResourceType_RepositoryCredentials,
		State:                   db.OperationState_Waiting,
		Operation_owner_user_id: clusterUser.Clusteruser_id,
	}

	operationCR, operationDB, err := operations.CreateOperation(ctx, false, dbOperationInput, clusterUser.Clusteruser_id, ns, dbQueries,
		apiNamespaceClient, l)
	if err != nil {
		err2 := fmt.Errorf("unable to create operation: %v", err)
		return err2
	}

	l.Info("operationCR", "operationCR", operationCR)
	l.Info("operationDB", "operationDB", operationDB)

	return nil
}

func compareAndModifyClusterResourceWithDatabaseRow(cr managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential,
	dbr *db.RepositoryCredentials, secret *corev1.Secret, l logr.Logger) bool {

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

	// Fetch these data from the secret
	authUsername := string(secret.Data["username"])
	authPassword := string(secret.Data["password"])
	authSSHKey := string(secret.Data["sshPrivateKey"])

	// Compare the data from the secret with the data from the DB
	var isAuthUsernameUpdateNeeded bool
	if authUsername != dbr.AuthUsername {
		l.Info("AuthUsername changed", "old", dbr.AuthUsername, "new", authUsername)
		dbr.AuthUsername = authUsername
		isAuthUsernameUpdateNeeded = true
	}

	var isAuthPasswordUpdateNeeded bool
	if authPassword != dbr.AuthPassword {
		l.Info("AuthPassword changed", "old", dbr.AuthPassword, "new", authPassword)
		dbr.AuthPassword = authPassword
		isAuthPasswordUpdateNeeded = true
	}

	var isAuthSSHKeyUpdateNeeded bool
	if authSSHKey != dbr.AuthSSHKey {
		l.Info("AuthSSHKey changed", "old", dbr.AuthSSHKey, "new", authSSHKey)
		dbr.AuthSSHKey = authSSHKey
		isAuthSSHKeyUpdateNeeded = true
	}

	return isSecretUpdateNeeded || isRepoUpdateNeeded || isAuthUsernameUpdateNeeded ||
		isAuthPasswordUpdateNeeded || isAuthSSHKeyUpdateNeeded
}

func internalProcessMessage_GetGitopsEngineInstanceById(ctx context.Context, id string, dbq db.DatabaseQueries) (*db.GitopsEngineInstance, error) {

	gitopsEngineInstance := db.GitopsEngineInstance{
		Gitopsengineinstance_id: id,
	}

	err := dbq.GetGitopsEngineInstanceById(ctx, &gitopsEngineInstance)
	return &gitopsEngineInstance, err
}

// The bool return value is 'true' if ClusterUser is created; 'false' if it already exists in DB or in case of failure.
func internalProcessMessage_GetOrCreateClusterUserByNamespaceUID(ctx context.Context, workspaceNamespace corev1.Namespace,
	dbq db.DatabaseQueries, l logr.Logger) (*db.ClusterUser, bool, error) {
	isNewUser := false

	// TODO: GITOPSRVCE-19 - KCP support: for now, we assume that the namespace UID that the request occurred in is the user id.
	clusterUser := db.ClusterUser{User_name: string(workspaceNamespace.UID)}

	// TODO: GITOPSRVCE-41 - We are assuming that user namespace uid == username, which is messy. We should add a new field for unique user id, and username should be human readable and not used for security, etc.
	err := dbq.GetClusterUserByUsername(ctx, &clusterUser)
	if err != nil {
		if db.IsResultNotFoundError(err) {
			isNewUser = true

			if err := dbq.CreateClusterUser(ctx, &clusterUser); err != nil {
				l.Error(err, "Unable to create ClusterUser with User ID: "+clusterUser.Clusteruser_id, clusterUser.GetAsLogKeyValues()...)
				return nil, false, err
			}
			l.Info("Created Cluster User with User ID: "+clusterUser.Clusteruser_id, clusterUser.GetAsLogKeyValues()...)

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
	l logr.Logger) (SharedResourceManagedEnvContainer, error) {

	clusterUser, isNewUser, err := internalGetOrCreateClusterUserByNamespaceUID(ctx, string(workspaceNamespace.UID), dbQueries, l)
	if err != nil {
		return SharedResourceManagedEnvContainer{},
			fmt.Errorf("unable to retrieve cluster user in processMessage, '%s': %v", string(workspaceNamespace.UID), err)
	}

	managedEnv, isNewManagedEnv, err := dbutil.GetOrCreateManagedEnvironmentByNamespaceUID(ctx, workspaceNamespace, dbQueries, l)
	if err != nil {
		return SharedResourceManagedEnvContainer{},
			fmt.Errorf("unable to get or created managed env on deployment modified event: %v", err)
	}

	engineInstance, isNewInstance, gitopsEngineCluster, err := internalDetermineGitOpsEngineInstanceForNewApplication(ctx, *clusterUser, *managedEnv, workspaceClient, dbQueries, l)
	if err != nil {
		return SharedResourceManagedEnvContainer{}, fmt.Errorf("unable to determine gitops engine instance: %v", err)
	}

	// Create the cluster access object, to allow us to interact with the GitOpsEngine and ManagedEnvironment on the user's behalf
	ca := db.ClusterAccess{
		Clusteraccess_user_id:                   clusterUser.Clusteruser_id,
		Clusteraccess_managed_environment_id:    managedEnv.Managedenvironment_id,
		Clusteraccess_gitops_engine_instance_id: engineInstance.Gitopsengineinstance_id,
	}

	err, isNewClusterAccess := internalGetOrCreateClusterAccess(ctx, &ca, dbQueries, l)
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
func internalDetermineGitOpsEngineInstanceForNewApplication(ctx context.Context, user db.ClusterUser, managedEnv db.ManagedEnvironment,
	k8sClient client.Client, dbq db.DatabaseQueries, l logr.Logger) (*db.GitopsEngineInstance, bool, *db.GitopsEngineCluster, error) {

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dbutil.GetGitOpsEngineSingleInstanceNamespace(), Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace()}}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
		return nil, false, nil, fmt.Errorf("unable to retrieve gitopsengine namespace in determineGitOpsEngineInstanceForNewApplication: %v", err)
	}

	kubeSystemNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", Namespace: "kube-system"}}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(kubeSystemNamespace), kubeSystemNamespace); err != nil {
		return nil, false, nil, fmt.Errorf("unable to retrieve kube-system namespace in determineGitOpsEngineInstanceForNewApplication: %v", err)
	}

	gitopsEngineInstance, isNewInstance, gitopsEngineCluster, err := dbutil.GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID(ctx, *namespace, string(kubeSystemNamespace.UID), dbq, l)
	if err != nil {
		return nil, false, nil, fmt.Errorf("unable to get or create engine instance for new application: %v", err)
	}

	// When we support multiple Argo CD instance, the algorithm would be:
	//
	// algorithm input:
	// - user
	// - managed environment
	//
	// output:
	// - gitops engine instance
	//
	// In a way that ensures that applications are balanced between instances.
	// Preliminary thoughts: https://docs.google.com/document/d/15E8d5frNuTFEdCHMlNSk0LQr6DI7BtiypxIC2AnW-OQ/edit#

	return gitopsEngineInstance, isNewInstance, gitopsEngineCluster, nil
}

// The bool return value is 'true' if ClusterAccess is created; 'false' if it already exists in DB or in case of failure.
func internalGetOrCreateClusterAccess(ctx context.Context, ca *db.ClusterAccess, dbq db.DatabaseQueries, l logr.Logger) (error, bool) {

	if err := dbq.GetClusterAccessByPrimaryKey(ctx, ca); err != nil {

		if !db.IsResultNotFoundError(err) {
			return err, false
		}
	} else {
		return nil, false
	}

	if err := dbq.CreateClusterAccess(ctx, ca); err != nil {
		l.Error(err, "Unable to create ClusterAccess", ca.GetAsLogKeyValues()...)

		return err, false
	}
	l.Info(fmt.Sprintf("Created ClusterAccess for UserID: %s, for ManagedEnvironment: %s", ca.Clusteraccess_user_id,
		ca.Clusteraccess_managed_environment_id), ca.GetAsLogKeyValues()...)

	return nil, true
}

// The bool return value is 'true' if ClusterUser is created; 'false' if it already exists in DB or in case of failure.
func internalGetOrCreateClusterUserByNamespaceUID(ctx context.Context, namespaceUID string, dbq db.DatabaseQueries, l logr.Logger) (*db.ClusterUser, bool, error) {
	isNewUser := false

	// TODO: GITOPSRVCE-19 - KCP support: for now, we assume that the namespace UID that the request occurred in is the user id.
	clusterUser := db.ClusterUser{User_name: namespaceUID}

	err := dbq.GetClusterUserByUsername(ctx, &clusterUser)
	if err != nil {
		if db.IsResultNotFoundError(err) {
			isNewUser = true

			if err := dbq.CreateClusterUser(ctx, &clusterUser); err != nil {
				l.Error(err, "Unable to create ClusterUser", clusterUser.GetAsLogKeyValues()...)
				return nil, false, err
			}
			l.Info("Created ClusterUser", clusterUser.GetAsLogKeyValues()...)

		} else {
			return nil, false, err
		}
	}

	return &clusterUser, isNewUser, nil
}
