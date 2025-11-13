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
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	HypervisorMaintenanceControllerName = "HypervisorMaintenanceController"
)

type HypervisorMaintenanceController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	computeClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete

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

	log := logger.FromContext(ctx).
		WithName("HypervisorService")
	ctx = logger.IntoContext(ctx, log)

	changed, err := hec.reconcileComputeService(ctx, hv)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		return ctrl.Result{}, hec.Status().Update(ctx, hv)
	} else {
		return ctrl.Result{}, nil
	}
}

func (hec *HypervisorMaintenanceController) reconcileComputeService(ctx context.Context, hv *kvmv1.Hypervisor) (bool, error) {
	log := logger.FromContext(ctx)
	serviceId := hv.Status.ServiceID

	switch hv.Spec.Maintenance {
	case "": // Enable the compute service (in case we haven't done so already)
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionFalse,
			Message: "Hypervisor enabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Spec matches status
			return false, nil
		}
		// We need to enable the host as per spec
		enableService := services.UpdateOpts{Status: services.ServiceEnabled}
		log.Info("Enabling hypervisor", "id", serviceId)
		_, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract()
		if err != nil {
			return false, fmt.Errorf("failed to enable hypervisor due to %w", err)
		}
	case "manual", "auto", "ha": // Disable the compute service
		if !meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeHypervisorDisabled,
			Status:  metav1.ConditionTrue,
			Message: "Hypervisor disabled",
			Reason:  kvmv1.ConditionReasonSucceeded,
		}) {
			// Spec matches status
			return false, nil
		}

		// We need to disable the host as per spec
		enableService := services.UpdateOpts{
			Status:         services.ServiceDisabled,
			DisabledReason: "Hypervisor CRD: spec.maintenance=" + hv.Spec.Maintenance,
		}
		log.Info("Disabling hypervisor", "id", serviceId)
		_, err := services.Update(ctx, hec.computeClient, serviceId, enableService).Extract()
		if err != nil {
			return false, fmt.Errorf("failed to disable hypervisor due to %w", err)
		}
	}

	return true, nil
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
		Complete(hec)
}
