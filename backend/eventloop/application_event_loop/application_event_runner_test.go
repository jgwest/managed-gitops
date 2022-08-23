package application_event_loop

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strings"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/errors"

	"github.com/golang/mock/gomock"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1/mocks"
	condition "github.com/redhat-appstudio/managed-gitops/backend/condition/mocks"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventloop_test_util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/shared_resource_loop"
	"github.com/redhat-appstudio/managed-gitops/backend/util"
	"gopkg.in/yaml.v2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/tests"

	db "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"

	testStructs "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1/mocks/structs"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("ApplicationEventLoop Test", func() {

	Context("Handle deployment modified", func() {
		var err error
		var workspaceID string
		var ctx context.Context
		var scheme *runtime.Scheme
		var workspace *corev1.Namespace
		var argocdNamespace *corev1.Namespace
		var dbQueries db.AllDatabaseQueries
		var k8sClientOuter client.WithWatch
		var k8sClient *sharedutil.ProxyClient
		var kubesystemNamespace *corev1.Namespace
		var informer sharedutil.ListEventReceiver
		var gitopsDepl *managedgitopsv1alpha1.GitOpsDeployment
		var appEventLoopRunnerAction applicationEventLoopRunner_Action

		BeforeEach(func() {
			ctx = context.Background()
			informer = sharedutil.ListEventReceiver{}

			scheme,
				argocdNamespace,
				kubesystemNamespace,
				workspace,
				err = tests.GenericTestSetup()
			Expect(err).To(BeNil())

			workspaceID = string(workspace.UID)

			gitopsDepl = &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL:        "https://github.com/abc-org/abc-repo",
						Path:           "/abc-path",
						TargetRevision: "abc-commit"},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
					Destination: managedgitopsv1alpha1.ApplicationDestination{
						Namespace: "abc-namespace",
					},
				},
			}

			k8sClientOuter = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).
				Build()

			k8sClient = &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
				Informer:    &informer,
			}

			dbQueries, err = db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).To(BeNil())

			appEventLoopRunnerAction = applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}
		})

		It("Should update existing deployment, instead of creating new.", func() {

			// ----------------------------------------------------------------------------
			By("Create new deployment.")
			// ----------------------------------------------------------------------------
			var message deploymentModifiedResult
			_, _, _, message, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)

			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_Created))

			// ----------------------------------------------------------------------------
			By("Verify that database entries are created.")
			// ----------------------------------------------------------------------------

			var appMappingsFirst []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappingsFirst)

			Expect(err).To(BeNil())
			Expect(len(appMappingsFirst)).To(Equal(1))

			deplToAppMappingFirst := appMappingsFirst[0]
			applicationFirst := db.Application{Application_id: deplToAppMappingFirst.Application_id}
			err = dbQueries.GetApplicationById(context.Background(), &applicationFirst)

			Expect(err).To(BeNil())

			//############################################################################

			// ----------------------------------------------------------------------------
			By("Update existing deployment.")
			// ----------------------------------------------------------------------------

			gitopsDepl.Spec.Source.Path = "/def-path"
			gitopsDepl.Spec.Source.RepoURL = "https://github.com/def-org/def-repo"
			gitopsDepl.Spec.Source.TargetRevision = "def-commit"
			gitopsDepl.Spec.Destination.Namespace = "def-namespace"

			// Create new client and application runner, but pass existing gitOpsDeployment object.
			k8sClientOuter = fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).
				Build()
			k8sClient = &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
				Informer:    &informer,
			}

			appEventLoopRunnerActionSecond := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			// ----------------------------------------------------------------------------
			By("This should update the existing application.")
			// ----------------------------------------------------------------------------

			_, _, _, message, userDevErr = appEventLoopRunnerActionSecond.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_Updated))

			// ----------------------------------------------------------------------------
			By("Verify that the database entries have been updated.")
			// ----------------------------------------------------------------------------

			var appMappingsSecond []db.DeploymentToApplicationMapping
			err := dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappingsSecond)

			Expect(err).To(BeNil())
			Expect(len(appMappingsSecond)).To(Equal(1))

			deplToAppMappingSecond := appMappingsSecond[0]
			applicationSecond := db.Application{Application_id: deplToAppMappingSecond.Application_id}
			err = dbQueries.GetApplicationById(context.Background(), &applicationSecond)

			Expect(err).To(BeNil())
			Expect(applicationFirst.SeqID).To(Equal(applicationSecond.SeqID))
			Expect(applicationFirst.Spec_field).NotTo(Equal(applicationSecond.Spec_field))

			clusterUser := db.ClusterUser{User_name: string(workspace.UID)}
			err = dbQueries.GetClusterUserByUsername(context.Background(), &clusterUser)
			Expect(err).To(BeNil())

			gitopsEngineInstance := db.GitopsEngineInstance{Gitopsengineinstance_id: applicationSecond.Engine_instance_inst_id}
			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			managedEnvironment := db.ManagedEnvironment{Managedenvironment_id: applicationSecond.Managed_environment_id}
			err = dbQueries.GetManagedEnvironmentById(ctx, &managedEnvironment)
			Expect(err).To(BeNil())

			//############################################################################

			// ----------------------------------------------------------------------------
			By("Delete the GitOpsDepl and verify that the corresponding DB entries are removed.")
			// ----------------------------------------------------------------------------

			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			_, _, _, message, userDevErr = appEventLoopRunnerActionSecond.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_Deleted))

			// Application should no longer exist
			err = dbQueries.GetApplicationById(ctx, &applicationSecond)
			Expect(err).ToNot(BeNil())
			Expect(db.IsResultNotFoundError(err)).To(BeTrue())

			// DeploymentToApplicationMapping should be removed, too
			var appMappings []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)
			Expect(err).To(BeNil())
			Expect(len(appMappings)).To(Equal(0))

			// GitopsEngine instance should still be reachable
			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			operationCreated := false
			operationDeleted := false
			for _, event := range informer.Events {
				if event.Action == sharedutil.Create && event.ObjectTypeOf() == "Operation" {
					operationCreated = true
				}
				if event.Action == sharedutil.Delete && event.ObjectTypeOf() == "Operation" {
					operationDeleted = true
				}
			}

			Expect(operationCreated).To(BeTrue())
			Expect(operationDeleted).To(BeTrue())
		})

		It("Should not deploy application, as request data is not valid.", func() {

			appEventLoopRunnerAction = applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			// This should fail while creating new application
			_, _, _, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).NotTo(BeNil())

			// ----------------------------------------------------------------------------
			By("Verify that the database entry is not created.")
			// ----------------------------------------------------------------------------

			var appMappings []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)

			Expect(err).To(BeNil())
			Expect(len(appMappings)).To(Equal(0))
		})

		It("Should not update existing deployment, if no changes were done in fields.", func() {
			// This should create new application
			var message deploymentModifiedResult
			_, _, _, message, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)

			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_Created))

			// ----------------------------------------------------------------------------
			By("Verify that the database entries have been created.")
			// ----------------------------------------------------------------------------

			var appMappingsFirst []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappingsFirst)

			Expect(err).To(BeNil())
			Expect(len(appMappingsFirst)).To(Equal(1))

			deplToAppMappingFirst := appMappingsFirst[0]
			applicationFirst := db.Application{Application_id: deplToAppMappingFirst.Application_id}
			err = dbQueries.GetApplicationById(context.Background(), &applicationFirst)
			Expect(err).To(BeNil())

			//--------------------------------------------------------------------------------------
			// Now create new client and event loop runner object.
			//--------------------------------------------------------------------------------------

			k8sClientOuter = fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).Build()
			k8sClient = &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
				Informer:    &informer,
			}

			appEventLoopRunnerActionSecond := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			//--------------------------------------------------------------------------------------
			// Pass same gitOpsDeployment again, but no changes should be done in the application.
			//--------------------------------------------------------------------------------------
			_, _, _, message, userDevErr = appEventLoopRunnerActionSecond.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)

			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_NoChange))

			//############################################################################

			// ----------------------------------------------------------------------------
			By("Delete the GitOpsDepl and verify that the corresponding DB entries are removed.")
			// ----------------------------------------------------------------------------

			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			_, _, _, message, userDevErr = appEventLoopRunnerActionSecond.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
			Expect(message).To(Equal(deploymentModifiedResult_Deleted))

			// Application should no longer exist
			err = dbQueries.GetApplicationById(ctx, &applicationFirst)
			Expect(err).ToNot(BeNil())
			Expect(db.IsResultNotFoundError(err)).To(BeTrue())

			// DeploymentToApplicationMapping should be removed, too
			var appMappings []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)
			Expect(err).To(BeNil())
			Expect(len(appMappings)).To(Equal(0))

			// GitopsEngine instance should still be reachable
			gitopsEngineInstance := db.GitopsEngineInstance{Gitopsengineinstance_id: applicationFirst.Engine_instance_inst_id}
			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			operationCreated := false
			operationDeleted := false
			for _, event := range informer.Events {
				if event.Action == sharedutil.Create && event.ObjectTypeOf() == "Operation" {
					operationCreated = true
				}
				if event.Action == sharedutil.Delete && event.ObjectTypeOf() == "Operation" {
					operationDeleted = true
				}
			}

			Expect(operationCreated).To(BeTrue())
			Expect(operationDeleted).To(BeTrue())
		})

		It("create a deployment and ensure it processed, then delete it an ensure that is processed", func() {
			_, _, _, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// Verify that the database entries have been created -----------------------------------------

			var deplToAppMapping db.DeploymentToApplicationMapping
			{
				var appMappings []db.DeploymentToApplicationMapping

				err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)
				Expect(err).To(BeNil())

				Expect(len(appMappings)).To(Equal(1))

				deplToAppMapping = appMappings[0]
			}

			clusterUser := db.ClusterUser{
				User_name: string(workspace.UID),
			}
			err = dbQueries.GetClusterUserByUsername(context.Background(), &clusterUser)
			Expect(err).To(BeNil())

			application := db.Application{
				Application_id: deplToAppMapping.Application_id,
			}

			err = dbQueries.GetApplicationById(context.Background(), &application)
			Expect(err).To(BeNil())

			gitopsEngineInstance := db.GitopsEngineInstance{
				Gitopsengineinstance_id: application.Engine_instance_inst_id,
			}

			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			managedEnvironment := db.ManagedEnvironment{
				Managedenvironment_id: application.Managed_environment_id,
			}
			err = dbQueries.GetManagedEnvironmentById(ctx, &managedEnvironment)
			Expect(err).To(BeNil())

			// Delete the GitOpsDepl and verify that the corresponding DB entries are removed -------------

			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			_, _, _, _, userDevErr = appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// Application should no longer exist
			err = dbQueries.GetApplicationById(ctx, &application)
			Expect(err).ToNot(BeNil())
			Expect(db.IsResultNotFoundError(err)).To(BeTrue())

			// DeploymentToApplicationMapping should be removed, too
			var appMappings []db.DeploymentToApplicationMapping
			err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)
			Expect(err).To(BeNil())
			Expect(len(appMappings)).To(Equal(0))

			// GitopsEngine instance should still be reachable
			err = dbQueries.GetGitopsEngineInstanceById(context.Background(), &gitopsEngineInstance)
			Expect(err).To(BeNil())

			operatorCreated := false
			operatorDeleted := false

			for idx, event := range informer.Events {

				if event.Action == sharedutil.Create && event.ObjectTypeOf() == "Operation" {
					operatorCreated = true
				}
				if event.Action == sharedutil.Delete && event.ObjectTypeOf() == "Operation" {
					operatorDeleted = true
				}

				fmt.Printf("%d) %v\n", idx, event)
			}

			Expect(operatorCreated).To(BeTrue())
			Expect(operatorDeleted).To(BeTrue())

		})

		It("create an invalid deployment and ensure it fails.", func() {
			ctx := context.Background()

			scheme, argocdNamespace, kubesystemNamespace, workspace, err := tests.GenericTestSetup()
			Expect(err).To(BeNil())

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      strings.Repeat("abc", 100),
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			k8sClientOuter := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			workspaceID := string(workspace.UID)

			dbQueries, err := db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).To(BeNil())

			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			// ------

			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).NotTo(BeNil())

		})

	})

	Context("Handle sync run modified", func() {

		It("Ensure the sync run handler can handle a new sync run resource", func() {
			ctx := context.Background()

			scheme, argocdNamespace, kubesystemNamespace, workspace, err := tests.GenericTestSetup()
			Expect(err).To(BeNil())

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			gitopsDeplSyncRun := &managedgitopsv1alpha1.GitOpsDeploymentSyncRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl-sync",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSyncRunSpec{
					GitopsDeploymentName: gitopsDepl.Name,
					RevisionID:           "HEAD",
				},
			}

			informer := sharedutil.ListEventReceiver{}

			k8sClientOuter := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitopsDepl, gitopsDeplSyncRun, workspace, argocdNamespace, kubesystemNamespace).Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
				Informer:    &informer,
			}

			dbQueries, err := db.NewUnsafePostgresDBQueries(true, false)
			Expect(err).To(BeNil())

			opts := zap.Options{
				Development: true,
			}
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

			sharedResourceLoop := shared_resource_loop.NewSharedResourceLoop()

			// 1) send a deployment modified event, to ensure the deployment is added to the database, and processed
			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     sharedResourceLoop,
				workspaceID:                 string(workspace.UID),
				testOnlySkipCreateOperation: true,
			}
			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// 2) add a sync run modified event, to ensure the sync run is added to the database, and processed
			a = applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:       gitopsDeplSyncRun.Name,
				eventResourceNamespace:  gitopsDeplSyncRun.Namespace,
				workspaceClient:         k8sClient,
				log:                     log.FromContext(context.Background()),
				sharedResourceEventLoop: sharedResourceLoop,
				workspaceID:             a.workspaceID,
			}
			_, err = a.applicationEventRunner_handleSyncRunModified(ctx, dbQueries)
			Expect(err).To(BeNil())
		})

		It("Ensure the sync run handler fails when an invalid new sync run resource is passed.", func() {
			ctx := context.Background()

			scheme, argocdNamespace, kubesystemNamespace, workspace, err := tests.GenericTestSetup()
			Expect(err).To(BeNil())

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			gitopsDeplSyncRun := &managedgitopsv1alpha1.GitOpsDeploymentSyncRun{
				ObjectMeta: metav1.ObjectMeta{
					Name:      strings.Repeat("abc", 100),
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSyncRunSpec{
					GitopsDeploymentName: gitopsDepl.Name,
					RevisionID:           "HEAD",
				},
			}

			k8sClientOuter := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitopsDepl, gitopsDeplSyncRun, workspace, argocdNamespace, kubesystemNamespace).Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			dbQueries, err := db.NewUnsafePostgresDBQueries(true, false)
			Expect(err).To(BeNil())

			opts := zap.Options{
				Development: true,
			}
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

			sharedResourceLoop := shared_resource_loop.NewSharedResourceLoop()

			// 1) send a deployment modified event, to ensure the deployment is added to the database, and processed
			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     sharedResourceLoop,
				workspaceID:                 string(workspace.UID),
				testOnlySkipCreateOperation: true,
			}
			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// 2) add a sync run modified event, to ensure the sync run is added to the database, and processed
			a = applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:       gitopsDeplSyncRun.Name,
				eventResourceNamespace:  gitopsDeplSyncRun.Namespace,
				workspaceClient:         k8sClient,
				log:                     log.FromContext(context.Background()),
				sharedResourceEventLoop: sharedResourceLoop,
				workspaceID:             a.workspaceID,
			}
			_, err = a.applicationEventRunner_handleSyncRunModified(ctx, dbQueries)
			Expect(err).NotTo(BeNil())

		})
	})

	Context("Handle deployment Health/Sync status.", func() {
		var err error
		var workspaceID string
		var ctx context.Context
		var scheme *runtime.Scheme
		var workspace *corev1.Namespace
		var argocdNamespace *corev1.Namespace
		var dbQueries db.AllDatabaseQueries
		var kubesystemNamespace *corev1.Namespace

		BeforeEach(func() {
			ctx = context.Background()

			scheme,
				argocdNamespace,
				kubesystemNamespace,
				workspace,
				err = tests.GenericTestSetup()

			Expect(err).To(BeNil())

			workspaceID = string(workspace.UID)

			dbQueries, err = db.NewUnsafePostgresDBQueries(false, false)
			Expect(err).To(BeNil())
		})

		It("Should update correct status of deployment after calling DeploymentStatusTick handler.", func() {
			// ----------------------------------------------------------------------------
			By("Create new deployment.")
			// ----------------------------------------------------------------------------

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			k8sClient := fake.
				NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).
				Build()

			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Get DeploymentToApplicationMapping and Application objects, to be used later.")
			// ----------------------------------------------------------------------------
			var deplToAppMapping db.DeploymentToApplicationMapping
			{
				var appMappings []db.DeploymentToApplicationMapping

				err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(context.Background(), gitopsDepl.Name, gitopsDepl.Namespace, workspaceID, &appMappings)
				Expect(err).To(BeNil())

				Expect(len(appMappings)).To(Equal(1))

				deplToAppMapping = appMappings[0]
			}

			// ----------------------------------------------------------------------------
			By("Inserting dummy data into ApplicationState table, because we are not calling the Reconciler for this, which updates the status of application into db.")
			// ----------------------------------------------------------------------------
			var resourceStatus managedgitopsv1alpha1.ResourceStatus
			var resources []managedgitopsv1alpha1.ResourceStatus
			resources = append(resources, resourceStatus)

			var buffer bytes.Buffer
			// Convert ResourceStatus object into String.
			resourceStr, err := yaml.Marshal(&resources)
			Expect(err).To(BeNil())

			// Compress the data
			gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
			Expect(err).To(BeNil())

			_, err = gzipWriter.Write([]byte(string(resourceStr)))
			Expect(err).To(BeNil())

			err = gzipWriter.Close()
			Expect(err).To(BeNil())

			applicationState := &db.ApplicationState{
				Applicationstate_application_id: deplToAppMapping.Application_id,
				Health:                          string(managedgitopsv1alpha1.HeathStatusCodeHealthy),
				Sync_Status:                     string(managedgitopsv1alpha1.SyncStatusCodeSynced),
				Revision:                        "abcdefg",
				Message:                         "Success",
				Resources:                       buffer.Bytes(),
			}

			err = dbQueries.CreateApplicationState(ctx, applicationState)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Retrieve latest version of GitOpsDeployment and check Health/Sync before calling applicationEventRunner_handleUpdateDeploymentStatusTick function.")
			// ----------------------------------------------------------------------------

			gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
			gitopsDeploymentKey := client.ObjectKey{Namespace: gitopsDepl.Namespace, Name: gitopsDepl.Name}
			clientErr := a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment)

			Expect(clientErr).To(BeNil())

			Expect(gitopsDeployment.Status.Health.Status).To(BeEmpty())
			Expect(gitopsDeployment.Status.Sync.Status).To(BeEmpty())
			Expect(gitopsDeployment.Status.Sync.Revision).To(BeEmpty())
			Expect(gitopsDeployment.Status.Health.Message).To(BeEmpty())

			// ----------------------------------------------------------------------------
			By("Call applicationEventRunner_handleUpdateDeploymentStatusTick function to update Health/Sync status.")
			// ----------------------------------------------------------------------------

			err = a.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, string(gitopsDepl.UID), dbQueries)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Retrieve latest version of GitOpsDeployment and check Health/Sync after calling applicationEventRunner_handleUpdateDeploymentStatusTick function.")
			// ----------------------------------------------------------------------------

			clientErr = a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment)

			Expect(clientErr).To(BeNil())

			Expect(gitopsDeployment.Status.Health.Status).To(Equal(managedgitopsv1alpha1.HeathStatusCodeHealthy))
			Expect(gitopsDeployment.Status.Sync.Status).To(Equal(managedgitopsv1alpha1.SyncStatusCodeSynced))
			Expect(gitopsDeployment.Status.Sync.Revision).To(Equal("abcdefg"))
			Expect(gitopsDeployment.Status.Health.Message).To(Equal("Success"))

			// ----------------------------------------------------------------------------
			By("Delete GitOpsDepl to clean resources.")
			// ----------------------------------------------------------------------------
			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			_, _, _, _, userDevErr = a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
		})

		It("Should not return any error, if deploymentToApplicationMapping doesn't exist for given gitopsdeployment.", func() {
			// Don't create Deployment and resources by calling applicationEventRunner_handleDeploymentModified function,
			// but check for Health/Sync status of deployment which doesn't exist.
			k8sClientOuter := fake.NewClientBuilder().Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			a := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           "dummy-deployment",
				eventResourceNamespace:      workspace.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			// ----------------------------------------------------------------------------
			By("Call applicationEventRunner_handleUpdateDeploymentStatusTick function to update Health/Sync status of a deployment which doesn't exist.")
			// ----------------------------------------------------------------------------
			err = a.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, "dummy-deployment", dbQueries)
			Expect(err).To(BeNil())
		})

		It("Should return error, if deploymentToApplicationMapping object is invalid.", func() {
			// Don't create Deployment and resources by calling applicationEventRunner_handleDeploymentModified function,
			// But check for Health/Sync status of a deployment having invalid name.
			k8sClientOuter := fake.NewClientBuilder().Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			a := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           "dummy-deployment",
				eventResourceNamespace:      workspace.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			// ----------------------------------------------------------------------------
			By("Call applicationEventRunner_handleUpdateDeploymentStatusTick function to update Health/Sync status of a deployment having invalid name.")
			// ----------------------------------------------------------------------------

			err = a.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, "", dbQueries)
			Expect(err).NotTo(BeNil())
			Expect(strings.Contains(err.Error(), "field should not be empty string")).To(BeTrue())
		})

		It("Should not return error, if GitOpsDeployment doesn't exist in given namespace.", func() {
			// ----------------------------------------------------------------------------
			By("Create new deployment, even though it will be deleted later, but we need to do this to create resources to pass initial checks of applicationEventRunner_handleUpdateDeploymentStatusTick function.")
			// ----------------------------------------------------------------------------

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			k8sClientOuter := fake.
				NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).
				Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Delete deployment, but we don't want to delete other DB entries, hence not calling applicationEventRunner_handleDeploymentModified after deleting deployment.")
			// ----------------------------------------------------------------------------
			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Call applicationEventRunner_handleUpdateDeploymentStatusTick function to update Health/Sync status for a deployment which doesn'r exist in given namespace.")
			// ----------------------------------------------------------------------------
			err = a.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, string(gitopsDepl.UID), dbQueries)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Deployment is already been deleted in previous step, now delete related db entries and clean resources.")
			// ----------------------------------------------------------------------------
			_, _, _, _, userDevErr = a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
		})

		It("Should not return error, if ApplicationState doesnt exists for given GitOpsDeployment.", func() {
			// ----------------------------------------------------------------------------
			By("Create new deployment.")
			// ----------------------------------------------------------------------------

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: workspace.Name,
					UID:       uuid.NewUUID(),
				},
			}

			k8sClientOuter := fake.
				NewClientBuilder().
				WithScheme(scheme).
				WithObjects(gitopsDepl, workspace, argocdNamespace, kubesystemNamespace).
				Build()
			k8sClient := &sharedutil.ProxyClient{
				InnerClient: k8sClientOuter,
			}

			a := applicationEventLoopRunner_Action{
				// When the code asks for a new k8s client, give it our fake client
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 workspaceID,
				testOnlySkipCreateOperation: true,
			}

			_, _, _, _, userDevErr := a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Call applicationEventRunner_handleUpdateDeploymentStatusTick function to update Health/Sync status for deployment which is missing ApplicationState entries.")
			// ----------------------------------------------------------------------------

			err = a.applicationEventRunner_handleUpdateDeploymentStatusTick(ctx, string(gitopsDepl.UID), dbQueries)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Retrieve latest version of GitOpsDeployment and check Health/Sync, it should not have any Health/Sync status.")
			// ----------------------------------------------------------------------------

			gitopsDeployment := &managedgitopsv1alpha1.GitOpsDeployment{}
			gitopsDeploymentKey := client.ObjectKey{Namespace: gitopsDepl.Namespace, Name: gitopsDepl.Name}
			clientErr := a.workspaceClient.Get(ctx, gitopsDeploymentKey, gitopsDeployment)

			Expect(clientErr).To(BeNil())

			Expect(gitopsDeployment.Status.Health.Status).To(BeEmpty())
			Expect(gitopsDeployment.Status.Sync.Status).To(BeEmpty())
			Expect(gitopsDeployment.Status.Sync.Revision).To(BeEmpty())
			Expect(gitopsDeployment.Status.Health.Message).To(BeEmpty())

			// ----------------------------------------------------------------------------
			By("Delete GitOpsDepl to clean resources.")
			// ----------------------------------------------------------------------------

			err = k8sClient.Delete(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			_, _, _, _, userDevErr = a.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(userDevErr).To(BeNil())
		})

	})

	Context("Check decompressResourceData function.", func() {
		It("Should decompress resource data and return actual Array of ResourceStatus objects.", func() {
			// ----------------------------------------------------------------------------
			By("Creating sample resource data.")
			// ----------------------------------------------------------------------------

			resourceStatus := managedgitopsv1alpha1.ResourceStatus{
				Group:     "apps",
				Version:   "v1",
				Kind:      "Deployment",
				Namespace: "argoCD",
				Name:      "component-a",
				Status:    "Synced",
				Health: &managedgitopsv1alpha1.HealthStatus{
					Status:  "Healthy",
					Message: "success",
				},
			}

			var resourcesIn []managedgitopsv1alpha1.ResourceStatus
			resourcesIn = append(resourcesIn, resourceStatus)

			// ----------------------------------------------------------------------------
			By("Convert sample ResourceStatus objects into String.")
			// ----------------------------------------------------------------------------
			resourceStr, err := yaml.Marshal(&resourcesIn)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Compress sample data to be passed as input for decompressResourceData function.")
			// ----------------------------------------------------------------------------
			var buffer bytes.Buffer
			gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)

			Expect(err).To(BeNil())

			_, err = gzipWriter.Write([]byte(string(resourceStr)))
			Expect(err).To(BeNil())

			err = gzipWriter.Close()
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Decompress data and convert it to String, then convert String into ResourceStatus Array.")
			// ----------------------------------------------------------------------------

			var resourcesOut []managedgitopsv1alpha1.ResourceStatus

			resourcesOut, err = decompressResourceData(buffer.Bytes())

			Expect(err).To(BeNil())

			Expect(resourcesOut).NotTo(BeNil())
			Expect(resourcesOut).NotTo(BeEmpty())

			Expect(resourcesOut[0]).NotTo(BeNil())
			Expect(resourcesOut[0].Group).To(Equal("apps"))
			Expect(resourcesOut[0].Version).To(Equal("v1"))
			Expect(resourcesOut[0].Kind).To(Equal("Deployment"))
			Expect(resourcesOut[0].Namespace).To(Equal("argoCD"))
			Expect(resourcesOut[0].Status).To(Equal(managedgitopsv1alpha1.SyncStatusCodeSynced))
			Expect(resourcesOut[0].Health.Status).To(Equal(managedgitopsv1alpha1.HeathStatusCodeHealthy))
			Expect(resourcesOut[0].Health.Message).To(Equal("success"))
		})

		It("Should decompress empty resource data and return actual Array of ResourceStatus objects.", func() {
			// ----------------------------------------------------------------------------
			By("Creating sample resource data.")
			// ----------------------------------------------------------------------------
			resourceStatus := managedgitopsv1alpha1.ResourceStatus{}

			var resourcesIn []managedgitopsv1alpha1.ResourceStatus
			resourcesIn = append(resourcesIn, resourceStatus)

			// ----------------------------------------------------------------------------
			By("Convert sample ResourceStatus objects into String.")
			// ----------------------------------------------------------------------------
			resourceStr, err := yaml.Marshal(&resourcesIn)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Compress sample data to be passed as input for decompressResourceData function.")
			// ----------------------------------------------------------------------------
			var buffer bytes.Buffer
			gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)

			Expect(err).To(BeNil())

			_, err = gzipWriter.Write([]byte(string(resourceStr)))
			Expect(err).To(BeNil())

			err = gzipWriter.Close()
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Decompress data and convert it to String, then convert String into ResourceStatus Array.")
			// ----------------------------------------------------------------------------

			var resourcesOut []managedgitopsv1alpha1.ResourceStatus

			resourcesOut, err = decompressResourceData(buffer.Bytes())

			Expect(err).To(BeNil())

			Expect(resourcesOut).NotTo(BeNil())
			Expect(resourcesOut).NotTo(BeEmpty())

			Expect(resourcesOut[0]).NotTo(BeNil())
			Expect(managedgitopsv1alpha1.ResourceStatus{} == resourcesOut[0]).To(BeTrue())
		})

		It("Should decompress empty resource data and return empty Array of ResourceStatus objects.", func() {
			// ----------------------------------------------------------------------------
			By("Creating sample resource data.")
			// ----------------------------------------------------------------------------

			var resourcesIn []managedgitopsv1alpha1.ResourceStatus

			// ----------------------------------------------------------------------------
			By("Convert sample ResourceStatus objects into String.")
			// ----------------------------------------------------------------------------
			resourceStr, err := yaml.Marshal(&resourcesIn)
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Compress sample data to be passed as input for decompressResourceData function.")
			// ----------------------------------------------------------------------------
			var buffer bytes.Buffer
			gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
			Expect(err).To(BeNil())

			_, err = gzipWriter.Write([]byte(string(resourceStr)))
			Expect(err).To(BeNil())

			err = gzipWriter.Close()
			Expect(err).To(BeNil())

			// ----------------------------------------------------------------------------
			By("Decompress data and convert it to String, then convert String into ResourceStatus Array.")
			// ----------------------------------------------------------------------------

			var resourcesOut []managedgitopsv1alpha1.ResourceStatus

			resourcesOut, err = decompressResourceData(buffer.Bytes())

			Expect(err).To(BeNil())

			Expect(resourcesOut).NotTo(BeNil())
			Expect(resourcesOut).To(BeEmpty())
		})
	})
})

