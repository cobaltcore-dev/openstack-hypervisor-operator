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

package controller

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ---------------------------------------------------------------------------
// Stateful mock for OpenStack endpoints
//
// mockState is a thread-safe in-memory representation of a single Nova
// compute node and its associated Placement/Glance/Neutron state.  The
// handlers below implement only the subset of the OpenStack API that the
// controllers under test actually call.
// ---------------------------------------------------------------------------

type mockAggregate struct {
	ID    int
	Name  string
	UUID  string
	Hosts []string
}

type mockServer struct {
	ID     string
	Name   string
	Status string
}

type mockState struct {
	mu sync.Mutex

	// Nova hypervisor identity
	hypervisorID string
	serviceID    string

	// Nova service state
	serviceEnabled    bool
	serviceForcedDown bool

	// Aggregates (keyed by aggregate ID)
	aggregates map[int]*mockAggregate

	// Placement resource-provider traits
	traits       []string
	rpGeneration int

	// Test VMs created during the smoke-test phase (keyed by server ID)
	servers map[string]*mockServer
}

func newMockState() *mockState {
	return &mockState{
		hypervisorID:      "hv-uuid-1234",
		serviceID:         "service-id-1",
		serviceEnabled:    false,
		serviceForcedDown: true,
		aggregates: map[int]*mockAggregate{
			1: {ID: 1, Name: "test-az", UUID: "az-uuid-1", Hosts: []string{}},
			2: {ID: 2, Name: testAggregateName, UUID: "test-agg-uuid-2", Hosts: []string{}},
			3: {ID: 3, Name: "prod-aggregate", UUID: "prod-uuid-3", Hosts: []string{}},
		},
		traits:       []string{"HW_CPU_X86_VMX"},
		rpGeneration: 1,
		servers:      map[string]*mockServer{},
	}
}

// ---------------------------------------------------------------------------
// Handler registration
// ---------------------------------------------------------------------------

// registerHandlers wires all OpenStack API routes onto mux.
// A catch-all route at "/" fails the test on any unrecognised request so
// that missing stubs are caught immediately rather than silently returning
// empty responses.
func (s *mockState) registerHandlers(mux *http.ServeMux, hvName string) {
	// Catch-all: fail on any unhandled request
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			body = fmt.Appendf(nil, "<read error: %v>", err)
		}
		Fail(fmt.Sprintf("Unhandled request to fake OpenStack server: %s %s body=%s",
			r.Method, r.URL.String(), string(body)))
	})

	// Nova: Hypervisors
	mux.HandleFunc("GET /os-hypervisors/detail", s.handleListHypervisors(hvName))

	// Nova: Services
	mux.HandleFunc("PUT /os-services/service-id-1", s.handleUpdateService)

	// Nova: Aggregates
	mux.HandleFunc("GET /os-aggregates", s.handleListAggregates)
	mux.HandleFunc("POST /os-aggregates/1/action", s.handleAggregateAction(1))
	mux.HandleFunc("POST /os-aggregates/2/action", s.handleAggregateAction(2))
	mux.HandleFunc("POST /os-aggregates/3/action", s.handleAggregateAction(3))

	// Placement: Traits
	mux.HandleFunc("GET /resource_providers/hv-uuid-1234/traits", s.handleGetTraits)
	mux.HandleFunc("PUT /resource_providers/hv-uuid-1234/traits", s.handlePutTraits)

	// Nova: Flavors (used by test compute client during smoke test)
	mux.HandleFunc("GET /flavors/detail", s.handleListFlavors)

	// Glance: Images (test image client)
	mux.HandleFunc("GET /images", s.handleListImages)

	// Neutron: Networks (test network client)
	mux.HandleFunc("GET /networks", s.handleListNetworks)

	// Nova: Servers (smoke-test VM lifecycle)
	mux.HandleFunc("POST /servers", s.handleCreateServer)
	mux.HandleFunc("GET /servers/detail", s.handleListServersDetail)
	mux.HandleFunc("GET /servers", s.handleListServers)
	mux.HandleFunc("GET /servers/", s.handleServerGet)
	mux.HandleFunc("DELETE /servers/", s.handleServerDelete)
	mux.HandleFunc("POST /servers/", s.handleServerAction)
}

// ---------------------------------------------------------------------------
// Handler implementations
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprint(w, body)
}

