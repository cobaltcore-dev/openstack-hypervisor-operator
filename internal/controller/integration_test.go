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
	"time"

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
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller/ready"
)

// ---------------------------------------------------------------------------
// Integration test: Full Onboarding Lifecycle
//
// These tests start the full controller suite against a fake OpenStack API
// and a real (envtest) Kubernetes API server, then assert on the observable
// end-to-end outcomes:
//
//   - A Hypervisor CRD appears when a lifecycle-labelled Node is created.
//   - Nova IDs are discovered and stored on the Hypervisor status.
//   - The Nova compute service is enabled and forced_down is cleared.
//   - The node's aggregates and traits are applied to OpenStack.
//   - Any smoke-test VMs are cleaned up before onboarding completes.
//   - The Hypervisor reaches Ready=True with Onboarding=False/Succeeded.
//
// The fake OpenStack server (mockState) captures all API calls so the
// assertions can verify exact OpenStack state without any live services.
// ---------------------------------------------------------------------------

var _ = Describe("Integration: Full Onboarding Lifecycle", Label("integration"), func() {
	const (
		// Give the controllers plenty of time to converge before failing.
		eventuallyTimeout = 2 * time.Minute
		pollingInterval   = 500 * time.Millisecond
	)

	// run sets up the full controller suite, creates a Node with the given
	// lifecycle label, and asserts the full onboarding lifecycle completes.
	run := func(nodeName, lifecycleLabel string) {
		var (
			fakeServer testhelper.FakeServer
			state      *mockState
			mgrCancel  context.CancelFunc
		)

		BeforeEach(func(ctx SpecContext) {
			By("Standing up the fake OpenStack API server")
			fakeServer = testhelper.SetupHTTP()
			DeferCleanup(fakeServer.Teardown)

			state = newMockState()
			state.registerHandlers(fakeServer.Mux, nodeName)

			By("Creating the controller manager")
			skipNameValidation := true
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:  scheme.Scheme,
				Metrics: metricsserver.Options{BindAddress: "0"},
				Controller: config.Controller{
					SkipNameValidation: &skipNameValidation,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// All controllers that have OpenStack dependencies receive a
			// pre-configured service client pointing at the fake server.
			// Controllers without OpenStack deps (HypervisorController,
			// GardenerNodeLifecycleController, ready.Controller) use their
			// normal SetupWithManager path.
			sc := thclient.ServiceClient(fakeServer)

			Expect((&HypervisorController{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}).SetupWithManager(mgr)).To(Succeed())

			Expect((&GardenerNodeLifecycleController{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}).SetupWithManager(mgr, "default")).To(Succeed())

			Expect((&ready.Controller{
				Client: mgr.GetClient(),
				Scheme: mgr.GetScheme(),
			}).SetupWithManager(mgr)).To(Succeed())

			Expect((&OnboardingController{
				Client:            mgr.GetClient(),
				Scheme:            mgr.GetScheme(),
				computeClient:     sc,
				testComputeClient: sc,
				testImageClient:   sc,
				testNetworkClient: sc,
				// Speed up polling so the test completes in seconds, not minutes.
				requeueInterval: 1 * time.Second,
			}).registerWithManager(mgr)).To(Succeed())

			Expect((&AggregatesController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: sc,
			}).registerWithManager(mgr)).To(Succeed())

			Expect((&TraitsController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				serviceClient: sc,
			}).registerWithManager(mgr)).To(Succeed())

			Expect((&HypervisorMaintenanceController{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: sc,
			}).registerWithManager(mgr)).To(Succeed())

			Expect((&HypervisorOffboardingReconciler{
				Client:          mgr.GetClient(),
				Scheme:          mgr.GetScheme(),
				computeClient:   sc,
				placementClient: sc,
			}).registerWithManager(mgr)).To(Succeed())

			Expect((&EvictionReconciler{
				Client:        mgr.GetClient(),
				Scheme:        mgr.GetScheme(),
				computeClient: sc,
			}).registerWithManager(mgr)).To(Succeed())

			By("Starting the manager")
			var mgrCtx context.Context
			mgrCtx, mgrCancel = context.WithCancel(context.Background())
			go func() {
				defer GinkgoRecover()
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			Eventually(mgr.GetCache().WaitForCacheSync).
				WithArguments(mgrCtx).
				WithTimeout(30 * time.Second).WithPolling(100 * time.Millisecond).
				Should(BeTrue())

			By("Starting a fake kvm-ha-service that sets HaEnabled=True at Handover")
			go func() {
				defer GinkgoRecover()
				// Poll until the Hypervisor reaches Handover, then set HaEnabled=True.
				// This simulates what the external kvm-ha-service does in production.
				ticker := time.NewTicker(500 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-mgrCtx.Done():
						return
					case <-ticker.C:
						hv := &kvmv1.Hypervisor{}
						if err := mgr.GetClient().Get(mgrCtx, types.NamespacedName{Name: nodeName}, hv); err != nil {
							continue
						}
						onboarding := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeOnboarding)
						if onboarding == nil || onboarding.Reason != kvmv1.ConditionReasonHandover {
							continue
						}
						if meta.IsStatusConditionTrue(hv.Status.Conditions, kvmv1.ConditionTypeHaEnabled) {
							return // already set
						}
						meta.SetStatusCondition(&hv.Status.Conditions, metav1.Condition{
							Type:    kvmv1.ConditionTypeHaEnabled,
							Status:  metav1.ConditionTrue,
							Reason:  kvmv1.ConditionReasonSucceeded,
							Message: "HA enabled (simulated by integration test)",
						})
						if err := mgr.GetClient().Status().Update(mgrCtx, hv); err != nil {
							continue // retry on conflict
						}
						return
					}
				}
			}()

			By("Creating the Node that triggers the onboarding lifecycle")
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Labels: map[string]string{
						labelHypervisor:            "kvm",
						labelLifecycleMode:         lifecycleLabel,
						corev1.LabelTopologyZone:   "test-az",
						corev1.LabelTopologyRegion: "test-region",
						corev1.LabelHostname:       nodeName,
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
			if mgrCancel != nil {
				mgrCancel()
			}
			hv := &kvmv1.Hypervisor{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), hv))).To(Succeed())
		})

		It("reaches Ready=True with all onboarding conditions succeeded", func(ctx SpecContext) {
			getHypervisor := func() (*kvmv1.Hypervisor, error) {
				hv := &kvmv1.Hypervisor{}
				return hv, k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, hv)
			}

			By("Waiting for the Hypervisor CRD to be created")
			Eventually(getHypervisor).
				WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).
				Should(HaveField("Spec.LifecycleEnabled", true))

			By("Waiting for Nova IDs to be populated")
			Eventually(func(g Gomega) {
				hv, err := getHypervisor()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(hv.Status.HypervisorID).NotTo(BeEmpty())
				g.Expect(hv.Status.ServiceID).NotTo(BeEmpty())
			}).WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).Should(Succeed())

			By("Waiting for Ready=True and all onboarding conditions succeeded")
			Eventually(func(g Gomega) {
				hv, err := getHypervisor()
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

				traitsCond := meta.FindStatusCondition(hv.Status.Conditions, kvmv1.ConditionTypeTraitsUpdated)
				g.Expect(traitsCond).NotTo(BeNil())
				g.Expect(traitsCond.Status).To(Equal(metav1.ConditionTrue))
			}).WithTimeout(eventuallyTimeout).WithPolling(pollingInterval).Should(Succeed())

			By("Verifying the Nova compute service is enabled and not forced down")
			state.mu.Lock()
			defer state.mu.Unlock()
			Expect(state.serviceEnabled).To(BeTrue(), "Nova service should be enabled after onboarding")
			Expect(state.serviceForcedDown).To(BeFalse(), "Nova service should not be forced down after onboarding")

			By("Verifying traits were applied to Placement")
			Expect(state.traits).To(ContainElement("CUSTOM_INTEG_TEST"),
				"custom trait from node annotation should be applied to Placement")

			By("Verifying aggregate membership matches spec aggregates")
			for _, agg := range state.aggregates {
				switch agg.Name {
				case "test-az":
					Expect(agg.Hosts).To(ContainElement(nodeName),
						"node should be in the zone aggregate")
				case "prod-aggregate":
					Expect(agg.Hosts).To(ContainElement(nodeName),
						"node should be in the spec aggregate")
				case testAggregateName:
					Expect(agg.Hosts).NotTo(ContainElement(nodeName),
						"node should have been removed from the test aggregate after onboarding")
				}
			}

			By("Verifying all smoke-test VMs were cleaned up")
			Expect(state.servers).To(BeEmpty(),
				"smoke-test VMs should be deleted before onboarding completes")
		})
	}

	Context("with SkipTests=true (no smoke-test VM)", func() {
		run("integ-skip-tests-hv", "skip-tests")
	})

	Context("with SkipTests=false (full smoke-test VM flow)", func() {
		run("integ-full-test-hv", "")
	})
})
