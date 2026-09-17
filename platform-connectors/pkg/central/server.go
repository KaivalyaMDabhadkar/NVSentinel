// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package central is the deployment platform connector: the platform
// connector binary (PC_MODE=deployment) serving the same PlatformConnector
// gRPC service as the node-local DaemonSet, but over TCP with TLS to the whole
// fleet, with a small fixed pool of datastore connections.
//
// It reuses the gRPC handler, the event pipeline, the node-binding
// interceptor and the connectors as they are. What differs from the
// DaemonSet role:
//   - callers authenticate with projected ServiceAccount tokens (TokenReview)
//     and every batch is pinned to the caller token's node claim;
//   - there is no queue: the handler hands the batch to the connectors
//     themselves instead of to their ring buffers. The datastore write
//     decides the reply; node conditions and the gRPC sink run alongside it,
//     best effort inside a bounded wait;
//   - every batch carries an idempotency key, so a resent batch is never
//     stored twice;
//   - node conditions are updated, and Kubernetes Events written, only when
//     the batch would change them;
//   - connections are closed after a set age or idle time, and shutdown waits
//     a bounded time for open connections;
//   - a replica is ready only while the idempotency index is verified, and
//     refuses writes otherwise.
package central

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	k8sconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/prom"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/overrides"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// appName is the deployment platform connector's application name: the
// Kubernetes objects, the TLS certificate and the audience all derive from it.
const appName = "platform-connector-deployment"

// indexVerifyInterval is how often an unready replica re-checks the
// idempotency index while waiting for the datastore setup to create it;
// indexRecheckInterval is how often a ready replica confirms the index is
// still there.
const (
	indexVerifyInterval  = 5 * time.Second
	indexRecheckInterval = 5 * time.Minute
	// indexVerifyTimeout bounds one index check, so a datastore call that
	// never returns cannot silence the loop for the rest of the replica's life.
	indexVerifyTimeout = 30 * time.Second
	// shutdownTimeout bounds the wait for open connections at shutdown, so one
	// unresponsive client cannot block it. The chart's termination grace period
	// is sized from it (this wait, then the datastore disconnect and the trace
	// exporter, 5 s each), so it is fixed here rather than configured.
	shutdownTimeout = 20 * time.Second
)

// readiness gates /readyz and the write path: ready only when the idempotency
// index has been verified (so no batch is acknowledged before the datastore
// can suppress its duplicates) and the replica is not shutting down. A failed
// write leaves readiness alone: refusing writes on every transient error would
// turn a small error rate into a total refusal on the replica, and replicas
// stay in the Service through datastore outages, as the design requires.
type readiness struct {
	indexVerified atomic.Bool
	shuttingDown  atomic.Bool
}

// writesAllowed reports whether a batch may be written: the index is verified.
func (r *readiness) writesAllowed() bool {
	return r.indexVerified.Load()
}

func (r *readiness) Ready(_ context.Context) error {
	if r.shuttingDown.Load() {
		return errors.New("shutting down")
	}

	if !r.indexVerified.Load() {
		return errors.New("idempotency index not verified yet")
	}

	return nil
}

// indexVerifier is the one store connector call the readiness loop needs.
type indexVerifier interface {
	VerifyIdempotencyIndex(ctx context.Context) error
}

