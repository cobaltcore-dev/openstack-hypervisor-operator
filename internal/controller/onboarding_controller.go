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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
)

var errRequeue = errors.New("requeue requested")

const (
	defaultWaitTime           = 1 * time.Minute
	ConditionTypeOnboarding   = "Onboarding"
	ConditionReasonInitial    = "initial"
	ConditionReasonOnboarding = "onboarding"
	ConditionReasonTesting    = "testing"
	ConditionReasonCompleted  = "completed"
	ConditionReasonReady      = "ready"
	testAggregateName         = "tenant_filter_tests"
	testProjectName           = "test"
	testDomainName            = "cc3test"
	testFlavorName            = "c_k_c2_m2_v2"
	testImageName             = "cirros-d240801-kvm"
	testPrefixName            = "ohooc-"
	testVolumeType            = "kvm-pilot"
	OnboardingControllerName  = "onboarding"
)

type OnboardingController struct {
	k8sclient.Client
	Scheme            *runtime.Scheme
	computeClient     *gophercloud.ServiceClient
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

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;patch
func (r *OnboardingController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	// check if lifecycle management is enabled
	if !hv.Spec.LifecycleEnabled {
		return ctrl.Result{}, nil
	}

	// check if hv is terminating
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTerminating) {
		return ctrl.Result{}, nil
	}

	computeHost := hv.Name
	// We bail here out, because the openstack api is not the best to poll
	if hv.Status.HypervisorID == "" || hv.Status.ServiceID == "" {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			return r.ensureNovaProperties(ctx, hv)
		}); err != nil {
			if errors.Is(err, errRequeue) {
				return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// check condition reason
	status := meta.FindStatusCondition(hv.Status.Conditions, ConditionTypeOnboarding)
	if status == nil {
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  ConditionReasonOnboarding,
			Message: "Onboarding in progress",
		})

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue,
			Reason:  ConditionReasonInitial,
			Message: "Initial onboarding",
		})
		return ctrl.Result{}, r.Status().Update(ctx, hv)
	}

	// TODO: cleanup node retrieval
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: hv.Name}, node); err != nil {
		if k8sclient.IgnoreNotFound(err) == nil {
			// Node not found, could be deleted
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	switch status.Reason {
	case ConditionReasonInitial:
		return ctrl.Result{}, r.initialOnboarding(ctx, hv, computeHost)
	case ConditionReasonTesting:
		if hv.Spec.SkipTests {
			return r.completeOnboarding(ctx, computeHost, node, hv)
		} else {
			return r.smokeTest(ctx, node, hv, computeHost)
		}
	default:
		// Nothing to be done
		return ctrl.Result{}, nil
	}
}

func (r *OnboardingController) initialOnboarding(ctx context.Context, hv *kvmv1.Hypervisor, host string) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: hv.Name}, node); err != nil {
		if k8sclient.IgnoreNotFound(err) == nil {
			// Node not found, could be deleted
			return nil
		}
		return err
	}

	zone, found := node.Labels[corev1.LabelTopologyZone]
	if !found || zone == "" {
		return fmt.Errorf("cannot find availability-zone label %v on node", corev1.LabelTopologyZone)
	}

	aggs, err := aggregatesByName(ctx, r.computeClient)
	if err != nil {
		return fmt.Errorf("cannot list aggregates %w", err)
	}

	if err = addToAggregate(ctx, r.computeClient, aggs, host, zone, zone); err != nil {
		return fmt.Errorf("failed to agg to availability-zone aggregate %w", err)
	}

	if _, found := node.Labels[corev1.LabelTopologyZone]; found {
		err = addToAggregate(ctx, r.computeClient, aggs, host, testAggregateName, "")
		if err != nil {
			return fmt.Errorf("failed to agg to test aggregate %w", err)
		}
	}

	// The service may be forced down previously due to an HA event,
	// so we need to ensure it not only enabled, but also not forced to be down.
	falseVal := false
	opts := openstack.UpdateServiceOpts{
		Status:     services.ServiceEnabled,
		ForcedDown: &falseVal,
	}
	if result := services.Update(ctx, r.computeClient, hv.Status.ServiceID, opts); result.Err != nil {
		return result.Err
	}

	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeOnboarding,
		Status:  metav1.ConditionTrue,
		Reason:  ConditionReasonTesting,
		Message: "Running onboarding tests",
	})
	return r.Status().Update(ctx, hv)
}

