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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

// EvictionReconciler reconciles a Eviction object
type EvictionReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

const (
	EvictionControllerName = "eviction"
	shortRetryTime         = 1 * time.Second
	defaultPollTime        = 10 * time.Second
)

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	eviction := &kvmv1.Eviction{}
	if err := r.Get(ctx, req.NamespacedName, eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	hv := &kvmv1.Hypervisor{}
	// Let's fetch the Hypervisor assigned to the eviction, it won't be cached if it's not part of our partition so
	// we won't reconcile evictions for nodes outside our partition
	if err := r.Get(ctx, types.NamespacedName{Name: eviction.Spec.Hypervisor}, hv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := logger.FromContext(ctx).
		WithName("Eviction").
		WithValues("hypervisor", eviction.Spec.Hypervisor)
	ctx = logger.IntoContext(ctx, log)

	// Being deleted
	if !eviction.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	statusCondition := meta.FindStatusCondition(eviction.Status.Conditions, kvmv1.ConditionTypeEvicting)
	if statusCondition == nil {
		base := eviction.DeepCopy()
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeEvicting,
			Status:  metav1.ConditionTrue,
			Message: "Running",
			Reason:  kvmv1.ConditionReasonRunning,
		})

		return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
	}

	switch statusCondition.Status {
	case metav1.ConditionTrue:
		// We are running, so we need to evict the next instance
		return r.handleRunning(ctx, eviction, hv)
	case metav1.ConditionFalse:
		// We are done, so we can just return
		log.Info("finished")
		return ctrl.Result{}, nil
	default:
		log.
			WithValues("reason", statusCondition.Reason).
			WithValues("msg", statusCondition.Message).
			Info("unknown status condition")
	}

	return ctrl.Result{}, nil
}

func (r *EvictionReconciler) handleRunning(ctx context.Context, eviction *kvmv1.Eviction, hypervisor *kvmv1.Hypervisor) (ctrl.Result, error) {
	if !meta.IsStatusConditionTrue(eviction.Status.Conditions, kvmv1.ConditionTypePreflight) {
		// Ensure the hypervisor is disabled and we have the preflight condition
		return r.handlePreflight(ctx, eviction, hypervisor)
	}

	// That should leave us with "Running" and the hypervisor should be deactivated
	if len(eviction.Status.OutstandingInstances) > 0 {
		return r.evictNext(ctx, eviction)
	}

	base := eviction.DeepCopy()
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeEvicting,
		Status:  metav1.ConditionFalse,
		Message: "eviction completed successfully",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})

	eviction.Status.OutstandingRamMb = 0
	logger.FromContext(ctx).Info("succeeded")
	return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
}

func (r *EvictionReconciler) updateStatus(ctx context.Context, eviction, base *kvmv1.Eviction) error {
	return r.Status().Patch(ctx, eviction, client.MergeFromWithOptions(base,
		client.MergeFromWithOptimisticLock{}), client.FieldOwner(EvictionControllerName))
}

func (r *EvictionReconciler) handlePreflight(ctx context.Context, eviction *kvmv1.Eviction, hv *kvmv1.Hypervisor) (ctrl.Result, error) {
	base := eviction.DeepCopy()
	expectHypervisor := hv.Status.HypervisorID != "" && hv.Status.ServiceID != "" // The hypervisor has been registered

	// If the hypervisor should exist, then it should also be disabled before we evict
	if expectHypervisor && !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled) {
		// Hypervisor is not disabled/ensured, so we need to disable it
		if meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypePreflight,
			Status:  metav1.ConditionFalse,
			Message: "hypervisor not disabled",
			Reason:  kvmv1.ConditionReasonFailed,
		}) {
			return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
		}
		return ctrl.Result{RequeueAfter: defaultPollTime}, nil // Wait for hypervisor to be disabled
	}

	// Fetch all virtual machines on the hypervisor
	trueVal := true
	hypervisor, err := hypervisors.GetExt(ctx, r.computeClient, hv.Status.HypervisorID, hypervisors.GetOpts{WithServers: &trueVal}).Extract()
	if err != nil {
		if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return ctrl.Result{}, err
		}

		if expectHypervisor {
			// Abort eviction
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeEvicting,
				Status:  metav1.ConditionFalse,
				Message: fmt.Sprintf("failed to get hypervisor %v", err),
				Reason:  kvmv1.ConditionReasonFailed,
			})
			return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
		} else {
			// That is (likely) an eviction for a node that never registered
			// so we are good to go
			msg := "eviction completed successfully due to expected case of no hypervisor"
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeEvicting,
				Status:  metav1.ConditionFalse,
				Message: msg,
				Reason:  kvmv1.ConditionReasonSucceeded,
			})
			eviction.Status.OutstandingRamMb = 0
			logger.FromContext(ctx).Info(msg)
			return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
		}
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
	eviction.Status.OutstandingRamMb = int64(hypervisor.MemoryMBUsed)
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypePreflight,
		Status:  metav1.ConditionTrue,
		Message: "Preflight checks passed, hypervisor is disabled and ready for eviction",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})
	return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
}

// Tries to handle the NotFound-error by updating the status
func (r *EvictionReconciler) handleNotFound(ctx context.Context, eviction, base *kvmv1.Eviction, err error) error {
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return err
	}
	logger.FromContext(ctx).Info("Instance is gone")
	if base == nil {
		base = eviction.DeepCopy()
	}
	instances := &eviction.Status.OutstandingInstances
	uuid := (*instances)[len(*instances)-1]
	*instances = (*instances)[:len(*instances)-1]
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeMigration,
		Status:  metav1.ConditionFalse,
		Message: fmt.Sprintf("Instance %s is gone", uuid),
		Reason:  kvmv1.ConditionReasonSucceeded,
	})
	return r.updateStatus(ctx, eviction, base)
}

