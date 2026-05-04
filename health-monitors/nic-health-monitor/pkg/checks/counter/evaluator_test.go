// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package counter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

const (
	testNode    = "node1"
	testDevice  = "mlx5_0"
	testPort    = 1
	testIBLayer = "InfiniBand"
)

// newReaderFor returns a MockReader whose ReadIBPortCounter returns the
// pointee of the supplied uint64 each call. Tests flip the value
// mid-test by mutating the captured variable.
func newReaderFor(value *uint64) *sysfs.MockReader {
	return &sysfs.MockReader{
		ReadIBPortCounterFunc: func(_ string, _ int, _ string) (uint64, error) {
			return *value, nil
		},
	}
}

func newEvaluator(
	t *testing.T,
	reader sysfs.Reader,
	bootIDChanged bool,
) *Evaluator {
	t.Helper()

	return NewEvaluator(
		testNode, reader, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		nil, nil, bootIDChanged,
	)
}

func ibPort() *discovery.IBPort {
	return &discovery.IBPort{Device: testDevice, Port: testPort, LinkLayer: testIBLayer}
}

func ibDevice() *discovery.IBDevice {
	return &discovery.IBDevice{Name: testDevice}
}

func deltaCounter(threshold float64) config.CounterConfig {
	return config.CounterConfig{
		Name:              "link_downed",
		Path:              "counters/link_downed",
		Enabled:           true,
		IsFatal:           true,
		ThresholdType:     "delta",
		Threshold:         threshold,
		Description:       "QP disconnect",
		RecommendedAction: "REPLACE_VM",
	}
}

func velocityCounter(unit string, threshold float64) config.CounterConfig {
	return config.CounterConfig{
		Name:              "symbol_error_fatal",
		Path:              "counters/symbol_error",
		Enabled:           true,
		IsFatal:           true,
		ThresholdType:     "velocity",
		Threshold:         threshold,
		VelocityUnit:      unit,
		Description:       "BER spec violation",
		RecommendedAction: "REPLACE_VM",
	}
}

func TestCalculateDelta(t *testing.T) {
	tests := []struct {
		name     string
		current  uint64
		previous uint64
		expected uint64
	}{
		{"normal increment", 100, 50, 50},
		{"zero delta", 50, 50, 0},
		{"counter reset", 10, 1000, 10},
		{"from zero", 42, 0, 42},
		{"both zero", 0, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateDelta(tt.current, tt.previous)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestComputeRate(t *testing.T) {
	tests := []struct {
		name    string
		delta   uint64
		elapsed time.Duration
		unit    string
		want    float64
	}{
		{"per second", 100, 5 * time.Second, "second", 20.0},
		{"per minute", 100, 5 * time.Second, "minute", 1200.0},
		{"per hour", 100, 5 * time.Second, "hour", 72000.0},
		{"zero elapsed", 100, 0, "second", 0},
		{"unknown unit defaults to second", 100, 5 * time.Second, "unknown", 20.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeRate(tt.delta, tt.elapsed, tt.unit)
			assert.InDelta(t, tt.want, got, 0.01)
		})
	}
}

func TestResolveAction(t *testing.T) {
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, ResolveAction("REPLACE_VM"))
	assert.Equal(t, pb.RecommendedAction_NONE, ResolveAction("NONE"))
	assert.Equal(t, pb.RecommendedAction_NONE, ResolveAction(""))
	assert.Equal(t, pb.RecommendedAction_NONE, ResolveAction("unknown"))
}

func TestFatalCheckName(t *testing.T) {
	assert.Equal(t, checks.InfiniBandStateCheckName, fatalCheckName(checks.InfiniBandDegradationCheckName))
	assert.Equal(t, checks.EthernetStateCheckName, fatalCheckName(checks.EthernetDegradationCheckName))
	assert.Equal(t, "SomeOtherCheck", fatalCheckName("SomeOtherCheck"))
}

func TestEvaluateCounters_FirstPollSeedsBaselineNoEvent(t *testing.T) {
	value := uint64(100)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	events := ev.EvaluateCounters(ibDevice(), ibPort(),
		[]config.CounterConfig{deltaCounter(0)}, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "first poll should only seed snapshot, no event")

	snapshots := ev.Snapshots()
	require.Contains(t, snapshots, "mlx5_0:1:link_downed")
	assert.Equal(t, uint64(100), snapshots["mlx5_0:1:link_downed"].Value)
}

func TestEvaluateCounters_DeltaBreachAndLatch(t *testing.T) {
	value := uint64(0)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{deltaCounter(0)}

	// Poll 1: seed snapshot.
	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	// Poll 2: counter increments → fatal event under the IB STATE check
	// name (per fatalCheckName mapping).
	value = 1
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.False(t, events[0].IsHealthy)
	assert.Equal(t, checks.InfiniBandStateCheckName, events[0].CheckName)
	assert.Contains(t, events[0].Message, "link_downed")

	// Poll 3: delta returns to 0, but breach is latched → no event.
	events = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "latched breach must suppress further events")

	// Poll 4: counter increments again, still latched → no event.
	value = 2
	events = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "latched breach must suppress further events even on more increments")
}

