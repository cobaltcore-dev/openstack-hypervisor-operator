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
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// setNodeLabels sets the labels on the node.
func setNodeLabels(ctx context.Context, writer client.Writer, node *corev1.Node, labels map[string]string) (bool, error) {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Labels, labels)
	if maps.Equal(node.Labels, newNode.Labels) {
		return false, nil
	}

	return true, writer.Patch(ctx, newNode, client.MergeFrom(node))
}

func InstanceHaUrl(region, zone, hostname string) string {
	if haURL, found := os.LookupEnv("KVM_HA_SERVICE_URL"); found {
		return haURL
	}
	return fmt.Sprintf("https://kvm-ha-service-%v.%v.cloud.sap/api/hypervisors/%v", zone, region, hostname)
}

func updateInstanceHA(node *corev1.Node, data string, acceptedCodes []int) error {
	zone, found := node.Labels[corev1.LabelTopologyZone]
	if !found {
		return fmt.Errorf("could not find label %v for node", corev1.LabelTopologyZone)
	}
	region, found := node.Labels[corev1.LabelTopologyRegion]
	if !found {
		return fmt.Errorf("could not find label %v for node", corev1.LabelTopologyRegion)
	}

	hostname, found := node.Labels[corev1.LabelHostname]
	if !found {
		return fmt.Errorf("could not find label %v for node", corev1.LabelHostname)
	}

	url := InstanceHaUrl(region, zone, hostname)
	// G107: Potential HTTP request made with variable url
	resp, err := http.Post(url, "application/json", bytes.NewBuffer([]byte(data))) //nolint:gosec,bodyclose
	if err != nil {
		return fmt.Errorf("failed to send request to ha service due to %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	if !slices.Contains(acceptedCodes, resp.StatusCode) {
		return fmt.Errorf("ha service answered with unexpected response %v for %v from %v", resp.StatusCode, data, url)
	}
	return nil
}

func enableInstanceHA(node *corev1.Node) error {
	return updateInstanceHA(node, `{"enabled": true}`, []int{http.StatusOK})
}

func disableInstanceHA(node *corev1.Node) error {
	return updateInstanceHA(node, `{"enabled": false}`, []int{http.StatusOK, http.StatusNotFound})
}

// IsNodeConditionTrue returns true when the conditionType is present and set to `corev1.ConditionTrue`
func IsNodeConditionTrue(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) bool {
	return IsNodeConditionPresentAndEqual(conditions, conditionType, corev1.ConditionTrue)
}

// IsNodeConditionPresentAndEqual returns true when conditionType is present and equal to status.
func IsNodeConditionPresentAndEqual(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType, status corev1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}

// FindNodeStatusCondition returns the condition of the given type if it exists.
func FindNodeStatusCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) *corev1.NodeCondition {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return &condition
		}
	}
	return nil
}

func HasStatusCondition(conditions []metav1.Condition, conditionType string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return true
		}
	}
	return false
}

// returns all elements in b not in a
func Difference[S ~[]E, E comparable](s1, s2 S) S {
	diff := make(S, 0)
	for item := range slices.Values(s2) {
		if !slices.Contains(s1, item) {
			diff = append(diff, item)
		}
	}

	return diff
}
