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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type netboxDevice struct {
	Name string `json:"name"`
}

type netboxInterfaceItem struct {
	Device netboxDevice `json:"device"`
}

type netboxData struct {
	InterfaceList []netboxInterfaceItem `json:"interface_list"`
}

type netboxResponse struct {
	Data netboxData `json:"data"`
}

type netboxQuery struct {
	Query string `json:"query"`
}

// GetHostName retrieves the host name from Netbox by the given MAC address.
func GetHostName(ctx context.Context, macAddress string) (string, error) {
	// TODO: move to config/env
	graphql := "https://netbox.global.cloud.sap/graphql/"

	query := fmt.Sprintf(`	{
		interface_list(mac_address: "%v") {
			device {
				name
			}
		}
	}`, macAddress)

	payload := new(bytes.Buffer)
	if err := json.NewEncoder(payload).Encode(netboxQuery{Query: query}); err != nil {
		return "", err
	}
	r, err := http.NewRequest("POST", graphql, payload)
	if err != nil {
		return "", err
	}
	r.Header.Add("Content-Type", "application/json")

	c := http.DefaultClient
	res, err := c.Do(r.WithContext(ctx))
	if err != nil {
		return "", err
	}

	defer func() { _ = res.Body.Close() }()

	response := &netboxResponse{}
	err = json.NewDecoder(res.Body).Decode(response)
	if err != nil {
		return "", err
	}

	if len(response.Data.InterfaceList) == 0 {
		return "", fmt.Errorf("no device found for MAC address %v", macAddress)
	}

	return response.Data.InterfaceList[0].Device.Name, nil
}
