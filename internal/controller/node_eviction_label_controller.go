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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

const (
	disabledSuffix          = "-disabled"
	labelMl2MechanismDriver = "neutron.openstack.cloud.sap/ml2-mechanism-driver"
)

type NodeEvictionLabelReconciler struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=evictions,verbs=get;list;watch;create;update;patch;delete

func (r *NodeEvictionLabelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	hostname, found := node.Labels[corev1.LabelHostname]
	if !found {
		// Should never happen (tm)
		return ctrl.Result{}, nil
	}

	maintenanceValue, found := node.Labels[labelEvictionRequired]
	name := fmt.Sprintf("maintenance-required-%v", hostname)
	eviction := &kvmv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	var err error
	if !found {
		newNode := node.DeepCopy()
		permitAgentsLabels(newNode.Labels)
		delete(newNode.Labels, labelEvictionApproved)
		if !maps.Equal(newNode.Labels, node.Labels) {
			err = r.Patch(ctx, newNode, k8sclient.MergeFrom(node))
			if err != nil {
				return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
			}
		}
		err = r.Delete(ctx, eviction)
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	var value string
	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Name: node.Name}, hv); err != nil {
		return ctrl.Result{}, err
	}
	if HasStatusCondition(hv.Status.Conditions, ConditionTypeOnboarding) {
		// not clear what this if for
		value = "true" //nolint:goconst
	} else {
		// check for existing eviction, else create it
		value, err = r.reconcileEviction(ctx, eviction, node, hostname, maintenanceValue)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if value != "" {
		err = disableInstanceHA(node)
		if err != nil {
			return ctrl.Result{}, err
		}

		newNode := node.DeepCopy()
		if value == "true" { //nolint:goconst
			evictAgentsLabels(newNode.Labels)
		}
		newNode.Labels[labelEvictionApproved] = maintenanceValue

		if !maps.Equal(newNode.Labels, node.Labels) {
			err = r.Patch(ctx, newNode, k8sclient.MergeFrom(node))
		}
	}

	return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
}

func (r *NodeEvictionLabelReconciler) reconcileEviction(ctx context.Context, eviction *kvmv1.Eviction, node *corev1.Node, hostname string, maintenanceValue string) (string, error) {
	log := logger.FromContext(ctx)
	if err := r.Get(ctx, k8sclient.ObjectKeyFromObject(eviction), eviction); err != nil {
		if !k8serrors.IsNotFound(err) {
			return "", err
		}
		if err := controllerutil.SetOwnerReference(node, eviction, r.Scheme); err != nil {
			return "", err
		}
		log.Info("Creating new eviction", "name", eviction.Name)
		eviction.Spec = kvmv1.EvictionSpec{
			Hypervisor: hostname,
			Reason:     fmt.Sprintf("openstack-hypervisor-operator: label %v=%v", labelEvictionRequired, maintenanceValue),
		}

		if err = enableInstanceHAMissingOkay(node); err != nil {
			return "", fmt.Errorf("failed to enable instance ha before eviction due to %w", err)
		}

		if err = r.Create(ctx, eviction); err != nil {
			return "", fmt.Errorf("failed to create eviction due to %w", err)
		}
	}

	// check if the eviction is already succeeded
	var evictionState string
	if status := meta.FindStatusCondition(eviction.Status.Conditions, kvmv1.ConditionTypeEviction); status != nil {
		evictionState = status.Reason
	}
	switch evictionState {
	case "Succeeded":
		return "true", nil //nolint:goconst
	case "Failed":
		return "false", nil
	default:
		return "", nil
	}
}

func evictAgentsLabels(labels map[string]string) {
	hypervisorType, found := labels[labelHypervisor]
	if found && !strings.HasSuffix(hypervisorType, disabledSuffix) {
		labels[labelHypervisor] = hypervisorType + disabledSuffix
	}
	ml2MechanismDriver, found := labels[labelMl2MechanismDriver]
	if found && !strings.HasSuffix(ml2MechanismDriver, disabledSuffix) {
		labels[labelMl2MechanismDriver] = ml2MechanismDriver + disabledSuffix
	}
}

func permitAgentsLabels(labels map[string]string) {
	hypervisorType, found := labels[labelHypervisor]
	if found && strings.HasSuffix(hypervisorType, disabledSuffix) {
		labels[labelHypervisor] = strings.TrimSuffix(hypervisorType, disabledSuffix)
	}
	ml2MechanismDriver, found := labels[labelMl2MechanismDriver]
	if found && strings.HasSuffix(ml2MechanismDriver, disabledSuffix) {
		labels[labelMl2MechanismDriver] = strings.TrimSuffix(ml2MechanismDriver, disabledSuffix)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeEvictionLabelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)

	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeEvictionLabel").
		For(&corev1.Node{}).     // trigger the r.Reconcile whenever a node is created/updated/deleted.
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
