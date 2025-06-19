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
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logger "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gophercloud/gophercloud/v2"
)

const ()

type NodeCertificateController struct {
	k8sclient.Client
	Scheme            *runtime.Scheme
	computeClient     *gophercloud.ServiceClient
	instanceHAClient  *gophercloud.ServiceClient
	namespace         string
	issuerName        string
	testComputeClient *gophercloud.ServiceClient
	testImageClient   *gophercloud.ServiceClient
	testNetworkClient *gophercloud.ServiceClient
}

func getSecretAndCertName(name string) (string, string) {
	certName := fmt.Sprintf("libvirt-%s", name)
	secretName := fmt.Sprintf("tls-%s", certName)
	return secretName, certName
}

// ensureCertificate ensures that a certificate exists for the node and its ips
func (r *NodeCertificateController) ensureCertificate(ctx context.Context, node *corev1.Node, computeHost string) error {
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
				cmapi.UsageCertSign, // Really?
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

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="cert-manager.io",resources=certificates,verbs=get;list;watch;patch;update

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

	found := hasAnyLabel(node.Labels, labelHypervisor)

	if !found {
		return ctrl.Result{}, nil
	}

	host, found := node.Labels[labelMetalName]
	if !found {
		return ctrl.Result{}, nil // That is expected, the label will be set eventually
	}

	if err := r.ensureCertificate(ctx, node, host); err != nil {
		return ctrl.Result{}, fmt.Errorf("could create certificate %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeCertificateController) SetupWithManager(mgr ctrl.Manager, namespace, issuerName string) error {
	r.namespace = namespace
	r.issuerName = issuerName

	return ctrl.NewControllerManagedBy(mgr).
		Named("certificate").
		For(&corev1.Node{}).
		Complete(r)
}
