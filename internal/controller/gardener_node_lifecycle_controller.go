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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	apps1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	v1 "k8s.io/client-go/applyconfigurations/meta/v1"
	policyv1ac "k8s.io/client-go/applyconfigurations/policy/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

type GardenerNodeLifecycleController struct {
	k8sclient.Client
	Scheme    *runtime.Scheme
	namespace string
}

const (
	labelCriticalComponent    = "node.gardener.cloud/critical-component"
	MaintenanceControllerName = "maintenance"
)

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=create;delete;get;list;patch;update;watch

func (r *GardenerNodeLifecycleController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	if !hv.Spec.LifecycleEnabled {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	// Apply the offboarding taint once VMs are gone; gate on Evicting=False
	// to avoid racing with live-migration.
	if hv.Spec.Maintenance == kvmv1.MaintenanceTermination &&
		meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeEvicting) {
		node := &corev1.Node{}
		if err := r.Get(ctx, k8sclient.ObjectKey{Name: hv.Name}, node); err != nil {
			if k8sclient.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			// Node already gone, nothing to taint
		} else {
			patched, err := r.ensureOffboardingTaint(ctx, node)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to ensure offboarding taint: %w", err)
			}
			if patched {
				return ctrl.Result{}, nil
			}
		}
	}

	var minAvailable int32 = 0
	if !meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeEvicting) {
		// Evicting condition is either not present or is true (i.e. ongoing)
		minAvailable = 1 // Do not allow draining of the pod
	}

	offboarded := meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeOffboarded)
	if offboarded {
		minAvailable = 0

		onboardingCompleted := meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
		if onboardingCompleted && hv.Spec.Maintenance == kvmv1.MaintenanceTermination {
			// Wait for HypervisorInstanceHa controller to disable HA
			if !meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled) {
				return ctrl.Result{}, nil // Will be reconciled again when condition changes
			}
		}
	}

	if err := r.ensureBlockingPodDisruptionBudget(ctx, hv, minAvailable); err != nil {
		return ctrl.Result{}, err
	}

	onboardingCompleted := meta.IsStatusConditionFalse(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if err := r.ensureSignallingDeployment(ctx, hv, minAvailable, onboardingCompleted); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureOffboardingTaint adds the offboarding NoExecute taint if not already
// present. Returns true when a patch was issued (caller should return early).
func (r *GardenerNodeLifecycleController) ensureOffboardingTaint(ctx context.Context, node *corev1.Node) (bool, error) {
	for _, t := range node.Spec.Taints {
		if t.Key == taintKeyOffboarding && t.Effect == corev1.TaintEffectNoExecute {
			return false, nil
		}
	}

	log := logger.FromContext(ctx)
	log.Info("Adding offboarding taint to node",
		"node", node.Name,
		"taint", taintKeyOffboarding,
		"effect", corev1.TaintEffectNoExecute)

	// StrategicMergeFrom merges taints by key, preserving concurrent additions.
	patch := k8sclient.StrategicMergeFrom(node.DeepCopy())
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    taintKeyOffboarding,
		Effect: corev1.TaintEffectNoExecute,
	})
	return true, r.Patch(ctx, node, patch, k8sclient.FieldOwner(MaintenanceControllerName))
}

func (r *GardenerNodeLifecycleController) ensureBlockingPodDisruptionBudget(ctx context.Context, hypervisor *kvmv1.Hypervisor, minAvailable int32) error {
	name := nameForHypervisor(hypervisor)
	nodeLabels := labelsForHypervisor(hypervisor)
	gvk, err := apiutil.GVKForObject(hypervisor, r.Scheme)
	if err != nil {
		return err
	}

	podDisruptionBudget := policyv1ac.PodDisruptionBudget(name, MaintenanceNamespace).
		WithLabels(nodeLabels).
		WithOwnerReferences(OwnerReference(hypervisor, &gvk)).
		WithSpec(policyv1ac.PodDisruptionBudgetSpec().
			WithMinAvailable(intstr.FromInt32(minAvailable)).
			WithSelector(v1.LabelSelector().WithMatchLabels(nodeLabels)))

	return r.Apply(ctx, podDisruptionBudget, k8sclient.FieldOwner(MaintenanceControllerName))
}

func nameForHypervisor(hypervisor *kvmv1.Hypervisor) string {
	return fmt.Sprintf("maint-%v", hypervisor.Name)
}

func labelsForHypervisor(hypervisor *kvmv1.Hypervisor) map[string]string {
	return map[string]string{
		MaintenanceLabelKey: nameForHypervisor(hypervisor),
	}
}

func (r *GardenerNodeLifecycleController) ensureSignallingDeployment(ctx context.Context, hypervisor *kvmv1.Hypervisor, scale int32, ready bool) error {
	name := nameForHypervisor(hypervisor)
	labels := labelsForHypervisor(hypervisor)

	podLabels := maps.Clone(labels)
	podLabels[labelCriticalComponent] = "true"

	var command string
	if ready {
		command = "/bin/true"
	} else {
		command = "/bin/false"
	}

	gvk, err := apiutil.GVKForObject(hypervisor, r.Scheme)
	if err != nil {
		return err
	}

	deployment := apps1ac.Deployment(name, MaintenanceNamespace).
		WithOwnerReferences(OwnerReference(hypervisor, &gvk)).
		WithLabels(labels).
		WithSpec(apps1ac.DeploymentSpec().
			WithReplicas(scale).
			WithSelector(v1.LabelSelector().
				WithMatchLabels(labels)).
			WithStrategy(apps1ac.DeploymentStrategy().
				WithRollingUpdate(apps1ac.RollingUpdateDeployment().
					WithMaxUnavailable(intstr.FromInt(0)).
					WithMaxSurge(intstr.FromInt(1)))).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(podLabels).
				WithSpec(corev1ac.PodSpec().
					WithHostNetwork(true).
					WithNodeSelector(map[string]string{
						corev1.LabelHostname: hypervisor.Labels[corev1.LabelHostname],
					}).
					WithTerminationGracePeriodSeconds(1).
					WithTolerations(
						corev1ac.Toleration().
							WithEffect(corev1.TaintEffectNoExecute).
							WithOperator(corev1.TolerationOpExists),
						corev1ac.Toleration().
							WithEffect(corev1.TaintEffectNoSchedule).
							WithOperator(corev1.TolerationOpExists),
					).
					WithContainers(
						corev1ac.Container().
							WithName("sleep").
							WithImage("keppel.global.cloud.sap/ccloud-dockerhub-mirror/library/busybox:latest").
							WithCommand("sh", "-c", "trap 'exit 0' TERM; sleep infinity & wait").
							WithStartupProbe(corev1ac.Probe().
								WithExec(corev1ac.ExecAction().WithCommand(command)).
								WithInitialDelaySeconds(0).
								WithPeriodSeconds(1).
								WithFailureThreshold(1).
								WithSuccessThreshold(1))))))

	return r.Apply(ctx, deployment, k8sclient.FieldOwner(MaintenanceControllerName))
}

// SetupWithManager sets up the controller with the Manager.
func (r *GardenerNodeLifecycleController) SetupWithManager(mgr ctrl.Manager, namespace string) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)
	r.namespace = namespace

	return ctrl.NewControllerManagedBy(mgr).
		Named(MaintenanceControllerName).
		For(&kvmv1.Hypervisor{}).
		Owns(&appsv1.Deployment{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Complete(r)
}
