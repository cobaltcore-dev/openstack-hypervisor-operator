/*
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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

package eviction

import (
	"errors"
	"fmt"
	"strings"
)

// Options configures the eviction controller.
type Options struct {
	Concurrency      int            `envconfig:"EVICTION_CONCURRENCY" default:"1" desc:"Default maximum number of VM migrations to run concurrently while draining a hypervisor. Defaults to 1 (serial)."`
	TraitConcurrency map[string]int `envconfig:"EVICTION_TRAIT_CONCURRENCY" desc:"Per-host migration concurrency overrides. Set EVICTION_TRAIT_CONCURRENCY to comma-separated TRAIT:N entries, e.g. CUSTOM_HANA_EXCLUSIVE_HOST:1,CUSTOM_FOO:2. Integers use base 0 (0x10 is 16, 010 is 8). Repeat --eviction-trait-concurrency=TRAIT:N for CLI overrides of individual keys. Empty CLI values are invalid. The lowest matching trait limit wins."`
}

// TraitConcurrency maps Placement trait names to migration concurrency limits.
type TraitConcurrency = map[string]int

// ResolveConcurrency returns the maximum number of concurrent migrations
// for a host given its traits: the default, lowered by the lowest matching
// trait limit, and never below 1.
func (o Options) ResolveConcurrency(traits []string) int {
	limit := o.Concurrency
	for _, trait := range traits {
		if traitLimit, ok := o.TraitConcurrency[trait]; ok && traitLimit < limit {
			limit = traitLimit
		}
	}
	return max(limit, 1)
}

// Validate checks that the concurrency and every trait limit are >= 1 and
// that trait names are non-empty and carry no surrounding whitespace.
func (o Options) Validate() error {
	if o.Concurrency < 1 {
		return fmt.Errorf("--eviction-concurrency (EVICTION_CONCURRENCY) must be >= 1, got %d", o.Concurrency)
	}

	for trait, limit := range o.TraitConcurrency {
		if limit < 1 {
			return fmt.Errorf("--eviction-trait-concurrency (EVICTION_TRAIT_CONCURRENCY): trait %q must have an integer limit >= 1, got %d", trait, limit)
		}
		if trait != strings.TrimSpace(trait) {
			return fmt.Errorf("--eviction-trait-concurrency (EVICTION_TRAIT_CONCURRENCY): trait name %q must not contain surrounding whitespace", trait)
		}
		if trait == "" {
			return errors.New("--eviction-trait-concurrency (EVICTION_TRAIT_CONCURRENCY): trait name must not be empty")
		}
	}

	return nil
}
