
# GitOps Service API

This document describes the APIs supported by the GitOps Service. The examples provided here *might* differ slightly from their current form (but let us know if they do, so we can update this doc). 

The K8s resource structs defined in the `managed-gitops` repository, and the corresponding CustomResourceDefinitions (CRDs), are the canonical representation of the current state of this API.

## Core GitOps Service API

### GitOpsDeployment

The `GitOpsDeployment` custom resource (CR) describes an active continuous deployment from a GitOps repository, to a namespace (workspace).

```yaml
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeployment

metadata:
  name: managed-environment-gitops-depl
  namespace: jane 

spec:

  # Application/component to deploy
  source:
    repoURL: https://github.com/redhat-appstudio/managed-gitops
    path: resources/test-data/sample-gitops-repository/environments/overlays/dev

  destination:  # destination is user workspace if empty
    environment: my-managed-environment
    namespace: jane # NOTE: namespace must exist on remote cluster

  # Only 'automated' type is currently supported: changes to the GitOps repo immediately take effect (as soon as Argo CD detects them).
  type: automated
  # type: 'manual' # Will only deploys when a `GitOpsDeploymentSyncRun` resource is created.
```

This resource is roughly translated into an [Argo CD Application Resource](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#applications).

### GitOpsDeploymentManagedEnvironment 

The `GitOpsDeploymentManagedEnvironment` CR describes a remote cluster (or KCP workspace) which the GitOps Service will deploy to (via Argo CD). This resource references a second `Secret` resource, of type `managed-gitops.redhat.com/managed-environment`, that contains the cluster credentials.

```yaml
# Describes the API URL and credentials for the target cluster 
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeploymentManagedEnvironment
metadata:
  name: my-managed-environment
  namespace: jane
spec:
  apiURL: "https://api.my-cluster.dev.rhcloud.com:6443"  
  credentialsSecret: "my-managed-environment-secret"

---
# The GitOpsDeploymentManagedEnvironment references a Secret, containing the connection information
# - Kubeconfig credentials for the target cluster (as a Secret)
apiVersion: v1
kind: Secret
metadata:
  name: my-managed-environment-secret
  namespace: jane
type: managed-gitops.redhat.com/managed-environment
data:
  # Note: This would be base 64 when stored on the cluster
  kubeconfig: |
    apiVersion: v1
    clusters:
    - cluster:
        insecure-skip-tls-verify: true
        server: https://api.my-cluster.dev.rhcloud.com:6443
      name: api-my-cluster-dev-rhcloud-com:6443
    contexts:
    - context:
        cluster: api-my-cluster-dev-rhcloud-com:6443
        namespace: default
        user: kube:admin/api-my-cluster-dev-rhcloud-com:6443
      name: default/api-my-cluster-dev-rhcloud-com:6443/kube:admin
    current-context: default/api-my-cluster-dev-rhcloud-com:6443/kube:admin
    kind: Config
    preferences: {}
    users:
    - name: kube:admin/api-my-cluster-dev-rhcloud-com:6443
      user:
        token: sha256~ABCdEF1gHiJKlMnoP-Q19qrTuv1_W9X2YZABCDefGH4
```

These resources roughly translate into an [Argo CD Cluster `Secret`](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#clusters).

### GitOpsDeploymentRepositoryCredentials (*in-progress*)

The `GitOpsDeploymentRepositoryCredentials` resource is used to provide Git credentials for a private Git repository.

```yaml
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeploymentRepositoryCredentials
metadata:
  Name: private-repo-creds
spec:
  url: https://github.com/jgwest/private-app
  secret: private-repo-creds-secret

---
apiVersion: v1
kind: Secret
metadata:
  name: private-repo-creds-secret
stringData:
  url: https://github.com/jgwest/my-private-app
  # and:
  username: jgwest
  password: (my password)
  # or:
  sshPrivateKey: (...)
```

These resources roughly translate into an [Argo CD Repository Credentials `Secret`](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#repository-credentials)



### GitOpsDeploymentSyncRun (*in-progress*)

The `GitOpsDeploymentSyncRun` resource is use to manually trigger a synchronize operation (this can be roughly thought of as the equivalent to a 'deployment' operation). 

Upon creation of this resource, the GitOps Service will trigger a deployment of the Kubernetes resources describe in the GitOps repository, to the target environment (cluster/workspace).

The `GitOpsDeploymentSyncRun` resource is not required when the `GitOpsDeployment` is of type `automated`. When automated, any changes to the GitOps repository will automatically be deployed to the target environment.

```yaml
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeploymentSyncRun
spec:
  gitopsDeploymentName: jgwest-app # ref to GitOpsDeployment CR
  revisionId: (...)  # GitOps repo GitHub commit SHA

status: 
  health: Healthy # (enum from Argo CD Application health field: Healthy / Progressing / Degraded / Suspended / Missing / Unknown)
  syncStatus: Synced # (enum from Argo CD status: Synced / OutOfSync)
  conditions:
    - lastTransitionTime: "2022-10-04T02:19:14Z"
      message: "Successfully completed synchronize operation."
      reason: Succeeded
      status: true
      type: Succeeded
    # (Other useful conditions)
```

Behind the scenes, this will trigger a manual sync of the corresponding Argo CD `Application`. The manual sync will cause Argo CD to ensure that the K8s resources described in the GitOps repository are consistent with what is on the target cluster.

This resource has no corresponding Argo CD CR equivalent: with Argo CD, a manual sync operation can only be triggered via the Web/GRPC API (for example, via the argocd CLI). In this case, the GitOps Service uses the Web API.

## AppStudio GitOps Service API

### Environment (*in-progress*)

```yaml
apiVersion: appstudio.redhat.com/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  type: poc
  displayName: “Staging for Team A”
  deploymentStrategy: AppStudioAutomated
  parentEnvironment: staging
  tags:
    - staging
  configuration:
    env:
      - name: My_STG_ENV
        value: "100"
  unstableConfigurationFields:
    kubernetesCredentials:
      apiURL: https://api.ci-ln-a1b2c3d4e5-76543.origin-ci-int-gce.dev.rhcloud.com:6443
      targetNamespace: my-namespace
      # See GitOpsDeploymentManagedEnvironment above for secret above:
      clusterCredentialsSecret: secret-containing-cluster-credential

```


### Snapshot  (*in-progress*)

```yaml
apiVersion: appstudio.redhat.com/v1alpha1
kind: Snapshot
metadata:
  name: my-snapshot
spec:
  application: "new-demo-app"
  displayName: "My known good staging snapshot"
  displayDescription: "The best we've had so far!"
  components:
    - name: component-a
      containerImage: quay.io/jgwest-redhat/sample-workload:latest
```

### SnapshotEnvironmentBinding  (*in-progress*)

```yaml
apiVersion: appstudio.redhat.com/v1alpha1
kind: SnapshotEnvironmentBinding
metadata:
  name: appa-staging-binding
  labels:
    appstudio.application: new-demo-app
    appstudio.environment: staging
spec:
  application: new-demo-app
  environment: staging
  snapshot: my-snapshot
  components:
    - name: component-a
      configuration:
        env:
          - name: My_STG_ENV
            value: "200"
        replicas: 3
status:
  components:
    - name: component-a
      gitopsRepository:
        url: "https://github.com/redhat-appstudio/managed-gitops"
        branch: main
        path: components/componentA/overlays/staging
        generatedResources:
          - abc.yaml
``` 

### PromotionRun  (*in-progress*)

```yaml
apiVersion: appstudio.redhat.com/v1alpha1
kind: PromotionRun  # better name suggestions welcome
metadata:
  name: appA-manual-promotion
  # labels for application/environment

spec:
  snapshot: my-snapshot # reference to snapshot to promote between environments.
  application: appA  # application to target

  # Fields specific to manual promotion.
  # Only one field should be defined: either 'manualPromotion' or 'automatedPromotion', but not both.
  manualPromotion:
    targetEnvironment: staging # promote TO this environment
    
  # Fields specific to automated promotion
  # Only one field should be defined: either 'manualPromotion' or 'automatedPromotion', but not both.  
  automatedPromotion:
    initialEnvironment: staging # start iterating through the digraph, beginning with the value specified in 'initialEnvironment'

status:

  # Whether or not the overall promotion (either manual or automated is complete)
  state: Active # Waiting (not yet scheduled) / Active (in progress) / Completed (either successfully/unsuccessfully)

  # on completion:
  completionResult: Success / Failure

  # For an automated promotion, there will be multiple steps (one for each promotion within the promotion process, beginning with targetEnvironment)
  # For a manual promotion, there will only be the first step (the promotion to the targetEnvironment)
  environmentStatus:
    - step: 1
      environmentName: poc
      status: success
    - step: 2
      environmentName: dev
      status: success
    - step: 3
      environmentName: stage
      status: in-progress
    - (...)

  # For an automated promotion, there can be multiple active bindings at a time (one for each env at a particular tree depth)
  # For a manual promotion, there will be only one.
  activeBindings:
    - appA-staging1
    - appA-staging2
    - appA-staging3
```
