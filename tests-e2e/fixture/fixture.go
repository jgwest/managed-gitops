package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	codereadytoolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"

	argocdoperator "github.com/argoproj-labs/argocd-operator/api/v1alpha1"
	appv1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appstudiosharedv1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/tests-e2e/fixture/k8s"

	routev1 "github.com/openshift/api/route/v1"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/db/util"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiexts "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// GitOpsServiceE2ENamespace is the namespace that GitOpsService API resources (GitOpsDeployments, etc) should created it
	GitOpsServiceE2ENamespace = "gitops-service-e2e"

	// NewArgoCDInstanceNamespace  is the namespace thats test should use if they wish to install a new Argo CD instance
	NewArgoCDInstanceNamespace = "test-my-argocd"

	// NewArgoCDInstanceDestNamespace is the destinaton Argo CD Application namespace tests should use if they wish to deploy from a new Argo CD instance
	NewArgoCDInstanceDestNamespace = "argocd-instance-dest-namespace"

	// RepoURL is the URL to the git repository
	RepoURL = "https://github.com/redhat-appstudio/managed-gitops"

	GitopsDeploymentName = "my-gitops-depl"

	DTCName = "test-dtc"

	DTName = "test-dt"

	GitopsDeploymentPath = "resources/test-data/sample-gitops-repository/environments/overlays/dev"
)

const (
	// EnableNamespaceBackedArgoCD is currently disabled, due to concerns with performance issues when Argo CD is running in namespace-scoped mode.
	// When enabled, this will ensure that certain tests/fixtures add the managed by label to Namespace.
	EnableNamespaceBackedArgoCD = false
)

// EnsureCleanSlateNonKCPVirtualWorkspace should be called before every E2E tests:
// it ensures that the state of the GitOpsServiceE2ENamespace namespace (and other resources on the cluster) is reset
// to scratch before each test, including:
// - Removing any old GitOps Service API resources in GitOpsServiceE2ENamespace
// - Removing any old resources in GitOpsServiceE2ENamespace, by deleting the namespace
// - Recreating the GitOpsServiceE2ENamespace with the expected settings
// - Waiting for Argo CD to watch the namespace before exiting (to prevent race conditions)
// - Remove the second Argo CD namespace, for the limited # of tests that create a new Argo CD instance.
//
// This ensures that previous E2E tests runs do not interfere with the results of current test runs.
// This function can also be called after a test, in order to clean up any resources it create in the GitOpsServiceE2ENamespace.
func EnsureCleanSlateNonKCPVirtualWorkspace() error {

	clientconfig, err := GetSystemKubeConfig()
	if err != nil {
		return err
	}
	// Clean up after tests that target the non-default Argo CD instance (only used by a few E2E tests)
	if err := cleanUpOldArgoCDApplications(NewArgoCDInstanceDestNamespace, NewArgoCDInstanceDestNamespace, clientconfig); err != nil {
		return fmt.Errorf("unable to clean up old Argo CD applications that target a non-default Argo CD: %v", err)
	}

	// Clean up after various old namespaces that are used in tests
	prevTestNamespacesToDelete := []string{"new-e2e-test-namespace", "new-e2e-test-namespace2", NewArgoCDInstanceNamespace, NewArgoCDInstanceDestNamespace}
	for _, namespaceName := range prevTestNamespacesToDelete {
		if err := DeleteNamespace(namespaceName, clientconfig); err != nil {
			return fmt.Errorf("unable to delete namespace '%s': %v", namespaceName, err)
		}
	}

	// Clean up after tests that target the default Argo CD E2E instance (used by most E2E tests)
	if err := cleanUpOldArgoCDApplications(dbutil.GetGitOpsEngineSingleInstanceNamespace(), GitOpsServiceE2ENamespace, clientconfig); err != nil {
		return fmt.Errorf("unable to clean up old Argo CD applications that target default Argo CD NS: %v", err)
	}

	if err := EnsureDestinationNamespaceExists(GitOpsServiceE2ENamespace, dbutil.GetGitOpsEngineSingleInstanceNamespace(), clientconfig); err != nil {
		return fmt.Errorf("unable to ensure destination namespace '%s' exists: %v", GitOpsServiceE2ENamespace, err)
	}

	if err := cleanUpOldKubeSystemResources(clientconfig); err != nil {
		return fmt.Errorf("unable to clean up old kube system resources: %v", err)
	}

	// Delete all Argo CD Cluster Secrets from the default Argo CD Namespace
	secretList := &corev1.SecretList{}
	k8sClient, err := GetKubeClient(clientconfig)
	if err != nil {
		return fmt.Errorf("unable to get kube client: %v", err)
	}
	if err := k8sClient.List(context.Background(), secretList, &client.ListOptions{Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace()}); err != nil {
		return fmt.Errorf("unable to list secrets in Argo CD namespace: %v", err)
	}
	for idx := range secretList.Items {
		secret := secretList.Items[idx]
		if strings.HasPrefix(secret.Name, "managed-env") {
			if err := k8sClient.Delete(context.Background(), &secret); err != nil {
				return fmt.Errorf("unable to delete old secret '%s': %v", secret.Name, err)
			}
		}
	}

	return nil
}

