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

package eviction

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/global"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

// EvictionReconciler reconciles a Eviction object
type EvictionReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

const (
	EvictionControllerName = "eviction"
)

// candidate is an instance eligible to start migrating this pass, along with
// the migration mode decided from its already-fetched state.
type candidate struct {
	id   string
	live bool
}

// removeInstance returns instances with the first occurrence of uuid removed,
// preserving order. Used when a specific VM completes migration, which - once
// migrations run in parallel - is no longer necessarily the tail of the slice.
func removeInstance(instances []string, uuid string) []string {
	for i, u := range instances {
		if u == uuid {
			return append(instances[:i:i], instances[i+1:]...)
		}
	}
	return instances
}

// deprioritize moves the given instance to the back of the queue (index 0,
// since the queue is processed from the tail). Used to defer an ERROR or
// terminating instance so the eviction retries healthier instances first. If
// the uuid is absent or already at the back, the slice is returned unchanged.
func deprioritize(instances []string, uuid string) []string {
	idx := -1
	for i, u := range instances {
		if u == uuid {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return instances
	}
	// Shift [0:idx] up by one and place uuid at the front (= back of queue).
	copy(instances[1:idx+1], instances[0:idx])
	instances[0] = uuid
	return instances
}

// isMigrationTaskState reports whether a nova task_state indicates a migration
// (live or cold/resize) is already underway. A freshly-triggered migration
// reports the instance as ACTIVE with such a task_state for a few seconds before
// its Status flips to MIGRATING/RESIZE; counting these prevents the eviction
// loop from over-triggering past the concurrency limit during that window.
// Nova values include "migrating", "migrating-start", "resize_prep",
// "resize_migrating", "resize_migrated", "resize_finish".
func isMigrationTaskState(taskState string) bool {
	return strings.Contains(taskState, "migrat") || strings.Contains(taskState, "resize")
}

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
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeEvicting,
			Status:  metav1.ConditionTrue,
			Message: "Running",
			Reason:  kvmv1.ConditionReasonRunning,
		})

		return ctrl.Result{}, r.updateStatus(ctx, eviction)
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
		limit := global.ResolveConcurrency(hypervisor.Status.Traits)
		return r.evictNext(ctx, eviction, limit)
	}

	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeEvicting,
		Status:  metav1.ConditionFalse,
		Message: "eviction completed successfully",
		Reason:  kvmv1.ConditionReasonSucceeded,
	})

	eviction.Status.OutstandingRamMb = 0
	logger.FromContext(ctx).Info("succeeded")
	return ctrl.Result{}, r.updateStatus(ctx, eviction)
}

func (r *EvictionReconciler) updateStatus(ctx context.Context, eviction *kvmv1.Eviction) error {
	desiredStatus := eviction.Status.DeepCopy()
	return retry.RetryOnConflict(utils.StatusPatchBackoff, func() error {
		freshEviction := &kvmv1.Eviction{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(eviction), freshEviction); err != nil {
			return err
		}
		freshBase := freshEviction.DeepCopy()
		// Apply desired conditions and scalar fields onto the fresh status
		for _, c := range desiredStatus.Conditions {
			meta.SetStatusCondition(&freshEviction.Status.Conditions, c)
		}
		freshEviction.Status.OutstandingInstances = desiredStatus.OutstandingInstances
		freshEviction.Status.OutstandingRamMb = desiredStatus.OutstandingRamMb
		freshEviction.Status.HypervisorServiceId = desiredStatus.HypervisorServiceId
		return r.Status().Patch(ctx, freshEviction, client.MergeFromWithOptions(freshBase,
			client.MergeFromWithOptimisticLock{}), client.FieldOwner(EvictionControllerName))
	})
}

func (r *EvictionReconciler) handlePreflight(ctx context.Context, eviction *kvmv1.Eviction, hv *kvmv1.Hypervisor) (ctrl.Result, error) {
	expectHypervisor := hv.Status.HypervisorID != "" && hv.Status.ServiceID != "" // The hypervisor has been registered

	// If the hypervisor should exist, then we need to ensure it is disabled before we start evicting
	if expectHypervisor && !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled) {
		// Hypervisor is not disabled (yet?), reflect that as a failing preflight check
		if meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypePreflight,
			Status:  metav1.ConditionFalse,
			Message: "hypervisor not disabled",
			Reason:  kvmv1.ConditionReasonFailed,
		}) {
			return ctrl.Result{}, r.updateStatus(ctx, eviction)
		}
		return ctrl.Result{RequeueAfter: global.DefaultPollTime}, nil // Wait for hypervisor to be disabled
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
			return ctrl.Result{}, r.updateStatus(ctx, eviction)
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
			return ctrl.Result{}, r.updateStatus(ctx, eviction)
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
	return ctrl.Result{}, r.updateStatus(ctx, eviction)
}

