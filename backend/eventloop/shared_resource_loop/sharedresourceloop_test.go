package shared_resource_loop

import (
	"context"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/tests"

	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Used to list down resources for deletion which are created while running tests.
type testResources struct {
	clusterAccess           *db.ClusterAccess
	Clusteruser_id          string
	Managedenvironment_id   string
	Gitopsengineinstance_id string
	EngineCluster_id        string
	Clustercredentials_id   []string
}

var _ = Describe("SharedResourceEventLoop Test", func() {

	// This will be used by AfterEach to clean resources
	var resourcesToBeDeleted testResources

	var ctx context.Context
	var k8sClient *sharedutil.ProxyClient
	var namespace *v1.Namespace

	l := log.FromContext(context.Background())

	Context("Shared Resource Event Loop test", func() {

		// Create a fake k8s client before each test
		BeforeEach(func() {
			ctx = context.Background()
			scheme,
				argocdNamespace,
				kubesystemNamespace,
				namespaceTemp, err := tests.GenericTestSetup()

			Expect(err).To(BeNil())

			namespace = namespaceTemp

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: namespace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			k8sClientOuter := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, namespace, argocdNamespace, kubesystemNamespace).
				Build()

			k8sClient = &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			// After each test delete the resources created by it.
			DeferCleanup(func() {
				dbq, err := db.NewUnsafePostgresDBQueries(true, true)
				Expect(err).To(BeNil())

				defer dbq.CloseDatabase()

				// Delete clusterAccess
				if resourcesToBeDeleted.clusterAccess != nil {
					rowsAffected, err := dbq.DeleteClusterAccessById(ctx,
						resourcesToBeDeleted.clusterAccess.Clusteraccess_user_id,
						resourcesToBeDeleted.clusterAccess.Clusteraccess_managed_environment_id,
						resourcesToBeDeleted.clusterAccess.Clusteraccess_gitops_engine_instance_id)

					Expect(rowsAffected).To(Equal(1))
					Expect(err).To(BeNil())
				}

				// Delete clusterUser
				if resourcesToBeDeleted.Clusteruser_id != "" {
					rowsAffected, err := dbq.DeleteClusterUserById(ctx, resourcesToBeDeleted.Clusteruser_id)
					Expect(rowsAffected).To(Equal(1))
					Expect(err).To(BeNil())
				}

				// Delete managedEnv
				if resourcesToBeDeleted.Managedenvironment_id != "" {
					rowsAffected, err := dbq.DeleteManagedEnvironmentById(ctx, resourcesToBeDeleted.Managedenvironment_id)
					Expect(rowsAffected).To(Equal(1))
					Expect(err).To(BeNil())
				}

				// Delete engineInstance
				if resourcesToBeDeleted.Gitopsengineinstance_id != "" {
					rowsAffected, err := dbq.DeleteGitopsEngineInstanceById(ctx, resourcesToBeDeleted.Gitopsengineinstance_id)
					Expect(rowsAffected).To(Equal(1))
					Expect(err).To(BeNil())
				}

				// Delete engineCluster
				if resourcesToBeDeleted.EngineCluster_id != "" {
					rowsAffected, err := dbq.DeleteGitopsEngineClusterById(ctx, resourcesToBeDeleted.EngineCluster_id)
					Expect(rowsAffected).To(Equal(1))
					Expect(err).To(BeNil())
				}

				// Delete clusterCredentials
				if len(resourcesToBeDeleted.Clustercredentials_id) != 0 {
					for _, clustercredentials_id := range resourcesToBeDeleted.Clustercredentials_id {
						rowsAffected, err := dbq.DeleteClusterCredentialsById(ctx, clustercredentials_id)
						Expect(rowsAffected).To(Equal(1))
						Expect(err).To(BeNil())
					}
				}

			})
		})

		It("Should create or fetch a user by Namespace id.", func() {

			sharedResourceEventLoop := &SharedResourceEventLoop{inputChannel: make(chan sharedResourceLoopMessage)}

			go internalSharedResourceEventLoop(sharedResourceEventLoop.inputChannel)

			// At first assuming there are no existing users, hence creating new.
			usrOld,
				isNewUser,
				err := sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(usrOld).NotTo(BeNil())
			Expect(isNewUser).To(BeTrue())

			// User is created in previous call, then same user should be returned instead of creating new.
			usrNew,
				isNewUser,
				err := sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(usrNew).NotTo(BeNil())
			Expect(isNewUser).To(BeFalse())
			Expect(usrOld).To(Equal(usrNew))

			// To be used by AfterEach to clean up the resources created by test
			resourcesToBeDeleted = testResources{Clusteruser_id: usrNew.Clusteruser_id}
		})

		It("Should create or fetch resources.", func() {
			sharedResourceEventLoop := &SharedResourceEventLoop{inputChannel: make(chan sharedResourceLoopMessage)}

			go internalSharedResourceEventLoop(sharedResourceEventLoop.inputChannel)

			// At first assuming there are no existing resources, hence creating new.
			sharedResourceOld, err := sharedResourceEventLoop.ReconcileSharedManagedEnv(ctx, k8sClient, *namespace, "", "",
				true, MockSRLK8sClientFactory{fakeClient: k8sClient}, l)

			Expect(err).To(BeNil())
			Expect(sharedResourceOld.ClusterUser).NotTo(BeNil())
			Expect(sharedResourceOld.ManagedEnv).NotTo(BeNil())
			Expect(sharedResourceOld.GitopsEngineInstance).NotTo(BeNil())
			Expect(sharedResourceOld.ClusterAccess).NotTo(BeNil())

			Expect(sharedResourceOld.IsNewUser).To(BeTrue())
			Expect(sharedResourceOld.IsNewManagedEnv).To(BeTrue())
			Expect(sharedResourceOld.IsNewInstance).To(BeTrue())
			Expect(sharedResourceOld.IsNewClusterAccess).To(BeTrue())

			// Resources are created in previous call, then same resources should be returned instead of creating new.
			sharedResourceNew, err := sharedResourceEventLoop.ReconcileSharedManagedEnv(ctx, k8sClient, *namespace, "", "",
				true, MockSRLK8sClientFactory{fakeClient: k8sClient}, l)

			Expect(err).To(BeNil())
			Expect(sharedResourceNew.ClusterUser).NotTo(BeNil())
			Expect(sharedResourceNew.ManagedEnv).NotTo(BeNil())
			Expect(sharedResourceNew.GitopsEngineInstance).NotTo(BeNil())
			Expect(sharedResourceNew.ClusterAccess).NotTo(BeNil())

			Expect(sharedResourceNew.IsNewUser).To(BeFalse())
			Expect(sharedResourceNew.IsNewManagedEnv).To(BeFalse())
			Expect(sharedResourceNew.IsNewInstance).To(BeFalse())
			Expect(sharedResourceNew.IsNewClusterAccess).To(BeFalse())

			Expect(sharedResourceOld.ClusterUser).To(Equal(sharedResourceNew.ClusterUser))
			Expect(sharedResourceOld.ManagedEnv).To(Equal(sharedResourceNew.ManagedEnv))
			Expect(sharedResourceOld.GitopsEngineInstance).To(Equal(sharedResourceNew.GitopsEngineInstance))
			Expect(sharedResourceOld.ClusterAccess).To(Equal(sharedResourceNew.ClusterAccess))

			// To be used by AfterEach to clean up the resources created by test
			resourcesToBeDeleted = testResources{
				Clusteruser_id:          sharedResourceOld.ClusterUser.Clusteruser_id,
				Managedenvironment_id:   sharedResourceOld.ManagedEnv.Managedenvironment_id,
				Gitopsengineinstance_id: sharedResourceOld.GitopsEngineInstance.Gitopsengineinstance_id,
				clusterAccess:           sharedResourceOld.ClusterAccess,
				EngineCluster_id:        sharedResourceOld.GitopsEngineInstance.EngineCluster_id,
				Clustercredentials_id: []string{
					sharedResourceOld.GitopsEngineCluster.Clustercredentials_id,
					sharedResourceOld.ManagedEnv.Clustercredentials_id,
				},
			}
		})

		It("Should fetch a engine instance by ID.", func() {
			sharedResourceEventLoop := &SharedResourceEventLoop{inputChannel: make(chan sharedResourceLoopMessage)}

			go internalSharedResourceEventLoop(sharedResourceEventLoop.inputChannel)

			// Negative test, engineInstance is not present, it should return error
			engineInstanceOld, err := sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx, "", k8sClient, *namespace, l)
			Expect(err).NotTo(BeNil())
			Expect(engineInstanceOld.EngineCluster_id).To(BeEmpty())

			// Create new engine instance which will be used by "GetGitopsEngineInstanceById" fucntion
			dbq, err := db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())

			defer dbq.CloseDatabase()

			clusterCredentials := db.ClusterCredentials{
				Clustercredentials_cred_id: string(uuid.NewUUID()),
			}

			gitopsEngineCluster := db.GitopsEngineCluster{
				Gitopsenginecluster_id: string(uuid.NewUUID()),
				Clustercredentials_id:  clusterCredentials.Clustercredentials_cred_id,
			}

			gitopsEngineInstance := db.GitopsEngineInstance{
				Gitopsengineinstance_id: string(uuid.NewUUID()),
				Namespace_name:          namespace.Name,
				Namespace_uid:           string(namespace.UID),
				EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
			}

			err = dbq.CreateClusterCredentials(ctx, &clusterCredentials)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineCluster(ctx, &gitopsEngineCluster)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineInstance(ctx, &gitopsEngineInstance)
			Expect(err).To(BeNil())

			// Fetch the same engineInstance by ID
			engineInstanceNew, err := sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx,
				gitopsEngineInstance.Gitopsengineinstance_id, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(engineInstanceNew.EngineCluster_id).NotTo(BeNil())

			// To be used by AfterEach to clean up the resources created by test
			resourcesToBeDeleted = testResources{
				Gitopsengineinstance_id: gitopsEngineInstance.Gitopsengineinstance_id,
				EngineCluster_id:        gitopsEngineInstance.EngineCluster_id,
				Clustercredentials_id: []string{
					clusterCredentials.Clustercredentials_cred_id,
				},
			}
		})

		It("Should fetch a GitOpsDeploymentRepositoryCredential.", func() {
			sharedResourceEventLoop := &SharedResourceEventLoop{inputChannel: make(chan sharedResourceLoopMessage)}

			go internalSharedResourceEventLoop(sharedResourceEventLoop.inputChannel)

			// Create new engine instance which will be used by "GetGitopsEngineInstanceById" fucntion
			dbq, err := db.NewUnsafePostgresDBQueries(true, true)
			Expect(err).To(BeNil())

			defer dbq.CloseDatabase()

			clusterCredentials := db.ClusterCredentials{
				Clustercredentials_cred_id: string(uuid.NewUUID()),
			}

			gitopsEngineCluster := db.GitopsEngineCluster{
				Gitopsenginecluster_id: string(uuid.NewUUID()),
				Clustercredentials_id:  clusterCredentials.Clustercredentials_cred_id,
			}

			gitopsEngineInstance := db.GitopsEngineInstance{
				Gitopsengineinstance_id: string(uuid.NewUUID()),
				Namespace_name:          namespace.Name,
				Namespace_uid:           string(namespace.UID),
				EngineCluster_id:        gitopsEngineCluster.Gitopsenginecluster_id,
			}

			err = dbq.CreateClusterCredentials(ctx, &clusterCredentials)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineCluster(ctx, &gitopsEngineCluster)
			Expect(err).To(BeNil())

			err = dbq.CreateGitopsEngineInstance(ctx, &gitopsEngineInstance)
			Expect(err).To(BeNil())

			// Fetch the same engineInstance by ID
			engineInstanceNew, err := sharedResourceEventLoop.GetGitopsEngineInstanceById(ctx,
				gitopsEngineInstance.Gitopsengineinstance_id, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(engineInstanceNew.EngineCluster_id).NotTo(BeNil())

			// At first assuming there are no existing users, hence creating new.
			usrOld,
				isNewUser,
				err := sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(usrOld).NotTo(BeNil())
			Expect(isNewUser).To(BeTrue())

			// User is created in previous call, then same user should be returned instead of creating new.
			usrNew,
				isNewUser,
				err := sharedResourceEventLoop.GetOrCreateClusterUserByNamespaceUID(ctx, k8sClient, *namespace, l)

			Expect(err).To(BeNil())
			Expect(usrNew).NotTo(BeNil())
			Expect(isNewUser).To(BeFalse())
			Expect(usrOld).To(Equal(usrNew))

			// To be used by AfterEach to clean up the resources created by test
			resourcesToBeDeleted = testResources{
				Gitopsengineinstance_id: gitopsEngineInstance.Gitopsengineinstance_id,
				EngineCluster_id:        gitopsEngineInstance.EngineCluster_id,
				// Clusteruser_id:          usrNew.Clusteruser_id,
				Clustercredentials_id: []string{
					clusterCredentials.Clustercredentials_cred_id,
				},
			}

			// Create new GitOpsDeploymentRepositoryCredential
			cr := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gitopsdeploymenrepositorycredential",
					Namespace: gitopsEngineInstance.Namespace_name,
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredentialSpec{
					Repository: "https://fakegithub.com/test/test-repository",
					Secret:     "test-secret",
				}}

			err = k8sClient.Create(ctx, cr)
			Expect(err).To(BeNil())

			// Fetch the GitOpsDeploymentRepositoryCredential created
			cred := &managedgitopsv1alpha1.GitOpsDeploymentRepositoryCredential{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, cred)
			Expect(err).To(BeNil())

			var repositoryCredentialCRNamespace v1.Namespace
			repositoryCredentialCRNamespace.Name = gitopsEngineInstance.Namespace_name
			repositoryCredentialCRNamespace.UID = types.UID(gitopsEngineInstance.Namespace_uid)

			var k8sClientFactory SRLK8sClientFactory

			dbRepoCred, err := internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)

			// Negative test (there is no Secret)
			Expect(err).NotTo(BeNil())
			Expect(dbRepoCred).To(BeNil())

			// Create new Secret
			secret := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: gitopsEngineInstance.Namespace_name,
				},
				Data: map[string][]byte{
					"username": []byte("test-username"),
					"password": []byte("test-password"),
				},
			}
			err = k8sClient.Create(ctx, secret)
			Expect(err).To(BeNil())

			// Create again the CR
			// Expected: Since there's no DB entry for the CR, it will create an operation
			dbRepoCred, err = internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)
			Expect(err).To(BeNil())
			Expect(dbRepoCred).NotTo(BeNil())
			var operationDB db.Operation
			operationDB.Resource_id = dbRepoCred.RepositoryCredentialsID

			// Verify there is one Operation CR created and find its matching DB Entry
			operationList := &managedgitopsv1alpha1.OperationList{}
			err = k8sClient.List(ctx, operationList)
			Expect(err).To(BeNil())

			if len(operationList.Items) == 1 {
				operationDB.Operation_id = operationList.Items[0].Spec.OperationID
			}

			err = dbq.GetOperationById(ctx, &operationDB)
			Expect(err).To(BeNil())
			Expect(operationDB.State).Should(Equal(db.OperationState_Waiting))

			// Fetch the RepositoryCredential DB row using information provided by the Operation
			// this is how the cluster-agent will find the RepositoryCredential DB row
			// and verify that this db row is the same with the output of the internalProcessMessage_ReconcileRepositoryCredential()
			fetch, err := dbq.GetRepositoryCredentialsByID(ctx, operationDB.Resource_id)
			Expect(err).To(BeNil())
			Expect(fetch).Should(Equal(*dbRepoCred))

			// Re-running should not error
			dbRepoCred, err = internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)
			Expect(fetch).Should(Equal(*dbRepoCred))
			Expect(err).To(BeNil())

			// Check if there are any operations left (should be 1)
			operationList = &managedgitopsv1alpha1.OperationList{}
			err = k8sClient.List(ctx, operationList)
			Expect(err).To(BeNil())
			Expect(len(operationList.Items)).Should(Equal(1))

			// Fetch the operation db
			operationDB.Operation_id = operationList.Items[0].Spec.OperationID
			err = dbq.GetOperationById(ctx, &operationDB)
			Expect(err).To(BeNil())
			Expect(operationDB.State).Should(Equal(db.OperationState_Waiting))

			// Modify the repository credential database, pointing to a wrong secret
			// Expected: The diff should be detected and roll-back to what the GitOpsDeploymentRepositoryCredential CR has (source of truth)
			// also it should fire-up an operation to fix this
			dbRepoCred.SecretObj = "test-secret-2"
			err = dbq.UpdateRepositoryCredentials(ctx, dbRepoCred)
			Expect(err).To(BeNil())
			dbRepoCred, err = internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)
			Expect(err).To(BeNil())

			// Check if there are any operations left (should not create 2nd operation if already exists for this resource)
			operationList = &managedgitopsv1alpha1.OperationList{}
			err = k8sClient.List(ctx, operationList)
			Expect(err).To(BeNil())
			Expect(len(operationList.Items)).Should(Equal(1))

			// Fetch the operation db
			operationDB.Operation_id = operationList.Items[0].Spec.OperationID
			err = dbq.GetOperationById(ctx, &operationDB)
			Expect(err).To(BeNil())
			Expect(operationDB.State).Should(Equal(db.OperationState_Waiting))

			// Delete the operation db and operation cr
			_, err = dbq.DeleteOperationById(ctx, operationDB.Operation_id)
			Expect(err).To(BeNil())
			err = k8sClient.Delete(ctx, &operationList.Items[0])
			Expect(err).To(BeNil())

			// Delete the Secret
			err = k8sClient.Delete(ctx, secret)
			Expect(err).To(BeNil())

			// Delete the GitOpsDeploymentRepositoryCredential CR and reconcile again
			// Expected: Since there is no GitOpsDeploymentRepositoryCredential CR, it will delete the DB entry
			err = k8sClient.Delete(ctx, cr)
			Expect(err).To(BeNil())
			dbRepoCred, err = internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)
			Expect(err).To(BeNil())
			Expect(dbRepoCred).To(BeNil())

			// Negative test: Get the RepositoryCredential from the DB
			// Expected: It should not exist
			_, err = dbq.GetRepositoryCredentialsByID(ctx, cr.Name)
			Expect(err).ToNot(BeNil())

			// A new Operation should be created
			// Check if there are any operations left (should be 1)
			operationList = &managedgitopsv1alpha1.OperationList{}
			err = k8sClient.List(ctx, operationList)
			Expect(err).To(BeNil())
			Expect(len(operationList.Items)).Should(Equal(1))
			// Fetch the operation db
			operationDB.Operation_id = operationList.Items[0].Spec.OperationID
			err = dbq.GetOperationById(ctx, &operationDB)
			Expect(err).To(BeNil())
			Expect(operationDB.State).Should(Equal(db.OperationState_Waiting))

			// Negative test: Try again to reconcile the RepositoryCredential
			// Expected: It should not error (both db row and CR should be deleted). Nothing we can do.
			dbRepoCred, err = internalProcessMessage_ReconcileRepositoryCredential(ctx, cr.Name, repositoryCredentialCRNamespace, k8sClient, k8sClientFactory, dbq, l)
			Expect(err).To(BeNil())
			Expect(dbRepoCred).To(BeNil())
		})

	})
})