// cleanUpOldArgoCDApplications issues a Delete request to k8s, for any Argo CD Applications in 'namespaceParam' that
// have a .Spec.Destination of 'destNamespace'
func cleanUpOldArgoCDApplications(namespaceParam string, destNamespace string, clientConfig *rest.Config) error {
	k8sClient, err := GetKubeClient(clientConfig)
	if err != nil {
		return err
	}

	// Delete (and remove finalizers) from any Argo CD Applications in this Namespace
	if err := wait.PollImmediate(time.Second*1, time.Minute*2, func() (done bool, err error) {

		// List all the Argo CD Applications in 'namespaceParam' namespace
		argoCDApplicationList := appv1alpha1.ApplicationList{}
		if err = k8sClient.List(context.Background(), &argoCDApplicationList, &client.ListOptions{Namespace: namespaceParam}); err != nil {
			return false, fmt.Errorf("unable to list '%s': %v . Verify that Argo CD is installed on the cluster", namespaceParam, err)
		}

		matchesFound := 0

		// Delete all Argo CD Application resources that reference the destination namespace
		for idx := range argoCDApplicationList.Items {

			app := argoCDApplicationList.Items[idx]
			if app.Spec.Destination.Namespace == destNamespace {

				matchesFound++

				// Remove any finalizers on the Application, so they can be deleted
				if len(app.Finalizers) > 0 {
					app.Finalizers = []string{}
					if err := k8sClient.Update(context.Background(), &app); err != nil {
						GinkgoWriter.Println("an error occurred on removing finalizer", err)
						return false, nil
					}
				}

				GinkgoWriter.Println("Deleting Argo CD Application:", app.Name)
				if err := k8sClient.Delete(context.Background(), &app); err != nil {
					if !apierr.IsNotFound(err) {
						return false, fmt.Errorf("unable to delete '%s': %v", namespaceParam, err)
					}
				}

			}
		}

		// Keep trying until there are no Argo CD Applications left in the Namespace
		return matchesFound == 0, nil

	}); err != nil {
		return err
	}

	return nil
}