func TestEvaluateCounters_ResetEmitsRecovery(t *testing.T) {
	value := uint64(0)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{deltaCounter(0)}

	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	value = 1
	require.Len(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName), 1)

	// Admin clears the counter -- next poll observes current < lastPollValue.
	value = 0
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy, "reset of breached counter must emit recovery")
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assert.Equal(t, checks.InfiniBandStateCheckName, events[0].CheckName)

	// Breach flag must be cleared. We expect the entry to remain in
	// the returned map with Breached=false so statefile.Manager can
	// propagate the deletion to the persisted state.
	flags := ev.BreachFlags()
	require.Contains(t, flags, "mlx5_0:1:link_downed")
	assert.False(t, flags["mlx5_0:1:link_downed"].Breached)
}

func TestEvaluateCounters_ResetWithoutBreachIsSilent(t *testing.T) {
	value := uint64(50)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{deltaCounter(100)}

	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	// Counter advances but stays below threshold (delta=10, threshold=100).
	value = 60
	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	// Counter reset (no breach was set).
	value = 0
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "reset without prior breach must be silent")

	snapshots := ev.Snapshots()
	assert.Equal(t, uint64(0), snapshots["mlx5_0:1:link_downed"].Value)
}

func TestEvaluateCounters_VelocityWaitsForFullWindow(t *testing.T) {
	value := uint64(1000)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{velocityCounter("hour", 120.0)}

	// Poll 1: seed snapshot at value=1000.
	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	// Subsequent polls within the window: huge spike. Without window
	// gating this would fire (1000 / 1s extrapolated to 3,600,000/hour).
	// With gating it must not fire because the 1-hour window is not
	// elapsed.
	value = 2000
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "velocity counter must not evaluate before window elapses")

	value = 5000
	events = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "velocity counter must remain silent for the full window")
}

// TestEvaluateCounters_VelocityEvaluatesAfterWindow exercises the full
// window path by injecting an old snapshot directly so we don't have to
// sleep an hour in tests.
func TestEvaluateCounters_VelocityEvaluatesAfterWindow(t *testing.T) {
	value := uint64(1200)
	reader := newReaderFor(&value)

	// Pre-seed a persisted snapshot from "1 hour ago" with value=1000.
	// Over that elapsed time the counter climbed to 1200 → 200/hour > 120/hour → breach.
	preSnapshots := map[string]statefile.CounterSnapshot{
		"mlx5_0:1:symbol_error_fatal": {
			Value:     1000,
			Timestamp: time.Now().Add(-time.Hour - time.Second),
		},
	}

	ev := NewEvaluator(testNode, reader, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		preSnapshots, nil, false)

	cfg := []config.CounterConfig{velocityCounter("hour", 120.0)}

	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.False(t, events[0].IsHealthy)
	assert.Contains(t, events[0].Message, "symbol_error_fatal")

	// Snapshot is advanced after evaluation.
	snapshots := ev.Snapshots()
	assert.Equal(t, uint64(1200), snapshots["mlx5_0:1:symbol_error_fatal"].Value)

	// Breach flag now set.
	flags := ev.BreachFlags()
	require.Contains(t, flags, "mlx5_0:1:symbol_error_fatal")
	assert.True(t, flags["mlx5_0:1:symbol_error_fatal"].Breached)
	assert.Equal(t, checks.InfiniBandStateCheckName, flags["mlx5_0:1:symbol_error_fatal"].CheckName)
}

