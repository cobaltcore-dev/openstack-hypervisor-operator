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
	"slices"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmapplyv1 "github.com/cert-manager/cert-manager/pkg/client/applyconfigurations/certmanager/v1"
	cmapplymetav1 "github.com/cert-manager/cert-manager/pkg/client/applyconfigurations/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sacmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	logger "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	NodeCertificateControllerName = "certificate"
)

type NodeCertificateController struct {
	k8sclient.Client
	Scheme     *runtime.Scheme
	namespace  string
	issuerName string
}

func getSecretAndCertName(name string) (secretName, certName string) {
	certName = "libvirt-" + name
	secretName = "tls-" + certName
	return secretName, certName
}

// ensureCertificate ensures that a certificate exists for the node and its ips
func (r *NodeCertificateController) ensureCertificate(ctx context.Context, node *corev1.Node, computeHost string) error {
	log := logger.FromContext(ctx)

	secretName, certName := getSecretAndCertName(node.Name)

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

	ownerRef := k8sacmetav1.OwnerReference().
		WithAPIVersion(corev1.SchemeGroupVersion.String()).
		WithKind("Node").
		WithName(node.Name).
		WithUID(node.UID)

	certApplyCfg := cmapplyv1.Certificate(certName, r.namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(cmapplyv1.CertificateSpec().
			WithSecretName(secretName).
			WithPrivateKey(cmapplyv1.CertificatePrivateKey().
				WithAlgorithm(cmapi.RSAKeyAlgorithm).
				WithEncoding(cmapi.PKCS1).
				WithSize(4096)).
			// Matching the CA/Browser Forum's maximum duration for 2029
			WithDuration(metav1.Duration{Duration: 47 * 24 * time.Hour}).
			WithRenewBefore(metav1.Duration{Duration: 37 * 24 * time.Hour}).
			WithIsCA(false).
			WithUsages(
				cmapi.UsageServerAuth,
				cmapi.UsageClientAuth,
				cmapi.UsageCertSign, // Really?
				cmapi.UsageDigitalSignature,
				cmapi.UsageKeyEncipherment,
			).
			WithSubject(cmapplyv1.X509Subject().
				WithOrganizations("nova")).
			WithCommonName(computeHost).
			WithDNSNames(dnsNames...).
			WithIPAddresses(ipAddresses...).
			WithIssuerRef(cmapplymetav1.IssuerReference().
				WithName(r.issuerName).
				WithKind(cmapi.IssuerKind).
				WithGroup(cmapi.SchemeGroupVersion.Group)))

	if err := r.Apply(ctx, certApplyCfg,
		k8sclient.ForceOwnership, k8sclient.FieldOwner(NodeCertificateControllerName)); err != nil {
		return err
	}

	log.Info(fmt.Sprintf("Applied Certificate %s", certName))
	return nil
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodeCertificateController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.FromContext(ctx).WithName(req.Name)
	ctx = logger.IntoContext(ctx, log)

	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		// Node not found, nothing to be done
		return ctrl.Result{}, k8sclient.IgnoreNotFound(err)
	}

	if err := r.ensureCertificate(ctx, node, node.Name); err != nil {
		return ctrl.Result{}, fmt.Errorf("could not apply certificate: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeCertificateController) SetupWithManager(mgr ctrl.Manager, namespace, issuerName string) error {
	r.namespace = namespace
	r.issuerName = issuerName

	novaVirtLabeledPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      labelHypervisor,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create label selector predicate: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(NodeCertificateControllerName).
		For(&corev1.Node{}).
		WithEventFilter(novaVirtLabeledPredicate).
		Complete(r)
}