// cleanUpOldKubeSystemResources cleans up ServiceAccounts, ClusterRoles, and ClusterRoleBindings created by these tests.
func cleanUpOldKubeSystemResources(clientConfig *rest.Config) error {
	k8sClient, err := GetKubeClient(clientConfig)
	if err != nil {
		return err
	}

	saList := corev1.ServiceAccountList{}
	if err := k8sClient.List(context.Background(), &saList, &client.ListOptions{Namespace: "kube-system"}); err != nil {
		return err
	}

	for idx := range saList.Items {
		sa := saList.Items[idx]
		// Skip any service accounts that DON'T contain argocd, and skip argocd-manager
		if !strings.Contains(sa.Name, "argocd") || sa.Name == "argocd-manager" {
			continue
		}

		if err := k8sClient.Delete(context.Background(), &sa); err != nil {
			fmt.Println("unable to delete service account", sa)
			return err
		}
	}

	crList := rbacv1.ClusterRoleBindingList{}
	if err := k8sClient.List(context.Background(), &crList, &client.ListOptions{}); err != nil {
		return err
	}

	for idx := range crList.Items {
		sa := crList.Items[idx]
		// Skip any CRBs that DON'T contain argocd-manager
		if !strings.Contains(sa.Name, "argocd-manager") {
			continue
		}

		if err := k8sClient.Delete(context.Background(), &sa); err != nil {
			return err
		}
	}

	crbList := rbacv1.ClusterRoleList{}
	if err := k8sClient.List(context.Background(), &crbList, &client.ListOptions{}); err != nil {
		return err
	}

	for idx := range crbList.Items {
		sa := crbList.Items[idx]
		// Skip any CRBs that DON'T contain argocd-manager
		if !strings.Contains(sa.Name, "argocd-manager") {
			continue
		}

		if err := k8sClient.Delete(context.Background(), &sa); err != nil {
			return err
		}
	}

	return nil
}

// cleanUpOldGitOpsServiceAPIs deletes old GitOpsDeployment* APIs
func cleanUpOldGitOpsServiceAPIs(namespace string, k8sClient client.Client) (bool, error) {

	gitopsDeploymentList := managedgitopsv1alpha1.GitOpsDeploymentList{}
	if err := k8sClient.List(context.Background(), &gitopsDeploymentList, &client.ListOptions{Namespace: namespace}); err != nil {
		return false, fmt.Errorf("unable to cleanup old GitOpsDeployments: %v", err)
	}

	for idx := range gitopsDeploymentList.Items {
		gitopsDeployment := gitopsDeploymentList.Items[idx]

		// If the GitOpsDeployment contains a finalizer, remove it before deleting.
		if len(gitopsDeployment.Finalizers) > 0 {
			gitopsDeployment.Finalizers = []string{}
			if err := k8sClient.Update(context.Background(), &gitopsDeployment); err != nil {
				return false, fmt.Errorf("unable to remove finalizer from GitOpsDeployment: %v", err)
			}
		}

		if err := k8sClient.Delete(context.Background(), &gitopsDeployment); err != nil {
			return false, err
		}
	}

	gitopsManagedEnvironments := managedgitopsv1alpha1.GitOpsDeploymentManagedEnvironmentList{}
	if err := k8sClient.List(context.Background(), &gitopsManagedEnvironments, &client.ListOptions{Namespace: namespace}); err != nil {
		return false, fmt.Errorf("unable to cleanup old GitOpsDeployments: %v", err)
	}

	for idx := range gitopsManagedEnvironments.Items {
		gitopsManagedEnv := gitopsManagedEnvironments.Items[idx]
		if err := k8sClient.Delete(context.Background(), &gitopsManagedEnv); err != nil {
			return false, err
		}
	}

	return len(gitopsDeploymentList.Items) == 0 && len(gitopsManagedEnvironments.Items) == 0, nil

}

func cleanUpOldHASApplicationAPIs(namespace string, k8sClient client.Client) (bool, error) {
	applicationList := appstudiosharedv1.ApplicationList{}
	if err := k8sClient.List(context.Background(), &applicationList, &client.ListOptions{Namespace: namespace}); err != nil {
		return false, fmt.Errorf("unable to cleanup old HAS Applications: %w", err)
	}

	for i := range applicationList.Items {
		application := applicationList.Items[i]
		if err := k8sClient.Delete(context.Background(), &application); err != nil {
			if !apierr.IsNotFound(err) {
				return false, err
			}
		}
	}

	return len(applicationList.Items) == 0, nil
}

func cleanUpOldEnvironmentAPIs(namespace string, k8sClient client.Client) (bool, error) {
	environmentList := appstudiosharedv1.EnvironmentList{}
	if err := k8sClient.List(context.Background(), &environmentList, &client.ListOptions{Namespace: namespace}); err != nil {
		return false, fmt.Errorf("unable to cleanup old AppStudio Environments: %w", err)
	}

	for i := range environmentList.Items {
		environment := environmentList.Items[i]
		if err := k8sClient.Delete(context.Background(), &environment); err != nil {
			return false, err
		}
	}

	return len(environmentList.Items) == 0, nil
}

