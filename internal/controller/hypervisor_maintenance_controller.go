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
	statusCfg := apiv1.HypervisorStatus().WithEvicted(hv.Status.Evicted)
	statusCfg.Conditions = utils.ConditionsFromStatus(hv.Status.Conditions)

	if err := hec.reconcileComputeService(ctx, hv, statusCfg); err != nil {
		return ctrl.Result{}, err
	}

	if err := hec.reconcileEviction(ctx, hv, statusCfg); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, hec.Status().Apply(ctx,
		apiv1.Hypervisor(hv.Name, "").WithStatus(statusCfg),
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorMaintenanceControllerName))
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
			// We need to enable the host as per spec
			enableService := services.UpdateOpts{Status: services.ServiceEnabled}
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

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceHA, kvmv1.MaintenanceTermination:
		// Disable the compute service.
		// Also in case of HA, as it doesn't hurt to disable it twice, and this
		// allows us to enable the service again, when the maintenance field is
		// cleared in the case above.
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
// condition and Evicted scalar on statusCfg. When eviction should be removed, the
// condition entry is filtered out so SSA prunes it.
func (hec *HypervisorMaintenanceController) reconcileEviction(ctx context.Context, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration) error {
	eviction := &kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: hv.Name},
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		// Avoid deleting the eviction over and over.
		if !hv.Status.Evicted && meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting) == nil {
			return nil
		}
		if err := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction)); err != nil {
			return err
		}
		// Remove ConditionTypeEvicting by omitting it — SSA prunes sole-owned entries.
		filtered := statusCfg.Conditions[:0]
		for _, c := range statusCfg.Conditions {
			if c.Type == nil || *c.Type != kvmv1.ConditionTypeEvicting {
				filtered = append(filtered, c)
			}
		}
		statusCfg.Conditions = filtered
		statusCfg.WithEvicted(false)

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceTermination:
		// In case of "ha", the host gets emptied from the HA service
		if cond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting); cond != nil {
			if cond.Reason == kvmv1.ConditionReasonSucceeded {
				// We are done here, no need to look at the eviction any more
				return nil
			}
		}

		status, err := hec.ensureEviction(ctx, eviction, hv)
		if err != nil {
			return err
		}

		var reason, message string
		if status == metav1.ConditionFalse {
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

	return nil
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

	evictionApplyCfg := apiv1.Eviction(eviction.Name, eviction.Namespace).
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

// SetupWithManager sets up the controller with the Manager.
func (hec *HypervisorMaintenanceController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if hec.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	hec.computeClient.Microversion = "2.90" // Xena (or later)

	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorMaintenanceControllerName).
		For(&kvmv1.Hypervisor{}).
		Owns(&kvmv1.Eviction{}).
		Complete(hec)
}
