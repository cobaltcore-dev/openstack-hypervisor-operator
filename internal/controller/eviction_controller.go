/*
SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
SPDX-License-Identifier: Apache-2.0

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

package controller

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-openapi/swag"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

// EvictionReconciler reconciles a Eviction object
type EvictionReconciler struct {
	serviceClient *gophercloud.ServiceClient
	rand          *rand.Rand
}

const (
	finalizerName = "eviction-controller.cloud.sap/finalizer"
	Succeeded     = "Succeeded"
	Running       = "Running"
	Reconciling   = "Reconciling"
	Reconciled    = "Reconciled"
	Failed        = "Failed"
)

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req request) (ctrl.Result, error) {
	eviction := &kvmv1.Eviction{}
	if err := req.client.Get(ctx, req.NamespacedName, eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := logger.FromContext(ctx).WithName("Eviction").WithValues("hypervisor", eviction.Spec.Hypervisor, "status", eviction.Status.EvictionState)
	ctx = logger.IntoContext(ctx, log)
	// Being deleted
	if !eviction.DeletionTimestamp.IsZero() {
		err := r.handleFinalizer(ctx, req.client, eviction)
		log.Info("deleted")
		return ctrl.Result{}, err
	}

	switch eviction.Status.EvictionState {
	case Succeeded, Failed:
		// Nothing left to be done
		log.Info("finished")
		return ctrl.Result{}, nil
	case "Pending", "":
		// Let's see if we can take it up
		eviction.Status.EvictionState = "Pending" // "" -> "Pending: Fixes the test
		log.Info("setup")
		return r.handlePending(ctx, eviction, req)
	}

	if len(eviction.Status.OutstandingInstances) > 0 {
		return r.evictNext(ctx, req, eviction)
	}

	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionTrue,
		Message: Reconciled,
		Reason:  Reconciled,
	})

	eviction.Status.OutstandingRamMb = 0
	eviction.Status.EvictionState = Succeeded
	log.Info("succeeded")
	return ctrl.Result{}, req.client.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) handlePending(ctx context.Context, eviction *kvmv1.Eviction, req request) (reconcile.Result, error) {
	hypervisorName := eviction.Spec.Hypervisor

	// Does the hypervisor even exist? Is it enabled/disabled?
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.serviceClient, hypervisorName, false)
	if err != nil {
		// Abort eviction
		err = fmt.Errorf("failed to get hypervisor %w", err)
		r.addErrorCondition(ctx, req.client, eviction, err)
		return ctrl.Result{}, err
	}

	log := logger.FromContext(ctx)
	currentHypervisor, _, _ := strings.Cut(hypervisor.HypervisorHostname, ".")
	if currentHypervisor != hypervisorName {
		err = fmt.Errorf("hypervisor name %q does not match spec %q", currentHypervisor, hypervisorName)
		log.Error(err, "Hpyervisor name mismatch")
		if eviction.Status.EvictionState != Failed ||
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    "Eviction",
				Status:  metav1.ConditionFalse,
				Message: err.Error(),
				Reason:  Failed}) {
			eviction.Status.EvictionState = Failed
			log.Info("Update", "status", eviction.Status)
			if err := req.client.Status().Update(ctx, eviction); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	crdModified, err := r.disableHypervisor(ctx, req, hypervisor, eviction)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not disable the hypervisor %v due to %w", hypervisorName, err)
	}
	if crdModified {
		return ctrl.Result{}, nil
	}

	// Fetch all virtual machines on the hypervisor
	hypervisor, err = openstack.GetHypervisorByName(ctx, r.serviceClient, hypervisorName, true)
	if err != nil {
		return ctrl.Result{}, err
	}

	var uuids []string
	if hypervisor.Servers == nil {
		uuids = make([]string, 0)
	} else {
		uuids = make([]string, len(*hypervisor.Servers))
		for i, server := range *hypervisor.Servers {
			uuids[i] = server.UUID
		}
	}

	// Update status
	eviction.Status.HypervisorServiceId = hypervisor.ID
	eviction.Status.EvictionState = Running
	eviction.Status.OutstandingInstances = uuids
	eviction.Status.OutstandingRamMb = hypervisor.MemoryMbUsed
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionTrue,
		Message: Reconciling,
		Reason:  Reconciling,
	})

	return ctrl.Result{}, req.client.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) evictNext(ctx context.Context, req request, eviction *kvmv1.Eviction) (reconcile.Result, error) {
	instances := &eviction.Status.OutstandingInstances
	uuid := (*instances)[len(*instances)-1]
	log := logger.FromContext(ctx).WithName("Evict").WithValues("server", uuid)

	res := servers.Get(ctx, r.serviceClient, uuid)
	vm, err := res.Extract()

	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			log.Info("Instance is gone")
			*instances = (*instances)[:len(*instances)-1]
			return ctrl.Result{}, req.client.Status().Update(ctx, eviction)
		}
		return reconcile.Result{}, err
	}

	log = log.WithValues("server_status", vm.Status)

	currentHypervisor, _, _ := strings.Cut(vm.HypervisorHostname, ".")

	if currentHypervisor != eviction.Spec.Hypervisor {
		log.Info("migrated")
		// So, it is already off this one, do we need to verify it?
		if vm.Status == "VERIFY_RESIZE" {
			if err := servers.ConfirmResize(ctx, r.serviceClient, vm.ID).ExtractErr(); err != nil {
				if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
					log.Info("Instance is gone")
					// Fall-back to beginning, which will clean it out
					return reconcile.Result{Requeue: true}, nil
				}
				// Retry confirm in next reconciliation
				return reconcile.Result{}, err
			}
		}

		// All done
		*instances = (*instances)[:len(*instances)-1]
		return reconcile.Result{}, req.client.Status().Update(ctx, eviction)
	}

	switch vm.Status {
	case "MIGRATING", "RESIZE":
		// wait for the migration to finish
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	case "ERROR":
		// Needs manual intervention (or another operator fixes it)
		// put it at the end of the list (beginning of array)
		copy((*instances)[1:], (*instances)[:len(*instances)-1])
		(*instances)[0] = uuid
		log.Info("error", "faultMessage", vm.Fault.Message)
		if err := req.client.Status().Update(ctx, eviction); err != nil {
			return ctrl.Result{}, err
		}

		return reconcile.Result{}, fmt.Errorf("error migrating instance %v", uuid)
	}

	if vm.Status == "ACTIVE" || vm.PowerState == 1 {
		log.Info("trigger live-migration")
		if err := r.liveMigrate(ctx, req.client, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				// Fall-back to beginning, which will clean it out
				return reconcile.Result{Requeue: true}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			if err2 := req.client.Status().Update(ctx, eviction); err != nil {
				return ctrl.Result{}, fmt.Errorf("could not live-migrate due to %w and %w", err, err2)
			}

			return ctrl.Result{}, err
		}
	} else {
		log.Info("trigger cold-migration")
		if err := r.coldMigrate(ctx, req.client, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				return reconcile.Result{Requeue: true}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			if err2 := req.client.Status().Update(ctx, eviction); err != nil {
				return ctrl.Result{}, fmt.Errorf("could not cold-migrate due to %w and %w", err, err2)
			}

			return ctrl.Result{}, err
		}
	}

	// Triggered a migration, give it a generous time to start, so we do not
	// see the old state because the migration didn't start
	log.Info("poll")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, err
}

func (r *EvictionReconciler) evictionReason(eviction *kvmv1.Eviction) string {
	return fmt.Sprintf("Eviction %v/%v: %v", eviction.Namespace, eviction.Name, eviction.Spec.Reason)
}

func (r *EvictionReconciler) handleFinalizer(ctx context.Context, client client.Client, eviction *kvmv1.Eviction) error {
	if controllerutil.RemoveFinalizer(eviction, finalizerName) {
		if err := r.enableHypervisorService(ctx, client, eviction); err != nil {
			return err
		}

		if err := client.Update(ctx, eviction); err != nil {
			return err
		}
	}
	return nil
}

func (r *EvictionReconciler) enableHypervisorService(ctx context.Context, client client.Client, eviction *kvmv1.Eviction) error {
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.serviceClient, eviction.Spec.Hypervisor, false)
	if err != nil {
		err2 := fmt.Errorf("failed to get hypervisor due to %w", err)
		// Abort eviction
		r.addErrorCondition(ctx, client, eviction, err2)
		return err
	}

	if hypervisor.Service.DisabledReason != r.evictionReason(eviction) {
		return nil
	}

	return r.enableHypervisorServiceInternal(ctx, hypervisor.Service.ID)
}

func (r *EvictionReconciler) enableHypervisorServiceInternal(ctx context.Context, id string) error {
	log := logger.FromContext(ctx)
	enableService := services.UpdateOpts{Status: services.ServiceEnabled}
	log.Info("Enabling hypervisor", "id", id)
	_, err := services.Update(ctx, r.serviceClient, id, enableService).Extract()
	return err
}

func (r *EvictionReconciler) disableHypervisor(ctx context.Context, req request, hypervisor *openstack.Hypervisor, eviction *kvmv1.Eviction) (bool, error) {
	evictionReason := r.evictionReason(eviction)
	disabledReason := hypervisor.Service.DisabledReason

	if disabledReason != nil && disabledReason != "" && disabledReason != evictionReason {
		// Disabled for another reason already
		return r.addProgressCondition(ctx, req.client, eviction, "Found host already disabled", "Update"), nil
	}

	if controllerutil.AddFinalizer(eviction, finalizerName) {
		return true, req.client.Update(ctx, eviction)
	}

	if hypervisor.Service.DisabledReason == evictionReason {
		return false, nil
	}

	disableService := services.UpdateOpts{Status: services.ServiceDisabled,
		DisabledReason: r.evictionReason(eviction)}

	_, err := services.Update(ctx, r.serviceClient, hypervisor.Service.ID, disableService).Extract()
	if err != nil {
		return r.addErrorCondition(ctx, req.client, eviction, err), err
	}

	if r.addProgressCondition(ctx, req.client, eviction, "Host disabled", "Update") {
		return true, nil
	}

	return false, nil
}

func (r *EvictionReconciler) liveMigrate(ctx context.Context, client client.Client, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: swag.Bool(false),
	}

	res := servers.LiveMigrate(ctx, r.serviceClient, uuid, liveMigrateOpts)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict VM %s due to %w", uuid, res.Err)
		r.addErrorCondition(ctx, client, eviction, err)
		return res.Err
	}

	log.Info("Live migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

func (r *EvictionReconciler) coldMigrate(ctx context.Context, client client.Client, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	res := servers.Migrate(ctx, r.serviceClient, uuid)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict stopped server %s due to %w", uuid, res.Err)
		r.addErrorCondition(ctx, client, eviction, err)
		return err
	}

	log.Info("Cold-migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

// addCondition adds a condition to the Eviction status and updates the status
func (r *EvictionReconciler) addCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, status metav1.ConditionStatus, message string, reason string) bool {
	if !meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Eviction",
		Status:  status,
		Message: message,
		Reason:  reason,
	}) {
		return false
	}

	if err := client.Status().Update(ctx, eviction); err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "Failed to update conditions")
		return false
	}

	return true
}

func (r *EvictionReconciler) addProgressCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, message string, reason string) bool {
	return r.addCondition(ctx, client, eviction, metav1.ConditionTrue, message, reason)
}

func (r *EvictionReconciler) addErrorCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, err error) bool {
	return r.addCondition(ctx, client, eviction, metav1.ConditionFalse, err.Error(), Failed)
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManagerAndClusters(mgr ctrl.Manager, clusters map[string]cluster.Cluster) error {
	_ = logger.FromContext(context.Background())
	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}
	r.serviceClient.Microversion = "2.90" // Xena (or later)
	r.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	b := builder.TypedControllerManagedBy[request](mgr).Named("eviction-controller")

	eviction := kvmv1.Eviction{}
	for clusterName, cluster := range clusters {
		b = b.WatchesRawSource(source.TypedKind(
			cluster.GetCache(),
			&eviction,
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, e *kvmv1.Eviction) []request {
				return []request{{
					NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name},
					clusterName:    clusterName,
					client:         cluster.GetClient(),
				}}
			}),
		))
	}

	return b.Complete(r)
}