// verifyIndexLoop keeps readiness tied to the idempotency index for as long
// as the replica runs: every verifyEvery until the index matches its expected
// definition (this is what orders the datastore setup before any client
// traffic), then every recheckEvery. A definitive answer, the index missing or
// defined differently, turns the replica unready and makes the readiness
// interceptor refuse writes, so a database restored without the index stops
// taking writes as soon as the next check notices (within recheckEvery); once
// the datastore setup has recreated it the loop turns the replica ready again.
// A datastore failure changes nothing: replicas stay ready through outages,
// as the design requires. Every check has its own deadline (attemptTimeout);
// a check that runs out of time counts as a datastore failure. The loop ends
// with ctx, and a check cut short by that end is not applied.
func verifyIndexLoop(
	ctx context.Context, connector indexVerifier, ready *readiness,
	verifyEvery, recheckEvery, attemptTimeout time.Duration,
) {
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := connector.VerifyIdempotencyIndex(attemptCtx)

		cancel()

		if ctx.Err() != nil {
			return
		}

		applyIndexCheck(ctx, ready, err, verifyEvery)

		interval := verifyEvery
		if ready.writesAllowed() {
			interval = recheckEvery
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// applyIndexCheck folds one check's result into readiness: a verified index
// makes the replica ready; a confirmed missing or mismatched index makes it
// unready; any other failure changes nothing.
func applyIndexCheck(ctx context.Context, ready *readiness, err error, retryIn time.Duration) {
	switch {
	case err == nil:
		if !ready.indexVerified.Swap(true) {
			slog.InfoContext(ctx, "Idempotency index verified, replica is ready")
		}
	case errors.Is(err, datastore.ErrIndexMissing), errors.Is(err, datastore.ErrIndexMismatch):
		if ready.indexVerified.Swap(false) {
			slog.ErrorContext(ctx, "Idempotency index lost; the replica is unready and refuses writes until "+
				"the datastore setup recreates it: the MongoDB setup Job on its next run, or on PostgreSQL the "+
				"start of a component that sets the tables up (fault-quarantine, fault-remediation, "+
				"health-events-analyzer or node-drainer)", "error", err)
		} else {
			slog.WarnContext(ctx, "Idempotency index not verified yet, retrying",
				"error", err, "retryIn", retryIn)
		}
	default:
		slog.WarnContext(ctx, "Idempotency index check failed, readiness unchanged", "error", err)
	}
}

// k8sClientRateLimit resolves the Kubernetes client rate limit each of a
// replica's two clients gets (the k8s connector's and the metadata
// transformer's): the fleet-sized override when set, else the shared
// config's per-node values.
func k8sClientRateLimit(cfg *config, rawCfg map[string]any) (float32, int, error) {
	qps, err := cfgFloat64(rawCfg, "K8sConnectorQps")
	if err != nil {
		return 0, 0, err
	}

	burst, err := cfgInt64(rawCfg, "K8sConnectorBurst")
	if err != nil {
		return 0, 0, err
	}

	if cfg.k8sClientQPS > 0 {
		qps = float64(cfg.k8sClientQPS)
	}

	if cfg.k8sClientBurst > 0 {
		burst = int64(cfg.k8sClientBurst)
	}

	return float32(qps), int(burst), nil
}

// newK8sConnector starts the k8s connector for the fleet: node conditions
// are updated and Kubernetes Events written only when a batch changes them.
func newK8sConnector(
	ctx context.Context, cfg *config, rawCfg map[string]any, qps float32, burst int,
) (*k8sconnector.K8sConnector, error) {
	maxLen, err := cfgInt64(rawCfg, "MaxNodeConditionMessageLength")
	if err != nil {
		return nil, err
	}

	compactLen, err := cfgInt64(rawCfg, "CompactedHealthEventMsgLen")
	if err != nil {
		return nil, err
	}

	connector, _, err := k8sconnector.InitializeK8sConnector(ctx, nil, qps, burst, nil,
		k8sconnector.K8sConnectorConfig{
			MaxNodeConditionMessageLength: maxLen,
			CompactedHealthEventMsgLen:    compactLen,
		}, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize k8s connector: %w", err)
	}

	slog.InfoContext(ctx, "k8s connector enabled: node conditions and Events are written inside requests, on change")

	return connector, nil
}

// newSink builds the optional ADR-033 gRPC sink connector from the shared
// config; the connector set calls it once per batch, best effort.
func newSink(ctx context.Context, rawCfg map[string]any) (*grpcsink.GRPCSinkConnector, error) {
	target, ok := rawCfg["GRPCSinkTarget"].(string)
	if !ok || target == "" {
		return nil, fmt.Errorf("grpcSinkTarget not configured or empty")
	}

	tokenPath, _ := rawCfg["GRPCSinkTokenPath"].(string)

	connector, err := grpcsink.InitializeGRPCSinkConnector(nil, target, 0, tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
	}

	slog.InfoContext(ctx, "gRPC sink connector enabled", "target", target)

	return connector, nil
}

// buildServerOptions assembles the gRPC server options: transport security,
// connection lifetimes, per-connection buffer sizes and the interceptors, run
// in the order given. grpc-go itself spreads MaxConnectionAge by plus or
// minus 10 percent per connection, so fleet connections do not expire in
// synchronized waves. The returned certificate watcher, when non-nil, must be
// started for rotation to take effect.
func buildServerOptions(cfg *config, interceptors ...grpc.UnaryServerInterceptor) (
	[]grpc.ServerOption, *certwatcher.CertWatcher, error,
) {
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(interceptors...),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:  cfg.maxConnAge,
			MaxConnectionIdle: cfg.maxConnIdle,
		}),
	}

	if cfg.grpcReadBufferBytes > 0 {
		opts = append(opts, grpc.ReadBufferSize(cfg.grpcReadBufferBytes))
	}

	if cfg.grpcWriteBufferBytes > 0 {
		opts = append(opts, grpc.WriteBufferSize(cfg.grpcWriteBufferBytes))
	}

	if cfg.tlsCertDir == "" {
		// loadConfigFromEnv only allows this with the explicitly named
		// insecure development mode.
		slog.Warn("Serving PLAINTEXT: TLS_INSECURE_DEVELOPMENT_MODE is set; never use this outside development")

		return opts, nil, nil
	}

	cw, err := newCertWatcher(cfg.tlsCertDir)
	if err != nil {
		return nil, nil, err
	}

	opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfigFor(cw))))

	return opts, cw, nil
}