// TestEvaluateCounters_VelocityBelowThresholdNoEvent verifies that an
// elapsed window with a low rate does not trigger a breach.
func TestEvaluateCounters_VelocityBelowThresholdNoEvent(t *testing.T) {
	value := uint64(1050) // 50 errors in 1 hour → 50/hour, below 120/hour
	reader := newReaderFor(&value)

	preSnapshots := map[string]statefile.CounterSnapshot{
		"mlx5_0:1:symbol_error_fatal": {
			Value:     1000,
			Timestamp: time.Now().Add(-time.Hour - time.Second),
		},
	}

	ev := NewEvaluator(testNode, reader, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		preSnapshots, nil, false)

	cfg := []config.CounterConfig{velocityCounter("hour", 120.0)}

	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "rate below threshold must not trigger")

	// Snapshot still advances even when no breach (window did elapse).
	snapshots := ev.Snapshots()
	assert.Equal(t, uint64(1050), snapshots["mlx5_0:1:symbol_error_fatal"].Value)
}

func TestEvaluateCounters_BootIDBaselineEmitsHealthy(t *testing.T) {
	value := uint64(0)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, true)

	cfg := []config.CounterConfig{deltaCounter(0)}

	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1, "boot ID baseline must emit one healthy event per configured counter")
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assert.Contains(t, events[0].Message, "healthy after reboot")

	// Subsequent polls must NOT emit baseline again, even though the
	// flag has not been cleared yet inside the evaluator (the
	// degradation check clears it via ClearBootIDFlag).
	ev.ClearBootIDFlag()

	value = 1
	events = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "after baseline, normal evaluation resumes")
	assert.False(t, events[0].IsHealthy)
}

// TestEvaluateCounters_VelocitySinglePollSpikeNoTrigger is the
// regression test for the original bug -- a single-poll spike on an
// hour-windowed counter must NOT be extrapolated into a breach.
func TestEvaluateCounters_VelocitySinglePollSpikeNoTrigger(t *testing.T) {
	value := uint64(0)
	reader := newReaderFor(&value)
	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{velocityCounter("hour", 120.0)}

	// Poll 1: seed at 0.
	require.Empty(t, ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName))

	// Poll 2 (1s later): a sudden spike of 1000 errors. Old code would
	// compute 1000 / (1/3600 hours) = 3.6M/hour and breach. New code
	// must wait for the full hour window.
	value = 1000
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events, "1s spike must not be extrapolated into an hourly breach")
}

// TestEvaluateCounters_DisabledCounterSkipped verifies that disabled
// counters are not even read.
func TestEvaluateCounters_DisabledCounterSkipped(t *testing.T) {
	called := 0
	reader := &sysfs.MockReader{
		ReadIBPortCounterFunc: func(_ string, _ int, _ string) (uint64, error) {
			called++
			return 0, nil
		},
	}

	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{
		{
			Name:    "link_downed",
			Path:    "counters/link_downed",
			Enabled: false,
		},
	}

	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	assert.Empty(t, events)
	assert.Zero(t, called, "disabled counters must not be read from sysfs")
}

// TestEvaluateCounters_CarrierChangesNotEvaluatedHere verifies that the
// interface-level carrier_changes counter is skipped by EvaluateCounters
// (it is handled by EvaluateNetCounters instead).
func TestEvaluateCounters_CarrierChangesNotEvaluatedHere(t *testing.T) {
	called := 0
	reader := &sysfs.MockReader{
		ReadIBPortCounterFunc: func(_ string, _ int, _ string) (uint64, error) {
			called++
			return 0, nil
		},
	}

	ev := newEvaluator(t, reader, false)

	cfg := []config.CounterConfig{
		{
			Name:          "carrier_changes",
			Path:          "statistics/carrier_changes",
			Enabled:       true,
			ThresholdType: "delta",
			Threshold:     2,
		},
	}

	_ = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.EthernetDegradationCheckName)
	assert.Zero(t, called, "carrier_changes must be routed through EvaluateNetCounters, not the IB path")
}

