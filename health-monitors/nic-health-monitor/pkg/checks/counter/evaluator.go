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

// Package counter implements counter-based health checks for the NIC
// Health Monitor. The Evaluator reads sysfs counters once per poll and
// translates breaches into HealthEvents using the persistent snapshot
// model described in docs/nic-health-monitor/link-counter-detection.md.
//
// Three rules govern when an event is emitted:
//
//  1. Delta thresholds compare against the previous poll's snapshot
//     (snapshot updated every poll); breach == delta > threshold.
//  2. Velocity thresholds hold the snapshot for a fixed window matching
//     the configured velocityUnit (1s / 60s / 3600s) and only evaluate
//     once that window has fully elapsed; breach == rate > threshold.
//     The snapshot is then advanced to the current reading so the next
//     window starts fresh.
//  3. Breach is latching: the breach flag stays set until the counter
//     resets (current < previous) or the host reboots. Subsequent polls
//     while breached emit nothing. Recovery events fire only on counter
//     reset of a previously breached counter.
package counter

import (
	"fmt"
	"log/slog"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

const (
	thresholdTypeDelta    = "delta"
	thresholdTypeVelocity = "velocity"

	velocityUnitSecond = "second"
	velocityUnitMinute = "minute"
	velocityUnitHour   = "hour"

	netCarrierChangesPath = "statistics/carrier_changes"
)

// Evaluator owns the counter evaluation state for one degradation
// check. State is rehydrated from the statefile.Manager at construction
// and flushed back to it at the end of each poll cycle by the caller.
//
// The maps are keyed by `<device>:<port>:<counter_name>` (or
// `net:<iface>:<counter_name>` for interface-level counters such as
// carrier_changes).
type Evaluator struct {
	nodeName           string
	reader             sysfs.Reader
	processingStrategy pb.ProcessingStrategy

	// snapshots is the persisted (value, timestamp) per counter. For
	// delta thresholds it is updated every poll; for velocity thresholds
	// it is held for the duration of a window so rates are computed over
	// real elapsed time.
	snapshots map[string]statefile.CounterSnapshot

	// breachFlags is the persisted latching breach state per counter.
	// Once an entry has Breached=true the counter only emits a recovery
	// event after a counter reset is detected (current < previous).
	breachFlags map[string]statefile.CounterBreachFlag

	// lastPollValues is in-memory only; it lets us detect a counter
	// reset on the very next poll regardless of how stale the snapshot
	// is. Sysfs counters are monotonic, so current < lastPollValue is a
	// definitive reset signal. The map is empty after a pod restart, in
	// which case we fall back to comparing against snapshots[key].Value.
	lastPollValues map[string]uint64

	// ownedKeys tracks the counter keys this evaluator has actually
	// touched during its lifetime. Snapshots() and BreachFlags() filter
	// their returned maps to only these keys so that a sibling
	// evaluator's keys (e.g., the Ethernet check's keys carried in this
	// IB evaluator's map after a pod restart) are NOT written back to
	// the shared state file with stale data. Without this filter the
	// two evaluators would clobber each other's persisted state.
	ownedKeys map[string]struct{}

	// bootIDChanged signals that the first poll after construction must
	// emit a healthy baseline event for every configured counter to
	// clear stale FATAL conditions on the platform from the previous
	// boot. The flag self-clears after the first emission.
	bootIDChanged bool
}

// NewEvaluator constructs an Evaluator seeded with snapshots and breach
// flags loaded from the persistent state file. bootIDChanged should be
// true on the first poll after a host reboot so the evaluator emits
// healthy baselines.
func NewEvaluator(
	nodeName string,
	reader sysfs.Reader,
	processingStrategy pb.ProcessingStrategy,
	snapshots map[string]statefile.CounterSnapshot,
	breachFlags map[string]statefile.CounterBreachFlag,
	bootIDChanged bool,
) *Evaluator {
	if snapshots == nil {
		snapshots = make(map[string]statefile.CounterSnapshot)
	}

	if breachFlags == nil {
		breachFlags = make(map[string]statefile.CounterBreachFlag)
	}

	return &Evaluator{
		nodeName:           nodeName,
		reader:             reader,
		processingStrategy: processingStrategy,
		snapshots:          snapshots,
		breachFlags:        breachFlags,
		lastPollValues:     make(map[string]uint64),
		ownedKeys:          make(map[string]struct{}),
		bootIDChanged:      bootIDChanged,
	}
}

// Snapshots returns a copy of this evaluator's owned snapshots so the
// caller can persist them via statefile.Manager.UpdateCounterSnapshots.
// Only keys this evaluator has actually evaluated (via evaluateOne) are
// returned; cross-evaluator keys carried in the in-memory map (loaded
// from the shared state file at construction) are filtered out so the
// two checks cannot overwrite each other's persisted entries.
func (e *Evaluator) Snapshots() map[string]statefile.CounterSnapshot {
	out := make(map[string]statefile.CounterSnapshot, len(e.ownedKeys))

	for k := range e.ownedKeys {
		if v, ok := e.snapshots[k]; ok {
			out[k] = v
		}
	}

	return out
}

// BreachFlags returns a copy of this evaluator's owned breach flags.
// Keys explicitly cleared during this lifetime are returned with
// Breached=false so that statefile.Manager.UpdateBreachFlags can
// propagate the deletion to the persisted map (the merge logic deletes
// any incoming Breached=false entry).
func (e *Evaluator) BreachFlags() map[string]statefile.CounterBreachFlag {
	out := make(map[string]statefile.CounterBreachFlag, len(e.ownedKeys))

	for k := range e.ownedKeys {
		if v, ok := e.breachFlags[k]; ok {
			out[k] = v
		} else {
			// Owned but not currently breached — explicit Breached=false
			// signals the manager to delete any stale persisted entry.
			out[k] = statefile.CounterBreachFlag{Breached: false}
		}
	}

	return out
}

// EvaluateCounters reads each enabled, non-interface counter for the
// given port and returns any HealthEvents produced. Interface-level
// counters (statistics/carrier_changes) are routed through
// EvaluateNetCounters because they live under /sys/class/net.
func (e *Evaluator) EvaluateCounters(
	dev *discovery.IBDevice,
	port *discovery.IBPort,
	counters []config.CounterConfig,
	checkName string,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	now := time.Now()

	for _, counterCfg := range counters {
		if !counterCfg.Enabled || counterCfg.Path == netCarrierChangesPath {
			continue
		}

		currentValue, err := e.reader.ReadIBPortCounter(port.Device, port.Port, counterCfg.Path)
		if err != nil {
			continue
		}

		key := portCounterKey(port.Device, port.Port, counterCfg.Name)
		entities := checks.PortEntities(port.Device, port.Port)

		if event := e.evaluateOne(key, currentValue, now, counterCfg, entities, checkName); event != nil {
			events = append(events, event)
		}
	}

	return events
}

// EvaluateNetCounters reads interface-level counters (currently only
// carrier_changes) for the network device associated with the port.
func (e *Evaluator) EvaluateNetCounters(
	dev *discovery.IBDevice,
	port *discovery.IBPort,
	counters []config.CounterConfig,
	checkName string,
) []*pb.HealthEvent {
	if dev.NetDev == "" {
		return nil
	}

	var events []*pb.HealthEvent

	now := time.Now()

	for _, counterCfg := range counters {
		if !counterCfg.Enabled || counterCfg.Path != netCarrierChangesPath {
			continue
		}

		currentValue, err := e.reader.ReadNetStatistic(dev.NetDev, "carrier_changes")
		if err != nil {
			continue
		}

		key := netCounterKey(dev.NetDev, counterCfg.Name)
		entities := checks.PortEntities(port.Device, port.Port)

		if event := e.evaluateOne(key, currentValue, now, counterCfg, entities, checkName); event != nil {
			events = append(events, event)
		}
	}

	return events
}

// evaluateOne applies the snapshot/threshold/breach state machine for a
// single counter and returns an event if a health boundary was crossed.
// All snapshot, breach-flag, and last-poll-value mutations happen here.
func (e *Evaluator) evaluateOne(
	key string,
	currentValue uint64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	// Mark this key as owned by this evaluator. Snapshots() and
	// BreachFlags() filter on this so cross-evaluator keys carried in
	// the in-memory map (loaded from the shared state file at startup)
	// aren't written back when this evaluator persists.
	e.ownedKeys[key] = struct{}{}

	// Boot ID baseline takes precedence over everything else: emit a
	// healthy event, seed the snapshot, and skip evaluation. The flag is
	// not cleared per-counter — it is cleared at the end of the first
	// poll cycle by the degradation check (see ClearBootIDFlag).
	if e.bootIDChanged {
		e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}
		e.lastPollValues[key] = currentValue
		// Explicit Breached=false so the manager deletes any stale
		// persisted breach flag for this counter from the previous boot.
		e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

		return e.baselineEvent(counterCfg, entities, checkName)
	}

	defer func() { e.lastPollValues[key] = currentValue }()

	prev, hasSnapshot := e.snapshots[key]
	if !hasSnapshot {
		// First time seeing this counter. Seed snapshot only.
		e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

		return nil
	}

	if e.isReset(key, currentValue, prev.Value) {
		return e.handleReset(key, currentValue, now, counterCfg, entities, checkName)
	}

	// Latching breach: while we are already breached, suppress all
	// further events. The snapshot is left untouched so that the same
	// reset detection works whenever the admin clears the counter; a
	// post-reset window starts cleanly from the new baseline.
	if e.isBreached(key) {
		return nil
	}

	switch counterCfg.ThresholdType {
	case thresholdTypeDelta:
		return e.evaluateDelta(key, currentValue, prev, now, counterCfg, entities, checkName)
	case thresholdTypeVelocity:
		return e.evaluateVelocity(key, currentValue, prev, now, counterCfg, entities, checkName)
	default:
		slog.Warn("Unknown threshold type",
			"type", counterCfg.ThresholdType, "counter", counterCfg.Name)

		return nil
	}
}

