package db_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

func ginkgoTestSetup() {
	var deploymentToApplicationMappings []db.DeploymentToApplicationMapping
	var syncOperations []db.SyncOperation
	ctx := context.Background()

	// 'testSetup' deletes all database rows that start with 'test-' in the primary key of the row.
	// This ensures a clean slate for the test run.
	dbq, err := db.NewUnsafePostgresDBQueries(true, true)
	Expect(err).To(BeNil())
	defer dbq.CloseDatabase()
	err = dbq.UnsafeListAllDeploymentToApplicationMapping(ctx, &deploymentToApplicationMappings)
	Expect(err).To(BeNil())
	for _, deploydeploymentToApplicationMapping := range deploymentToApplicationMappings {
		if strings.HasPrefix(deploydeploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id, "test-") {
			rowsAffected, err := dbq.DeleteDeploymentToApplicationMappingByDeplId(ctx, deploydeploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id)
			Expect(err).To(BeNil())
			if err == nil {
				Expect(rowsAffected).To(Equal(1))
			}
		}
	}
	err = dbq.UnsafeListAllSyncOperations(ctx, &syncOperations)
	Expect(err).To(BeNil())
	for _, syncOperation := range syncOperations {
		if strings.HasPrefix(syncOperation.SyncOperation_id, "test-") {
			rowsAffected, err := dbq.DeleteSyncOperationById(ctx, syncOperation.SyncOperation_id)
			Expect(err).To(BeNil())
			if err == nil {
				Expect(rowsAffected).To(Equal(1))
			}
		}
	}
	var applicationStates []db.ApplicationState
	err = dbq.UnsafeListAllApplicationStates(ctx, &applicationStates)
	Expect(err).To(BeNil())
	for _, applicationState := range applicationStates {
		if strings.HasPrefix(applicationState.Applicationstate_application_id, "test-") {
			rowsAffected, err := dbq.DeleteApplicationStateById(ctx, applicationState.Applicationstate_application_id)
			Expect(err).To(BeNil())
			if err == nil {
				Expect(rowsAffected).To(Equal(1))
			}
		}
	}
	var operations []db.Operation
	err = dbq.UnsafeListAllOperations(ctx, &operations)
	Expect(err).To(BeNil())
	for _, operation := range operations {

		if strings.HasPrefix(operation.Operation_id, "test-") {
			rowsAffected, err := dbq.CheckedDeleteOperationById(ctx, operation.Operation_id, operation.Operation_owner_user_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}
	var applications []db.Application
	err = dbq.UnsafeListAllApplications(ctx, &applications)
	Expect(err).To(BeNil())
	for _, application := range applications {
		if strings.HasPrefix(application.Application_id, "test-") {
			rowsAffected, err := dbq.DeleteApplicationById(ctx, application.Application_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}

	var clusterAccess []db.ClusterAccess
	err = dbq.UnsafeListAllClusterAccess(ctx, &clusterAccess)
	Expect(err).To(BeNil())
	for _, clusterAccess := range clusterAccess {
		if strings.HasPrefix(clusterAccess.Clusteraccess_managed_environment_id, "test-") {
			rowsAffected, err := dbq.DeleteClusterAccessById(ctx, clusterAccess.Clusteraccess_user_id,
				clusterAccess.Clusteraccess_managed_environment_id,
				clusterAccess.Clusteraccess_gitops_engine_instance_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}
	var engineInstances []db.GitopsEngineInstance
	err = dbq.UnsafeListAllGitopsEngineInstances(ctx, &engineInstances)
	Expect(err).To(BeNil())
	for _, gitopsEngineInstance := range engineInstances {
		if strings.HasPrefix(gitopsEngineInstance.Gitopsengineinstance_id, "test-") {
			rowsAffected, err := dbq.DeleteGitopsEngineInstanceById(ctx, gitopsEngineInstance.Gitopsengineinstance_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}

	var engineClusters []db.GitopsEngineCluster
	err = dbq.UnsafeListAllGitopsEngineClusters(ctx, &engineClusters)
	Expect(err).To(BeNil())
	for _, engineCluster := range engineClusters {
		if strings.HasPrefix(engineCluster.Gitopsenginecluster_id, "test-") {
			rowsAffected, err := dbq.DeleteGitopsEngineClusterById(ctx, engineCluster.Gitopsenginecluster_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}
	var managedEnvironments []db.ManagedEnvironment
	err = dbq.UnsafeListAllManagedEnvironments(ctx, &managedEnvironments)
	Expect(err).To(BeNil())
	for _, managedEnvironment := range managedEnvironments {
		if strings.HasPrefix(managedEnvironment.Managedenvironment_id, "test-") {
			rowsAffected, err := dbq.DeleteManagedEnvironmentById(ctx, managedEnvironment.Managedenvironment_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}
	var clusterCredentials []db.ClusterCredentials
	err = dbq.UnsafeListAllClusterCredentials(ctx, &clusterCredentials)
	Expect(err).To(BeNil())
	for _, clusterCredential := range clusterCredentials {
		if strings.HasPrefix(clusterCredential.Clustercredentials_cred_id, "test-") {
			rowsAffected, err := dbq.DeleteClusterCredentialsById(ctx, clusterCredential.Clustercredentials_cred_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}
	var clusterUsers []db.ClusterUser
	err = dbq.UnsafeListAllClusterUsers(ctx, &clusterUsers)
	Expect(err).To(BeNil())

	for _, user := range clusterUsers {
		if strings.HasPrefix(user.Clusteruser_id, "test-") {
			rowsAffected, err := dbq.DeleteClusterUserById(ctx, (user.Clusteruser_id))
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}

	err = dbq.CreateClusterUser(ctx, testClusterUser)
	Expect(err).To(BeNil())

	var kubernetesToDBResourceMappings []db.KubernetesToDBResourceMapping
	err = dbq.UnsafeListAllKubernetesResourceToDBResourceMapping(ctx, &kubernetesToDBResourceMappings)
	Expect(err).To(BeNil())
	for _, kubernetesToDBResourceMapping := range kubernetesToDBResourceMappings {
		if strings.HasPrefix(kubernetesToDBResourceMapping.KubernetesResourceUID, "test-") {

			rowsAffected, err := dbq.DeleteKubernetesResourceToDBResourceMapping(ctx, &kubernetesToDBResourceMapping)

			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
		}
	}

}

var _ = Describe("Apicrtodatabasemapping Tests", func() {
	Context("Tests all the DB functions for Apicrtodatabasemapping", func() {
		It("Should execute all Apicrtodatabasemapping Functions", func() {
			ginkgoTestSetup()
			item := db.APICRToDatabaseMapping{
				APIResourceType:      db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentSyncRun,
				APIResourceUID:       "test-k8s-uid",
				APIResourceName:      "test-k8s-name",
				APIResourceNamespace: "test-k8s-namespace",
				WorkspaceUID:         "test-workspace-uid",
				DBRelationType:       db.APICRToDatabaseMapping_DBRelationType_SyncOperation,
				DBRelationKey:        "test-key",
			}
			ctx := context.Background()
			dbq, err := db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())
			defer dbq.CloseDatabase()
			err = dbq.CreateAPICRToDatabaseMapping(ctx, &item)
			Expect(err).To(BeNil())

			fetchRow := db.APICRToDatabaseMapping{
				APIResourceType: item.APIResourceType,
				APIResourceUID:  item.APIResourceUID,
				DBRelationKey:   item.DBRelationKey,
				DBRelationType:  item.DBRelationType,
			}

			err = dbq.GetDatabaseMappingForAPICR(ctx, &fetchRow)
			Expect(err).To(BeNil())
			Expect(fetchRow).Should(Equal(item))

			var items []db.APICRToDatabaseMapping

			err = dbq.ListAPICRToDatabaseMappingByAPINamespaceAndName(ctx, item.APIResourceType, item.APIResourceName, item.APIResourceNamespace, item.WorkspaceUID, item.DBRelationType, &items)
			Expect(err).To(BeNil())
			Expect(items[0]).Should(Equal(item))

			rowsAffected, err := dbq.DeleteAPICRToDatabaseMapping(ctx, &fetchRow)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal((1)))
			fetchRow = db.APICRToDatabaseMapping{
				APIResourceType: item.APIResourceType,
				APIResourceUID:  item.APIResourceUID,
				DBRelationKey:   item.DBRelationKey,
				DBRelationType:  item.DBRelationType,
			}
			err = dbq.GetDatabaseMappingForAPICR(ctx, &fetchRow)
			Expect(db.IsResultNotFoundError(err)).To(Equal(true))
		})
	})
})
