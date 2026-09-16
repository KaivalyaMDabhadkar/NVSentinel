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

package connectors

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// fake records calls and answers with a scripted result: an error, a delay,
// or blocking until the context ends.
type fake struct {
	calls atomic.Int32
	err   error
	delay time.Duration
	block bool
	// endedBy records the context error the fake saw when it was cut short.
	endedBy atomic.Pointer[error]
}

func (f *fake) ProcessBatch(ctx context.Context, _ *pb.HealthEvents) error {
	f.calls.Add(1)

	if f.block {
		<-ctx.Done()
		err := ctx.Err()
		f.endedBy.Store(&err)

		return err
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			err := ctx.Err()
			f.endedBy.Store(&err)

			return err
		}
	}

	return f.err
}

func batch() *pb.HealthEvents {
	return &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-a", CheckName: "check"}}}
}

func failures(name, reason string) float64 {
	return testutil.ToFloat64(bestEffortFailures.WithLabelValues(name, reason))
}

// TestSet_EveryMemberGetsTheBatch: the happy path hands the batch to every
// member once and succeeds.
func TestSet_EveryMemberGetsTheBatch(t *testing.T) {
	members := []*fake{{}, {}, {}}

	set := Set{members[0], members[1], members[2]}
	require.NoError(t, set.ProcessBatch(context.Background(), batch()))

	for i, m := range members {
		require.EqualValues(t, 1, m.calls.Load(), "member %d", i)
	}
}

// TestSet_EmptyIsANoOp: a set with no members accepts the batch.
func TestSet_EmptyIsANoOp(t *testing.T) {
	require.NoError(t, Set{}.ProcessBatch(context.Background(), batch()))
}

// TestSet_FailsWithTheFirstErrorAndCancelsTheRest: a failing member decides
// the result, and the members still running are cancelled so the reply does
// not wait for work on a batch that will be resent.
func TestSet_FailsWithTheFirstErrorAndCancelsTheRest(t *testing.T) {
	failing := &fake{err: errors.New("primary stepped down")}
	blocking := &fake{block: true}

	start := time.Now()
	err := Set{blocking, failing}.ProcessBatch(context.Background(), batch())

	require.ErrorContains(t, err, "primary stepped down")
	require.Less(t, time.Since(start), 2*time.Second, "the blocking member was cancelled, not waited for")
	require.NotNil(t, blocking.endedBy.Load())
	require.ErrorIs(t, *blocking.endedBy.Load(), context.Canceled)
}

// funcConnector adapts a function to the Connector interface.
type funcConnector func(ctx context.Context, he *pb.HealthEvents) error

func (f funcConnector) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	return f(ctx, he)
}

// TestSet_MembersRunAtTheSameTime: each member waits until the other has
// started before it returns. Run one after the other they would wait for the
// context to end and fail; run together they both return.
func TestSet_MembersRunAtTheSameTime(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})

	member := funcConnector(func(ctx context.Context, _ *pb.HealthEvents) error {
		arrived <- struct{}{}

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	go func() {
		<-arrived
		<-arrived
		close(release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, Set{member, member}.ProcessBatch(ctx, batch()))
}

// TestSet_CallerCancellationReachesTheMembers: when the caller gives up, the
// members see it and the set reports the caller's context error.
func TestSet_CallerCancellationReachesTheMembers(t *testing.T) {
	blocking := &fake{block: true}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := Set{blocking}.ProcessBatch(ctx, batch())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestBestEffort_FailureIsCountedNotReturned: the wrapped connector's failure
// is counted as failed and the batch succeeds anyway.
func TestBestEffort_FailureIsCountedNotReturned(t *testing.T) {
	before := failures("k8s-failed", reasonFailed)

	c := BestEffort("k8s-failed", &fake{err: errors.New("node not found")}, time.Second)
	require.NoError(t, c.ProcessBatch(context.Background(), batch()))
	require.Equal(t, before+1, failures("k8s-failed", reasonFailed))
}

// TestBestEffort_TimeoutIsBoundedAndCounted: a hung connector cannot hold the
// batch past the bound; the timeout is counted separately from a failure.
func TestBestEffort_TimeoutIsBoundedAndCounted(t *testing.T) {
	before := failures("k8s-timeout", reasonTimedOut)

	c := BestEffort("k8s-timeout", &fake{block: true}, 50*time.Millisecond)

	start := time.Now()
	require.NoError(t, c.ProcessBatch(context.Background(), batch()))
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, before+1, failures("k8s-timeout", reasonTimedOut))
	require.Zero(t, failures("k8s-timeout", reasonFailed))
}

// TestBestEffort_CancelledRequestIsNotAFailure: when the caller gives up, the
// connector ends with the request and nothing is counted, since the batch is
// not acknowledged and will be resent.
func TestBestEffort_CancelledRequestIsNotAFailure(t *testing.T) {
	c := BestEffort("k8s-cancelled", &fake{block: true}, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	require.NoError(t, c.ProcessBatch(ctx, batch()))
	require.Zero(t, failures("k8s-cancelled", reasonFailed))
	require.Zero(t, failures("k8s-cancelled", reasonTimedOut))
}

// TestSet_BestEffortMemberDoesNotFailTheBatch: the deployment shape. The
// store decides; a failing or slow best-effort member is counted, and the
// batch is acknowledged once the store is done.
func TestSet_BestEffortMemberDoesNotFailTheBatch(t *testing.T) {
	store := &fake{}
	conditions := &fake{err: errors.New("conflict")}

	set := Set{store, BestEffort("k8s-set", conditions, time.Second)}
	require.NoError(t, set.ProcessBatch(context.Background(), batch()))
	require.EqualValues(t, 1, store.calls.Load())
	require.EqualValues(t, 1, conditions.calls.Load(), "the best-effort member runs alongside the store")
}

// TestSet_DeciderFailureCutsBestEffortShort: when the store fails, a
// best-effort member still running is cancelled and not counted as a
// failure, and the reply carries the store's error at once.
func TestSet_DeciderFailureCutsBestEffortShort(t *testing.T) {
	store := &fake{err: errors.New("primary stepped down")}
	conditions := &fake{block: true}

	set := Set{store, BestEffort("k8s-cut", conditions, time.Minute)}

	start := time.Now()
	err := set.ProcessBatch(context.Background(), batch())

	require.ErrorContains(t, err, "primary stepped down")
	require.Less(t, time.Since(start), 2*time.Second)
	require.Zero(t, failures("k8s-cut", reasonFailed))
	require.Zero(t, failures("k8s-cut", reasonTimedOut))
}
