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

package initializer

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/nvidia/nvsentinel/configmanager"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/mongodb"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/reconciler"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog/v2"
)

type InitializationParams struct {
	MongoClientCertMountPath string
	KubeconfigPath           string
	TomlConfigPath           string
	MetricsPort              string
	DryRun                   bool
	CircuitBreakerPercentage int
	CircuitBreakerDuration   time.Duration
	CircuitBreakerEnabled    bool
}

type Components struct {
	Reconciler     *reconciler.Reconciler
	EventWatcher   *mongodb.EventWatcher
	Informer       *informer.NodeInformer
	CircuitBreaker breaker.CircuitBreaker
}

type EnvConfig struct {
	Namespace                                    string
	MongoURI                                     string
	MongoDatabase                                string
	MongoCollection                              string
	TokenDatabase                                string
	TokenCollection                              string
	TotalTimeoutSeconds                          int
	IntervalSeconds                              int
	TotalCACertTimeoutSeconds                    int
	IntervalCACertSeconds                        int
	UnprocessedEventsMetricUpdateIntervalSeconds int
}

func startMetricsServer(port string) {
	klog.Infof("Starting metrics port on: %s", port)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+port, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()
}

func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	klog.Info("Starting fault quarantine module initialization")

	envConfig, err := loadEnvConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load environment configuration: %w", err)
	}

	startMetricsServer(params.MetricsPort)

	mongoConfig := createMongoConfig(envConfig, params.MongoClientCertMountPath)
	tokenConfig := createTokenConfig(envConfig)
	pipeline := createMongoPipeline()

	var tomlCfg config.TomlConfig
	if err := configmanager.LoadTOMLConfig(params.TomlConfigPath, &tomlCfg); err != nil {
		return nil, fmt.Errorf("error while loading the toml config: %w", err)
	}

	if params.DryRun {
		klog.Info("Running in dry-run mode")
	}

	k8sClient, err := informer.NewFaultQuarantineClient(params.KubeconfigPath, params.DryRun)
	if err != nil {
		return nil, fmt.Errorf("error while initializing kubernetes client: %w", err)
	}

	klog.Info("Successfully initialized kubernetes client")

	nodeInformer, err := initializeNodeInformer(k8sClient)
	if err != nil {
		return nil, fmt.Errorf("error while initializing node informer: %w", err)
	}

	klog.Info("Successfully initialized node informer")

	var circuitBreaker breaker.CircuitBreaker

	if params.CircuitBreakerEnabled {
		cb, err := initializeCircuitBreaker(
			ctx,
			k8sClient,
			envConfig.Namespace,
			params.CircuitBreakerPercentage,
			params.CircuitBreakerDuration,
		)
		if err != nil {
			return nil, fmt.Errorf("error while initializing circuit breaker: %w", err)
		}

		circuitBreaker = cb

		klog.Info("Successfully initialized circuit breaker")
	} else {
		klog.Info("Circuit breaker is disabled, skipping initialization")
	}

	reconcilerCfg := createReconcilerConfig(
		tomlCfg,
		params.DryRun,
		params.CircuitBreakerEnabled,
	)

	reconcilerInstance := reconciler.NewReconciler(
		reconcilerCfg,
		k8sClient,
		circuitBreaker,
		nodeInformer,
	)

	healthEventCollection, err := initializeMongoCollection(ctx, mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("error while initializing mongo collection: %w", err)
	}

	eventWatcher := mongodb.NewEventWatcher(
		mongoConfig,
		tokenConfig,
		pipeline,
		healthEventCollection,
		time.Duration(envConfig.UnprocessedEventsMetricUpdateIntervalSeconds)*time.Second,
		reconcilerInstance,
	)

	reconcilerInstance.SetEventWatcher(eventWatcher)

	klog.Info("Initialization completed successfully")

	return &Components{
		Reconciler:     reconcilerInstance,
		EventWatcher:   eventWatcher,
		Informer:       nodeInformer,
		CircuitBreaker: circuitBreaker,
	}, nil
}

