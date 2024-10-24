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
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	corev1 "k8s.io/api/core/v1" // Required for Watching
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"     // Required for Watching
	ctrl "sigs.k8s.io/controller-runtime" // Required for Watching
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/netbox"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	MAINTENANCE_NEEDED_LABEL   = "cloud.sap/maintenance-required"
	MAINTENANCE_APPROVED_LABEL = "cloud.sap/maintenance-approved"
	HOST_LABEL                 = "kubernetes.metal.cloud.sap/host"
)

type NodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ServiceClient *gophercloud.ServiceClient
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// +kubebuilder:rbac:group=core,resources=nodes,verbs=get;list;watch;patch
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	host, err := r.normalizeName(ctx, node)
	if err != nil {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, err
	}

	err = r.ensureEvictionIfNeeded(ctx, node, host)
	if err != nil {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, err
	}

	return ctrl.Result{}, nil
}

// normalizeName returns the host name of the node. If the host name is not set, it will be set to the node name.
// If the host is provisioned by Ironic, the host name will be retrieved from Netbox.
// Eventually ensure these labels are in Gardener
// kubernetes.metal.cloud.sap/role: kvm (rather set these in gardener, as it is fixed)
// kubernetes.metal.cloud.sap/bb
// kubernetes.metal.cloud.sap/host
// kubernetes.metal.cloud.sap/node-ip
func (r *NodeReconciler) normalizeName(ctx context.Context, node corev1.Node) (string, error) {
	if host, found := node.Labels[HOST_LABEL]; found {
		return host, nil
	}

	providerId := node.Spec.ProviderID

	// openstack:/// is the prefix for Ironic nodes
	if !strings.HasPrefix(providerId, "openstack:///") {
		// Assumption: The node name will be correct
		r.setHostLabel(ctx, node, node.Name)
		return node.Name, nil
	}

	serverId := providerId[strings.LastIndex(providerId, "/")+1:]
	listOpts := ports.ListOpts{
		DeviceID: serverId,
		Limit:    1,
	}

	pages, err := ports.List(r.ServiceClient, listOpts).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("could not retrieve ports for %v (%v) due to %w", node.Name, serverId, err)
	}

	nodePorts, err := ports.ExtractPorts(pages)
	// will raise error, if no nodePorts have been found (404)
	if err != nil {
		return "", fmt.Errorf("could not extract ports for %v (%v) due to %w", node.Name, serverId, err)
	}

	if len(nodePorts) == 0 {
		return "", fmt.Errorf("no Port found for %v (%v)", node.Name, serverId)
	}

	macAddress := nodePorts[0].MACAddress
	host, err := netbox.GetHostName(ctx, macAddress)

	if err != nil {
		return host, err
	}

	r.setHostLabel(ctx, node, host)

	return host, nil
}

// ensureEvictionIfNeeded ensures that an eviction is created if the node has the maintenance label.
func (r *NodeReconciler) ensureEvictionIfNeeded(ctx context.Context, node corev1.Node, host string) error {
	value, found := node.Labels[MAINTENANCE_NEEDED_LABEL]
	if !found {
		return nil
	}

	name := fmt.Sprintf("maintenance-required-%v", host)
	eviction := &kvmv1.Eviction{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "monsoon3", // todo: change to the correct namespace or use cluster scoped CRs
	}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, eviction, func() error {
		eviction.Spec.Hypervisor = node.Name
		eviction.Spec.Reason = fmt.Sprintf("Node %v, label %v=%v", node.Name, MAINTENANCE_NEEDED_LABEL, value)
		return nil
	})

	return err
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setHostLabel(ctx context.Context, node corev1.Node, host string) {
	newNode := node.DeepCopy()
	newNode.Labels[HOST_LABEL] = host

	err := r.Patch(ctx, newNode, client.MergeFrom(&node))
	if err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "cannot set label on node", "host", host)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())

	var err error
	if r.ServiceClient, err = openstack.GetServiceClient(context.Background(), "network"); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("OpenstackNodeController").
		For(&corev1.Node{}).
		Complete(r)
}
