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

package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

const (
	cspPollingInterval = 30 * time.Second
	processingBuffer   = 5 * time.Second
)

// TestCSPHealthMonitorGCPMaintenanceEvent verifies the complete GCP maintenance event lifecycle.
//
// Test Steps:
// 1. Setup: Configure csp-health-monitor for GCP, select clean test node, add GCP instance annotation
// 2. Inject PENDING maintenance event into mock API (scheduled 15 min ahead)
// 3. Verify quarantine: Wait for GCP monitor to poll (≤30s) + trigger engine (≤10s) → node cordoned with CSPMaintenance condition
// 4. Update to ONGOING status and verify node remains cordoned during active maintenance
// 5. Update to COMPLETE status and verify recovery: node uncordoned after 1-min healthy delay
// 6. Teardown: Restore original config, cleanup node
//
// Validates: Event detection, quarantine workflow, status transitions, and recovery behavior for GCP.
func TestCSPHealthMonitorGCPMaintenanceEvent(t *testing.T) {
	feature := features.New("TestCSPHealthMonitorGCPMaintenanceEvent").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTest(ctx, t, c, helpers.CSPGCP)

		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPGCP), "failed to clear GCP events")

		testInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())
		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.AddGCPInstanceIDAnnotation(ctx, client, testCtx.NodeName, testInstanceID, "us-central1-a"))
		t.Logf("Added GCP instance ID annotation: %s to node %s", testInstanceID, testCtx.NodeName)

		return newCtx
	})

	feature.Assess("Inject GCP PENDING maintenance event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		scheduledStart := time.Now().Add(15 * time.Minute)
		scheduledEnd := time.Now().Add(75 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPGCP,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Zone:            "us-central1-a",
			ProjectID:       "test-project",
			Status:          "PENDING",
			EventTypeCode:   "compute.instances.upcomingMaintenance",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Scheduled maintenance for GCP instance - e2e test",
		}

		var err error
		injectedEventID, _, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Injected GCP PENDING maintenance event: ID=%s", injectedEventID)

		return ctx
	})

	feature.Assess("Verify node is quarantined after GCP maintenance event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Waiting for CSP health monitor to process the event (polling interval: %v)...", cspPollingInterval)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)

		return ctx
	})

	feature.Assess("Update GCP event to ONGOING and verify node remains quarantined", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "ONGOING"))
		t.Logf("Updated GCP event to ONGOING status")

		time.Sleep(cspPollingInterval + processingBuffer)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.True(t, node.Spec.Unschedulable, "Node should remain cordoned during ONGOING maintenance")
		t.Logf("Node %s remains cordoned during ONGOING maintenance", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Update GCP event to COMPLETE and verify node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "COMPLETE"))
		t.Logf("Updated GCP event to COMPLETE status")

		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}

// TestCSPHealthMonitorAWSMaintenanceEvent verifies the complete AWS maintenance event lifecycle.
//
// Test Steps:
// 1. Setup: Configure csp-health-monitor for AWS, add AWS providerID to node, restart to sync informer
// 2. Inject 'upcoming' maintenance event into mock API (scheduled 15 min ahead)
// 3. Verify quarantine: Wait for AWS monitor to poll Health API (≤30s) + trigger engine (≤10s) → node cordoned
// 4. Update to 'open' status (maintenance in progress) and verify node remains cordoned
// 5. Update to 'closed' status, clear mock API to stop re-polling, verify recovery after 1-min healthy delay
// 6. Teardown: Restore original config, cleanup node
//
// Validates: AWS Health API integration, node mapping via providerID, and AWS-specific status transitions.
func TestCSPHealthMonitorAWSMaintenanceEvent(t *testing.T) {
	feature := features.New("TestCSPHealthMonitorAWSMaintenanceEvent").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTest(ctx, t, c, helpers.CSPAWS)

		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPAWS), "failed to clear AWS events")

		testInstanceID = fmt.Sprintf("i-%d", time.Now().UnixNano()%1000000000000)
		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.AddAWSProviderID(ctx, client, testCtx.NodeName, testInstanceID, "us-east-1a"))
		t.Logf("Added AWS provider ID with instance: %s to node %s", testInstanceID, testCtx.NodeName)

		// Restart so AWS node informer picks up the provider ID (uses AddFunc on initial sync)
		t.Log("Restarting csp-health-monitor to sync AWS node informer with provider ID")
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace))

		return newCtx
	})

	feature.Assess("Inject AWS upcoming maintenance event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		scheduledStart := time.Now().Add(15 * time.Minute)
		scheduledEnd := time.Now().Add(75 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPAWS,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Region:          "us-east-1",
			AccountID:       "123456789012",
			Status:          "upcoming",
			EventTypeCode:   "AWS_EC2_MAINTENANCE_SCHEDULED",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Instance is scheduled for maintenance.",
		}

		var err error
		var eventARN string
		injectedEventID, eventARN, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Injected AWS upcoming maintenance event: ID=%s, ARN=%s", injectedEventID, eventARN)

		return ctx
	})

	feature.Assess("Verify node is quarantined after AWS maintenance event", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Waiting for CSP health monitor to process the event (polling interval: %v)...", cspPollingInterval)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)

		return ctx
	})

	feature.Assess("Update AWS event to open and verify node remains quarantined", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPAWS, injectedEventID, "open"))
		t.Logf("Updated AWS event to open status")

		time.Sleep(cspPollingInterval + processingBuffer)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.True(t, node.Spec.Unschedulable, "Node should remain cordoned during open maintenance")
		t.Logf("Node %s remains cordoned during open maintenance", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Update AWS event to closed and verify node is uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPAWS, injectedEventID, "closed"))
		t.Logf("Updated AWS event to closed status")

		time.Sleep(cspPollingInterval + processingBuffer)

		// Clear event to stop re-polling (re-polls reset actualEndTime, blocking healthy trigger)
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPAWS))
		t.Logf("Cleared AWS event - trigger engine will use MongoDB record")

		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}

