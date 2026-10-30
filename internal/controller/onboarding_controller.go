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
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	apiv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/applyconfigurations/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/utils"
)

var errRequeue = errors.New("requeue requested")

const (
	defaultWaitTime          = 1 * time.Minute
	smokeTestTimeout         = 5 * time.Minute
	testProjectName          = "test"
	testDomainName           = "cc3test"
	testImageName            = "cirros-kvm"
	testPrefixName           = "ohooc-"
	testVolumeType           = "premium"
	OnboardingControllerName = "onboarding"
)

type OnboardingController struct {
	k8sclient.Client
	Scheme            *runtime.Scheme
	TestFlavorID      string
	Clock             clock.Clock
	computeClient     *gophercloud.ServiceClient
	testComputeClient *gophercloud.ServiceClient
	testImageClient   *gophercloud.ServiceClient
	testNetworkClient *gophercloud.ServiceClient
	requeueInterval   time.Duration
}

func (r *OnboardingController) getRequeueInterval() time.Duration {
	if r.requeueInterval > 0 {
		return r.requeueInterval
	}
	return defaultWaitTime
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

	// Build the status apply config upfront; sub-functions mutate it.
	// HypervisorID and ServiceID are always retained so they are never pruned.
	statusCfg := apiv1.HypervisorStatus().
		WithHypervisorID(hv.Status.HypervisorID).
		WithServiceID(hv.Status.ServiceID)
	// Seed the Onboarding condition this controller owns so it is not pruned.
	if c := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding); c != nil {
		statusCfg.WithConditions(utils.ConditionFromStatus(*c))
	}

	apply := func() error {
		return r.Status().Apply(ctx,
			apiv1.Hypervisor(hv.Name).WithStatus(statusCfg),
			k8sclient.ForceOwnership, k8sclient.FieldOwner(OnboardingControllerName))
	}

	// check if lifecycle management is enabled
	if !hv.Spec.LifecycleEnabled {
		return ctrl.Result{}, r.abortOnboarding(ctx, hv, computeHost, "Aborted due to LifecycleEnabled being false", statusCfg, apply)
	}

	// check if hv is terminating
	if hv.Spec.Maintenance == kvmv1.MaintenanceTermination {
		return ctrl.Result{}, r.abortOnboarding(ctx, hv, computeHost, "Aborted due to MaintenanceTermination", statusCfg, apply)
	}

	// check condition reason
	status := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)

	// The Nova identity is observed state, not registration data. A node
	// replacement or a re-registered compute service changes it, and a stored
	// identity that is never re-validated leaves the current compute service
	// disabled while onboarding loops on 'No valid host' (nova-issues#786).
	// Successful onboarding is terminal and is not re-validated.
	if status == nil || !isOnboardingSucceeded(status) {
		identity, err := r.currentNovaIdentity(ctx, hv)
		switch {
		case errors.Is(err, errRequeue),
			errors.Is(err, openstack.ErrNoHypervisor),
			errors.Is(err, openstack.ErrHypervisorDown):
			// Nothing has registered (yet) that we may adopt.
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		case errors.Is(err, openstack.ErrAmbiguousHypervisor):
			return r.recordIdentityAmbiguity(ctx, status, statusCfg, apply)
		case err != nil:
			return ctrl.Result{}, err
		}

		if identity.HypervisorID != hv.Status.HypervisorID || identity.ServiceID != hv.Status.ServiceID {
			return r.repairIdentity(ctx, hv, status, identity, statusCfg, apply)
		}

		if status == nil {
			// IDs are set and current but no condition yet — treat as the initial phase.
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeOnboarding).
					WithStatus(metav1.ConditionTrue).
					WithReason(kvmv1.ConditionReasonInitial).
					WithMessage("Initial onboarding"))
			return ctrl.Result{}, apply()
		}

		// Tests and handover require the current compute service to be
		// enabled; a disabled service (e.g. freshly registered under
		// compute/disable_new_services) only yields 'No valid host'.
		if (status.Reason == kvmv1.ConditionReasonTesting || status.Reason == kvmv1.ConditionReasonHandover) &&
			identity.ServiceStatus != string(services.ServiceEnabled) {
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeOnboarding).
					WithStatus(metav1.ConditionTrue).
					WithReason(kvmv1.ConditionReasonInitial).
					WithMessage(fmt.Sprintf("Nova compute service %s is disabled; re-enabling before continuing onboarding", identity.ServiceID)))
			return ctrl.Result{}, apply()
		}
	}
	switch status.Reason {
	case kvmv1.ConditionReasonInitial:
		err := r.initialOnboarding(ctx, hv, statusCfg, apply)
		if errors.Is(err, errRequeue) {
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}
		return ctrl.Result{}, err
	case kvmv1.ConditionReasonTesting:
		if hv.Spec.SkipTests {
			return r.completeOnboarding(ctx, computeHost, hv, statusCfg, apply)
		} else {
			return r.smokeTest(ctx, hv, computeHost, statusCfg, apply)
		}
	case kvmv1.ConditionReasonHandover:
		return r.completeOnboarding(ctx, computeHost, hv, statusCfg, apply)
	default:
		// Nothing to be done
		return ctrl.Result{}, nil
	}
}

