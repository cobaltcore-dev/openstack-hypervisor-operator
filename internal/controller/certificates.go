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
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func getSecretAndCertName(host string) (string, string) {
	certName := fmt.Sprintf("libvirt-%s", host)
	secretName := fmt.Sprintf("tls-%s", certName)
	return secretName, certName
}

func (n *NodeReconciler) ensureCertificate(ctx context.Context, client k8sclient.Client, host string, dnsNames, ips []string) error {
	secretName, certName := getSecretAndCertName(host)
	cert := &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{
		Namespace: n.namespace,
		Name:      certName,
	}}

	_, err := controllerutil.CreateOrUpdate(ctx, client, cert, func() error {
		if cert.Labels == nil {
			cert.Labels = map[string]string{MANAGED_BY: MANAGER_NAME}
		} else {
			cert.Labels[MANAGED_BY] = MANAGER_NAME
		}
		cert.Spec = cmapi.CertificateSpec{
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
			Usages:      []cmapi.KeyUsage{cmapi.UsageServerAuth, cmapi.UsageCertSign, cmapi.UsageDigitalSignature, cmapi.UsageKeyEncipherment},
			Subject: &cmapi.X509Subject{
				Organizations: []string{"nova"},
			},
			CommonName:  host,
			DNSNames:    dnsNames,
			IPAddresses: ips,
			IssuerRef:   n.issuerRef,
		}
		return nil
	})

	return err
}
