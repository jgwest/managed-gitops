package db_test

import (
	"context"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

func generateUuid() string {
	return uuid.New().String()
}

var _ = Describe("DeploymentToApplicationMapping Tests", func() {
	Context("It should execute all DeploymentToApplicationMapping Functions", func() {
		It("Should execute all DeploymentToApplicationMapping Functions", func() {
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

			deploymentToApplicationMapping := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: "test-" + generateUuid(),
				Application_id:                        application.Application_id,
				DeploymentName:                        "test-deployment",
				DeploymentNamespace:                   "test-namespace",
				WorkspaceUID:                          "demo-workspace",
			}

			err = dbq.CreateDeploymentToApplicationMapping(ctx, deploymentToApplicationMapping)
			Expect(err).To(BeNil())
			fetchRow := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id,
			}
			err = dbq.GetDeploymentToApplicationMappingByDeplId(ctx, fetchRow)
			Expect(err).To(BeNil())
			Expect(fetchRow).To(Equal(deploymentToApplicationMapping))

			rowsAffected, err := dbq.DeleteDeploymentToApplicationMappingByDeplId(ctx, deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).To(Equal(1))
			fetchRow = &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id,
			}
			err = dbq.GetDeploymentToApplicationMappingByDeplId(ctx, fetchRow)
			Expect(true).To(Equal(db.IsResultNotFoundError(err)))
		})
		It("Should Successfully Test ListAll Function", func() {
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

			deploymentToApplicationMapping := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: "test-" + generateUuid(),
				Application_id:                        application.Application_id,
				DeploymentName:                        "test-deployment",
				DeploymentNamespace:                   "test-namespace",
				WorkspaceUID:                          "demo-workspace",
			}

			err = dbq.CreateDeploymentToApplicationMapping(ctx, deploymentToApplicationMapping)
			Expect(err).To(BeNil())
			var dbResults []db.DeploymentToApplicationMapping
			dbResult := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id,
			}

			err = dbq.ListDeploymentToApplicationMappingByWorkspaceUID(ctx, "demo-workspace", &dbResults)
			Expect(err).To(BeNil())
			Expect(len(dbResults)).Should(Equal(1))
			Expect(dbResults[0]).Should(Equal(*deploymentToApplicationMapping))

			err = dbq.ListDeploymentToApplicationMappingByNamespaceAndName(ctx, deploymentToApplicationMapping.DeploymentName, deploymentToApplicationMapping.DeploymentNamespace, deploymentToApplicationMapping.WorkspaceUID, &dbResults)
			Expect(err).To(BeNil())
			Expect(dbResults[0]).Should(Equal(*deploymentToApplicationMapping))

			err = dbq.GetDeploymentToApplicationMappingByDeplId(ctx, dbResult)
			Expect(err).To(BeNil())
			Expect(dbResult).Should(Equal(deploymentToApplicationMapping))
			err = dbq.GetDeploymentToApplicationMappingByApplicationId(ctx, dbResult)
			Expect(err).To(BeNil())
			Expect(dbResult).Should(Equal(deploymentToApplicationMapping))

			rowsAffected, err := dbq.DeleteDeploymentToApplicationMappingByDeplId(ctx, deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id)
			Expect(err).To(BeNil())
			Expect(rowsAffected).Should(Equal(1))
			fetchRow := &db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: deploymentToApplicationMapping.Deploymenttoapplicationmapping_uid_id,
			}
			err = dbq.GetDeploymentToApplicationMappingByDeplId(ctx, fetchRow)
			Expect(true).To(Equal(db.IsResultNotFoundError(err)))
		})
	})
})