func (r *OnboardingController) abortOnboarding(ctx context.Context, hv *kvmv1.Hypervisor, computeHost, message string, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) error {
	status := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	// Never onboarded
	if status == nil {
		return nil
	}

	// Successful onboarding is terminal. Maintenance and offboarding track the
	// host's subsequent lifecycle without erasing its successful onboarding.
	if status.Reason == kvmv1.ConditionReasonSucceeded {
		return nil
	}

	// Already aborted with the same message — nothing to do
	if status.Status == metav1.ConditionFalse &&
		status.Reason == kvmv1.ConditionReasonAborted &&
		status.Message == message {
		return nil
	}

	if err := r.deleteTestServers(ctx, computeHost); err != nil {
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeOnboarding).
				WithStatus(metav1.ConditionTrue). // No cleanup, so we are still "onboarding"
				WithReason(kvmv1.ConditionReasonAborted).
				WithMessage(err.Error()))
		return errors.Join(err, apply())
	}

	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeOnboarding).
			WithStatus(metav1.ConditionFalse).
			WithReason(kvmv1.ConditionReasonAborted).
			WithMessage(message))
	return apply()
}

func (r *OnboardingController) initialOnboarding(ctx context.Context, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) error {
	zone, found := hv.Labels[corev1.LabelTopologyZone]
	if !found || zone == "" {
		return fmt.Errorf("cannot find availability-zone label %v on node", corev1.LabelTopologyZone)
	}

	// Wait for aggregates controller to apply the desired state (zone and test aggregate)
	currentAggregateNames := make([]string, len(hv.Status.Aggregates))
	for i, agg := range hv.Status.Aggregates {
		currentAggregateNames[i] = agg.Name
	}
	expectedAggregates := []string{zone, testAggregateName}
	if !slicesEqualUnordered(currentAggregateNames, expectedAggregates) {
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
		if gophercloud.ResponseCodeIs(result.Err, http.StatusNotFound) {
			// The compute service vanished underneath us (deleted and
			// re-registered); the identity resolution on the next reconcile
			// repairs the stored IDs.
			logger.FromContext(ctx).Info("nova-compute service disappeared; re-resolving identity",
				"serviceID", hv.Status.ServiceID)
			return errRequeue
		}
		return result.Err
	}

	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeOnboarding).
			WithStatus(metav1.ConditionTrue).
			WithReason(kvmv1.ConditionReasonTesting).
			WithMessage("Running onboarding tests"))
	return apply()
}

