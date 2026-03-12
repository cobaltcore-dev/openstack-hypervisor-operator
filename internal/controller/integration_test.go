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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/v2/testhelper"
	thclient "github.com/gophercloud/gophercloud/v2/testhelper/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
)

// ---------------------------------------------------------------------------
// Stateful mock state for OpenStack endpoints
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

	// Aggregates (keyed by ID)
	aggregates map[int]*mockAggregate

	// Placement traits
	traits       []string
	rpGeneration int

	// Test VMs (keyed by server ID)
	servers map[string]*mockServer

	// HA service
	haEnabled bool

	// K8s client for dynamic lookups (e.g. Hypervisor UID)
	k8sClient client.Client
	hvName    string
}

func newMockState(k8sClient client.Client, hvName string) *mockState {
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
		haEnabled:    false,
		k8sClient:    k8sClient,
		hvName:       hvName,
	}
}

// ---------------------------------------------------------------------------
// Helper: JSON writing
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprint(w, body)
}

// ---------------------------------------------------------------------------
// Mock handler registration
// ---------------------------------------------------------------------------

func (s *mockState) registerHandlers(mux *http.ServeMux) {
	// Catch-all: fail on any unhandled request
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		defer GinkgoRecover()
		body, err := io.ReadAll(r.Body)
		Expect(err).NotTo(HaveOccurred())
		Fail(fmt.Sprintf("Unhandled request to fake server: %s %s body=%s", r.Method, r.URL.String(), string(body)))
	})

	// --- Nova: Hypervisors ---
	mux.HandleFunc("GET /os-hypervisors/detail", s.handleListHypervisors)

	// --- Nova: Services ---
	mux.HandleFunc("PUT /os-services/service-id-1", s.handleUpdateService)

	// --- Nova: Aggregates ---
	mux.HandleFunc("GET /os-aggregates", s.handleListAggregates)
	mux.HandleFunc("POST /os-aggregates/1/action", s.handleAggregateAction(1))
	mux.HandleFunc("POST /os-aggregates/2/action", s.handleAggregateAction(2))
	mux.HandleFunc("POST /os-aggregates/3/action", s.handleAggregateAction(3))

	// --- Placement: Traits ---
	mux.HandleFunc("GET /resource_providers/hv-uuid-1234/traits", s.handleGetTraits)
	mux.HandleFunc("PUT /resource_providers/hv-uuid-1234/traits", s.handlePutTraits)

	// --- Nova: Flavors (test compute client) ---
	mux.HandleFunc("GET /flavors/detail", s.handleListFlavors)

	// --- Glance: Images (test image client, no ResourceBase so path is /images) ---
	mux.HandleFunc("GET /images", s.handleListImages)

	// --- Neutron: Networks (test network client, no ResourceBase so path is /networks) ---
	mux.HandleFunc("GET /networks", s.handleListNetworks)

	// --- Nova: Servers ---
	mux.HandleFunc("POST /servers", s.handleCreateServer)
	mux.HandleFunc("GET /servers/detail", s.handleListServersDetail)
	mux.HandleFunc("GET /servers", s.handleListServers)

	// Dynamic server routes: individual server operations (GET, DELETE, POST action).
	mux.HandleFunc("GET /servers/", s.handleServerGet)
	mux.HandleFunc("DELETE /servers/", s.handleServerDelete)
	mux.HandleFunc("POST /servers/", s.handleServerAction)

	// --- HA Service ---
	mux.HandleFunc("POST /instance-ha", s.handleInstanceHA)
}

// --- Handler implementations ---

