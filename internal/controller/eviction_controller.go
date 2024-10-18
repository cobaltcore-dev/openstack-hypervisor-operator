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
	"math/rand"
	"time"

	"github.com/go-openapi/swag"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	ServiceClient *gophercloud.ServiceClient
	rand          *rand.Rand
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx)

	var eviction kvmv1.Eviction
	if err := r.Get(ctx, req.NamespacedName, &eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ignore if eviction already succeeded
	if eviction.Status.EvictionState == "Succeeded" || eviction.Status.EvictionState == "Failed" {
		return ctrl.Result{}, nil
	}

	// Update status
	eviction.Status.EvictionState = "Running"
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Reconciling",
		Status:  metav1.ConditionTrue,
		Message: "Reconciling",
		Reason:  "Reconciling",
	})
	if err := r.Status().Update(ctx, &eviction); err != nil {
		return ctrl.Result{}, err
	}

	// Fetch all virtual machines on the hypervisor
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.ServiceClient, eviction.Spec.Hypervisor)
	if err != nil {
		log.Error(err, "failed to get hypervisor")
		// Abort eviction
		r.addErrorCondition(ctx, &eviction, err)
		return ctrl.Result{}, nil
	}

	// Not sure, if it is an error condition, but I assume it will return []
	if hypervisor.Servers == nil {
		err = errors.New("no servers on hypervisor found")
		// Abort eviction
		r.addErrorCondition(ctx, &eviction, err)
		return ctrl.Result{}, nil
	}

	disableService := services.UpdateOpts{Status: services.ServiceDisabled,
		DisabledReason: fmt.Sprintf("K8S Operator by eviction %v/%v", eviction.Namespace, eviction.Name)}

	_, err = services.Update(ctx, r.ServiceClient, hypervisor.Service.ID, disableService).Extract()
	if err != nil {
		r.addErrorCondition(ctx, &eviction, err)
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, nil
	}

	r.addProgressCondition(ctx, &eviction, "Host disabled", "Update")

	// Evict all virtual machines
	anyFailed := false
	for _, server := range *hypervisor.Servers {
		res := servers.Get(ctx, r.ServiceClient, server.UUID)
		vm, err := res.Extract()
		if err != nil {
			anyFailed = true
			log.Info("Failed to get server", "server", vm.ID)
			r.addErrorCondition(ctx, &eviction, err)
			continue
		}

		if vm.Status != "ACTIVE" {
			continue
		}

		liveMigrateOpts := servers.LiveMigrateOpts{
			BlockMigration: swag.Bool(false),
		}

		log.Info("Live migrating server", "server", vm.ID, "source", eviction.Spec.Hypervisor)
		if res := servers.LiveMigrate(ctx, r.ServiceClient, vm.ID, liveMigrateOpts); res.Err != nil {
			anyFailed = true
			log.Info("Failed to evict server", "server", vm.ID)
			r.addErrorCondition(ctx, &eviction, res.ExtractErr())
		}
	}

	// Requeue after 30 seconds
	if anyFailed {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, nil
	}

	eviction.Status.EvictionState = "Succeeded"
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Reconciling",
		Status:  metav1.ConditionFalse,
		Message: "Reconciled",
		Reason:  "Reconciled",
	})

	return ctrl.Result{}, r.Status().Update(ctx, &eviction)
}

func (r *EvictionReconciler) addCondition(ctx context.Context, eviction *kvmv1.Eviction, status metav1.ConditionStatus, message string, reason string) {
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Eviction",
		Status:  status,
		Message: message,
		Reason:  reason,
	})
	if err := r.Status().Update(ctx, eviction); err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "Failed to update conditions")
	}
}

func (r *EvictionReconciler) addProgressCondition(ctx context.Context, eviction *kvmv1.Eviction, message string, reason string) {
	r.addCondition(ctx, eviction, metav1.ConditionTrue, message, reason)
}

func (r *EvictionReconciler) addErrorCondition(ctx context.Context, eviction *kvmv1.Eviction, err error) {
	r.addCondition(ctx, eviction, metav1.ConditionFalse, err.Error(), "Failed")
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())
	var err error
	if r.ServiceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}
	r.ServiceClient.Microversion = "2.90" // Xena (or later)
	r.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	return ctrl.NewControllerManagedBy(mgr).
		For(&kvmv1.Eviction{}).
		Complete(r)
}
