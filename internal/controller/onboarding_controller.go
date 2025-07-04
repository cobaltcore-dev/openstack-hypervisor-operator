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
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
)

const (
	defaultWaitTime          = 1 * time.Minute
	labelHypervisorID        = "nova.openstack.cloud.sap/hypervisor-id"
	labelServiceID           = "nova.openstack.cloud.sap/service-id"
	labelOnboardingState     = "cobaltcore.cloud.sap/onboarding-state"
	labelSegmentName         = "kubernetes.metal.cloud.sap/bb"
	onboardingValueInitial   = "initial"
	onboardingValueTesting   = "testing"
	onboardingValueCompleted = "completed"
	testAggregateName        = "tenant_filter_tests"
	testProjectName          = "test"
	testDomainName           = "cc3test"
	testFlavorName           = "c_k_c2_m2_v2"
	testImageName            = "cirros-d240801-kvm"
	testPrefixName           = "ohooc-"
	testVolumeType           = "nfs"
)

type OnboardingController struct {
	k8sclient.Client
	Scheme            *runtime.Scheme
	computeClient     *gophercloud.ServiceClient
	namespace         string
	testComputeClient *gophercloud.ServiceClient
	testImageClient   *gophercloud.ServiceClient
	testNetworkClient *gophercloud.ServiceClient
}

func getHypervisorAddress(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Address == "" {
			continue
		}

		if addr.Type == corev1.NodeHostName {
			return addr.Address
		}
	}

	return ""
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
func (r *OnboardingController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	found := hasAnyLabel(node.Labels, labelHypervisor)

	if !found {
		return ctrl.Result{}, nil
	}

	computeHost := node.Name
	result, changed, err := r.ensureNovaLabels(ctx, node)
	if !result.IsZero() || changed || err != nil {
		return result, k8sclient.IgnoreNotFound(err)
	}

	switch node.Labels[labelOnboardingState] {
	case onboardingValueTesting:
		if node.Labels[labelLifecycleMode] == "skip-tests" {
			result, err = r.completeOnboarding(ctx, computeHost, node)
		} else {
			result, err = r.smokeTest(ctx, node, computeHost)
		}
		return result, k8sclient.IgnoreNotFound(err)
	case "":
		err = r.initialOnboarding(ctx, node, computeHost)
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	default:
		// No idea how we ended up here.
		return ctrl.Result{}, nil
	}
}

func (r *OnboardingController) initialOnboarding(ctx context.Context, node *corev1.Node, host string) error {
	zone, found := node.Labels[corev1.LabelTopologyZone]
	if !found || zone == "" {
		return fmt.Errorf("cannot find availability-zone label %v on node", corev1.LabelTopologyZone)
	}

	aggs, err := aggregatesByName(ctx, r.computeClient)

	if err != nil {
		return fmt.Errorf("cannot list aggregates %w", err)
	}

	err = addToAggregate(ctx, r.computeClient, aggs, host, zone, zone)
	if err != nil {
		return fmt.Errorf("failed to agg to availability-zone aggregate %w", err)
	}

	if _, found := node.Labels[corev1.LabelTopologyZone]; found {
		err = addToAggregate(ctx, r.computeClient, aggs, host, testAggregateName, "")
		if err != nil {
			return fmt.Errorf("failed to agg to test aggregate %w", err)
		}
	}

	serviceId, found := node.Labels[labelServiceID]
	if !found || serviceId == "" {
		return fmt.Errorf("empty service-id for label %v on node", labelServiceID)
	}
	result := services.Update(ctx, r.computeClient, serviceId, services.UpdateOpts{Status: services.ServiceEnabled})
	if result.Err != nil {
		return result.Err
	}

	_, err = setNodeLabels(ctx, r, node, map[string]string{
		labelOnboardingState: onboardingValueTesting, // Move to testing
	})

	return err
}

func (r *OnboardingController) smokeTest(ctx context.Context, node *corev1.Node, host string) (ctrl.Result, error) {
	zone := node.Labels[corev1.LabelTopologyZone]
	server, err := r.createOrGetTestServer(ctx, zone, host, node.UID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or get test instance %w", err)
	}

	switch server.Status {
	case "ERROR":
		// Drop the error of delete, we will retry anyway for the server error
		_ = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr()
		return ctrl.Result{}, fmt.Errorf("server ended up in error state")
	case "ACTIVE":
		consoleOutput, err := servers.ShowConsoleOutput(ctx, r.testComputeClient, server.ID, servers.ShowConsoleOutputOpts{Length: 11}).Extract()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("could not get console output %w", err)
		}

		if !strings.Contains(consoleOutput, server.Name) {
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to terminate instance %w", err)
		}

		return r.completeOnboarding(ctx, host, node)
	default:
		return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
	}
}

func (r *OnboardingController) completeOnboarding(ctx context.Context, host string, node *corev1.Node) (ctrl.Result, error) {
	aggs, err := aggregatesByName(ctx, r.computeClient)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get aggregates %w", err)
	}

	err = removeFromAggregate(ctx, r.computeClient, aggs, host, testAggregateName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove from test aggregate %w", err)
	}

	_, err = setNodeLabels(ctx, r, node, map[string]string{
		labelOnboardingState: onboardingValueCompleted,
	})
	return ctrl.Result{}, nil
}

