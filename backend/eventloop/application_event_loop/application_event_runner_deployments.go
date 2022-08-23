package application_event_loop

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"

	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"

	argosharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/errors"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/fauxargocd"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/operations"
	"github.com/redhat-appstudio/managed-gitops/backend/condition"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	goyaml "gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type deploymentModifiedResult string

const (
	deploymentModifiedResult_Failed   deploymentModifiedResult = "failed"
	deploymentModifiedResult_Deleted  deploymentModifiedResult = "deletedApp"
	deploymentModifiedResult_Created  deploymentModifiedResult = "createdNewApp"
	deploymentModifiedResult_Updated  deploymentModifiedResult = "updatedApp"
	deploymentModifiedResult_NoChange deploymentModifiedResult = "noChangeInApp"
)

// This file is responsible for processing events related to GitOpsDeployment CR.

// applicationEventRunner_handleDeploymentModified handles GitOpsDeployment resource events, ensuring that the
// database is consistent with the corresponding GitOpsDeployment resource.
//
// For example:
// - If a GitOpsDeployment resource is created in the namespace, ensure there exists a corresponding Application database row.
// - If a GitOpsDeployment resource is modifeid in the namespace, ensure the corresponding Application database row is modified.
// - Likewise, if a GitOpsDeployment previously existed, and has been deleted, then ensure the Application is cleaned up.
//
// If a change is made to the database, the cluster-agent will be informed by creating an Operation resource, pointing to the changed Application.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - references to the Application and GitOpsEngineInstance database fields.
// - error is non-nil, if an error occurred
func (a *applicationEventLoopRunner_Action) applicationEventRunner_handleDeploymentModified(ctx context.Context,
	dbQueries db.ApplicationScopedQueries) (bool, *db.Application, *db.GitopsEngineInstance, deploymentModifiedResult,
	errors.UserError) {

	deplName := a.eventResourceName
	deplNamespace := a.eventResourceNamespace
	workspaceClient := a.workspaceClient
	log := a.log

	gitopsDeplNamespace := corev1.Namespace{}
	if err := workspaceClient.Get(ctx, types.NamespacedName{Namespace: deplNamespace, Name: deplNamespace}, &gitopsDeplNamespace); err != nil {
		userError := fmt.Sprintf("unable to retrieve the contents of the namespace '%s' containing the API resource '%s'. Does it exist?",
			deplNamespace, deplName)
		devError := fmt.Errorf("unable to retrieve namespace '%s': %v", deplNamespace, err)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	clusterUser, _, err := a.sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, workspaceClient, gitopsDeplNamespace)
	if err != nil {
		userError := "unable to identify the identify of the user"
		devError := fmt.Errorf("unable to retrieve cluster user in handleDeploymentModified, '%s': %v",
			string(gitopsDeplNamespace.UID), err)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	// 1) Retrieve the GitOpsDeployment from the namespace
	gitopsDeploymentCRExists := true // True if the GitOpsDeployment resource exists in the namespace, false otherwise
	gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
	{
		gitopsDeploymentKey := client.ObjectKey{Namespace: deplNamespace, Name: deplName}

		if err := workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment); err != nil {

			if apierr.IsNotFound(err) {
				gitopsDeploymentCRExists = false
			} else {

				userError := "unable to retrieve the GitOpsDeployment object from the namespace, due to unknown error."
				log.Error(err, "unable to locate object in handleDeploymentModified", "request", gitopsDeploymentKey)
				return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, err)
			}
		}
	}

	// 2) Next, retrieve the corresponding database for the gitopsdepl, if applicable
	deplToAppMapExistsInDB := false

	var deplToAppMappingList []db.DeploymentToApplicationMapping
	if gitopsDeploymentCRExists {
		// 2a) The CR exists, so use the UID of the CR to retrieve the database entry, if possible
		deplToAppMapping := &db.DeploymentToApplicationMapping{Deploymenttoapplicationmapping_uid_id: string(gitopsDeployment.UID)}

		if err = dbQueries.GetDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping); err != nil {

			if db.IsResultNotFoundError(err) {
				deplToAppMapExistsInDB = false
			} else {
				userError := "unable to retrieve GitOpsDeployment metadata from the database, due to an unknown error"
				log.Error(err, "unable to retrieve deployment to application mapping", "uid", string(gitopsDeployment.UID))
				return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, err)
			}

		} else {
			deplToAppMappingList = append(deplToAppMappingList, *deplToAppMapping)
			deplToAppMapExistsInDB = true
		}
	} else {
		// 2b) The CR no longer exists (it was likely deleted), so instead we retrieve the UID of the GitOpsDeployment from
		// the DeploymentToApplicationMapping table, by combination of (name/namespace/workspace).

		if err := dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(ctx, deplName, deplNamespace,
			eventlooptypes.GetWorkspaceIDFromNamespaceID(gitopsDeplNamespace), &deplToAppMappingList); err != nil {

			userError := "unable to retrive data related to previous GitOpsDeployments in the namespace, due to an unknown error"

			log.Error(err, "unable to retrieve deployment to application mapping by name/namespace/uid",
				"name", deplName, "namespace", deplNamespace, "UID", string(gitopsDeplNamespace.UID))
			return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, err)
		}

		if len(deplToAppMappingList) == 0 {
			// Not found: the database does not contain an entry for the GitOpsDeployment, and the CR doesn't exist,
			// so there is no more work for us to do.
			deplToAppMapExistsInDB = false
		} else {
			// Found: we were able to locate the DeplToAppMap resource for the deleted GitOpsDeployment,
			deplToAppMapExistsInDB = true
		}
	}

	log.V(sharedutil.LogLevel_Debug).Info("workspacerEventLoopRunner_handleDeploymentModified processing event",
		"gitopsDeploymentExists", gitopsDeploymentCRExists, "deplToAppMapExists", deplToAppMapExistsInDB)

	// 3) Next, the work that we need below depends on how the CR differs from the DB entry

	if !gitopsDeploymentCRExists && !deplToAppMapExistsInDB {
		// 3a) if neither exists, our work is done
		return false, nil, nil, deploymentModifiedResult_Failed, nil
	}

	if gitopsDeploymentCRExists && !deplToAppMapExistsInDB {
		// 3b) If the gitopsdepl CR exists, but the database entry doesn't,
		// then this is the first time we have seen the GitOpsDepl CR.
		// Create it in the DB and create the operation.
		return a.handleNewGitOpsDeplEvent(ctx, gitopsDeployment, clusterUser, dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries)
	}

	if !gitopsDeploymentCRExists && deplToAppMapExistsInDB {
		// 3c) If the gitopsdepl CR doesn't exist, but the database row does, then the CR has been deleted, so handle it.
		signalShutdown, err := a.handleDeleteGitOpsDeplEvent(ctx, clusterUser, dbutil.GetGitOpsEngineSingleInstanceNamespace(),
			&deplToAppMappingList, dbQueries)

		return signalShutdown, nil, nil, deploymentModifiedResult_Deleted, err
	}

	if gitopsDeploymentCRExists && deplToAppMapExistsInDB {

		if len(deplToAppMappingList) != 1 {
			err := fmt.Errorf("SEVERE - Update only supports one operation parameter")
			log.Error(err, err.Error())
			return false, nil, nil, deploymentModifiedResult_Failed,
				errors.NewDevOnlyError(fmt.Errorf("SEVERE - All cases should be handled by above if statements"))
		}

		// 3d) if both exist: it's an update (or a no-op)
		return a.handleUpdatedGitOpsDeplEvent(ctx, &deplToAppMappingList[0], gitopsDeployment, clusterUser,
			dbutil.GetGitOpsEngineSingleInstanceNamespace(), dbQueries)
	}

	return false, nil, nil, deploymentModifiedResult_Failed,
		errors.NewDevOnlyError(fmt.Errorf("SEVERE - All cases should be handled by above if statements"))

}