func cleanUpOldSnapshotEnvironmentBindingAPIs(namespace string, k8sClient client.Client) (bool, error) {
	bindingList := appstudiosharedv1.SnapshotEnvironmentBindingList{}
	if err := k8sClient.List(context.Background(), &bindingList, &client.ListOptions{Namespace: namespace}); err != nil {
		return false, fmt.Errorf("unable to cleanup old SnapshotEnvironmentBindings: %w", err)
	}

	for i := range bindingList.Items {
		binding := bindingList.Items[i]
		if err := k8sClient.Delete(context.Background(), &binding); err != nil {
			return false, err
		}
	}

	return len(bindingList.Items) == 0, nil

}

func cleanUpOldDeploymentTargetClaimAPIs(ns string, k8sClient client.Client) (bool, error) {
	dtcList := appstudiosharedv1.DeploymentTargetClaimList{}

	if err := k8sClient.List(context.Background(), &dtcList, &client.ListOptions{Namespace: ns}); err != nil {
		if !apierr.IsNotFound(err) {
			return false, err
		} else { // ignore not found
			return true, nil
		}
	}

	for i := range dtcList.Items {
		dtc := dtcList.Items[i]

		if err := removeFinalizersAndDeleteK8sObject(&dtc, k8sClient); err != nil {
			return false, err
		}
	}

	return len(dtcList.Items) == 0, nil

}

func cleanUpOldDeploymentTargetAPIs(ns string, k8sClient client.Client) (bool, error) {
	dtList := appstudiosharedv1.DeploymentTargetList{}
	if err := k8sClient.List(context.Background(), &dtList, &client.ListOptions{Namespace: ns}); err != nil {
		return false, err
	}

	for i := range dtList.Items {
		dt := dtList.Items[i]

		if err := removeFinalizersAndDeleteK8sObject(&dt, k8sClient); err != nil {
			return false, err
		}

	}

	return len(dtList.Items) == 0, nil

}

func cleanUpOldDeploymentTargetClassAPIs(ns string, k8sClient client.Client) (bool, error) {
	dtclsList := appstudiosharedv1.DeploymentTargetClassList{}
	if err := k8sClient.List(context.Background(), &dtclsList); err != nil {
		return false, err
	}

	for i := range dtclsList.Items {
		dtcls := dtclsList.Items[i]

		if err := removeFinalizersAndDeleteK8sObject(&dtcls, k8sClient); err != nil {
			return false, err
		}
	}

	return len(dtclsList.Items) == 0, nil

}

func cleanUpOldSpaceRequestAPIs(ns string, k8sClient client.Client) (bool, error) {
	spaceRequestList := codereadytoolchainv1alpha1.SpaceRequestList{}
	if err := k8sClient.List(context.Background(), &spaceRequestList, &client.ListOptions{Namespace: ns}); err != nil {
		return false, err
	}

	for i := range spaceRequestList.Items {
		spaceRequest := spaceRequestList.Items[i]

		if err := removeFinalizersAndDeleteK8sObject(&spaceRequest, k8sClient); err != nil {
			return false, err
		}
	}

	return len(spaceRequestList.Items) == 0, nil

}

func removeFinalizersAndDeleteK8sObject(spaceRequest client.Object, k8sClient client.Client) error {

	// Make sure that all finalizers are removed before deletion.
	if err := removeAllFinalizers(k8sClient, spaceRequest); err != nil {
		if !apierr.IsNotFound(err) { // ignore not found
			return err
		}
	}

	if err := k8sClient.Delete(context.Background(), spaceRequest); err != nil {
		if !apierr.IsNotFound(err) { // ignore not found
			return err
		}
	}

	return nil

}

