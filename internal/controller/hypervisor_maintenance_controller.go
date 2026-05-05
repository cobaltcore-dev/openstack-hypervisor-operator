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

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

	// Determine desired disabled condition and eviction state
	disabledCond, evictingCond, evicted, err := hec.reconcileComputeService(ctx, hv)
	if err != nil {
		return ctrl.Result{}, err
	}

	evictingCond, evicted, err = hec.reconcileEviction(ctx, hv, evictingCond, evicted)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Build status apply config: always include conditions this controller owns;
	// omit ConditionTypeEvicting when it should be removed (SSA prunes it).
	statusCfg := apiv1.HypervisorStatus().WithEvicted(evicted)
	statusCfg.Conditions = utils.ConditionsFromStatus(hv.Status.Conditions)

	if disabledCond != nil {
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions, *disabledCond)
	}

	if evictingCond != nil {
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions, *evictingCond)
	} else {
		// Remove ConditionTypeEvicting by omitting it — SSA prunes sole-owned entries.
		filtered := statusCfg.Conditions[:0]
		for _, c := range statusCfg.Conditions {
			if c.Type == nil || *c.Type != kvmv1.ConditionTypeEvicting {
				filtered = append(filtered, c)
			}
		}
		statusCfg.Conditions = filtered
	}

	return ctrl.Result{}, hec.Status().Apply(ctx,
		apiv1.Hypervisor(hv.Name, "").WithStatus(statusCfg),
		k8sclient.ForceOwnership, k8sclient.FieldOwner(HypervisorMaintenanceControllerName))
}

// reconcileComputeService enables/disables the nova-compute service based on
// hv.Spec.Maintenance. Returns the desired HypervisorDisabled condition (nil if
// service ID is unset) and the initial evicting/evicted state.
func (hec *HypervisorMaintenanceController) reconcileComputeService(ctx context.Context, hv *kvmv1.Hypervisor) (
	disabledCond *k8sacmetav1.ConditionApplyConfiguration,
	evictingCond *k8sacmetav1.ConditionApplyConfiguration,
	evicted bool,
	err error,
) {
	log := logger.FromContext(ctx)
	serviceId := hv.Status.ServiceID

	if serviceId == "" {
		// We can only do something here, if there is a service to begin with.
		// The onboarding should take care of that.
		return nil, nil, false, nil
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		cond := k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeHypervisorDisabled).
			WithStatus(metav1.ConditionFalse).
			WithMessage("Hypervisor is enabled").
			WithReason(kvmv1.ConditionReasonSucceeded)

		existing := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
		if existing != nil && existing.Status == metav1.ConditionFalse {
			// Already enabled, nothing to do
			return cond, nil, false, nil
		}

		enableService := services.UpdateOpts{Status: services.ServiceEnabled}
		// We need to enable the host as per spec
		log.Info("Enabling hypervisor", "id", serviceId)
		if _, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract(); err != nil {
			return nil, nil, false, fmt.Errorf("failed to enable hypervisor due to %w", err)
		}
		return cond, nil, false, nil

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceHA, kvmv1.MaintenanceTermination:
		// Disable the compute service.
		// Also in case of HA, as it doesn't hurt to disable it twice, and this
		// allows us to enable the service again, when the maintenance field is
		// cleared in the case above.
		cond := k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeHypervisorDisabled).
			WithStatus(metav1.ConditionTrue).
			WithMessage("Hypervisor is disabled").
			WithReason(kvmv1.ConditionReasonSucceeded)

		existing := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeHypervisorDisabled)
		if existing != nil && existing.Status == metav1.ConditionTrue {
			// Already disabled, nothing to do
			return cond, nil, false, nil
		}

		disableService := services.UpdateOpts{
			Status:         services.ServiceDisabled,
			DisabledReason: "Hypervisor CRD: spec.maintenance=" + hv.Spec.Maintenance,
		}
		// We need to disable the host as per spec
		log.Info("Disabling hypervisor", "id", serviceId)
		if _, err := services.Update(ctx, hec.computeClient, serviceId, disableService).Extract(); err != nil {
			return nil, nil, false, fmt.Errorf("failed to disable hypervisor due to %w", err)
		}
		return cond, nil, false, nil
	}

	return nil, nil, false, nil
}

// reconcileEviction creates/deletes the Eviction CR and returns the desired
// ConditionTypeEvicting apply configuration (nil means remove the condition).
func (hec *HypervisorMaintenanceController) reconcileEviction(
	ctx context.Context,
	hv *kvmv1.Hypervisor,
	evictingCond *k8sacmetav1.ConditionApplyConfiguration,
	evicted bool,
) (*k8sacmetav1.ConditionApplyConfiguration, bool, error) {
	eviction := &kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: hv.Name},
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		// Avoid deleting the eviction over and over.
		if !hv.Status.Evicted && meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting) == nil {
			return nil, false, nil
		}
		err := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction))
		return nil, false, err // nil evictingCond → condition omitted from apply → SSA prunes it

	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceTermination:
		// In case of "ha", the host gets emptied from the HA service
		if cond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeEvicting); cond != nil {
			if cond.Reason == kvmv1.ConditionReasonSucceeded {
				// We are done here, no need to look at the eviction any more
				return evictingCond, evicted, nil
			}
		}

		status, err := hec.ensureEviction(ctx, eviction, hv)
		if err != nil {
			return evictingCond, evicted, err
		}

		var reason, message string
		if status == metav1.ConditionFalse {
			message = "Evicted"
			reason = kvmv1.ConditionReasonSucceeded
			evicted = true
		} else {
			message = "Evicting"
			reason = kvmv1.ConditionReasonRunning
			evicted = false
		}

		cond := k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeEvicting).
			WithStatus(status).
			WithReason(reason).
			WithMessage(message)
		return cond, evicted, nil
	}

	return evictingCond, evicted, nil
}

func (hec *HypervisorMaintenanceController) ensureEviction(ctx context.Context, eviction *kvmv1.Eviction, hypervisor *kvmv1.Hypervisor) (metav1.ConditionStatus, error) {
	log := logger.FromContext(ctx)
	if err := hec.Get(ctx, k8sclient.ObjectKeyFromObject(eviction), eviction); err != nil {
		if !k8serrors.IsNotFound(err) {
			return metav1.ConditionUnknown, fmt.Errorf("failed to get eviction due to %w", err)
		}
		if err := controllerutil.SetControllerReference(hypervisor, eviction, hec.Scheme); err != nil {
			return metav1.ConditionUnknown, err
		}
		log.Info("Creating new eviction", "name", eviction.Name)
		eviction.Spec = kvmv1.EvictionSpec{
			Hypervisor: hypervisor.Name,
			Reason:     "openstack-hypervisor-operator maintenance",
		}

		// This also transports the label-selector, if set
		transportLabels(&hypervisor.ObjectMeta, &eviction.ObjectMeta)

		if err = hec.Create(ctx, eviction); err != nil {
			return metav1.ConditionUnknown, fmt.Errorf("failed to create eviction due to %w", err)
		}
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
