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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	defaultWaitTime          = 1 * time.Minute
	testProjectName          = "test"
	testDomainName           = "cc3test"
	testImageName            = "cirros-d240801-kvm"
	testPrefixName           = "ohooc-"
	testVolumeType           = "kvm-pilot"
	OnboardingControllerName = "onboarding"
)

type OnboardingController struct {
	k8sclient.Client
	Scheme            *runtime.Scheme
	computeClient     *gophercloud.ServiceClient
	testComputeClient *gophercloud.ServiceClient
	testImageClient   *gophercloud.ServiceClient
	testNetworkClient *gophercloud.ServiceClient
}

// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kvm.cloud.sap,resources=hypervisors/status,verbs=get;list;watch;patch
func (r *OnboardingController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	hv := &kvmv1.Hypervisor{}
	if err := r.Get(ctx, req.NamespacedName, hv); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	computeHost := hv.Name

	// check if lifecycle management is enabled
	if !hv.Spec.LifecycleEnabled {
		return ctrl.Result{}, r.abortOnboarding(ctx, hv, computeHost)
	}

	// check if hv is terminating
	if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTerminating) {
		return ctrl.Result{}, r.abortOnboarding(ctx, hv, computeHost)
	}

	// We bail here out, because the openstack api is not the best to poll
	if hv.Status.HypervisorID == "" || hv.Status.ServiceID == "" {
		if err := r.ensureNovaProperties(ctx, hv); err != nil {
			if errors.Is(err, errRequeue) {
				return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// check condition reason
	status := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if status == nil {
		base := hv.DeepCopy()
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonOnboarding,
			Message: "Onboarding in progress",
		})

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue,
			Reason:  kvmv1.ConditionReasonInitial,
			Message: "Initial onboarding",
		})
		return ctrl.Result{}, r.patchStatus(ctx, hv, base)
	}

	switch status.Reason {
	case kvmv1.ConditionReasonInitial:
		return ctrl.Result{}, r.initialOnboarding(ctx, hv)
	case kvmv1.ConditionReasonTesting:
		if hv.Spec.SkipTests {
			return r.completeOnboarding(ctx, computeHost, hv)
		} else {
			return r.smokeTest(ctx, hv, computeHost)
		}
	case kvmv1.ConditionReasonRemovingTestAggregate:
		return r.completeOnboarding(ctx, computeHost, hv)
	default:
		// Nothing to be done
		return ctrl.Result{}, nil
	}
}

func (r *OnboardingController) abortOnboarding(ctx context.Context, hv *kvmv1.Hypervisor, computeHost string) error {
	status := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	// Never onboarded
	if status == nil {
		return nil
	}

	base := hv.DeepCopy()
	ready := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeReady)
	if ready != nil {
		// Only undo ones own readiness status reporting
		if ready.Reason == kvmv1.ConditionReasonOnboarding {
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeReady,
				Status:  metav1.ConditionFalse,
				Reason:  kvmv1.ConditionReasonOnboarding,
				Message: "Onboarding aborted",
			})
		}
	}

	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeOnboarding,
		Status:  metav1.ConditionFalse,
		Reason:  kvmv1.ConditionReasonAborted,
		Message: "Aborted due to LifecycleEnabled being false",
	})

	if equality.Semantic.DeepEqual(hv, base) {
		// Already aborted
		return nil
	}

	if err := r.deleteTestServers(ctx, computeHost); err != nil {
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue, // No cleanup, so we are still "onboarding"
			Reason:  kvmv1.ConditionReasonAborted,
			Message: err.Error(),
		})

		if equality.Semantic.DeepEqual(hv, base) {
			return err
		}

		return errors.Join(err, r.patchStatus(ctx, hv, base))
	}

	return r.patchStatus(ctx, hv, base)
}

