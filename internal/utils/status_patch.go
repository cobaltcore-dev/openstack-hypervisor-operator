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

package utils

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

// StatusPatchBackoff is a retry backoff for status patches that may conflict
// with other controllers. Uses exponential backoff with more retries than the
// default to handle high contention scenarios.
var StatusPatchBackoff = wait.Backoff{
	Steps:    10,
	Duration: 50 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.2,
}

// PatchHypervisorStatusWithRetry patches hypervisor status with retry on conflict.
// The updateFn receives a fresh copy of the hypervisor and should apply status changes to it.
// It re-fetches the resource before each retry attempt to get the latest resourceVersion.
func PatchHypervisorStatusWithRetry(ctx context.Context, c k8sclient.Client, name, fieldOwner string, updateFn func(*kvmv1.Hypervisor)) error {
	return retry.RetryOnConflict(StatusPatchBackoff, func() error {
		hv := &kvmv1.Hypervisor{}
		if err := c.Get(ctx, k8sclient.ObjectKey{Name: name}, hv); err != nil {
			return err
		}
		base := hv.DeepCopy()
		updateFn(hv)
		return c.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(base,
			k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(fieldOwner))
	})
}

// PatchEvictionStatusWithRetry patches eviction status with retry on conflict.
// The updateFn receives a fresh copy of the eviction and should apply status changes to it.
// It re-fetches the resource before each retry attempt to get the latest resourceVersion.
func PatchEvictionStatusWithRetry(ctx context.Context, c k8sclient.Client, name, fieldOwner string, updateFn func(*kvmv1.Eviction)) error {
	return retry.RetryOnConflict(StatusPatchBackoff, func() error {
		eviction := &kvmv1.Eviction{}
		if err := c.Get(ctx, k8sclient.ObjectKey{Name: name}, eviction); err != nil {
			return err
		}
		base := eviction.DeepCopy()
		updateFn(eviction)
		return c.Status().Patch(ctx, eviction, k8sclient.MergeFromWithOptions(base,
			k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(fieldOwner))
	})
}