func removeAllFinalizers(k8sClient client.Client, obj client.Object) error {

	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		if !apierr.IsNotFound(err) {
			return err
		} else {
			// ignore not found
			return nil
		}
	}

	if len(obj.GetFinalizers()) == 0 {
		return nil
	}

	obj.SetFinalizers([]string{})

	if err := k8sClient.Update(context.Background(), obj); err != nil {
		if !apierr.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func isArgoCDClusterScoped(argoCDNamespaceParam string, clientConfig *rest.Config) (bool, error) {

	k8sClient, err := GetKubeClient(clientConfig)
	if err != nil {
		return false, err
	}

	saList := &corev1.ServiceAccountList{}

	if err := k8sClient.List(context.Background(), saList, &client.ListOptions{Namespace: argoCDNamespaceParam}); err != nil {
		return false, err
	}

	crbList := &rbacv1.ClusterRoleBindingList{}
	if err := k8sClient.List(context.Background(), crbList); err != nil {
		return false, err
	}

	// For each ClusterRoleBinding
	for _, crbItem := range crbList.Items {

		// For each Subject of the Binding
		for _, subject := range crbItem.Subjects {

			// Return true if there exists a subject pointing to one of the ServiceAccounts
			// in the Argo CD namepace.
			if subject.Namespace == argoCDNamespaceParam {

				for _, sa := range saList.Items {

					if sa.Name == subject.Name {
						return true, nil
					}
				}

			}
		}
	}

	return false, nil

}

func EnsureDestinationNamespaceExists(namespaceParam string, argoCDNamespaceParam string, clientConfig *rest.Config) error {

	kubeClientSet, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("unable to retrieve kubernetes config: %v", err)
	}

	if err := DeleteNamespace(namespaceParam, clientConfig); err != nil {
		return fmt.Errorf("unable to delete namespace '%s': %v", namespaceParam, err)
	}

	namespaceToCreate := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespaceParam,
	}}

	if EnableNamespaceBackedArgoCD {
		namespaceToCreate.Labels = map[string]string{
			"argocd.argoproj.io/managed-by": argoCDNamespaceParam,
		}
	}

	// Create the namespace again
	_, err = kubeClientSet.CoreV1().Namespaces().Create(context.Background(), &namespaceToCreate, metav1.CreateOptions{})

	if err != nil {
		return fmt.Errorf("unable to create namespace '%s': %v", namespaceParam, err)
	}

	// We used to wait here, prior to moving to Cluster Scoped Argo CD
	// eg. Call: waitForArgoCDToProcessNamespace(namespaceParam, argoCDNamespaceParam, kubeClientSet)
	if EnableNamespaceBackedArgoCD {
		if err := waitForArgoCDToProcessNamespace(namespaceParam, argoCDNamespaceParam, kubeClientSet, clientConfig); err != nil {
			return fmt.Errorf("unable to wait for Argo CD to process namespace: %w", err)
		}
	}

	return nil
}

