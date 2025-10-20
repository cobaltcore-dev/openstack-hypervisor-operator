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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	evictionFinalizerName  = "eviction-controller.cloud.sap/finalizer"
	EvictionControllerName = "eviction"
	shortRetryTime         = 1 * time.Second
	defaultPollTime        = 10 * time.Second
)

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	eviction := &kvmv1.Eviction{}
	if err := r.Get(ctx, req.NamespacedName, eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := logger.FromContext(ctx).
		WithName("Eviction").
		WithValues("hypervisor", eviction.Spec.Hypervisor)
	ctx = logger.IntoContext(ctx, log)

	// Being deleted
	if !eviction.DeletionTimestamp.IsZero() {
		err := r.handleFinalizer(ctx, eviction)
		if err != nil {
			if errors.Is(err, ErrRetry) {
				return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
			}
			return ctrl.Result{}, err
		}
		log.Info("deleted")
		return ctrl.Result{}, err
	}

	statusCondition := meta.FindStatusCondition(eviction.Status.Conditions, kvmv1.ConditionTypeEvicting)
	if statusCondition == nil {
		// No status condition, so we need to add it
		if r.addCondition(ctx, eviction, metav1.ConditionTrue, "Running", kvmv1.ConditionReasonRunning) {
			log.Info("running")
			return ctrl.Result{}, nil
		}
		// We just checked if the condition is there, so this should never
		// be reached, but let's cover our bass
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	switch statusCondition.Status {
	case metav1.ConditionTrue:
		// We are running, so we need to evict the next instance
		return r.handleRunning(ctx, eviction)
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

func (r *EvictionReconciler) handleRunning(ctx context.Context, eviction *kvmv1.Eviction) (ctrl.Result, error) {
	if !meta.IsStatusConditionTrue(eviction.Status.Conditions, kvmv1.ConditionTypePreflight) {
		// Ensure the hypervisor is disabled and we have the preflight condition
		return r.handlePreflight(ctx, eviction)
	}

	// That should leave us with "Running" and the hypervisor should be deactivated
	if len(eviction.Status.OutstandingInstances) > 0 {
		return r.evictNext(ctx, eviction)
	}

	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeEvicting,
		Status:  metav1.ConditionFalse,
		Message: "eviction completed successfully",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})

	eviction.Status.OutstandingRamMb = 0
	logger.FromContext(ctx).Info("succeeded")
	return ctrl.Result{}, r.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) handlePreflight(ctx context.Context, eviction *kvmv1.Eviction) (ctrl.Result, error) {
	hypervisorName := eviction.Spec.Hypervisor

	// Does the hypervisor even exist? Is it enabled/disabled?
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, hypervisorName, false)
	if err != nil {
		expectHypervisor := true
		hv := &kvmv1.Hypervisor{}
		if err := r.Get(ctx, client.ObjectKey{Name: eviction.Spec.Hypervisor}, hv); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}

		if hv.Name != "" {
			expectHypervisor = HasStatusCondition(hv.Status.Conditions, ConditionTypeOnboarding)
		}

		if expectHypervisor {
			// Abort eviction
			err = fmt.Errorf("failed to get hypervisor %w", err)
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeEvicting,
				Status:  metav1.ConditionFalse,
				Message: err.Error(),
				Reason:  kvmv1.ConditionReasonFailed,
			})
			return ctrl.Result{}, r.Status().Update(ctx, eviction)
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
			return ctrl.Result{}, r.Status().Update(ctx, eviction)
		}
	}

	log := logger.FromContext(ctx)
	currentHypervisor, _, _ := strings.Cut(hypervisor.HypervisorHostname, ".")
	if currentHypervisor != hypervisorName {
		err = fmt.Errorf("hypervisor name %q does not match spec %q", currentHypervisor, hypervisorName)
		log.Error(err, "Hypervisor name mismatch")
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeEvicting,
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  kvmv1.ConditionReasonFailed,
		})
		return ctrl.Result{}, r.Status().Update(ctx, eviction)
	}

	if !meta.IsStatusConditionTrue(eviction.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled) {
		// Hypervisor is not disabled/ensured, so we need to disable it
		return ctrl.Result{}, r.disableHypervisor(ctx, hypervisor, eviction)
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
	eviction.Status.OutstandingRamMb = int64(hypervisor.MemoryMBUsed)
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypePreflight,
		Status:  metav1.ConditionTrue,
		Message: "Preflight checks passed, hypervisor is disabled and ready for eviction",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})
	return ctrl.Result{}, r.Status().Update(ctx, eviction)
}

