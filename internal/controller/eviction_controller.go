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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

// EvictionReconciler reconciles a Eviction object
type EvictionReconciler struct {
	serviceClient *gophercloud.ServiceClient
	rand          *rand.Rand
}

const (
	finalizerName = "eviction-controller.cloud.sap/finalizer"
	Succeeded     = "Succeeded"
	Running       = "Running"
	Reconciling   = "Reconciling"
	Reconciled    = "Reconciled"
)

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *EvictionReconciler) Reconcile(ctx context.Context, req request) (ctrl.Result, error) {
	log := logger.FromContext(ctx)

	var eviction kvmv1.Eviction
	if err := req.client.Get(ctx, req.NamespacedName, &eviction); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	finalized, err := r.handleFinalizer(ctx, req.client, &eviction)

	if err != nil || finalized {
		return ctrl.Result{}, err
	}

	// Ignore if eviction already succeeded
	if eviction.Status.EvictionState == Succeeded {
		return ctrl.Result{}, nil
	}

	// Update status
	eviction.Status.EvictionState = Running
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionTrue,
		Message: Reconciling,
		Reason:  Reconciling,
	})
	if err := req.client.Status().Update(ctx, &eviction); err != nil {
		return ctrl.Result{}, err
	}

	// Fetch all virtual machines on the hypervisor
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.serviceClient, eviction.Spec.Hypervisor, true)
	if err != nil {
		log.Error(err, "failed to get hypervisor")
		// Abort eviction
		r.addErrorCondition(ctx, req.client, &eviction, err)
		return ctrl.Result{}, nil
	}

	eviction.Status.HypervisorServiceId = hypervisor.ID
	if err := req.client.Status().Update(ctx, &eviction); err != nil {
		return ctrl.Result{}, err
	}

	// TODO: Not sure, if it is an error condition, but I assume it will return []
	if hypervisor.Servers == nil {
		err = errors.New("no servers on hypervisor found")
		// Abort eviction
		r.addErrorCondition(ctx, req.client, &eviction, err)
		return ctrl.Result{}, nil
	}

	if hypervisor.Service.DisabledReason == "" {
		disableService := services.UpdateOpts{Status: services.ServiceDisabled,
			DisabledReason: r.evictionReason(eviction)}

		_, err = services.Update(ctx, r.serviceClient, hypervisor.Service.ID, disableService).Extract()
		if err != nil {
			r.addErrorCondition(ctx, req.client, &eviction, err)
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Second * 30,
			}, nil
		}
	}

	r.addProgressCondition(ctx, req.client, &eviction, "Host disabled", "Update")

	// Evict all virtual machines
	anyFailed := false
	for _, server := range *hypervisor.Servers {
		res := servers.Get(ctx, r.serviceClient, server.UUID)
		vm, err := res.Extract()
		if err != nil {
			anyFailed = true
			log.Info("Failed to get server", "server", vm.ID)
			r.addErrorCondition(ctx, req.client, &eviction, err)
			continue
		}

		if vm.HypervisorHostname != hypervisor.HypervisorHostname {
			// Someone else might have triggered a migration (in parallel)
			continue
		}

		vmIsActive := vm.Status == "ACTIVE" || vm.PowerState == 1

		switch vm.Status {
		case "MIGRATING", "RESIZE":
			// wait for migration to finish
			if !r.waitForMigration(ctx, req.client, *vm, &eviction) {
				anyFailed = true
			}
			continue
		case "ERROR":
			// Needs manual intervention (or another operator fixes it)
			anyFailed = true
			continue
		default:
		}

		if vmIsActive {
			if !r.liveMigrate(ctx, req.client, *vm, &eviction) {
				anyFailed = true
			}
		} else {
			if !r.coldMigrate(ctx, req.client, *vm, &eviction) {
				anyFailed = true
			}
		}
	}

	// Requeue after 30 seconds
	if anyFailed {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, nil
	}

	eviction.Status.EvictionState = Succeeded
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    Reconciling,
		Status:  metav1.ConditionFalse,
		Message: Reconciled,
		Reason:  Reconciled,
	})

	return ctrl.Result{}, req.client.Status().Update(ctx, &eviction)
}

func (r *EvictionReconciler) evictionReason(eviction kvmv1.Eviction) string {
	return fmt.Sprintf("Eviction %v/%v: %v", eviction.Namespace, eviction.Name, eviction.Spec.Reason)
}

func (r *EvictionReconciler) handleFinalizer(ctx context.Context, client client.Client, eviction *kvmv1.Eviction) (bool, error) {
	containsFinalizer := controllerutil.ContainsFinalizer(eviction, finalizerName)
	// Not being deleted
	if eviction.DeletionTimestamp.IsZero() {
		if !containsFinalizer {
			controllerutil.AddFinalizer(eviction, finalizerName)
			if err := client.Update(ctx, eviction); err != nil {
				return false, err
			}
		}
	} else {
		// being deleted
		if controllerutil.RemoveFinalizer(eviction, finalizerName) {
			if err := r.enableHypervisorService(ctx, client, *eviction); err != nil {
				return false, err
			}

			if err := client.Update(ctx, eviction); err != nil {
				return false, err
			}
		}

		return true, nil
	}

	return false, nil
}

