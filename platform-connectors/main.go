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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/json"
)

const (
	True = "true"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLogger("platform-connectors", version)
	slog.Info("Starting platform-connectors", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Platform connectors exited with error", "error", err)
		os.Exit(1)
	}
}

func loadConfig(configFilePath string) (map[string]interface{}, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform-connector-configmap with err %w", err)
	}

	result := make(map[string]interface{})

	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal platform-connector-configmap with err %w", err)
	}

	return result, nil
}

func initializeK8sConnector(
	ctx context.Context,
	config map[string]interface{},
	stopCh chan struct{},
) (*ringbuffer.RingBuffer, error) {
	k8sRingBuffer := ringbuffer.NewRingBuffer("kubernetes", ctx)
	server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)

	qpsTemp, ok := config["K8sConnectorQps"].(float64)
	if !ok {
		return nil, fmt.Errorf("failed to convert K8sConnectorQps to float: %v", config["K8sConnectorQps"])
	}

	qps := float32(qpsTemp)

	burst, ok := config["K8sConnectorBurst"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert K8sConnectorBurst to int: %v", config["K8sConnectorBurst"])
	}

	k8sConnector, err := kubernetes.InitializeK8sConnector(ctx, k8sRingBuffer, qps, int(burst), stopCh)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize K8sConnector: %w", err)
	}

	go k8sConnector.FetchAndProcessHealthMetric(ctx)

	return k8sRingBuffer, nil
}

func initializeMongoDBConnector(
	ctx context.Context,
	mongoClientCertMountPath string,
) (*store.MongoDbStoreConnector, error) {
	ringBuffer := ringbuffer.NewRingBuffer("mongodbStore", ctx)
	server.InitializeAndAttachRingBufferForConnectors(ringBuffer)

	storeConnector, err := store.InitializeMongoDbStoreConnector(ctx, ringBuffer, mongoClientCertMountPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize MongoDB store connector: %w", err)
	}

	go storeConnector.FetchAndProcessHealthMetric(ctx)

	return storeConnector, nil
}

func startGRPCServer(socket string) (net.Listener, error) {
	err := os.Remove(socket)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing socket: %w", err)
	}

	lis, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", socket, err)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{})

	go func() {
		err = grpcServer.Serve(lis)
		if err != nil {
			slog.Error("Not able to accept incoming connections", "error", err)
			os.Exit(1)
		}
	}()

	return lis, nil
}

func startMetricsServer(metricsPort string) {
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		slog.Info("Starting metrics server", "port", metricsPort)
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+metricsPort, nil)
		if err != nil {
			slog.Error("Failed to start metrics server", "error", err)
			os.Exit(1)
		}
	}()

	slog.Info("Metrics server goroutine started")
}

//nolint:cyclop // Main run function complexity is acceptable
func run() error {
	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	mongoClientCertMountPath := flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	flag.Parse()

	if *socket == "" {
		return fmt.Errorf("socket is not present")
	}

	sigs := make(chan os.Signal, 1)
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	config, err := loadConfig(*configFilePath)
	if err != nil {
		return err
	}

	var k8sRingBuffer *ringbuffer.RingBuffer

	var storeConnector *store.MongoDbStoreConnector

	if config["enableK8sPlatformConnector"] == True {
		k8sRingBuffer, err = initializeK8sConnector(ctx, config, stopCh)
		if err != nil {
			return err
		}
	}

	if config["enableMongoDBStorePlatformConnector"] == True {
		storeConnector, err = initializeMongoDBConnector(ctx, *mongoClientCertMountPath)
		if err != nil {
			return fmt.Errorf("failed to initialize MongoDB store connector: %w", err)
		}
	}

	lis, err := startGRPCServer(*socket)
	if err != nil {
		return err
	}

	startMetricsServer(*metricsPort)

	slog.Info("Waiting for signal")
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	slog.Info("Received signal", "signal", sig)

	close(stopCh)

	if lis != nil {
		if k8sRingBuffer != nil {
			k8sRingBuffer.ShutDownHealthMetricQueue()
		}

		lis.Close()
		os.Remove(*socket)
	}

	if storeConnector != nil {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer disconnectCancel()

		if err := storeConnector.Disconnect(disconnectCtx); err != nil {
			slog.Error("Failed to disconnect MongoDB client", "error", err)
		}
	}

	cancel()

	return nil
}
