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
	"k8s.io/client-go/util/retry"
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
	labelDeployment           = "cobaltcore-maintenance-controller"
	maintenancePodsNamespace  = "kube-system"
	labelCriticalComponent    = "node.gardener.cloud/critical-component"
	valueReasonTerminating    = "terminating"
	MaintenanceControllerName = "maintenance"
)

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=create;delete;get;list;patch;update;watch

func (r *GardenerNodeLifecycleController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	hv := kvmv1.Hypervisor{}
	if err := r.Get(ctx, k8sclient.ObjectKey{Name: req.Name}, &hv); k8sclient.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	if !hv.Spec.LifecycleEnabled {
		// Nothing to be done
		return ctrl.Result{}, nil
	}

	if isTerminating(node) {
		changed, err := setNodeLabels(ctx, r.Client, node, map[string]string{labelEvictionRequired: valueReasonTerminating})
		if changed || err != nil {
			return ctrl.Result{}, err
		}
	}

	// We do not care about the particular value, as long as it isn't an error
	var minAvailable int32 = 1
	evictionValue, found := node.Labels[labelEvictionApproved]
	if found && evictionValue != "false" {
		minAvailable = 0
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return r.ensureBlockingPodDisruptionBudget(ctx, node, minAvailable)
	}); err != nil {
		return ctrl.Result{}, err
	}

	onboardingCompleted := meta.IsStatusConditionFalse(hv.Status.Conditions, ConditionTypeOnboarding)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return r.ensureSignallingDeployment(ctx, node, minAvailable, onboardingCompleted)
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GardenerNodeLifecycleController) ensureBlockingPodDisruptionBudget(ctx context.Context, node *corev1.Node, minAvailable int32) error {
	name := nameForNode(node)
	nodeLabels := labelsForNode(node)
	gvk, err := apiutil.GVKForObject(node, r.Scheme)
	if err != nil {
		return err
	}

	podDisruptionBudget := policyv1ac.PodDisruptionBudget(name, maintenancePodsNamespace).
		WithLabels(nodeLabels).
		WithOwnerReferences(OwnerReference(node, &gvk)).
		WithSpec(policyv1ac.PodDisruptionBudgetSpec().
			WithMinAvailable(intstr.FromInt32(minAvailable)).
			WithSelector(v1.LabelSelector().WithMatchLabels(nodeLabels)))

	return r.Apply(ctx, podDisruptionBudget, k8sclient.FieldOwner(MaintenanceControllerName))
}

func isTerminating(node *corev1.Node) bool {
	conditions := node.Status.Conditions
	if conditions == nil {
		return false
	}

	// See: https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L658-L659
	for _, condition := range conditions {
		if condition.Type == "Terminating" {
			return true
		}
	}

	return false
}

func nameForNode(node *corev1.Node) string {
	return fmt.Sprintf("maint-%v", node.Name)
}

func labelsForNode(node *corev1.Node) map[string]string {
	return map[string]string{
		labelDeployment: nameForNode(node),
	}
}

func (r *GardenerNodeLifecycleController) ensureSignallingDeployment(ctx context.Context, node *corev1.Node, scale int32, ready bool) error {
	name := nameForNode(node)
	labels := labelsForNode(node)

	podLabels := maps.Clone(labels)
	podLabels[labelCriticalComponent] = "true"

	var command string
	if ready {
		command = "/bin/true"
	} else {
		command = "/bin/false"
	}

	gvk, err := apiutil.GVKForObject(node, r.Scheme)
	if err != nil {
		return err
	}

	deployment := apps1ac.Deployment(name, maintenancePodsNamespace).
		WithOwnerReferences(OwnerReference(node, &gvk)).
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
						corev1.LabelHostname: node.Labels[corev1.LabelHostname],
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
							WithCommand("sleep", "inf").
							WithStartupProbe(corev1ac.Probe().
								WithExec(corev1ac.ExecAction().WithCommand(command)).
								WithInitialDelaySeconds(0).
								WithPeriodSeconds(0).
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
		For(&corev1.Node{}).
		Owns(&appsv1.Deployment{}). // trigger the r.Reconcile whenever an Own-ed deployment is created/updated/deleted
		Owns(&policyv1.PodDisruptionBudget{}).
		Complete(r)
}
