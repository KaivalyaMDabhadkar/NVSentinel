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

// Package monitor hosts the orchestrator that runs registered checks on
// their polling cadence and forwards their events to the platform
// connector.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
)

const agentName = "nic-health-monitor"

// CounterPollingInterval is the fixed cadence for counter checks. It is
// intentionally not user-configurable: counter snapshots want a fast
// poll so delta counters are responsive and so velocity windows have
// fresh data when they evaluate. Velocity thresholds gate themselves on
// the configured velocityUnit, so a 1s read frequency is safe for every
// counter type without producing false alerts on long windows.
const CounterPollingInterval = time.Second

// NICHealthMonitor orchestrates state checks on the user-configurable
// state-polling cadence and counter checks on a fixed 1s cadence. Each
// category has its own polling loop so the two cadences are independent.
type NICHealthMonitor struct {
	nodeName string
	pub      *healthpub.Publisher

	stateChecks   []checks.Check
	counterChecks []checks.Check

	stateInterval time.Duration
}

// NewNICHealthMonitor constructs a NICHealthMonitor. The allChecks
// slice is automatically partitioned into state and counter categories
// based on each check's name. target must match the gRPC target string
// used to dial pcClient (typically "unix:///var/run/nvsentinel.sock").
func NewNICHealthMonitor(
	nodeName string,
	pcClient pb.PlatformConnectorClient,
	target string,
	allChecks []checks.Check,
	stateInterval time.Duration,
) *NICHealthMonitor {
	m := &NICHealthMonitor{
		nodeName:      nodeName,
		pub:           healthpub.New(pcClient, target, agentName),
		stateInterval: stateInterval,
	}

	for _, chk := range allChecks {
		switch checks.CategoryOf(chk.Name()) {
		case checks.StateCheck:
			m.stateChecks = append(m.stateChecks, chk)
		case checks.CounterCheck:
			m.counterChecks = append(m.counterChecks, chk)
		}
	}

	slog.Info("NIC Health Monitor initialized",
		"state_checks", len(m.stateChecks),
		"counter_checks", len(m.counterChecks),
		"state_interval", m.stateInterval,
		"counter_interval", CounterPollingInterval,
	)

	return m
}

// RunStateChecks executes all state-category checks once.
func (m *NICHealthMonitor) RunStateChecks(ctx context.Context) error {
	return m.runChecks(ctx, m.stateChecks, "state")
}

// RunCounterChecks executes all counter-category checks once.
func (m *NICHealthMonitor) RunCounterChecks(ctx context.Context) error {
	return m.runChecks(ctx, m.counterChecks, "counter")
}

// StateInterval returns the configurable state polling interval.
func (m *NICHealthMonitor) StateInterval() time.Duration { return m.stateInterval }

// checkTx adapts plain and transactional checks to one call surface so
// the polling loop stays linear. For non-transactional checks commit and
// discard are no-ops — their Run has already committed.
type checkTx struct {
	chk checks.Check
	tx  checks.TransactionalCheck
}

func newCheckTx(chk checks.Check) checkTx {
	tx, _ := chk.(checks.TransactionalCheck)

	return checkTx{chk: chk, tx: tx}
}

func (c checkTx) prepare() ([]*pb.HealthEvent, error) {
	if c.tx != nil {
		return c.tx.Prepare()
	}

	return c.chk.Run()
}

func (c checkTx) commit() {
	if c.tx != nil {
		c.tx.Commit()
	}
}

func (c checkTx) discard() {
	if c.tx != nil {
		c.tx.Discard()
	}
}

// runChecks executes the checks in a category and sends any resulting
// events in a single batch per check. Check errors are logged and do
// not cancel the remaining checks.
func (m *NICHealthMonitor) runChecks(ctx context.Context, checkList []checks.Check, category string) error {
	start := time.Now()

	for _, chk := range checkList {
		m.runOneCheck(ctx, newCheckTx(chk), category)
	}

	metrics.PollCycleDuration.WithLabelValues(m.nodeName, category).
		Observe(time.Since(start).Seconds())

	return nil
}

// runOneCheck prepares one check, publishes its events, and commits the
// staged state only after successful delivery so a failed publication
// cannot consume a health boundary.
func (m *NICHealthMonitor) runOneCheck(ctx context.Context, c checkTx, category string) {
	events, err := c.prepare()
	if err != nil {
		c.discard()
		slog.Error("Check failed",
			"check", c.chk.Name(),
			"category", category,
			"error", err,
		)

		return
	}

	if len(events) == 0 {
		c.commit()

		return
	}

	slog.Info("Check produced events", "check", c.chk.Name(), "count", len(events))

	batch := &pb.HealthEvents{Version: 1, Events: events}

	if err := m.pub.Publish(ctx, batch); err != nil {
		c.discard()
		slog.Error("Failed to send health events",
			"check", c.chk.Name(), "error", err)

		return
	}

	c.commit()
	m.logSentEvents(c.chk.Name(), events)
}

// logSentEvents records the per-event log line and delivery metric after
// a batch has been published.
func (m *NICHealthMonitor) logSentEvents(checkName string, events []*pb.HealthEvent) {
	for _, evt := range events {
		slog.Info("Health event sent",
			"check", evt.CheckName,
			"is_fatal", evt.IsFatal,
			"is_healthy", evt.IsHealthy,
			"recommended_action", evt.RecommendedAction.String(),
			"entities", formatEntities(evt.EntitiesImpacted),
			"message", evt.Message,
		)

		isFatal := "false"
		if evt.IsFatal {
			isFatal = "true"
		}

		metrics.HealthEventsSent.WithLabelValues(
			m.nodeName, checkName, isFatal,
		).Inc()
	}
}

// formatEntities produces a compact "NIC=mlx5_0, NICPort=1" string for logs.
func formatEntities(entities []*pb.Entity) string {
	if len(entities) == 0 {
		return "[]"
	}

	var b strings.Builder

	for i, e := range entities {
		if i > 0 {
			b.WriteString(", ")
		}

		fmt.Fprintf(&b, "%s=%s", e.EntityType, e.EntityValue)
	}

	return b.String()
}
