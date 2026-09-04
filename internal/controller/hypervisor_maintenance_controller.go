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

// This controller only takes care of enabling or disabling the compute
// service depending on the hypervisor spec Maintenance field

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	HypervisorMaintenanceControllerName = "HypervisorMaintenance"

	// settleRequeueInterval is the interval at which the controller re-checks
	// incoming migrations when they haven't settled yet.
	settleRequeueInterval = 10 * time.Second
)

type HypervisorMaintenanceController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
func (hec *HypervisorMaintenanceController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hv := &kvmv1.Hypervisor{}
	if err := hec.Get(ctx, req.NamespacedName, hv); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// If onboarding hasn't even started, no value will be set
	// If it has been started, but not finished yet, we need to wait for it to be aborted
	// So we can continue, if the condition is either not set at all or false
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding) {
		return ctrl.Result{}, nil
	}

	// Build status apply config upfront; sub-functions mutate it directly.
	// Seed only the HypervisorDisabled condition (the only one always retained).
	// reconcileEviction conditionally seeds ConditionTypeEvicting and
	// ConditionTypeIncomingMigrationsSettled: they are included when maintenance
	// is active, and intentionally omitted when MaintenanceUnset so that SSA
	// prunes them from the field manager's managed fields.
	statusCfg := apiv1.HypervisorStatus().WithEvicted(hv.Status.Evicted)
	if c := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled); c != nil {
		statusCfg.WithConditions(utils.ConditionFromStatus(*c))
	}

	if err := hec.reconcileComputeService(ctx, hv, statusCfg); err != nil {
		return ctrl.Result{}, err
	}

	result, err := hec.reconcileEviction(ctx, hv, statusCfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	applyErr := hec.Status().Apply(ctx,
		apiv1.Hypervisor(hv.Name).WithStatus(statusCfg),
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorMaintenanceControllerName))
	return result, applyErr
}

// reconcileComputeService enables/disables the nova-compute service based on
// hv.Spec.Maintenance and sets the HypervisorDisabled condition on statusCfg.
func (hec *HypervisorMaintenanceController) reconcileComputeService(ctx context.Context, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration) error {
	log := logger.FromContext(ctx)
	serviceId := hv.Status.ServiceID

	if serviceId == "" {
		// We can only do something here, if there is a service to begin with.
		// The onboarding should take care of that.
		return nil
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		existing := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
		if existing == nil || existing.Status != metav1.ConditionFalse {
			// We need to enable the host as per spec.
			// Also clear forced_down in case a previous HA event set it.
			falseVal := false
			enableService := openstack.UpdateServiceOpts{
				Status:     services.ServiceEnabled,
				ForcedDown: &falseVal,
			}
			log.Info("Enabling hypervisor", "id", serviceId)
			if _, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract(); err != nil {
				return fmt.Errorf("failed to enable hypervisor due to %w", err)
			}
		}
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeHypervisorDisabled).
				WithStatus(metav1.ConditionFalse).
				WithMessage("Hypervisor is enabled").
				WithReason(kvmv1.ConditionReasonSucceeded))

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceTermination:
		// Disable the compute service.
		existing := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
		if existing == nil || existing.Status != metav1.ConditionTrue {
			disableService := services.UpdateOpts{
				Status:         services.ServiceDisabled,
				DisabledReason: "Hypervisor CRD: spec.maintenance=" + hv.Spec.Maintenance,
			}
			// We need to disable the host as per spec
			log.Info("Disabling hypervisor", "id", serviceId)
			if _, err := services.Update(ctx, hec.computeClient, serviceId, disableService).Extract(); err != nil {
				return fmt.Errorf("failed to disable hypervisor due to %w", err)
			}
		}
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeHypervisorDisabled).
				WithStatus(metav1.ConditionTrue).
				WithMessage("Hypervisor is disabled").
				WithReason(kvmv1.ConditionReasonSucceeded))
	}

	return nil
}

