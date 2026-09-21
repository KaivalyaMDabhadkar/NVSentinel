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

package central

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// scriptedVerifier answers each index check with the next result the test
// sends; a check blocks until the test provides one, which makes the loop's
// progress observable: when the n+1th check is waiting, the nth result has
// been applied.
type scriptedVerifier struct {
	results chan error
	calls   atomic.Int32
}

func (v *scriptedVerifier) VerifyIdempotencyIndex(ctx context.Context) error {
	v.calls.Add(1)

	select {
	case err := <-v.results:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *scriptedVerifier) awaitCall(t *testing.T, n int32) {
	t.Helper()
	require.Eventually(t, func() bool { return v.calls.Load() >= n }, 5*time.Second, time.Millisecond)
}

// TestWaitForIndex_ReadyOnceTheIndexVerifies: a datastore failure and a
// missing index keep the replica unready; the first verification makes it
// ready and ends the wait, so nothing re-checks the index afterwards.
func TestWaitForIndex_ReadyOnceTheIndexVerifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	verifier := &scriptedVerifier{results: make(chan error)}
	gate := &indexGate{}
	done := make(chan struct{})

	var waitErr error

	go func() {
		waitErr = waitForIndex(ctx, verifier, gate, time.Millisecond, 10*time.Second, time.Minute)
		close(done)
	}()

	verifier.awaitCall(t, 1)
	require.Error(t, gate.Ready(), "unready until the first verification")

	verifier.results <- errors.New("connection refused")
	verifier.awaitCall(t, 2)
	require.Error(t, gate.Ready(), "a datastore failure keeps the replica unready")

	verifier.results <- datastore.ErrIndexMissing
	verifier.awaitCall(t, 3)
	require.Error(t, gate.Ready(), "a missing index keeps the replica unready")

	verifier.results <- nil

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end after the index verified")
	}

	require.NoError(t, waitErr)
	require.NoError(t, gate.Ready(), "a verified index makes the replica ready")
	require.EqualValues(t, 3, verifier.calls.Load(), "no check after the first verification")
}

// TestWaitForIndex_StalledCheckDoesNotStopTheWait: a check that never
// returns ends with its own deadline and the wait goes on to the next check.
func TestWaitForIndex_StalledCheckDoesNotStopTheWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No result is ever sent: every check blocks until its own deadline.
	verifier := &scriptedVerifier{results: make(chan error)}
	gate := &indexGate{}

	go func() { _ = waitForIndex(ctx, verifier, gate, time.Millisecond, 20*time.Millisecond, time.Minute) }()

	verifier.awaitCall(t, 3)
	require.Error(t, gate.Ready(), "still unready while the checks time out")
}

// TestWaitForIndex_EndsWithTheContext: shutdown ends the wait without
// marking the replica ready.
func TestWaitForIndex_EndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	verifier := &scriptedVerifier{results: make(chan error)}
	gate := &indexGate{}
	done := make(chan struct{})

	var waitErr error

	go func() {
		waitErr = waitForIndex(ctx, verifier, gate, time.Millisecond, 10*time.Second, time.Minute)
		close(done)
	}()

	verifier.awaitCall(t, 1)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end with the context")
	}

	require.NoError(t, waitErr, "shutdown is not a failure")
	require.Error(t, gate.Ready())
}

// TestWaitForIndex_GivesUpAfterTheBudget: a replica that waits out the whole
// budget without a verified index returns the last check's error, so the
// process exits and the failure is visible as a crash loop.
func TestWaitForIndex_GivesUpAfterTheBudget(t *testing.T) {
	verifier := &scriptedVerifier{results: make(chan error, 100)}
	for range 100 {
		verifier.results <- datastore.ErrIndexMissing
	}

	gate := &indexGate{}

	err := waitForIndex(context.Background(), verifier, gate, time.Millisecond, time.Second, 10*time.Millisecond)

	require.ErrorContains(t, err, "not verified within 10ms")
	require.ErrorIs(t, err, datastore.ErrIndexMissing)
	require.Error(t, gate.Ready(), "still unready")
}

// recordingConnector keeps the last batch it was handed.
type recordingConnector struct {
	calls atomic.Int32
	mu    sync.Mutex
	last  *pb.HealthEvents
}

func (r *recordingConnector) ProcessBatch(_ context.Context, he *pb.HealthEvents) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.last = he
	r.mu.Unlock()

	return nil
}

func (r *recordingConnector) lastBatch() *pb.HealthEvents {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.last
}

// TestInterceptorChain_AsWired: the deployment's chain over a real gRPC
// server, auth then idempotency then readiness then the handler. A caller
// without a token is refused first; a missing key is refused before the
// readiness gate answers; while the index is unverified a keyed batch gets a
// retryable refusal; once verified the handler sees the batch stamped with
// the caller's pod UID and pinned to its node.
func TestInterceptorChain_AsWired(t *testing.T) {
	validator := validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a")))
	authInterceptor, err := newAuthInterceptor(context.Background(),
		auth.Settings{Enabled: true, Audience: testAudience}, validator)
	require.NoError(t, err)

	gate := &indexGate{}
	recorder := &recordingConnector{}

	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(authInterceptor, idempotencyInterceptor, readinessInterceptor(gate)))
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{Pipeline: pipeline.New(), Connector: recorder})

	lis := bufconn.Listen(1 << 20)

	go func() { _ = grpcServer.Serve(lis) }()

	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewPlatformConnectorClient(conn)

	send := func(token, key string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		if key != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, healthpub.IdempotencyKeyHeader, key)
		}

		_, err := client.HealthEventOccurredV1(ctx, batchNaming("node-a", ""))

		return err
	}

	require.Equal(t, codes.Unauthenticated, status.Code(send("", "batch-1")), "no token")
	require.Equal(t, codes.InvalidArgument, status.Code(send("tok", "")), "a missing key is refused before readiness")
	require.Equal(t, codes.Unavailable, status.Code(send("tok", "batch-1")), "the index is not verified yet")
	require.Zero(t, recorder.calls.Load(), "nothing reached the connector")

	gate.verified.Store(true)

	require.NoError(t, send("tok", "batch-1"))
	require.EqualValues(t, 1, recorder.calls.Load())

	got := recorder.lastBatch()
	require.Len(t, got.Events, 2)
	require.Equal(t, "pod-uid-1#batch-1#0", got.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "pod-uid-1#batch-1#1", got.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "node-a", got.Events[1].NodeName, "a blank node name is pinned to the token's node")
}
