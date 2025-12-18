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
	"net/http"
	"slices"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/placement/v1/resourceproviders"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	DecommissionControllerName = "decommission"
)

type NodeDecommissionReconciler struct {
	k8sclient.Client
	Scheme          *runtime.Scheme
	computeClient   *gophercloud.ServiceClient
	placementClient *gophercloud.ServiceClient
}

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;update;patch

func (r *NodeDecommissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	hv := &kvmv1.Hypervisor{}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
			// ignore not found errors, could be deleted
			return k8sclient.IgnoreNotFound(err)
		}

		setDecommissioningCondition := func(msg string) {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeReady,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonDecommissioning,
				Message: msg,
			})
		}

		if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeReady) {
			setDecommissioningCondition("Node is being decommissioned, removing host from nova")
			return r.Status().Update(ctx, hv)
		}

		hypervisor, err := openstack.GetHypervisorByName(ctx, r.computeClient, hv.Name, true)
		if err != nil {
			if errors.Is(err, openstack.ErrNoHypervisor) {
				// We are (hopefully) done
				setDecommissioningCondition("Node not registered in nova anymore, proceeding with deletion")
				hv.Status.Evicted = true
				return r.Status().Update(ctx, hv)
			}

			setDecommissioningCondition(fmt.Sprintf("Failed to get %q from openstack: %v", hv.Name, err))
			return r.Status().Update(ctx, hv)
		}

		if err = r.doDecomission(ctx, hv, hypervisor); err != nil {
			log.Error(err, "Failed to decomission node", "node", hv.Name)
			setDecommissioningCondition(err.Error())
			return r.Status().Update(ctx, hv)
		}

		// Decommissioning succeeded, proceed with deletion
		hv.Status.Evicted = true
		return r.Status().Update(ctx, hv)
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: utils.ShortRetryTime}, nil
}

func (r *NodeDecommissionReconciler) doDecomission(ctx context.Context, hv *kvmv1.Hypervisor, hypervisor *hypervisors.Hypervisor) error {
	// TODO: remove since RunningVMs is only available until micro-version 2.87, and also is updated asynchronously
	// so it might be not accurate
	if hypervisor.RunningVMs > 0 {
		// Still running VMs, cannot delete the service
		return fmt.Errorf("node is being decommissioned, but still has %d running VMs", hypervisor.RunningVMs)
	}

	if hypervisor.Servers != nil && len(*hypervisor.Servers) > 0 {
		// Still VMs assigned to the host, cannot delete the service
		return fmt.Errorf("node is being decommissioned, but still has %d assigned VMs, "+
			"check with `openstack server list --all-projects --host %s`", len(*hypervisor.Servers), hv.Name)
	}

	// Before removing the service, first take the hypervisor out of the aggregates,
	// so when the hypervisor comes back, it doesn't up with the old associations
	aggs, err := openstack.GetAggregatesByName(ctx, r.computeClient)
	if err != nil {
		return fmt.Errorf("cannot list aggregates due to: %w", err)
	}

	host := hv.Name
	for name, aggregate := range aggs {
		if slices.Contains(aggregate.Hosts, host) {
			opts := aggregates.RemoveHostOpts{Host: host}
			if err = aggregates.RemoveHost(ctx, r.computeClient, aggregate.ID, opts).Err; err != nil {
				return fmt.Errorf("failed to remove host %v from aggregate %v due to %w", name, host, err)
			}
		}
	}

	// Deleting and evicted, so better delete the service
	err = services.Delete(ctx, r.computeClient, hypervisor.Service.ID).ExtractErr()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("cannot delete service %s due to %w", hypervisor.Service.ID, err)
	}

	rp, err := resourceproviders.Get(ctx, r.placementClient, hypervisor.ID).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("cannot get resource provider %s due to %w", hypervisor.ID, err)
	}

	if err = openstack.CleanupResourceProvider(ctx, r.placementClient, rp); err != nil {
		return fmt.Errorf("cannot cleanup resource provider: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDecommissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}

	r.computeClient.Microversion = "2.93"

	r.placementClient, err = openstack.GetServiceClient(ctx, "placement", nil)
	if err != nil {
		return err
	}
	r.placementClient.Microversion = "1.39" // yoga, or later

	return ctrl.NewControllerManagedBy(mgr).
		Named(DecommissionControllerName).
		For(&kvmv1.Hypervisor{}).
		WithEventFilter(utils.HypervisorTerminationPredicate).
		WithEventFilter(utils.LifecycleEnabledPredicate).
		Complete(r)
}