var _ = Describe("GitOpsDeployment Conditions", func() {
	var (
		adapter          *gitOpsDeploymentAdapter
		mockCtrl         *gomock.Controller
		mockClient       *mocks.MockClient
		mockStatusWriter *mocks.MockStatusWriter
		gitopsDeployment *managedgitopsv1alpha1.GitOpsDeployment
		mockConditions   *condition.MockConditions
		ctx              context.Context
	)

	BeforeEach(func() {
		gitopsDeployment = testStructs.NewGitOpsDeploymentBuilder().Initialized().GetGitopsDeployment()
		mockCtrl = gomock.NewController(GinkgoT())
		mockClient = mocks.NewMockClient(mockCtrl)
		mockConditions = condition.NewMockConditions(mockCtrl)
		mockStatusWriter = mocks.NewMockStatusWriter(mockCtrl)
	})
	JustBeforeEach(func() {
		adapter = newGitOpsDeploymentAdapter(gitopsDeployment, log.Log.WithName("Test Logger"), mockClient, mockConditions, ctx)
	})
	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("setGitopsDeploymentCondition()", func() {
		var (
			userDevErr    = errors.NewUserDevError("reconcile error", fmt.Errorf("reconcile error"))
			reason        = managedgitopsv1alpha1.GitOpsDeploymentReasonType("ReconcileError")
			conditionType = managedgitopsv1alpha1.GitOpsDeploymentConditionErrorOccurred
		)
		Context("when no conditions defined before and the err is nil", func() {
			BeforeEach(func() {
				mockConditions.EXPECT().HasCondition(gomock.Any(), conditionType).Return(false)
			})
			It("It returns nil ", func() {
				errTemp := adapter.setGitOpsDeploymentCondition(conditionType, reason, nil)
				Expect(errTemp).To(BeNil())
			})
		})
		Context("when the err comes from reconcileHandler", func() {
			It("should update the CR", func() {
				matcher := testStructs.NewGitopsDeploymentMatcher()
				mockClient.EXPECT().Status().Return(mockStatusWriter)
				mockStatusWriter.EXPECT().Update(gomock.Any(), matcher, gomock.Any())
				mockConditions.EXPECT().SetCondition(gomock.Any(), conditionType,
					managedgitopsv1alpha1.GitOpsConditionStatusTrue, reason, userDevErr.UserError()).Times(1)
				err := adapter.setGitOpsDeploymentCondition(conditionType, reason, userDevErr)
				Expect(err).NotTo(HaveOccurred())
			})
		})
		Context("when the err has been resolved", func() {
			BeforeEach(func() {
				mockConditions.EXPECT().HasCondition(gomock.Any(), conditionType).Return(true)
				mockConditions.EXPECT().FindCondition(gomock.Any(), conditionType).Return(&managedgitopsv1alpha1.GitOpsDeploymentCondition{}, true)
			})
			It("It should update the CR condition status as resolved", func() {
				matcher := testStructs.NewGitopsDeploymentMatcher()
				conditions := &gitopsDeployment.Status.Conditions
				*conditions = append(*conditions, managedgitopsv1alpha1.GitOpsDeploymentCondition{})
				mockClient.EXPECT().Status().Return(mockStatusWriter)
				mockStatusWriter.EXPECT().Update(gomock.Any(), matcher, gomock.Any())
				mockConditions.EXPECT().SetCondition(conditions, conditionType, managedgitopsv1alpha1.GitOpsConditionStatusFalse, managedgitopsv1alpha1.GitOpsDeploymentReasonType("ReconcileErrorResolved"), "").Times(1)
				err := adapter.setGitOpsDeploymentCondition(conditionType, reason, nil)
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

})

type OperationCheck struct {
	operationEvents []managedgitopsv1alpha1.Operation
}

func (oc *OperationCheck) ReceiveEvent(event util.ProxyClientEvent) {

	if event.Obj == nil {
		return
	}

	operation, ok := (*event.Obj).(*managedgitopsv1alpha1.Operation)
	if ok {
		oc.operationEvents = append(oc.operationEvents, *operation)
	}
}

var _ = Describe("application_event_runner_deployments.go Tests", func() {

	Context("testing handling of GitOpsDeployments that reference managed environments", func() {

		var ctx context.Context
		var scheme *runtime.Scheme
		var err error

		var namespace *corev1.Namespace
		var argocdNamespace *corev1.Namespace
		var dbQueries db.AllDatabaseQueries
		var kubesystemNamespace *corev1.Namespace

		operationCheck := &OperationCheck{}

		k8sClient := &util.ProxyClient{
			Informer: operationCheck,
		}

		// createManagedEnv creates a managed environment DB row and CR, connected via APICRToDBMApping
		createManagedEnv := func() (managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, corev1.Secret) {

			managedEnvCR := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-managed-env",
					Namespace: namespace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
					APIURL:                   "https://api.fake-unit-test-data.origin-ci-int-gce.dev.rhcloud.com:6443",
					ClusterCredentialsSecret: "fake-secret-name",
				},
			}
			err = k8sClient.Create(ctx, &managedEnvCR)
			Expect(err).To(BeNil())

			kubeConfigContents := generateFakeKubeConfig()
			managedEnvSecret := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      managedEnvCR.Spec.ClusterCredentialsSecret,
					Namespace: namespace.Name,
				},
				Type: sharedutil.ManagedEnvironmentSecretType,
				Data: map[string][]byte{
					"kubeconfig": ([]byte)(kubeConfigContents),
				},
			}
			err = k8sClient.Create(ctx, &managedEnvSecret)
			Expect(err).To(BeNil())

			return managedEnvCR, managedEnvSecret
		}

		// Create a simple GitOpsDeployment CR
		createGitOpsDepl := func() *managedgitopsv1alpha1.GitOpsDeployment {
			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: namespace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL:        "https://github.com/abc-org/abc-repo",
						Path:           "/abc-path",
						TargetRevision: "abc-commit"},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
					Destination: managedgitopsv1alpha1.ApplicationDestination{
						Namespace: "abc-namespace",
					},
				},
			}
			err = k8sClient.Create(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			return gitopsDepl
		}

		// getManagedEnvironmentForGitOpsDeployment returns the managed environment row that is reference by the CR
		getManagedEnvironmentForGitOpsDeployment := func(gitopsDepl managedgitopsv1alpha1.GitOpsDeployment) (db.ManagedEnvironment, db.Application, error) {
			dtam := []db.DeploymentToApplicationMapping{}
			if err = dbQueries.ListDeploymentToApplicationMappingByNamespaceAndName(ctx, gitopsDepl.Name, gitopsDepl.Namespace,
				string(namespace.UID), &dtam); err != nil {
				return db.ManagedEnvironment{}, db.Application{}, err
			}
			Expect(len(dtam)).To(Equal(1))

			application := db.Application{
				Application_id: dtam[0].Application_id,
			}
			if err := dbQueries.GetApplicationById(ctx, &application); err != nil {
				return db.ManagedEnvironment{}, db.Application{}, err
			}

			managedEnvRow := db.ManagedEnvironment{
				Managedenvironment_id: application.Managed_environment_id,
			}
			if err := dbQueries.GetManagedEnvironmentById(ctx, &managedEnvRow); err != nil {
				return db.ManagedEnvironment{}, db.Application{}, err
			}
			return managedEnvRow, application, nil
		}

		listOperationRowsForResource := func(resourceId string, resourceType string) ([]db.Operation, error) {
			operations := []db.Operation{}
			if err := dbQueries.UnsafeListAllOperations(ctx, &operations); err != nil {
				return []db.Operation{}, err
			}

			res := []db.Operation{}

			for idx := range operations {
				operation := operations[idx]
				if operation.Resource_id == resourceId && operation.Resource_type == resourceType {
					res = append(res, operation)
				}
			}
			return res, nil
		}

		// findManagedEnvironmentRowFromCR locates the ManagedEnvironment row that corresponds to the ManagedEnvironment CR
		findManagedEnvironmentRowFromCR := func(managedEnvCR managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment) (string, error) {

			apiCRToDBMapping := db.APICRToDatabaseMapping{
				APIResourceType:      db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
				APIResourceUID:       string(managedEnvCR.UID),
				APIResourceName:      managedEnvCR.Name,
				APIResourceNamespace: managedEnvCR.Namespace,
				NamespaceUID:         string(namespace.UID),
				DBRelationType:       db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment,
			}
			if err := dbQueries.GetDatabaseMappingForAPICR(ctx, &apiCRToDBMapping); err != nil {
				return "", err
			}

			return apiCRToDBMapping.DBRelationKey, nil
		}

		BeforeEach(func() {
			ctx = context.Background()
			scheme,
				argocdNamespace,
				kubesystemNamespace,
				namespace,
				err = tests.GenericTestSetup()
			Expect(err).To(BeNil())

			dbQueries, err = db.NewUnsafePostgresDBQueries(false, true)
			Expect(err).To(BeNil())

			k8sClient.InnerClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, argocdNamespace, kubesystemNamespace).Build()

			err = db.SetupForTestingDBGinkgo()
			Expect(err).To(BeNil())

			_, _, _, _, _, err = db.CreateSampleData(dbQueries)
			Expect(err).To(BeNil())

		})

		It("reconciles a GitOpsDeployment that references a managed environment, then delete the managed env and reconciles", func() {

			managedEnvCR, secretManagedEnv := createManagedEnv()

			By("Creating a GitOpsDeployment pointing to our ManagedEnvironment CR")
			gitopsDepl := createGitOpsDepl()
			gitopsDepl.Spec.Destination.Environment = managedEnvCR.Name
			err = k8sClient.Update(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			eventloop_test_util.StartServiceAccountListenerOnFakeClient(ctx, string(managedEnvCR.UID), k8sClient)

			mockK8sClientFactory := MockSRLK8sClientFactory{
				fakeClient: k8sClient,
			}

			appEventLoopRunnerAction := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					// TODO: GITOPSRVCE-66: Replace this with the new interface: SRLK8sClientFactory
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 string(namespace.UID),
				testOnlySkipCreateOperation: true,
				k8sClientFactory:            mockK8sClientFactory,
			}

			canShutdown, appFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(appFromCall).ToNot(BeNil())
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(appFromCall.Managed_environment_id).ToNot(BeEmpty())
			Expect(userDevErr).To(BeNil())

			By("locating the ManagedEnvironment row that is associated with the ManagedEnvironment CR")
			managedEnvRowFromAPICRToDBMapping, err := findManagedEnvironmentRowFromCR(managedEnvCR)
			Expect(err).To(BeNil())

			By("ensuring the Application row of the GitOpsDeployment matches the Managed Environment row of the ManagedEnv CR")
			managedEnvRow, application, err := getManagedEnvironmentForGitOpsDeployment(*gitopsDepl)
			Expect(err).To(BeNil())
			Expect(managedEnvRow.Managedenvironment_id).To(Equal(managedEnvRowFromAPICRToDBMapping),
				"the managed env from the GitOpsDeployment CR should match the one from the ManagedEnvironment CR")
			Expect(application.Application_id).To(Equal(appFromCall.Application_id),
				"the application object returned from the function call should match the GitOpsDeployment CR we created")

			By("ensuring an Operation was created for the Application")
			applicationOperations, err := listOperationRowsForResource(application.Application_id, "Application")
			Expect(err).To(BeNil())

			Expect(len(applicationOperations)).To(Equal(1))

			applicationOperationFound := false

			for _, operation := range operationCheck.operationEvents {
				if operation.Spec.OperationID == applicationOperations[0].Operation_id {
					applicationOperationFound = true
				}
			}
			Expect(applicationOperationFound).To(BeTrue())

			err = k8sClient.Delete(ctx, &managedEnvCR)
			Expect(err).To(BeNil())

			err = k8sClient.Delete(ctx, &secretManagedEnv)
			Expect(err).To(BeNil())

			By("calling handleDeploymentModified again, after deleting the managed environent and secret")
			canShutdown, appFromSecondCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(appFromCall).ToNot(BeNil())
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(userDevErr).To(BeNil())
			Expect(appFromSecondCall.Application_id).To(Equal(appFromCall.Application_id))

			Expect(appFromSecondCall.Managed_environment_id).To(BeEmpty(),
				"if the managed environment CR is deleted, the ManagedEnvironment field of the application row should be nil")

		})

		It("reconciles a GitOpsDeployment where the managed environment changes from local to GitOpsDeploymentManagedEnvironment", func() {

			By("Creating a GitOpsDeployment targeting the API namespace")
			gitopsDepl := createGitOpsDepl()
			mockK8sClientFactory := MockSRLK8sClientFactory{
				fakeClient: k8sClient,
			}

			appEventLoopRunnerAction := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					// TODO: GITOPSRVCE-66: Replace this with new interface: SRLK8sClientFactory
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 string(namespace.UID),
				testOnlySkipCreateOperation: true,
				k8sClientFactory:            mockK8sClientFactory,
			}

			canShutdown, originalAppFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(originalAppFromCall).ToNot(BeNil())
			Expect(engineInstanceFromCall).ToNot(BeNil())

			Expect(userDevErr).To(BeNil())

			By("ensuring an Operation was created for the Application")
			applicationOperations, err := listOperationRowsForResource(originalAppFromCall.Application_id, "Application")
			Expect(err).To(BeNil())
			Expect(len(applicationOperations)).To(Equal(1))

			{
				By("Modifying the GitOpsDeployment to target a GitOpsDeploymentManagedEnvironment CR, instead of targeting the API Namespace")

				managedEnvCR, _ := createManagedEnv()

				gitopsDepl.Spec.Destination.Environment = managedEnvCR.Name
				err = k8sClient.Update(ctx, gitopsDepl)
				Expect(err).To(BeNil())

				By("calling handleDeploymentModified with the changed GitOpsDeployment")
				eventloop_test_util.StartServiceAccountListenerOnFakeClient(ctx, string(managedEnvCR.UID), k8sClient)
				canShutdown, appFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
				Expect(canShutdown).To(BeFalse())
				Expect(appFromCall).ToNot(BeNil())
				Expect(engineInstanceFromCall).ToNot(BeNil())
				Expect(userDevErr).To(BeNil())
				Expect(appFromCall.Application_id).To(Equal(originalAppFromCall.Application_id))

				Expect(appFromCall.Managed_environment_id).ToNot(Equal(originalAppFromCall.Managed_environment_id),
					"the managed environment on the Application row should have changed.")

				By("locating the ManagedEnvironment row that is associated with the ManagedEnvironment CR")
				managedEnvRowFromAPICRToDBMapping, err := findManagedEnvironmentRowFromCR(managedEnvCR)
				Expect(err).To(BeNil())

				By("ensuring the Application row of the GitOpsDeployment matches the Managed Environment row of the ManagedEnv CR")
				managedEnvRow, application, err := getManagedEnvironmentForGitOpsDeployment(*gitopsDepl)
				Expect(err).To(BeNil())
				Expect(managedEnvRow.Managedenvironment_id).To(Equal(managedEnvRowFromAPICRToDBMapping),
					"the managed env from the GitOpsDeployment CR should match the one from the ManagedEnvironment CR")
				Expect(application.Application_id).To(Equal(appFromCall.Application_id),
					"the application object returned from the function call should match the GitOpsDeployment CR we created")

				applicationOperations, err := listOperationRowsForResource(appFromCall.Application_id, "Application")
				Expect(err).To(BeNil())
				Expect(len(applicationOperations)).To(Equal(2), "a new operation targetting the Application should have been created for the Application")
			}

		})

		It("reconciles a GitOpsDeployment where the managed environment changes from GitOpsDeploymentManagedEnvironment to local", func() {

			managedEnvCR, _ := createManagedEnv()

			By("Creating a GitOpsDeployment pointing to our ManagedEnvironment CR")
			gitopsDepl := createGitOpsDepl()
			gitopsDepl.Spec.Destination.Environment = managedEnvCR.Name
			err = k8sClient.Update(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			eventloop_test_util.StartServiceAccountListenerOnFakeClient(ctx, string(managedEnvCR.UID), k8sClient)

			mockK8sClientFactory := MockSRLK8sClientFactory{
				fakeClient: k8sClient,
			}

			appEventLoopRunnerAction := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					// TODO: GITOPSRVCE-66: Replace this with new interface: SRLK8sClientFactory
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 string(namespace.UID),
				testOnlySkipCreateOperation: true,
				k8sClientFactory:            mockK8sClientFactory,
			}

			canShutdown, originalAppFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.
				applicationEventRunner_handleDeploymentModified(ctx, dbQueries)

			Expect(canShutdown).To(BeFalse())
			Expect(originalAppFromCall).ToNot(BeNil())
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(userDevErr).To(BeNil())
			applicationOperations, err := listOperationRowsForResource(originalAppFromCall.Application_id, "Application")
			Expect(err).To(BeNil())
			Expect(len(applicationOperations)).To(Equal(1))

			By("modifying the GitOpsDeployment CR to target the local namespace, rather than a ManagedEnvironment CR")
			gitopsDepl.Spec.Destination.Environment = ""
			err = k8sClient.Update(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			By("calling handleDeploymentModified again, now that we have updated the GitOpsDeployment")

			canShutdown, appFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.
				applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(appFromCall).ToNot(BeNil())
			Expect(appFromCall.Application_id).To(Equal(originalAppFromCall.Application_id))
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(userDevErr).To(BeNil())

			Expect(appFromCall.Managed_environment_id).ToNot(Equal(originalAppFromCall.Managed_environment_id))

			By("ensuring an Operation was created for the Application")
			applicationOperations, err = listOperationRowsForResource(appFromCall.Application_id, "Application")
			Expect(err).To(BeNil())
			Expect(len(applicationOperations)).To(Equal(2),
				"a second Operation should have been created, since the Application should have changed")

		})

		It("reconciles a GitOpsDeployment where the managed environment changes from one GitOpsDeploymentManagedEnvironment to a second GitOpsDeploymentManagedEnvironment", func() {

			managedEnvCR, _ := createManagedEnv()

			By("Creating a GitOpsDeployment pointing to our ManagedEnvironment CR")
			gitopsDepl := createGitOpsDepl()
			gitopsDepl.Spec.Destination.Environment = managedEnvCR.Name
			err = k8sClient.Update(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			eventloop_test_util.StartServiceAccountListenerOnFakeClient(ctx, string(managedEnvCR.UID), k8sClient)

			mockK8sClientFactory := MockSRLK8sClientFactory{
				fakeClient: k8sClient,
			}

			appEventLoopRunnerAction := applicationEventLoopRunner_Action{
				getK8sClientForGitOpsEngineInstance: func(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
					// TODO: GITOPSRVCE-66: Replace this with new interface: SRLK8sClientFactory
					return k8sClient, nil
				},
				eventResourceName:           gitopsDepl.Name,
				eventResourceNamespace:      gitopsDepl.Namespace,
				workspaceClient:             k8sClient,
				log:                         log.FromContext(context.Background()),
				sharedResourceEventLoop:     shared_resource_loop.NewSharedResourceLoop(),
				workspaceID:                 string(namespace.UID),
				testOnlySkipCreateOperation: true,
				k8sClientFactory:            mockK8sClientFactory,
			}

			canShutdown, originalAppFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(originalAppFromCall).ToNot(BeNil())
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(userDevErr).To(BeNil())
			applicationOperations, err := listOperationRowsForResource(originalAppFromCall.Application_id, "Application")
			Expect(err).To(BeNil())
			Expect(len(applicationOperations)).To(Equal(1))

			By("creating a second GitOpsDeploymentManagedEnvironment")
			var managedEnvCR2 managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment
			{
				kubeConfigContents := generateFakeKubeConfig()
				managedEnvSecret2 := corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-cluster-secret-2",
						Namespace: namespace.Name,
					},
					Type: sharedutil.ManagedEnvironmentSecretType,
					Data: map[string][]byte{
						"kubeconfig": ([]byte)(kubeConfigContents),
					},
				}
				err = k8sClient.Create(ctx, &managedEnvSecret2)
				Expect(err).To(BeNil())

				managedEnvCR2 = managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-managed-env-2",
						Namespace: namespace.Name,
						UID:       uuid.NewUUID(),
					},
					Spec: managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
						APIURL:                   "https://api.fake-unit-test-data.origin-ci-int-gce.dev.rhcloud.com:6443",
						ClusterCredentialsSecret: managedEnvSecret2.Name,
					},
				}
				err = k8sClient.Create(ctx, &managedEnvCR2)
				Expect(err).To(BeNil())

				eventloop_test_util.StartServiceAccountListenerOnFakeClient(ctx, string(managedEnvCR2.UID), k8sClient)

			}

			By("updating the GitOpsDeployment to point to the new ManagedEnvironment")
			gitopsDepl.Spec.Destination.Environment = managedEnvCR2.Name
			err = k8sClient.Update(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			By("calling handleDeploymentModified again, now that we have updated the GitOpsDeployment")
			canShutdown, appFromCall, engineInstanceFromCall, _, userDevErr := appEventLoopRunnerAction.applicationEventRunner_handleDeploymentModified(ctx, dbQueries)
			Expect(canShutdown).To(BeFalse())
			Expect(appFromCall).ToNot(BeNil())
			Expect(appFromCall.Application_id).To(Equal(originalAppFromCall.Application_id))
			Expect(engineInstanceFromCall).ToNot(BeNil())
			Expect(userDevErr).To(BeNil())
			Expect(appFromCall.Managed_environment_id).ToNot(Equal(originalAppFromCall.Managed_environment_id))

			By("locating the ManagedEnvironment row that is associated with the new ManagedEnvironment CR")
			managedEnvRowFromAPICRToDBMapping2, err := findManagedEnvironmentRowFromCR(managedEnvCR2)
			Expect(err).To(BeNil())

			By("ensuring the Application row of the GitOpsDeployment matches the Managed Environment row of the new ManagedEnv CR")
			managedEnvRow2, application, err := getManagedEnvironmentForGitOpsDeployment(*gitopsDepl)
			Expect(err).To(BeNil())
			Expect(managedEnvRow2.Managedenvironment_id).To(Equal(managedEnvRowFromAPICRToDBMapping2),
				"the managed env from the GitOpsDeployment CR should match the one from the new ManagedEnvironment CR")
			Expect(application.Application_id).To(Equal(appFromCall.Application_id),
				"the application object returned from the function call should match the GitOpsDeployment CR we created")

			applicationOperations, err = listOperationRowsForResource(appFromCall.Application_id, "Application")
			Expect(err).To(BeNil())
			Expect(len(applicationOperations)).To(Equal(2),
				"a new operation targetting the Application should have been created for the Application")

		})
	})
})

