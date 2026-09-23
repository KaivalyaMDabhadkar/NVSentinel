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

// Tests for the health event reporter: bearer-token call metadata attached to
// HealthEventOccurredV1 publishes, exercised against a real gRPC server on a
// Unix socket (socket mode) and on a TCP port (direct mode).
package health

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// idempotencyKeyPattern is the format the deployment platform connector
// accepts for the idempotency-key header.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// capturingConnector is a PlatformConnector implementation that records the
// "authorization" and "idempotency-key" metadata of every
// HealthEventOccurredV1 call it receives.
type capturingConnector struct {
	pb.UnimplementedPlatformConnectorServer

	mu              sync.Mutex
	authHeaders     [][]string
	idempotencyKeys [][]string
}

func (c *capturingConnector) HealthEventOccurredV1(
	ctx context.Context,
	_ *pb.HealthEvents,
) (*emptypb.Empty, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.authHeaders = append(c.authHeaders, md.Get("authorization"))
	c.idempotencyKeys = append(c.idempotencyKeys, md.Get("idempotency-key"))

	return &emptypb.Empty{}, nil
}

func (c *capturingConnector) calls(t *testing.T) [][]string {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	captured := make([][]string, len(c.authHeaders))
	copy(captured, c.authHeaders)

	return captured
}

func (c *capturingConnector) lastAuth(t *testing.T) []string {
	t.Helper()

	captured := c.calls(t)
	if len(captured) == 0 {
		t.Fatal("no HealthEventOccurredV1 calls were captured")
	}

	return captured[len(captured)-1]
}

func (c *capturingConnector) lastIdempotencyKeys(t *testing.T) []string {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.idempotencyKeys) == 0 {
		t.Fatal("no HealthEventOccurredV1 calls were captured")
	}

	return c.idempotencyKeys[len(c.idempotencyKeys)-1]
}

// clearPublishEnv blanks every HEALTH_PUBLISH_* variable so the reporter runs
// in socket mode whatever the ambient environment holds. Direct mode tests
// set their own values on top.
func clearPublishEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"HEALTH_PUBLISH_TARGET",
		"HEALTH_PUBLISH_TOKEN_PATH",
		"HEALTH_PUBLISH_TLS_CA_FILE",
		"HEALTH_PUBLISH_TLS_SERVER_NAME",
		"HEALTH_PUBLISH_INSECURE",
		"HEALTH_PUBLISH_RETRY_WINDOW",
	} {
		t.Setenv(key, "")
	}
}

// startTestConnector serves a capturingConnector on a Unix socket in a
// temporary directory and returns the socket path.
func startTestConnector(t *testing.T) (string, *capturingConnector) {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "pc.sock")

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", socketPath, err)
	}

	return socketPath, serveConnector(t, lis)
}

// startTestTCPConnector serves a capturingConnector on a loopback TCP port,
// standing in for the deployment platform connector Service in direct mode,
// and returns its host:port.
func startTestTCPConnector(t *testing.T) (string, *capturingConnector) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on loopback TCP: %v", err)
	}

	return lis.Addr().String(), serveConnector(t, lis)
}

func serveConnector(t *testing.T, lis net.Listener) *capturingConnector {
	t.Helper()

	connector := &capturingConnector{}
	serveOn(t, lis, connector)

	return connector
}

// serveOn serves impl on lis until the test ends or the returned server is
// stopped.
func serveOn(t *testing.T, lis net.Listener, impl pb.PlatformConnectorServer) *grpc.Server {
	t.Helper()

	server := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(server, impl)

	go func() {
		_ = server.Serve(lis)
	}()

	t.Cleanup(server.Stop)

	return server
}

// listenUnix opens a Unix listener at socketPath, failing the test on error.
func listenUnix(t *testing.T, socketPath string) net.Listener {
	t.Helper()

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", socketPath, err)
	}

	return lis
}

// blockingConnector is a PlatformConnector whose handler never answers until
// release is closed, standing in for a node-local connector that hangs.
type blockingConnector struct {
	pb.UnimplementedPlatformConnectorServer

	release chan struct{}
}

func (b *blockingConnector) HealthEventOccurredV1(context.Context, *pb.HealthEvents) (*emptypb.Empty, error) {
	<-b.release

	return &emptypb.Empty{}, nil
}

