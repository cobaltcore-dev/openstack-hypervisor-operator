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
	"maps"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	corev1 "k8s.io/api/core/v1"                   // Required for Watching
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // Required for Watching
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime" // Required for Watching
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	serviceClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodeReconciler) Reconcile(ctx context.Context, req request) (ctrl.Result, error) {
	var node corev1.Node
	if err := req.client.Get(ctx, req.NamespacedName, &node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	host, err := r.normalizeName(ctx, req.client, node)
	if err != nil {
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Second * 30,
		}, err
	}

	err = r.reconcileEviction(ctx, req.client, node, host)
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
func (r *NodeReconciler) normalizeName(ctx context.Context, client client.Client, node corev1.Node) (string, error) {
	if host, found := node.Labels[HOST_LABEL]; found {
		return host, nil
	}

	providerId := node.Spec.ProviderID

	// openstack:/// is the prefix for Ironic nodes
	if !strings.HasPrefix(providerId, "openstack:///") {
		// Assumption: The node name will be correct
		r.setHostLabel(ctx, client, node, node.Name)
		return node.Name, nil
	}

	serverId := providerId[strings.LastIndex(providerId, "/")+1:]
	listOpts := ports.ListOpts{
		DeviceID: serverId,
		Limit:    1,
	}

	pages, err := ports.List(r.serviceClient, listOpts).AllPages(ctx)
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

	r.setHostLabel(ctx, client, node, host)

	return host, nil
}

// reconcileEviction ensures that an eviction is created if the node has the maintenance label.
func (r *NodeReconciler) reconcileEviction(ctx context.Context, client client.Client, node corev1.Node, host string) error {
	neededValue, neededFound := node.Labels[MAINTENANCE_NEEDED_LABEL]
	_, approvedFound := node.Labels[MAINTENANCE_APPROVED_LABEL]

	name := fmt.Sprintf("maintenance-required-%v", host)
	eviction := &kvmv1.Eviction{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "monsoon3", // todo: change to the correct namespace or use cluster scoped CRs
	}}

	if !neededFound && !approvedFound {
		client.Delete(ctx, eviction)
		return nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, client, eviction, func() error {
		eviction.Spec.Hypervisor = node.Name
		eviction.Spec.Reason = fmt.Sprintf("Node %v, label %v=%v", node.Name, MAINTENANCE_NEEDED_LABEL, neededValue)
		return nil
	})

	if err != nil {
		return err
	}

	switch eviction.Status.EvictionState {
	case "Succeeded":
		err = r.setNodeLabels(ctx, client, node, map[string]string{MAINTENANCE_APPROVED_LABEL: "true"})
	case "Failed":
		err = r.setNodeLabels(ctx, client, node, map[string]string{MAINTENANCE_APPROVED_LABEL: "false"})
	}

	return err
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setHostLabel(ctx context.Context, client client.Client, node corev1.Node, host string) {
	err := r.setNodeLabels(ctx, client, node, map[string]string{
		HOST_LABEL: host,
	})
	if err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "cannot set host label on node", "host", host)
	}
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setNodeLabels(ctx context.Context, c client.Client, node corev1.Node, labels map[string]string) error {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Labels, labels)

	return c.Patch(ctx, newNode, client.MergeFrom(&node))
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManagerAndClusters(mgr ctrl.Manager, clusters map[string]cluster.Cluster) error {
	_ = logger.FromContext(context.Background())

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "network"); err != nil {
		return err
	}

	if !strings.HasSuffix(r.serviceClient.Endpoint, "v2.0/") {
		r.serviceClient.ResourceBase = r.serviceClient.Endpoint + "v2.0/"
	}

	b := builder.TypedControllerManagedBy[request](mgr).
		Named("openstack-node-controller")

	for clusterName, cluster := range clusters {
		b = b.WatchesRawSource(source.TypedKind(
			cluster.GetCache(),
			&corev1.Node{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, n *corev1.Node) []request {
				return []request{{
					NamespacedName: types.NamespacedName{Namespace: n.Namespace, Name: n.Name},
					clusterName:    clusterName,
					client:         cluster.GetClient(),
				}}
			}),
		))
	}

	return b.Complete(r)
}
