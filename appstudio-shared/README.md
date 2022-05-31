
# AppStudio-Shared

The APIs defined in this module is based on the [Environment API discussions](https://docs.google.com/document/d/1-_rWLgALd5pdSlqNNcQ5FSrD00fZb0l_exU-_FiL68o/) of April/May 2022.

## Warning for API Consumers

⚠️ Until these APIs are stabilized, they are subject to change with limited notice. Best efforts will be made to inform API consumers of how to migrate when the changes are breaking. ⚠️

### Expected upcoming changes, as of this writing (May 2022):
- Drop the `Application-` prefix for prefixed resources
- Move away from using the `Snapshot` noun (replacement TBD)
- Consider moving this module out of the GitOps Service monorepo and into its own Git repository

## Consuming the API

### Install CRDs to the cluster

To install the CRDs to a cluster:
```bash
kubectl apply -f https://raw.githubusercontent.com/redhat-appstudio/managed-gitops/main/appstudio-shared/manifests/appstudio-shared-customresourcedefinitions.yaml
```

### Generate controllers for the APIs in your operator

To generate a controller for one or more of these APIs, run these commands in your project:
```bash
# Tested on operator-sdk v1.17 - the commands may require minor adjustment if you are using a different version:

operator-sdk create api --group appstudio --version v1alpha1 --kind ApplicationSnapshot --controller
operator-sdk create api --group appstudio --version v1alpha1 --kind ApplicationPromotionRun --controller
operator-sdk create api --group appstudio --version v1alpha1 --kind ApplicationSnapshotEnvironmentBinding --controller
operator-sdk create api --group appstudio --version v1alpha1 --kind Environment --controller

# In all cases, answer `N` to the `Create Resource [y/n]` prompt from operator-sdk CLI.
# These commands presume you have `domain: redhat.com` defined in your PROJECT
```

#### Add the dependency to your go.mod

Run the following command:
```bash
go get github.com/redhat-appstudio/managed-gitops/appstudio-shared
```


#### Update SetupWithManager within the controller

After generating each controller, ensure you update the `SetupWithManager` function within the controller to reference the resource. See examples below for details.


#### Remember to add the scheme to your operator's `main.go`:
```go
import (
    // (...)
    appstudioshared "github.com/redhat-appstudio/managed-gitops/appstudio-shared/apis/appstudio.redhat.com/v1alpha1"
)

func init() {
    // (...)
    utilruntime.Must(appstudioshared.AddToScheme(scheme))
}
```


#### Examples

For [an example of generated controllers](https://github.com/redhat-appstudio/managed-gitops/tree/main/appstudio-controller/controllers/appstudio.redhat.com), see the controllers that were generated for the `appstudio-controller` component of GitOps Service.




## Resources for appstudio-shared developers

Bootstrapped with operator-sdk v1.17.

These are the commands that were used to bootstrap the resources. This should be useful when we later upgrade (re-bootstrap) onto a newer version of the operator-sdk.

```yaml
go mod init github.com/redhat-appstudio/managed-gitops/appstudio-shared

operator-sdk-v1.17 init --domain redhat.com

# Next, edit the PROJECT file and add:
# multigroup: true
# to an empty line.

operator-sdk-v1.17 create api --group appstudio.redhat.com --version v1alpha1 --kind (resource)  --resource # --controller
```

