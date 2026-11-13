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

import "time"

const (
	// DefaultPollTime is the standard requeue interval controllers use while
	// waiting for a slow external state transition (an OpenStack migration, a
	// service being disabled, ...) to settle.
	DefaultPollTime = 10 * time.Second

	// ShortRetryTime is a brief requeue interval used to retry quickly when a
	// reconcile could make progress on the next pass without waiting for a full
	// poll cycle.
	ShortRetryTime = 1 * time.Second
)
