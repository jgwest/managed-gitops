/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package managedgitops

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/eventlooptypes"
	"github.com/redhat-appstudio/managed-gitops/backend/eventloop/preprocess_event_loop"
)

// GitOpsDeploymentSyncRunReconciler reconciles a GitOpsDeploymentSyncRun object
type GitOpsDeploymentSyncRunReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	PreprocessEventLoop *preprocess_event_loop.PreprocessEventLoop
}

//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentsyncruns,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentsyncruns/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=managed-gitops.redhat.com,resources=gitopsdeploymentsyncruns/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitOpsDeploymentSyncRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	ctx = sharedutil.AddKCPClusterToContext(ctx, "")
	_ = log.FromContext(ctx)

	rClient := sharedutil.IfEnabledSimulateUnreliableClient(r.Client)

	namespace := v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Namespace,
		},
	}
	if err := rClient.Get(ctx, client.ObjectKeyFromObject(&namespace), &namespace); err != nil {
		return ctrl.Result{}, err
	}

	r.PreprocessEventLoop.EventReceived(req, eventlooptypes.GitOpsDeploymentSyncRunTypeName, rClient, eventlooptypes.SyncRunModified, string(namespace.UID))

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitOpsDeploymentSyncRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&managedgitopsv1alpha1.GitOpsDeploymentSyncRun{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