// Wait for Argo CD to process the namespace, before we exit:
//   - This helps us avoid a race condition where the namespace is created, but Argo CD has not yet
//     set up proper roles for it.
func waitForArgoCDToProcessNamespace(namespaceParam string, argoCDNamespaceParam string, kubeClientSet *kubernetes.Clientset, clientConfig *rest.Config) error {

	if isClusterScoped, err := isArgoCDClusterScoped(argoCDNamespaceParam, clientConfig); isClusterScoped || err != nil {
		return err
	}

	// Wait for Argo CD to process the namespace, before we exit:
	// - This helps us avoid a race condition where the namespace is created, but Argo CD has not yet
	//   set up proper roles for it.

	if err := wait.PollImmediate(time.Second*1, time.Minute*2, func() (done bool, err error) {
		var roleBindings *rbacv1.RoleBindingList
		roleBindings, err = kubeClientSet.RbacV1().RoleBindings(namespaceParam).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		count := 0
		// Exit the poll loop when there exists at least one rolebinding containing 'gitops-service-argocd' in it's name
		// This helps us avoid a race condition where the namespace is created, but Argo CD has not yet
		// set up proper roles for it.
		for _, item := range roleBindings.Items {
			if strings.Contains(item.Name, argoCDNamespaceParam+"-") {
				count++
			}
		}

		if count >= 2 {
			// We expect at least 2 rolebindings
			return true, nil
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("PollImmediate waiting for Argo CD role in namespace '%s', failed: %v", namespaceParam, err)
	}

	if err := wait.PollImmediate(time.Second*1, time.Minute*2, func() (done bool, err error) {
		var roles *rbacv1.RoleList
		roles, err = kubeClientSet.RbacV1().Roles(namespaceParam).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}

		count := 0
		// Exit the poll loop when there exists at least one rolebinding containing 'gitops-service-argocd' in it's name
		// This helps us avoid a race condition where the namespace is created, but Argo CD has not yet
		// set up proper roles for it.
		for _, item := range roles.Items {
			if strings.Contains(item.Name, argoCDNamespaceParam+"-") {
				count++
			}
		}

		if count >= 2 {
			// We expect at least 2 roles
			return true, nil
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("argo CD never setup rolebindings for namespace '%s': %v", namespaceParam, err)
	}

	return nil
}

// DeleteNamespace deletes a namespace, and waits for it to be reported as deleted.
func DeleteNamespace(namespaceParam string, clientConfig *rest.Config) error {

	k8sClient, err := GetKubeClient(clientConfig)
	if err != nil {
		return err
	}

	// Delete the namespace:
	// - First, we do a bunch of clean up in the namespace (and in the Argo CD namespace), to ensure the namespace
	//   can be successfully clean.ed
	// - Next, we issue a request to Delete the namespace
	// - Finally, we check if it has been deleted.
	if err := wait.PollImmediate(time.Second*5, time.Minute*6, func() (done bool, err error) {

		// Deletes old HAS Application APIs in this namespace
		if emptyList, err := cleanUpOldHASApplicationAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old HAS Application APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old appstudio Environment APIs in this namespace
		if emptyList, err := cleanUpOldEnvironmentAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old Environment APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old SnapshotEnvironmentBinding APIs in this namespace
		if emptyList, err := cleanUpOldSnapshotEnvironmentBindingAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old Environment APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old DeploymentTargetClaim APIs in this namespace
		if emptyList, err := cleanUpOldDeploymentTargetClaimAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old DeploymentTargetClaim APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old DeploymentTarget APIs in this namespace
		if emptyList, err := cleanUpOldDeploymentTargetAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old DeploymentTarget APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old DeploymentTargetClass APIs in this namespace
		if emptyList, err := cleanUpOldDeploymentTargetClassAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old DeploymentTargetClass APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old SpaceRequest APIs in this namespace
		if emptyList, err := cleanUpOldSpaceRequestAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old SpaceRequest APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Deletes old GitOpsDeployment* APIs in this namespace
		if emptyList, err := cleanUpOldGitOpsServiceAPIs(namespaceParam, k8sClient); err != nil {
			GinkgoWriter.Printf("unable to delete old GitOps Service APIs in '%s': %v\n", namespaceParam, err)
			return false, nil
		} else if !emptyList {
			return false, nil
		}

		// Delete any Argo CD Applications in the Argo CD namespace that target this namespace
		if err := cleanUpOldArgoCDApplications(dbutil.GetGitOpsEngineSingleInstanceNamespace(), namespaceParam, clientConfig); err != nil {
			GinkgoWriter.Printf("unable to clean up old Argo CD Applications targeting '%s': %v'\n", namespaceParam, err)
			return false, nil
		}

		// Remove finalizers from any Argo CD Applications in this Namespace
		if err := wait.PollImmediate(time.Second*1, time.Minute*2, func() (done bool, err error) {

			argoCDApplicationList := appv1alpha1.ApplicationList{}
			if err = k8sClient.List(context.Background(), &argoCDApplicationList, &client.ListOptions{Namespace: namespaceParam}); err != nil {
				GinkgoWriter.Println("unable to list Argo CD Applications in '"+namespaceParam+"'", err)
				return false, nil
			}

			for idx := range argoCDApplicationList.Items {

				app := argoCDApplicationList.Items[idx]
				if len(app.Finalizers) > 0 {
					err = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&app), &app)
					if err != nil {
						GinkgoWriter.Println("unable to get Application '"+app.Name+"'", err)
						return false, nil
					}
					app.Finalizers = []string{}
					err = k8sClient.Update(context.Background(), &app)
					if err != nil {
						GinkgoWriter.Println("unable to update Application '"+app.Name+"'", err)
						return false, nil
					}
				}
			}

			return true, nil
		}); err != nil {
			GinkgoWriter.Printf("unable to remove finalizer from Argo CD Applications in namespace '%s': %v\n", namespaceParam, err)
			return false, nil
		}

		// Delete the namespace, if it exists
		namespace := corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespaceParam,
			},
		}
		if err := k8sClient.Delete(context.Background(), &namespace); err != nil {
			if !apierr.IsNotFound(err) {
				GinkgoWriter.Printf("unable to delete namespace '%s': %v\n", namespaceParam, err)
				return false, nil
			}
		}

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(&namespace), &namespace); err != nil {
			if apierr.IsNotFound(err) {
				return true, nil
			} else {
				GinkgoWriter.Printf("unable to Get namespace '%s': %v\n", namespaceParam, err)
				return false, nil
			}
		}

		return false, nil
	}); err != nil {
		return fmt.Errorf("namespace was never deleted, after delete was issued. '%s':%v", namespaceParam, err)
	}

	return nil
}

