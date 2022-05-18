package utils

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	argocdoperator "github.com/argoproj-labs/argocd-operator/api/v1alpha1"
)

func CreateNamespaceScopedArgoCD(ctx context.Context, name string, namespace string, k8sClient client.Client) error {

	argoCDOperand := argocdoperator.ArgoCD{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: argocdoperator.ArgoCDSpec{
			// TODO: Use the values from manifests/staging-cluster-resources/argo-cd.yaml in this Go struct.
			// for example:
			Controller: argocdoperator.ArgoCDApplicationControllerSpec{
				Processors: argocdoperator.ArgoCDApplicationControllerProcessorsSpec{},
				Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						// (...)
					},
					Requests: corev1.ResourceList{
						// (...)
					},
				},
				Sharding: argocdoperator.ArgoCDApplicationControllerShardSpec{},
			},
		},
	}

	// This will create the ArgoCD resource in the target namespace. The OpenShift GitOps operator (which should already be installed)
	// will then install Argo CD in the target namespace.
	err := k8sClient.Create(ctx, &argoCDOperand)
	if err != nil {
		return err
	}

	// TODO: Wait for Argo CD to be installed by gitops operator. Use wait.Poll for ths

	// TODO: setup serviceaccount/secret/clusterole/clusterolebinding on the cluster for remote deploy, in the same
	// way that the shell script is doing: https://gist.github.com/jgwest/64cdc63a978324958ab8ac91aea74700

	return nil
}
