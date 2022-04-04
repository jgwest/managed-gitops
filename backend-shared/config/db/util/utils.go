package util

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	v1 "k8s.io/api/core/v1"
)

// Whenever a new Argo CD Application needs to be created, we need to find an Argo CD instance
// that is available to use it. In the future, when we have multiple instances, there would
// be an algorithm that intelligently places applications -> instances, to ensure that
// there are no Argo CD instances that are overloaded (have too many users).
//
// However, at the moment we are using a single shared Argo CD instance in 'argocd', so we will
// just return that.
//
// This logic would be improved by https://issues.redhat.com/browse/GITOPSRVCE-73 (and others)
const DefaultGitOpsEngineSingleInstanceNamespace = "argocd"

func GetGitOpsEngineSingleInstanceNamespace() string {

	argoEnv := os.Getenv("ARGO_CD_NAMESPACE")
	if len(strings.TrimSpace(argoEnv)) != 0 {
		return argoEnv
	}

	return DefaultGitOpsEngineSingleInstanceNamespace
}

// The bool return value is 'true' if ManagedEnvironment is created; 'false' if it already exists in DB or in case of failure.
func GetOrCreateManagedEnvironmentByNamespaceUID(ctx context.Context, workspaceNamespace v1.Namespace,
	dbq db.DatabaseQueries, log logr.Logger) (*db.ManagedEnvironment, bool, error) {

	workspaceNamespaceUID := string(workspaceNamespace.UID)

	var err error

	// Is the namespace we are deploying to already mapped to a ManagedEnvironment in the database?
	// - Attempt to retrieve the relationship DB entry
	dbResourceMapping := &db.KubernetesToDBResourceMapping{
		KubernetesResourceType: db.K8sToDBMapping_Namespace,
		KubernetesResourceUID:  string(workspaceNamespaceUID),
		DBRelationType:         db.K8sToDBMapping_ManagedEnvironment}
	err = dbq.GetDBResourceMappingForKubernetesResource(ctx, dbResourceMapping)

	if err == nil {
		// If there already exists a managed environment for this workspace, return it

		// Retrieve the managed environment database entry for this resource
		managedEnvironment := db.ManagedEnvironment{Managedenvironment_id: string(dbResourceMapping.DBRelationKey)}
		err = dbq.GetManagedEnvironmentById(ctx, &managedEnvironment)

		if err == nil {
			// Success: mapping exists, and so does the environment it points to
			return &managedEnvironment, false, nil
		}

		if !db.IsResultNotFoundError(err) {
			// Failure: A generic error occurred.
			return nil, false, fmt.Errorf("unable to retrieve resource mapping: %v", err)
		}

		// At this point, we found the mapping, but didn't find the ManagedEnvironment that the
		// mapping pointed to.

		// Since the managed environment doesn't exist, delete the mapping; it will be recreated below.
		if _, err := dbq.DeleteKubernetesResourceToDBResourceMapping(ctx, dbResourceMapping); err != nil {
			return nil, false, fmt.Errorf("unable to delete K8s resource to DB mapping: %v", dbResourceMapping)
		} else {
			log.V(util.LogLevel_Debug).Info("Deleted KubernetesResourceToDBResourceMapping: " + fmt.Sprintf("%v", dbResourceMapping))
		}

	} else if !db.IsResultNotFoundError(err) {
		return nil, false, fmt.Errorf("unable to retrieve resource mapping: %v", err)
	}

	// At this point in the function, both the managed environment and mapping necessarily don't exist

	// Create cluster credentials for the managed env
	// TODO: GITOPSRVCE-66 - Cluster credentials placeholder values - we will need to create a service account on the target cluster, which we can store in the database.

	clusterCreds := db.ClusterCredentials{
		Host:                        "host",
		Kube_config:                 "kube_config",
		Kube_config_context:         "kube_config_context",
		Serviceaccount_bearer_token: "serviceaccount_bearer_token",
		Serviceaccount_ns:           "serviceaccount_ns",
	}
	if err := dbq.CreateClusterCredentials(ctx, &clusterCreds); err != nil {
		return nil, false, fmt.Errorf("unable to create cluster creds for managed env: %v", err)
	} else {
		log.V(util.LogLevel_Debug).Info("Created Cluster Credentials: " + clusterCreds.Clustercredentials_cred_id)
	}

	managedEnvironment := db.ManagedEnvironment{
		Name:                  "Managed Environment for " + workspaceNamespace.Name,
		Clustercredentials_id: clusterCreds.Clustercredentials_cred_id,
	}
	if err := dbq.CreateManagedEnvironment(ctx, &managedEnvironment); err != nil {
		return nil, false, fmt.Errorf("unable to create managed env: %v", err)
	} else {
		log.V(util.LogLevel_Debug).Info("Created Managed Environment: " + managedEnvironment.Managedenvironment_id)
	}

	dbResourceMapping = &db.KubernetesToDBResourceMapping{
		KubernetesResourceType: db.K8sToDBMapping_Namespace,
		KubernetesResourceUID:  string(workspaceNamespaceUID),
		DBRelationType:         db.K8sToDBMapping_ManagedEnvironment,
		DBRelationKey:          managedEnvironment.Managedenvironment_id,
	}

	if err := dbq.CreateKubernetesResourceToDBResourceMapping(ctx, dbResourceMapping); err != nil {
		return nil, false, fmt.Errorf("unable to create KubernetesResourceToDBResourceMapping: %v", err)
	} else {
		log.V(util.LogLevel_Debug).Info("Created KubernetesResourceToDBResourceMapping: " + dbResourceMapping.KubernetesResourceUID)
	}

	return &managedEnvironment, true, nil
}

// GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID gets (or creates it if it doesn't exist) a GitOpsEngineInstance database entry that
// corresponds to an GitOps engine instance running on the cluster.
// The bool return value is 'true' if GitopsEngineInstance is created; 'false' if it already exists in DB or in case of failure.
func GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID(ctx context.Context,
	gitopsEngineNamespace v1.Namespace, kubesystemNamespaceUID string,
	dbq db.DatabaseQueries, log logr.Logger) (*db.GitopsEngineInstance, bool, *db.GitopsEngineCluster, error) {

	// First create the GitOpsEngine cluster if needed; this will be used to create the instance.
	gitopsEngineCluster, err := GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx, kubesystemNamespaceUID, dbq, log)
	if err != nil {
		return nil, false, nil, fmt.Errorf("unable to create GitOpsEngineCluster for '%v', error: '%v'", kubesystemNamespaceUID, err)
	}

	var gitopsEngineInstance *db.GitopsEngineInstance

	expectedDBResourceMapping := db.KubernetesToDBResourceMapping{
		KubernetesResourceType: db.K8sToDBMapping_Namespace,
		KubernetesResourceUID:  kubesystemNamespaceUID,
		DBRelationType:         db.K8sToDBMapping_GitopsEngineInstance,
	}

	var dbResourceMapping *db.KubernetesToDBResourceMapping
	{
		var expectedDBResourceMappingCopy db.KubernetesToDBResourceMapping = expectedDBResourceMapping
		dbResourceMapping = &expectedDBResourceMappingCopy
	}

	if err := dbq.GetDBResourceMappingForKubernetesResource(ctx, dbResourceMapping); err != nil {

		if !db.IsResultNotFoundError(err) {
			return nil, false, nil, fmt.Errorf("unable to get DBResourceMapping for getOrCreateGitopsEngineInstanceByInstanceNamespaceUID: %v", err)
		}

		dbResourceMapping = nil

	} else {
		// If there exists a db resource mapping for this cluster, see if we can get the GitOpsEngineCluster
		gitopsEngineInstance = &db.GitopsEngineInstance{
			Gitopsengineinstance_id: dbResourceMapping.DBRelationKey,
		}

		if err := dbq.GetGitopsEngineInstanceById(ctx, gitopsEngineInstance); err != nil {
			if !db.IsResultNotFoundError(err) {
				return nil, false, nil, err
			}

			log.V(util.LogLevel_Warn).Error(nil, "GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID found a resource mapping, but no engine instance.")

			// We have found a mapping without the corresponding mapped entity, so delete the mapping.
			// (We will recreate the mapping below)
			if _, err := dbq.DeleteKubernetesResourceToDBResourceMapping(ctx, dbResourceMapping); err != nil {
				return nil, false, nil, err
			} else {
				log.V(util.LogLevel_Debug).Info("Deleted KubernetesResourceToDBResourceMapping: " + fmt.Sprintf("%v", dbResourceMapping))
			}

			gitopsEngineInstance = nil
			dbResourceMapping = nil
		} else {

			if gitopsEngineInstance.EngineCluster_id != gitopsEngineCluster.Gitopsenginecluster_id {
				return nil, false, nil, fmt.Errorf("able to locate engine instance, and engine cluster, but they mismatched: instance id: %v, cluster id: %v",
					gitopsEngineInstance.Gitopsengineinstance_id, gitopsEngineCluster.Gitopsenginecluster_id)
			}

			// Success: both existed.
			return gitopsEngineInstance, false, gitopsEngineCluster, nil
		}
	}

	if dbResourceMapping == nil && gitopsEngineInstance == nil {
		// Scenario A) neither exists: create both

		gitopsEngineInstance = &db.GitopsEngineInstance{
			Namespace_name:   gitopsEngineNamespace.Name,
			Namespace_uid:    string(gitopsEngineNamespace.UID),
			EngineCluster_id: gitopsEngineCluster.Gitopsenginecluster_id,
		}

		if err := dbq.CreateGitopsEngineInstance(ctx, gitopsEngineInstance); err != nil {
			return nil, false, nil, fmt.Errorf("unable to create engine instance, when neither existed: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created GitopsEngineInstance: " + gitopsEngineInstance.Gitopsengineinstance_id)
		}

		expectedDBResourceMapping.DBRelationKey = gitopsEngineInstance.Gitopsengineinstance_id
		if err := dbq.CreateKubernetesResourceToDBResourceMapping(ctx, &expectedDBResourceMapping); err != nil {
			return nil, false, nil, fmt.Errorf("unable to create mapping when neither existed: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created KubernetesResourceToDBResourceMapping with KubernetesResourceUID: " + expectedDBResourceMapping.KubernetesResourceUID)
		}

		return gitopsEngineInstance, true, gitopsEngineCluster, nil

	} else if dbResourceMapping != nil && gitopsEngineInstance == nil {
		// Scenario B) this shouldn't happen: the above logic should ensure that dbResourceMapping is always nil, if gitopsEngineInstance is nil
		return nil, false, nil, fmt.Errorf("SEVERE: the dbResourceMapping existed, but the gitops engine instance did not")

	} else if dbResourceMapping == nil && gitopsEngineInstance != nil {
		// Scenario C) this will happen if the instance exists, but there is no mapping for it

		expectedDBResourceMapping.DBRelationKey = gitopsEngineInstance.Gitopsengineinstance_id
		if err := dbq.CreateKubernetesResourceToDBResourceMapping(ctx, &expectedDBResourceMapping); err != nil {
			return nil, false, nil, fmt.Errorf("unable to create mapping when dbResourceMapping didn't exist: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created KubernetesResourceToDBResourceMapping with KubernetesResourceUID: " + expectedDBResourceMapping.KubernetesResourceUID)
		}

		return gitopsEngineInstance, false, gitopsEngineCluster, nil

	} else if dbResourceMapping != nil && gitopsEngineInstance != nil {
		// Scenario D) both exist, so just return the cluster

		if gitopsEngineInstance.EngineCluster_id != gitopsEngineCluster.Gitopsenginecluster_id {
			return nil, false, nil, fmt.Errorf("able to locate engine instance, and engine cluster, but they mismatched: instance id: %v, cluster id: %v",
				gitopsEngineInstance.Gitopsengineinstance_id, gitopsEngineCluster.Gitopsenginecluster_id)
		}

		return gitopsEngineInstance, false, gitopsEngineCluster, nil

	} else {
		return nil, false, nil, fmt.Errorf("unexpected state in GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID")
	}

}

