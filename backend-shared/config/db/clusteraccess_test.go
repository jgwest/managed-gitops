package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

var _ = Describe("ClusterAccess Tests", func() {
	Context("It should execute all DB functions for ClusterAccess", func() {
		It("Should execute all ClusterAccess Functions", func() {
			ginkgoTestSetup()
			dbq, err := db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())
			defer dbq.CloseDatabase()

			ctx := context.Background()

			var clusterUser = &db.ClusterUser{
				Clusteruser_id: "test-user-application",
				User_name:      "test-user-application",
			}
			err = dbq.CreateClusterUser(ctx, clusterUser)
			Expect(err).To(BeNil())

			clusterCredentials := db.ClusterCredentials{
				Clustercredentials_cred_id:  "test-cluster-creds-test-5",
				Host:                        "host",
				Kube_config:                 "kube-config",
				Kube_config_context:         "kube-config-context",
				Serviceaccount_bearer_token: "serviceaccount_bearer_token",
				Serviceaccount_ns:           "Serviceaccount_ns",
			}

			managedEnvironment := db.ManagedEnvironment{
				Managedenvironment_id: "test-managed-env-5",
				Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
				Name:                  "my env",
			}

			gitopsEngineCluster := db.GitopsEngineCluster{
				Gitopsenginecluster_id: "test-fake-cluster-5",
				Clustercredentials_id:  clusterCredentials.Clustercredentials_cred_id,
			}

			gitopsEngineInstance := db.GitopsEngineInstance{
				Gitopsengineinstance_id: "test-fake-engine-instance-id",
				Namespace_name:          "test-fake-namespace",
				Namespace_uid:           "test-fake-namespace-5",
				EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
			}

			clusterAccess := db.ClusterAccess{
				Clusteraccess_user_id:                   clusterUser.Clusteruser_id,
				Clusteraccess_managed_environment_id:    managedEnvironment.Managedenvironment_id,
				Clusteraccess_gitops_engine_instance_id: gitopsEngineInstance.Gitopsengineinstance_id,
			}

			err = dbq.CreateClusterCredentials(ctx, &clusterCredentials)
			Expect(err).To(BeNil())

			err = dbq.CreateManagedEnvironment(ctx, &managedEnvironment)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineCluster(ctx, &gitopsEngineCluster)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineInstance(ctx, &gitopsEngineInstance)
			Expect(err).To(BeNil())

			err = dbq.CreateClusterAccess(ctx, &clusterAccess)
			Expect(err).To(BeNil())
			fetchRow := db.ClusterAccess{Clusteraccess_user_id: clusterAccess.Clusteraccess_user_id,
				Clusteraccess_managed_environment_id:    clusterAccess.Clusteraccess_managed_environment_id,
				Clusteraccess_gitops_engine_instance_id: clusterAccess.Clusteraccess_gitops_engine_instance_id}
			err = dbq.GetClusterAccessByPrimaryKey(ctx, &fetchRow)
			Expect(err).To(BeNil())
			Expect(fetchRow).Should(Equal(clusterAccess))

			affectedRows, err := dbq.DeleteClusterAccessById(ctx, fetchRow.Clusteraccess_user_id, fetchRow.Clusteraccess_managed_environment_id, fetchRow.Clusteraccess_gitops_engine_instance_id)
			Expect(err).To(BeNil())
			Expect(affectedRows).To(Equal(1))

			err = dbq.GetClusterAccessByPrimaryKey(ctx, &fetchRow)
			Expect(db.IsResultNotFoundError(err)).To(Equal(true))
		})
	})
})
