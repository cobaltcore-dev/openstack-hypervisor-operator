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
	"maps"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
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

// setNodeAnnotations sets annotations on the node.
func setNodeAnnotations(ctx context.Context, writer client.Writer, node *corev1.Node, annotations map[string]string) error {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Annotations, annotations)
	if maps.Equal(node.Annotations, newNode.Annotations) {
		return nil
	}

	return writer.Patch(ctx, newNode, client.MergeFrom(node))
}

func InstanceHaUrl(region, zone, hostname string) string {
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
	resp, err := http.Post(url, "application/json", bytes.NewBuffer([]byte(data)))
	if err != nil {
		return fmt.Errorf("failed to send request to ha service due to %w", err)
	}
	if !slices.Contains(acceptedCodes, resp.StatusCode) {
		return fmt.Errorf("ha service answered with unexpected response %v for %v from %v", resp.StatusCode, data, url)
	}
	return nil
}

func enableInstanceHA(node *corev1.Node) error {
	return updateInstanceHA(node, `{"enabled": true}`, []int{http.StatusOK})
}

func enableInstanceHAMissingOkay(node *corev1.Node) error {
	return updateInstanceHA(node, `{"enabled": true}`, []int{http.StatusOK, http.StatusNotFound})
}

func disableInstanceHA(node *corev1.Node) error {
	return updateInstanceHA(node, `{"enabled": false}`, []int{http.StatusOK, http.StatusNotFound})
}
