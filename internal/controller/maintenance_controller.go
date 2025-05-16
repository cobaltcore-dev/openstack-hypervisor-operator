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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
)

type MaintenanceController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
	namespace     string
}

const (
	labelManagedBy                  = "app.kubernetes.io/managed-by"
	labelValueMaintenanceController = "cobaltcore.cloud.sap/maintenance-controller"
	labelDeployment                 = "cobaltcore-maintenance-controller"
)

// The counter-side in gardener is here:
// https://github.com/gardener/machine-controller-manager/blob/rel-v0.56/pkg/util/provider/machinecontroller/machine.go#L646

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch;update;watch
// +kubebuilder:rbac:groups="",resources=nodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;;get;list;patch;update;watch
// +kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=create;delete;get;list;patch;update;watch

func (r *MaintenanceController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// ignore not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	if !hasAnyLabel(node.Labels, labelLifecycleMode) {
		return ctrl.Result{}, nil
	}

	if !hasAnyLabel(node.Labels, labelMetalName) {
		return ctrl.Result{}, nil
	}

	if isTerminating(node) {
		changed, err := setNodeLabels(ctx, r.Client, node, map[string]string{labelEvictionRequired: "true"})
		if changed || err != nil {
			return ctrl.Result{}, err
		}
	}

	var minAvailable int32 = 1
	value, found := node.Labels[labelEvictionApproved]
	if found && value == "true" {
		minAvailable = 0
	}

	if err := r.ensureBlockingPodDisruptionBudget(ctx, node, minAvailable); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureBlockingDeployment(ctx, node, minAvailable); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *MaintenanceController) ensureBlockingPodDisruptionBudget(ctx context.Context, node *corev1.Node, minAvailable int32) error {
	name := nameForNode(node)
	podDisruptionBudget := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, podDisruptionBudget, func() error {
		minAvail := intstr.FromInt32(minAvailable)
		podDisruptionBudget.Labels = map[string]string{
			labelMetalName: node.Labels[labelMetalName],
		}
		podDisruptionBudget.Spec = policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvail,
			Selector: &metav1.LabelSelector{
				MatchLabels: labelsForNode(node),
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
	return fmt.Sprintf("block-%v", node.Name)
}

func labelsForNode(node *corev1.Node) map[string]string {
	return map[string]string{
		labelDeployment: nameForNode(node),
		labelMetalName:  node.Labels[labelMetalName],
	}
}

func (r *MaintenanceController) ensureBlockingDeployment(ctx context.Context, node *corev1.Node, scale int32) error {
	name := nameForNode(node)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := labelsForNode(node)
		deployment.Name = name
		deployment.Labels = labels
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: &scale,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					HostNetwork: true, // As the agents are also running int that mode
					NodeSelector: map[string]string{
						corev1.LabelHostname: node.Labels[corev1.LabelHostname],
					},
					Containers: []corev1.Container{
						{
							Name:    "sleep",
							Image:   "keppel.global.cloud.sap/ccloud-dockerhub-mirror/library/busybox:latest",
							Command: []string{"sleep", "inf"},
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
	if r.serviceClient, err = openstack.GetServiceClient(ctx, "compute"); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("maintenance").
		For(&corev1.Node{}).
		Owns(&appsv1.Deployment{}). // trigger the r.Reconcile whenever an Own-ed deployment is created/updated/deleted
		Owns(&policyv1.PodDisruptionBudget{}).
		Complete(r)
}
