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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/global"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

const (
	labelLifecycleMode       = "cobaltcore.cloud.sap/node-hypervisor-lifecycle"
	annotationAggregates     = "nova.openstack.cloud.sap/aggregates"
	annotationCustomTraits   = "nova.openstack.cloud.sap/custom-traits"
	HypervisorControllerName = "hypervisor"
)

var defaultTransferLabels = []string{
	corev1.LabelHostname,
	"kubernetes.metal.cloud.sap/bb",
	"kubernetes.metal.cloud.sap/cluster",
	"kubernetes.metal.cloud.sap/name",
	"kubernetes.metal.cloud.sap/type",
	"worker.garden.sapcloud.io/group",
	"worker.gardener.cloud/pool",
	corev1.LabelTopologyRegion,
	corev1.LabelTopologyZone,
}

var transferLabels = append([]string{}, defaultTransferLabels...)

type HypervisorController struct {
	k8sclient.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;update;patch

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
		// First, propagate spec/metadata derived from the Node (labels,
		// annotations -> aggregates/traits, lifecycle). This must run on
		// every reconcile, including those where status will also change
		// (e.g. AgentPodsEvicted=False during termination); otherwise the
		// Hypervisor spec/labels go stale.
		specBase := hypervisor.DeepCopy()
		syncLabelsAndAnnotations(nodeLabels, hypervisor, node)
		if !equality.Semantic.DeepEqual(hypervisor, specBase) {
			if err := hv.Patch(ctx, hypervisor, k8sclient.MergeFromWithOptions(specBase,
				k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(HypervisorControllerName)); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Then, compute and persist any status changes derived from the Node.
		statusBase := hypervisor.DeepCopy()

		// transfer internal IP
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP && hypervisor.Status.InternalIP != address.Address {
				hypervisor.Status.InternalIP = address.Address
				break
			}
		}

		// update terminating condition
		nodeTerminationCondition := FindNodeStatusCondition(node.Status.Conditions, "Terminating")
		if nodeTerminationCondition != nil && nodeTerminationCondition.Status == corev1.ConditionTrue {
			// Node might be terminating, propagate condition to hypervisor
			meta.SetStatusCondition(&hypervisor.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeTerminating,
				Status:  metav1.ConditionStatus(nodeTerminationCondition.Status),
				Reason:  nodeTerminationCondition.Reason,
				Message: nodeTerminationCondition.Message,
			})
		}

		// Only evaluate after VM eviction; a spurious True on a fresh node
		// (agents not yet scheduled) would be misleading.
		var statusRequeueAfter time.Duration
		if hypervisor.Spec.Maintenance == kvmv1.MaintenanceTermination &&
			meta.IsStatusConditionFalse(hypervisor.Status.Conditions, kvmv1.ConditionTypeEvicting) &&
			nodeHasOffboardingTaint(node) {
			cond, err := hv.computeAgentPodsEvictedCondition(ctx, log, node.Name)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to compute %s condition: %w", kvmv1.ConditionTypeAgentPodsEvicted, err)
			}
			meta.SetStatusCondition(&hypervisor.Status.Conditions, cond)
			if cond.Status == metav1.ConditionFalse {
				// No pod watch — rely on periodic requeue.
				statusRequeueAfter = defaultPollTime
			}
		}

		if !equality.Semantic.DeepEqual(hypervisor, statusBase) {
			// Capture values to apply - only mutate fields this controller owns
			newInternalIP := hypervisor.Status.InternalIP
			terminatingCondition := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeTerminating)
			agentPodsCondition := meta.FindStatusCondition(hypervisor.Status.Conditions, kvmv1.ConditionTypeAgentPodsEvicted)

			if err := utils.PatchHypervisorStatusWithRetry(ctx, hv.Client, hypervisor.Name, HypervisorControllerName, func(h *kvmv1.Hypervisor) {
				h.Status.InternalIP = newInternalIP
				if terminatingCondition != nil {
					meta.SetStatusCondition(&h.Status.Conditions, *terminatingCondition)
				}
				if agentPodsCondition != nil {
					meta.SetStatusCondition(&h.Status.Conditions, *agentPodsCondition)
				}
			}); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{RequeueAfter: statusRequeueAfter}, nil
	}

	syncLabelsAndAnnotations(nodeLabels, hypervisor, node)

	if err := controllerutil.SetOwnerReference(node, hypervisor, hv.Scheme,
		controllerutil.WithBlockOwnerDeletion(true)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed setting controller reference: %w", err)
	}

	if IsNodeConditionPresentAndEqual(node.Status.Conditions, "Terminating", corev1.ConditionTrue) {
		hypervisor.Spec.Maintenance = kvmv1.MaintenanceTermination
	}

	if err := hv.Create(ctx, hypervisor, k8sclient.FieldOwner(HypervisorControllerName)); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Hypervisor resource", "hypervisor", hypervisor.Name)
	return ctrl.Result{}, nil
}