func (r *OnboardingController) smokeTest(ctx context.Context, node *corev1.Node, hv *kvmv1.Hypervisor, host string) (ctrl.Result, error) {
	log := logger.FromContext(ctx)
	zone := node.Labels[corev1.LabelTopologyZone]
	server, err := r.createOrGetTestServer(ctx, zone, host, node.UID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or get test instance %w", err)
	}

	switch server.Status {
	case "ERROR":
		// servers.List doesn't provide the fault field, so fetch the server again
		id := server.ID
		server, err = servers.Get(ctx, r.testComputeClient, id).Extract()
		if err != nil {
			// should not happened
			log.Error(err, "failed to get test instance, instance vanished", "id", id)
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		// Set condition back to testing
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue,
			Reason:  ConditionReasonTesting,
			Message: "Server ended up in error state: " + server.Fault.Message,
		})
		if err = r.Status().Update(ctx, hv); err != nil {
			return ctrl.Result{}, err
		}

		// now delete the server and requeue
		if err = servers.Delete(ctx, r.testComputeClient, id).ExtractErr(); err != nil {
			log.Error(err, "failed to delete test instance", "id", id)
		}

		return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
	case "ACTIVE":
		consoleOutput, err := servers.ShowConsoleOutput(ctx, r.testComputeClient, server.ID, servers.ShowConsoleOutputOpts{Length: 11}).Extract()
		if err != nil {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  ConditionReasonTesting,
				Message: fmt.Sprintf("could not get console output %v", err),
			})
			if err := r.Status().Update(ctx, hv); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		if !strings.Contains(consoleOutput, server.Name) {
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		if err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr(); err != nil {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  ConditionReasonTesting,
				Message: fmt.Sprintf("failed to terminate instance %v", err),
			})
			if err := r.Status().Update(ctx, hv); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		return r.completeOnboarding(ctx, host, node, hv)
	default:
		return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
	}
}

func (r *OnboardingController) completeOnboarding(ctx context.Context, host string, node *corev1.Node, hv *kvmv1.Hypervisor) (ctrl.Result, error) {
	log := logger.FromContext(ctx)

	serverPrefix := fmt.Sprintf("%v-%v", testPrefixName, host)

	serverPages, err := servers.ListSimple(r.testComputeClient, servers.ListOpts{
		Name: serverPrefix,
	}).AllPages(ctx)

	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ctrl.Result{}, err
	}

	serverList, err := servers.ExtractServers(serverPages)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, server := range serverList {
		log.Info("deleting server", "name", server.Name)
		err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr()
		if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return ctrl.Result{}, err
		}
	}

	aggs, err := aggregatesByName(ctx, r.computeClient)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get aggregates %w", err)
	}

	err = removeFromAggregate(ctx, r.computeClient, aggs, host, testAggregateName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove from test aggregate %w", err)
	}
	log.Info("removed from test-aggregate", "name", testAggregateName)

	err = enableInstanceHA(node)
	log.Info("enabled instance-ha")
	if err != nil {
		return ctrl.Result{}, err
	}

	// Set hypervisor ready condition
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeReady,
		Status:  metav1.ConditionTrue,
		Reason:  ConditionReasonReady,
		Message: "Hypervisor is ready",
	})

	// set onboarding condition completed
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeOnboarding,
		Status:  metav1.ConditionFalse,
		Reason:  ConditionReasonCompleted,
		Message: "Onboarding completed",
	})
	return ctrl.Result{}, r.Status().Update(ctx, hv)
}

func (r *OnboardingController) ensureNovaProperties(ctx context.Context, hv *kvmv1.Hypervisor) error {
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: hv.Name}, node); err != nil {
		return k8sclient.IgnoreNotFound(err)
	}

	hypervisorAddress := getHypervisorAddress(node)
	if hypervisorAddress == "" {
		return errRequeue
	}

	shortHypervisorAddress := strings.SplitN(hypervisorAddress, ".", 1)[0]

	hypervisorQuery := hypervisors.ListOpts{HypervisorHostnamePattern: &shortHypervisorAddress}
	hypervisorPages, err := hypervisors.List(r.computeClient, hypervisorQuery).AllPages(ctx)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return errRequeue
		}
		return err
	}

	hs, err := hypervisors.ExtractHypervisors(hypervisorPages)
	if err != nil {
		return err
	}

	if len(hs) < 1 {
		return errRequeue
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
		return fmt.Errorf("could not find exact match for %v", shortHypervisorAddress)
	}

	hv.Status.HypervisorID = myHypervisor.ID
	hv.Status.ServiceID = myHypervisor.Service.ID
	return r.Status().Update(ctx, hv)
}