// removeGone drops a specific instance from the outstanding set when it is
// gone (NotFound), recording a success condition. Returns true if the error was
// a NotFound and was handled, false otherwise (caller should treat the original
// error as a real failure).
func (r *EvictionReconciler) removeGone(eviction *kvmv1.Eviction, uuid string, err error) bool {
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return false
	}
	eviction.Status.OutstandingInstances = removeInstance(eviction.Status.OutstandingInstances, uuid)
	r.setMigrationSucceeded(eviction, fmt.Sprintf("Instance %s is gone", uuid))
	return true
}

// evictNext scans the whole outstanding set once and keeps up to `limit`
// migrations in flight. Instances that have already left the host are removed,
// transient (MIGRATING/RESIZE) and terminating instances are counted as busy,
// and fresh migrations are started only while there is spare concurrency. With
// limit == 1 the behavior is equivalent to the historical one-at-a-time drain.
func (r *EvictionReconciler) evictNext(ctx context.Context, eviction *kvmv1.Eviction, limit int) (ctrl.Result, error) {
	if limit < 1 {
		limit = 1
	}
	baseLog := logger.FromContext(ctx).WithName("Evict")

	// Snapshot the current queue; we mutate eviction.Status.OutstandingInstances
	// as instances complete, so iterate over a copy.
	outstanding := append([]string(nil), eviction.Status.OutstandingInstances...)

	inFlight := 0 // migrations currently running (or terminating)
	started := 0  // migrations we triggered this pass
	var candidates []candidate
	var errs []error // errors to surface after the status update

	for _, uuid := range outstanding {
		log := baseLog.WithValues("server", uuid)
		vmCtx := logger.IntoContext(ctx, log)

		vm, err := servers.Get(vmCtx, r.computeClient, uuid).Extract()
		if err != nil {
			if r.removeGone(eviction, uuid, err) {
				continue
			}
			// Transient Get error - leave the instance queued and retry soon.
			errs = append(errs, err)
			continue
		}

		log = log.WithValues("server_status", vm.Status)

		switch vm.Status {
		case "MIGRATING", "RESIZE":
			// Already draining - occupies an in-flight slot.
			inFlight++
			continue
		case "ERROR":
			// Needs manual intervention (or another operator fixes it);
			// deprioritize and record the failure, but don't hold a slot.
			eviction.Status.OutstandingInstances = deprioritize(eviction.Status.OutstandingInstances, uuid)
			log.Info("error", "faultMessage", vm.Fault.Message)
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeMigration,
				Status:  metav1.ConditionFalse,
				Message: fmt.Sprintf("Migration of instance %s failed: %s", vm.ID, vm.Fault.Message),
				Reason:  kvmv1.ConditionReasonFailed,
			})
			errs = append(errs, fmt.Errorf("error migrating instance %v", uuid))
			continue
		}

		// A migration we triggered on a previous pass does not flip Status to
		// MIGRATING/RESIZE immediately - nova first reports the instance as
		// ACTIVE with a migration task_state (e.g. "migrating",
		// "migrating-start", "resize_migrating"/"resize_prep"). Count those as
		// in-flight too, otherwise the slot looks free and we would trigger a
		// second migration, exceeding the concurrency limit.
		if isMigrationTaskState(vm.TaskState) {
			inFlight++
			continue
		}

		currentHypervisor, _, _ := strings.Cut(vm.HypervisorHostname, ".")
		if currentHypervisor != eviction.Spec.Hypervisor {
			// It has left this host. Confirm a pending resize first, otherwise
			// consider it done and drop it from the outstanding set.
			if vm.Status == "VERIFY_RESIZE" {
				log.Info("confirm-resize")
				err := servers.ConfirmResize(vmCtx, r.computeClient, vm.ID).ExtractErr()
				if err != nil && !r.removeGone(eviction, uuid, err) {
					// Retry confirm on the next pass.
					errs = append(errs, err)
				}
				// Whether confirmed now or gone, treat as busy this pass so we
				// re-check it next time before declaring completion.
				inFlight++
				continue
			}

			log.Info("migrated")
			eviction.Status.OutstandingInstances = removeInstance(eviction.Status.OutstandingInstances, uuid)
			r.setMigrationSucceeded(eviction, fmt.Sprintf("Migration of instance %s finished", vm.ID))
			continue
		}

		if vm.TaskState == "deleting" {
			// Just wait for it to disappear; deprioritize and count as busy so
			// we don't spend a fresh slot on it.
			eviction.Status.OutstandingInstances = deprioritize(eviction.Status.OutstandingInstances, uuid)
			meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeMigration,
				Status:  metav1.ConditionFalse,
				Message: fmt.Sprintf("Live migration of terminating instance %s skipped", vm.ID),
				Reason:  kvmv1.ConditionReasonFailed,
			})
			inFlight++
			continue
		}

		// Otherwise it is a candidate to start migrating this pass. Decide the
		// migration mode now from the state we already fetched.
		candidates = append(candidates, candidate{
			id:   vm.ID,
			live: vm.Status == "ACTIVE" || vm.PowerState == 1,
		})
	}

	// Start fresh migrations until we hit the concurrency limit.
	for _, c := range candidates {
		if inFlight+started >= limit {
			break
		}
		log := baseLog.WithValues("server", c.id)
		migrateCtx := logger.IntoContext(ctx, log)

		var migErr error
		if c.live {
			log.Info("trigger live-migration")
			migErr = r.liveMigrate(migrateCtx, c.id, eviction)
		} else {
			log.Info("trigger cold-migration")
			migErr = r.coldMigrate(migrateCtx, c.id, eviction)
		}
		if migErr != nil {
			if r.removeGone(eviction, c.id, migErr) {
				continue
			}
			errs = append(errs, migErr)
			continue
		}
		started++
	}

	// Persisting the status is the only genuinely fatal error here: if it fails
	// we must return it (with no result) so controller-runtime retries. Per-VM
	// problems (ERROR-state instances, failed migrate triggers, transient Gets)
	// are expected and recoverable - they are recorded on the MigratingInstance
	// condition and retried via RequeueAfter, so returning them as a reconcile
	// error would only discard our RequeueAfter (controller-runtime ignores the
	// result when the error is non-nil) and emit a warning.
	if err := r.updateStatus(ctx, eviction); err != nil {
		return ctrl.Result{}, err
	}

	if joined := errors.Join(errs...); joined != nil {
		baseLog.Info("instances need attention this pass; will retry", "err", joined.Error())
	}

	baseLog.Info("poll", "inFlight", inFlight, "started", started,
		"outstanding", len(eviction.Status.OutstandingInstances), "limit", limit)

	// Requeue while there is still work; use a short retry when nothing is
	// actually migrating yet (e.g. all instances errored) so we don't idle.
	requeue := global.DefaultPollTime
	if inFlight+started == 0 && len(eviction.Status.OutstandingInstances) > 0 {
		requeue = global.ShortRetryTime
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// setMigrationSucceeded records a successful migration (with the given message)
// without clobbering a sticky Failed condition while other instances are still
// outstanding - an earlier ERROR instance moved to the back of the queue means
// the eviction is not actually clean yet. The condition is only allowed to flip
// to Succeeded once the whole eviction completes (OutstandingInstances empty).
func (r *EvictionReconciler) setMigrationSucceeded(eviction *kvmv1.Eviction, msg string) {
	prior := meta.FindStatusCondition(eviction.Status.Conditions, kvmv1.ConditionTypeMigration)
	stickyFailure := len(eviction.Status.OutstandingInstances) > 0 && prior != nil &&
		prior.Status == metav1.ConditionFalse &&
		prior.Reason == kvmv1.ConditionReasonFailed
	if stickyFailure {
		return
	}
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeMigration,
		Status:  metav1.ConditionFalse,
		Message: msg,
		Reason:  kvmv1.ConditionReasonSucceeded,
	})
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

	log.Info("Live migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header.Get("X-Openstack-Request-Id"))
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

	log.Info("Cold-migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header.Get("X-Openstack-Request-Id"))
	return nil
}

// registerWithManager registers the controller with the Manager without acquiring OpenStack clients.
// This is useful for testing where clients are injected directly.
func (r *EvictionReconciler) registerWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(EvictionControllerName).
		For(&kvmv1.Eviction{}).
		Complete(r)
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.90" // Xena (or later)

	return r.registerWithManager(mgr)
}
