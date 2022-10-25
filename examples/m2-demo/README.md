# GitOps Service M2 Demo

**Note: This demo hardcodes specific 'known good' commits of the managed-gitops repo and infra-deployments repo, for demo stability.** If you wish to test on the latest code, remove the `git checkout` statements from `setup-on-openshift.sh`.

## Setup the demo

1) Ensure you have [kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/binaries/), [kubectl](https://kubernetes.io/releases/download/), and Go, installed and on your path.

2) Clone this repo and move into the `m2-demo` directory:
```
git clone https://github.com/redhat-appstudio/managed-gitops
cd managed-gitops/examples/m2-demo
```

3) Acquire a disposable OpenShift cluster from cluster-bot. (Send `launch` command to *cluster-bot* user on Red Hat Slack). 
    - You _could_ use your own OpenShift cluster, but I highly recommend a disposable cluster-bot cluster so as to prevent the demo script from interfering with your existing cluster. 
    - If using your own cluster, carefully consider the consequences 😅.

4) Wait for the cluster to orchestate, then `oc login` from the CLI.

5) Next, log in to the OpenShift cluster console UI.
    - This sets the OpenShift login credentials in your browser, which we will use to log in to Argo CD in a couple steps.

6) Run `setup-on-openshift.sh` from `m2-demo` directory.
    - After a few minutes, verify that:
        - OpenShift GitOps (Argo CD) is successfully running in `openshift-gitops` namespace.
        - 'managed-gitops-postgres'  Docker container is running successfully on your local machine (do a `docker ps` and look for this container)
        - The GitOps Service controller processes start without error, and continue to run on your terminal.
            - Note: the `go vet`, `go get`, and `go: downloading`  steps may take a while to download/compile dependencies (this only occurs on first run, and only if you have never built the GitOps service before)
    - You may safely ignore these error messages which may be printed by the setup script:
	    - "`You are in 'detached HEAD' state.`"
	    - "`Error: No such container: (...)`"
	    - "`Error: No such network: (...)`"
        - "`namespaces "(...)" already exists`"


7) Log in to Argo CD Web UI
    - Get the URL for Argo CD by running: `kubectl get route -n openshift-gitops openshift-gitops-server`
    - Open the URL in your browser, then click *Login with OpenShift*. The login should use your OpenShift console credentials from step 2.
    - Verify that:
        - You see the Argo CD application lists (with no applications listed)

8) On the console, where you ran `setup-on-openshift.sh`, wait until you see 'Starting Controller' from both backend and cluster agent:
    - `INFO backend  | (...) INFO controller-runtime.manager.controller.gitopsdeploymentsyncrun	Starting Controller'`
    - `INFO cluster-agent | (...)	INFO	controller-runtime.manager.controller.application	Starting Controller`
    - Verify there are no error messages output by the GitOps service components (only _INFO_ and _DEBUG_ statments in the console output)


## Run the demo

This demo is fully built-around the `GitOpsDeployment` custom resource (CR), which describes an active continous deployment from a GitOps repository, to a namespace (workspace):
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

  # destination: {}  # destination is user workspace if empty

  # Only 'automated' type is currently supported: changes to the GitOps repo immediately take effect (as soon as Argo CD detects them).
  type: automated
```



Create a `GitOpsDeployment` resource in the *jane* namespace (workspace), for our first tenant, 'jane':
```bash
kubectl apply -f jane-deployment.yaml
```

Observe that:
- Open the Argo CD Web UI, and observe the corresponding Argo CD Application CR has been created, and points to jane namespace
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
- The Argo CD `Application` CR is likewise cleaned up, along with deployed resources.

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

## Clean up the demo

To clean up the demo, just remove the local containers from your workspace:
```bash
docker rm -f managed-gitops-pgadmin managed-gitops-postgres
```

## Questions and troubleshooting

Something went wrong? Let us know on [#forum-gitops-service](https://coreos.slack.com/archives/C02C3SE8QS2) on Red Hat Slack!