// GetGitopsEngineClusterByKubeSystemNamespaceUID returns nil (with no error) if the cluster could not be found.
func GetGitopsEngineClusterByKubeSystemNamespaceUID(ctx context.Context, kubesystemNamespaceUID string, dbq db.DatabaseQueries,
	log logr.Logger) (*db.GitopsEngineCluster, error) {

	var gitopsEngineCluster *db.GitopsEngineCluster

	expectedDBResourceMapping := db.KubernetesToDBResourceMapping{
		KubernetesResourceType: db.K8sToDBMapping_Namespace,
		KubernetesResourceUID:  kubesystemNamespaceUID,
		DBRelationType:         db.K8sToDBMapping_GitopsEngineCluster,
	}

	var dbResourceMapping *db.KubernetesToDBResourceMapping
	{
		var expectedDBResourceMappingCopy db.KubernetesToDBResourceMapping = expectedDBResourceMapping
		dbResourceMapping = &expectedDBResourceMappingCopy
	}

	if err := dbq.GetDBResourceMappingForKubernetesResource(ctx, dbResourceMapping); err != nil {

		if !db.IsResultNotFoundError(err) {
			return nil, err
		}
		return nil, nil

	} else {
		// If there exists a db resource mapping for this cluster, see if we can get the GitOpsEngineCluster
		gitopsEngineCluster = &db.GitopsEngineCluster{
			Gitopsenginecluster_id: dbResourceMapping.DBRelationKey,
		}

		if err := dbq.GetGitopsEngineClusterById(ctx, gitopsEngineCluster); err != nil {
			if !db.IsResultNotFoundError(err) {
				return nil, err
			}

			return nil, nil
		} else {
			// Success: both existed.
			return gitopsEngineCluster, nil
		}
	}

}

// GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID gets (or creates it if it doesn't exist) a GitOpsEngineCluster
// database entry that corresponds to an GitOps engine cluster.
//
// In order to determine which cluster we are on, we use the UID of the 'kube-system' namespace.
func GetOrCreateGitopsEngineClusterByKubeSystemNamespaceUID(ctx context.Context, kubesystemNamespaceUID string, dbq db.DatabaseQueries, log logr.Logger) (*db.GitopsEngineCluster, error) {

	var gitopsEngineCluster *db.GitopsEngineCluster

	expectedDBResourceMapping := db.KubernetesToDBResourceMapping{
		KubernetesResourceType: db.K8sToDBMapping_Namespace,
		KubernetesResourceUID:  kubesystemNamespaceUID,
		DBRelationType:         db.K8sToDBMapping_GitopsEngineCluster,
	}

	var dbResourceMapping *db.KubernetesToDBResourceMapping
	{
		var expectedDBResourceMappingCopy db.KubernetesToDBResourceMapping = expectedDBResourceMapping
		dbResourceMapping = &expectedDBResourceMappingCopy
	}

	if err := dbq.GetDBResourceMappingForKubernetesResource(ctx, dbResourceMapping); err != nil {

		if !db.IsResultNotFoundError(err) {
			return nil, fmt.Errorf("unable to get DBResourceMapping: %v", err)
		}

		dbResourceMapping = nil

	} else {
		// If there exists a db resource mapping for this cluster, see if we can get the GitOpsEngineCluster
		gitopsEngineCluster = &db.GitopsEngineCluster{
			Gitopsenginecluster_id: dbResourceMapping.DBRelationKey,
		}

		if err := dbq.GetGitopsEngineClusterById(ctx, gitopsEngineCluster); err != nil {
			if !db.IsResultNotFoundError(err) {
				return nil, err
			}

			log.V(util.LogLevel_Warn).Error(nil, "GetOrCreateGitopsEngineInstanceByInstanceNamespaceUID found a resource mapping, but no engine instance.")

			// We have found a mapping without the corresponding mapped entity, so delete the mapping.
			// (We will recreate the mapping below)
			if _, err := dbq.DeleteKubernetesResourceToDBResourceMapping(ctx, dbResourceMapping); err != nil {
				return nil, err
			} else {
				log.V(util.LogLevel_Debug).Info("Deleted KubernetesResourceToDBResourceMapping: " + fmt.Sprintf("%v", dbResourceMapping))
			}

			gitopsEngineCluster = nil
			dbResourceMapping = nil
		} else {
			// Success: both existed.
			return gitopsEngineCluster, nil
		}
	}

	if dbResourceMapping == nil && gitopsEngineCluster == nil {
		// Scenario A) neither exists: create both

		// Create cluster credentials for the managed env
		// TODO: GITOPSRVCE-66 - Cluster credentials placeholder values - we will need to create a service account on the target cluster, which we can store in the database.
		clusterCreds := db.ClusterCredentials{
			Host:                        "host",
			Kube_config:                 "kube_config",
			Kube_config_context:         "kube_config_context",
			Serviceaccount_bearer_token: "serviceaccount_bearer_token",
			Serviceaccount_ns:           "serviceaccount_ns",
		}
		if err := dbq.CreateClusterCredentials(ctx, &clusterCreds); err != nil {
			return nil, fmt.Errorf("unable to create cluster creds for managed env: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created Cluster Credentials: " + clusterCreds.Clustercredentials_cred_id)
		}

		gitopsEngineCluster = &db.GitopsEngineCluster{
			Clustercredentials_id: clusterCreds.Clustercredentials_cred_id,
		}

		if err := dbq.CreateGitopsEngineCluster(ctx, gitopsEngineCluster); err != nil {
			return nil, fmt.Errorf("unable to create engine cluster, when neither existed: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created GitopsEngineCluster: " + gitopsEngineCluster.Gitopsenginecluster_id)
		}

		expectedDBResourceMapping.DBRelationKey = gitopsEngineCluster.Gitopsenginecluster_id
		if err := dbq.CreateKubernetesResourceToDBResourceMapping(ctx, &expectedDBResourceMapping); err != nil {
			return nil, fmt.Errorf("unable to create mapping when neither existed: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created KubernetesResourceToDBResourceMapping with DBRelationKey: " + expectedDBResourceMapping.DBRelationKey)
		}

		return gitopsEngineCluster, nil

	} else if dbResourceMapping != nil && gitopsEngineCluster == nil {
		// Scenario B) This shouldn't happen: the above logic should ensure that dbResourceMapping is always nil, if gitopsEngineCluster is nil

		err := fmt.Errorf("SEVERE: the dbResourceMapping existed, but the gitops engine cluster did not")

		log.Error(err, err.Error())

		return nil, err

	} else if dbResourceMapping == nil && gitopsEngineCluster != nil {
		// Scenario C) this will happen if the engine cluster db entry exists, but there is no mapping for it

		expectedDBResourceMapping.DBRelationKey = gitopsEngineCluster.Gitopsenginecluster_id
		if err := dbq.CreateKubernetesResourceToDBResourceMapping(ctx, &expectedDBResourceMapping); err != nil {
			return nil, fmt.Errorf("unable to create mapping when dbResourceMapping didn't exist: %v", err)
		} else {
			log.V(util.LogLevel_Debug).Info("Created KubernetesResourceToDBResourceMapping with DBRelationKey: " + expectedDBResourceMapping.DBRelationKey)
		}

		return gitopsEngineCluster, nil

	} else if dbResourceMapping != nil && gitopsEngineCluster != nil {
		// Scenario D) both exist, so just return the cluster
		return gitopsEngineCluster, nil
	}

	return nil, fmt.Errorf("unexpected return")

}

