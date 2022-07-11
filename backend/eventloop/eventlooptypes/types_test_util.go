package eventlooptypes

import (
	"testing"

	appstudiosharedv1 "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
	operation "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	dbutil "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend/apis/managed-gitops/v1alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func GenericTestSetupForTestingT(t *testing.T) (*runtime.Scheme, *corev1.Namespace, *corev1.Namespace, *corev1.Namespace) {

	scheme, argocdNamespace, kubesystemNamespace, workspace, err := GenericTestSetup()
	assert.NoError(t, err)

	return scheme, argocdNamespace, kubesystemNamespace, workspace

}

func GenericTestSetup() (*runtime.Scheme, *corev1.Namespace, *corev1.Namespace, *corev1.Namespace, error) {
	scheme := runtime.NewScheme()

	opts := zap.Options{
		Development: true,
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	err := managedgitopsv1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	err = operation.AddToScheme(scheme)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	err = appstudiosharedv1.AddToScheme(scheme)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	err = rbacv1.AddToScheme(scheme)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	argocdNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbutil.GetGitOpsEngineSingleInstanceNamespace(),
			UID:       uuid.NewUUID(),
			Namespace: dbutil.GetGitOpsEngineSingleInstanceNamespace(),
		},
		Spec: corev1.NamespaceSpec{},
	}

	kubesystemNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-system",
			UID:       uuid.NewUUID(),
			Namespace: "kube-system",
		},
		Spec: corev1.NamespaceSpec{},
	}

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-user",
			UID:       uuid.NewUUID(),
			Namespace: "my-user",
		},
		Spec: corev1.NamespaceSpec{},
	}

	return scheme, argocdNamespace, kubesystemNamespace, namespace, nil

}
