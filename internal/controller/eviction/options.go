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
	"strconv"
	"strings"
)

// Options configures the eviction controller.
type Options struct {
	Concurrency      int              `envconfig:"EVICTION_CONCURRENCY" default:"1"`
	TraitConcurrency TraitConcurrency `envconfig:"EVICTION_TRAIT_CONCURRENCY"`
}

// TraitConcurrency maps Placement trait names to migration concurrency limits.
type TraitConcurrency map[string]int

// Set parses a comma-separated list of TRAIT=N entries, replacing the map.
// Each limit must be a decimal integer >= 1. The map is only replaced after
// the whole list parsed successfully, so a bad entry (even one followed by a
// valid duplicate) is rejected as a whole and leaves the previous value.
func (t *TraitConcurrency) Set(value string) error {
	result := make(TraitConcurrency)
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		trait, limit, ok := strings.Cut(entry, "=")
		trait = strings.TrimSpace(trait)
		if !ok || trait == "" {
			return fmt.Errorf("invalid entry %q: expected TRAIT=N", entry)
		}
		limit = strings.TrimSpace(limit)
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 {
			return fmt.Errorf("--eviction-trait-concurrency (EVICTION_TRAIT_CONCURRENCY): trait %q must have an integer limit >= 1, got %q", trait, limit)
		}
		result[trait] = n
	}
	*t = result
	return nil
}

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
