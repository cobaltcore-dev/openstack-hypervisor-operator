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
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
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

// evictionStatusCfg builds an EvictionStatusApplyConfiguration pre-populated
// with the current scalar fields and the conditions this controller owns.
// Call sites upsert the single condition they are setting via
// utils.SetApplyConfigurationStatusCondition, then apply.
func evictionStatusCfg(eviction *kvmv1.Eviction) *apiv1.EvictionStatusApplyConfiguration {
	cfg := apiv1.EvictionStatus().
		WithHypervisorServiceId(eviction.Status.HypervisorServiceId).
		WithOutstandingRamMb(eviction.Status.OutstandingRamMb)
	cfg.OutstandingInstances = eviction.Status.OutstandingInstances
	// Seed all conditions this controller owns so they are not pruned on Apply.
	for _, t := range []string{kvmv1.ConditionTypeEvicting, kvmv1.ConditionTypePreflight, kvmv1.ConditionTypeMigration} {
		if c := meta.FindStatusCondition(eviction.Status.Conditions, t); c != nil {
			cfg.WithConditions(utils.ConditionFromStatus(*c))
		}
	}
	return cfg
}

// applyStatus applies the eviction status via SSA with this controller as the
// field owner.
func (r *EvictionReconciler) applyStatus(ctx context.Context, name string, cfg *apiv1.EvictionStatusApplyConfiguration) error {
	return r.Status().Apply(ctx,
		apiv1.Eviction(name).WithStatus(cfg),
		client.ForceOwnership, client.FieldOwner(EvictionControllerName))
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
		statusCfg := evictionStatusCfg(eviction)
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeEvicting).
				WithStatus(metav1.ConditionTrue).
				WithReason(kvmv1.ConditionReasonRunning).
				WithMessage("Running"))
		return ctrl.Result{}, r.applyStatus(ctx, eviction.Name, statusCfg)
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

	statusCfg := evictionStatusCfg(eviction)
	statusCfg.WithOutstandingRamMb(0)
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeEvicting).
			WithStatus(metav1.ConditionFalse).
			WithReason(kvmv1.ConditionReasonSucceeded).
			WithMessage("eviction completed successfully"))
	logger.FromContext(ctx).Info("succeeded")
	return ctrl.Result{}, r.applyStatus(ctx, eviction.Name, statusCfg)
}

func (r *EvictionReconciler) handlePreflight(ctx context.Context, eviction *kvmv1.Eviction, hv *kvmv1.Hypervisor) (ctrl.Result, error) {
	expectHypervisor := hv.Status.HypervisorID != "" && hv.Status.ServiceID != "" // The hypervisor has been registered

	// If the hypervisor should exist, then we need to ensure it is disabled before we start evicting
	if expectHypervisor && !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled) {
		// Hypervisor is not disabled (yet?), reflect that as a failing preflight check.
		// Only apply if the condition is new or changed, to avoid unnecessary API calls on repeated requeues.
		existing := meta.FindStatusCondition(eviction.Status.Conditions, kvmv1.ConditionTypePreflight)
		if existing == nil || existing.Status != metav1.ConditionFalse {
			statusCfg := evictionStatusCfg(eviction)
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypePreflight).
					WithStatus(metav1.ConditionFalse).
					WithReason(kvmv1.ConditionReasonFailed).
					WithMessage("hypervisor not disabled"))
			if err := r.applyStatus(ctx, eviction.Name, statusCfg); err != nil {
				return ctrl.Result{}, err
			}
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
			statusCfg := evictionStatusCfg(eviction)
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeEvicting).
					WithStatus(metav1.ConditionFalse).
					WithReason(kvmv1.ConditionReasonFailed).
					WithMessage(fmt.Sprintf("failed to get hypervisor %v", err)))
			return ctrl.Result{}, r.applyStatus(ctx, eviction.Name, statusCfg)
		}

		// That is (likely) an eviction for a node that never registered
		// so we are good to go
		msg := "eviction completed successfully due to expected case of no hypervisor"
		statusCfg := evictionStatusCfg(eviction)
		statusCfg.WithOutstandingRamMb(0)
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeEvicting).
				WithStatus(metav1.ConditionFalse).
				WithReason(kvmv1.ConditionReasonSucceeded).
				WithMessage(msg))
		logger.FromContext(ctx).Info(msg)
		return ctrl.Result{}, r.applyStatus(ctx, eviction.Name, statusCfg)
	}

	var instances []string
	if hypervisor.Servers != nil {
		instances = make([]string, len(*hypervisor.Servers))
		for i, server := range *hypervisor.Servers {
			instances[i] = server.UUID
		}
	}

	statusCfg := evictionStatusCfg(eviction)
	statusCfg.WithHypervisorServiceId(hypervisor.ID)
	statusCfg.WithOutstandingRamMb(int64(hypervisor.MemoryMBUsed))
	statusCfg.OutstandingInstances = instances
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypePreflight).
			WithStatus(metav1.ConditionTrue).
			WithReason(kvmv1.ConditionReasonSucceeded).
			WithMessage("Preflight checks passed, hypervisor is disabled and ready for eviction"))
	return ctrl.Result{}, r.applyStatus(ctx, eviction.Name, statusCfg)
}

