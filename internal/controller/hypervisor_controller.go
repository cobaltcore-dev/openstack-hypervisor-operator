/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

const (
	labelLifecycleMode       = "cobaltcore.cloud.sap/node-hypervisor-lifecycle"
	annotationAggregates     = "nova.openstack.cloud.sap/aggregates"
	annotationCustomTraits   = "nova.openstack.cloud.sap/custom-traits"
	HypervisorControllerName = "hypervisor"
)

var transferLabels = []string{
	"kubernetes.metal.cloud.sap/name",
	"kubernetes.metal.cloud.sap/cluster",
	"kubernetes.metal.cloud.sap/bb",
	"worker.garden.sapcloud.io/group",
	corev1.LabelTopologyZone,
	corev1.LabelTopologyRegion,
	corev1.LabelHostname,
}

type HypervisorController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;create;update;patch;delete

func (hv *HypervisorController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)

	node := &corev1.Node{}
	if err := hv.Get(ctx, req.NamespacedName, node); err != nil {
		// Ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	nodeLabels := labels.Set(node.Labels)
	hypervisor := &kvmv1.Hypervisor{
		ObjectMeta: metav1.ObjectMeta{
			Name:   node.Name,
			Labels: map[string]string{},
		},
		Spec: kvmv1.HypervisorSpec{
			HighAvailability:   true,
			InstallCertificate: true,
		},
	}

	// Check if hypervisor already exists
	if err := hv.Get(ctx, k8sclient.ObjectKeyFromObject(hypervisor), hypervisor); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get hypervisor: %w", err)
		}
		// continue with creation
	} else {
		// update Status if needed
		changed := false

		// transfer internal IP
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP && hypervisor.Status.InternalIP != address.Address {
				hypervisor.Status.InternalIP = address.Address
				changed = true
				break
			}
		}

		// update terminating condition
		nodeTerminationCondition := FindNodeStatusCondition(node.Status.Conditions, "Terminating")
		if nodeTerminationCondition != nil && nodeTerminationCondition.Status == corev1.ConditionTrue {
			// Node might be terminating, propagate condition to hypervisor
			changed = meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeReady,
				Status:  metav1.ConditionFalse,
				Reason:  nodeTerminationCondition.Reason,
				Message: nodeTerminationCondition.Message,
			}) || changed
			changed = meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeTerminating,
				Status:  metav1.ConditionStatus(nodeTerminationCondition.Status),
				Reason:  nodeTerminationCondition.Reason,
				Message: nodeTerminationCondition.Message,
			}) || changed
		}

		if changed {
			return ctrl.Result{}, hv.Status().Update(ctx, hypervisor)
		}

		// transport label/anotations changes
		before := hypervisor.DeepCopy()
		updateLabelsAndAnnotations(&node.ObjectMeta, hypervisor)
		if !reflect.DeepEqual(before, hypervisor) {
			return ctrl.Result{}, hv.Patch(ctx, hypervisor, k8sclient.MergeFrom(before))
		}

		return ctrl.Result{}, nil
	}

	// transport lifecycle label to hypervisor spec
	if nodeLabels.Has(labelLifecycleMode) {
		hypervisor.Spec.LifecycleEnabled = true
		hypervisor.Spec.SkipTests = nodeLabels.Get(labelLifecycleMode) == "skip-tests"
	}

	// transport relevant annotations
	updateLabelsAndAnnotations(&node.ObjectMeta, hypervisor)

	if err := controllerutil.SetOwnerReference(node, hypervisor, hv.Scheme, controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed setting controller reference: %w", err)
	}

	if err := hv.Create(ctx, hypervisor); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Hypervisor resource", "hypervisor", hypervisor.Name)
	return ctrl.Result{}, nil
}

func (hv *HypervisorController) SetupWithManager(mgr ctrl.Manager) error {
	novaVirtLabeledPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      labelHypervisor,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create label selector predicate: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorControllerName).
		For(&corev1.Node{}).
		WithEventFilter(novaVirtLabeledPredicate).
		Complete(hv)
}

// updateLabelsAndAnnotations transports relevant annotations from the Node to the Hypervisor spec
func updateLabelsAndAnnotations(node *metav1.ObjectMeta, hypervisor *kvmv1.Hypervisor) {
	// transport aggregates annotation to hypervisor spec
	if aggregates, found := node.Annotations[annotationAggregates]; found {
		// split aggregates string
		hypervisor.Spec.Aggregates = slices.Collect(func(yield func(string) bool) {
			for _, agg := range strings.Split(aggregates, ",") {
				trimmed := strings.TrimSpace(agg)
				if trimmed != "" && !yield(trimmed) {
					return
				}
			}
		})
	}

	// transport custom traits annotation to hypervisor spec
	if customTraits, found := node.Annotations[annotationCustomTraits]; found {
		// split custom traits string
		hypervisor.Spec.CustomTraits = slices.Collect(func(yield func(string) bool) {
			for _, trait := range strings.Split(customTraits, ",") {
				trimmed := strings.TrimSpace(trait)
				if trimmed != "" && !yield(trimmed) {
					return
				}
			}
		})
	}

	// transfer labels
	for _, transferLabel := range transferLabels {
		if label, ok := node.Labels[transferLabel]; ok {
			hypervisor.Labels[transferLabel] = label
		}
	}
}