func (r *OnboardingController) initialOnboarding(ctx context.Context, hv *kvmv1.Hypervisor) error {
	zone, found := hv.Labels[corev1.LabelTopologyZone]
	if !found || zone == "" {
		return fmt.Errorf("cannot find availability-zone label %v on node", corev1.LabelTopologyZone)
	}

	// Wait for aggregates controller to apply the desired state (zone and test aggregate)
	expectedAggregates := []string{zone, testAggregateName}
	if !slices.Equal(hv.Status.Aggregates, expectedAggregates) {
		// Aggregates not yet applied, requeue
		return errRequeue
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

	base := hv.DeepCopy()
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeOnboarding,
		Status:  metav1.ConditionTrue,
		Reason:  kvmv1.ConditionReasonTesting,
		Message: "Running onboarding tests",
	})
	return r.patchStatus(ctx, hv, base)
}

func (r *OnboardingController) smokeTest(ctx context.Context, hv *kvmv1.Hypervisor, host string) (ctrl.Result, error) {
	log := logger.FromContext(ctx)
	zone := hv.Labels[corev1.LabelTopologyZone]
	server, err := r.createOrGetTestServer(ctx, zone, host, hv.UID)
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

		base := hv.DeepCopy()
		// Set condition back to testing
		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue,
			Reason:  kvmv1.ConditionReasonTesting,
			Message: "Server ended up in error state: " + server.Fault.Message,
		})
		if err = r.patchStatus(ctx, hv, base); err != nil {
			return ctrl.Result{}, err
		}

		// now delete the server and requeue
		if err = servers.Delete(ctx, r.testComputeClient, id).ExtractErr(); err != nil {
			log.Error(err, "failed to delete test instance", "id", id)
		}

		return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
	case "ACTIVE":
		consoleOutput, err := servers.
			ShowConsoleOutput(ctx, r.testComputeClient, server.ID, servers.ShowConsoleOutputOpts{Length: 11}).
			Extract()
		if err != nil {
			base := hv.DeepCopy()
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  kvmv1.ConditionReasonTesting,
				Message: fmt.Sprintf("could not get console output %v", err),
			})
			if err := r.patchStatus(ctx, hv, base); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		if !strings.Contains(consoleOutput, server.Name) {
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		if err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr(); err != nil {
			base := hv.DeepCopy()
			meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
				Type:    kvmv1.ConditionTypeOnboarding,
				Status:  metav1.ConditionTrue,
				Reason:  kvmv1.ConditionReasonTesting,
				Message: fmt.Sprintf("failed to terminate instance %v", err),
			})
			if err := r.patchStatus(ctx, hv, base); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		return r.completeOnboarding(ctx, host, hv)
	default:
		return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
	}
}

func (r *OnboardingController) completeOnboarding(ctx context.Context, host string, hv *kvmv1.Hypervisor) (ctrl.Result, error) {
	log := logger.FromContext(ctx)

	// Check if we're in the RemovingTestAggregate phase
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition != nil && onboardingCondition.Reason == kvmv1.ConditionReasonRemovingTestAggregate {
		// We're waiting for aggregates controller to sync
		if !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated) {
			log.Info("waiting for aggregates to be updated", "condition", kvmv1.ConditionTypeAggregatesUpdated)
			return ctrl.Result{RequeueAfter: defaultWaitTime}, nil
		}

		// Aggregates have been synced, mark onboarding as complete
		log.Info("aggregates updated successfully", "aggregates", hv.Status.Aggregates)
		base := hv.DeepCopy()

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionFalse,
			Reason:  kvmv1.ConditionReasonSucceeded,
			Message: "Onboarding completed",
		})

		meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeReady,
			Status:  metav1.ConditionTrue,
			Reason:  kvmv1.ConditionReasonReadyReady,
			Message: "Hypervisor is ready",
		})

		return ctrl.Result{}, r.patchStatus(ctx, hv, base)
	}

	// First time in completeOnboarding - clean up and prepare for aggregate sync
	err := r.deleteTestServers(ctx, host)
	if err != nil {
		base := hv.DeepCopy()
		if meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
			Type:    kvmv1.ConditionTypeOnboarding,
			Status:  metav1.ConditionTrue, // No cleanup, so we are still "onboarding"
			Reason:  kvmv1.ConditionReasonAborted,
			Message: err.Error(),
		}) {
			return ctrl.Result{}, errors.Join(err, r.patchStatus(ctx, hv, base))
		}

		return ctrl.Result{}, err
	}

	// Enable HA service before marking onboarding complete
	err = enableInstanceHA(hv)
	log.Info("enabled instance-ha")
	if err != nil {
		return ctrl.Result{}, err
	}

	base := hv.DeepCopy()

	// Mark onboarding as removing test aggregate - signals aggregates controller to use Spec.Aggregates
	meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
		Type:    kvmv1.ConditionTypeOnboarding,
		Status:  metav1.ConditionTrue,
		Reason:  kvmv1.ConditionReasonRemovingTestAggregate,
		Message: "Removing test aggregate",
	})

	// Patch status to signal aggregates controller
	return ctrl.Result{}, r.patchStatus(ctx, hv, base)
}