func loadEnvConfig() (*EnvConfig, error) {
	envSpecs := []configmanager.EnvVarSpec{
		{Name: "POD_NAMESPACE"},
		{Name: "MONGODB_URI"},
		{Name: "MONGODB_DATABASE_NAME"},
		{Name: "MONGODB_COLLECTION_NAME"},
		{Name: "MONGODB_TOKEN_COLLECTION_NAME"},
	}

	envVars, envErrors := configmanager.ReadEnvVars(envSpecs)
	if len(envErrors) > 0 {
		for _, err := range envErrors {
			klog.Error(err)
		}

		return nil, fmt.Errorf("required environment variables are missing")
	}

	totalTimeoutSeconds, err := getPositiveIntEnvVar("MONGODB_PING_TIMEOUT_TOTAL_SECONDS", 300)
	if err != nil {
		return nil, err
	}

	intervalSeconds, err := getPositiveIntEnvVar("MONGODB_PING_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, err
	}

	totalCACertTimeoutSeconds, err := getPositiveIntEnvVar("CA_CERT_MOUNT_TIMEOUT_TOTAL_SECONDS", 360)
	if err != nil {
		return nil, err
	}

	intervalCACertSeconds, err := getPositiveIntEnvVar("CA_CERT_READ_INTERVAL_SECONDS", 5)
	if err != nil {
		return nil, err
	}

	unprocessedEventsMetricUpdateIntervalSeconds, err :=
		getPositiveIntEnvVar("UNPROCESSED_EVENTS_METRIC_UPDATE_INTERVAL_SECONDS", 25)
	if err != nil {
		return nil, err
	}

	return &EnvConfig{
		Namespace:                 envVars["POD_NAMESPACE"],
		MongoURI:                  envVars["MONGODB_URI"],
		MongoDatabase:             envVars["MONGODB_DATABASE_NAME"],
		MongoCollection:           envVars["MONGODB_COLLECTION_NAME"],
		TokenDatabase:             envVars["MONGODB_DATABASE_NAME"],
		TokenCollection:           envVars["MONGODB_TOKEN_COLLECTION_NAME"],
		TotalTimeoutSeconds:       totalTimeoutSeconds,
		IntervalSeconds:           intervalSeconds,
		TotalCACertTimeoutSeconds: totalCACertTimeoutSeconds,
		IntervalCACertSeconds:     intervalCACertSeconds,
		UnprocessedEventsMetricUpdateIntervalSeconds: unprocessedEventsMetricUpdateIntervalSeconds,
	}, nil
}

func getPositiveIntEnvVar(name string, defaultValue int) (int, error) {
	value, err := configmanager.GetEnvVar[int](name, defaultValue,
		func(v int) error {
			if v <= 0 {
				return fmt.Errorf("must be positive")
			}

			return nil
		})
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}

	return value, nil
}

func createMongoConfig(envConfig *EnvConfig, mongoClientCertMountPath string) storewatcher.MongoDBConfig {
	return storewatcher.MongoDBConfig{
		URI:        envConfig.MongoURI,
		Database:   envConfig.MongoDatabase,
		Collection: envConfig.MongoCollection,
		ClientTLSCertConfig: storewatcher.MongoDBClientTLSCertConfig{
			TlsCertPath: filepath.Join(mongoClientCertMountPath, "tls.crt"),
			TlsKeyPath:  filepath.Join(mongoClientCertMountPath, "tls.key"),
			CaCertPath:  filepath.Join(mongoClientCertMountPath, "ca.crt"),
		},
		TotalPingTimeoutSeconds:    envConfig.TotalTimeoutSeconds,
		TotalPingIntervalSeconds:   envConfig.IntervalSeconds,
		TotalCACertTimeoutSeconds:  envConfig.TotalCACertTimeoutSeconds,
		TotalCACertIntervalSeconds: envConfig.IntervalCACertSeconds,
	}
}

func createTokenConfig(envConfig *EnvConfig) storewatcher.TokenConfig {
	return storewatcher.TokenConfig{
		ClientName:      "fault-quarantine-module",
		TokenDatabase:   envConfig.TokenDatabase,
		TokenCollection: envConfig.TokenCollection,
	}
}

func createMongoPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{
			bson.E{Key: "$match", Value: bson.D{
				bson.E{Key: "operationType", Value: bson.D{
					bson.E{Key: "$in", Value: bson.A{"insert"}},
				}},
			}},
		},
	}
}

func createReconcilerConfig(
	tomlCfg config.TomlConfig,
	dryRun bool,
	circuitBreakerEnabled bool,
) reconciler.ReconcilerConfig {
	return reconciler.ReconcilerConfig{
		TomlConfig:            tomlCfg,
		DryRun:                dryRun,
		CircuitBreakerEnabled: circuitBreakerEnabled,
	}
}

func initializeMongoCollection(
	ctx context.Context,
	mongoConfig storewatcher.MongoDBConfig,
) (*mongo.Collection, error) {
	collection, err := storewatcher.GetCollectionClient(ctx, mongoConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get MongoDB collection: %w", err)
	}

	return collection, nil
}

func initializeNodeInformer(
	k8sClient *informer.FaultQuarantineClient,
) (*informer.NodeInformer, error) {
	nodeInformer, err := informer.NewNodeInformer(
		k8sClient.GetK8sClient(),
		30*time.Minute,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create node informer: %w", err)
	}

	k8sClient.SetNodeInformer(nodeInformer)

	return nodeInformer, nil
}

func initializeCircuitBreaker(
	ctx context.Context,
	k8sClient *informer.FaultQuarantineClient,
	namespace string,
	percentage int,
	duration time.Duration,
) (breaker.CircuitBreaker, error) {
	circuitBreakerName := "fault-quarantine-circuit-breaker"

	klog.Infof("Initializing circuit breaker with config map %s in namespace %s", circuitBreakerName, namespace)

	cb, err := breaker.NewSlidingWindowBreaker(ctx, breaker.Config{
		Window:             duration,
		TripPercentage:     float64(percentage),
		K8sClient:          k8sClient,
		ConfigMapName:      circuitBreakerName,
		ConfigMapNamespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize circuit breaker: %w", err)
	}

	return cb, nil
}