func (r *OnboardingController) smokeTest(ctx context.Context, hv *kvmv1.Hypervisor, host string, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) (ctrl.Result, error) {
	log := logger.FromContext(ctx)
	zone := hv.Labels[corev1.LabelTopologyZone]
	server, err := r.createOrGetTestServer(ctx, zone, host, hv.UID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create or get test instance: %w", err)
	}

	setTestingCondition := func(message string) error {
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeOnboarding).
				WithStatus(metav1.ConditionTrue).
				WithReason(kvmv1.ConditionReasonTesting).
				WithMessage(message))
		return apply()
	}

	switch server.Status {
	case "ERROR":
		// servers.List doesn't provide the fault field, so fetch the server again
		id := server.ID
		server, err = servers.Get(ctx, r.testComputeClient, id).Extract()
		if err != nil {
			log.Error(err, "failed to get test instance, instance vanished", "id", id)
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}

		// Set condition back to testing
		if err = setTestingCondition("Server ended up in error state: " + server.Fault.Message); err != nil {
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
			if err2 := setTestingCondition(fmt.Sprintf("could not get console output %v", err)); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}

		if !strings.Contains(consoleOutput, server.Name) {
			if !server.LaunchedAt.IsZero() && r.Clock.Now().After(server.LaunchedAt.Add(smokeTestTimeout)) {
				if err2 := setTestingCondition(fmt.Sprintf("timeout waiting for console output since %v", server.LaunchedAt)); err2 != nil {
					return ctrl.Result{}, err2
				}
				if err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr(); err != nil {
					if !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
						return ctrl.Result{}, fmt.Errorf("failed to delete timed out test instance %v: %w", server.ID, err)
					}
				}
			}
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}

		if err = servers.Delete(ctx, r.testComputeClient, server.ID).ExtractErr(); err != nil {
			if err2 := setTestingCondition(fmt.Sprintf("failed to terminate instance %v", err)); err2 != nil {
				return ctrl.Result{}, err2
			}
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}

		return r.completeOnboarding(ctx, host, hv, statusCfg, apply)

	default:
		return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
	}
}

func (r *OnboardingController) completeOnboarding(ctx context.Context, host string, hv *kvmv1.Hypervisor, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) (ctrl.Result, error) {
	onboardingCondition := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
	if onboardingCondition != nil && onboardingCondition.Reason == kvmv1.ConditionReasonHandover {
		// We're waiting for aggregates and traits controllers to sync
		if !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeAggregatesUpdated) {
			return ctrl.Result{}, nil
		}
		if !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated) {
			return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
		}

		// Wait for HypervisorInstanceHa controller to enable HA
		if hv.Spec.HighAvailability && !meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled) {
			return ctrl.Result{}, nil
		}

		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeOnboarding).
				WithStatus(metav1.ConditionFalse).
				WithReason(kvmv1.ConditionReasonSucceeded).
				WithMessage("Onboarding completed"))
		return ctrl.Result{}, apply()
	}

	// First time in completeOnboarding - clean up and prepare for aggregate sync
	if err := r.deleteTestServers(ctx, host); err != nil {
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeOnboarding).
				WithStatus(metav1.ConditionTrue). // No cleanup, so we are still "onboarding"
				WithReason(kvmv1.ConditionReasonAborted).
				WithMessage(err.Error()))
		return ctrl.Result{}, errors.Join(err, apply())
	}

	// Mark onboarding as almost complete, triggers other controllers to do their part
	// Patch status to signal aggregates controller
	utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
		*k8sacmetav1.Condition().
			WithType(kvmv1.ConditionTypeOnboarding).
			WithStatus(metav1.ConditionTrue).
			WithReason(kvmv1.ConditionReasonHandover).
			WithMessage("Waiting for other controllers to take over"))
	return ctrl.Result{}, apply()
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

// isOnboardingSucceeded reports whether the Onboarding condition marks the
// host as successfully onboarded. That state is terminal: maintenance and
// offboarding track the host from there, and the Nova identity is not
// re-validated.
func isOnboardingSucceeded(status *metav1.Condition) bool {
	return status.Status == metav1.ConditionFalse && status.Reason == kvmv1.ConditionReasonSucceeded
}

// currentNovaIdentity resolves the Nova identity that is currently
// authoritative for the hypervisor's hostname.
func (r *OnboardingController) currentNovaIdentity(ctx context.Context, hv *kvmv1.Hypervisor) (openstack.Identity, error) {
	hypervisorAddress := hv.Labels[corev1.LabelHostname]
	if hypervisorAddress == "" {
		return openstack.Identity{}, errRequeue
	}

	shortHypervisorAddress, _, _ := strings.Cut(hypervisorAddress, ".")

	hypervisorQuery := hypervisors.ListOpts{HypervisorHostnamePattern: &shortHypervisorAddress}
	hypervisorPages, err := hypervisors.List(r.computeClient, hypervisorQuery).AllPages(ctx)
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return openstack.Identity{}, errRequeue
		}
		return openstack.Identity{}, err
	}

	hs, err := hypervisors.ExtractHypervisors(hypervisorPages)
	if err != nil {
		return openstack.Identity{}, err
	}

	var matches []hypervisors.Hypervisor
	for _, h := range hs {
		short, _, _ := strings.Cut(h.HypervisorHostname, ".")
		if short == shortHypervisorAddress {
			matches = append(matches, h)
		}
	}

	return openstack.SelectCurrentIdentity(matches, hv.Status.ServiceID)
}