// components is everything run wires together and shutdown tears down.
type components struct {
	cfg        *config
	ready      *readiness
	store      *store.DatabaseStoreConnector
	handler    *server.PlatformConnectorServer
	grpcServer *grpc.Server
	sink       *grpcsink.GRPCSinkConnector
	pipeline   *pipeline.Pipeline
	// stopIndexLoop ends the index verification loop, before the store it
	// checks is disconnected.
	stopIndexLoop context.CancelFunc
}

// initComponents builds the store connector, the other connectors, the
// pipeline and the request handler from the environment and the shared
// config.json. The connectors go into one set without queues: the store's
// result is the reply, and every other connector is best effort inside the
// condition update timeout, so a slow node update or sink never fails a
// batch that is safely stored.
func initComponents(ctx context.Context, cfg *config) (*components, error) {
	c := &components{cfg: cfg, ready: &readiness{}}

	storeConnector, err := store.InitializeDatabaseStoreConnector(ctx, nil, cfg.certMountPath, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize store connector: %w", err)
	}

	c.store = storeConnector

	rawCfg, err := loadJSONConfig(cfg.configPath)
	if err != nil {
		return nil, err
	}

	qps, burst, err := k8sClientRateLimit(cfg, rawCfg)
	if err != nil {
		return nil, err
	}

	set := connectors.Set{storeConnector}

	if cfgBool(rawCfg, "enableK8sPlatformConnector") {
		k8s, err := newK8sConnector(ctx, cfg, rawCfg, qps, burst)
		if err != nil {
			return nil, err
		}

		set = append(set, connectors.BestEffort("kubernetes", k8s, cfg.conditionUpdateTimeout))
	}

	if cfgBool(rawCfg, "enableGRPCSinkConnector") {
		sink, err := newSink(ctx, rawCfg)
		if err != nil {
			return nil, err
		}

		c.sink = sink
		set = append(set, connectors.BestEffort("grpcsink", sink, cfg.conditionUpdateTimeout))
	}

	if cfgBool(rawCfg, "enablePromPlatformConnector") {
		// No ring buffer: the set hands it one batch at a time, so
		// health_events_total counts every batch that passed validation and
		// the pipeline, as the node-local role counts what reaches its queue.
		// The write's outcome is not waited for, so a batch resent after a
		// failed write is counted again.
		set = append(set, prom.InitializePromConnector(nil))

		slog.InfoContext(ctx, "Prometheus connector enabled: every accepted batch is counted in health_events_total")
	}

	c.pipeline, err = pipeline.NewFromRawConfig(ctx, rawCfg, pipeline.Options{
		KubeClientQPS:         qps,
		KubeClientBurst:       burst,
		NodeMetadataCacheSize: cfg.nodeMetadataCacheSize,
		NodeMetadataCacheTTL:  cfg.nodeMetadataCacheTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize pipeline: %w", err)
	}

	c.handler = &server.PlatformConnectorServer{Pipeline: c.pipeline, Connector: set}

	return c, nil
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}

	// Same tracing setup as the DaemonSet: the monitor's span context arrives
	// in the request metadata and every span below joins it.
	if err := tracing.InitTracing(appName); err != nil {
		slog.WarnContext(ctx, "Failed to initialize tracing, continuing without it", "error", err)
	}

	slog.InfoContext(ctx, "Starting the deployment platform connector",
		"listenAddr", cfg.listenAddr, "audience", cfg.audience, "conditionUpdateTimeout", cfg.conditionUpdateTimeout)

	c, err := initComponents(ctx, cfg)
	if err != nil {
		return err
	}

	defer c.pipeline.Close()

	indexCtx, stopIndexLoop := context.WithCancel(ctx)
	c.stopIndexLoop = stopIndexLoop

	go verifyIndexLoop(indexCtx, c.store, c.ready, indexVerifyInterval, indexRecheckInterval, indexVerifyTimeout)

	callerValidator, err := newValidator(cfg)
	if err != nil {
		return fmt.Errorf("failed to build caller token validator: %w", err)
	}

	authInterceptor, err := newAuthInterceptor(cfg, callerValidator)
	if err != nil {
		return err
	}

	// Authenticate and scope the caller, then key the batch, then refuse it
	// while the index is unverified; the handler validates, runs the
	// pipeline and hands the batch to the connectors.
	serverOpts, certWatcher, err := buildServerOptions(cfg,
		authInterceptor, idempotencyInterceptor(), readinessInterceptor(c.ready))
	if err != nil {
		return err
	}

	c.grpcServer = grpc.NewServer(serverOpts...)
	pb.RegisterPlatformConnectorServer(c.grpcServer, c.handler)

	return serve(ctx, cancel, c, certWatcher)
}