// shortenSocketWaits replaces the socket mode timing for one test and
// restores the defaults when it ends.
func shortenSocketWaits(t *testing.T, waitTimeout, pollInterval, sendTimeout time.Duration) {
	t.Helper()

	oldWait, oldPoll, oldSend := socketWaitTimeout, socketPollInterval, socketSendTimeout
	socketWaitTimeout, socketPollInterval, socketSendTimeout = waitTimeout, pollInterval, sendTimeout

	t.Cleanup(func() {
		socketWaitTimeout, socketPollInterval, socketSendTimeout = oldWait, oldPoll, oldSend
	})
}

// reporterResult carries a NewReporter outcome out of a goroutine.
type reporterResult struct {
	reporter *Reporter
	err      error
}

// newReporterAsync starts NewReporter in a goroutine and returns the channel
// that receives its outcome.
func newReporterAsync(ctx context.Context, socketPath string) <-chan reporterResult {
	done := make(chan reporterResult, 1)

	go func() {
		reporter, err := NewReporter(ctx, socketPath, "test-node", pb.ProcessingStrategy_STORE_ONLY, "")
		done <- reporterResult{reporter: reporter, err: err}
	}()

	return done
}

func writeToken(t *testing.T, contents string) string {
	t.Helper()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}

	return tokenPath
}

func newTestReporter(t *testing.T, socketPath, tokenPath string) *Reporter {
	t.Helper()

	reporter, err := NewReporter(
		context.Background(), socketPath, "test-node", pb.ProcessingStrategy_STORE_ONLY, tokenPath,
	)
	if err != nil {
		t.Fatalf("NewReporter failed: %v", err)
	}

	t.Cleanup(reporter.Close)

	return reporter
}

// sendTestEvent sends one event in socket mode: the HEALTH_PUBLISH_* env is
// cleared first so the reporter dials the Unix socket at socketPath.
func sendTestEvent(t *testing.T, socketPath, tokenPath string) error {
	t.Helper()

	clearPublishEnv(t)

	reporter := newTestReporter(t, socketPath, tokenPath)

	return reporter.SendEvent(context.Background(), true, false, "test event", "")
}

func TestSendEventAttachesBearerToken(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "projected-token")

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	auth := connector.lastAuth(t)
	if len(auth) != 1 || auth[0] != "Bearer projected-token" {
		t.Errorf("got authorization %v, want [Bearer projected-token]", auth)
	}
}

func TestSendEventRereadsTokenOnEveryCall(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "token-one")

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("first SendEvent failed: %v", err)
	}

	// The kubelet rewrites the projected token file, so every call must
	// read it fresh instead of caching the first contents.
	if err := os.WriteFile(tokenPath, []byte("token-two"), 0o600); err != nil {
		t.Fatalf("failed to rotate token file: %v", err)
	}

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("second SendEvent failed: %v", err)
	}

	captured := connector.calls(t)
	if len(captured) != 2 {
		t.Fatalf("got %d calls, want 2", len(captured))
	}

	// Length-checked before indexing: md.Get returns an empty slice when the
	// header is absent, and indexing it would panic instead of reporting the
	// assertion failure.
	if len(captured[0]) == 0 || len(captured[1]) == 0 {
		t.Fatalf("got authorization %v, want one value per call", captured)
	}

	if captured[0][0] != "Bearer token-one" || captured[1][0] != "Bearer token-two" {
		t.Errorf("got authorization %v, want [Bearer token-one] then [Bearer token-two]", captured)
	}
}

func TestSendEventRejectsPaddedTokenInsteadOfRepairingIt(t *testing.T) {
	// A configured credential is forwarded verbatim. A token file containing
	// whitespace is a broken mount, not something to silently repair: trimming
	// it here would mean this client, rather than whatever wrote the file,
	// decides what the credential is. grpc-go forwards the value unchanged and
	// the receiving HTTP/2 transport refuses the newline, so SendEvent fails and
	// the request never reaches the RPC handler.
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "  padded-token\n")

	err := sendTestEvent(t, socketPath, tokenPath)
	if err == nil {
		t.Fatalf("SendEvent succeeded with a padded token; want failure")
	}

	if captured := connector.calls(t); len(captured) != 0 {
		t.Errorf("a request reached the handler (%v); want none delivered", captured)
	}
}

