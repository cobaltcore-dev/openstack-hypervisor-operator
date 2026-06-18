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
	"sync"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
	"golang.org/x/sync/singleflight"
)

// providerCache caches ProviderClients keyed by a string derived from the
// authInfo, so multiple GetServiceClient calls with the same credentials
// only authenticate against Keystone once.
var (
	providerCacheMu sync.Mutex
	providerCache   = map[string]*gophercloud.ProviderClient{}
	providerGroup   singleflight.Group
)

func cacheKey(authInfo *clientconfig.AuthInfo) string {
	if authInfo == nil {
		return ""
	}
	// Include all fields that affect the resulting Keystone token and catalog
	// so a future auth context differing only in user, domain, or AuthURL
	// doesn't silently reuse the wrong cached provider.
	return strings.Join([]string{
		authInfo.AuthURL,
		authInfo.ProjectName,
		authInfo.ProjectDomainName,
		authInfo.Username,
		authInfo.UserDomainName,
	}, "\x00")
}

// GetServiceClient returns a ServiceClient for the given serviceType.
// Providers are cached per auth context so Keystone is only hit once per
// unique set of credentials across all SetupWithManager calls. Concurrent
// callers with the same key share a single in-flight request via singleflight,
// preventing duplicate Keystone round-trips on startup.
func GetServiceClient(ctx context.Context, serviceType string, authInfo *clientconfig.AuthInfo) (*gophercloud.ServiceClient, error) {
	key := cacheKey(authInfo)

	providerCacheMu.Lock()
	provider, ok := providerCache[key]
	providerCacheMu.Unlock()

	if !ok {
		v, err, _ := providerGroup.Do(key, func() (any, error) {
			// Re-check under the group: another goroutine may have populated
			// the cache while we were waiting for the singleflight slot.
			providerCacheMu.Lock()
			if p, hit := providerCache[key]; hit {
				providerCacheMu.Unlock()
				return p, nil
			}
			providerCacheMu.Unlock()

			p, err := NewProviderClient(ctx, authInfo)
			if err != nil {
				return nil, err
			}
			providerCacheMu.Lock()
			providerCache[key] = p
			providerCacheMu.Unlock()
			return p, nil
		})
		if err != nil {
			return nil, err
		}
		provider = v.(*gophercloud.ProviderClient)
	}

	return ServiceClientFromProvider(provider, serviceType)
}

// NewProviderClient authenticates against OpenStack and returns a ProviderClient
// that can be reused across multiple service clients via ServiceClientFromProvider,
// avoiding repeated Keystone round-trips and catalog deserialisations.
func NewProviderClient(ctx context.Context, authInfo *clientconfig.AuthInfo) (*gophercloud.ProviderClient, error) {
	if authInfo == nil {
		authInfo = &clientconfig.AuthInfo{}
	}
	authInfo.AllowReauth = true

	if osPWCmd := os.Getenv("OS_PW_CMD"); osPWCmd != "" {
		// run external command to get password
		cmd := exec.Command("sh", "-c", osPWCmd) // #nosec G702 -- OS_PW_CMD is set by operator/admin, not user input
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		authInfo.Password = strings.TrimSuffix(string(out), "\n")
	}

	var clientOpts clientconfig.ClientOpts
	clientOpts.AuthInfo = authInfo
	return clientconfig.AuthenticatedClient(ctx, &clientOpts)
}

// ServiceClientFromProvider returns a ServiceClient for the given serviceType
// using an already-authenticated ProviderClient.
func ServiceClientFromProvider(provider *gophercloud.ProviderClient, serviceType string) (*gophercloud.ServiceClient, error) {
	eo := gophercloud.EndpointOpts{}
	eo.ApplyDefaults(serviceType)
	url, err := provider.EndpointLocator(eo)
	if err != nil {
		return nil, err
	}
	return &gophercloud.ServiceClient{
		ProviderClient: provider,
		Endpoint:       url,
		Type:           serviceType,
	}, nil
}
