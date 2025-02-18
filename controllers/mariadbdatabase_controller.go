/*


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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	job "github.com/openstack-k8s-operators/lib-common/modules/common/job"
	databasev1beta1 "github.com/openstack-k8s-operators/mariadb-operator/api/v1beta1"
	mariadb "github.com/openstack-k8s-operators/mariadb-operator/pkg"
)

// MariaDBDatabaseReconciler reconciles a MariaDBDatabase object
type MariaDBDatabaseReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

// GetClient -
func (r *MariaDBDatabaseReconciler) GetClient() client.Client {
	return r.Client
}

// GetKClient -
func (r *MariaDBDatabaseReconciler) GetKClient() kubernetes.Interface {
	return r.Kclient
}

// GetLogger -
func (r *MariaDBDatabaseReconciler) GetLogger() logr.Logger {
	return r.Log
}

// GetScheme -
func (r *MariaDBDatabaseReconciler) GetScheme() *runtime.Scheme {
	return r.Scheme
}

// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=mariadbdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=mariadbdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=mariadbdatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=mariadbs/status,verbs=get;list
// +kubebuilder:rbac:groups=mariadb.openstack.org,resources=galeras/status,verbs=get;list
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;delete;patch

// Reconcile reconcile mariadbdatabase API requests
func (r *MariaDBDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	_ = r.Log.WithValues("mariadbdatabase", req.NamespacedName)

	var err error

	// Fetch the MariaDBDatabase instance
	instance := &databasev1beta1.MariaDBDatabase{}
	err = r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	// Fetch the Galera or MariaDB instance from which we'll pull the credentials
	// Note: this will go away when we transition to galera as the db
	db, dbGalera, dbMariadb, err := r.getDatabaseObject(ctx, instance)

	// if we are being deleted then we have to remove the finalizer from MariaDB/Galera and then remove it from ourselves
	if !instance.DeletionTimestamp.IsZero() {
		if err == nil { // so we have MariaDB or Galera to remove finalizer from
			if controllerutil.RemoveFinalizer(db, fmt.Sprintf("%s-%s", helper.GetFinalizer(), instance.Name)) {
				err := r.Update(ctx, db)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		// all our external cleanup logic is done so we can remove our own finalizer to signal that we can be deleted.
		controllerutil.RemoveFinalizer(instance, helper.GetFinalizer())
		// we can unconditionally return here as this is basically the end of the delete sequence for MariaDBDatabase
		// so nothing else needs to be done in the reconcile.
		return ctrl.Result{}, nil
	}

	// we now know that this is not a delete case
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// as it is not a delete case we need to wait for MariaDB or Galera to exists before we can continue.
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
		}

		return ctrl.Result{}, err
	}

	// here we know that MariaDB or Galera exists so add a finalizer to ourselves and to the db CR. Before this point there is no reason to have a finalizer on ourselves as nothing to cleanup.
	if instance.DeletionTimestamp.IsZero() { // this condition can be removed if you wish as it is always true at this point otherwise we would returned earlier.
		if controllerutil.AddFinalizer(db, fmt.Sprintf("%s-%s", helper.GetFinalizer(), instance.Name)) {
			err := r.Update(ctx, db)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

		if controllerutil.AddFinalizer(instance, helper.GetFinalizer()) {
			// we need to persist this right away
			return ctrl.Result{}, nil
		}
	}

	//
	// Non-deletion (normal) flow follows
	//
	var dbName, dbSecret, dbContainerImage string

	// It is impossible to reach here without either dbGalera or dbMariadb not being nil, due to the checks above
	if dbGalera != nil {
		if !dbGalera.Status.Bootstrapped {
			r.Log.Info("DB bootstrap not complete. Requeue...")
			return ctrl.Result{RequeueAfter: time.Second * 10}, nil
		}

		dbName = dbGalera.Name
		dbSecret = dbGalera.Spec.Secret
		dbContainerImage = dbGalera.Spec.ContainerImage
	} else if dbMariadb != nil {
		if dbMariadb.Status.DbInitHash == "" {
			r.Log.Info("DB initialization not complete. Requeue...")
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
		}

		dbName = dbMariadb.Name
		dbSecret = dbMariadb.Spec.Secret
		dbContainerImage = dbMariadb.Spec.ContainerImage
	}

	// Define a new Job object (hostname, password, containerImage)
	jobDef, err := mariadb.DbDatabaseJob(instance, dbName, dbSecret, dbContainerImage)
	if err != nil {
		return ctrl.Result{}, err
	}

	dbCreateHash := instance.Status.Hash[databasev1beta1.DbCreateHash]
	dbCreateJob := job.NewJob(
		jobDef,
		databasev1beta1.DbCreateHash,
		false,
		time.Duration(5)*time.Second,
		dbCreateHash,
	)
	ctrlResult, err := dbCreateJob.DoJob(
		ctx,
		helper,
	)
	if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if dbCreateJob.HasChanged() {
		if instance.Status.Hash == nil {
			instance.Status.Hash = make(map[string]string)
		}
		instance.Status.Hash[databasev1beta1.DbCreateHash] = dbCreateJob.GetHash()
		r.Log.Info(fmt.Sprintf("Job %s hash added - %s", jobDef.Name, instance.Status.Hash[databasev1beta1.DbCreateHash]))
	}

	// database creation finished... okay to set to completed
	instance.Status.Completed = true

	return ctrl.Result{}, nil
}

// SetupWithManager -
func (r *MariaDBDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1beta1.MariaDBDatabase{}).
		Complete(r)
}

// getDatabaseObject - returns either a Galera or MariaDB object (and an associated client.Object interface)
func (r *MariaDBDatabaseReconciler) getDatabaseObject(ctx context.Context, instance *databasev1beta1.MariaDBDatabase) (client.Object, *databasev1beta1.Galera, *databasev1beta1.MariaDB, error) {
	dbGalera := &databasev1beta1.Galera{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.ObjectMeta.Labels["dbName"],
			Namespace: instance.Namespace,
		},
	}

	objectKey := client.ObjectKeyFromObject(dbGalera)

	err := r.Client.Get(ctx, objectKey, dbGalera)
	if err != nil && !k8s_errors.IsNotFound(err) {
		return nil, nil, nil, err
	}

	if err != nil {
		// Try to fetch MariaDB when Galera is not used
		dbMariadb := &databasev1beta1.MariaDB{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.ObjectMeta.Labels["dbName"],
				Namespace: instance.Namespace,
			},
		}

		objectKey = client.ObjectKeyFromObject(dbMariadb)

		err = r.Client.Get(ctx, objectKey, dbMariadb)
		if err != nil {
			return nil, nil, nil, err
		}

		return dbMariadb, nil, dbMariadb, nil
	}

	return dbGalera, dbGalera, nil, nil
}