func (s *mockState) handleListHypervisors(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hostname := s.hvName
	body := fmt.Sprintf(`{
		"hypervisors": [{
			"service": {"host": "compute-host", "id": "%s", "disabled_reason": ""},
			"cpu_info": {"arch": "x86_64", "model": "Nehalem", "vendor": "Intel", "features": ["pge"], "topology": {"cores": 1, "threads": 1, "sockets": 4}},
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
			"id": "%s",
			"local_gb": 1028,
			"local_gb_used": 0,
			"memory_mb": 8192,
			"memory_mb_used": 512,
			"running_vms": 0,
			"vcpus": 1,
			"vcpus_used": 0
		}]
	}`, s.serviceID, hostname, s.hypervisorID)
	writeJSON(w, http.StatusOK, body)
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

	status := "disabled"
	if s.serviceEnabled {
		status = "enabled"
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"service": {"id": "%s", "status": "%s"}}`, s.serviceID, status))
}

func (s *mockState) handleListAggregates(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var parts []string
	for _, agg := range s.aggregates {
		hostsJSON, err := json.Marshal(agg.Hosts)
		Expect(err).NotTo(HaveOccurred())
		parts = append(parts, fmt.Sprintf(`{
			"id": %d,
			"name": %q,
			"uuid": %q,
			"hosts": %s,
			"availability_zone": null,
			"metadata": {}
		}`, agg.ID, agg.Name, agg.UUID, string(hostsJSON)))
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
			newHosts := make([]string, 0, len(agg.Hosts))
			for _, h := range agg.Hosts {
				if h != host {
					newHosts = append(newHosts, h)
				}
			}
			agg.Hosts = newHosts
		}

		hostsJSON, err := json.Marshal(agg.Hosts)
		Expect(err).NotTo(HaveOccurred())
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{
			"aggregate": {
				"id": %d,
				"name": %q,
				"uuid": %q,
				"hosts": %s,
				"availability_zone": null,
				"metadata": {}
			}
		}`, agg.ID, agg.Name, agg.UUID, string(hostsJSON)))
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
	}`, string(traitsJSON), s.rpGeneration))
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
	}`, string(traitsJSON), s.rpGeneration))
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
	srv := &mockServer{
		ID:     serverID,
		Name:   req.Server.Name,
		Status: "BUILD",
	}
	s.servers[serverID] = srv

	writeJSON(w, http.StatusAccepted, fmt.Sprintf(`{
		"server": {
			"id": "%s",
			"name": "%s",
			"status": "BUILD"
		}
	}`, srv.ID, srv.Name))
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
		// Transition BUILD -> ACTIVE on list (simulates async boot completing)
		if srv.Status == "BUILD" {
			srv.Status = "ACTIVE"
		}
		parts = append(parts, fmt.Sprintf(`{
			"id": "%s",
			"name": "%s",
			"status": "%s"
		}`, srv.ID, srv.Name, srv.Status))
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"servers": [%s], "servers_links": []}`, strings.Join(parts, ",")))
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
		parts = append(parts, fmt.Sprintf(`{
			"id": "%s",
			"name": "%s",
			"status": "%s"
		}`, srv.ID, srv.Name, srv.Status))
	}
	writeJSON(w, http.StatusOK, fmt.Sprintf(`{"servers": [%s], "servers_links": []}`, strings.Join(parts, ",")))
}

func (s *mockState) handleServerGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")
	serverID := parts[0]

	srv, exists := s.servers[serverID]
	if !exists {
		writeJSON(w, http.StatusNotFound, `{"itemNotFound": {"message": "server not found"}}`)
		return
	}

	writeJSON(w, http.StatusOK, fmt.Sprintf(`{
		"server": {
			"id": "%s",
			"name": "%s",
			"status": "%s"
		}
	}`, srv.ID, srv.Name, srv.Status))
}

func (s *mockState) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")
	serverID := parts[0]

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

	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/servers/"), "/")
	serverID := pathParts[0]

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
		// Return console output containing the server name (magic string)
		writeJSON(w, http.StatusOK, fmt.Sprintf(`{"output": "FAKE CONSOLE OUTPUT\n%s\nDONE\n"}`, srv.Name))
		return
	}

	// Unhandled action
	Fail(fmt.Sprintf("Unhandled server action for %s: %s", serverID, string(bodyBytes)))
}

func (s *mockState) handleInstanceHA(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.haEnabled = true
	writeJSON(w, http.StatusOK, `{}`)
}

// ---------------------------------------------------------------------------
// Integration test
// ---------------------------------------------------------------------------

var _ = Describe("Integration: Full Onboarding Lifecycle", func() {
	const (
		eventuallyTimeout = 2 * time.Minute
		pollingInterval   = 500 * time.Millisecond
	)

	// Shared helper to get the Hypervisor
	getHypervisor := func(ctx context.Context, name string) func() (*kvmv1.Hypervisor, error) {
		return func() (*kvmv1.Hypervisor, error) {
			hv := &kvmv1.Hypervisor{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, hv)
			return hv, err
		}
	}

	setupAndRun := func(nodeName, lifecycleLabel string) {
		var (
			fakeServer testhelper.FakeServer
			state      *mockState
			mgrCancel  context.CancelFunc
		)

		BeforeEach(func(ctx SpecContext) {
			By("Setting up the OpenStack fake server")
			fakeServer = testhelper.SetupHTTP()
			DeferCleanup(fakeServer.Teardown)

			// Point HA service URL at our fake server
			os.Setenv("KVM_HA_SERVICE_URL", fakeServer.Endpoint()+"instance-ha")
			DeferCleanup(func() { os.Unsetenv("KVM_HA_SERVICE_URL") })

			// Create stateful mock
			state = newMockState(k8sClient, nodeName)
			state.registerHandlers(fakeServer.Mux)

			By("Creating a controller manager with all controllers")
			skipNameValidation := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme: scheme.Scheme,
				Metrics: metricsserver.Options{
					BindAddress: "0", // disable metrics server
				},
				Controller: config.Controller{
					SkipNameValidation: &skipNameValidation,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Build service clients pointing at the fake server
			computeClient := thclient.ServiceClient(fakeServer)
			testComputeClient := thclient.ServiceClient(fakeServer)
			testImageClient := thclient.ServiceClient(fakeServer)
			testNetworkClient := thclient.ServiceClient(fakeServer)
			placementClient := thclient.ServiceClient(fakeServer)

			// -- Controllers without OpenStack deps --
			hvCtrl := &HypervisorController{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(hvCtrl.SetupWithManager(mgr)).To(Succeed())

			gardenerCtrl := &GardenerNodeLifecycleController{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(gardenerCtrl.SetupWithManager(mgr, "default")).To(Succeed())

			// -- Controllers with OpenStack deps (use registerWithManager) --
			onboardingCtrl := &OnboardingController{
				Client:            mgr.GetClient(),
				Scheme:            mgr.GetScheme(),
				computeClient:     computeClient,
				testComputeClient: testComputeClient,
				testImageClient:   testImageClient,
				testNetworkClient: testNetworkClient,
				requeueInterval:   1 * time.Second,
			}
			Expect(onboardingCtrl.registerWithManager(mgr)).To(Succeed())

			aggregatesCtrl := &AggregatesController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: computeClient,
			}
			Expect(aggregatesCtrl.registerWithManager(mgr)).To(Succeed())

			traitsCtrl := &TraitsController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				serviceClient: placementClient,
			}
			Expect(traitsCtrl.registerWithManager(mgr)).To(Succeed())

			maintenanceCtrl := &HypervisorMaintenanceController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: computeClient,
			}
			Expect(maintenanceCtrl.registerWithManager(mgr)).To(Succeed())

			decommissionCtrl := &NodeDecommissionReconciler{
				Client:          mgr.GetClient(),
				Scheme:          mgr.GetScheme(),
				computeClient:   computeClient,
				placementClient: placementClient,
			}
			Expect(decommissionCtrl.registerWithManager(mgr)).To(Succeed())

			evictionCtrl := &EvictionReconciler{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: computeClient,
			}
			Expect(evictionCtrl.registerWithManager(mgr)).To(Succeed())

			By("Starting the manager")
			var mgrCtx context.Context
			mgrCtx, mgrCancel = context.WithCancel(context.Background())
			go func() {
				defer GinkgoRecover()
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()

			// Wait for caches to sync
			Eventually(func() bool {
				return mgr.GetCache().WaitForCacheSync(mgrCtx)
			}).WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).Should(BeTrue())

			By("Creating the Node to trigger the lifecycle")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						labelHypervisor: "kvm",
						"cobaltcore.cloud.sap/node-hypervisor-lifecycle": lifecycleLabel,
						corev1.LabelTopologyZone:                         "test-az",
						corev1.LabelTopologyRegion:                       "test-region",
						corev1.LabelHostname:                             nodeName,
					},
					Annotations: map[string]string{
						"nova.openstack.cloud.sap/aggregates":    "prod-aggregate",
						"nova.openstack.cloud.sap/custom-traits": "CUSTOM_INTEG_TEST",
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
			})
		})

		AfterEach(func() {
			By("Stopping the manager")
			if mgrCancel != nil {
				mgrCancel()
			}

			// Cleanup the Hypervisor CRD (may have been created by the HypervisorController)
			hv := &kvmv1.Hypervisor{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), hv))).To(Succeed())
		})

		It("should complete the full onboarding lifecycle to Ready=True", func(ctx SpecContext) {
			By("1. Waiting for Hypervisor CRD to be created by HypervisorController")
			Eventually(getHypervisor(ctx, nodeName)).
				WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).
				Should(SatisfyAll(
					HaveField("Name", nodeName),
					HaveField("Spec.LifecycleEnabled", true),
				))

			By("2. Waiting for HypervisorID and ServiceID to be populated")
			Eventually(func(g Gomega) {
				hv, err := getHypervisor(ctx, nodeName)()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hv.Status.HypervisorID).NotTo(BeEmpty())
				g.Expect(hv.Status.ServiceID).NotTo(BeEmpty())
			}).WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).Should(Succeed())

			By("3. Waiting for final state: Ready=True, Onboarding=False/Succeeded")
			// Controllers race through phases quickly: Initial → Testing → Handover → Succeeded.
			// Intermediate phases may be too transient to observe, so we assert on the final state.
			Eventually(func(g Gomega) {
				hv, err := getHypervisor(ctx, nodeName)()
				g.Expect(err).NotTo(HaveOccurred())

				readyCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeReady)
				g.Expect(readyCond).NotTo(BeNil())
				g.Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(readyCond.Reason).To(Equal(kvmv1.ConditionReasonReadyReady))

				onboardingCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
				g.Expect(onboardingCond).NotTo(BeNil())
				g.Expect(onboardingCond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(onboardingCond.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))

				aggCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated)
				g.Expect(aggCond).NotTo(BeNil())
				g.Expect(aggCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(aggCond.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))

				traitsCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated)
				g.Expect(traitsCond).NotTo(BeNil())
				g.Expect(traitsCond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(traitsCond.Reason).To(Equal(kvmv1.ConditionReasonSucceeded))
			}).WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).Should(Succeed())

			By("4. Verifying final mock state")
			state.mu.Lock()
			defer state.mu.Unlock()

			// Nova service was enabled and forced_down cleared
			Expect(state.serviceEnabled).To(BeTrue(), "Nova service should be enabled")
			Expect(state.serviceForcedDown).To(BeFalse(), "Nova service should not be forced down")

			// Instance HA was registered
			Expect(state.haEnabled).To(BeTrue(), "Instance HA should be enabled")

			// All test servers cleaned up
			Expect(state.servers).To(BeEmpty(), "All test servers should have been cleaned up")

			// Custom trait was applied to placement
			Expect(state.traits).To(ContainElement("CUSTOM_INTEG_TEST"), "Custom trait should be applied")

			// Final aggregates: host in spec aggregates (test-az + prod-aggregate),
			// removed from test aggregate
			for _, agg := range state.aggregates {
				switch agg.Name {
				case "test-az":
					Expect(agg.Hosts).To(ContainElement(nodeName), "host should be in zone aggregate")
				case "prod-aggregate":
					Expect(agg.Hosts).To(ContainElement(nodeName), "host should be in prod aggregate")
				case testAggregateName:
					Expect(agg.Hosts).NotTo(ContainElement(nodeName), "host should be removed from test aggregate")
				}
			}
		})
	}

	Context("with SkipTests=true (skip-tests label)", func() {
		setupAndRun("integ-skip-tests-hv", "skip-tests")
	})

	Context("with SkipTests=false (full smoke test VM flow)", func() {
		setupAndRun("integ-full-test-hv", "")
	})
})
