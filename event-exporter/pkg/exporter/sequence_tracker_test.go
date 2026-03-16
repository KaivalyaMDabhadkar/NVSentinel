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

package exporter

import (
	"bytes"
	"fmt"
	"testing"
)

// TestSequenceTracker_OutOfOrder exercises the core invariant: the resume token
// only advances to the highest contiguously completed sequence, even when
// workers finish out of order. Mirrors the example from the type doc comment.
func TestSequenceTracker_OutOfOrder(t *testing.T) {
	tracker := newSequenceTracker()
	tok := func(id int) []byte { return []byte(fmt.Sprintf("tok-%d", id)) }

	// seq 2 completes first — seq 1 still pending, no advancement
	if got := tracker.markCompleted(2, tok(2)); got != nil {
		t.Fatalf("after seq 2: expected nil (seq 1 pending), got %q", got)
	}

	if tracker.pending() != 1 {
		t.Fatalf("pending = %d, want 1", tracker.pending())
	}

	// seq 1 completes — contiguous run [1,2] unlocked, should return tok(2)
	if got := tracker.markCompleted(1, tok(1)); !bytes.Equal(got, tok(2)) {
		t.Fatalf("after seq 1: expected %q, got %q", tok(2), got)
	}

	// seq 5 arrives — gap at 3,4
	if got := tracker.markCompleted(5, tok(5)); got != nil {
		t.Fatalf("after seq 5: expected nil (gap at 3,4), got %q", got)
	}

	// seq 3 fills partly — only seq 3 is contiguous (4 still missing)
	if got := tracker.markCompleted(3, tok(3)); !bytes.Equal(got, tok(3)) {
		t.Fatalf("after seq 3: expected %q, got %q", tok(3), got)
	}

	// seq 4 fills the remaining gap — [4,5] flushed, return tok(5)
	if got := tracker.markCompleted(4, tok(4)); !bytes.Equal(got, tok(5)) {
		t.Fatalf("after seq 4: expected %q, got %q", tok(5), got)
	}

	if tracker.pending() != 0 {
		t.Fatalf("pending = %d, want 0 after all sequences complete", tracker.pending())
	}
}

// TestSequenceTracker_InOrder verifies that when completions arrive in
// sequence order, every call advances the token immediately (no buffering).
func TestSequenceTracker_InOrder(t *testing.T) {
	tracker := newSequenceTracker()

	for i := uint64(1); i <= 5; i++ {
		token := []byte(fmt.Sprintf("tok-%d", i))
		got := tracker.markCompleted(i, token)

		if !bytes.Equal(got, token) {
			t.Fatalf("seq %d: expected immediate advancement to %q, got %q", i, token, got)
		}

		if tracker.pending() != 0 {
			t.Fatalf("seq %d: pending = %d, want 0", i, tracker.pending())
		}
	}
}

// TestSequenceTracker_LargeGapFlush verifies that when many out-of-order
// completions are buffered behind a single missing sequence, completing
// that sequence flushes the entire buffer in one call and returns the
// token for the highest sequence.
func TestSequenceTracker_LargeGapFlush(t *testing.T) {
	tracker := newSequenceTracker()

	const n = 100

	// Complete sequences 2..n (all buffered behind missing seq 1)
	for i := uint64(2); i <= n; i++ {
		if got := tracker.markCompleted(i, []byte(fmt.Sprintf("tok-%d", i))); got != nil {
			t.Fatalf("seq %d: expected nil (seq 1 pending), got %q", i, got)
		}
	}

	if tracker.pending() != n-1 {
		t.Fatalf("pending = %d, want %d", tracker.pending(), n-1)
	}

	// Complete seq 1 — should flush everything, returning tok for seq n
	want := []byte(fmt.Sprintf("tok-%d", n))
	got := tracker.markCompleted(1, []byte("tok-1"))

	if !bytes.Equal(got, want) {
		t.Fatalf("after gap fill: expected %q, got %q", want, got)
	}

	if tracker.pending() != 0 {
		t.Fatalf("pending = %d, want 0 after full flush", tracker.pending())
	}
}
