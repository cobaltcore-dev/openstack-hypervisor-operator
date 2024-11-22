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
	corev1 "k8s.io/api/core/v1"                   // Required for Watching
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // Required for Watching
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime" // Required for Watching
	"sigs.k8s.io/controller-runtime/pkg/builder"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/netbox"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

const (
	EVICTION_REQUIRED_LABEL = "cloud.sap/hypervisor-eviction-required"
	EVICTION_APPROVED_LABEL = "cloud.sap/hypervisor-eviction-succeeded"
	HOST_LABEL              = "kubernetes.metal.cloud.sap/host"
	MANAGED_BY              = "app.kubernetes.io/managed-by"
	MANAGER_NAME            = "openstack-node-controller"
	// Changing MANAGER_NAME will cause the app to lose track of already
	// created Evictions by this controller
)

type NodeReconciler struct {
	serviceClient *gophercloud.ServiceClient
}

type NodeMetadata = metav1.PartialObjectMetadata

type nodeControllerRequest struct {
	kind           string
	namespacedName types.NamespacedName
	clusterName    string
	client         k8sclient.Client
	state          string
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodeReconciler) Reconcile(ctx context.Context, req nodeControllerRequest) (ctrl.Result, error) {
	log := logger.FromContext(ctx)
	log = log.WithName("Reconcile").WithName(req.kind)
	ctx = logger.IntoContext(ctx, log)

	switch req.kind {
	case "Node":
		return r.reconcileNode(ctx, req)
	case "Eviction":
		return r.reconcileEviction(ctx, req)
	default:
		log.Info("Got reconciliation request for unknown kind", "kind", req.kind)
		return ctrl.Result{}, nil
	}
}

