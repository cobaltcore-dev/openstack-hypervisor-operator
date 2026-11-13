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
	"encoding/json"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"

	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller/eviction"
)

type options struct {
	controller.Shared `koanf:",squash"`
	Certificate       controller.CertificateOptions `koanf:",squash"`
	Eviction          eviction.Options              `koanf:",squash"`
}

// loadOptions reads the operator settings from the environment, starting from
// the built-in defaults for settings whose env var is not set. Each module
// owns its options struct and defaults. A new tagged field needs no loader
// changes or flag registration.
func loadOptions() (options, error) {
	cfg := options{
		Shared:      controller.DefaultOptions(),
		Certificate: controller.DefaultCertificateOptions(),
		Eviction:    eviction.DefaultOptions(),
	}
	k := koanf.New(".")

	// Environment variables use the same names as before, lowercased as keys.
	if err := k.Load(env.Provider("", ".", strings.ToLower), nil); err != nil {
		return cfg, err
	}

	// Decode lists and JSON maps by target type. Keep text unmarshaling for
	// other types. Plain strings, including label selectors, stay unchanged.
	decoderConfig := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToSliceHookFunc(","),
			stringToMapJSONHook,
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
	}

	err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		DecoderConfig: decoderConfig,
	})
	return cfg, err
}

// stringToMapJSONHook decodes JSON into a fresh map of the target type.
// An empty environment value clears the map. Errors do not change the target.
func stringToMapJSONHook(from, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.String || to.Kind() != reflect.Map {
		return data, nil
	}
	result := reflect.New(to)
	if text := data.(string); text != "" {
		if err := json.Unmarshal([]byte(text), result.Interface()); err != nil {
			return nil, err
		}
	}
	return result.Elem().Interface(), nil
}