func (r *EvictionReconciler) evictNext(ctx context.Context, eviction *kvmv1.Eviction) (ctrl.Result, error) {
	instances := &eviction.Status.OutstandingInstances
	uuid := (*instances)[len(*instances)-1]
	log := logger.FromContext(ctx).WithName("Evict").WithValues("server", uuid)

	res := servers.Get(ctx, r.computeClient, uuid)
	vm, err := res.Extract()

	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			log.Info("Instance is gone")
			*instances = (*instances)[:len(*instances)-1]
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeMigration,
				Status:  metav1.ConditionFalse,
				Message: fmt.Sprintf("Instance %s is gone", uuid),
				Reason:  kvmv1.ConditionReasonSucceeded,
			})
			return ctrl.Result{}, r.Status().Update(ctx, eviction)
		}
		return ctrl.Result{}, err
	}

	log = log.WithValues("server_status", vm.Status)

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
		if err := r.Status().Update(ctx, eviction); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, fmt.Errorf("error migrating instance %v", uuid)
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
			if err := servers.ConfirmResize(ctx, r.computeClient, vm.ID).ExtractErr(); err != nil {
				if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
					log.Info("Instance is gone")
					// Fall-back to beginning, which will clean it out
					return ctrl.Result{RequeueAfter: shortRetryTime}, nil
				}
				// Retry confirm in next reconciliation
				return ctrl.Result{}, err
			}
		}

		// All done
		*instances = (*instances)[:len(*instances)-1]
		return ctrl.Result{}, r.Status().Update(ctx, eviction)
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
		if err2 := r.Status().Update(ctx, eviction); err2 != nil {
			return ctrl.Result{}, fmt.Errorf("could update status due to %w", err2)
		}
	} else if vm.Status == "ACTIVE" || vm.PowerState == 1 {
		log.Info("trigger live-migration")
		if err = r.liveMigrate(ctx, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				// Fall-back to beginning, which will clean it out
				return ctrl.Result{RequeueAfter: shortRetryTime}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeMigration,
				Status:  metav1.ConditionTrue,
				Message: fmt.Sprintf("Live migration of instance %s triggered", vm.ID),
				Reason:  kvmv1.ConditionReasonRunning,
			})
			if err2 := r.Status().Update(ctx, eviction); err2 != nil {
				return ctrl.Result{}, fmt.Errorf("could not live-migrate due to %w and %w", err, err2)
			}

			return ctrl.Result{}, err
		}
	} else {
		log.Info("trigger cold-migration")
		if err := r.coldMigrate(ctx, vm.ID, eviction); err != nil {
			if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Info("Instance is gone")
				return ctrl.Result{RequeueAfter: shortRetryTime}, nil
			}
			copy((*instances)[1:], (*instances)[:len(*instances)-1])
			(*instances)[0] = uuid

			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeMigration,
				Status:  metav1.ConditionTrue,
				Message: fmt.Sprintf("Cold-migration of instance %s triggered", vm.ID),
				Reason:  kvmv1.ConditionReasonRunning,
			})
			if err2 := r.Status().Update(ctx, eviction); err2 != nil {
				return ctrl.Result{}, fmt.Errorf("could not cold-migrate due to %w and %w", err, err2)
			}

			return ctrl.Result{}, err
		}
	}

	// Triggered a migration, give it a generous time to start, so we do not
	// see the old state because the migration didn't start
	log.Info("poll")
	return ctrl.Result{RequeueAfter: defaultPollTime}, err
}

func (r *EvictionReconciler) evictionReason(eviction *kvmv1.Eviction) string {
	return fmt.Sprintf("Eviction %v/%v: %v", eviction.Namespace, eviction.Name, eviction.Spec.Reason)
}

func (r *EvictionReconciler) handleFinalizer(ctx context.Context, eviction *kvmv1.Eviction) error {
	if !controllerutil.ContainsFinalizer(eviction, evictionFinalizerName) {
		return nil
	}

	// As long as we didn't succeed to re-enable the hypervisor, which includes
	// - the hypervisor being gone, because it has been torn down
	// - the hypervisor having been enabled by someone else
	if !meta.IsStatusConditionTrue(eviction.Status.Conditions, kvmv1.ConditionTypeHypervisorReEnabled) {
		err := r.enableHypervisorService(ctx, eviction)
		if err != nil {
			return err
		}
	}

	evictionBase := eviction.DeepCopy()
	controllerutil.RemoveFinalizer(eviction, evictionFinalizerName)
	return r.Patch(ctx, eviction, client.MergeFromWithOptions(evictionBase, client.MergeFromWithOptimisticLock{}))
}

