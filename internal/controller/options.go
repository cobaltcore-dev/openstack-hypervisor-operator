/*
SPDX-FileCopyrightText: Copyright 2025 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
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
	"errors"

	"github.com/spf13/pflag"
)

// CertificateOptions configures the node certificate controller.
type CertificateOptions struct {
	Namespace  string
	IssuerName string
}

// AddFlags registers the certificate options on the flag set.
func (o *CertificateOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Namespace, "certificate-namespace", "monsoon3", "The namespace for the certificates.")
	fs.StringVar(&o.IssuerName, "certificate-issuer-name", "nova-hypervisor-agents-ca-issuer",
		"Name of the certificate issuer.")
}

// Validate checks that the certificate controller has the required settings.
func (o CertificateOptions) Validate() error {
	if o.Namespace == "" {
		return errors.New("--certificate-namespace (CERTIFICATE_NAMESPACE) cannot be empty")
	}
	if o.IssuerName == "" {
		return errors.New("--certificate-issuer-name (CERTIFICATE_ISSUER_NAME) cannot be empty")
	}

	return nil
}
