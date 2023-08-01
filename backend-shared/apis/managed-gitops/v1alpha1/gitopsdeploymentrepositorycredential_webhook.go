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
	"fmt"
	"net/url"

	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const error_invalid_repository = "repository must begin with ssh:// or https://"

// log is for logging in this package.
var gitopsdeploymentrepositorycredentiallog = logf.Log.WithName(logutil.LogLogger_managed_gitops)

func (r *GitOpsDeploymentRepositoryCredential) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-managed-gitops-redhat-com-v1alpha1-gitopsdeploymentrepositorycredential,mutating=true,failurePolicy=fail,sideEffects=None,groups=managed-gitops.redhat.com,resources=gitopsdeploymentrepositorycredentials,verbs=create;update,versions=v1alpha1,name=mgitopsdeploymentrepositorycredential.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &GitOpsDeploymentRepositoryCredential{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *GitOpsDeploymentRepositoryCredential) Default() {
	gitopsdeploymentrepositorycredentiallog.Info("default", "name", r.Name)

}

//+kubebuilder:webhook:path=/validate-managed-gitops-redhat-com-v1alpha1-gitopsdeploymentrepositorycredential,mutating=false,failurePolicy=fail,sideEffects=None,groups=managed-gitops.redhat.com,resources=gitopsdeploymentrepositorycredentials,verbs=create;update,versions=v1alpha1,name=vgitopsdeploymentrepositorycredential.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &GitOpsDeploymentRepositoryCredential{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentRepositoryCredential) ValidateCreate() (admission.Warnings, error) {
	gitopsdeploymentrepositorycredentiallog.Info("validate create", "name", r.Name)

	if err := r.ValidateGitOpsDeploymentRepoCred(); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentRepositoryCredential) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	gitopsdeploymentrepositorycredentiallog.Info("validate update", "name", r.Name)

	if err := r.ValidateGitOpsDeploymentRepoCred(); err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *GitOpsDeploymentRepositoryCredential) ValidateDelete() (admission.Warnings, error) {
	gitopsdeploymentrepositorycredentiallog.Info("validate delete", "name", r.Name)

	return nil, nil
}
func (r *GitOpsDeploymentRepositoryCredential) ValidateGitOpsDeploymentRepoCred() error {

	if r.Spec.Repository != "" {
		apiURL, err := url.ParseRequestURI(r.Spec.Repository)
		if err != nil {
			return fmt.Errorf(err.Error())
		}

		if !(apiURL.Scheme == "https" || apiURL.Scheme == "ssh") {
			return fmt.Errorf(error_invalid_repository)
		}
	}

	return nil
}