// reconcileEviction creates/deletes the Eviction CR and sets the ConditionTypeEvicting
// and ConditionTypeIncomingMigrationsSettled conditions plus the Evicted scalar on
// statusCfg. When eviction should be removed, the condition entries are filtered out
// so SSA prunes them.
func (hec *HypervisorMaintenanceController) reconcileEviction(ctx context.Context, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration) (ctrl.Result, error) {
	eviction := &kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: hv.Name},
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		// Avoid deleting the eviction over and over.
		if !hv.Status.Evicted &&
			meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting) == nil &&
			meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeIncomingMigrationsSettled) == nil {
			return ctrl.Result{}, nil
		}
		if err := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction)); err != nil {
			return ctrl.Result{}, err
		}
		// ConditionTypeEvicting and ConditionTypeIncomingMigrationsSettled are
		// intentionally absent from statusCfg — SSA will prune them from this
		// field manager's managed fields on Apply.
		statusCfg.WithEvicted(false)

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceTermination:
		// Gate: ensure all incoming migrations are settled before proceeding.
		// This runs on every reconcile (defense in depth).
		settled, err := hec.settleIncomingMigrations(ctx, hv, statusCfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !settled {
			// Cannot proceed with eviction while incoming migrations are outstanding.
			// Seed the existing evicting condition so SSA does not prune it.
			if cond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting); cond != nil {
				statusCfg.WithConditions(utils.ConditionFromStatus(*cond))
			}
			return ctrl.Result{RequeueAfter: settleRequeueInterval}, nil
		}

		// Terminal guard: if Evicting was already Succeeded, verify the host is truly empty.
		if cond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting); cond != nil {
			if cond.Reason == kvmv1.ConditionReasonSucceeded {
				if hv.Status.NumInstances > 0 {
					// Instance appeared after eviction completed.
					// Delete the Eviction CR so ensureEviction recreates a fresh one.
					log := logger.FromContext(ctx)
					log.Info("Eviction reported succeeded but instances remain on host; re-entering drain",
						"numInstances", hv.Status.NumInstances)
					if delErr := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction)); delErr != nil {
						return ctrl.Result{}, delErr
					}
					statusCfg.WithEvicted(false)
					utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
						*k8sacmetav1.Condition().
							WithType(kvmv1.ConditionTypeEvicting).
							WithStatus(metav1.ConditionTrue).
							WithReason(kvmv1.ConditionReasonRunning).
							WithMessage(fmt.Sprintf("Re-entering drain: %d instance(s) still on host", hv.Status.NumInstances)))
					return ctrl.Result{}, nil
				}
				// Truly done: host is empty and migrations are settled.
				statusCfg.WithConditions(utils.ConditionFromStatus(*cond))
				return ctrl.Result{}, nil
			}
		}

		status, err := hec.ensureEviction(ctx, eviction, hv)
		if err != nil {
			return ctrl.Result{}, err
		}

		var reason, message string
		if status == metav1.ConditionFalse {
			if hv.Status.NumInstances > 0 {
				// Eviction CR says done but instances exist (race with in-flight migration).
				// Delete the Eviction CR and restart drain.
				log := logger.FromContext(ctx)
				log.Info("Eviction finished but instances remain on host; restarting drain",
					"numInstances", hv.Status.NumInstances)
				if delErr := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction)); delErr != nil {
					return ctrl.Result{}, delErr
				}
				statusCfg.WithEvicted(false)
				utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
					*k8sacmetav1.Condition().
						WithType(kvmv1.ConditionTypeEvicting).
						WithStatus(metav1.ConditionTrue).
						WithReason(kvmv1.ConditionReasonRunning).
						WithMessage(fmt.Sprintf("Restarting drain: %d instance(s) still on host", hv.Status.NumInstances)))
				return ctrl.Result{}, nil
			}
			message = "Evicted"
			reason = kvmv1.ConditionReasonSucceeded
			statusCfg.WithEvicted(true)
		} else {
			message = "Evicting"
			reason = kvmv1.ConditionReasonRunning
			statusCfg.WithEvicted(false)
		}

		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeEvicting).
				WithStatus(status).
				WithReason(reason).
				WithMessage(message))
	}

	return ctrl.Result{}, nil
}

