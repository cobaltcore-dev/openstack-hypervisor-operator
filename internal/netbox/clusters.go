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
	"errors"
	"strings"
	"time"
)

type ipAddress struct {
	Address string `json:"address"`
}

type ipsInterface struct {
	MacAddress  string      `json:"mac_address"`
	IPAddresses []ipAddress `json:"ip_addresses"`
}

type ipsDevice struct {
	Name       string         `json:"name"`
	Interfaces []ipsInterface `json:"interfaces"`
}

type clusterListItem struct {
	Name    string      `json:"name"`
	Devices []ipsDevice `json:"devices"`
}

type ipsData struct {
	ClusterList []clusterListItem `json:"cluster_list"`
}

type ipsResponse struct {
	Data ipsData `json:"data"`
}

const _QUERY_CLUSTER = `query pollClusters($region: [String!]!, $type_id: [String!]!) {
	cluster_list(region: $region, type_id: $type_id) {
		name devices {
			name interfaces {
				mac_address ip_addresses {
					address
				}
			}
		}
	}
}`

// GetHostName retrieves the host name from Netbox by the given MAC address.
func (n *netboxClient) pollClusters(ctx context.Context) error {
	if n.nextPoll.After(time.Now()) {
		return nil
	}

	vars := map[string]any{"region": n.region, "type_id": n.clusterTypeIDs}
	response := &ipsResponse{}
	if err := queryNetbox(ctx, n.graphQLURL, _QUERY_CLUSTER, vars, response); err != nil {
		return err
	}

	if len(response.Data.ClusterList) == 0 {
		return errors.New("could not find any matching clusters / hosts")
	}

	clusterNames := make([]string, 0)
	ipsForHost := make(map[string][]string)
	hostnameForMac := make(map[string]string)

	for _, netboxCluster := range response.Data.ClusterList {
		clusterNames = append(clusterNames, netboxCluster.Name)
		for _, device := range netboxCluster.Devices {
			ipList := make([]string, 0)
			for _, intf := range device.Interfaces {
				if intf.MacAddress != "" {
					mac := strings.ToLower(intf.MacAddress)
					hostnameForMac[mac] = device.Name
				}

				for _, addr := range intf.IPAddresses {
					ip, _, _ := strings.Cut(addr.Address, "/")
					ipList = append(ipList, ip)
				}
			}

			ipsForHost[device.Name] = ipList
		}
	}

	n.clusterNames = clusterNames
	n.ipsForHost = ipsForHost
	n.hostnameForMac = hostnameForMac
	n.nextPoll = time.Now().Add(15 * time.Minute) // TODO: Make it configurable
	return nil
}
