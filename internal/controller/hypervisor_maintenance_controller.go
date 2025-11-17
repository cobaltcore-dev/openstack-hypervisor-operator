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

	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
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
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete

func (hec *HypervisorMaintenanceController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hv := &kvmv1.Hypervisor{}
	if err := hec.Get(ctx, req.NamespacedName, hv); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// is onboarding completed?
	if !meta.IsStatusConditionFalse(hv.Status.Conditions, ConditionTypeOnboarding) {
		return ctrl.Result{}, nil
	}

	// ensure serviceId is set
	if hv.Status.ServiceID == "" {
		return ctrl.Result{}, nil
	}

	old := hv.DeepCopy()

	if err := hec.reconcileComputeService(ctx, hv); err != nil {
		return ctrl.Result{}, err
	}

	if err := hec.reconcileEviction(ctx, hv); err != nil {
		return ctrl.Result{}, err
	}

	if equality.Semantic.DeepEqual(hv, old) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, hec.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(old, k8sclient.MergeFromWithOptimisticLock{}))
}

func (hec *HypervisorMaintenanceController) reconcileComputeService(ctx context.Context, hv *kvmv1.Hypervisor) error {
	log := logger.FromContext(ctx)
	serviceId := hv.Status.ServiceID

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		// Enable the compute service (in case we haven't done so already)
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionFalse,
			Message: "Hypervisor is enabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Spec matches status
			return nil
		}

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionTrue,
			Reason:  kvmv1.ConditionReasonReadyReady,
			Message: "Hypervisor is ready",
		})

		// We need to enable the host as per spec
		enableService := services.UpdateOpts{Status: services.ServiceEnabled}
		log.Info("Enabling hypervisor", "id", serviceId)
		_, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract()
		if err != nil {
			return fmt.Errorf("failed to enable hypervisor due to %w", err)
		}
	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto, kvmv1.MaintenanceHA:
		// Disable the compute service:
		// Also in case of HA, as it doesn't hurt to disable it twice, and this
		// allows us to enable the service again, when the maintenance field is
		// cleared in the case above.
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionTrue,
			Message: "Hypervisor is disabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Spec matches status
			return nil
		}

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonReadyMaintenance,
			Message: "Hypervisor is disabled",
		})

		// We need to disable the host as per spec
		enableService := services.UpdateOpts{
			Status:         services.ServiceDisabled,
			DisabledReason: "Hypervisor CRD: spec.maintenance=" + hv.Spec.Maintenance,
		}
		log.Info("Disabling hypervisor", "id", serviceId)
		_, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract()
		if err != nil {
			return fmt.Errorf("failed to disable hypervisor due to %w", err)
		}
	}

	return nil
}

func (hec *HypervisorMaintenanceController) reconcileEviction(ctx context.Context, hv *kvmv1.Hypervisor) error {
	eviction := &kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name: hv.Name,
		},
	}

	switch hv.Spec.Maintenance {
	case kvmv1.MaintenanceUnset:
		// Avoid deleting the eviction over and over.
		if hv.Status.Evicted || meta.RemoveStatusCondition(&hv.Status.Conditions, kvmv1.ConditionTypeEvicting) {
			err := k8sclient.IgnoreNotFound(hec.Delete(ctx, eviction))
			hv.Status.Evicted = false
			return err
		}
		return nil
	case kvmv1.MaintenanceManual, kvmv1.MaintenanceAuto:
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
			hv.Status.Evicted = true
		} else {
			message = "Evicting"
			reason = kvmv1.ConditionReasonRunning
			hv.Status.Evicted = false
		}

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeEvicting,
			Status:  status,
			Reason:  reason,
			Message: message,
		})

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonReadyEvicted,
			Message: "Hypervisor is disabled and evicted",
		})

		return nil
	}

	return nil
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
		transportLabels(&eviction.ObjectMeta, hypervisor)

		if err = hec.Create(ctx, eviction); err != nil {
			return metav1.ConditionUnknown, fmt.Errorf("failed to create eviction due to %w", err)
		}
	}

	// check if we are still evicting (defaulting to yes)
	if meta.IsStatusConditionFalse(eviction.Status.Conditions, kvmv1.ConditionTypeEvicting) {
		return metav1.ConditionFalse, nil
	} else {
		return metav1.ConditionTrue, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (hec *HypervisorMaintenanceController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	var err error
	if hec.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	hec.computeClient.Microversion = "2.90" // Xena (or later)

	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorMaintenanceControllerName).
		For(&kvmv1.Hypervisor{}).
		Owns(&kvmv1.Eviction{}). // trigger Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(hec)
}