// serve runs the gRPC server, the metrics server, the certificate watcher and
// the signal handler until one of them stops the group.
func serve(ctx context.Context, cancel context.CancelFunc, c *components, certWatcher *certwatcher.CertWatcher) error {
	cfg := c.cfg

	var lc net.ListenConfig

	lis, err := lc.Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cfg.listenAddr, err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	// The watcher's event and polling loops are what pick up certificate
	// rotations; without them the listener would serve the startup pair
	// forever.
	if certWatcher != nil {
		g.Go(func() error {
			if err := certWatcher.Start(gCtx); err != nil {
				return fmt.Errorf("certificate watcher failed: %w", err)
			}

			return nil
		})
	}

	g.Go(func() error {
		slog.InfoContext(gCtx, "Deployment platform connector gRPC listening",
			"addr", cfg.listenAddr, "tls", cfg.tlsCertDir != "")

		err := c.grpcServer.Serve(lis)
		if err != nil {
			slog.ErrorContext(gCtx, "gRPC Serve returned", "error", err)

			return err
		}

		// Serve returns nil once Stop or GracefulStop has run: the normal end
		// of a shutdown, not an error.
		slog.InfoContext(gCtx, "gRPC server stopped")

		return nil
	})

	metricsSrv := srv.NewServer(
		srv.WithPort(cfg.metricsPort),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
		srv.WithReadinessCheck(c.ready),
	)

	// This server also answers the readiness and liveness probes, so its
	// failure stops the replica instead of leaving it serving unobserved.
	g.Go(func() error {
		if err := metricsSrv.Serve(gCtx); err != nil {
			return fmt.Errorf("metrics and probe server failed: %w", err)
		}

		return nil
	})

	g.Go(func() error {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		select {
		case sig := <-sigs:
			slog.InfoContext(gCtx, "Received signal, shutting down", "signal", sig)
		case <-gCtx.Done():
			slog.InfoContext(gCtx, "errgroup context done, shutting down", "cause", context.Cause(gCtx))
		}

		shutdown(gCtx, c)
		cancel()

		return nil
	})

	return g.Wait()
}

// shutdown turns the replica unready, lets requests in flight finish for a
// bounded time, then disconnects. The server holds nothing between requests,
// so there is nothing to drain: a client whose request did not complete
// resends it, with the same idempotency key, to another replica.
func shutdown(ctx context.Context, c *components) {
	if c.stopIndexLoop != nil {
		c.stopIndexLoop()
	}

	c.ready.shuttingDown.Store(true)

	// GracefulStop waits for every open connection; one wedged client would
	// otherwise block shutdown forever, so it is bounded.
	done := make(chan struct{})

	go func() {
		c.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		slog.WarnContext(ctx, "GracefulStop timed out, forcing Stop", "timeout", shutdownTimeout)
		c.grpcServer.Stop()
	}

	if c.sink != nil {
		if err := c.sink.Close(); err != nil {
			slog.WarnContext(ctx, "Error closing gRPC sink connector", "error", err)
		}
	}

	disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer disconnectCancel()

	if err := c.store.Disconnect(disconnectCtx); err != nil {
		slog.WarnContext(ctx, "Error disconnecting store connector", "error", err)
	}

	tracingCtx, tracingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer tracingCancel()

	if err := tracing.ShutdownTracing(tracingCtx); err != nil {
		slog.WarnContext(ctx, "Error shutting down tracing", "error", err)
	}
}

// Main is the deployment platform connector entrypoint, reached through the
// platform connector binary with PC_MODE=deployment. version comes from the
// caller because the build stamps only main.version.
func Main(version string) {
	logger.SetDefaultStructuredLoggerWithTraceCorrelation(appName, version)
	// controller-runtime's certwatcher logs through logr; without a sink it
	// drops its lines ("Updated current TLS certificate") and prints a
	// "SetLogger(...) was never called" warning with a stack trace.
	ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	// Node condition updates and Events are cluster mutations; the k8s
	// connector's client audits them through the same logger the DaemonSet
	// uses, which is a no-op until initialized.
	if err := auditlogger.InitAuditLogger(appName); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	err := run()

	if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
		slog.Warn("Failed to close audit logger", "error", closeErr)
	}

	if err != nil {
		slog.Error("Deployment platform connector exited with error", "error", err)
		os.Exit(1)
	}
}
