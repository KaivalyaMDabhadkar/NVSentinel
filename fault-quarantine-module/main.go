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
	"context"
	"flag"
	"os/signal"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/configmanager"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/initializer"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)

	// Create a context that gets cancelled on OS interrupt signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop() // Ensure the signal listener is cleaned up

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	var kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	var tomlConfigPath = flag.String("config-path", "/etc/config/config.toml",
		"path where the fault quarantine config file is present")

	var dryRun = flag.Bool("dry-run", false, "flag to run fault quarantine module in dry-run mode")

	var circuitBreakerPercentage = flag.Int("circuit-breaker-percentage",
		50, "percentage of nodes to cordon before tripping the circuit breaker")

	var circuitBreakerDuration = flag.Duration("circuit-breaker-duration",
		5*time.Minute, "duration of the circuit breaker window")

	var circuitBreakerEnabled = flag.Bool("circuit-breaker-enabled", true,
		"enable or disable fault quarantine circuit breaker")

	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "fault-quarantine-module",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting fault-quarantine-module", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

	if _, err := configmanager.GetEnvVar[string]("POD_NAMESPACE"); err != nil {
		klog.Fatalf("Failed to get POD_NAMESPACE: %v", err)
	}

	params := initializer.InitializationParams{
		MongoClientCertMountPath: *mongoClientCertMountPath,
		KubeconfigPath:           *kubeconfigPath,
		TomlConfigPath:           *tomlConfigPath,
		MetricsPort:              *metricsPort,
		DryRun:                   *dryRun,
		CircuitBreakerPercentage: *circuitBreakerPercentage,
		CircuitBreakerDuration:   *circuitBreakerDuration,
		CircuitBreakerEnabled:    *circuitBreakerEnabled,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		klog.Fatalf("Initialization failed: %v", err)
	}

	klog.Info("Starting node informer")

	if err := components.Informer.Run(ctx.Done()); err != nil {
		klog.Fatalf("Failed to start node informer: %v", err)
	}

	klog.Info("Node informer started and synced")

	klog.Info("Starting fault quarantine reconciler")
	components.Reconciler.Start(ctx)
}