// settleIncomingMigrations checks for in-flight migrations targeting this host,
// aborts those that can be aborted, and reports whether the host is settled.
// It sets the IncomingMigrationsSettled condition on statusCfg.
// Returns true if settled (no incoming migrations), false otherwise.
func (hec *HypervisorMaintenanceController) settleIncomingMigrations(ctx context.Context, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration) (bool, error) {
	log := logger.FromContext(ctx)

	aborted, waiting, err := openstack.SettleIncomingMigrations(ctx, hec.computeClient, hv.Name)
	if err != nil {
		return false, fmt.Errorf("settling incoming migrations for %s: %w", hv.Name, err)
	}

	if len(waiting) > 0 {
		instanceUUIDs := make([]string, len(waiting))
		for i, m := range waiting {
			instanceUUIDs[i] = m.InstanceUUID
		}
		log.Info("Waiting for non-abortable incoming migrations", "host", hv.Name, "instances", instanceUUIDs)
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeIncomingMigrationsSettled).
				WithStatus(metav1.ConditionFalse).
				WithReason(kvmv1.ConditionReasonWaiting).
				WithMessage(fmt.Sprintf("Waiting for %d non-abortable incoming migration(s) to complete", len(waiting))))
		return false, nil
	}

	if len(aborted) > 0 {
		instanceUUIDs := make([]string, len(aborted))
		for i, m := range aborted {
			instanceUUIDs[i] = m.InstanceUUID
		}
		log.Info("Aborted incoming migrations", "host", hv.Name, "instances", instanceUUIDs)
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeIncomingMigrationsSettled).
				WithStatus(metav1.ConditionFalse).
				WithReason(kvmv1.ConditionReasonAborting).
				WithMessage(fmt.Sprintf("Aborted %d incoming migration(s); verifying on next reconcile", len(aborted))))
		return false, nil
	}

	// No incoming migrations — settled.
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeIncomingMigrationsSettled).
			WithStatus(metav1.ConditionTrue).
			WithReason(kvmv1.ConditionReasonSettled).
			WithMessage("No incoming migrations targeting this host"))
	return true, nil
}

func (hec *HypervisorMaintenanceController) ensureEviction(ctx context.Context, eviction *kvmv1.Eviction, hypervisor *kvmv1.Hypervisor) (metav1.ConditionStatus, error) {
	log := logger.FromContext(ctx)

	// Build labels to transport from hypervisor (e.g. label-selector, if set)
	evictionLabels := make(map[string]string)
	for _, label := range transferLabels {
		if v, ok := hypervisor.Labels[label]; ok {
			evictionLabels[label] = v
		}
	}

	ownerRef := k8sacmetav1.OwnerReference().
		WithAPIVersion(kvmv1.GroupVersion.String()).
		WithKind("Hypervisor").
		WithName(hypervisor.Name).
		WithUID(hypervisor.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)

	evictionApplyCfg := apiv1.Eviction(eviction.Name).
		WithLabels(evictionLabels).
		WithOwnerReferences(ownerRef).
		WithSpec(apiv1.EvictionSpec().
			WithHypervisor(hypervisor.Name).
			WithReason("openstack-hypervisor-operator maintenance"))

	log.Info("Applying eviction", "name", eviction.Name)
	if err := hec.Apply(ctx, evictionApplyCfg,
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorMaintenanceControllerName)); err != nil {
		return metav1.ConditionUnknown, fmt.Errorf("failed to apply eviction due to %w", err)
	}

	// Re-fetch to read current eviction status
	if err := hec.Get(ctx, k8sclient.ObjectKeyFromObject(eviction), eviction); err != nil {
		return metav1.ConditionUnknown, fmt.Errorf("failed to get eviction status due to %w", err)
	}

	// check if we are still evicting (defaulting to yes)
	if meta.IsStatusConditionFalse(eviction.Status.Conditions, kvmv1.ConditionTypeEvicting) {
		return metav1.ConditionFalse, nil
	}
	return metav1.ConditionTrue, nil
}

// registerWithManager registers the controller with the Manager without acquiring OpenStack clients.
// This is useful for testing where clients are injected directly.
func (hec *HypervisorMaintenanceController) registerWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorMaintenanceControllerName).
		For(&kvmv1.Hypervisor{}).
		Owns(&kvmv1.Eviction{}). // trigger Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(hec)
}

// SetupWithManager sets up the controller with the Manager.
func (hec *HypervisorMaintenanceController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if hec.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	hec.computeClient.Microversion = "2.90" // Xena (or later)

	return hec.registerWithManager(mgr)
}
