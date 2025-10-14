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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

type MaintenanceController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
	namespace     string
}

const (
	labelManagedBy                  = "app.kubernetes.io/managed-by"
	labelDeployment                 = "cobaltcore-maintenance-controller"
	maintenancePodsNamespace        = "kube-system"
	labelCriticalComponent          = "node.gardener.cloud/critical-component"
	labelCriticalComponentsNotReady = "node.gardener.cloud/critical-components-not-ready"
	valueReasonTerminating          = "terminating"
	MaintenanceControllerName       = "maintenance"
)

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update;watch
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=create;delete;get;list;patch;update;watch

func (r *MaintenanceController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

func (r *MaintenanceController) ensureBlockingPodDisruptionBudget(ctx context.Context, node *corev1.Node, minAvailable int32) error {
	name := nameForNode(node)
	podDisruptionBudget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: maintenancePodsNamespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, podDisruptionBudget, func() error {
		minAvail := intstr.FromInt32(minAvailable)
		nodeLabels := labelsForNode(node)
		podDisruptionBudget.Labels = nodeLabels
		podDisruptionBudget.Spec = policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector: &metav1.LabelSelector{
				MatchLabels: nodeLabels,
			},
		}
		return controllerutil.SetControllerReference(node, podDisruptionBudget, r.Scheme)
	})
	return err
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

func (r *MaintenanceController) ensureSignallingDeployment(ctx context.Context, node *corev1.Node, scale int32, ready bool) error {
	name := nameForNode(node)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: maintenancePodsNamespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := labelsForNode(node)
		deployment.Labels = labels

		podLabels := maps.Clone(labels)
		podLabels[labelCriticalComponent] = "true" //nolint:goconst

		var command []string
		if ready {
			command = []string{"/bin/true"}
		} else {
			command = []string{"/bin/false"}
		}

		var one int64 = 1
		zeroStr := intstr.FromInt(0)
		oneStr := intstr.FromInt(1)

		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &scale,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &zeroStr,
					MaxSurge:       &oneStr,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					HostNetwork: true, // to make it run as early as possible
					NodeSelector: map[string]string{
						corev1.LabelHostname: node.Labels[corev1.LabelHostname],
					},
					TerminationGracePeriodSeconds: &one, // busybox sleep doesn't handle TERM so well as pid 1
					Tolerations: []corev1.Toleration{
						{
							Effect:   corev1.TaintEffectNoExecute,
							Operator: corev1.TolerationOpExists,
						},
						{
							Effect:   corev1.TaintEffectNoSchedule,
							Operator: corev1.TolerationOpExists,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "sleep",
							Image:   "keppel.global.cloud.sap/ccloud-dockerhub-mirror/library/busybox:latest",
							Command: []string{"sleep", "inf"},
							// We need apparently retry to get the timing of the container being up correct
							// The startup probe makes sure that we only need to poll this for the (hopefully)
							// non-standard case of the host not being integrated yet
							StartupProbe: &corev1.Probe{
								ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: command}},
								InitialDelaySeconds: 0,
								PeriodSeconds:       1,
								SuccessThreshold:    1,
								FailureThreshold:    1,
							},
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(node, deployment, r.Scheme)
	})
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *MaintenanceController) SetupWithManager(mgr ctrl.Manager, namespace string) error {
	ctx := context.Background()
	_ = logger.FromContext(ctx)
	r.namespace = namespace

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(MaintenanceControllerName).
		For(&corev1.Node{}).
		Owns(&appsv1.Deployment{}). // trigger the r.Reconcile whenever an Own-ed deployment is created/updated/deleted
		Owns(&policyv1.PodDisruptionBudget{}).
		Complete(r)
}
