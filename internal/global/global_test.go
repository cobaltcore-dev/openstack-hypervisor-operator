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

import "testing"

func TestResolveConcurrency(t *testing.T) {
	// Save and restore the package globals so tests don't leak into each other.
	origDefault := EvictionConcurrency
	origMap := EvictionTraitConcurrency
	t.Cleanup(func() {
		EvictionConcurrency = origDefault
		EvictionTraitConcurrency = origMap
	})

	tests := []struct {
		name       string
		defaultVal int
		traitMap   map[string]int
		traits     []string
		want       int
	}{
		{
			name:       "no traits falls back to default",
			defaultVal: 3,
			traitMap:   map[string]int{"CUSTOM_HANA_EXCLUSIVE_HOST": 1},
			traits:     nil,
			want:       3,
		},
		{
			name:       "non-matching trait falls back to default",
			defaultVal: 3,
			traitMap:   map[string]int{"CUSTOM_HANA_EXCLUSIVE_HOST": 1},
			traits:     []string{"CUSTOM_SOMETHING_ELSE"},
			want:       3,
		},
		{
			name:       "matching trait overrides default",
			defaultVal: 5,
			traitMap:   map[string]int{"CUSTOM_HANA_EXCLUSIVE_HOST": 1},
			traits:     []string{"CUSTOM_HANA_EXCLUSIVE_HOST"},
			want:       1,
		},
		{
			name:       "lowest matching trait wins",
			defaultVal: 10,
			traitMap:   map[string]int{"CUSTOM_A": 4, "CUSTOM_B": 2, "CUSTOM_C": 7},
			traits:     []string{"CUSTOM_A", "CUSTOM_B", "CUSTOM_C"},
			want:       2,
		},
		{
			name:       "trait limit above default is ignored (default is the cap)",
			defaultVal: 2,
			traitMap:   map[string]int{"CUSTOM_BIG": 8},
			traits:     []string{"CUSTOM_BIG"},
			want:       2,
		},
		{
			name:       "result is clamped to at least 1",
			defaultVal: 0,
			traitMap:   map[string]int{},
			traits:     nil,
			want:       1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			EvictionConcurrency = tt.defaultVal
			EvictionTraitConcurrency = tt.traitMap
			if got := ResolveConcurrency(tt.traits); got != tt.want {
				t.Errorf("ResolveConcurrency(%v) = %d, want %d", tt.traits, got, tt.want)
			}
		})
	}
}
