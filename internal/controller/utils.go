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
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func addNodeOwnerReference(obj *metav1.ObjectMeta, owner *corev1.Node) bool {
	for _, item := range obj.OwnerReferences {
		if item.APIVersion == owner.APIVersion &&
			item.Kind == owner.Kind &&
			item.Name == owner.Name &&
			item.UID == owner.UID {
			return false
		}
	}

	obj.OwnerReferences = append(obj.OwnerReferences, metav1.OwnerReference{
		APIVersion: owner.APIVersion,
		Kind:       owner.Kind,
		Name:       owner.Name,
		UID:        owner.UID,
	})

	return true
}

// setNodeLabels sets the labels on the node.
func setNodeLabels(ctx context.Context, writer client.Writer, node *corev1.Node, labels map[string]string) (bool, error) {
	newNode := node.DeepCopy()
	maps.Copy(newNode.Labels, labels)
	if maps.Equal(node.Labels, newNode.Labels) {
		return false, nil
	}

	return true, writer.Patch(ctx, newNode, client.MergeFrom(node))
}

func hasAnyLabel(labels map[string]string, list ...string) bool {
	for _, label := range list {
		if _, found := labels[label]; found {
			return true
		}
	}

	return false
}
