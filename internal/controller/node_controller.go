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

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/netbox"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	EVICTION_REQUIRED_LABEL = "cloud.sap/hypervisor-eviction-required"
	EVICTION_APPROVED_LABEL = "cloud.sap/hypervisor-eviction-succeeded"
	HOST_LABEL              = "kubernetes.metal.cloud.sap/host"
)

type NodeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
	NetboxClient  netbox.Client
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(fmt.Sprintf("%T %s", r, req.Name))

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	host, changed, err := r.normalizeName(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	name := fmt.Sprintf("maintenance-required-%v", host)
	eviction := kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: kvmv1.EvictionSpec{
			Hypervisor: node.Name,
			Reason: fmt.Sprintf("openstack-hypervisor-operator: label %v=%v", EVICTION_REQUIRED_LABEL,
				node.Labels[EVICTION_REQUIRED_LABEL]),
		},
	}

	// check for existing eviction, else create it
	if err = r.Get(ctx, client.ObjectKeyFromObject(&eviction), &eviction); err != nil {
		if errors.IsNotFound(err) {
			// attach ownerReference to the eviction, so we get notified about its changes
			if err = controllerutil.SetControllerReference(node, &eviction, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Creating new eviction", "name", name)
			if err = r.Create(ctx, &eviction); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, err
	}

	// check if the eviction is already succeeded
	switch eviction.Status.EvictionState {
	case "Succeeded":
		_, err = r.setNodeLabels(ctx, node, map[string]string{EVICTION_APPROVED_LABEL: "true"})
	case "Failed":
		_, err = r.setNodeLabels(ctx, node, map[string]string{EVICTION_APPROVED_LABEL: "false"})
	}

	return ctrl.Result{}, err
}

// normalizeName returns the host name of the node. If the host name is not set, it will be set to the node name.
// If the host is provisioned by Ironic, the host name will be retrieved from Netbox.
// Eventually ensure these labels are in Gardener
// kubernetes.metal.cloud.sap/role: kvm (rather set these in gardener, as it is fixed)
// kubernetes.metal.cloud.sap/bb
// kubernetes.metal.cloud.sap/host
// kubernetes.metal.cloud.sap/node-ip
func (r *NodeReconciler) normalizeName(ctx context.Context, node *corev1.Node) (string, bool, error) {
	if host, found := node.Labels[HOST_LABEL]; found {
		return host, false, nil
	}

	providerId := node.Spec.ProviderID

	// openstack:/// is the prefix for Ironic nodes
	if !strings.HasPrefix(providerId, "openstack:///") {
		// Assumption: The node name will be correct
		changed := r.setHostLabel(ctx, node, node.Name)
		return node.Name, changed, nil
	}

	serverId := providerId[strings.LastIndex(providerId, "/")+1:]
	listOpts := ports.ListOpts{
		DeviceID: serverId,
		Limit:    1,
	}

	pages, err := ports.List(r.serviceClient, listOpts).AllPages(ctx)
	if err != nil {
		return "", false, fmt.Errorf("could not retrieve ports for %v (%v) due to %w", node.Name, serverId, err)
	}

	nodePorts, err := ports.ExtractPorts(pages)
	// will raise error, if no nodePorts have been found (404)
	if err != nil {
		return "", false, fmt.Errorf("could not extract ports for %v (%v) due to %w", node.Name, serverId, err)
	}

	if len(nodePorts) == 0 {
		return "", false, fmt.Errorf("no Port found for %v (%v)", node.Name, serverId)
	}

	macAddress := nodePorts[0].MACAddress
	host, err := r.NetboxClient.GetHostName(ctx, macAddress)

	if err != nil {
		return host, false, err
	}

	changed := r.setHostLabel(ctx, node, host)

	return host, changed, nil
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setHostLabel(ctx context.Context, node *corev1.Node, host string) bool {
	changed, err := r.setNodeLabels(ctx, node, map[string]string{
		HOST_LABEL: host,
	})
	if err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "cannot set host label on node", "host", host)
	}
	return changed
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setNodeLabels(ctx context.Context, node *corev1.Node, labels map[string]string) (bool, error) {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Labels, labels)
	if maps.Equal(node.Labels, newNode.Labels) {
		return false, nil
	}

	return true, r.Patch(ctx, newNode, k8sclient.MergeFrom(node))
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = logger.FromContext(context.Background())

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "network"); err != nil {
		return err
	}

	if !strings.HasSuffix(r.serviceClient.Endpoint, "v2.0/") {
		r.serviceClient.ResourceBase = r.serviceClient.Endpoint + "v2.0/"
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).     // trigger the r.Reconcile whenever a node is created/updated/deleted
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