func (s *mockState) handleListHypervisors(hvName string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		writeJSON(w, http.StatusOK, fmt.Sprintf(`{
			"hypervisors": [{
				"service": {"host": "compute-host", "id": %q, "disabled_reason": ""},
				"cpu_info": {"arch": "x86_64", "model": "Nehalem", "vendor": "Intel",
				             "features": ["pge"], "topology": {"cores": 1, "threads": 1, "sockets": 4}},
				"current_workload": 0,
				"status": "enabled",
				"state": "up",
				"disk_available_least": 0,
				"host_ip": "1.1.1.1",
				"free_disk_gb": 1028,
				"free_ram_mb": 7680,
				"hypervisor_hostname": %q,
				"hypervisor_type": "fake",
				"hypervisor_version": 2002000,
				"id": %q,
				"local_gb": 1028,
				"local_gb_used": 0,
				"memory_mb": 8192,
				"memory_mb_used": 512,
				"running_vms": 0,
				"vcpus": 1,
				"vcpus_used": 0
			}]
		}`, s.serviceID, hvName, s.hypervisorID))
	}
}

func (s *mockState) handleUpdateService(w http.ResponseWriter, r *http.Request) {
	defer GinkgoRecover()
	s.mu.Lock()
	defer s.mu.Unlock()

	bodyBytes, err := io.ReadAll(r.Body)
	Expect(err).NotTo(HaveOccurred())

	var req map[string]any
	Expect(json.Unmarshal(bodyBytes, &req)).To(Succeed())

	if status, ok := req["status"].(string); ok {
		s.serviceEnabled = (status == "enabled")
	}
	if fd, ok := req["forced_down"].(bool); ok {
		s.serviceForcedDown = fd
	}

	statusStr := "disabled"
	if s.serviceEnabled {
		statusStr = "enabled"
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"service": {"id": %q, "status": %q}}`, s.serviceID, statusStr))
}

func (s *mockState) handleListAggregates(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var parts []string
	for _, agg := range s.aggregates {
		hostsJSON, err := json.Marshal(agg.Hosts)
		Expect(err).NotTo(HaveOccurred())
		parts = append(parts, fmt.Sprintf(`{
			"id": %d, "name": %q, "uuid": %q,
			"hosts": %s, "availability_zone": null, "metadata": {}
		}`, agg.ID, agg.Name, agg.UUID, hostsJSON))
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"aggregates": [%s]}`, strings.Join(parts, ",")))
}

func (s *mockState) handleAggregateAction(aggID int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		s.mu.Lock()
		defer s.mu.Unlock()

		bodyBytes, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())

		var req map[string]map[string]string
		Expect(json.Unmarshal(bodyBytes, &req)).To(Succeed())

		agg, exists := s.aggregates[aggID]
		Expect(exists).To(BeTrue(), "aggregate %d not found", aggID)

		if addHost, ok := req["add_host"]; ok {
			host := addHost["host"]
			if !slices.Contains(agg.Hosts, host) {
				agg.Hosts = append(agg.Hosts, host)
			}
		}
		if removeHost, ok := req["remove_host"]; ok {
			host := removeHost["host"]
			filtered := agg.Hosts[:0]
			for _, h := range agg.Hosts {
				if h != host {
					filtered = append(filtered, h)
				}
			}
			agg.Hosts = filtered
		}

		hostsJSON, err := json.Marshal(agg.Hosts)
		Expect(err).NotTo(HaveOccurred())
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{
			"aggregate": {
				"id": %d, "name": %q, "uuid": %q,
				"hosts": %s, "availability_zone": null, "metadata": {}
			}
		}`, agg.ID, agg.Name, agg.UUID, hostsJSON))
	}
}

func (s *mockState) handleGetTraits(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	traitsJSON, err := json.Marshal(s.traits)
	Expect(err).NotTo(HaveOccurred())
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{
		"traits": %s,
		"resource_provider_generation": %d
	}`, traitsJSON, s.rpGeneration))
}

func (s *mockState) handlePutTraits(w http.ResponseWriter, r *http.Request) {
	defer GinkgoRecover()
	s.mu.Lock()
	defer s.mu.Unlock()

	bodyBytes, err := io.ReadAll(r.Body)
	Expect(err).NotTo(HaveOccurred())

	var req struct {
		Traits                     []string `json:"traits"`
		ResourceProviderGeneration int      `json:"resource_provider_generation"`
	}
	Expect(json.Unmarshal(bodyBytes, &req)).To(Succeed())

	s.traits = req.Traits
	s.rpGeneration++

	traitsJSON, err := json.Marshal(s.traits)
	Expect(err).NotTo(HaveOccurred())
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{
		"traits": %s,
		"resource_provider_generation": %d
	}`, traitsJSON, s.rpGeneration))
}

func (s *mockState) handleListFlavors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{
		"flavors": [{
			"id": "flavor-1",
			"name": "c_k_c2_m2_v2",
			"ram": 2048,
			"vcpus": 2,
			"disk": 0,
			"swap": 0,
			"OS-FLV-DISABLED:disabled": false,
			"OS-FLV-EXT-DATA:ephemeral": 0,
			"os-flavor-access:is_public": true,
			"rxtx_factor": 1.0,
			"description": null,
			"extra_specs": {},
			"links": []
		}]
	}`)
}

