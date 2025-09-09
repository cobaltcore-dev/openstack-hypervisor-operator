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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
	labelLifecycleMode = "cobaltcore.cloud.sap/node-hypervisor-lifecycle"
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
	var lifecycleEnabled, skipTest bool
	log := logger.FromContext(ctx).WithName(req.Name)

	node := &corev1.Node{}
	if err := hv.Get(ctx, req.NamespacedName, node); err != nil {
		// Ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	nodeLabels := labels.Set(node.Labels)
	if nodeLabels.Has(labelLifecycleMode) {
		lifecycleEnabled = true
		skipTest = nodeLabels.Get(labelLifecycleMode) == "skip-test"
	}

	hypervisor := &kvmv1.Hypervisor{
		ObjectMeta: metav1.ObjectMeta{
			Name:   node.Name,
			Labels: map[string]string{},
		},
		Spec: kvmv1.HypervisorSpec{
			LifecycleEnabled:   lifecycleEnabled,
			SkipTests:          skipTest,
			HighAvailability:   true,
			InstallCertificate: true,
		},
	}

	// Transfer Labels
	for _, label := range transferLabels {
		if nodeLabels.Has(label) {
			hypervisor.Labels[label] = nodeLabels.Get(label)
		}
	}

	// Ensure corresponding hypervisor exists
	if err := hv.Get(ctx, k8sclient.ObjectKeyFromObject(hypervisor), hypervisor); err != nil {
		if k8serrors.IsNotFound(err) {
			// attach ownerReference for cascading deletion
			if err = controllerutil.SetControllerReference(node, hypervisor, hv.Scheme); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed setting controller reference: %w", err)
			}

			log.Info("Setup hypervisor", "name", node.Name)
			if err = hv.Create(ctx, hypervisor); err != nil {
				return ctrl.Result{}, err
			}

			// Requeue to update status
			return ctrl.Result{RequeueAfter: 0}, nil
		}

		return ctrl.Result{}, err
	}

	nodeTerminationCondition := FindNodeStatusCondition(node.Status.Conditions, "Terminating")
	if nodeTerminationCondition != nil && nodeTerminationCondition.Status == corev1.ConditionTrue {
		// Node might be terminating, propagate condition to hypervisor
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  nodeTerminationCondition.Reason,
			Message: nodeTerminationCondition.Message,
		})
		meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeTerminating,
			Status:  metav1.ConditionStatus(nodeTerminationCondition.Status),
			Reason:  nodeTerminationCondition.Reason,
			Message: nodeTerminationCondition.Message,
		})

		// update status
		if err := hv.Status().Update(ctx, hypervisor); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update hypervisor status: %w", err)
		}
	}

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
		For(&corev1.Node{}).
		Owns(&kvmv1.Hypervisor{}).
		WithEventFilter(novaVirtLabeledPredicate).
		Complete(hv)
}
