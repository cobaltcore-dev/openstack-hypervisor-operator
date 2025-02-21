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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
)

const (
	decommissionFinalizerName = "cobaltcore.cloud.sap/decommission-hypervisor"
	lifecycleLabel            = "cobaltcore.cloud.sap/node-hypervisor-lifecycle"
)

type NodeDecommissionReconciler struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
func (r *NodeDecommissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	host, err := normalizeName(node)

	_, found := node.Labels[lifecycleLabel]

	if !found {
		// Get out of the way
		controllerutil.RemoveFinalizer(node, decommissionFinalizerName)
		return ctrl.Result{}, r.Update(ctx, node)
	}

	if controllerutil.AddFinalizer(node, decommissionFinalizerName) {
		log.Info("Added finalizer")
		return ctrl.Result{}, r.Update(ctx, node)
	}

	// Not deleting node, nothing to do
	if node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	// Getting deleted, so we better clean-up
	computeHostQuery := services.ListOpts{Binary: "nova-compute", Host: host}
	hostPages, err := services.List(r.serviceClient, computeHostQuery).AllPages(ctx)

	var anyError error
	if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		serviceList, err := services.ExtractServices(hostPages)
		if err != nil {
			return ctrl.Result{}, err
		}

		for _, service := range serviceList {
			// Deleting and evicted, so better delete the service
			err = services.Delete(ctx, r.serviceClient, service.ID).ExtractErr()
			if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				anyError = err
			}
		}

		if anyError != nil {
			return ctrl.Result{}, err
		}
	}

	// Host empty, service deleted, we are good to go
	if controllerutil.RemoveFinalizer(node, decommissionFinalizerName) {
		if err := r.Update(ctx, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// normalizeName returns the host name of the node.
func normalizeName(node *corev1.Node) (string, error) {
	if name, found := node.Labels[NAME_LABEL]; found {
		return name, nil
	}

	if host, found := node.Labels[HOST_LABEL]; found {
		return host, nil
	}

	return "", errors.New("could not find node name")
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeDecommissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}

	if err != nil {
		return fmt.Errorf("could not create label selector due to %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeDecommission").
		For(&corev1.Node{}).
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