func TestSendEventWithoutTokenPathSendsNoAuthorization(t *testing.T) {
	socketPath, connector := startTestConnector(t)

	if err := sendTestEvent(t, socketPath, ""); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	if auth := connector.lastAuth(t); len(auth) != 0 {
		t.Errorf("got authorization %v, want none", auth)
	}
}

func TestSendEventMissingTokenFileFailsWithoutSending(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")

	if err := sendTestEvent(t, socketPath, missingPath); err == nil {
		t.Fatal("expected SendEvent to fail when the token file is unreadable")
	}

	if captured := connector.calls(t); len(captured) != 0 {
		t.Errorf("got %d calls, want 0: a configured token must not be skipped silently", len(captured))
	}
}

func TestSendEventDirectModeAttachesTokenAndIdempotencyKey(t *testing.T) {
	// With HEALTH_PUBLISH_TARGET set the reporter ignores the socket and
	// publishes to the deployment platform connector, carrying the token from
	// HEALTH_PUBLISH_TOKEN_PATH and one idempotency key per batch.
	target, connector := startTestTCPConnector(t)
	tokenPath := writeToken(t, "direct-token")

	clearPublishEnv(t)
	t.Setenv("HEALTH_PUBLISH_TARGET", target)
	t.Setenv("HEALTH_PUBLISH_INSECURE", "true")
	t.Setenv("HEALTH_PUBLISH_TOKEN_PATH", tokenPath)

	// The socket path points nowhere: direct mode must not wait for it.
	missingSocket := filepath.Join(t.TempDir(), "absent.sock")
	reporter := newTestReporter(t, missingSocket, "")

	if err := reporter.SendEvent(context.Background(), true, false, "test event", ""); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	auth := connector.lastAuth(t)
	if len(auth) != 1 || auth[0] != "Bearer direct-token" {
		t.Errorf("got authorization %v, want [Bearer direct-token]", auth)
	}

	keys := connector.lastIdempotencyKeys(t)
	if len(keys) != 1 {
		t.Fatalf("got idempotency-key %v, want exactly one value", keys)
	}

	if !idempotencyKeyPattern.MatchString(keys[0]) {
		t.Errorf("idempotency-key %q does not match %s", keys[0], idempotencyKeyPattern)
	}
}

func TestNewReporterDirectModeWithoutTokenPathFails(t *testing.T) {
	// The deployment platform connector accepts no batch without a token, so
	// a direct mode environment without a token path is a configuration error
	// at startup, not a send failure later.
	clearPublishEnv(t)
	t.Setenv("HEALTH_PUBLISH_TARGET", "127.0.0.1:50051")
	t.Setenv("HEALTH_PUBLISH_INSECURE", "true")

	socketPath := filepath.Join(t.TempDir(), "pc.sock")

	reporter, err := NewReporter(context.Background(), socketPath, "test-node", pb.ProcessingStrategy_STORE_ONLY, "")
	if err == nil {
		reporter.Close()
		t.Fatal("expected NewReporter to fail without HEALTH_PUBLISH_TOKEN_PATH")
	}
}

func TestNewReporterWaitsForSocketToAppear(t *testing.T) {
	// The node-local platform connector pod may still be starting when the
	// check runs: NewReporter waits for the socket file instead of failing.
	clearPublishEnv(t)
	shortenSocketWaits(t, 10*time.Second, 50*time.Millisecond, socketSendTimeout)

	socketPath := filepath.Join(t.TempDir(), "pc.sock")
	started := time.Now()
	done := newReporterAsync(context.Background(), socketPath)

	time.Sleep(300 * time.Millisecond)

	connector := serveConnector(t, listenUnix(t, socketPath))

	var result reporterResult

	select {
	case result = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("NewReporter did not return after the socket appeared")
	}

	if result.err != nil {
		t.Fatalf("NewReporter failed: %v", result.err)
	}

	t.Cleanup(result.reporter.Close)

	if waited := time.Since(started); waited < 300*time.Millisecond {
		t.Errorf("NewReporter returned after %v, before the socket existed", waited)
	}

	if err := result.reporter.SendEvent(context.Background(), true, false, "test event", ""); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	if captured := connector.calls(t); len(captured) != 1 {
		t.Errorf("got %d calls, want 1", len(captured))
	}
}

