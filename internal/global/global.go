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

package global

import "time"

const (
	// DefaultPollTime is the standard requeue interval controllers use while
	// waiting for a slow external state transition (an OpenStack migration, a
	// service being disabled, ...) to settle.
	DefaultPollTime = 10 * time.Second

	// ShortRetryTime is a brief requeue interval used to retry quickly when a
	// reconcile could make progress on the next pass without waiting for a full
	// poll cycle.
	ShortRetryTime = 1 * time.Second
)

var (
	// LabelSelector is a custom label that is used to select resources managed by the operator.
	LabelSelector = ""

	// AgentNamespaces is the list of namespaces in which agent pods (nova-compute,
	// neutron) are scheduled. The pod list during offboarding is restricted to
	// these namespaces. Must be non-empty; set via --agent-namespaces.
	AgentNamespaces []string

	// EvictionConcurrency is the default maximum number of VM migrations that may
	// run concurrently while draining a single hypervisor. Defaults to 1 (serial),
	// which preserves the historical one-at-a-time behavior. Set via
	// --eviction-concurrency.
	EvictionConcurrency = 1

	// EvictionTraitConcurrency maps a Placement trait name to a maximum number of
	// concurrent migrations for hosts carrying that trait. It overrides
	// EvictionConcurrency for matching hosts. Typical use is forcing exclusive host
	// classes (e.g. CUSTOM_HANA_EXCLUSIVE_HOST) to migrate serially. Set via
	// --eviction-trait-concurrency.
	EvictionTraitConcurrency = map[string]int{}
)

// ResolveConcurrency returns the maximum number of concurrent migrations for a
// host given its traits. It starts from EvictionConcurrency and, for every trait
// present in EvictionTraitConcurrency, keeps the lowest configured limit. The
// result is always clamped to at least 1 so a misconfiguration can never stall a
// drain entirely.
func ResolveConcurrency(traits []string) int {
	limit := EvictionConcurrency
	for _, t := range traits {
		if traitLimit, ok := EvictionTraitConcurrency[t]; ok && traitLimit < limit {
			limit = traitLimit
		}
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}
