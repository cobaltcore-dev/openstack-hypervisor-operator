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

package openstack

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"unicode"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

func cleanPassword(password string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsGraphic(r) {
			return r
		}
		return -1
	}, password)
}

// GetServiceClient returns an gophercloud ServiceClient for the given serviceType.
func GetServiceClient(ctx context.Context, serviceType string) (*gophercloud.ServiceClient, error) {
	var authInfo *clientconfig.AuthInfo
	if osPWCmd := os.Getenv("OS_PW_CMD"); osPWCmd != "" {
		// run external command to get password
		cmd := exec.Command("sh", "-c", osPWCmd)
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		authInfo = &clientconfig.AuthInfo{
			Password: cleanPassword(string(out))}
	}

	return GetServiceClientAuth(ctx, serviceType, authInfo)
}

// GetServiceClient returns an gophercloud ServiceClient for the given serviceType.
func GetServiceClientAuth(ctx context.Context, serviceType string, authInfo *clientconfig.AuthInfo) (*gophercloud.ServiceClient, error) {
	if authInfo == nil {
		password := cleanPassword(os.Getenv("OS_PASSWORD"))
		authInfo = &clientconfig.AuthInfo{Password: password}
	}
	authInfo.AllowReauth = true

	var clientOpts clientconfig.ClientOpts
	clientOpts.AuthInfo = authInfo
	provider, err := clientconfig.AuthenticatedClient(ctx, &clientOpts)
	if err != nil {
		return nil, err
	}
	eo := gophercloud.EndpointOpts{}
	eo.ApplyDefaults(serviceType)

	// Override endpoint?
	var url string
	if url, err = provider.EndpointLocator(eo); err != nil {
		return nil, err
	}

	return &gophercloud.ServiceClient{
		ProviderClient: provider,
		Endpoint:       url,
		Type:           serviceType,
	}, nil
}
