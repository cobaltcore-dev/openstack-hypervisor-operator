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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

// EvictionReconciler reconciles a Eviction object
type EvictionReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	computeClient    *gophercloud.ServiceClient
	instanceHAClient *gophercloud.ServiceClient
	rand             *rand.Rand
}

const (
	evictionFinalizerName = "eviction-controller.cloud.sap/finalizer"
	Succeeded             = "Succeeded"
	Running               = "Running"
	Reconciling           = "Reconciling"
	Reconciled            = "Reconciled"
	Failed                = "Failed"
)

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	eviction := &kvmv1.Eviction{}
	if err := r.Get(ctx, req.NamespacedName, eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := logger.FromContext(ctx).WithName("Eviction").WithValues("hypervisor", eviction.Spec.Hypervisor, "status", eviction.Status.EvictionState)
	ctx = logger.IntoContext(ctx, log)
	// Being deleted
	if !eviction.DeletionTimestamp.IsZero() {
		err := r.handleFinalizer(ctx, eviction)
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
		return r.handlePending(ctx, eviction)
	case "Running":
		return r.handleRunning(ctx, eviction)
	default:
		log.Info("Unknown eviction-state", "EvictionState", eviction.Status.EvictionState)
		return ctrl.Result{}, nil
	}
}

func (r *EvictionReconciler) handleRunning(ctx context.Context, eviction *kvmv1.Eviction) (reconcile.Result, error) {
	// That should leave us with "Running" and the hypervisor should be deactivated
	if len(eviction.Status.OutstandingInstances) > 0 {
		return r.evictNext(ctx, eviction)
	}

	err := r.setServerMaintenance(ctx, eviction, true)
	if err != nil {
		return ctrl.Result{}, err
	}

	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionTrue,
		Message: Reconciled,
		Reason:  Reconciled,
	})

	eviction.Status.OutstandingRamMb = 0
	eviction.Status.EvictionState = Succeeded
	logger.FromContext(ctx).Info("succeeded")
	return ctrl.Result{}, r.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) setServerMaintenance(ctx context.Context, eviction *kvmv1.Eviction, maintenance bool) error {
	if r.instanceHAClient == nil {
		return nil
	}

	for _, owner := range eviction.GetOwnerReferences() {
		if owner.Kind == "Node" && owner.APIVersion == "v1" {
			node := &corev1.Node{}
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: "", Name: owner.Name}, node)
			// Not found means no labels, so the rest will work as well
			if client.IgnoreNotFound(err) != nil {
				return err
			}

			segmentID, segmentIDFound := node.Labels[labelSegmentID]
			hostID, hostIDFound := node.Labels[labelMasakariHostID]

			if !segmentIDFound || !hostIDFound {
				continue
			}

			return openstack.UpdateSegmentHost(ctx, r.instanceHAClient, segmentID, hostID, openstack.UpdateSegmentHostOpts{OnMaintenance: &maintenance}).Err
		}
	}
	return nil
}