func (r *EvictionReconciler) evictNext(ctx context.Context, eviction *kvmv1.Eviction) (ctrl.Result, error) {
	base := eviction.DeepCopy()
	instances := &eviction.Status.OutstandingInstances
	uuid := (*instances)[len(*instances)-1]
	log := logger.FromContext(ctx).WithName("Evict").WithValues("server", uuid)
	logger.IntoContext(ctx, log)

	res := servers.Get(ctx, r.computeClient, uuid)
	vm, err := res.Extract()

	if err != nil {
		if err2 := r.handleNotFound(ctx, eviction, base, err); err2 != nil {
			return ctrl.Result{}, err2
		} else {
			return ctrl.Result{RequeueAfter: shortRetryTime}, nil
		}
	}

	log = log.WithValues("server_status", vm.Status)
	logger.IntoContext(ctx, log)

	// First, check the transient statuses
	switch vm.Status {
	case "MIGRATING", "RESIZE":
		// wait for the migration to finish
		return ctrl.Result{RequeueAfter: defaultPollTime}, nil
	case "ERROR":
		// Needs manual intervention (or another operator fixes it)
		// put it at the end of the list (beginning of array)
		copy((*instances)[1:], (*instances)[:len(*instances)-1])
		(*instances)[0] = uuid
		log.Info("error", "faultMessage", vm.Fault.Message)
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeMigration,
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Migration of instance %s failed: %s", vm.ID, vm.Fault.Message),
			Reason:  kvmv1.ConditionReasonFailed,
		})

		return ctrl.Result{}, errors.Join(fmt.Errorf("error migrating instance %v", uuid),
			r.updateStatus(ctx, eviction, base))
	}

	currentHypervisor, _, _ := strings.Cut(vm.HypervisorHostname, ".")

	if currentHypervisor != eviction.Spec.Hypervisor {
		log.Info("migrated")
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeMigration,
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Migration of instance %s finished", vm.ID),
			Reason:  kvmv1.ConditionReasonSucceeded,
		})

		// So, it is already off this one, do we need to verify it?
		if vm.Status == "VERIFY_RESIZE" {
			err := servers.ConfirmResize(ctx, r.computeClient, vm.ID).ExtractErr()
			if err2 := r.handleNotFound(ctx, eviction, base, err); err2 != nil {
				// Retry confirm in next reconciliation
				return ctrl.Result{}, err2
			} else {
				// handled not found without errors
				return ctrl.Result{RequeueAfter: shortRetryTime}, nil
			}
		}

		// All done
		*instances = (*instances)[:len(*instances)-1]
		return ctrl.Result{}, r.updateStatus(ctx, eviction, base)
	}

	if vm.TaskState == "deleting" { //nolint:gocritic
		// We just have to wait for it to be gone. Try the next one.
		copy((*instances)[1:], (*instances)[:len(*instances)-1])
		(*instances)[0] = uuid

		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeMigration,
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Live migration of terminating instance %s skipped", vm.ID),
			Reason:  kvmv1.ConditionReasonFailed,
		})
		if err2 := r.updateStatus(ctx, eviction, base); err2 != nil {
			return ctrl.Result{}, fmt.Errorf("could update status due to %w", err2)
		}
	} else if vm.Status == "ACTIVE" || vm.PowerState == 1 {
		log.Info("trigger live-migration")
		err := r.liveMigrate(ctx, vm.ID, eviction)
		if err != nil {
			if err2 := r.handleNotFound(ctx, eviction, base, err); err2 != nil {
				return ctrl.Result{}, err2
			} else {
				return ctrl.Result{RequeueAfter: shortRetryTime}, nil
			}
		}
	} else {
		log.Info("trigger cold-migration")
		err := r.coldMigrate(ctx, vm.ID, eviction)
		if err != nil {
			if err2 := r.handleNotFound(ctx, eviction, base, err); err2 != nil {
				return ctrl.Result{}, err2
			} else {
				return ctrl.Result{RequeueAfter: shortRetryTime}, nil
			}
		}
	}

	// Triggered a migration, give it a generous time to start, so we do not
	// see the old state because the migration didn't start
	log.Info("poll")
	return ctrl.Result{RequeueAfter: defaultPollTime}, err
}

func (r *EvictionReconciler) liveMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: &[]bool{false}[0],
	}

	res := servers.LiveMigrate(ctx, r.computeClient, uuid, liveMigrateOpts)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict VM %s due to %w", uuid, res.Err)
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeMigration,
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  kvmv1.ConditionReasonFailed,
		})
		return err
	}

	log.Info("Live migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

func (r *EvictionReconciler) coldMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	res := servers.Migrate(ctx, r.computeClient, uuid)
	if res.Err != nil {
		err := fmt.Errorf("failed to evict stopped server %s due to %w", uuid, res.Err)
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeMigration,
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  kvmv1.ConditionReasonFailed,
		})
		return err
	}

	log.Info("Cold-migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.90" // Xena (or later)

	return ctrl.NewControllerManagedBy(mgr).
		Named(EvictionControllerName).
		For(&kvmv1.Eviction{}).
		Complete(r)
}
