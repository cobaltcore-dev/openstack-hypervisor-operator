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

package netbox

import (
	"context"
	"fmt"
)

// GetHostName retrieves the host name from Netbox by the given MAC address.
func (n *netboxClient) GetHostName(ctx context.Context, macAddress string) (string, error) {
	if err := n.pollClusters(ctx); err != nil {
		return "", err
	}

	hostname, found := n.hostnameForMac[macAddress]
	if !found {
		return "", fmt.Errorf("cannot find hostname for mac %q", macAddress)
	}

	return hostname, nil
}