func (s *mockState) handleListImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{
		"images": [{
			"id": "image-1",
			"name": "cirros-kvm",
			"status": "active",
			"visibility": "public",
			"container_format": "bare",
			"disk_format": "qcow2",
			"min_disk": 0,
			"min_ram": 0,
			"size": 13167616,
			"created_at": "2024-01-01T00:00:00Z",
			"updated_at": "2024-01-01T00:00:00Z",
			"self": "/v2/images/image-1",
			"file": "/v2/images/image-1/file",
			"schema": "/v2/schemas/image"
		}]
	}`)
}

func (s *mockState) handleListNetworks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{
		"networks": [{
			"id": "network-1",
			"name": "test-net",
			"admin_state_up": true,
			"shared": false,
			"status": "ACTIVE",
			"subnets": [],
			"tenant_id": "project-id",
			"project_id": "project-id"
		}]
	}`)
}

func (s *mockState) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	defer GinkgoRecover()
	s.mu.Lock()
	defer s.mu.Unlock()

	bodyBytes, err := io.ReadAll(r.Body)
	Expect(err).NotTo(HaveOccurred())

	var req struct {
		Server struct {
			Name string `json:"name"`
		} `json:"server"`
	}
	Expect(json.Unmarshal(bodyBytes, &req)).To(Succeed())

	serverID := "server-" + uuid.New().String()[:8]
	s.servers[serverID] = &mockServer{ID: serverID, Name: req.Server.Name, Status: "BUILD"}

	writeJSON(w, http.StatusAccepted, fmt.Sprintf(`{
		"server": {"id": %q, "name": %q, "status": "BUILD"}
	}`, serverID, req.Server.Name))
}

func (s *mockState) handleListServersDetail(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nameFilter := r.URL.Query().Get("name")
	var parts []string
	for _, srv := range s.servers {
		if nameFilter != "" && !strings.HasPrefix(srv.Name, nameFilter) {
			continue
		}
		// Simulate async boot: BUILD → ACTIVE on first list
		if srv.Status == "BUILD" {
			srv.Status = "ACTIVE"
		}
		parts = append(parts, fmt.Sprintf(
			`{"id": %q, "name": %q, "status": %q}`, srv.ID, srv.Name, srv.Status))
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"servers": [%s], "servers_links": []}`, strings.Join(parts, ",")))
}

func (s *mockState) handleListServers(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nameFilter := r.URL.Query().Get("name")
	var parts []string
	for _, srv := range s.servers {
		if nameFilter != "" && !strings.HasPrefix(srv.Name, nameFilter) {
			continue
		}
		parts = append(parts, fmt.Sprintf(
			`{"id": %q, "name": %q, "status": %q}`, srv.ID, srv.Name, srv.Status))
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"servers": [%s], "servers_links": []}`, strings.Join(parts, ",")))
}

func (s *mockState) handleServerGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	serverID := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")[0]
	srv, exists := s.servers[serverID]
	if !exists {
		writeJSON(w, http.StatusNotFound, `{"itemNotFound": {"message": "server not found"}}`)
		return
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(
		`{"server": {"id": %q, "name": %q, "status": %q}}`, srv.ID, srv.Name, srv.Status))
}

func (s *mockState) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	serverID := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")[0]
	if _, exists := s.servers[serverID]; !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(s.servers, serverID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *mockState) handleServerAction(w http.ResponseWriter, r *http.Request) {
	defer GinkgoRecover()
	s.mu.Lock()
	defer s.mu.Unlock()

	serverID := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")[0]
	srv, exists := s.servers[serverID]
	if !exists {
		writeJSON(w, http.StatusNotFound, `{"itemNotFound": {"message": "server not found"}}`)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	Expect(err).NotTo(HaveOccurred())

	var req map[string]any
	Expect(json.Unmarshal(bodyBytes, &req)).To(Succeed())

	if _, ok := req["os-getConsoleOutput"]; ok {
		// Return output containing the server name — the magic string the
		// smoke-test looks for to confirm the VM booted successfully.
		writeJSON(w, http.StatusOK,
			fmt.Sprintf(`{"output": "FAKE CONSOLE OUTPUT\n%s\nDONE\n"}`, srv.Name))
		return
	}

	Fail(fmt.Sprintf("Unhandled server action for %s: %s", serverID, string(bodyBytes)))
}
