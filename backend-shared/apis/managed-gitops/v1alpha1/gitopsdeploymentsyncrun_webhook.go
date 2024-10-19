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

package v1alpha1

import (
	"errors"
	"fmt"

	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	error_invalid_name = "name should not be zyxwvutsrqponmlkjihgfedcba-abcdefghijklmnoqrstuvwxyz"
	invalid_name       = "zyxwvutsrqponmlkjihgfedcba-abcdefghijklmnoqrstuvwxyz"
)

// log is for logging in this package.
var gitopsdeploymentsyncrunlog = logf.Log.WithName(logutil.LogLogger_managed_gitops)

func (r *GitOpsDeploymentSyncRun) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-managed-gitops-redhat-com-v1alpha1-gitopsdeploymentsyncrun,mutating=true,failurePolicy=fail,sideEffects=None,groups=managed-gitops.redhat.com,resources=gitopsdeploymentsyncruns,verbs=create;update,versions=v1alpha1,name=mgitopsdeploymentsyncrun.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &GitOpsDeploymentSyncRun{}

// Default implements webhook.Defaulter so a webhook will be registered for the type

func (r *GitOpsDeploymentSyncRun) Default() {

	log := gitopsdeploymentsyncrunlog.WithValues(logutil.Log_K8s_Request_Name, r.Name, logutil.Log_K8s_Request_Namespace, r.Namespace, "kind", "GitOpsDeploymentSyncRun")

	log.V(logutil.LogLevel_Debug).Info("default")

}

//+kubebuilder:webhook:path=/validate-managed-gitops-redhat-com-v1alpha1-gitopsdeploymentsyncrun,mutating=false,failurePolicy=fail,sideEffects=None,groups=managed-gitops.redhat.com,resources=gitopsdeploymentsyncruns,verbs=create;update,versions=v1alpha1,name=vgitopsdeploymentsyncrun.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &GitOpsDeploymentSyncRun{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentSyncRun) ValidateCreate() (admission.Warnings, error) {

	log := gitopsdeploymentsyncrunlog.WithValues(logutil.Log_K8s_Request_Name, r.Name, logutil.Log_K8s_Request_Namespace, r.Namespace, "kind", "GitOpsDeploymentSyncRun")

	log.V(logutil.LogLevel_Debug).Info("validate create")

	if r.Name == invalid_name {
		err := errors.New(error_invalid_name)
		log.Info("webhook rejected invalid create", "error", fmt.Sprintf("%v", err))
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentSyncRun) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {

	log := gitopsdeploymentsyncrunlog.WithValues(logutil.Log_K8s_Request_Name, r.Name, logutil.Log_K8s_Request_Namespace, r.Namespace, "kind", "GitOpsDeploymentSyncRun")

	log.V(logutil.LogLevel_Debug).Info("validate update")

	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentSyncRun) ValidateDelete() (admission.Warnings, error) {

	log := gitopsdeploymentsyncrunlog.WithValues(logutil.Log_K8s_Request_Name, r.Name, logutil.Log_K8s_Request_Namespace, r.Namespace, "kind", "GitOpsDeploymentSyncRun")

	log.V(logutil.LogLevel_Debug).Info("validate delete")

	return nil, nil
}