// evaluateDelta evaluates a delta-typed counter. Snapshot is advanced
// every poll regardless of whether a breach was detected so the next
// poll's delta is measured against the latest reading.
func (e *Evaluator) evaluateDelta(
	key string,
	currentValue uint64,
	prev statefile.CounterSnapshot,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	delta := currentValue - prev.Value

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if float64(delta) <= counterCfg.Threshold {
		return nil
	}

	return e.recordBreach(key, currentValue, delta, 0, now, counterCfg, entities, checkName)
}

// evaluateVelocity evaluates a velocity-typed counter. The snapshot is
// held for the configured window so that the rate is computed over a
// real, fully elapsed window rather than a partial sample. Once the
// window has elapsed (and only then) the snapshot is advanced to the
// current reading so the next window measures fresh data.
func (e *Evaluator) evaluateVelocity(
	key string,
	currentValue uint64,
	prev statefile.CounterSnapshot,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	elapsed := now.Sub(prev.Timestamp)
	window := windowDuration(counterCfg.VelocityUnit)

	if elapsed < window {
		return nil
	}

	delta := currentValue - prev.Value
	rate := computeRate(delta, elapsed, counterCfg.VelocityUnit)

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if rate <= counterCfg.Threshold {
		return nil
	}

	return e.recordBreach(key, currentValue, delta, rate, now, counterCfg, entities, checkName)
}

