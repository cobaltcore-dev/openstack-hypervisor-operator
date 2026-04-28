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

package utils

import "slices"

// Difference returns all elements in s2 that are not in s1.
func Difference[S ~[]E, E comparable](s1, s2 S) S {
	diff := make(S, 0)
	for item := range slices.Values(s2) {
		if !slices.Contains(s1, item) {
			diff = append(diff, item)
		}
	}

	return diff
}
