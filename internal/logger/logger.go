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
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

func NewSanitzeReconcileErrorEncoder(cfg zapcore.EncoderConfig) zapcore.Encoder {
	return &SanitzeReconcileErrorEncoder{zapcore.NewConsoleEncoder(cfg), cfg}
}

type SanitzeReconcileErrorEncoder struct {
	zapcore.Encoder
	cfg zapcore.EncoderConfig
}

func (e *SanitzeReconcileErrorEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	if entry.Message == "Reconcile error" {
		// Downgrade the log level to debug to avoid log spam
		entry.Level = zapcore.WarnLevel
		entry.Stack = ""
	}
	return e.Encoder.EncodeEntry(entry, fields)
}

func (e *SanitzeReconcileErrorEncoder) Clone() zapcore.Encoder {
	return &SanitzeReconcileErrorEncoder{
		Encoder: e.Encoder.Clone(),
	}
}