// repairIdentity updates the stored Nova identity. Onboarding that is still
// in progress is restarted from the initial phase so the enable step and the
// smoke test run against the current compute service; an aborted onboarding
// only has its IDs refreshed.
func (r *OnboardingController) repairIdentity(ctx context.Context, hv *kvmv1.Hypervisor, status *metav1.Condition, identity openstack.Identity, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) (ctrl.Result, error) {
	logger.FromContext(ctx).Info("Repairing stale Nova identity",
		"storedHypervisorID", hv.Status.HypervisorID, "currentHypervisorID", identity.HypervisorID,
		"storedServiceID", hv.Status.ServiceID, "currentServiceID", identity.ServiceID)

	statusCfg.WithHypervisorID(identity.HypervisorID).WithServiceID(identity.ServiceID)

	if status == nil || status.Status == metav1.ConditionTrue {
		message := "Initial onboarding"
		if hv.Status.HypervisorID != "" || hv.Status.ServiceID != "" {
			message = fmt.Sprintf("Nova compute identity changed (service %s → %s, hypervisor %s → %s); restarting onboarding",
				hv.Status.ServiceID, identity.ServiceID, hv.Status.HypervisorID, identity.HypervisorID)
		}
		utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
			*k8sacmetav1.Condition().
				WithType(kvmv1.ConditionTypeOnboarding).
				WithStatus(metav1.ConditionTrue).
				WithReason(kvmv1.ConditionReasonInitial).
				WithMessage(message))
	}

	return ctrl.Result{}, apply()
}

// recordIdentityAmbiguity surfaces an unresolvable identity on the Onboarding
// condition instead of guessing which compute service to act on, and requeues.
func (r *OnboardingController) recordIdentityAmbiguity(ctx context.Context, status *metav1.Condition, statusCfg *apiv1.HypervisorStatusApplyConfiguration, apply func() error) (ctrl.Result, error) {
	message := fmt.Sprintf("Waiting for Nova to report an unambiguous compute identity: %v", openstack.ErrAmbiguousHypervisor)

	if status == nil || status.Status == metav1.ConditionTrue {
		logger.FromContext(ctx).Info("Waiting for an unambiguous Nova compute identity")
		if status == nil || status.Message != message {
			utils.SetApplyConfigurationStatusCondition(&statusCfg.Conditions,
				*k8sacmetav1.Condition().
					WithType(kvmv1.ConditionTypeOnboarding).
					WithStatus(metav1.ConditionTrue).
					WithReason(kvmv1.ConditionReasonInitial).
					WithMessage(message))
			if err := apply(); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{RequeueAfter: r.getRequeueInterval()}, nil
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
	var foundServer *servers.Server
	// Cleanup all other server with the same test prefix, except for the exact match
	// as the cleanup after onboarding may leak resources
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

	flavorRef := r.TestFlavorID

	imageRef, err := r.findTestImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not find test image: %w", err)
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

	return "", fmt.Errorf("couldn't find image with name %v", testImageName)
}

// registerWithManager registers the controller with the Manager without acquiring OpenStack clients.
// This is useful for testing where clients are injected directly.
func (r *OnboardingController) registerWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(OnboardingControllerName).
		For(&kvmv1.Hypervisor{}).
		Complete(r)
}

// SetupWithManager sets up the controller with the Manager.
func (r *OnboardingController) SetupWithManager(mgr ctrl.Manager) error {
	if r.TestFlavorID == "" {
		r.TestFlavorID = "1"
	}

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

	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}

	return r.registerWithManager(mgr)
}