// recordBreach builds the unhealthy event, sets the breach flag, and
// bumps the breach-counter metric. Caller must have already updated the
// snapshot.
func (e *Evaluator) recordBreach(
	key string,
	currentValue uint64,
	delta uint64,
	rate float64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	effectiveCheckName := checkName
	if counterCfg.IsFatal {
		effectiveCheckName = fatalCheckName(checkName)
	}

	device, port := devicePortFromEntities(entities)

	metrics.CounterThresholdBreaches.WithLabelValues(
		e.nodeName,
		counterCfg.Name,
		device,
		port,
		fmt.Sprintf("%t", counterCfg.IsFatal),
	).Inc()

	e.breachFlags[key] = statefile.CounterBreachFlag{
		Breached:  true,
		CheckName: effectiveCheckName,
		IsFatal:   counterCfg.IsFatal,
		Since:     now,
	}

	message := fmt.Sprintf(
		"Port %s port %s: %s - %s (value=%d, delta=%d, rate=%.2f/sec)",
		device, port, counterCfg.Name, counterCfg.Description,
		currentValue, delta, rate,
	)

	return checks.NewHealthEvent(
		e.nodeName, effectiveCheckName, message,
		entities,
		counterCfg.IsFatal, false,
		ResolveAction(counterCfg.RecommendedAction),
		e.processingStrategy,
	)
}