func (r *NodeReconciler) reconcileNode(ctx context.Context, req nodeControllerRequest) (ctrl.Result, error) {
	node := &corev1.Node{}
	log := logger.FromContext(ctx)
	log = log.WithValues("node", req.namespacedName.Name)
	ctx = logger.IntoContext(ctx, log)

	if err := req.client.Get(ctx, req.namespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	host, changed, err := r.normalizeName(ctx, req.client, node)
	if err != nil {
		return ctrl.Result{}, err
	}

	if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	err = r.reconcileEvictionForNode(ctx, req.client, node, host, req.state)
	return ctrl.Result{}, err
}

// normalizeName returns the host name of the node. If the host name is not set, it will be set to the node name.
// If the host is provisioned by Ironic, the host name will be retrieved from Netbox.
// Eventually ensure these labels are in Gardener
// kubernetes.metal.cloud.sap/role: kvm (rather set these in gardener, as it is fixed)
// kubernetes.metal.cloud.sap/bb
// kubernetes.metal.cloud.sap/host
// kubernetes.metal.cloud.sap/node-ip
func (r *NodeReconciler) normalizeName(ctx context.Context, client k8sclient.Client, node *corev1.Node) (string, bool, error) {
	if host, found := node.Labels[HOST_LABEL]; found {
		return host, false, nil
	}

	providerId := node.Spec.ProviderID

	// openstack:/// is the prefix for Ironic nodes
	if !strings.HasPrefix(providerId, "openstack:///") {
		// Assumption: The node name will be correct
		changed := r.setHostLabel(ctx, client, node, node.Name)
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
	host, err := netbox.GetHostName(ctx, macAddress)

	if err != nil {
		return host, false, err
	}

	changed := r.setHostLabel(ctx, client, node, host)

	return host, changed, nil
}

// reconcileEvictionForNode ensures that an eviction is created if the node has the maintenance label.
func (r *NodeReconciler) reconcileEvictionForNode(ctx context.Context, client k8sclient.Client, node *corev1.Node, host, state string) error {
	name := fmt.Sprintf("maintenance-required-%v", host)
	eviction := &kvmv1.Eviction{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "monsoon3", // todo: change to the correct namespace or use cluster scoped CRs
		Labels:    map[string]string{MANAGED_BY: MANAGER_NAME},
	}}

	log := logger.FromContext(ctx)

	if state == "" {
		log.Info("no label", "eviction", eviction)
		return k8sclient.IgnoreNotFound(client.Delete(ctx, eviction))
	}

	log.Info("with label", "eviction", eviction)
	_, err := controllerutil.CreateOrUpdate(ctx, client, eviction, func() error {
		eviction.Labels[MANAGED_BY] = MANAGER_NAME
		eviction.Spec.Hypervisor = node.Name
		eviction.Spec.Reason = fmt.Sprintf("Node %v, label %v=%v", node.Name, EVICTION_REQUIRED_LABEL, state)
		return nil
	})
	return err
}

// reconcileEviction ensures that an eviction is created if the node has the maintenance label.
func (r *NodeReconciler) reconcileEviction(ctx context.Context, req nodeControllerRequest) (ctrl.Result, error) {
	var eviction kvmv1.Eviction
	if err := req.client.Get(ctx, req.namespacedName, &eviction); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	log := logger.FromContext(ctx)
	log = log.WithValues("eviction", eviction.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := req.client.Get(ctx, types.NamespacedName{Name: eviction.Spec.Hypervisor}, node); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	switch eviction.Status.EvictionState {
	case "Succeeded":
		_, err := r.setNodeLabels(ctx, req.client, node, map[string]string{EVICTION_APPROVED_LABEL: "true"})
		return ctrl.Result{}, err
	case "Failed":
		_, err := r.setNodeLabels(ctx, req.client, node, map[string]string{EVICTION_APPROVED_LABEL: "false"})
		return ctrl.Result{}, err
	default:
		// We will also be called for the old state (i.e. Running) and simply ignore that
		return ctrl.Result{}, nil
	}
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setHostLabel(ctx context.Context, client k8sclient.Client, node *corev1.Node, host string) bool {
	changed, err := r.setNodeLabels(ctx, client, node, map[string]string{
		HOST_LABEL: host,
	})
	if err != nil {
		log := logger.FromContext(ctx)
		log.Error(err, "cannot set host label on node", "host", host)
	}
	return changed
}

// setHostLabel sets the host label on the node.
func (r *NodeReconciler) setNodeLabels(ctx context.Context, c k8sclient.Client, node *corev1.Node, labels map[string]string) (bool, error) {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Labels, labels)
	if maps.Equal(node.Labels, newNode.Labels) {
		return false, nil
	}

	return true, c.Patch(ctx, newNode, k8sclient.MergeFrom(node))
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

	b := builder.TypedControllerManagedBy[nodeControllerRequest](mgr).
		Named(MANAGER_NAME)

	metaObj := &NodeMetadata{}
	gvk, err := apiutil.GVKForObject(&corev1.Node{}, mgr.GetScheme())
	if err != nil {
		return fmt.Errorf("unable to determine GVK of corev1.Node for a metadata-only watch: %w", err)
	}
	metaObj.SetGroupVersionKind(gvk)

	for clusterName, clusterObj := range clusters {
		b = b.WatchesRawSource(source.TypedKind(
			clusterObj.GetCache(),
			metaObj,
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, n *NodeMetadata) []nodeControllerRequest {
				// This will be called twice: once for the old and once for the new value (unfortunately)
				return []nodeControllerRequest{{
					kind:           "Node",
					namespacedName: types.NamespacedName{Namespace: n.Namespace, Name: n.Name},
					clusterName:    clusterName,
					client:         clusterObj.GetClient(),
					state:          n.Labels[EVICTION_REQUIRED_LABEL],
				}}
			}),
			predicate.TypedFuncs[*NodeMetadata]{
				UpdateFunc: func(e event.TypedUpdateEvent[*NodeMetadata]) bool {
					newValue := e.ObjectNew.Labels[EVICTION_REQUIRED_LABEL]
					oldValue := e.ObjectOld.Labels[EVICTION_REQUIRED_LABEL]

					return newValue != oldValue
				},
				DeleteFunc:  func(e event.TypedDeleteEvent[*NodeMetadata]) bool { return false },
				GenericFunc: func(e event.TypedGenericEvent[*NodeMetadata]) bool { return false },
			},
		))

		b = b.WatchesRawSource(source.TypedKind(
			clusterObj.GetCache(),
			&kvmv1.Eviction{},
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, e *kvmv1.Eviction) []nodeControllerRequest {
				// This will be called twice: once for the old and once for the new value (unfortunately)
				return []nodeControllerRequest{{
					kind:           "Eviction",
					namespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Name},
					clusterName:    clusterName,
					client:         clusterObj.GetClient(),
					state:          e.Status.EvictionState,
				}}
			}),
			predicate.TypedFuncs[*kvmv1.Eviction]{
				CreateFunc: func(e event.TypedCreateEvent[*kvmv1.Eviction]) bool { return false },
				UpdateFunc: func(e event.TypedUpdateEvent[*kvmv1.Eviction]) bool {
					if value, found := e.ObjectNew.Labels[MANAGED_BY]; !found || value != MANAGER_NAME {
						return false
					}
					newEvictionState := e.ObjectNew.Status.EvictionState
					if newEvictionState == e.ObjectOld.Status.EvictionState {
						// Only changes
						return false
					}
					if newEvictionState != "Failed" && newEvictionState != "Succeeded" {
						// And only the final states
						return false
					}
					return true
				},
				DeleteFunc:  func(e event.TypedDeleteEvent[*kvmv1.Eviction]) bool { return false },
				GenericFunc: func(e event.TypedGenericEvent[*kvmv1.Eviction]) bool { return false },
			}),
		)
	}

	return b.Complete(r)
}
