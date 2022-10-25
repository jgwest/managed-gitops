# GitOps Service M6 Demo


## Setup the demo

1) Ensure you have [kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/binaries/), [kubectl](https://kubernetes.io/releases/download/), and Go, installed and on your path.

2) Clone this repo, and move into the `m6-demo` directory:
```
git clone https://github.com/redhat-appstudio/managed-gitops
cd managed-gitops/examples/m6-demo
```

3) Acquire a disposable OpenShift cluster from cluster-bot. (Send `launch 4.10` command to *cluster-bot* user on Red Hat Slack). 
    - You _could_ use your own OpenShift cluster, but I highly recommend a disposable cluster-bot cluster so as to prevent the demo script from interfering with your existing cluster. 
    - If using your own cluster, carefully consider the consequences 😅.
    - **Note**: Use an OpenShift V4.10 cluster. The `ServiceAccount` behaviour of Kubernetes v1.24 (Openshift v4.11) has changed, and we have not yet responded to it in the ManagedEnvironment code.

4) Wait for the cluster to orchestrate, then `oc login` from the CLI.

5) Next, log in to the OpenShift cluster console UI.
    - This sets the OpenShift login credentials in your browser, which we will use to log in to Argo CD in a couple steps.

6) Run `setup-on-openshift.sh` from `m6-demo` directory.
    - After a few minutes, verify that:
        - OpenShift GitOps (Argo CD) is successfully running in `gitops-service-argocd` namespace.
        - 'managed-gitops-postgres'  Docker container is running successfully on your local machine (do a `docker ps` and look for this container)
        - The GitOps Service controller processes start without error, and continue to run on your terminal.
            - Note: the `go vet`, `go get`, and `go: downloading`  steps may take a while to download/compile dependencies (this only occurs on first run, and only if you have never built the GitOps service before)
    - You may safely ignore these error messages which may be printed by the setup script:
	    - "`You are in 'detached HEAD' state.`"
	    - "`Error: No such container: (...)`"
	    - "`Error: No such network: (...)`"
        - "`namespaces "(...)" already exists`"

7) On the console, where you ran `setup-on-openshift.sh`, wait until you see 'Starting Controller' from both backend and cluster agent:
    - `INFO backend  | (...) INFO controller-runtime.manager.controller.gitopsdeploymentsyncrun	Starting Controller'`
    - `INFO cluster-agent | (...)	INFO	controller-runtime.manager.controller.application	Starting Controller`
    - Verify there are no error messages output by the GitOps service components (only _INFO_ and _DEBUG_ statments in the console output)

8) Optional: if you want to see the Argo CD Applications within the Argo CD UI
    - Output the Argo CD admin password:

```bash
echo `kubectl -n gitops-service-argocd get secret gitops-service-argocd-cluster -o jsonpath="{.data.admin\.password}" | base64 --decode`
```
    - Output the Argo CD Web UI URL: `kubectl get routes -n gitops-service-argocd`
    - Open the Web UI URL, and specify username `admin`, password as above.

## Run the demo

This demo will walk through two GitOps Service API Resources: 

The `GitOpsDeployment` custom resource (CR) describes an active continuous deployment from a GitOps repository, to a namespace (workspace):

```yaml
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeployment

metadata:
  name: gitops-depl
  namespace: jgw  # 'jgw' workspace

spec:

  # Application/component to deploy
  source:
    repoURL: https://github.com/redhat-appstudio/managed-gitops
    path: resources/test-data/sample-gitops-repository/environments/overlays/dev

  destination:
    environment: my-remote-cluster
    namespace: my-namepsace
  # or
  # destination: {}  # destination is user workspace if empty
  

  # Only 'automated' type is currently supported: changes to the GitOps repo immediately take effect (as soon as Argo CD detects them).
  type: automated
```

The `GitOpsDeploymentManagedEnvironment` CR describes a remote cluster (or KCP workspace) which the GitOps Service (via Argo CD) will deploy to. 