// TestCSPHealthMonitorQuarantineThreshold verifies the triggerQuarantineWorkflowTimeLimitMinutes threshold logic.
//
// Test Steps:
// 1. Setup: Configure csp-health-monitor with 1-minute quarantine threshold (minimum allowed)
// 2. Inject event scheduled 3 minutes ahead (OUTSIDE threshold) and verify NO quarantine occurs
// 3. Update same event to 50 seconds ahead (INSIDE threshold) and verify quarantine IS triggered
// 4. Update to COMPLETE and verify recovery
//
// Validates: Trigger engine only quarantines nodes when scheduledStartTime <= now + threshold.
// This ensures quarantine isn't triggered too early for distant maintenance windows.
func TestCSPHealthMonitorQuarantineThreshold(t *testing.T) {
	feature := features.New("TestCSPHealthMonitorQuarantineThreshold").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	// Use 1 minute threshold (minimum allowed) for faster testing
	const thresholdMinutes = 1

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTestWithThreshold(ctx, t, c, helpers.CSPGCP, thresholdMinutes)

		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPGCP), "failed to clear GCP events")

		testInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())
		client, err := c.NewClient()
		require.NoError(t, err)

		require.NoError(t, helpers.AddGCPInstanceIDAnnotation(ctx, client, testCtx.NodeName, testInstanceID, "us-central1-a"))
		t.Logf("Added GCP instance ID annotation: %s to node %s", testInstanceID, testCtx.NodeName)

		return newCtx
	})

	feature.Assess("Event outside threshold window does NOT trigger quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		scheduledStart := time.Now().Add(3 * time.Minute) // 3 min ahead, beyond 1 min threshold
		scheduledEnd := time.Now().Add(63 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPGCP,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Zone:            "us-central1-a",
			ProjectID:       "test-project",
			Status:          "PENDING",
			EventTypeCode:   "compute.instances.upcomingMaintenance",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Scheduled maintenance - threshold test",
		}

		var err error
		injectedEventID, _, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Injected event with scheduledStart=%v (3 min ahead, outside %d min threshold)", scheduledStart.Format(time.RFC3339), thresholdMinutes)

		t.Logf("Waiting for poll cycles to verify NO quarantine occurs...")
		time.Sleep(cspPollingInterval + processingBuffer)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.False(t, node.Spec.Unschedulable, "Node should NOT be cordoned - event is scheduled beyond threshold window")
		t.Logf("Verified: Node %s is NOT cordoned (event outside threshold)", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Event inside threshold window DOES trigger quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// 50s ahead accounts for polling delays (GCP: 30s + trigger: 10s = ~40s worst case)
		scheduledStart := time.Now().Add(50 * time.Second)

		require.NoError(t, testCtx.CSPClient.UpdateGCPEventScheduledTime(injectedEventID, scheduledStart))
		t.Logf("Updated event scheduledStart to %v (50s ahead, inside %d min threshold)", scheduledStart.Format(time.RFC3339), thresholdMinutes)

		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Waiting for quarantine to trigger...")
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)
		t.Logf("Verified: Node %s is now cordoned (event entered threshold window)", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Complete event and verify recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "COMPLETE"))
		t.Logf("Updated event to COMPLETE status")

		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)
		t.Logf("Verified: Node %s has recovered", testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}