// removeGone drops a specific instance from the outstanding set when it is
// gone (NotFound), recording a success condition. Returns true if the error was
// a NotFound and was handled, false otherwise (caller should treat the original
// error as a real failure).
func (r *EvictionReconciler) removeGone(statusCfg *apiv1.EvictionStatusApplyConfiguration, uuid string, err error) bool {
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return false
	}
	statusCfg.OutstandingInstances = removeInstance(statusCfg.OutstandingInstances, uuid)
	r.setMigrationSucceeded(statusCfg, fmt.Sprintf("Instance %s is gone", uuid))
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

	// Snapshot the current queue; we mutate statusCfg.OutstandingInstances as
	// instances complete, so iterate over a copy.
	outstanding := append([]string(nil), eviction.Status.OutstandingInstances...)

	statusCfg := evictionStatusCfg(eviction)

	inFlight := 0 // migrations currently running (or terminating)
	started := 0  // migrations we triggered this pass
	var candidates []candidate
	var errs []error // errors to surface after the status update

	for _, uuid := range outstanding {
		if inFlight >= limit {
			break
		}
		log := baseLog.WithValues("server", uuid)
		vmCtx := logger.IntoContext(ctx, log)

		vm, err := servers.Get(vmCtx, r.computeClient, uuid).Extract()
		if err != nil {
			if r.removeGone(statusCfg, uuid, err) {
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
			statusCfg.OutstandingInstances = deprioritize(statusCfg.OutstandingInstances, uuid)
			log.Info("error", "faultMessage", vm.Fault.Message)
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeMigration).
					WithStatus(metav1.ConditionFalse).
					WithReason(kvmv1.ConditionReasonFailed).
					WithMessage(fmt.Sprintf("Migration of instance %s failed: %s", vm.ID, vm.Fault.Message)))
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
				if err != nil && !r.removeGone(statusCfg, uuid, err) {
					// Retry confirm on the next pass.
					errs = append(errs, err)
				}
				// Whether confirmed now or gone, treat as busy this pass so we
				// re-check it next time before declaring completion.
				inFlight++
				continue
			}

			log.Info("migrated")
			statusCfg.OutstandingInstances = removeInstance(statusCfg.OutstandingInstances, uuid)
			r.setMigrationSucceeded(statusCfg, fmt.Sprintf("Migration of instance %s finished", vm.ID))
			continue
		}

		if vm.TaskState == "deleting" {
			// Just wait for it to disappear; deprioritize and count as busy so
			// we don't spend a fresh slot on it.
			statusCfg.OutstandingInstances = deprioritize(statusCfg.OutstandingInstances, uuid)
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeMigration).
					WithStatus(metav1.ConditionFalse).
					WithReason(kvmv1.ConditionReasonFailed).
					WithMessage(fmt.Sprintf("Live migration of terminating instance %s skipped", vm.ID)))
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
			if r.removeGone(statusCfg, c.id, migErr) {
				continue
			}
			// A migration trigger failed for a reason other than the instance
			// being gone; record the failure on the MigratingInstance condition
			// so it is visible even though we keep retrying.
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeMigration).
					WithStatus(metav1.ConditionFalse).
					WithReason(kvmv1.ConditionReasonFailed).
					WithMessage(migErr.Error()))
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
	if err := r.applyStatus(ctx, eviction.Name, statusCfg); err != nil {
		return ctrl.Result{}, err
	}

	if joined := errors.Join(errs...); joined != nil {
		baseLog.Info("instances need attention this pass; will retry", "err", joined.Error())
	}

	baseLog.Info("poll", "inFlight", inFlight, "started", started,
		"outstanding", len(statusCfg.OutstandingInstances), "limit", limit)

	// Requeue while there is still work; use a short retry when nothing is
	// actually migrating yet (e.g. all instances errored) so we don't idle.
	requeue := global.DefaultPollTime
	if inFlight+started == 0 && len(statusCfg.OutstandingInstances) > 0 {
		requeue = global.ShortRetryTime
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// setMigrationSucceeded records a successful migration (with the given message)
// without clobbering a sticky Failed condition while other instances are still
// outstanding - an earlier ERROR instance moved to the back of the queue means
// the eviction is not actually clean yet. The condition is only allowed to flip
// to Succeeded once the whole eviction completes (OutstandingInstances empty).
func (r *EvictionReconciler) setMigrationSucceeded(statusCfg *apiv1.EvictionStatusApplyConfiguration, msg string) {
	if len(statusCfg.OutstandingInstances) > 0 {
		for _, c := range statusCfg.Conditions {
			if c.Type != nil && *c.Type == kvmv1.ConditionTypeMigration &&
				c.Status != nil && *c.Status == metav1.ConditionFalse &&
				c.Reason != nil && *c.Reason == kvmv1.ConditionReasonFailed {
				return
			}
		}
	}
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeMigration).
			WithStatus(metav1.ConditionFalse).
			WithReason(kvmv1.ConditionReasonSucceeded).
			WithMessage(msg))
}

func (r *EvictionReconciler) liveMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: &[]bool{false}[0],
	}

	res := servers.LiveMigrate(ctx, r.computeClient, uuid, liveMigrateOpts)
	if res.Err != nil {
		return fmt.Errorf("failed to evict VM %s due to %w", uuid, res.Err)
	}

	log.Info("Live migrating server", "server", uuid, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header.Get("X-Openstack-Request-Id"))
	return nil
}

func (r *EvictionReconciler) coldMigrate(ctx context.Context, uuid string, eviction *kvmv1.Eviction) error {
	log := logger.FromContext(ctx)

	res := servers.Migrate(ctx, r.computeClient, uuid)
	if res.Err != nil {
		return fmt.Errorf("failed to evict stopped server %s due to %w", uuid, res.Err)
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
