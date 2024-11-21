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
	"strings"
	"time"
)

type netboxQuery struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type Client interface {
	GetHostName(ctx context.Context, macAddress string) (string, error)
	GetIpsForHost(ctx context.Context, hostname string) ([]string, error)
	GetClusterNames(ctx context.Context) ([]string, error)
}

type netboxClient struct {
	graphQLURL     string
	nextPoll       time.Time
	region         string
	clusterTypeIDs []string
	hostnameForMac map[string]string
	ipsForHost     map[string][]string
	clusterNames   []string
}

func NewClient(graphQLURL, region string, clusterTypeIDs []string) *netboxClient {
	if !strings.HasSuffix(graphQLURL, "/") {
		graphQLURL = graphQLURL + "/"
	}
	return &netboxClient{graphQLURL, time.Now(), region, clusterTypeIDs, nil, nil, nil}
}

func (n *netboxClient) GetIpsForHost(ctx context.Context, hostname string) ([]string, error) {
	if err := n.pollClusters(ctx); err != nil {
		return nil, err
	}

	return n.ipsForHost[hostname], nil
}

func (n *netboxClient) GetClusterNames(ctx context.Context) ([]string, error) {
	if err := n.pollClusters(ctx); err != nil {
		return nil, err
	}

	return n.clusterNames, nil
}

func queryNetbox[ResponseType any](ctx context.Context, graphQLURL, query string, variables map[string]any, response *ResponseType) error {
	payload := new(bytes.Buffer)
	if err := json.NewEncoder(payload).Encode(netboxQuery{Query: query, Variables: variables}); err != nil {
		return err
	}

	r, err := http.NewRequest("POST", graphQLURL, payload)
	if err != nil {
		return err
	}
	r.Header.Add("Accept", "application/graphql-response+json")
	r.Header.Add("Content-Type", "application/json")

	c := http.DefaultClient
	res, err := c.Do(r.WithContext(ctx))
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("received unexpected status %v", res.Status)
	}

	defer func() { _ = res.Body.Close() }()

	err = json.NewDecoder(res.Body).Decode(response)
	if err != nil {
		return err
	}

	return nil
}