func (r *OnboardingController) deleteTestServers(ctx context.Context, host string) error {
	log := logger.FromContext(ctx)
	serverPrefix := fmt.Sprintf("%v-%v", testPrefixName, host)

	serverPages, err := servers.ListSimple(r.testComputeClient, servers.ListOpts{
		Name: serverPrefix,
	}).AllPages(ctx)

	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return err
	}

	serverList, err := servers.ExtractServers(serverPages)
	if err != nil {
		return err
	}

	errs := make([]error, 0)
	for _, server := range serverList {
		log.Info("deleting server", "name", server.Name)
		err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr()
		if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *OnboardingController) ensureNovaProperties(ctx context.Context, hv *kvmv1.Hypervisor) error {
	hypervisorAddress := hv.Labels[corev1.LabelHostname]
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

	base := hv.DeepCopy()
	hv.Status.HypervisorID = myHypervisor.ID
	hv.Status.ServiceID = myHypervisor.Service.ID
	return r.patchStatus(ctx, hv, base)
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
			log.Info("deleting outdated server", "name", server.Name)
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

	flavorRef, err := r.findTestFlavor(ctx)
	if err != nil {
		return nil, err
	}

	imageRef, err := r.findTestImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not list networks due to %w", err)
	}

	networkRef, err := r.findTestNetwork(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not extract network due to %w", err)
	}

	log.Info("creating server", "name", serverName, "flavor", flavorRef)
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

func (r *OnboardingController) findTestNetwork(ctx context.Context) (string, error) {
	falseVal := false
	networkPages, err := networks.List(r.testNetworkClient, networks.ListOpts{Shared: &falseVal}).AllPages(ctx)
	if err != nil {
		return "", err
	}

	extractedNetworks, err := networks.ExtractNetworks(networkPages)
	if err != nil {
		return "", err
	}

	for _, network := range extractedNetworks {
		return network.ID, nil
	}

	return "", errors.New("couldn't find network")
}

func (r *OnboardingController) findTestImage(ctx context.Context) (string, error) {
	imagePages, err := images.List(r.testImageClient, images.ListOpts{Name: testImageName}).AllPages(ctx)
	if err != nil {
		return "", err
	}

	imagesList, err := images.ExtractImages(imagePages)
	if err != nil {
		return "", err
	}

	for _, image := range imagesList {
		if image.Name == testImageName {
			return image.ID, nil
		}
	}

	return "", errors.New("couldn't find image")
}

func (r *OnboardingController) findTestFlavor(ctx context.Context) (string, error) {
	flavorPages, err := flavors.ListDetail(r.testComputeClient, flavors.ListOpts{SortDir: "asc", SortKey: "memory_mb"}).AllPages(ctx)
	if err != nil {
		return "", err
	}

	extractedFlavors, err := flavors.ExtractFlavors(flavorPages)
	if err != nil {
		return "", err
	}

	for _, flavor := range extractedFlavors {
		_, found := flavor.ExtraSpecs["capabilities:hypervisor_type"]
		if !found {
			// Flavor does not restrict the hypervisor-type
			return flavor.ID, nil
		}
	}

	return "", errors.New("couldn't find flavor")
}

func (r *OnboardingController) patchStatus(ctx context.Context, hv, base *kvmv1.Hypervisor) error {
	return r.Status().Patch(ctx, hv, k8sclient.MergeFromWithOptions(base,
		k8sclient.MergeFromWithOptimisticLock{}), k8sclient.FieldOwner(OnboardingControllerName))
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