func GetOrCreateDeploymentToApplicationMapping(ctx context.Context, createDeplToAppMapping *db.DeploymentToApplicationMapping, dbq db.ApplicationScopedQueries, log logr.Logger) error {

	if err := dbq.GetDeploymentToApplicationMappingByDeplId(ctx, createDeplToAppMapping); err != nil {
		if !db.IsResultNotFoundError(err) {
			logErr := fmt.Errorf("unable to get obj in GetOrDeploymentToApplicationMapping: %v", err)
			log.Error(logErr, "unable to get deplToApp mapping", "createDeplToAppMapping", createDeplToAppMapping)
			return logErr
		}
		// Object not found, so continue.
	} else {
		// Object was found
		return nil
	}

	// Ensure that there isn't an old depltoappmapping hanging around with matching name/namespace, before we create a new one.
	if _, err := dbq.DeleteDeploymentToApplicationMappingByNamespaceAndName(ctx,
		createDeplToAppMapping.DeploymentName,
		createDeplToAppMapping.DeploymentNamespace,
		createDeplToAppMapping.WorkspaceUID); err != nil {
		log.Error(err, "unable to delete old deployment to application mapping for name '"+
			createDeplToAppMapping.DeploymentName+"', namespace '"+createDeplToAppMapping.DeploymentNamespace+"'")
		return err
	} else {
		log.V(util.LogLevel_Debug).Info(fmt.Sprintf("Deleted DeploymentToApplicationMappingByNamespaceAndName with NameSpace: %s and Name: %s", createDeplToAppMapping.DeploymentNamespace, createDeplToAppMapping.DeploymentName))
	}

	if err := dbq.CreateDeploymentToApplicationMapping(ctx, createDeplToAppMapping); err != nil {
		log.Error(err, "unable to create deplToApp mapping", "createDeplToAppMapping", createDeplToAppMapping)
		return err
	} else {
		log.V(util.LogLevel_Debug).Info("Created DeploymentToApplicationMapping: " + createDeplToAppMapping.Deploymenttoapplicationmapping_uid_id)
	}

	return nil
}

