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

// sequenceTracker tracks out-of-order completions from concurrent workers
// and determines the highest contiguously completed resume token.
//
// When N workers process events in parallel, they may complete out of order.
// The resume token must only advance to a point where ALL prior events are
// confirmed published. This tracker buffers out-of-order completions and
// yields the latest safe token when a contiguous run is available.
//
// Example:
//
//	Events dispatched: 1, 2, 3, 4, 5
//	Worker completions arrive: 2, 1, 5, 3, 4
//
//	After 2 completes: nextSeq=1, can't advance (1 pending)
//	After 1 completes: nextSeq=1, advance to 2 → token2
//	After 5 completes: nextSeq=3, can't advance (3 pending)
//	After 3 completes: nextSeq=3, advance to 3 → token3
//	After 4 completes: nextSeq=4, advance to 5 → token5
type sequenceTracker struct {
	completed map[uint64][]byte
	nextSeq   uint64
}

func newSequenceTracker() *sequenceTracker {
	return &sequenceTracker{
		completed: make(map[uint64][]byte),
		nextSeq:   1,
	}
}

// markCompleted records that the given sequence number has been published.
// Returns the latest contiguous resume token if one or more sequences
// can be advanced, or nil if there's still a gap.
func (t *sequenceTracker) markCompleted(seq uint64, resumeToken []byte) []byte {
	t.completed[seq] = resumeToken

	var latestToken []byte

	for {
		token, ok := t.completed[t.nextSeq]
		if !ok {
			break
		}

		latestToken = token

		delete(t.completed, t.nextSeq)
		t.nextSeq++
	}

	return latestToken
}

// pending returns the number of out-of-order completions buffered,
// waiting for earlier sequences to complete.
func (t *sequenceTracker) pending() int {
	return len(t.completed)
}