// handleNewGitOpsDeplEvent handles GitOpsDeployment events where the user has just created a new GitOpsDeployment resource.
// In this case, we need to create Application and DeploymentToApplicationMapping rows in the database (among others).
//
// Finally, we need to inform the cluster-agent component (via Operation), so that it configures Argo CD.
//
// Returns:
//   - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment
//     no longer exists, so no longer needs to be processed), false otherwise.
//   - references to the Application and GitOpsEngineInstance database fields.
//   - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleNewGitOpsDeplEvent(ctx context.Context,
	gitopsDeployment *managedgitopsv1alpha1.GitOpsDeployment, clusterUser *db.ClusterUser, operationNamespace string,
	dbQueries db.ApplicationScopedQueries) (bool, *db.Application, *db.GitopsEngineInstance, deploymentModifiedResult, errors.UserError) {

	gitopsDeplNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Namespace: gitopsDeployment.ObjectMeta.Namespace,
		Name: gitopsDeployment.ObjectMeta.Namespace}, &gitopsDeplNamespace); err != nil {

		userError := "unable to access the Namespace containing the GitOpsDeployment resource"
		devError := fmt.Errorf("unable to retrieve namespace for managed env, '%s': %v", gitopsDeployment.ObjectMeta.Namespace, err)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)

	}

	isWorkspaceTarget := gitopsDeployment.Spec.Destination.Environment == ""
	managedEnv, engineInstance, destinationName, err := a.reconcileManagedEnvironmentOfGitOpsDeployment(ctx, *gitopsDeployment,
		gitopsDeplNamespace, isWorkspaceTarget)
	if err != nil {

		userError := "Unable to reconcile the ManagedEnvironment. Verify that the ManagedEnvironment and Secret are correctly defined, and have valid credentials"
		devError := fmt.Errorf("unable to get or create managed environment, isworkspacetarget:%v: %v", isWorkspaceTarget, err)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)

	}

	if engineInstance == nil {
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(fmt.Errorf("unable to locate managed environment for new application"))
	}

	appName := argosharedutil.GenerateArgoCDApplicationName(string(gitopsDeployment.UID))

	// If the user specified a value, always use it. If not, use the API resource namespace (but only in the workspace target case)
	destinationNamespace := gitopsDeployment.Spec.Destination.Namespace
	if isWorkspaceTarget {
		if destinationNamespace == "" {
			destinationNamespace = a.eventResourceNamespace
		}
	}
	if destinationNamespace == "" {
		userError := fmt.Sprintf("the namespace specified in the destination field is invalid")
		devError := fmt.Errorf("invalid destination namespace: %s", destinationNamespace)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	specFieldInput := argoCDSpecInput{
		crName:               appName,
		crNamespace:          engineInstance.Namespace_name,
		destinationNamespace: destinationNamespace,
		// TODO: GITOPSRVCE-66 - Fill this in with cluster credentials
		destinationName:      destinationName,
		sourceRepoURL:        gitopsDeployment.Spec.Source.RepoURL,
		sourcePath:           gitopsDeployment.Spec.Source.Path,
		sourceTargetRevision: gitopsDeployment.Spec.Source.TargetRevision,
		automated:            strings.EqualFold(gitopsDeployment.Spec.Type, managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated),
	}

	specFieldText, err := createSpecField(specFieldInput)
	if err != nil {
		a.log.Error(err, "SEVERE: unable to marshal generated YAML")
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	var targetManagedEnvId string
	if managedEnv != nil {
		targetManagedEnvId = managedEnv.Managedenvironment_id
	}

	application := db.Application{
		Name:                    appName,
		Engine_instance_inst_id: engineInstance.Gitopsengineinstance_id,
		Managed_environment_id:  targetManagedEnvId,
		Spec_field:              specFieldText,
	}

	a.log.Info("Creating new Application in DB: " + application.Application_id)

	if err := dbQueries.CreateApplication(ctx, &application); err != nil {
		a.log.Error(err, "unable to create application", "application", application, "ownerId", clusterUser.Clusteruser_id)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	requiredDeplToAppMapping := &db.DeploymentToApplicationMapping{
		Deploymenttoapplicationmapping_uid_id: string(gitopsDeployment.UID),
		Application_id:                        application.Application_id,
		DeploymentName:                        gitopsDeployment.Name,
		DeploymentNamespace:                   gitopsDeployment.Namespace,
		NamespaceUID:                          eventlooptypes.GetWorkspaceIDFromNamespaceID(gitopsDeplNamespace),
	}

	a.log.Info("Upserting new DeploymentToApplicationMapping in DB: " + requiredDeplToAppMapping.Deploymenttoapplicationmapping_uid_id)

	if _, err := dbutil.GetOrCreateDeploymentToApplicationMapping(ctx, requiredDeplToAppMapping, dbQueries, a.log); err != nil {
		a.log.Error(err, "unable to create deplToApp mapping", "deplToAppMapping", requiredDeplToAppMapping)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	dbOperationInput := db.Operation{
		Instance_id:   engineInstance.Gitopsengineinstance_id,
		Resource_id:   application.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	gitopsEngineClient, err := a.getK8sClientForGitOpsEngineInstance(engineInstance)
	if err != nil {
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput,
		clusterUser.Clusteruser_id, operationNamespace, dbQueries, gitopsEngineClient, a.log)
	if err != nil {
		a.log.Error(err, "could not create operation", "namespace", operationNamespace)

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, operationNamespace, dbQueries, gitopsEngineClient, a.log); err != nil {
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	return false, &application, engineInstance, deploymentModifiedResult_Created, nil
}

// handleDeleteGitOpsDeplEvent handles GitOpsDeployment events where the user has just deleted a new GitOpsDeployment resource.
// In this case, we need to delete the Application and DeploymentToApplicationMapping rows in the database (among others).
//
// Finally, we need to inform the cluster-agent component, so that it configures Argo CD.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleDeleteGitOpsDeplEvent(ctx context.Context, clusterUser *db.ClusterUser,
	operationNamespace string, deplToAppMappingList *[]db.DeploymentToApplicationMapping,
	dbQueries db.ApplicationScopedQueries) (bool, errors.UserError) {

	if deplToAppMappingList == nil || clusterUser == nil {
		return false, errors.NewDevOnlyError(fmt.Errorf("required parameter should not be nil in handleDelete: %v %v", deplToAppMappingList, clusterUser))
	}

	workspaceNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Namespace: a.eventResourceNamespace, Name: a.eventResourceNamespace}, &workspaceNamespace); err != nil {
		userError := "unable to retrieve the namespace containing the GitOpsDeployment"
		devError := fmt.Errorf("unable to retrieve workspace namespace")

		return false, errors.NewUserDevError(userError, devError)
	}

	var allErrors error

	signalShutdown := true

	// For each of the database entries that reference this gitopsdepl
	for idx := range *deplToAppMappingList {

		deplToAppMapping := (*deplToAppMappingList)[idx]

		// Clean up the database entries
		itemSignalledShutdown, err := a.cleanOldGitOpsDeploymentEntry(ctx, &deplToAppMapping, clusterUser, operationNamespace, workspaceNamespace, dbQueries)
		if err != nil {
			signalShutdown = false

			if allErrors == nil {
				allErrors = fmt.Errorf("(error: %v)", err)
			} else {
				allErrors = fmt.Errorf("(error: %v) %v", err, allErrors)
			}
		}

		signalShutdown = signalShutdown && itemSignalledShutdown
	}

	if allErrors == nil {
		return signalShutdown, nil

	} else {
		return signalShutdown, errors.NewDevOnlyError(allErrors)
	}

}

// Note: this function will return a nil ManagedEnvironment and/or GitOpsEngineInstance if the ManagedEnvironment
// doesn't exist (for example, because it was deleted)
func (a applicationEventLoopRunner_Action) reconcileManagedEnvironmentOfGitOpsDeployment(ctx context.Context,
	gitopsDeployment managedgitopsv1alpha1.GitOpsDeployment, gitopsDeplNamespace corev1.Namespace,
	isWorkspaceTarget bool) (*db.ManagedEnvironment,
	*db.GitopsEngineInstance, string, error) {

	// Ask the event loop to ensure that the managed environment exists, is up-to-date, and is valid (can be connected to using k8s client)
	sharedResourceRes, err := a.sharedResourceEventLoop.ReconcileSharedManagedEnv(ctx, a.workspaceClient, gitopsDeplNamespace,
		gitopsDeployment.Spec.Destination.Environment, a.eventResourceNamespace, isWorkspaceTarget,
		a.k8sClientFactory)

	if err != nil {
		return nil, nil, "", fmt.Errorf("unable to get or create managed environment when reconciling for GitOpsDeployment: %v", err)
	}

	var destinationName string

	if isWorkspaceTarget {
		destinationName = sharedutil.ArgoCDDefaultDestinationInCluster

	} else {

		if sharedResourceRes.ManagedEnv != nil {
			destinationName = argosharedutil.GenerateArgoCDClusterSecretName(*sharedResourceRes.ManagedEnv)
		}
	}

	return sharedResourceRes.ManagedEnv, sharedResourceRes.GitopsEngineInstance, destinationName, nil
}

// handleUpdatedGitOpsDeplEvent handles GitOpsDeployment events where the user has updated an existing GitOpsDeployment resource.
// In this case, we need to ensure the Application row in the database is consistent with what the user has provided
// in the GitOpsDeployment.
//
// Finally, we need to inform the cluster-agent component, so that it configures Argo CD.
//
// Returns:
// - true if the goroutine responsible for this application can shutdown (e.g. because the GitOpsDeployment no longer exists, so no longer needs to be processed), false otherwise.
// - references to the Application and GitOpsEngineInstance database fields.
// - error is non-nil, if an error occurred
func (a applicationEventLoopRunner_Action) handleUpdatedGitOpsDeplEvent(ctx context.Context, deplToAppMapping *db.DeploymentToApplicationMapping,
	gitopsDeployment *managedgitopsv1alpha1.GitOpsDeployment, clusterUser *db.ClusterUser, operationNamespace string,
	dbQueries db.ApplicationScopedQueries) (bool, *db.Application, *db.GitopsEngineInstance, deploymentModifiedResult, errors.UserError) {

	if deplToAppMapping == nil || gitopsDeployment == nil || clusterUser == nil {
		return false, nil, nil, deploymentModifiedResult_Failed,
			errors.NewDevOnlyError(fmt.Errorf("unexpected nil param in handleUpdatedGitOpsDeplEvent: %v %v %v",
				deplToAppMapping, gitopsDeployment, clusterUser))
	}

	log := a.log.WithValues("applicationId", deplToAppMapping.Application_id, "gitopsDeplUID", gitopsDeployment.UID)

	application := &db.Application{Application_id: deplToAppMapping.Application_id}
	if err := dbQueries.GetApplicationById(ctx, application); err != nil {
		if !db.IsResultNotFoundError(err) {
			log.Error(err, "unable to retrieve Application DB entry in handleUpdatedGitOpsDeplEvent")
			return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
		} else {

			// The application pointed to by the deplToAppMapping doesn't exist; this shouldn't happen.
			log.Error(err, "SEVERE: Application pointed to by deplToAppMapping doesn't exist, in handleUpdatedGitOpsDeplEvent")

			// Delete the deplToAppMapping, since the app doesn't exist. This should cause the gitopsdepl to be reconciled by the event loop.
			if _, err := dbQueries.DeleteDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping.Deploymenttoapplicationmapping_uid_id); err != nil {
				log.Error(err, "Unable to delete deplToAppMapping which pointed to non-existent Application, in handleUpdatedGitOpsDeplEvent")
				return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
			}

			log.Info("Deleted DeploymentToApplicationMapping with Deployment ID: " + deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
			return false, nil, nil, deploymentModifiedResult_Deleted, errors.NewDevOnlyError(err)
		}
	}

	apiNamespace := corev1.Namespace{}
	if err := a.workspaceClient.Get(ctx, types.NamespacedName{Namespace: a.eventResourceNamespace, Name: a.eventResourceNamespace}, &apiNamespace); err != nil {
		userError := "unable to retrieve namespace containing the GitOpsDeployment"
		devError := fmt.Errorf("unable to retrieve workspace namespace")
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	isWorkspaceTarget := gitopsDeployment.Spec.Destination.Environment == ""
	managedEnv, engineInstance, destinationName, err := a.reconcileManagedEnvironmentOfGitOpsDeployment(ctx, *gitopsDeployment, apiNamespace, isWorkspaceTarget)
	if err != nil {
		userError := "unable to reconcile the ManagedEnvironment resource. Ensure that the ManagedEnvironment exists, it references a Secret, and the Secret is valid"
		devError := fmt.Errorf("unable to get or create managed environment: %v", err)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	if engineInstance == nil {
		// If engineInstance from reconcileManagedEnvironmentOfGitOpsDeployment is nil, instead get the engine instance from
		// the application.
		engineInstance = &db.GitopsEngineInstance{
			Gitopsengineinstance_id: application.Engine_instance_inst_id,
		}
		if err := dbQueries.GetGitopsEngineInstanceById(ctx, engineInstance); err != nil {
			return false, nil, nil, deploymentModifiedResult_Failed,
				errors.NewDevOnlyError(fmt.Errorf("unable to retrieve GitOpsEngineInstance for existing GitOpsDeployment: %v", err))
		}
	}

	// TODO: GITOPSRVCE-67 - Sanity check that the application.name matches the expected value set in handleCreateGitOpsEvent

	// If the user specified a value, always use it. If not, use the API resource namespace (but only in the workspace target case)
	destinationNamespace := gitopsDeployment.Spec.Destination.Namespace
	if isWorkspaceTarget {
		if destinationNamespace == "" {
			destinationNamespace = a.eventResourceNamespace
		}
	}

	if destinationNamespace == "" {
		userError := "destination namespace specified in the .spec.Destination.Namespace field is invalid"
		devError := fmt.Errorf("invalid destination namespace: %s", destinationNamespace)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewUserDevError(userError, devError)
	}

	specFieldInput := argoCDSpecInput{
		crName:               application.Name,
		crNamespace:          engineInstance.Namespace_name,
		destinationNamespace: destinationNamespace,
		destinationName:      destinationName,
		sourceRepoURL:        gitopsDeployment.Spec.Source.RepoURL,
		sourcePath:           gitopsDeployment.Spec.Source.Path,
		sourceTargetRevision: gitopsDeployment.Spec.Source.TargetRevision,
		automated:            strings.EqualFold(gitopsDeployment.Spec.Type, managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated),
	}

	shouldUpdateApplication := false

	// If the spec field changed from what is in the database, we should update the application
	{
		specFieldResult, err := createSpecField(specFieldInput)
		if err != nil {
			log.Error(err, "SEVERE: Unable to parse generated spec field")
			return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
		}

		if specFieldResult == application.Spec_field {
			log.Info("No spec change detected between Application DB entry and GitOpsDeployment CR")
			// No change required: the application database entry is consistent with the gitopsdepl CR
		} else {

			shouldUpdateApplication = true
			application.Spec_field = specFieldResult

			log.Info("Spec change detected between Application DB entry and GitOpsDeployment CR")
		}

		// If the managed environment changed from what is in the database, we should update the environment
		var newManagedEnvId string
		if managedEnv != nil {
			newManagedEnvId = managedEnv.Managedenvironment_id
		}
		if newManagedEnvId != application.Managed_environment_id {
			application.Managed_environment_id = newManagedEnvId
			shouldUpdateApplication = true
		}

	}

	// If neither the managed environment, nor the spec field changed, then no need to update the database, so exit.
	if !shouldUpdateApplication {
		log.Info("No Application row change detected")
		return false, application, engineInstance, deploymentModifiedResult_NoChange, nil
	}

	if err := dbQueries.UpdateApplication(ctx, application); err != nil {
		log.Error(err, "Unable to update application, after mismatch detected")

		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}
	log.Info("Application updated with ID: " + application.Application_id)

	// Create the operation
	gitopsEngineClient, err := a.getK8sClientForGitOpsEngineInstance(engineInstance)
	if err != nil {
		log.Error(err, "unable to retrieve gitopsengineinstance for updated gitopsdepl", "gitopsEngineIstance", engineInstance.EngineCluster_id)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	dbOperationInput := db.Operation{
		Instance_id:   engineInstance.Gitopsengineinstance_id,
		Resource_id:   application.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput, clusterUser.Clusteruser_id,
		operationNamespace, dbQueries, gitopsEngineClient, log)
	if err != nil {
		log.Error(err, "could not create operation", "operation", dbOperation.Operation_id, "namespace", operationNamespace)
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, operationNamespace, dbQueries, gitopsEngineClient, log); err != nil {
		return false, nil, nil, deploymentModifiedResult_Failed, errors.NewDevOnlyError(err)
	}

	return false, application, engineInstance, deploymentModifiedResult_Updated, nil

}

func (a applicationEventLoopRunner_Action) cleanOldGitOpsDeploymentEntry(ctx context.Context, deplToAppMapping *db.DeploymentToApplicationMapping,
	clusterUser *db.ClusterUser, operationNamespace string, workspaceNamespace corev1.Namespace, dbQueries db.ApplicationScopedQueries) (bool, error) {

	dbApplicationFound := true

	// If the gitopsdepl CR doesn't exist, but database row does, then the CR has been deleted, so handle it.
	dbApplication := db.Application{
		Application_id: deplToAppMapping.Application_id,
	}
	if err := dbQueries.GetApplicationById(ctx, &dbApplication); err != nil {

		a.log.Error(err, "unable to get application by id", "id", deplToAppMapping.Application_id)

		if db.IsResultNotFoundError(err) {
			dbApplicationFound = false
			// Log the error, but continue.
		} else {
			return false, err
		}
	}

	log := a.log.WithValues("id", dbApplication.Application_id)

	// Remove the ApplicationState from the database
	rowsDeleted, err := dbQueries.DeleteApplicationStateById(ctx, deplToAppMapping.Application_id)
	if err != nil {

		log.V(sharedutil.LogLevel_Warn).Error(err, "unable to delete application state by id")
		return false, err

	} else if rowsDeleted == 0 {
		// Log the warning, but continue
		log.Info("no application rows deleted for application state", "rowsDeleted", rowsDeleted)
	} else {
		log.Info("ApplicationState rows deleted App ID: ", "application_id", deplToAppMapping.Application_id, "rowsDeleted", rowsDeleted)
	}

	// Remove DeplToAppMapping
	rowsDeleted, err = dbQueries.DeleteDeploymentToApplicationMappingByDeplId(ctx, deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
	if err != nil {
		log.Error(err, "unable to delete deplToAppMapping by id", "deplToAppMapUid", deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
		return false, err

	} else if rowsDeleted == 0 {
		// Log the warning, but continue
		log.V(sharedutil.LogLevel_Warn).Error(nil, "unexpected number of rows deleted for deplToAppMapping", "rowsDeleted", rowsDeleted)
	} else {
		log.Info("Deleted deplToAppMapping by id", "deplToAppMapUid", deplToAppMapping.Deploymenttoapplicationmapping_uid_id)
	}

	rowsUpdated, err := dbQueries.UpdateSyncOperationRemoveApplicationField(ctx, deplToAppMapping.Application_id)
	if err != nil {
		log.Error(err, "unable to update old sync operations", "applicationId", deplToAppMapping.Application_id)
		return false, err

	} else if rowsUpdated == 0 {
		log.Info("no sync operation rows updated, for updating old syncoperations on gitopsdepl deletion")
	} else {
		log.Info("Removed Application Field with ID: " + deplToAppMapping.Application_id)
	}

	if !dbApplicationFound {
		log.Info("While cleaning up old gitopsdepl entries, db application wasn't found, id: " + deplToAppMapping.Application_id)
		// If the Application CR no longer exists, then our work is done.
		return true, nil
	}

	// If the Application table entry still exists, finish the cleanup...

	// Remove the Application from the database
	log.Info("deleting database Application, id: " + deplToAppMapping.Application_id)
	rowsDeleted, err = dbQueries.DeleteApplicationById(ctx, deplToAppMapping.Application_id)
	if err != nil {
		// Log the error, but continue
		log.Error(err, "unable to delete application by id", "appId", deplToAppMapping.Application_id)
	} else if rowsDeleted == 0 {
		// Log the error, but continue
		log.V(sharedutil.LogLevel_Warn).Error(nil, "unexpected number of rows deleted for application", "rowsDeleted", rowsDeleted, "appId", deplToAppMapping.Application_id)
	}

	gitopsEngineInstance, err := a.sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx, dbApplication.Engine_instance_inst_id, a.workspaceClient, workspaceNamespace)
	if err != nil {
		log := log.WithValues("id", dbApplication.Engine_instance_inst_id)

		if db.IsResultNotFoundError(err) {
			log.Error(err, "GitOpsEngineInstance could not be retrieved during gitopsdepl deletion handling")
			return false, err
		} else {
			log.Error(err, "Error occurred on attempting to retrieve gitops engine instance")
			return false, err
		}
	}

	// Create the operation that will delete the Argo CD application
	gitopsEngineClient, err := a.getK8sClientForGitOpsEngineInstance(gitopsEngineInstance)
	if err != nil {
		log.Error(err, "could not retrieve client for gitops engine instance", "instance", gitopsEngineInstance.Gitopsengineinstance_id)
		return false, err
	}
	dbOperationInput := db.Operation{
		Instance_id:   dbApplication.Engine_instance_inst_id,
		Resource_id:   deplToAppMapping.Application_id,
		Resource_type: db.OperationResourceType_Application,
	}

	waitForOperation := !a.testOnlySkipCreateOperation // if it's for a unit test, we don't wait for the operation
	k8sOperation, dbOperation, err := operations.CreateOperation(ctx, waitForOperation, dbOperationInput,
		clusterUser.Clusteruser_id, operationNamespace, dbQueries, gitopsEngineClient, log)
	if err != nil {
		log.Error(err, "unable to create operation", "operation", dbOperationInput.ShortString())
		return false, err
	}

	if err := operations.CleanupOperation(ctx, *dbOperation, *k8sOperation, operationNamespace, dbQueries, gitopsEngineClient, log); err != nil {
		log.Error(err, "unable to cleanup operation", "operation", dbOperationInput.ShortString())
		return false, err
	}

	return true, nil

}

// applicationEventRunner_handleUpdateDeploymentStatusTick updates the status field of all the GitOpsDeploymentCRs in the workspace.
func (a *applicationEventLoopRunner_Action) applicationEventRunner_handleUpdateDeploymentStatusTick(ctx context.Context,
	gitopsDeplID string, dbQueries db.ApplicationScopedQueries) error {

	// TODO: GITOPSRVCE-68 - PERF - In general, polling for all GitOpsDeployments in a workspace will scale poorly with large number of applications in the workspace. We should switch away from polling in the future.

	// 1) Retrieve the mapping for the CR we are processing
	mapping := db.DeploymentToApplicationMapping{
		Deploymenttoapplicationmapping_uid_id: string(gitopsDeplID),
	}
	if err := dbQueries.GetDeploymentToApplicationMappingByDeplId(ctx, &mapping); err != nil {
		if db.IsResultNotFoundError(err) {
			// Our work is done
			return nil
		} else {
			a.log.Error(err, "unable to locate dtam in handleUpdateDeploymentStatusTick")
			return err
		}
	}

	// 2) Retrieve the GitOpsDeployment from the namespace, using the expected values from the database
	gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
	{
		gitopsDeploymentKey := client.ObjectKey{Namespace: mapping.DeploymentNamespace, Name: mapping.DeploymentName}

		if err := a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment); err != nil {

			if apierr.IsNotFound(err) {
				// Our work is done
				return nil
			} else {
				a.log.Error(err, "unable to locate gitops object in handleUpdateDeploymentStatusTick", "request", gitopsDeploymentKey)
				return err
			}
		}
	}

	if string(gitopsDeployment.UID) != mapping.Deploymenttoapplicationmapping_uid_id {
		// This can occur if the GitOpsDeployment CR has been deleted from the workspace
		a.log.V(sharedutil.LogLevel_Warn).Info("On tick, the UID of the gitopsdeployment in the workspace differed from the tick value: " + string(gitopsDeployment.UID) + " " + mapping.Deploymenttoapplicationmapping_uid_id)
		return nil
	}

	// 3) Retrieve the application state for the application pointed to by the depltoappmapping
	applicationState := db.ApplicationState{Applicationstate_application_id: mapping.Application_id}
	if err := dbQueries.GetApplicationStateById(ctx, &applicationState); err != nil {

		if db.IsResultNotFoundError(err) {
			a.log.Info("ApplicationState not found for application, on deploymentStatusTick: " + applicationState.Applicationstate_application_id)
			return nil
		} else {
			return err
		}
	}

	// 4) update the health and status field of the GitOpsDepl CR

	// Update the local gitopsDeployment instance with health and status values (fetched from the database)
	gitopsDeployment.Status.Health.Status = managedgitopsv1alpha1.HealthStatusCode(applicationState.Health)
	gitopsDeployment.Status.Health.Message = applicationState.Message
	gitopsDeployment.Status.Sync.Status = managedgitopsv1alpha1.SyncStatusCode(applicationState.Sync_Status)
	gitopsDeployment.Status.Sync.Revision = applicationState.Revision

	// Fetch the list of resources created by deployment from table and update local gitopsDeployment instance.
	var err error
	gitopsDeployment.Status.Resources, err = decompressResourceData(applicationState.Resources)
	if err != nil {
		a.log.Error(err, "unable to decompress byte array received from table.")
		return err
	}

	// Update the actual object in Kubernetes
	if err := a.workspaceClient.Status().Update(ctx, gitopsDeployment, &client.UpdateOptions{}); err != nil {
		return err
	}
	sharedutil.LogAPIResourceChangeEvent(gitopsDeployment.Namespace, gitopsDeployment.Name, gitopsDeployment, sharedutil.ResourceModified, a.log)

	// NOTE: make sure to preserve the existing conditions fields that are in the status field of the CR, when updating the status!

	return nil

}

// gitOpsDeploymentAdapter is an "adapter" for GitOpsDeployment allowing you to easily plug any other related
// API component (i.e. for adding Conditions, look at setGitOpsDeploymentCondition() method)
// Same principle can be used for others, e.g. Finalizers, or any other field which is part of the GitOpsDeployment CRD
// It comes as a bundle along with a logger, client and ctx, so it can be easily adapted to the code
type gitOpsDeploymentAdapter struct {
	gitOpsDeployment *managedgitopsv1alpha1.GitOpsDeployment
	logger           logr.Logger
	client           client.Client
	conditionManager condition.Conditions
	ctx              context.Context
}

// newGitOpsDeploymentAdapter returns an initialized gitOpsDeploymentAdapter
func newGitOpsDeploymentAdapter(gitopsDeployment *managedgitopsv1alpha1.GitOpsDeployment, logger logr.Logger, client client.Client, manager condition.Conditions, ctx context.Context) *gitOpsDeploymentAdapter {
	return &gitOpsDeploymentAdapter{
		gitOpsDeployment: gitopsDeployment,
		logger:           logger,
		client:           client,
		conditionManager: manager,
		ctx:              ctx,
	}
}

// getMatchingGitOpsDeployment returns an updated instance of GitOpsDeployment obj from Kubernetes
func getMatchingGitOpsDeployment(ctx context.Context, name, namespace string, k8sClient client.Client) (*managedgitopsv1alpha1.GitOpsDeployment, error) {
	gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gitopsDepl), gitopsDepl)

	if err != nil {
		return &managedgitopsv1alpha1.GitOpsDeployment{}, err
	}

	return gitopsDepl, nil
}

// setGitOpsDeploymentCondition calls SetCondition() with GitOpsDeployment conditions
func (g *gitOpsDeploymentAdapter) setGitOpsDeploymentCondition(conditionType managedgitopsv1alpha1.GitOpsDeploymentConditionType,
	reason managedgitopsv1alpha1.GitOpsDeploymentReasonType, errMessage errors.UserError) error {

	conditions := &g.gitOpsDeployment.Status.Conditions

	// Create a new condition and update the object in k8s with the error message, if err does exist
	if errMessage != nil {

		// We should only show user errors here. If the error doesn't contain a user error (only a dev error),
		// then just return UnknownError.
		userError := errMessage.UserError()
		if userError == "" {
			userError = errors.UnknownError
		}

		g.conditionManager.SetCondition(conditions, conditionType, managedgitopsv1alpha1.GitOpsConditionStatus(corev1.ConditionTrue),
			reason, userError)
		return g.client.Status().Update(g.ctx, g.gitOpsDeployment, &client.UpdateOptions{})

	} else {
		// if error does not exist, check if the condition exists or not
		if g.conditionManager.HasCondition(conditions, conditionType) {
			reason = reason + "Resolved"
			// Check the condition and mark it as resolved, if it's resolved
			if cond, _ := g.conditionManager.FindCondition(conditions, conditionType); cond.Reason != reason {
				g.conditionManager.SetCondition(conditions, conditionType,
					managedgitopsv1alpha1.GitOpsConditionStatus(corev1.ConditionFalse), reason, "")

				return g.client.Status().Update(g.ctx, g.gitOpsDeployment, &client.UpdateOptions{})
			}
			// do nothing, if the condition is already marked as resolved
		}
		// do nothing, if the condition does not exist anymore
	}

	return nil
}

type argoCDSpecInput struct {
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	crName      string
	crNamespace string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	destinationNamespace string
	destinationName      string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	sourceRepoURL        string
	sourcePath           string
	sourceTargetRevision string
	// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
	automated bool

	// Hopefully you are getting the message, here :)
}

func createSpecField(fieldsParam argoCDSpecInput) (string, error) {

	sanitize := func(input string) string {
		input = strings.ReplaceAll(input, "\"", "")
		input = strings.ReplaceAll(input, "'", "")
		input = strings.ReplaceAll(input, "`", "")
		input = strings.ReplaceAll(input, "\r", "")
		input = strings.ReplaceAll(input, "\n", "")
		input = strings.ReplaceAll(input, "&", "")
		input = strings.ReplaceAll(input, ";", "")
		input = strings.ReplaceAll(input, "%", "")

		return input
	}

	fields := argoCDSpecInput{
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
		crName:               sanitize(fieldsParam.crName),
		crNamespace:          sanitize(fieldsParam.crNamespace),
		destinationNamespace: sanitize(fieldsParam.destinationNamespace),
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!
		destinationName:      sanitize(fieldsParam.destinationName),
		sourceRepoURL:        sanitize(fieldsParam.sourceRepoURL),
		sourcePath:           sanitize(fieldsParam.sourcePath),
		sourceTargetRevision: sanitize(fieldsParam.sourceTargetRevision),
		automated:            fieldsParam.automated,
		// MAKE SURE YOU SANITIZE ANY NEW FIELDS THAT ARE ADDED!!!!

		// Hopefully you are getting the message, here :)
	}

	application := fauxargocd.FauxApplication{
		FauxTypeMeta: fauxargocd.FauxTypeMeta{
			Kind:       "Application",
			APIVersion: "argoproj.io/v1alpha1",
		},
		FauxObjectMeta: fauxargocd.FauxObjectMeta{
			Name:      fields.crName,
			Namespace: fields.crNamespace,
		},
		Spec: fauxargocd.FauxApplicationSpec{
			Source: fauxargocd.ApplicationSource{
				RepoURL:        fields.sourceRepoURL,
				Path:           fields.sourcePath,
				TargetRevision: fields.sourceTargetRevision,
			},
			Destination: fauxargocd.ApplicationDestination{
				Name:      fields.destinationName,
				Namespace: fields.destinationNamespace,
			},
			Project: "default",
		},
	}

	if fields.automated {
		application.Spec.SyncPolicy = &fauxargocd.SyncPolicy{
			Automated: &fauxargocd.SyncPolicyAutomated{
				Prune:      true,
				SelfHeal:   true,
				AllowEmpty: true,
			},
		}
	}

	resBytes, err := goyaml.Marshal(application)

	if err != nil {
		return "", err
	}

	return string(resBytes), nil
}

// Decompress byte array received from table to get String and then convert it into ResourceStatus Array.
func decompressResourceData(resourceData []byte) ([]managedgitopsv1alpha1.ResourceStatus, error) {
	var resourceList []managedgitopsv1alpha1.ResourceStatus

	// Decompress data to get actual resource string
	bufferIn := bytes.NewBuffer(resourceData)
	gzipReader, err := gzip.NewReader(bufferIn)

	if err != nil {
		return resourceList, fmt.Errorf("unable to create gzipReader: %v", err)
	}

	var bufferOut bytes.Buffer

	// Using CopyN with For loop to avoid gosec error "Potential DoS vulnerability via decompression bomb",
	// occurred while using below code
	for {
		_, err := io.CopyN(&bufferOut, gzipReader, 131072)
		if err != nil {
			if err == io.EOF {
				break
			}
			return resourceList, fmt.Errorf("unable to convert resource data to string: %v", err)
		}
	}

	if err := gzipReader.Close(); err != nil {
		return resourceList, fmt.Errorf("unable to close gzip reader connection: %v", err)
	}

	// Convert resource string into ResourceStatus Array
	err = goyaml.Unmarshal(bufferOut.Bytes(), &resourceList)
	if err != nil {
		return resourceList, fmt.Errorf("unable to Unmarshal resource data: %v", err)
	}

	return resourceList, nil
}