// setRateLimitOnRestConfig: set the QPS and Burst of the rest config
func setRateLimitOnRestConfig(restConfig *rest.Config) error {
	if restConfig != nil {
		// Prevent rate limiting of our requests
		restConfig.QPS = 100
		restConfig.Burst = 250

		// Sanity check that we're not running on a known staging system: don't want to accidentally break staging :|
		if strings.Contains(restConfig.Host, "x99m.p1.openshiftapps.com") {
			return fmt.Errorf("E2E tests should not be run on staging server")
		}
	}
	return nil
}

// Retrieve the system-level Kubernetes config (e.g. ~/.kube/config)
func GetSystemKubeConfig() (*rest.Config, error) {

	overrides := clientcmd.ConfigOverrides{}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, &overrides, os.Stdin)

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	err = setRateLimitOnRestConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return restConfig, nil
}

// GetE2ETestUserWorkspaceKubeConfig retrieves the normal openshift/k8s cluster.
func GetE2ETestUserWorkspaceKubeConfig() (*rest.Config, error) {
	return GetSystemKubeConfig()
}

// GetServiceProviderWorkspaceKubeConfig Return the normal openshift/k8s cluster; For example, to see Argo CD Application CRs
func GetServiceProviderWorkspaceKubeConfig() (*rest.Config, error) {
	return GetSystemKubeConfig()
}

// GetKubeClientSet returns a Clientset for accesing K8s API resources.
// This API has the advantage over `GetKubeClient` in that it is strongly typed, but cannot be used for
// custom resources.
func GetKubeClientSet() (*kubernetes.Clientset, error) {
	config, err := GetSystemKubeConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

// GetKubeClient returns a controller-runtime Client for accessing K8s API resources.
// The client returned by this function will, by default, already be aware of all
// the necessary schemes for interacting with Argo CD/GitOps Service.
func GetKubeClient(config *rest.Config) (client.Client, error) {

	scheme := runtime.NewScheme()
	err := managedgitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = managedgitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = apps.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}
	err = rbacv1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = argocdoperator.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = routev1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = appv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = appstudiosharedv1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	err = admissionv1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	if err := apiexts.AddToScheme(scheme); err != nil {
		return nil, err
	}

	err = codereadytoolchainv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, err
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return k8sClient, nil

}