func (r *EvictionReconciler) enableHypervisorService(ctx context.Context, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, eviction.Spec.Hypervisor, false)
	if err != nil {
		if errors.Is(err, openstack.ErrNoHypervisor) {
			changed := meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeHypervisorReEnabled,
				Status:  metav1.ConditionTrue,
				Message: "Hypervisor is gone, no need to re-enable",
				Reason:  kvmv1.ConditionReasonSucceeded,
			})
			if changed {
				return r.Status().Update(ctx, eviction)
			} else {
				return nil
			}
		} else {
			errorMessage := fmt.Sprintf("failed to get hypervisor due to %s", err)
			// update the condition to reflect the error, and retry the reconciliation
			changed := meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeHypervisorReEnabled,
				Status:  metav1.ConditionFalse,
				Message: errorMessage,
				Reason:  kvmv1.ConditionReasonFailed,
			})

			if changed {
				if err2 := r.Status().Update(ctx, eviction); err2 != nil {
					log.Error(err, "failed to store error message in condition", "message", errorMessage)
					return err2
				}
			}

			return ErrRetry
		}
	}

	if hypervisor.Service.DisabledReason != r.evictionReason(eviction) {
		changed := meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorReEnabled,
			Status:  metav1.ConditionTrue,
			Message: "Hypervisor already re-enabled for reason:" + hypervisor.Service.DisabledReason,
			Reason:  kvmv1.ConditionReasonSucceeded,
		})
		if changed {
			return r.Status().Update(ctx, eviction)
		} else {
			return nil
		}
	}

	enableService := services.UpdateOpts{Status: services.ServiceEnabled}
	log.Info("Enabling hypervisor", "id", hypervisor.Service.ID)
	_, err = services.Update(ctx, r.computeClient, hypervisor.Service.ID, enableService).Extract()

	if err != nil {
		errorMessage := fmt.Sprintf("failed to enable hypervisor due to %s", err)
		changed := meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorReEnabled,
			Status:  metav1.ConditionFalse,
			Message: errorMessage,
			Reason:  kvmv1.ConditionReasonFailed,
		})
		if changed {
			if err2 := r.Status().Update(ctx, eviction); err2 != nil {
				log.Error(err, "failed to store error message in condition", "message", errorMessage)
			}
		}
		return ErrRetry
	} else {
		changed := meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorReEnabled,
			Status:  metav1.ConditionTrue,
			Message: "Hypervisor re-enabled successfully",
			Reason:  kvmv1.ConditionReasonSucceeded,
		})
		if changed {
			return r.Status().Update(ctx, eviction)
		} else {
			return nil
		}
	}

}

// disableHypervisor disables the hypervisor service and adds a finalizer to the eviction
// will add Condition HypervisorDisabled to the eviction status with the outcome
func (r *EvictionReconciler) disableHypervisor(ctx context.Context, hypervisor *hypervisors.Hypervisor, eviction *kvmv1.Eviction) error {
	evictionReason := r.evictionReason(eviction)
	disabledReason := hypervisor.Service.DisabledReason

	if disabledReason != "" && disabledReason != evictionReason {
		// Disabled for another reason already
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionTrue,
			Message: fmt.Sprintf("Hypervisor already disabled for reason %q", disabledReason),
			Reason:  kvmv1.ConditionReasonSucceeded,
		})
		return r.Status().Update(ctx, eviction)
	}

	if !controllerutil.ContainsFinalizer(eviction, evictionFinalizerName) {
		evictionBase := eviction.DeepCopy()
		controllerutil.AddFinalizer(eviction, evictionFinalizerName)
		return r.Patch(ctx, eviction, client.MergeFromWithOptions(evictionBase, client.MergeFromWithOptimisticLock{}))
	}

	disableService := services.UpdateOpts{Status: services.ServiceDisabled,
		DisabledReason: r.evictionReason(eviction)}

	_, err := services.Update(ctx, r.computeClient, hypervisor.Service.ID, disableService).Extract()
	if err != nil {
		// We expect OpenStack calls to be transient errors, so we retry for the next reconciliation
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionFalse,
			Message: fmt.Sprintf("Failed to disable hypervisor: %v", err),
			Reason:  kvmv1.ConditionReasonFailed,
		})
		return r.Status().Update(ctx, eviction)
	}

	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeHypervisorDisabled,
		Status:  metav1.ConditionTrue,
		Message: "Hypervisor disabled successfully",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})
	return r.Status().Update(ctx, eviction)
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

// addCondition adds a condition to the Eviction status and updates the status
func (r *EvictionReconciler) addCondition(ctx context.Context, eviction *kvmv1.Eviction,
	status metav1.ConditionStatus, message string, reason string) bool {
	if !meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeEvicting,
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