type MockSRLK8sClientFactory struct {
	fakeClient client.Client
}

func (f MockSRLK8sClientFactory) BuildK8sClient(restConfig *rest.Config) (client.Client, error) {
	return f.fakeClient, nil
}

func (f MockSRLK8sClientFactory) GetK8sClientForGitOpsEngineInstance(gitopsEngineInstance *db.GitopsEngineInstance) (client.Client, error) {
	return f.fakeClient, nil
}

var _ = Describe("Miscellaneous application_event_runner.go tests", func() {

	Context("Test handleManagedEnvironmentModified", func() {

		var ctx context.Context
		var scheme *runtime.Scheme
		var err error

		var namespace *corev1.Namespace
		var argocdNamespace *corev1.Namespace
		var dbQueries db.AllDatabaseQueries
		var kubesystemNamespace *corev1.Namespace

		var k8sClient client.Client

		var clusterCredentials *db.ClusterCredentials
		var engineInstance *db.GitopsEngineInstance

		// createManagedEnv creates a managed environment DB row and CR, connected via APICRToDBMApping
		createManagedEnv := func() (db.ManagedEnvironment, managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment, db.APICRToDatabaseMapping) {

			managedEnvCR := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-managed-env",
					Namespace: namespace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentSpec{
					APIURL:                   "https://api-url",
					ClusterCredentialsSecret: "fake-secret-name",
				},
			}
			err = k8sClient.Create(ctx, &managedEnvCR)
			Expect(err).To(BeNil())

			managedEnvironment := db.ManagedEnvironment{
				Managedenvironment_id: "test-managed-env",
				Name:                  "my-managed-env",
				Clustercredentials_id: clusterCredentials.Clustercredentials_cred_id,
			}
			err = dbQueries.CreateManagedEnvironment(ctx, &managedEnvironment)
			Expect(err).To(BeNil())

			apiCRToDBMapping := db.APICRToDatabaseMapping{
				APIResourceType:      db.APICRToDatabaseMapping_ResourceType_GitOpsDeploymentManagedEnvironment,
				APIResourceUID:       string(managedEnvCR.UID),
				APIResourceName:      managedEnvCR.Name,
				APIResourceNamespace: managedEnvCR.Namespace,
				NamespaceUID:         string(namespace.UID),
				DBRelationType:       db.APICRToDatabaseMapping_DBRelationType_ManagedEnvironment,
				DBRelationKey:        managedEnvironment.Managedenvironment_id,
			}
			err = dbQueries.CreateAPICRToDatabaseMapping(ctx, &apiCRToDBMapping)
			Expect(err).To(BeNil())

			return managedEnvironment, managedEnvCR, apiCRToDBMapping
		}

		// Create a simple GitOpsDeployment CR
		createGitOpsDepl := func() *managedgitopsv1alpha1.GitOpsDeployment {
			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: namespace.Name,
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL:        "https://github.com/abc-org/abc-repo",
						Path:           "/abc-path",
						TargetRevision: "abc-commit"},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
					Destination: managedgitopsv1alpha1.ApplicationDestination{
						Namespace: "abc-namespace",
					},
				},
			}
			err = k8sClient.Create(ctx, gitopsDepl)
			Expect(err).To(BeNil())

			return gitopsDepl
		}

		BeforeEach(func() {
			ctx = context.Background()
			scheme,
				argocdNamespace,
				kubesystemNamespace,
				namespace,
				err = tests.GenericTestSetup()
			Expect(err).To(BeNil())

			dbQueries, err = db.NewUnsafePostgresDBQueries(false, true)
			Expect(err).To(BeNil())

			k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, argocdNamespace, kubesystemNamespace).Build()

			err = db.SetupForTestingDBGinkgo()
			Expect(err).To(BeNil())

			clusterCredentials, _, _, engineInstance, _, err = db.CreateSampleData(dbQueries)
			Expect(err).To(BeNil())

		})

		It("return true/false if gitopsDeployment.Spec.Destination.Environment == managedEnvEvent.Request.Name", func() {

			_, managedEnvCR, _ := createManagedEnv()

			By("creating a GitOpsDeployment that initially doesn't reference the ManagedEnvironment CR")
			gitopsDepl := createGitOpsDepl()

			By("creating a new event that references the ManagedEnvironment")
			newEvent := eventlooptypes.EventLoopEvent{
				EventType: eventlooptypes.ManagedEnvironmentModified,
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: namespace.Name, Name: managedEnvCR.Name},
				},
				Client:                  k8sClient,
				ReqResource:             eventlooptypes.GitOpsDeploymentManagedEnvironmentTypeName,
				AssociatedGitopsDeplUID: "",
				WorkspaceID:             string(namespace.UID),
			}

			By("calling the function with a ManagedEnvironment event")
			informGitOpsDepl, err := handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx, *gitopsDepl, &newEvent, dbQueries)
			Expect(err).To(BeNil())
			Expect(informGitOpsDepl).To(BeFalse(), "GitOpsDepl runner should not be informed if the ManagedEnvironment CR doesn't reference the GitOpsDeployment CR")

			By("updating the environment field of GitOpsDeployment, which should change the test result")
			gitopsDepl.Spec.Destination = managedgitopsv1alpha1.ApplicationDestination{
				Environment: managedEnvCR.Name,
				Namespace:   gitopsDepl.Spec.Destination.Namespace,
			}
			informGitOpsDepl, err = handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx, *gitopsDepl, &newEvent, dbQueries)
			Expect(err).To(BeNil())
			Expect(informGitOpsDepl).To(BeTrue(), "GitOpsDepl runner SHOULD be informed if the ManagedEnvironment CR references the GitOpsDeployment CR")

		})

		It("should return true if there are applications that match managed environments, via APICRToDBMapping and DeplToAppMapping", func() {

			managedEnvironment, managedEnvCR, apiCRToDBMapping := createManagedEnv()

			gitopsDepl := createGitOpsDepl()

			By("creating an Application row that references the ManagedEnvironment row")
			application := db.Application{
				Application_id:          "test-my-application",
				Name:                    "application",
				Spec_field:              "{}",
				Engine_instance_inst_id: engineInstance.Gitopsengineinstance_id,
				Managed_environment_id:  managedEnvironment.Managedenvironment_id,
			}
			err = dbQueries.CreateApplication(ctx, &application)
			Expect(err).To(BeNil())

			By("connecting the Application row to the GitOpsDeployment CR")
			dtam := db.DeploymentToApplicationMapping{
				Deploymenttoapplicationmapping_uid_id: "test-dtam",
				DeploymentName:                        gitopsDepl.Name,
				DeploymentNamespace:                   gitopsDepl.Namespace,
				NamespaceUID:                          string(namespace.UID),
				Application_id:                        application.Application_id,
			}
			err = dbQueries.CreateDeploymentToApplicationMapping(ctx, &dtam)
			Expect(err).To(BeNil())

			By("calling the function with a ManagedEnvironment event")
			newEvent := eventlooptypes.EventLoopEvent{
				EventType: eventlooptypes.ManagedEnvironmentModified,
				Request: reconcile.Request{
					NamespacedName: types.NamespacedName{Namespace: namespace.Name, Name: managedEnvCR.Name},
				},
				Client:                  k8sClient,
				ReqResource:             eventlooptypes.GitOpsDeploymentManagedEnvironmentTypeName,
				AssociatedGitopsDeplUID: "",
				WorkspaceID:             string(namespace.UID),
			}

			informGitOpsDepl, err := handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx, *gitopsDepl, &newEvent, dbQueries)
			Expect(err).To(BeNil())
			Expect(informGitOpsDepl).To(BeTrue(), "when the Application DB row references the corresponding ManagedEnv row, it should return true")

			By("deleting the connection from the ManagedEnv CR to the ManagedEnv row")
			rowsDeleted, err := dbQueries.DeleteAPICRToDatabaseMapping(ctx, &apiCRToDBMapping)
			Expect(err).To(BeNil())
			Expect(rowsDeleted).To(Equal(1))

			By("calling the function with a ManagedEnvironment event, but this time we expect a different result")
			informGitOpsDepl, err = handleManagedEnvironmentModified_shouldInformGitOpsDeployment(ctx, *gitopsDepl, &newEvent, dbQueries)
			Expect(err).To(BeNil())
			Expect(informGitOpsDepl).To(BeFalse(), "when function can't locate the ManagedEnvironment row from the CR, it should return false")

		})

	})
})

func generateFakeKubeConfig() string {
	// This config has been sanitized of any real credentials.
	return `
apiVersion: v1
kind: Config
clusters:
  - cluster:
      insecure-skip-tls-verify: true
      server: https://api.fake-unit-test-data.origin-ci-int-gce.dev.rhcloud.com:6443
    name: api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
  - cluster:
      insecure-skip-tls-verify: true
      server: https://api2.fake-unit-test-data.origin-ci-int-gce.dev.rhcloud.com:6443
    name: api2-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
contexts:
  - context:
      cluster: api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
      namespace: jgw
      user: kube:admin/api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
    name: default/api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443/kube:admin
  - context:
      cluster: api2-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
      namespace: jgw
      user: kube:admin/api2-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
    name: default/api2-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443/kube:admin
current-context: default/api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443/kube:admin
preferences: {}
users:
  - name: kube:admin/api-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
    user:
      token: sha256~ABCdEF1gHiJKlMnoP-Q19qrTuv1_W9X2YZABCDefGH4
  - name: kube:admin/api2-fake-unit-test-data-origin-ci-int-gce-dev-rhcloud-com:6443
    user:
      token: sha256~abcDef1gHIjkLmNOp-q19QRtUV1_w9x2yzabcdEFgh4
`
}
