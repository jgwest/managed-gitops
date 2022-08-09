/*
Copyright 2021, 2022

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

package argoprojio

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierr "k8s.io/apimachinery/pkg/api/errors"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	cache "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	argosharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/argocd"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util/fauxargocd"
	"github.com/redhat-appstudio/managed-gitops/cluster-agent/controllers"
	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	TaskRetryLoop *sharedutil.TaskRetryLoop
	Cache         *cache.ApplicationInfoCache
	DB            db.DatabaseQueries
}

// storeInComparedToFieldInApplicationState will read 'comparedTo' field of an Argo CD Application, and write the
// correct value into 'applicationState'.
func storeInComparedToFieldInApplicationState(argoCDAppParam appv1.Application, applicationState db.ApplicationState) error {

	comparedToParam := argoCDAppParam.Status.Sync.ComparedTo

	// 1) Convert into a non-Argo CD version of this data, so that we can easily store it in the database as JSON
	comparedTo := fauxargocd.FauxComparedTo{
		Source: fauxargocd.ApplicationSource{
			RepoURL:        comparedToParam.Source.RepoURL,
			Path:           comparedToParam.Source.Path,
			TargetRevision: comparedToParam.Source.TargetRevision,
		},
		Destination: fauxargocd.ApplicationDestination{
			Namespace: comparedToParam.Destination.Namespace,
			Name:      comparedToParam.Destination.Name,
		},
	}

	// 2) Read the Argo CD Cluster Secret name (which points to an Argo CD Cluster Secret in the Argo CD namespace) from the
	// Argo CD Application, and extract the managed environment ID from it.
	managedEnvID, isLocal, err := argosharedutil.ConvertArgoCDClusterSecretNameToManagedIdDatabaseRowId(comparedTo.Destination.Name)
	if err != nil {
		return fmt.Errorf("unable to convert secret name to managed env: %v", err)
	}

	// 3) If the Argo CD cluster secret name is a real cluster secret that exists in the Argo CD namespace, then use
	// the managed environment database ID that we extracted, and store it in comparedTo.
	if !isLocal {
		if managedEnvID == "" {
			return fmt.Errorf("managed environment id was empty for '%s'", argoCDAppParam.Name)
		}

		// Rather than storing the name of the Argo CD cluster secret in the .Destination.Name field, we instead store
		// the ID of the managed environment in the database.
		comparedTo.Destination.Name = managedEnvID
	}

	// 4) Convert the fauxArgoCD object into JSON
	comparedToBytes, err := json.Marshal(comparedTo)
	if err != nil {
		return fmt.Errorf("SEVERE: unable to convert comparedTo to JSON")
	}

	// 5) Finally, store the JSON in the database.
	applicationState.ReconciledState = (string)(comparedToBytes)

	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := log.FromContext(ctx).WithValues("name", req.Name, "namespace", req.Namespace)

	defer log.V(sharedutil.LogLevel_Debug).Info("Application Reconcile() complete.")

	// TODO: GITOPSRVCE-68 - PERF - this is single-threaded only

	// 1) Retrieve the Application CR using the request vals
	app := appv1.Application{}
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierr.IsNotFound(err) {
			log.Info("Application deleted '" + req.NamespacedName.String() + "'")
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Unexpected error on retrieving Application '"+req.NamespacedName.String()+"'")
			return ctrl.Result{}, err
		}
	}

	// 2) Retrieve the Application DB entry, using the 'databaseID' field of the Application.
	// - This is a field we add ourselves to every Application we create, which references the
	//   corresponding an Application table primary key.
	applicationDB := &db.Application{}
	if databaseID, exists := app.Labels[controllers.ArgoCDApplicationDatabaseIDLabel]; !exists {
		log.V(sharedutil.LogLevel_Warn).Info("Application CR was missing 'databaseID' label")
		return ctrl.Result{}, nil
	} else {
		applicationDB.Application_id = databaseID
	}
	if _, _, err := r.Cache.GetApplicationById(ctx, applicationDB.Application_id); err != nil {
		if db.IsResultNotFoundError(err) {

			log.V(sharedutil.LogLevel_Warn).Info("Application CR '" + req.NamespacedName.String() +
				"' missing corresponding database entry: " + applicationDB.Application_id)

			adt := applicationDeleteTask{
				applicationCR: app,
				client:        r.Client,
				log:           log,
			}

			// Add the Application to the task loop, so that it can be deleted.
			r.TaskRetryLoop.AddTaskIfNotPresent(app.Namespace+"/"+app.Name, &adt,
				sharedutil.ExponentialBackoff{Factor: 2, Min: time.Millisecond * 200, Max: time.Second * 10, Jitter: true})

			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Unable to retrieve Application from database: "+applicationDB.Application_id)
			return ctrl.Result{}, err
		}
	}

	log = log.WithValues("applicationID", applicationDB.Application_id)

	// 3) Does there exist an ApplicationState for this Application, already?
	applicationState := &db.ApplicationState{
		Applicationstate_application_id: applicationDB.Application_id,
	}
	if _, _, errGet := r.Cache.GetApplicationStateById(ctx, applicationState.Applicationstate_application_id); errGet != nil {
		if db.IsResultNotFoundError(errGet) {

			// 3a) ApplicationState doesn't exist: so create it

			applicationState.Health = db.TruncateVarchar(string(app.Status.Health.Status), db.ApplicationStateHealthLength)
			applicationState.Message = db.TruncateVarchar(app.Status.Health.Message, db.ApplicationStateMessageLength)
			applicationState.Sync_Status = db.TruncateVarchar(string(app.Status.Sync.Status), db.ApplicationStateSyncStatusLength)
			applicationState.Revision = db.TruncateVarchar(app.Status.Sync.Revision, db.ApplicationStateRevisionLength)
			sanitizeHealthAndStatus(applicationState)

			// Get the list of resources created by deployment and convert it into a compressed YAML string.
			var err error
			applicationState.Resources, err = compressResourceData(app.Status.Resources)
			if err != nil {
				log.Error(err, "unable to compress resource data into byte array.")
				return ctrl.Result{}, err
			}

			if errCreate := r.Cache.CreateApplicationState(ctx, *applicationState); errCreate != nil {
				log.Error(errCreate, "unexpected error on writing new application state")
				return ctrl.Result{}, errCreate
			}
			// Successfully created ApplicationState
			return ctrl.Result{}, nil
		} else {
			log.Error(errGet, "Unable to retrieve ApplicationState from database: "+applicationDB.Application_id)
			return ctrl.Result{}, errGet
		}
	}

	// 4) ApplicationState already exists, so just update it.

	applicationState.Health = db.TruncateVarchar(string(app.Status.Health.Status), db.ApplicationStateHealthLength)
	applicationState.Message = db.TruncateVarchar(app.Status.Health.Message, db.ApplicationStateMessageLength)
	applicationState.Sync_Status = db.TruncateVarchar(string(app.Status.Sync.Status), db.ApplicationStateSyncStatusLength)
	applicationState.Revision = db.TruncateVarchar(app.Status.Sync.Revision, db.ApplicationStateRevisionLength)
	sanitizeHealthAndStatus(applicationState)

	// Get the list of resources created by deployment and convert it into a compressed YAML string.
	var err error
	applicationState.Resources, err = compressResourceData(app.Status.Resources)
	if err != nil {
		log.Error(err, "unable to compress resource data into byte array.")
		return ctrl.Result{}, err
	}

	if err := r.Cache.UpdateApplicationState(ctx, *applicationState); err != nil {
		log.Error(err, "unexpected error on updating existing application state")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil

}

func sanitizeHealthAndStatus(applicationState *db.ApplicationState) {

	if applicationState.Health == "" {
		applicationState.Health = "Unknown"
	}

	if applicationState.Sync_Status == "" {
		applicationState.Sync_Status = "Unknown"
	}

}

type applicationDeleteTask struct {
	applicationCR appv1.Application
	client        client.Client
	log           logr.Logger
}

func (adt *applicationDeleteTask) PerformTask(taskContext context.Context) (bool, error) {

	err := controllers.DeleteArgoCDApplication(context.Background(), adt.applicationCR, adt.client, adt.log)

	if err != nil {
		adt.log.Error(err, "Unable to delete Argo CD Application: "+adt.applicationCR.Name+"/"+adt.applicationCR.Namespace)
	}

	// Don't retry on fail, just return the error
	return false, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appv1.Application{}).
		Complete(r)
}

// Convert ResourceStatus Array into String and then compress it into Byte Array​
func compressResourceData(resources []appv1.ResourceStatus) ([]byte, error) {
	var byteArr []byte
	var buffer bytes.Buffer

	// Convert ResourceStatus object into String.
	resourceStr, err := yaml.Marshal(&resources)
	if err != nil {
		return byteArr, fmt.Errorf("unable to Marshal resource data. %v", err)
	}

	// Compress string data
	gzipWriter, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return byteArr, fmt.Errorf("unable to create Buffer writer. %v", err)
	}

	_, err = gzipWriter.Write([]byte(string(resourceStr)))

	if err != nil {
		return byteArr, fmt.Errorf("unable to compress resource string. %v", err)
	}

	if err := gzipWriter.Close(); err != nil {
		return byteArr, fmt.Errorf("unable to close gzip writer connection. %v", err)
	}

	return buffer.Bytes(), nil
}