func syncLabelsAndAnnotations(nodeLabels labels.Set, hypervisor *kvmv1.Hypervisor, node *corev1.Node) {
	// transport lifecycle label to hypervisor spec
	if nodeLabels.Has(labelLifecycleMode) {
		hypervisor.Spec.LifecycleEnabled = true
		hypervisor.Spec.SkipTests = nodeLabels.Get(labelLifecycleMode) == "skip-tests"
	}

	// transport relevant labels
	transportLabels(&node.ObjectMeta, &hypervisor.ObjectMeta)
	// transport relevant annotations
	transportAggregatesAndTraits(&node.ObjectMeta, hypervisor)
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

	// append the custom label selector from global config
	if global.LabelSelector != "" {
		requirements, err := labels.ParseToRequirements(global.LabelSelector)
		if err != nil {
			return fmt.Errorf("failed to parse global label selector: %w", err)
		}

		// Rebuild from immutable defaults to avoid accumulating state across repeated calls
		transferLabels = append([]string{}, defaultTransferLabels...)
		for _, requirement := range requirements {
			key := requirement.Key()
			if !slices.Contains(transferLabels, key) {
				transferLabels = append(transferLabels, key)
			}
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(HypervisorControllerName).
		For(&corev1.Node{}).
		WithEventFilter(novaVirtLabeledPredicate).
		Complete(hv)
}

// transportAggregatesAndTraits transports relevant aggregates/traits from the Node to the Hypervisor spec
func transportAggregatesAndTraits(node *metav1.ObjectMeta, hypervisor *kvmv1.Hypervisor) {
	// transport aggregates annotation to hypervisor spec
	if aggregates, found := node.Annotations[annotationAggregates]; found {
		// split aggregates string
		hypervisor.Spec.Aggregates = slices.Collect(func(yield func(string) bool) {
			for agg := range strings.SplitSeq(aggregates, ",") {
				trimmed := strings.TrimSpace(agg)
				if trimmed != "" && !yield(trimmed) {
					return
				}
			}
		})

		if zone, found := node.Labels[corev1.LabelTopologyZone]; found {
			// if zone label is present, add it as aggregate as well, as we follow the convention
			// to have an aggregate for each zone with the same name as the zone
			hypervisor.Spec.Aggregates = append(hypervisor.Spec.Aggregates, zone)
		}
	}

	// transport custom traits annotation to hypervisor spec
	if customTraits, found := node.Annotations[annotationCustomTraits]; found {
		// split custom traits string
		hypervisor.Spec.CustomTraits = slices.Collect(func(yield func(string) bool) {
			for trait := range strings.SplitSeq(customTraits, ",") {
				trimmed := strings.TrimSpace(trait)
				if trimmed != "" && !yield(trimmed) {
					return
				}
			}
		})
	}
}

// transportLabels transports relevant labels from the source to the destination metadata
func transportLabels(source, destination *metav1.ObjectMeta) {
	// If destination is created "manually" (not gotten from the api), this might be nil
	if destination.Labels == nil {
		destination.Labels = make(map[string]string)
	}
	// transfer labels
	for _, transferLabel := range transferLabels {
		if label, ok := source.Labels[transferLabel]; ok {
			destination.Labels[transferLabel] = label
		}
	}
}

// nodeHasOffboardingTaint reports whether the offboarding NoExecute taint has
// been applied to the node. The pod list is only meaningful after that point —
// before it, no agents have been evicted yet.
func nodeHasOffboardingTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == taintKeyOffboarding && t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// computeAgentPodsEvictedCondition checks whether pods that would be evicted
// by the offboarding taint are still running. Only call once Evicting=False.
func (hv *HypervisorController) computeAgentPodsEvictedCondition(ctx context.Context, log logr.Logger, nodeName string) (metav1.Condition, error) {
	offboardingTaint := corev1.Taint{
		Key:    taintKeyOffboarding,
		Effect: corev1.TaintEffectNoExecute,
	}

	var agentPods []string
	for _, ns := range global.AgentNamespaces {
		var continueToken string
		for {
			pods := &corev1.PodList{}
			if err := hv.List(ctx, pods,
				k8sclient.InNamespace(ns),
				k8sclient.MatchingFieldsSelector{
					Selector: fields.OneTermEqualSelector("spec.nodeName", nodeName),
				},
				&k8sclient.ListOptions{Limit: 100, Continue: continueToken},
			); err != nil {
				return metav1.Condition{}, fmt.Errorf("failed to list pods on node %s in namespace %q: %w", nodeName, ns, err)
			}

			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
					continue
				}
				if podToleratesTaint(log, &pod, &offboardingTaint) {
					continue
				}
				agentPods = append(agentPods, pod.Namespace+"/"+pod.Name)
			}

			if pods.Continue == "" {
				break
			}
			continueToken = pods.Continue
		}
	}

	if len(agentPods) == 0 {
		return metav1.Condition{
			Type:    kvmv1.ConditionTypeAgentPodsEvicted,
			Status:  metav1.ConditionTrue,
			Reason:  "NoAgentPods",
			Message: "No agent pods are running on this node",
		}, nil
	}

	sort.Strings(agentPods)
	return metav1.Condition{
		Type:    kvmv1.ConditionTypeAgentPodsEvicted,
		Status:  metav1.ConditionFalse,
		Reason:  "AgentPodsRunning",
		Message: fmt.Sprintf("%d agent pod(s) still running on node: %s", len(agentPods), strings.Join(agentPods, ", ")),
	}, nil
}

// podToleratesTaint reports whether the pod tolerates the taint indefinitely.
// Tolerations with a finite TolerationSeconds are excluded: the pod will
// eventually be evicted and must not be treated as safe to ignore.
func podToleratesTaint(log logr.Logger, pod *corev1.Pod, taint *corev1.Taint) bool {
	for i := range pod.Spec.Tolerations {
		t := &pod.Spec.Tolerations[i]
		if t.TolerationSeconds != nil {
			continue
		}
		if t.ToleratesTaint(log, taint, false) {
			return true
		}
	}
	return false
}
