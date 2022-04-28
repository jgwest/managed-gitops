package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

var testClusterUser = &db.ClusterUser{
	Clusteruser_id: "test-user",
	User_name:      "test-user",
}

func generateSampleData() (db.ClusterCredentials, db.ManagedEnvironment, db.GitopsEngineCluster, db.GitopsEngineInstance, db.ClusterAccess) {
	clusterCredentials := db.ClusterCredentials{
		Clustercredentials_cred_id:  "test-cluster-creds-test",
		Host:                        "host",
		Kube_config:                 "kube-config",
		Kube_config_context:         "kube-config-context",
		Serviceaccount_bearer_token: "serviceaccount_bearer_token",
		Serviceaccount_ns:           "Serviceaccount_ns",
	}

	managedEnvironment := db.ManagedEnvironment{
		Managedenvironment_id: "test-managed-env-914",
		Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
		Name:                  "my env",
	}

	gitopsEngineCluster := db.GitopsEngineCluster{
		Gitopsenginecluster_id: "test-fake-cluster-914",
		Clustercredentials_id:  clusterCredentials.Clustercredentials_cred_id,
	}

	gitopsEngineInstance := db.GitopsEngineInstance{
		Gitopsengineinstance_id: "test-fake-engine-instance-id",
		Namespace_name:          "test-fake-namespace",
		Namespace_uid:           "test-fake-namespace-914",
		EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
	}

	clusterAccess := db.ClusterAccess{
		Clusteraccess_user_id:                   testClusterUser.Clusteruser_id,
		Clusteraccess_managed_environment_id:    managedEnvironment.Managedenvironment_id,
		Clusteraccess_gitops_engine_instance_id: gitopsEngineInstance.Gitopsengineinstance_id,
	}

	return clusterCredentials, managedEnvironment, gitopsEngineCluster, gitopsEngineInstance, clusterAccess
}

func createSampleData(dbq db.AllDatabaseQueries) (*db.ClusterCredentials, *db.ManagedEnvironment, *db.GitopsEngineCluster, *db.GitopsEngineInstance, *db.ClusterAccess, error) {

	ctx := context.Background()
	var err error

	clusterCredentials, managedEnvironment, engineCluster, engineInstance, clusterAccess := generateSampleData()

	if err = dbq.CreateClusterCredentials(ctx, &clusterCredentials); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	if err = dbq.CreateManagedEnvironment(ctx, &managedEnvironment); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	if err = dbq.CreateGitopsEngineCluster(ctx, &engineCluster); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	if err = dbq.CreateGitopsEngineInstance(ctx, &engineInstance); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	if err = dbq.CreateClusterAccess(ctx, &clusterAccess); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	return &clusterCredentials, &managedEnvironment, &engineCluster, &engineInstance, &clusterAccess, nil

}

var _ = Describe("ApplicationStates Tests", func() {
	Context("It should execute all DB functions for ApplicationStates", func() {
		It("Should execute all ApplicationStates Functions", func() {
			ginkgoTestSetup()
			ctx := context.Background()

			dbq, err := db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())
			defer dbq.CloseDatabase()
			_, managedEnvironment, _, gitopsEngineInstance, _, err := createSampleData(dbq)
			Expect(err).To(BeNil())

			application := &db.Application{
				Application_id:          "test-my-application",
				Name:                    "my-application",
				Spec_field:              "{}",
				Engine_instance_inst_id: gitopsEngineInstance.Gitopsengineinstance_id,
				Managed_environment_id:  managedEnvironment.Managedenvironment_id,
			}

			err = dbq.CreateApplication(ctx, application)
			Expect(err).To(BeNil())

			applicationState := &db.ApplicationState{
				Applicationstate_application_id: application.Application_id,
				Health:                          "Progressing",
				Sync_Status:                     "Unknown",
			}

			err = dbq.CreateApplicationState(ctx, applicationState)
			Expect(err).To(BeNil())

			fetchObj := &db.ApplicationState{
				Applicationstate_application_id: application.Application_id,
			}
			err = dbq.GetApplicationStateById(ctx, fetchObj)
			Expect(err).To(BeNil())
			Expect(fetchObj).Should(Equal(applicationState))

			applicationState.Health = "Healthy"
			applicationState.Sync_Status = "Synced"
			err = dbq.UpdateApplicationState(ctx, applicationState)
			Expect(err).To(BeNil())

			err = dbq.GetApplicationStateById(ctx, fetchObj)
			Expect(err).To(BeNil())
			Expect(fetchObj).Should(Equal(applicationState))

			rowsAffected, err := dbq.DeleteApplicationStateById(ctx, fetchObj.Applicationstate_application_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
			err = dbq.GetApplicationStateById(ctx, fetchObj)
			Expect(db.IsResultNotFoundError(err)).To(Equal(true))
		})
	})
})
