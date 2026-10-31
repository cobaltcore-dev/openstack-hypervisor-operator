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

package config

import (
	"errors"
	"slices"
	"strings"

	"github.com/spf13/pflag"
)

// Shared contains configuration options used by more than one controller.
type Shared struct {
	LabelSelector   string
	AgentNamespaces []string
}

// AddFlags registers the shared options on the flag set.
func (s *Shared) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&s.LabelSelector, "label-selector", "",
		"Label selector to filter watched resources (namely nodes).")
	fs.StringSliceVar(&s.AgentNamespaces, "agent-namespaces", nil,
		"Comma-separated list of namespaces to search for agent pods (nova-compute, neutron) during offboarding.")
}

// Validate normalizes the agent namespace list and requires at least one
// namespace to remain.
func (s *Shared) Validate() error {
	namespaces := make([]string, 0, len(s.AgentNamespaces))
	for _, namespace := range s.AgentNamespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" || slices.Contains(namespaces, namespace) {
			continue
		}
		namespaces = append(namespaces, namespace)
	}
	s.AgentNamespaces = namespaces

	if len(namespaces) == 0 {
		return errors.New("--agent-namespaces (AGENT_NAMESPACES) must contain at least one namespace")
	}

	return nil
}