// handleReset clears the persisted breach flag (if any), advances the
// snapshot to the current reading, and emits a recovery event when the
// reset cleared a previously breached counter.
func (e *Evaluator) handleReset(
	key string,
	currentValue uint64,
	now time.Time,
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	flag, wasBreached := e.breachFlags[key]

	e.breachFlags[key] = statefile.CounterBreachFlag{Breached: false}

	e.snapshots[key] = statefile.CounterSnapshot{Value: currentValue, Timestamp: now}

	if !wasBreached || !flag.Breached {
		return nil
	}

	device, port := devicePortFromEntities(entities)
	message := fmt.Sprintf("Counter %s recovered on port %s port %s", counterCfg.Name, device, port)

	// Recovery uses the same CheckName as the original breach so the
	// platform clears the same condition. RecommendedAction on a
	// recovery is always NONE per design.
	recoveryCheckName := flag.CheckName
	if recoveryCheckName == "" {
		recoveryCheckName = checkName
		if counterCfg.IsFatal {
			recoveryCheckName = fatalCheckName(checkName)
		}
	}

	return checks.NewHealthEvent(
		e.nodeName, recoveryCheckName, message,
		entities,
		false, true,
		pb.RecommendedAction_NONE,
		e.processingStrategy,
	)
}

// baselineEvent constructs the IsHealthy=true event emitted on the
// first poll after a host reboot for every configured counter. Its
// purpose is to clear stale FATAL conditions left on the platform by
// the previous boot — see docs Section 6.5.
func (e *Evaluator) baselineEvent(
	counterCfg config.CounterConfig,
	entities []*pb.Entity,
	checkName string,
) *pb.HealthEvent {
	effectiveCheckName := checkName
	if counterCfg.IsFatal {
		effectiveCheckName = fatalCheckName(checkName)
	}

	device, port := devicePortFromEntities(entities)
	message := fmt.Sprintf("Counter %s healthy after reboot on port %s port %s",
		counterCfg.Name, device, port)

	return checks.NewHealthEvent(
		e.nodeName, effectiveCheckName, message,
		entities,
		false, true,
		pb.RecommendedAction_NONE,
		e.processingStrategy,
	)
}

// ClearBootIDFlag is called by the degradation check after the first
// poll completes so subsequent polls revert to normal evaluation.
func (e *Evaluator) ClearBootIDFlag() {
	e.bootIDChanged = false
}

// isReset returns true when the latest reading is strictly less than
// the previous poll's value (in-memory) or — if no in-memory value yet
// exists, e.g. the first poll after a pod restart — strictly less than
// the persisted snapshot value.
func (e *Evaluator) isReset(key string, currentValue, snapshotValue uint64) bool {
	if last, ok := e.lastPollValues[key]; ok {
		return currentValue < last
	}

	return currentValue < snapshotValue
}