// DisposeResources delets of a list of database entries in reverse order.
func DisposeResources(ctx context.Context, resources []db.DisposableResource, dbq db.DatabaseQueries, log logr.Logger) {

	if len(resources) == 0 {
		return
	}

	var err error

	for idx := len(resources) - 1; idx >= 0; idx-- {

		resource := resources[idx]
		if resource == nil {
			continue
		}

		log.V(util.LogLevel_Debug).Info(fmt.Sprintf("disposing of resource: %v", resource))

		disposeErr := resource.Dispose(ctx, dbq)
		if disposeErr != nil {
			if err == nil {
				err = disposeErr
			} else {
				// append the error to the existing error
				err = fmt.Errorf("error: %v. error: %v", disposeErr, err)
			}
		}
	}

	if err != nil {
		log.Error(err, "unable to delete old resources after operation")
	}
}

// DisposeApplicationScopedResources delets of a list of database entries in reverse order.
func DisposeApplicationScopedResources(ctx context.Context, resources []db.AppScopedDisposableResource, dbq db.ApplicationScopedQueries, log logr.Logger) {

	if len(resources) == 0 {
		return
	}

	var err error

	for idx := len(resources) - 1; idx >= 0; idx-- {

		resource := resources[idx]
		if resource == nil {
			continue
		}

		log.V(util.LogLevel_Debug).Info(fmt.Sprintf("disposing of resource: %v", resource))

		disposeErr := resource.DisposeAppScoped(ctx, dbq)
		if disposeErr != nil {
			if err == nil {
				err = disposeErr
			} else {
				// append the error to the existing error
				err = fmt.Errorf("error: %v. error: %v", disposeErr, err)
			}
		}
	}

	if err != nil {
		log.Error(err, "unable to delete old resources after operation")
	}
}