func (r *OnboardingController) createOrGetTestServer(ctx context.Context, zone, computeHost string, nodeUid types.UID) (*servers.Server, error) {
	serverPrefix := fmt.Sprintf("%v-%v", testPrefixName, computeHost)
	serverName := fmt.Sprintf("%v-%v", serverPrefix, nodeUid)

	serverPages, err := servers.List(r.testComputeClient, servers.ListOpts{
		Name: serverPrefix,
	}).AllPages(ctx)

	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return nil, err
	}

	serverList, err := servers.ExtractServers(serverPages)
	if err != nil {
		return nil, err
	}

	log := logger.FromContext(ctx)
	// Cleanup all other server with the same test prefix, except for the exact match
	// as the cleanup after onboarding may leak resources
	var foundServer *servers.Server
	for _, server := range serverList {
		// The query is a substring search, we are looking for a prefix
		if !strings.HasPrefix(server.Name, serverPrefix) {
			continue
		}
		if server.Name != serverName || foundServer != nil {
			log.Info("deleting server", "name", server.Name)
			err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr()
			if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
				log.Error(err, "failed deleting instance due to", "instance", server.ID)
			}
		} else {
			foundServer = &server
		}
	}

	if foundServer != nil {
		return foundServer, nil
	}

	flavorPages, err := flavors.ListDetail(r.testComputeClient, nil).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	extractedFlavors, err := flavors.ExtractFlavors(flavorPages)
	if err != nil {
		return nil, err
	}

	var flavorRef string
	for _, flavor := range extractedFlavors {
		if flavor.Name == testFlavorName {
			flavorRef = flavor.ID
			break
		}
	}

	if flavorRef == "" {
		return nil, errors.New("couldn't find flavor")
	}

	var imageRef string

	imagePages, err := images.List(r.testImageClient, images.ListOpts{Name: testImageName}).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	imagesList, err := images.ExtractImages(imagePages)
	if err != nil {
		return nil, err
	}

	for _, image := range imagesList {
		if image.Name == testImageName {
			imageRef = image.ID
			break
		}
	}

	if imageRef == "" {
		return nil, errors.New("couldn't find image")
	}

	falseVal := false
	networkPages, err := networks.List(r.testNetworkClient, networks.ListOpts{Shared: &falseVal}).AllPages(ctx)
	if err != nil {
		return nil, err
	}

	extractedNetworks, err := networks.ExtractNetworks(networkPages)
	if err != nil {
		return nil, err
	}

	var networkRef string
	for _, network := range extractedNetworks {
		networkRef = network.ID
		break
	}

	if networkRef == "" {
		return nil, errors.New("couldn't find network")
	}

	log.Info("creating server", "name", serverName)
	server, err := servers.Create(ctx, r.testComputeClient, servers.CreateOpts{
		Name:             serverName,
		AvailabilityZone: fmt.Sprintf("%v:%v", zone, computeHost),
		FlavorRef:        flavorRef,
		BlockDevice: []servers.BlockDevice{{
			UUID:                imageRef,
			BootIndex:           0,
			SourceType:          "image",
			VolumeSize:          64,
			DiskBus:             "virtio",
			DeleteOnTermination: true,
			DestinationType:     "volume",
			VolumeType:          testVolumeType,
		}},
		Networks: []servers.Network{{
			UUID: networkRef,
		}},
	}, nil).Extract()

	if err != nil {
		return nil, err
	}

	if server == nil {
		return nil, errors.New("server is nil")
	}
	// Apparently the response doesn't contain the value
	server.Name = serverName
	return server, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OnboardingController) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	var err error
	if r.computeClient, err = openstack.GetServiceClient(ctx, "compute", nil); err != nil {
		return err
	}
	r.computeClient.Microversion = "2.95"

	// TODO: Make this configurable
	testAuth := &clientconfig.AuthInfo{
		ProjectName:       testProjectName,
		ProjectDomainName: testDomainName,
	}

	if r.testComputeClient, err = openstack.GetServiceClient(ctx, "compute", testAuth); err != nil {
		return err
	}
	r.testComputeClient.Microversion = "2.95"

	if r.testImageClient, err = openstack.GetServiceClient(ctx, "image", testAuth); err != nil {
		return err
	}
	r.testImageClient.ResourceBase = fmt.Sprintf("%vv2/", r.testImageClient.Endpoint)

	if r.testNetworkClient, err = openstack.GetServiceClient(ctx, "network", testAuth); err != nil {
		return err
	}
	r.testNetworkClient.ResourceBase = fmt.Sprintf("%vv2.0/", r.testNetworkClient.Endpoint)

	return ctrl.NewControllerManagedBy(mgr).
		Named(OnboardingControllerName).
		For(&kvmv1.Hypervisor{}).
		Complete(r)
}
