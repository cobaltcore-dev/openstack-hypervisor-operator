/*
Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors.

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
	"math/rand"
	"time"

	"github.com/go-openapi/swag"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
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
	listOpts := servers.ListOpts{
		Host:       eviction.Spec.Hypervisor,
		AllTenants: true,
	}
	pages, err := servers.List(r.ServiceClient, listOpts).AllPages(ctx)
	if err != nil {
		log.Error(err, "Failed to list servers")
		// Abort eviction
		eviction.Status.EvictionState = "Failed"
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    "Reconciling",
			Status:  metav1.ConditionFalse,
			Message: err.Error(),
			Reason:  "Failed",
		})
		return ctrl.Result{}, r.Status().Update(ctx, &eviction)
	}

	vms, err := servers.ExtractServers(pages)
	if err != nil {
		log.Error(err, "Failed to extract servers")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, err
	}

	if len(vms) == 0 {
		// Update status
		eviction.Status.EvictionState = "Succeeded"
		meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
			Type:    "Reconciling",
			Status:  metav1.ConditionFalse,
			Message: "Reconciled",
			Reason:  "Reconciled",
		})
		if err = r.Status().Update(ctx, &eviction); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		// Get migration target candidates
		var candidates []*string
		if candidates, err = openstack.GetMigrationCandidates(ctx, eviction.Spec.Hypervisor, r.ServiceClient); err != nil {
			log.Error(err, "Failed to get migration candidates")
			return ctrl.Result{}, err
		}

		// Evict all virtual machines
		for _, vm := range vms {
			if vm.Status != "ACTIVE" {
				continue
			}

			randn := r.rand.Intn(len(candidates) - 1)
			liveMigrateOpts := servers.LiveMigrateOpts{
				Host:           candidates[randn],
				BlockMigration: swag.Bool(false),
				DiskOverCommit: swag.Bool(false),
			}

			log.Info("Live migrating server", "server", vm.ID, "source",
				eviction.Spec.Hypervisor, "target", liveMigrateOpts.Host)
			if res := servers.LiveMigrate(ctx, r.ServiceClient, vm.ID, liveMigrateOpts); res.Err != nil {
				log.Info("Failed to evict server", "server", vm.ID)
				meta.SetStatusCondition(&eviction.Status.Conditions, metav1.Condition{
					Type:    "Eviction",
					Status:  metav1.ConditionFalse,
					Message: res.ExtractErr().Error(),
					Reason:  "Failed",
				})
				if err = r.Status().Update(ctx, &eviction); err != nil {
					return ctrl.Result{}, err
				}

				return ctrl.Result{
					Requeue:      true,
					RequeueAfter: time.Second * 30,
				}, nil
			}
		}

		// Requeue after 30 seconds
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EvictionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())
	var err error
	if r.ServiceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}
	r.rand = rand.New(rand.NewSource(time.Now().UnixNano()))

	return ctrl.NewControllerManagedBy(mgr).
		For(&kvmv1.Eviction{}).
		Complete(r)
}
