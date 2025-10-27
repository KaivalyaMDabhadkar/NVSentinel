// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/controller"
	webhookv1alpha1 "github.com/nvidia/nvsentinel/janitor/pkg/webhook/v1alpha1"
)

var (
	scheme = runtime.NewScheme()
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme))
}

func main() {
	logger.SetDefaultStructuredLogger("janitor", version)
	slog.Info("Starting janitor", "version", version, "commit", commit, "date", date)

	// Bridge slog to logr for controller-runtime
	// This ensures that controllers using log.FromContext(ctx) get slog
	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)
		os.Exit(1)
	}
}

// nolint:cyclop,gocyclo,gocognit
func run() error {
	var metricsAddr string

	var probeAddr string

	var enableLeaderElection bool

	var configFile string

	var webhookPort int

	var webhookCertPath string

	var webhookCertName string

	var webhookCertKey string

	var enableHTTP2 bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&configFile, "config", "", "Path to configuration file")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port for the webhook server")
	flag.StringVar(
		&webhookCertPath,
		"webhook-cert-path",
		"",
		"The directory that contains the webhook certificate.",
	)
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the webhook server")

	flag.Parse()

	slog.Info("Parsed flags",
		"metrics-bind-address", metricsAddr,
		"health-probe-bind-address", probeAddr,
		"leader-elect", enableLeaderElection,
		"config", configFile,
		"webhook-port", webhookPort)

	// Load configuration from file
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		slog.Error("Unable to load configuration", "error", err)
		return err
	}

	slog.Info("Loaded configuration",
		"rebootNode.timeout", cfg.RebootNode.Timeout,
		"global.manualMode", cfg.Global.ManualMode)

	// Setup TLS options for webhook server
	var tlsOpts []func(*tls.Config)

	var webhookCertWatcher *certwatcher.CertWatcher

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	if !enableHTTP2 {
		disableHTTP2 := func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		}
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Setup controller manager options
	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "janitor.dgxc.nvidia.com",
	}

	// Configure webhook server
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		slog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath,
			"webhook-cert-name", webhookCertName,
			"webhook-cert-key", webhookCertKey)

		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize webhook certificate watcher", "error", err)
			return err
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	mgrOptions.WebhookServer = webhook.NewServer(webhook.Options{
		Port:    webhookPort,
		TLSOpts: webhookTLSOpts,
	})

	slog.Info("Webhook server configured", "port", webhookPort)

	// Setup controller manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		slog.Error("Unable to create manager", "error", err)
		return err
	}

	// Setup RebootNode controller
	if err = (&controller.RebootNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: &cfg.RebootNode,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "RebootNode", "error", err)
		return err
	}

	// Setup webhook
	if err = webhookv1alpha1.SetupRebootNodeWebhookWithManager(mgr, &cfg.RebootNode); err != nil {
		slog.Error("Unable to create webhook", "webhook", "RebootNode", "error", err)
		return err
	}

	slog.Info("RebootNode validation webhook registered")

	// Add certificate watcher to manager if configured
	if webhookCertWatcher != nil {
		slog.Info("Adding webhook certificate watcher to manager")

		if err := mgr.Add(webhookCertWatcher); err != nil {
			slog.Error("Unable to add webhook certificate watcher to manager", "error", err)
			return err
		}
	}

	// Setup health checks
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Unable to set up health check", "error", err)
		return err
	}

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Unable to set up ready check", "error", err)
		return err
	}

	slog.Info("Starting manager")

	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("Problem running manager", "error", err)
		return err
	}

	return nil
}