// isBreached reports whether the persisted breach flag for this key is
// currently set.
func (e *Evaluator) isBreached(key string) bool {
	flag, ok := e.breachFlags[key]
	return ok && flag.Breached
}

// fatalCheckName maps a degradation check name to its corresponding
// state check name so that fatal counter events appear under the state
// check condition on the node, keeping all fatal signals under one
// check name.
func fatalCheckName(checkName string) string {
	switch checkName {
	case checks.InfiniBandDegradationCheckName:
		return checks.InfiniBandStateCheckName
	case checks.EthernetDegradationCheckName:
		return checks.EthernetStateCheckName
	default:
		return checkName
	}
}

// windowDuration returns the time window over which a velocity counter
// must accumulate samples before it is evaluated. It mirrors the
// configured velocityUnit one-to-one so a `120/hour` threshold actually
// observes a one-hour window rather than extrapolating from a 1s
// sample.
func windowDuration(unit string) time.Duration {
	switch unit {
	case velocityUnitSecond:
		return time.Second
	case velocityUnitMinute:
		return time.Minute
	case velocityUnitHour:
		return time.Hour
	default:
		return time.Second
	}
}

// computeRate converts a delta and elapsed duration into a rate
// expressed in the configured velocity unit (events per unit).
func computeRate(delta uint64, elapsed time.Duration, unit string) float64 {
	if elapsed <= 0 {
		return 0
	}

	switch unit {
	case velocityUnitSecond:
		return float64(delta) / elapsed.Seconds()
	case velocityUnitMinute:
		return float64(delta) / elapsed.Minutes()
	case velocityUnitHour:
		return float64(delta) / elapsed.Hours()
	default:
		return float64(delta) / elapsed.Seconds()
	}
}

// CalculateDelta is retained for compatibility with existing tests
// (used outside the evaluator). It applies the same monotonic-counter
// rule used inside evaluateOne: a current value below the previous
// value is treated as a reset and the entire current value is returned
// as the delta.
func CalculateDelta(current, previous uint64) uint64 {
	if current < previous {
		return current
	}

	return current - previous
}

// EvaluateThreshold is retained for compatibility with existing tests.
// It evaluates a single observation against the configured threshold
// using the same comparison the evaluator does internally.
func EvaluateThreshold(cfg config.CounterConfig, delta uint64, elapsed time.Duration) bool {
	switch cfg.ThresholdType {
	case thresholdTypeDelta:
		return float64(delta) > cfg.Threshold
	case thresholdTypeVelocity:
		return computeRate(delta, elapsed, cfg.VelocityUnit) > cfg.Threshold
	default:
		return false
	}
}

// ComputeRate is the exported form of computeRate for tests.
func ComputeRate(delta uint64, elapsed time.Duration, unit string) float64 {
	return computeRate(delta, elapsed, unit)
}

// ResolveAction maps a config string to the protobuf enum. Only the
// two values that appear in the default counter configs are recognised;
// everything else falls through to NONE.
func ResolveAction(action string) pb.RecommendedAction {
	if action == "REPLACE_VM" {
		return pb.RecommendedAction_REPLACE_VM
	}

	return pb.RecommendedAction_NONE
}

func portCounterKey(device string, port int, counterName string) string {
	return fmt.Sprintf("%s:%d:%s", device, port, counterName)
}

func netCounterKey(iface, counterName string) string {
	return fmt.Sprintf("net:%s:%s", iface, counterName)
}

// devicePortFromEntities extracts the NIC device name and port string
// from a HealthEvent entity slice produced by checks.PortEntities. It
// returns empty strings if the slice does not contain the expected
// shape — the caller falls back to those for log/metric labels.
func devicePortFromEntities(entities []*pb.Entity) (string, string) {
	var device, port string

	for _, e := range entities {
		switch e.EntityType {
		case checks.EntityTypeNIC:
			device = e.EntityValue
		case checks.EntityTypePort:
			port = e.EntityValue
		}
	}

	return device, port
}