func TestNewReporterCancelledContextFails(t *testing.T) {
	// A context that is already over must not start a 20 second wait for a
	// socket that is not there.
	clearPublishEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	socketPath := filepath.Join(t.TempDir(), "absent.sock")

	reporter, err := NewReporter(ctx, socketPath, "test-node", pb.ProcessingStrategy_STORE_ONLY, "")
	if err == nil {
		reporter.Close()
		t.Fatal("expected NewReporter to fail with a cancelled context")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("got error %v, want it to wrap context.Canceled", err)
	}
}

func TestNewReporterWithoutSocketWarnsAndSendEventFails(t *testing.T) {
	// When the socket never appears NewReporter still returns a reporter (the
	// dial is lazy) and the send is what fails, keeping the send failure exit
	// code of the old reporter rather than a configuration error.
	clearPublishEnv(t)
	shortenSocketWaits(t, 300*time.Millisecond, 50*time.Millisecond, socketSendTimeout)

	socketPath := filepath.Join(t.TempDir(), "absent.sock")
	reporter := newTestReporter(t, socketPath, "")

	err := reporter.SendEvent(context.Background(), true, false, "test event", "")
	if err == nil {
		t.Fatal("expected SendEvent to fail without a platform connector socket")
	}

	if !errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
		t.Errorf("got error %v, want it to wrap ErrPlatformConnectorUnavailable", err)
	}
}

func TestSendEventWaitsForSocketToReappear(t *testing.T) {
	// The node-local platform connector can restart while the benchmark
	// runs. A missing socket at send time is waited for once, not failed at
	// once.
	clearPublishEnv(t)
	shortenSocketWaits(t, 10*time.Second, 50*time.Millisecond, socketSendTimeout)

	socketPath := filepath.Join(t.TempDir(), "pc.sock")
	firstServer := serveOn(t, listenUnix(t, socketPath), &capturingConnector{})
	reporter := newTestReporter(t, socketPath, "")

	// Stopping the server closes the listener, which removes the socket file.
	firstServer.Stop()

	if removeErr := os.Remove(socketPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		t.Fatalf("failed to remove socket file: %v", removeErr)
	}

	if _, statErr := os.Stat(socketPath); statErr == nil {
		t.Fatal("socket file still exists after the server stopped")
	}

	// The restarted connector is built here so its cleanup belongs to the
	// test; the goroutine only listens and serves once the socket is back.
	connector := &capturingConnector{}
	secondServer := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(secondServer, connector)
	t.Cleanup(secondServer.Stop)

	go func() {
		time.Sleep(300 * time.Millisecond)

		lis, err := net.Listen("unix", socketPath)
		if err != nil {
			return
		}

		_ = secondServer.Serve(lis)
	}()

	if err := reporter.SendEvent(context.Background(), true, false, "test event", ""); err != nil {
		t.Fatalf("SendEvent failed after the socket came back: %v", err)
	}

	if captured := connector.calls(t); len(captured) != 1 {
		t.Errorf("got %d calls on the restarted connector, want 1", len(captured))
	}
}

func TestSendEventTimesOutWhenHandlerHangs(t *testing.T) {
	// healthpub sets no deadline on the socket path, so the reporter bounds
	// each send itself: a hanging node-local connector must not block the
	// check forever.
	clearPublishEnv(t)
	shortenSocketWaits(t, socketWaitTimeout, socketPollInterval, 2*time.Second)

	socketPath := filepath.Join(t.TempDir(), "pc.sock")
	blocking := &blockingConnector{release: make(chan struct{})}

	t.Cleanup(func() { close(blocking.release) })

	serveOn(t, listenUnix(t, socketPath), blocking)

	reporter := newTestReporter(t, socketPath, "")

	done := make(chan error, 1)

	go func() {
		done <- reporter.SendEvent(context.Background(), true, false, "test event", "")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected SendEvent to fail when the handler never answers")
		}

		if !errors.Is(err, context.DeadlineExceeded) && status.Code(err) != codes.DeadlineExceeded {
			t.Errorf("got error %v, want a deadline error", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SendEvent did not return within the send timeout")
	}
}