func (r *EvictionReconciler) handlePending(ctx context.Context, eviction *kvmv1.Eviction) (reconcile.Result, error) {
	hypervisorName := eviction.Spec.Hypervisor

	// Does the hypervisor even exist? Is it enabled/disabled?
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, hypervisorName, false)
	if err != nil {
		// Abort eviction
		err = fmt.Errorf("failed to get hypervisor %w", err)
		r.addErrorCondition(ctx, eviction, err)
		return ctrl.Result{}, err
	}

	log := logger.FromContext(ctx)
	currentHypervisor, _, _ := strings.Cut(hypervisor.HypervisorHostname, ".")
	if currentHypervisor != hypervisorName {
		err = fmt.Errorf("hypervisor name %q does not match spec %q", currentHypervisor, hypervisorName)
		log.Error(err, "Hypervisor name mismatch")
		if eviction.Status.EvictionState != Failed ||
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    "Eviction",
				Status:  metav1.ConditionFalse,
				Message: err.Error(),
				Reason:  Failed}) {
			eviction.Status.EvictionState = Failed
			log.Info("Update", "status", eviction.Status)
			if err := r.Status().Update(ctx, eviction); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	}

	crdModified, err := r.disableHypervisor(ctx, hypervisor, eviction)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not disable the hypervisor %v due to %w", hypervisorName, err)
	}
	if crdModified {
		return ctrl.Result{}, nil
	}

	// Fetch all virtual machines on the hypervisor
	hypervisor, err = openstack.GetHypervisorByName(ctx, r.computeClient, hypervisorName, true)
	if err != nil {
		return ctrl.Result{}, err
	}

	if hypervisor.Servers != nil {
		uuids := make([]string, len(*hypervisor.Servers))
		for i, server := range *hypervisor.Servers {
			uuids[i] = server.UUID
		}
		eviction.Status.OutstandingInstances = uuids
	}

	// Update status
	eviction.Status.HypervisorServiceId = hypervisor.ID
	eviction.Status.EvictionState = Running
	eviction.Status.OutstandingRamMb = hypervisor.MemoryMbUsed
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionTrue,
		Message: Reconciling,
		Reason:  Reconciling,
	})

	return ctrl.Result{}, r.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) evictNext(ctx context.Context, eviction *kvmv1.Eviction) (reconcile.Result, error) {
	instances := &eviction.Status.OutstandingInstances
	uuid := (*instances)[len(*instances)-1]
	log := logger.FromContext(ctx).WithName("Evict").WithValues("server", uuid)

	res := servers.Get(ctx, r.computeClient, uuid)
	vm, err := res.Extract()

	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			log.Info("Instance is gone")
			*instances = (*instances)[:len(*instances)-1]
			return ctrl.Result{}, r.Status().Update(ctx, eviction)
		}
		return reconcile.Result{}, err
	}

	log = log.WithValues("server_status", vm.Status)

	// First, check the transient statuses
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
		if err := r.Status().Update(ctx, eviction); err != nil {
			return ctrl.Result{}, err
		}

		return reconcile.Result{}, fmt.Errorf("error migrating instance %v", uuid)
	}

	currentHypervisor, _, _ := strings.Cut(vm.HypervisorHostname, ".")

	if currentHypervisor != eviction.Spec.Hypervisor {
		log.Info("migrated")
		// So, it is already off this one, do we need to verify it?
		if vm.Status == "VERIFY_RESIZE" {
			if err := servers.ConfirmResize(ctx, r.computeClient, vm.ID).ExtractErr(); err != nil {
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
		return reconcile.Result{}, r.Status().Update(ctx, eviction)
	}

	if vm.Status == "ACTIVE" || vm.PowerState == 1 {
		log.Info("trigger live-migration")
		if err := r.liveMigrate(ctx, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				// Fall-back to beginning, which will clean it out
				return reconcile.Result{Requeue: true}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			if err2 := r.Status().Update(ctx, eviction); err != nil {
				return ctrl.Result{}, fmt.Errorf("could not live-migrate due to %w and %w", err, err2)
			}

			return ctrl.Result{}, err
		}
	} else {
		log.Info("trigger cold-migration")
		if err := r.coldMigrate(ctx, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				return reconcile.Result{Requeue: true}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			if err2 := r.Status().Update(ctx, eviction); err != nil {
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

func (r *EvictionReconciler) handleFinalizer(ctx context.Context, eviction *kvmv1.Eviction) error {
	if controllerutil.RemoveFinalizer(eviction, evictionFinalizerName) {
		err := r.setServerMaintenance(ctx, eviction, false)
		if err != nil {
			return err
		}

		err = r.enableHypervisorService(ctx, eviction)
		if err != nil {
			if _, ok := err.(*openstack.NoHypervisorError); ok {
				log := logger.FromContext(ctx)
				log.Info("Can't enable host, it is gone")
			} else {
				return err
			}
		}

		if err := r.Update(ctx, eviction); err != nil {
			return err
		}
	}
	return nil
}

func (r *EvictionReconciler) enableHypervisorService(ctx context.Context, eviction *kvmv1.Eviction) error {
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, eviction.Spec.Hypervisor, false)
	if err != nil {
		err2 := fmt.Errorf("failed to get hypervisor due to %w", err)
		// Abort eviction
		r.addErrorCondition(ctx, eviction, err2)
		return err
	}

	if hypervisor.Service.DisabledReason != r.evictionReason(eviction) {
		return nil
	}

	log := logger.FromContext(ctx)
	enableService := services.UpdateOpts{Status: services.ServiceEnabled}
	log.Info("Enabling hypervisor", "id", hypervisor.Service.ID)
	_, err = services.Update(ctx, r.computeClient, hypervisor.Service.ID, enableService).Extract()
	return err
}

func (r *EvictionReconciler) disableHypervisor(ctx context.Context, hypervisor *openstack.Hypervisor, eviction *kvmv1.Eviction) (bool, error) {
	evictionReason := r.evictionReason(eviction)
	disabledReason := hypervisor.Service.DisabledReason

	if disabledReason != nil && disabledReason != "" && disabledReason != evictionReason {
		// Disabled for another reason already
		return r.addProgressCondition(ctx, eviction, "Found host already disabled", "Update"), nil
	}

	if controllerutil.AddFinalizer(eviction, evictionFinalizerName) {
		return true, r.Update(ctx, eviction)
	}

	if hypervisor.Service.DisabledReason == evictionReason {
		return false, nil
	}

	disableService := services.UpdateOpts{Status: services.ServiceDisabled,
		DisabledReason: r.evictionReason(eviction)}

	_, err := services.Update(ctx, r.computeClient, hypervisor.Service.ID, disableService).Extract()
	if err != nil {
		return r.addErrorCondition(ctx, eviction, err), err
	}

	if r.addProgressCondition(ctx, eviction, "Host disabled", "Update") {
		return true, nil
	}

	return false, nil
}

func (r *EvictionReconciler) liveMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: swag.Bool(false),
	}

	res := servers.LiveMigrate(ctx, r.computeClient, uuid, liveMigrateOpts)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict VM %s due to %w", uuid, res.Err)
		r.addErrorCondition(ctx, eviction, err)
		return res.Err
	}

	log.Info("Live migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

func (r *EvictionReconciler) coldMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	res := servers.Migrate(ctx, r.computeClient, uuid)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict stopped server %s due to %w", uuid, res.Err)
		r.addErrorCondition(ctx, eviction, err)
		return err
	}

	log.Info("Cold-migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

// addCondition adds a condition to the Eviction status and updates the status
func (r *EvictionReconciler) addCondition(ctx context.Context, eviction *kvmv1.Eviction, status metav1.ConditionStatus, message string, reason string) bool {
	if !meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Eviction",
		Status:  status,
		Message: message,
		Reason:  reason,
	}) {
		return false
	}

	if err := r.Status().Update(ctx, eviction); err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "Failed to update conditions")
		return false
	}

	return true
}

func (r *EvictionReconciler) addProgressCondition(ctx context.Context, eviction *kvmv1.Eviction, message string, reason string) bool {
	return r.addCondition(ctx, eviction, metav1.ConditionTrue, message, reason)
}

func (r *EvictionReconciler) addErrorCondition(ctx context.Context, eviction *kvmv1.Eviction, err error) bool {
	return r.addCondition(ctx, eviction, metav1.ConditionFalse, err.Error(), Failed)
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	log := logger.FromContext(ctx)

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute"); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.90" // Xena (or later)

	r.instanceHAClient, err = openstack.GetServiceClient(ctx, "instance-ha")
	if err != nil {
		log.Error(err, "failed to find masakari")
	}

	r.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	return ctrl.NewControllerManagedBy(mgr).
		For(&kvmv1.Eviction{}).
		Complete(r)
}
