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

package logger

import (
	"github.com/go-logr/logr"
)

type wrapper struct {
	orig logr.LogSink
}

// Enabled implements logr.LogSink.
func (w wrapper) Enabled(level int) bool {
	return w.orig.Enabled(level)
}

// Error implements logr.LogSink.
func (w wrapper) Error(err error, msg string, keysAndValues ...any) {
	if msg == "Reconciler error" {
		w.orig.Info(2, msg, append(keysAndValues, "error", err.Error())...)
	} else {
		w.orig.Error(err, msg, keysAndValues...)
	}
}

// Info implements logr.LogSink.
func (w wrapper) Info(level int, msg string, keysAndValues ...any) {
	w.orig.Info(level, msg, keysAndValues...)
}

// Init implements logr.LogSink.
func (w wrapper) Init(info logr.RuntimeInfo) {
	w.orig.Init(info)
}

// WithName implements logr.LogSink.
func (w wrapper) WithName(name string) logr.LogSink {
	w.orig.WithName(name)
	return w
}

// WithValues implements logr.LogSink.
func (w wrapper) WithValues(keysAndValues ...any) logr.LogSink {
	w.orig.WithValues(keysAndValues...)
	return w
}

func WrapLogger(logger logr.Logger) logr.Logger {
	orig := logger.GetSink()
	return logger.WithSink(wrapper{orig})
}
