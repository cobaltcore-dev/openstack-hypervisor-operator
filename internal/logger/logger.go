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
	"go.uber.org/zap/zapcore"
)

type wrapCore struct {
	zapcore.Core
}

func WrapCore(core zapcore.Core) zapcore.Core {
	return wrapCore{core}
}

// Check implements zapcore.Core.
func (w wrapCore) Check(entry zapcore.Entry, checkedEntry *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if entry.Message == "Reconciler error" {
		entry.Level = zapcore.DebugLevel
	}
	return w.Core.Check(entry, checkedEntry)
}

// With implements zapcore.Core.
func (w wrapCore) With(fields []zapcore.Field) zapcore.Core {
	return wrapCore{w.Core.With(fields)}
}