func TestEvaluateNetCounters_DeltaBreach(t *testing.T) {
	value := uint64(0)
	reader := &sysfs.MockReader{
		ReadNetStatisticFunc: func(_ string, _ string) (uint64, error) {
			return value, nil
		},
	}

	ev := newEvaluator(t, reader, false)

	dev := &discovery.IBDevice{Name: testDevice, NetDev: "eth0"}
	port := &discovery.IBPort{Device: testDevice, Port: testPort, LinkLayer: "Ethernet"}
	cfg := []config.CounterConfig{
		{
			Name:              "carrier_changes",
			Path:              "statistics/carrier_changes",
			Enabled:           true,
			IsFatal:           false,
			ThresholdType:     "delta",
			Threshold:         2,
			Description:       "carrier flap",
			RecommendedAction: "NONE",
		},
	}

	require.Empty(t, ev.EvaluateNetCounters(dev, port, cfg, checks.EthernetDegradationCheckName))

	value = 5
	events := ev.EvaluateNetCounters(dev, port, cfg, checks.EthernetDegradationCheckName)
	require.Len(t, events, 1)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, checks.EthernetDegradationCheckName, events[0].CheckName)
}

// TestEvaluateCounters_ResetAcrossRestartViaSnapshotFallback verifies
// the reset-detection fallback path: when the in-memory lastPollValue
// is missing (simulating a pod restart) but the persisted snapshot
// reports a higher value than the current reading, we still detect the
// reset and emit a recovery event for the previously breached counter.
func TestEvaluateCounters_ResetAcrossRestartViaSnapshotFallback(t *testing.T) {
	value := uint64(0)
	reader := newReaderFor(&value)

	// Pre-seed: counter was at 1 with breach=true before pod restarted.
	preSnapshots := map[string]statefile.CounterSnapshot{
		"mlx5_0:1:link_downed": {Value: 1, Timestamp: time.Now().Add(-time.Hour)},
	}
	preFlags := map[string]statefile.CounterBreachFlag{
		"mlx5_0:1:link_downed": {
			Breached:  true,
			CheckName: checks.InfiniBandStateCheckName,
			IsFatal:   true,
			Since:     time.Now().Add(-time.Hour),
		},
	}

	ev := NewEvaluator(testNode, reader, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		preSnapshots, preFlags, false)

	cfg := []config.CounterConfig{deltaCounter(0)}

	// First poll after restart: current=0, lastPollValue missing, snapshot.value=1.
	// 0 < 1 → reset detected → recovery emitted.
	events := ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsHealthy, "reset across restart must emit recovery for previously breached counter")
	assert.Equal(t, checks.InfiniBandStateCheckName, events[0].CheckName)
}

// TestEvaluator_DoesNotPersistForeignKeys is a test for the
// cross-evaluator clobber: when both IB and Ethernet evaluators are
// constructed from the SAME persisted state file, each gets a copy of
// the FULL map (containing the other check's keys). If an evaluator
// were to write back its full in-memory map, it would overwrite the
// sibling's updates with its own stale view. The Snapshots() and
// BreachFlags() getters must filter to only the keys this evaluator
// has actually touched.
func TestEvaluator_DoesNotPersistForeignKeys(t *testing.T) {
	value := uint64(10)
	reader := newReaderFor(&value)

	// Pre-load with a foreign key (e.g. from the sibling Ethernet check)
	// plus a key this IB evaluator owns. Only the IB key should appear
	// in Snapshots() / BreachFlags() after evaluation.
	preSnapshots := map[string]statefile.CounterSnapshot{
		"mlx5_0:1:link_downed":     {Value: 5, Timestamp: time.Now().Add(-time.Hour)},
		"mlx5_2:1:carrier_changes": {Value: 99, Timestamp: time.Now().Add(-time.Hour)}, // foreign
	}
	preFlags := map[string]statefile.CounterBreachFlag{
		"mlx5_2:1:carrier_changes": {Breached: true, CheckName: "EthernetDegradationCheck"}, // foreign
	}

	ev := NewEvaluator(testNode, reader, pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		preSnapshots, preFlags, false)

	cfg := []config.CounterConfig{deltaCounter(100)}
	_ = ev.EvaluateCounters(ibDevice(), ibPort(), cfg, checks.InfiniBandDegradationCheckName)

	snapshots := ev.Snapshots()
	assert.Contains(t, snapshots, "mlx5_0:1:link_downed",
		"IB evaluator must persist the IB key it actually touched")
	assert.NotContains(t, snapshots, "mlx5_2:1:carrier_changes",
		"IB evaluator must NOT persist a foreign Ethernet key carried in the loaded map")

	flags := ev.BreachFlags()
	assert.NotContains(t, flags, "mlx5_2:1:carrier_changes",
		"IB evaluator must NOT persist a foreign Ethernet breach flag from the loaded map")
}