// reportRemainingArgoCDApplications outputs the current contents of all Argo CD Applications. This can be useful while debugging failing tests,
// to determine what the final state of an Argo CD Application was at the end of a test.
func ReportRemainingArgoCDApplications(k8sClient client.Client) error {
	argoCDApplicationList := appv1alpha1.ApplicationList{}
	if err := k8sClient.List(context.Background(), &argoCDApplicationList); err != nil {
		return err
	}

	GinkgoWriter.Println("Argo CD Applications present at end of test:")

	for idx, application := range argoCDApplicationList.Items {
		jsonStr, err := json.Marshal(application)
		if err != nil {
			return err
		}

		GinkgoWriter.Printf("- %d) %s\n", idx+1, (string)(jsonStr))
	}
	GinkgoWriter.Println()

	return nil
}

func EnsureCleanSlate() error {
	err := EnsureCleanSlateNonKCPVirtualWorkspace()
	return err
}

func GetE2ETestUserWorkspaceKubeClient() (client.Client, error) {
	config, err := GetE2ETestUserWorkspaceKubeConfig()
	if err != nil {
		return nil, err
	}

	k8sClient, err := GetKubeClient(config)
	if err != nil {
		return nil, err
	}

	return k8sClient, nil
}

// IsRunningInStonesoupEnvironment returns true if the tests are running in a Stonesoup environment, false otherwise
func IsRunningInStonesoupEnvironment() bool {
	// Stonesoup environment is bootstrapped by an AppOfApps Gitops Deployment.
	// Check if the AppOfApps exists.
	appOfApps := appv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "all-components-staging",
			Namespace: "openshift-gitops",
		},
	}

	config, err := GetE2ETestUserWorkspaceKubeConfig()
	Expect(err).ToNot(HaveOccurred())

	k8sClient, err := GetKubeClient(config)
	Expect(err).ToNot(HaveOccurred())

	err = k8s.Get(&appOfApps, k8sClient)
	if err != nil && apierr.IsNotFound(err) {
		return false
	}
	return true
}

// ExtractKubeConfigValues returns contents of k8s config from $KUBE_CONFIG, plus server api url (and error)
func ExtractKubeConfigValues() (string, string, error) {

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := loadingRules.Load()
	if err != nil {
		return "", "", err
	}

	context, ok := config.Contexts[config.CurrentContext]
	if !ok || context == nil {
		return "", "", fmt.Errorf("no context")
	}

	cluster, ok := config.Clusters[context.Cluster]
	if !ok || cluster == nil {
		return "", "", fmt.Errorf("no cluster")
	}

	var kubeConfigDefault string

	paths := loadingRules.Precedence
	{

		for _, path := range paths {

			GinkgoWriter.Println("Attempting to read kube config from", path)

			_, err = os.Stat(path)
			if err != nil {
				GinkgoWriter.Println("Unable to resolve path", path, err)
			} else {
				// Success
				kubeConfigDefault = path
				break
			}

		}

		if kubeConfigDefault == "" {
			return "", "", fmt.Errorf("unable to retrieve kube config path")
		}
	}

	// #nosec G304 no security concerns about reading a file from a variable, in a test
	kubeConfigContents, err := os.ReadFile(kubeConfigDefault)
	if err != nil {
		return "", "", err
	}

	return string(kubeConfigContents), cluster.Server, nil
}

// SkipIfArgoCDOperandRequired will cause a test to be skipped if the ArgoCD
// CustomResourceDefinition is not installed (because OpenShift GitOps Operator is not installed.)
func SkipIfArgoCDOperandRequired() {

	k8sConfig, err := GetServiceProviderWorkspaceKubeConfig()
	Expect(err).To(BeNil())

	k8sClient, err := GetKubeClient(k8sConfig)
	Expect(err).To(BeNil())

	crdList := &apiexts.CustomResourceDefinitionList{}
	err = k8sClient.List(context.Background(), crdList)
	Expect(err).To(BeNil())

	match := false
	for _, crd := range crdList.Items {
		if crd.Spec.Names.Kind == "ArgoCD" {
			match = true
		}
	}

	if !match {
		Skip("Skipping as ArgoCD CustomResourceDefinition is not installed, which is required for test.")
	}

}
