package operations

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const KubeSystemNamespace = "kube-system"

// Global variables to identify resources created by Namespace Reconciler.
const (
	IdentifierKey   = "source"
	IdentifierValue = "periodic-cleanup"
)

func CreateOperation(ctx context.Context, waitForOperation bool, dbOperationParam db.Operation, clusterUserID string,
	operationNamespace string, dbQueries db.ApplicationScopedQueries, gitopsEngineClient client.Client,
	logger logr.Logger) (*managedgitopsv1alpha1.Operation, *db.Operation, error) {
	var err error
	logger = logger.WithValues("Operation GitOpsEngineInstanceID", dbOperationParam.Instance_id,
		"Operation ResourceID", dbOperationParam.Resource_id,
		"Operation ResourceType", dbOperationParam.Resource_type,
		"Operation OwnerUserID", clusterUserID,
	)
	var dbOperationList []db.Operation
	if err = dbQueries.ListOperationsByResourceIdAndTypeAndOwnerId(ctx, dbOperationParam.Resource_id, dbOperationParam.Resource_type, &dbOperationList, clusterUserID); err != nil {
		logger.Error(err, "unable to fetch list of Operations")
		// We intentionally don't return here: if we were unable to fetch the list,
		// then we will create an Operation as usual (and it might be duplicate, but that's fine)
	}

	// Iterate through existing DB entries for a given resource: look to see if there is already an Operation
	// in the waiting state.
	for idx := range dbOperationList {

		dbOperation := dbOperationList[idx]

		if dbOperation.State != db.OperationState_Waiting {
			continue
		}

		k8sOperation := managedgitopsv1alpha1.Operation{
			// TODO: GITOPSRVCE-195: Update this when standardizing operation CRs
			ObjectMeta: metav1.ObjectMeta{
				Name:      "operation-" + dbOperation.Operation_id,
				Namespace: operationNamespace,
			},
		}

		if err = gitopsEngineClient.Get(ctx, client.ObjectKeyFromObject(&k8sOperation), &k8sOperation); err != nil {
			logger.Error(err, "unable to fetch existing Operation from cluster.", "Operation k8s Name", k8sOperation.Name)
			// We intentionally don't return here: we keep going through the list, even if an error occurs.
			// Only one needs to match.
		} else {
			// An operation already exists in waiting state, and the Operation CR for it still exists, so we don't need to create
			// a new operation.
			logger.Info("Skipping Operation creation, as it already exists for resource.", "existingOperationState", string(dbOperation.State))
			return &k8sOperation, &dbOperation, nil
		}
	}

	dbOperation := db.Operation{
		Instance_id:             dbOperationParam.Instance_id,
		Resource_id:             dbOperationParam.Resource_id,
		Resource_type:           dbOperationParam.Resource_type,
		Operation_owner_user_id: clusterUserID,
		Created_on:              time.Now(),
		Last_state_update:       time.Now(),
		State:                   db.OperationState_Waiting,
		Human_readable_state:    "",
	}

	if err := dbQueries.CreateOperation(ctx, &dbOperation, clusterUserID); err != nil {
		logger.Error(err, "Unable to create Operation database row")
		return nil, nil, err
	}
	logger.Info("Created Operation database row")

	// Create K8s operation
	operation := managedgitopsv1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "operation-" + dbOperation.Operation_id,
			Namespace: operationNamespace,
		},
		Spec: managedgitopsv1alpha1.OperationSpec{
			OperationID: dbOperation.Operation_id,
		},
	}

	// Set annotation as an identifier for Operations created by Namespace Reconciler.
	if clusterUserID == db.SpecialClusterUserName {
		operation.Annotations = map[string]string{IdentifierKey: IdentifierValue}
	}

	if err := gitopsEngineClient.Create(ctx, &operation, &client.CreateOptions{}); err != nil {
		logger.Error(err, "Unable to create K8s Operation")
		return nil, nil, err
	}
	logger.Info("Created K8s Operation CR")

	// Wait for operation to complete.
	if waitForOperation {
		logger.V(sharedutil.LogLevel_Debug).Info("Waiting for Operation to complete")

		if err = waitForOperationToComplete(ctx, &dbOperation, dbQueries, logger); err != nil {
			logger.Error(err, "operation did not complete", "operation", dbOperation.Operation_id, "namespace", operation.Namespace)
			return nil, nil, err
		}

		logger.Info("Operation completed", "operation", fmt.Sprintf("%v", operation.Spec.OperationID))
	}

	return &operation, &dbOperation, nil

}

// cleanupOperation cleans up the database entry and (optionally) the CR, once an operation has concluded.
func CleanupOperation(ctx context.Context, dbOperation db.Operation, k8sOperation managedgitopsv1alpha1.Operation, operationNamespace string,
	dbQueries db.ApplicationScopedQueries, gitopsEngineClient client.Client, log logr.Logger) error {

	log = log.WithValues("operation", dbOperation.Operation_id, "namespace", operationNamespace)

	// // Delete the database entry
	// rowsDeleted, err := dbQueries.DeleteOperationById(ctx, dbOperation.Operation_id)
	// if err != nil {
	// 	return err
	// }
	// if rowsDeleted != 1 {
	// 	log.V(sharedutil.LogLevel_Warn).Error(err, "unexpected number of operation rows deleted", "operation-id", dbOperation.Operation_id, "rows", rowsDeleted)
	// }

	// Optional: Delete the Operation CR
	if err := gitopsEngineClient.Delete(ctx, &k8sOperation); err != nil {
		if !apierr.IsNotFound(err) {
			// Log the error, but don't return it: it's the responsibility of the cluster agent to delete the operation cr
			log.Error(err, "Unable to delete Operation CR")
		}
	}
	log.V(sharedutil.LogLevel_Debug).Info("Deleted operation CR: " + k8sOperation.Name)

	return nil

}

// waitForOperationToComplete waits for an Operation database entry to have 'Completed' or 'Failed' status.
//
func waitForOperationToComplete(ctx context.Context, dbOperation *db.Operation, dbQueries db.ApplicationScopedQueries, log logr.Logger) error {

	backoff := sharedutil.ExponentialBackoff{Factor: 2, Min: time.Duration(100 * time.Millisecond), Max: time.Duration(10 * time.Second), Jitter: true}

	for {

		err := dbQueries.GetOperationById(ctx, dbOperation)
		if err != nil {
			// Either the operation couldn't be found (which shouldn't happen here), or some other issue, so return it
			return err
		}

		if err == nil && (dbOperation.State == db.OperationState_Completed || dbOperation.State == db.OperationState_Failed) {
			break
		}

		backoff.DelayOnFail(ctx)

		// Break if the request is cancelled, or the timeout expires
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation context is Done() in waitForOperationToComplete")
		default:
		}

	}

	return nil
}
