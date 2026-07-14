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
	"fmt"
	"log/slog"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// InfiniBandDegradationCheck monitors InfiniBand counter thresholds for
// both fatal and non-fatal degradation detection. The check owns its own
// Evaluator but shares the persistent state file with all sibling
// checks (state and degradation, IB and Ethernet).
type InfiniBandDegradationCheck struct {
	nodeName  string
	reader    sysfs.Reader
	cfg       *config.Config
	state     *statefile.Manager
	evaluator *Evaluator
	pending   *Evaluator
}

var _ checks.TransactionalCheck = (*InfiniBandDegradationCheck)(nil)

// NewInfiniBandDegradationCheck creates a new InfiniBandDegradationCheck.
// bootIDChanged is forwarded to the Evaluator so the first poll after a
// host reboot emits healthy baselines.
func NewInfiniBandDegradationCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) *InfiniBandDegradationCheck {
	evaluator := NewEvaluator(
		nodeName, reader, processingStrategy,
		state.CounterSnapshots(), state.BreachFlags(), bootIDChanged,
	)

	return &InfiniBandDegradationCheck{
		nodeName:  nodeName,
		reader:    reader,
		cfg:       cfg,
		state:     state,
		evaluator: evaluator,
	}
}

// Name returns the check identifier.
func (c *InfiniBandDegradationCheck) Name() string {
	return checks.InfiniBandDegradationCheckName
}

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *InfiniBandDegradationCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err != nil {
		return nil, err
	}

	c.Commit()

	return events, nil
}

// Prepare evaluates one poll against a cloned evaluator and stages the
// resulting counter state without mutating the committed evaluator.
func (c *InfiniBandDegradationCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	result, err := discovery.DiscoverDevicesWithOverride(
		c.reader, c.cfg.NicExclusionRegex, c.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to discover devices: %w", err)
	}
	if !result.Complete {
		if c.evaluator.HasState() {
			return nil, fmt.Errorf("failed to discover devices: InfiniBand sysfs tree unavailable")
		}

		// Do not consume the post-reboot baseline before the first complete
		// enumeration. Nodes without an IB tree remain a harmless no-op.
		return nil, nil
	}
	if len(result.UnreadableDevices) > 0 && !c.evaluator.HasState() {
		// A partial first poll cannot establish a trustworthy baseline for all
		// counters. Wait for a complete device read instead.
		return nil, nil
	}

	candidate := c.evaluator.Clone()

	var events []*pb.HealthEvent

	for i := range result.Devices {
		dev := &result.Devices[i]

		if !dev.IncludedByOverride && !discovery.IsSupportedVendor(dev) {
			continue
		}

		for j := range dev.Ports {
			port := &dev.Ports[j]

			if !discovery.IsIBPort(port) {
				continue
			}

			portEvents := candidate.EvaluateCounters(
				dev, port, c.cfg.CounterDetection.Counters, c.Name(),
			)
			events = append(events, portEvents...)
		}
	}

	candidate.ClearBootIDFlag()
	c.pending = candidate

	return events, nil
}

// Commit installs and persists the most recently prepared evaluator state.
func (c *InfiniBandDegradationCheck) Commit() {
	if c.pending == nil {
		return
	}

	c.evaluator = c.pending
	c.pending = nil
	c.persist()
}

// Discard abandons a prepared poll after check or publication failure.
func (c *InfiniBandDegradationCheck) Discard() {
	c.pending = nil
}

// persist writes any changes to the shared state file. Errors are
// logged and swallowed — the design explicitly chooses not to halt
// monitoring on persistence failures.
func (c *InfiniBandDegradationCheck) persist() {
	snapshotsChanged := c.state.UpdateCounterSnapshots(c.evaluator.Snapshots())
	flagsChanged := c.state.UpdateBreachFlags(c.evaluator.BreachFlags())

	if !snapshotsChanged && !flagsChanged {
		return
	}

	if err := c.state.Save(); err != nil {
		slog.Warn("Failed to persist counter state to disk",
			"check", c.Name(), "error", err)
	}
}
