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

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kvmv1 "github.com/cobaltcore-dev/openstack-hypervisor-operator/api/v1"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/controller"
	"github.com/cobaltcore-dev/openstack-hypervisor-operator/internal/netbox"

	// +kubebuilder:scaffold:imports
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kvmv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
	utilruntime.Must(cmapi.AddToScheme(scheme))
}

type stringArray []string

// String is an implementation of the flag.Value interface
func (i *stringArray) String() string {
	return fmt.Sprintf("%v", *i)
}

// Set is an implementation of the flag.Value interface
func (i *stringArray) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var clusters stringArray
	var netboxGraphQLURL string
	var region string
	var clusterTypeIDs stringArray
	var namespace string
	var issuerGroup string
	var issuerKind string
	var issuerName string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.Var(&clusters, "cluster", "Name of the target cluster(s).")
	flag.StringVar(&netboxGraphQLURL, "netbox-graphql-url", "",
		"The url pointing to the graphql endpoint of a Netbox installation.")
	flag.StringVar(&region, "region", "",
		"The name of the region.")
	flag.Var(&clusterTypeIDs, "cluster-type-id", "Id of the cluster-type (in Netbox).")
	flag.StringVar(&namespace, "namespace", "",
		"The namespace to create evictions and certificates in.")
	flag.StringVar(&issuerGroup, "issuer-group", "cert-manager.io",
		"The group of the certificate issuer")
	flag.StringVar(&issuerKind, "issuer-kind", cmapi.IssuerKind,
		"The kind of the certificate issuer")
	flag.StringVar(&issuerName, "issuer-name", "",
		"The name of certificate issuer")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if netboxGraphQLURL == "" {
		err := errors.New("the flag -netbox-graphql-url is required")
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if region == "" {
		err := errors.New("the flag -region is required")
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if len(clusterTypeIDs) == 0 {
		err := errors.New("need at least one -cluster-type-id")
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if namespace == "" {
		err := errors.New("the flag -namespace is required")
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if issuerName == "" {
		err := errors.New("the flag -issuer-name is required")
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		// TODO(user): TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
		// not provided, self-signed certificates will be generated by default. This option is not recommended for
		// production environments as self-signed certificates do not offer the same level of trust and security
		// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
		// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
		// to provide certificates, ensuring the server communicates using trusted and secure certificates.
		TLSOpts: tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4c28796a.cloud.sap",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	allClusters := map[string]cluster.Cluster{}
	if len(clusters) == 0 {
		cluster, err := cluster.New(restConfig, func(o *cluster.Options) {
			o.Scheme = scheme
		})
		if err != nil {
			setupLog.Error(err, "failed to construct clusters")
			os.Exit(1)
		}
		if err := mgr.Add(cluster); err != nil {
			setupLog.Error(err, "failed to add cluster to manager")
			os.Exit(1)
		}
		// Ensure we can actually get a client
		cluster.GetClient()
		allClusters["self"] = cluster
	}

	for _, context := range clusters {
		clusterConfig, err := config.GetConfigWithContext(context)
		if err != nil {
			setupLog.Error(err, "failed to load context", "context", context)
			os.Exit(1)
		}
		cluster, err := cluster.New(clusterConfig, func(o *cluster.Options) {
			o.Scheme = scheme
		})
		if err != nil {
			setupLog.Error(err, "failed to construct clusters")
			os.Exit(1)
		}
		if err := mgr.Add(cluster); err != nil {
			setupLog.Error(err, "failed to add cluster to manager")
			os.Exit(1)
		}
		cluster.GetClient()
		allClusters[context] = cluster
	}

	if err = (&controller.EvictionReconciler{}).SetupWithManagerAndClusters(mgr, allClusters); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Eviction")
		os.Exit(1)
	}

	netboxClient := netbox.NewClient(netboxGraphQLURL, region, clusterTypeIDs)

	issuerRef := cmmeta.ObjectReference{Name: issuerName, Kind: issuerKind, Group: issuerGroup}
	if err = (&controller.NodeReconciler{}).Setup(mgr, allClusters, netboxClient, namespace, issuerRef); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