func (r *EvictionReconciler) enableHypervisorService(ctx context.Context, client client.Client, eviction kvmv1.Eviction) error {
	log := logger.FromContext(ctx)
	hypervisor, err := openstack.GetHypervisorByName(ctx, r.serviceClient, eviction.Spec.Hypervisor, false)
	if err != nil {
		log.Error(err, "failed to get hypervisor")
		// Abort eviction
		r.addErrorCondition(ctx, client, &eviction, err)
		return err
	}

	if hypervisor.Service.DisabledReason != r.evictionReason(eviction) {
		return nil
	}

	return r.enableHypervisorServiceInternal(ctx, hypervisor.Service.ID)
}

func (r *EvictionReconciler) enableHypervisorServiceInternal(ctx context.Context, id string) error {
	log := logger.FromContext(ctx)
	enableService := services.UpdateOpts{Status: services.ServiceEnabled}
	log.Info("Enabling hypervisor", "id", id)
	_, err := services.Update(ctx, r.serviceClient, id, enableService).Extract()
	return err
}

func (r *EvictionReconciler) liveMigrate(ctx context.Context, client client.Client, vm servers.Server, eviction *kvmv1.Eviction) bool {
	log := logger.FromContext(ctx)

	liveMigrateOpts := servers.LiveMigrateOpts{
		BlockMigration: swag.Bool(false),
	}

	res := servers.LiveMigrate(ctx, r.serviceClient, vm.ID, liveMigrateOpts)
	if res.Err != nil {
		log.Error(res.Err, "Failed to evict running server", "server", vm.ID)
		r.addErrorCondition(ctx, client, eviction, res.Err)
		return false
	}

	log.Info("Live migrating server", "server", vm.ID, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return r.waitForMigration(ctx, client, vm, eviction)
}

func (r *EvictionReconciler) pollInstance(ctx context.Context, client client.Client, vm *servers.Server, eviction *kvmv1.Eviction) bool {
	res := servers.Get(ctx, r.serviceClient, vm.ID)

	// We are find with VMs being deleted, while we try to evict the host
	if res.StatusCode == 404 {
		return true
	}

	err := res.ExtractInto(vm)
	if err == nil {
		return true
	}

	log := logger.FromContext(ctx)
	log.Info("Failed to poll server", "server", vm.ID)
	r.addErrorCondition(ctx, client, eviction, err)
	return false
}

func (r *EvictionReconciler) waitForMigration(ctx context.Context, client client.Client, vm servers.Server, eviction *kvmv1.Eviction) bool {
	if !r.pollInstance(ctx, client, &vm, eviction) {
		return false
	}

	log := logger.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Info("Monitoring of migration cancelled")
			return false
		default:
			switch vm.Status {
			case "ACTIVE", "SHUTOFF":
				return true
			case "RESIZE", "MIGRATING":
				// Sleep and poll.
				select {
				case <-ctx.Done():
					log.Info("Monitoring of migration cancelled")
					return false
				case <-time.After(1 * time.Second):
					if !r.pollInstance(ctx, client, &vm, eviction) {
						return false
					}
				}
			case "VERIFY_RESIZE":
				if err := servers.ConfirmResize(ctx, r.serviceClient, vm.ID).ExtractErr(); err != nil {
					log.Error(err, "ColdMigration")
					return false
				}
			default:
				log.Info("Unexpected state when migrating", "vm", vm.ID, "status", vm.Status)
				return false
			}
		}
	}
}

func (r *EvictionReconciler) coldMigrate(ctx context.Context, client client.Client, vm servers.Server, eviction *kvmv1.Eviction) bool {
	log := logger.FromContext(ctx)

	res := servers.Migrate(ctx, r.serviceClient, vm.ID)
	if res.Err != nil {
		log.Error(res.Err, "Failed to evict stopped server", "server", vm.ID)
		r.addErrorCondition(ctx, client, eviction, res.Err)
		return false
	}

	log.Info("Cold-migrating server", "server", vm.ID, "source", eviction.Spec.Hypervisor, "X-Openstack-Request-Id", res.Header["X-Openstack-Request-Id"][0])
	return r.waitForMigration(ctx, client, vm, eviction)
}

// addCondition adds a condition to the Eviction status and updates the status
func (r *EvictionReconciler) addCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, status metav1.ConditionStatus, message string, reason string) {
	meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
		Type:    "Eviction",
		Status:  status,
		Message: message,
		Reason:  reason,
	})
	if err := client.Status().Update(ctx, eviction); err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "Failed to update conditions")
	}
}

func (r *EvictionReconciler) addProgressCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, message string, reason string) {
	r.addCondition(ctx, client, eviction, metav1.ConditionTrue, message, reason)
}

func (r *EvictionReconciler) addErrorCondition(ctx context.Context, client client.Client, eviction *kvmv1.Eviction, err error) {
	r.addCondition(ctx, client, eviction, metav1.ConditionFalse, err.Error(), "Failed")
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManagerAndClusters(mgr ctrl.Manager, clusters map[string]cluster.Cluster) error {
	_ = logger.FromContext(context.Background())
	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}
	r.serviceClient.Microversion = "2.90" // Xena (or later)
	r.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	b := builder.TypedControllerManagedBy[request](mgr).Named("eviction-controller")

	eviction := kvmv1.Eviction{}
	for clusterName, cluster := range clusters {
		b = b.WatchesRawSource(source.TypedKind(
			cluster.GetCache(),
			&eviction,
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, e *kvmv1.Eviction) []request {
				return []request{{
					NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name},
					clusterName:    clusterName,
					client:         cluster.GetClient(),
				}}
			}),
		))
	}

	return b.Complete(r)
}
