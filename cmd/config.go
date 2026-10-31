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

package main

import (
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/config"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller/eviction"
)

type options struct {
	Shared      config.Shared
	Certificate controller.CertificateOptions
	Eviction    eviction.Options
}

// loadOptions reads the operator settings from the environment, starting from
// the built-in defaults for settings whose env var is not set. Each module
// owns its options struct; adding a new setting only means adding a struct
// field with a koanf tag — no per-variable code here and no flag registration.
func loadOptions() (options, error) {
	cfg := options{
		Certificate: controller.CertificateOptions{
			Namespace:  "monsoon3",
			IssuerName: "nova-hypervisor-agents-ca-issuer",
		},
		Eviction: eviction.Options{
			Concurrency: 1,
		},
	}
	k := koanf.New(".")

	// Environment variables use the same names as before, lowercased as keys.
	if err := k.Load(env.ProviderWithValue("", ".", func(name, value string) (string, any) {
		return strings.ToLower(name), value
	}), nil); err != nil {
		return cfg, err
	}

	// koanf's default Unmarshal composes duration and text-unmarshaler hooks,
	// but no string->[]string hook, so a new slice field would silently get a
	// single unsplit element. Provide the hooks explicitly instead of naming
	// individual variables: a generic decoder hook keeps every new []string
	// field working automatically, and TextUnmarshallerHookFunc keeps the
	// text-unmarshaler decoding (e.g. eviction.TraitConcurrency via
	// UnmarshalText) that the default unmarshal provides. Both hooks only act
	// on their target kind, so plain string fields stay untouched.
	decoderConfig := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToSliceHookFunc(","),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
	}

	for _, target := range []any{&cfg.Shared, &cfg.Certificate, &cfg.Eviction} {
		if err := k.UnmarshalWithConf("", target, koanf.UnmarshalConf{
			DecoderConfig: decoderConfig,
		}); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}
