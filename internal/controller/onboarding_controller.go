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
	"slices"
	"strings"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/openstack"
	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/aggregates"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/hypervisors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/services"
)

const (
	DEFAULT_WAIT_TIME        = 1 * time.Minute
	HYPERVISOR_ID_LABEL      = "nova.openstack.cloud.sap/hypervisor-id"
	SERVICE_ID_LABEL         = "nova.openstack.cloud.sap/service-id"
	ONBOARDING_STATE_LABEL   = "cobaltcore.cloud.sap/onboarding-state"
	ONBOARDING_INITIAL_VALUE = "initial"
	AVAILABILITY_ZONE_LABEL  = "topology.kubernetes.io/zone"
	TEST_AGGREGATE_NAME      = "tenant_filter_tests"
)

type OnboardingController struct {
	k8sclient.Client
	Scheme        *runtime.Scheme
	serviceClient *gophercloud.ServiceClient
	namespace     string
	issuerName    string
}

func getSecretAndCertName(name string) (string, string) {
	certName := fmt.Sprintf("libvirt-%s", name)
	secretName := fmt.Sprintf("tls-%s", certName)
	return secretName, certName
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

// ensureCertificate ensures that a certificate exists for the node and its ips
func (r *OnboardingController) ensureCertificate(ctx context.Context, node *corev1.Node, computeHost string) error {
	log := logger.FromContext(ctx)

	apiVersion := "cert-manager.io/v1"
	secretName, certName := getSecretAndCertName(node.Name)

	certificate := &cmapi.Certificate{
		TypeMeta: metav1.TypeMeta{
			Kind:       cmapi.CertificateKind,
			APIVersion: apiVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      certName,
			Namespace: r.namespace,
		},
	}

	update, err := controllerutil.CreateOrUpdate(ctx, r.Client, certificate, func() error {
		addNodeOwnerReference(&certificate.ObjectMeta, node)

		ipAddressSet := make(map[string]bool)
		dnsNameSet := make(map[string]bool)

		dnsNameSet[computeHost] = true

		for _, addr := range node.Status.Addresses {
			if addr.Address == "" {
				continue
			}

			switch addr.Type {
			case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
				dnsNameSet[addr.Address] = true
			case corev1.NodeInternalIP, corev1.NodeExternalIP:
				ipAddressSet[addr.Address] = true
			}
		}

		ipAddresses := make([]string, 0, len(ipAddressSet))
		for k := range ipAddressSet {
			ipAddresses = append(ipAddresses, k)
		}

		slices.Sort(ipAddresses)

		dnsNames := make([]string, 0, len(dnsNameSet))
		for k := range dnsNameSet {
			dnsNames = append(dnsNames, k)
		}

		slices.Sort(dnsNames)

		certificate.Spec = cmapi.CertificateSpec{
			SecretName: secretName,
			PrivateKey: &cmapi.CertificatePrivateKey{
				Algorithm: cmapi.RSAKeyAlgorithm,
				Encoding:  cmapi.PKCS1,
				Size:      4096,
			},
			// Values for testing, increase for production to something sensible
			Duration:    &metav1.Duration{Duration: 8 * time.Hour},
			RenewBefore: &metav1.Duration{Duration: 2 * time.Hour},
			IsCA:        false,
			Usages: []cmapi.KeyUsage{
				cmapi.UsageServerAuth,
				cmapi.UsageClientAuth,
				cmapi.UsageDigitalSignature,
				cmapi.UsageKeyEncipherment,
			},
			Subject: &cmapi.X509Subject{
				Organizations: []string{"nova"},
			},
			CommonName:  computeHost,
			DNSNames:    dnsNames,
			IPAddresses: ipAddresses,
			IssuerRef: cmmeta.ObjectReference{
				Name:  r.issuerName,
				Kind:  cmapi.IssuerKind,
				Group: "cert-manager.io",
			},
		}
		return nil
	})

	if err != nil {
		return err
	}

	if update != controllerutil.OperationResultNone {
		log.Info(fmt.Sprintf("Certificate %s %s", certName, update))
	}

	return nil
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="cert-manager.io",resources=certificates,verbs=get;list;watch;patch;update

func (r *OnboardingController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// OnboardingReconciler not found errors, could be deleted
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	found := hasAnyLabel(node.Labels, HYPERVISOR_LABEL)

	if !found {
		return ctrl.Result{}, nil
	}

	host, err := normalizeName(node)
	if err != nil {
		return ctrl.Result{}, nil // That is expected, the label will be set eventually
	}

	if err := r.ensureCertificate(ctx, node, host); err != nil {
		return ctrl.Result{}, fmt.Errorf("could create certificate %w", err)
	}

	result, err := r.ensureOpenstackLabels(ctx, node)
	if !result.IsZero() || err != nil {
		return result, k8sclient.IgnoreNotFound(err)
	}

	err = r.initialOnboarding(ctx, node, host)
	if err != nil {
		return result, k8sclient.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

func aggregatesByName(ctx context.Context, serviceClient *gophercloud.ServiceClient) (map[string]*aggregates.Aggregate, error) {
	pages, err := aggregates.List(serviceClient).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list aggregates due to %w", err)
	}

	aggs, err := aggregates.ExtractAggregates(pages)
	if err != nil {
		return nil, fmt.Errorf("cannot list aggregates due to %w", err)
	}

	aggregateMap := make(map[string]*aggregates.Aggregate, len(aggs))
	for _, aggregate := range aggs {
		aggregateMap[aggregate.Name] = &aggregate
	}
	return aggregateMap, nil
}

func (r *OnboardingController) initialOnboarding(ctx context.Context, node *corev1.Node, host string) error {
	_, found := node.Labels[ONBOARDING_STATE_LABEL]
	if found {
		return nil
	}

	zone, found := node.Labels[AVAILABILITY_ZONE_LABEL]
	if !found || zone == "" {
		return fmt.Errorf("cannot find availability-zone label %v on node", AVAILABILITY_ZONE_LABEL)
	}

	aggs, err := aggregatesByName(ctx, r.serviceClient)

	if err != nil {
		return fmt.Errorf("cannot list aggregates %w", err)
	}

	err = addToAggregate(ctx, r.serviceClient, aggs, host, zone, zone)
	if err != nil {
		return fmt.Errorf("failed to agg to availability-zone aggregate %w", err)
	}

	err = addToAggregate(ctx, r.serviceClient, aggs, host, TEST_AGGREGATE_NAME, "")
	if err != nil {
		return fmt.Errorf("failed to agg to test aggregate %w", err)
	}

	serviceId, found := node.Labels[SERVICE_ID_LABEL]
	if !found || serviceId == "" {
		return fmt.Errorf("empty service-id for label %v on node", SERVICE_ID_LABEL)
	}
	result := services.Update(ctx, r.serviceClient, serviceId, services.UpdateOpts{Status: services.ServiceEnabled})
	if result.Err != nil {
		return result.Err
	}

	_, err = setNodeLabels(ctx, r, node, map[string]string{
		ONBOARDING_STATE_LABEL: ONBOARDING_INITIAL_VALUE,
	})

	return err
}

func addToAggregate(ctx context.Context, serviceClient *gophercloud.ServiceClient, aggs map[string]*aggregates.Aggregate, host, name, zone string) (err error) {
	aggregate, found := aggs[name]
	if !found {
		aggregate, err = aggregates.Create(ctx, serviceClient,
			aggregates.CreateOpts{
				Name:             name,
				AvailabilityZone: zone,
			}).Extract()
		if err != nil {
			return fmt.Errorf("failed to create aggregate %v due to %w", name, err)
		}
	}

	found = false
	for _, aggHost := range aggregate.Hosts {
		if aggHost == host {
			found = true
		}
	}

	if !found {
		err := aggregates.AddHost(ctx, serviceClient, aggregate.ID, aggregates.AddHostOpts{Host: host}).Err
		if err != nil {
			return fmt.Errorf("failed to add host %v to aggregate %v due to %w", host, name, err)
		}
	}
	return nil
}

func (r *OnboardingController) ensureOpenstackLabels(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	_, hypervisorIdSet := node.Labels[HYPERVISOR_ID_LABEL]
	_, serviceIdSet := node.Labels[SERVICE_ID_LABEL]

	// We bail here out, because the openstack api is not the best to poll
	if hypervisorIdSet && serviceIdSet {
		return ctrl.Result{}, nil
	}

	hypervisorAddress := getHypervisorAddress(node)
	if hypervisorAddress == "" {
		return ctrl.Result{RequeueAfter: DEFAULT_WAIT_TIME}, nil
	}

	shortHypervisorAddress := strings.SplitN(hypervisorAddress, ".", 1)[0]

	hypervisorQuery := hypervisors.ListOpts{HypervisorHostnamePattern: &shortHypervisorAddress}
	hypervisorPages, err := hypervisors.List(r.serviceClient, hypervisorQuery).AllPages(ctx)

	if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return ctrl.Result{RequeueAfter: DEFAULT_WAIT_TIME}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	hs, err := hypervisors.ExtractHypervisors(hypervisorPages)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(hs) < 1 {
		return ctrl.Result{RequeueAfter: DEFAULT_WAIT_TIME}, nil
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
		return ctrl.Result{}, fmt.Errorf("could not find exact match for %v", shortHypervisorAddress)
	}

	changed, err := setNodeLabels(ctx, r, node, map[string]string{
		HYPERVISOR_ID_LABEL: myHypervisor.ID,
		SERVICE_ID_LABEL:    myHypervisor.Service.ID,
	})

	if err != nil || changed {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OnboardingController) SetupWithManager(mgr ctrl.Manager, namespace, issuerName string) error {
	_ = logger.FromContext(context.Background())

	r.namespace = namespace
	r.issuerName = issuerName

	var err error
	if r.serviceClient, err = openstack.GetServiceClient(context.Background(), "compute"); err != nil {
		return err
	}
	r.serviceClient.Microversion = "2.88"

	return ctrl.NewControllerManagedBy(mgr).
		Named("onboarding").
		For(&corev1.Node{}).
		Owns(&kvmv1.Eviction{}). // trigger the r.Reconcile whenever an Own-ed eviction is created/updated/deleted
		Complete(r)
}