func (r *OnboardingController) ensureNovaLabels(ctx context.Context, node *corev1.Node) (ctrl.Result, bool, error) {
	_, hypervisorIdSet := node.Labels[labelHypervisorID]
	_, serviceIdSet := node.Labels[labelServiceID]

	// We bail here out, because the openstack api is not the best to poll
	if hypervisorIdSet && serviceIdSet {
		return ctrl.Result{}, false, nil
	}

	hypervisorAddress := getHypervisorAddress(node)
	if hypervisorAddress == "" {
		return ctrl.Result{RequeueAfter: defaultWaitTime}, false, nil
	}

	shortHypervisorAddress := strings.SplitN(hypervisorAddress, ".", 1)[0]

	hypervisorQuery := hypervisors.ListOpts{HypervisorHostnamePattern: &shortHypervisorAddress}
	hypervisorPages, err := hypervisors.List(r.computeClient, hypervisorQuery).AllPages(ctx)

	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ctrl.Result{RequeueAfter: defaultWaitTime}, false, nil
	} else if err != nil {
		return ctrl.Result{}, false, err
	}

	hs, err := hypervisors.ExtractHypervisors(hypervisorPages)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	if len(hs) < 1 {
		return ctrl.Result{RequeueAfter: defaultWaitTime}, false, nil
	}

	var found = false
	var myHypervisor hypervisors.Hypervisor
	for _, h := range hs {
		short := strings.SplitN(h.HypervisorHostname, ".", 1)[0]
		if short == shortHypervisorAddress {
			myHypervisor = h
			found = true
			break
		}
	}

	if !found {
		return ctrl.Result{}, false, fmt.Errorf("could not find exact match for %v", shortHypervisorAddress)
	}

	changed, err := setNodeLabels(ctx, r, node, map[string]string{
		labelHypervisorID: myHypervisor.ID,
		labelServiceID:    myHypervisor.Service.ID,
	})

	if err != nil || changed {
		return ctrl.Result{}, changed, err
	}

	return ctrl.Result{}, false, nil
}

func (r *OnboardingController) createOrGetTestServer(ctx context.Context, zone, computeHost string, nodeUid types.UID) (*servers.Server, error) {
	serverName := fmt.Sprintf("%v-%v-%v", testPrefixName, computeHost, nodeUid)

	serverPages, err := servers.List(r.testComputeClient, servers.ListOpts{
		Name: serverName,
	}).AllPages(ctx)

	if err != nil {
		return nil, err
	}

	serverList, err := servers.ExtractServers(serverPages)
	if err != nil {
		return nil, err
	}

	if len(serverList) > 0 {
		return &serverList[0], nil
	}

	flavorPages, err := flavors.ListDetail(r.testComputeClient, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	flavors, err := flavors.ExtractFlavors(flavorPages)
	if err != nil {
		return nil, err
	}

	var flavorRef string
	for _, flavor := range flavors {
		if flavor.Name == testFlavorName {
			flavorRef = flavor.ID
			break
		}
	}

	if flavorRef == "" {
		return nil, fmt.Errorf("couldn't find flavor")
	}

	var imageRef string

	imagePages, err := images.List(r.testImageClient, images.ListOpts{Name: testImageName}).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	imagesList, err := images.ExtractImages(imagePages)
	for _, image := range imagesList {
		if image.Name == testImageName {
			imageRef = image.ID
			break
		}
	}

	if imageRef == "" {
		return nil, fmt.Errorf("couldn't find image")
	}

	falseVal := false
	networkPages, err := networks.List(r.testNetworkClient, networks.ListOpts{Shared: &falseVal}).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	networks, err := networks.ExtractNetworks(networkPages)
	if err != nil {
		return nil, err
	}

	var networkRef string
	for _, network := range networks {
		networkRef = network.ID
		break
	}

	if networkRef == "" {
		return nil, fmt.Errorf("couldn't find network")
	}

	log := logger.FromContext(ctx)
	log.Info("creating server", "name", serverName)
	server, err := servers.Create(ctx, r.testComputeClient, servers.CreateOpts{
		Name:             serverName,
		AvailabilityZone: fmt.Sprintf("%v:%v", zone, computeHost),
		FlavorRef:        flavorRef,
		BlockDevice: []servers.BlockDevice{
			servers.BlockDevice{
				UUID:                imageRef,
				BootIndex:           0,
				SourceType:          "image",
				VolumeSize:          64,
				DiskBus:             "virtio",
				DeleteOnTermination: true,
				DestinationType:     "volume",
				VolumeType:          testVolumeType,
			},
		},
		Networks: []servers.Network{
			servers.Network{
				UUID: networkRef,
			},
		},
	}, nil).Extract()

	if err != nil {
		return nil, err
	}

	if server == nil {
		return nil, fmt.Errorf("server is nil")
	}
	// Apparently the response doesn't contain the value
	server.Name = serverName
	return server, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OnboardingController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute"); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.95"

	testAuth := &clientconfig.AuthInfo{
		ProjectName:       testProjectName,
		ProjectDomainName: testDomainName,
	}

	if r.testComputeClient, err = openstack.GetServiceClientAuth(ctx, "compute", testAuth); err != nil {
		return err
	}
	r.testComputeClient.Microversion = "2.95"

	if r.testImageClient, err = openstack.GetServiceClientAuth(ctx, "image", testAuth); err != nil {
		return err
	}
	r.testImageClient.ResourceBase = fmt.Sprintf("%vv2/", r.testImageClient.Endpoint)

	if r.testNetworkClient, err = openstack.GetServiceClientAuth(ctx, "network", testAuth); err != nil {
		return err
	}
	r.testNetworkClient.ResourceBase = fmt.Sprintf("%vv2.0/", r.testNetworkClient.Endpoint)

	return ctrl.NewControllerManagedBy(mgr).
		Named("onboarding").
		For(&corev1.Node{}).
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