```yaml
# Describe the API URL and credentials for the target cluster 
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeploymentManagedEnvironment
metadata:
  name: my-managed-environment
  namespace: jane
spec:
  apiURL: "https://api.my-cluster.dev.rhcloud.com:6443"  
  credentialsSecret: "my-managed-environment-secret"
---
# Kubeconfig credentials for the target cluster (as a Secret)
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


### `GitOpsDeployment` resource

First, create a `GitOpsDeployment` resource in the *jane* namespace (workspace), for our first tenant, 'jane':
```bash
kubectl apply -f jane-deployment.yaml
```

Observe that:
- Open the Argo CD Web UI (or do `kubectl get applications.argoproj.io -A -o yaml`), and observe the corresponding Argo CD Application CR has been created, and points to jane namespace
- Within the 'jane' namespace, you can see all the deployed resources (Deployment, Service, ConfigMap, Route)
- Various DEBUG/INFO log messages are output to the console by backend and cluster-agent, corresponding to behind-the-scenes actions.

Next, create a `GitOpsDeployment` resource in the jgw namespace (workspace), for our second tenant 'jgw':
```bash
kubectl apply -f jgw-deployment.yaml
```

Same observations as above, but notice that the deployment target is now the 'jgw' namespace. Each user has their own deployments, specified in their own namespace, deploying resources to their own namespace, backed by a shared Argo CD instance.

Next, you can change fields in the `GitOpsDeployment` CR, such as the path within the target GitOps repository:
````bash
# Edit jgw-deployment.yaml
vi jgw-deployment.yaml

# Next, change the path field from 'dev' to 'staging':
#
# spec:
#   source:
#     path: resources/test-data/sample-gitops-repository/environments/overlays/dev
# to
#
# spec:
#   source:
#     path: resources/test-data/sample-gitops-repository/environments/overlays/staging

# Apply the change
kubectl apply -f jgw-deployment.yaml
````


Observe that:
- The GitOps service quickly detects the change, and updates the Argo CD Application
- In the Argo CD UI, the corresponding Argo CD application and its resources are updated.
- The deployed resources in 'jgw' namespace are updated.

You can likewise delete the `GitOpsDeployment` CR...

```
kubectl delete -f jgw-deployment.yaml
```

... and observe that:
- The Argo CD `Application` CR is likewise cleaned up, along with all the deployed resources.

At any step in the process, you can also view the database internals:
- Run `psql.sh` from the [managed-gitops](https://github.com/redhat-appstudio/managed-gitops) repo to automatically open *psql* utility against the PostgreSQL database.
- Some example queries:
    - See Argo CD Application entries: `select * from application;`
        - _(Don't forget the semi-colon at the end of these statements!)_
    - See completed operations: `select * from operation;` 
    - See Argo CD instance info: `select * from gitopsengineinstance;`
    - See deployment targets: `select * from managedenvironment;`
    - See relationship between GitOpsDeploymentSync CRs and their corresponding Application DB entry: `select * from deploymenttoapplicationmapping;`
    - (etc)
- As a rule: all changes made to Argo CD are first made to the database, with the cluster-agent responsible for reconciling database state -> Argo CD state.

### `GitOpsDeploymentManagedEnvironment` resource

Create a `Secret` in your current namespace, containing the contents of your current kubeconfig (for simplicity sake, we use `~/.kube/config`)
````bash
kubectl create secret generic my-managed-environment-secret \
       --from-file=kubeconfig=$HOME/.kube/config \
       --type="managed-gitops.redhat.com/managed-environment" \
       -n jane
````

**Note**: This command will pick up ALL the cluster contexts that are specified in your kubeconfig file. Ensure you are specifying the `apiURL` of the correct context in the `GitOpsDeploymentManagedEnvironment` below.
- If you wish to simplify your configuration file, you can `rm $HOME/.kube/config`, and then `oc login` to the cluster agent. This ensures that only the appropriate cluster context is contained in your `.kube/config` file.
    - Warning: This will erase any other context and credentials you have defined in this file. You may wish to make a backup first, e.g. `cp $HOME/.kube/config $HOME/.kube/config.bak`


Create a `GitOpsDeploymentManagedEnvironment` pointing to the `Secret` resource:
```bash
kubectl apply -f my-managed-environment.yaml
```
or apply this:
```yaml
apiVersion: managed-gitops.redhat.com/v1alpha1
kind: GitOpsDeploymentManagedEnvironment
metadata:
  name: my-managed-environment
  namespace: jane
spec:
  apiURL: "https://(API url of a valid k8s cluster defined within your kube config)"
# example:
# apiURL: "https://api.ci-ln-vtdzzjb-72292.origin-ci-int-gce.dev.rhcloud.com:6443"  
  credentialsSecret: "my-managed-environment-secret"
```

**NOTE**: In either case, make sure you update the `apiURL` field to point to a valid API url from the kube config.

Next, create a `GitOpsDeployment` resource that references the `GitOpsDeploymentManagedEnvironment` resource:
```bash
kubectl apply -f gitops-deployment-managed-environment.yaml
```
or apply this:
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

  destination:
    environment: my-managed-environment
    namespace: jgw # NOTE: namespace must exist on remote cluster

  # Only 'automated' type is currently supported: changes to the GitOps repo immediately take effect (as soon as Argo CD detects them).
  type: automated
```
*NOTE:* In either case, make sure the `namespace` field points to a valid namespace on the cluster.

Once the `GitOpsDeployment` resource is created, the GitOps Service (via Argo CD) will deploy the contents of the `gitops-repository-template` repository to the target `my-managed-environment` cluster.

 Behind the scenes, it will have created an [Argo CD cluster secret](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#clusters) corresponding to the `GitOpsDeploymentManagedEnvironment` resource, and an Argo CD `Application` resource corresponding to the `GitOpsDeployment` (and pointing to that `GitOpsDeploymentManagedEnvironment`).

 You can view these Argo CD resources using `kubectl get applications.argoproj.io -A`, and `kubectl get secrets -n gitops-service-argocd | grep managed-env`.

## Clean up the demo

To clean up the demo, just remove the local containers from your workspace:
```bash
docker rm -f managed-gitops-pgadmin managed-gitops-postgres
```

## Questions and troubleshooting

Something went wrong? Let us know on [#forum-gitops-service](https://coreos.slack.com/archives/C02C3SE8QS2) on Red Hat Slack!
