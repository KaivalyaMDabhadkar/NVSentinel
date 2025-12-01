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
package tests

import (
	"context"
	"fmt"
	"testing"
	"tests/helpers"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestMultipleRemediationsCompleted(t *testing.T) {
	feature := features.New("TestMultipleRemediationsCompleted").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", "")

		t.Log("Waiting 90 seconds for the MultipleRemediations rule time window to complete")
		time.Sleep(90 * time.Second)

		t.Log("Triggering multiple remediations cycle")
		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.TriggerMultipleRemediationsCycle(ctx, t, client, testCtx.NodeName)

		return newCtx
	})

	feature.Assess("Check if MultipleRemediations node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")
		gpuNodeName := testCtx.NodeName

		event := helpers.NewHealthEvent(gpuNodeName).
			WithFatal(true).
			WithErrorCode(helpers.ERRORCODE_31).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
		helpers.SendHealthEvent(ctx, t, event)

		// FIXME(dims): This is not happening correctly and causing failures in CI.
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, gpuNodeName, "MultipleRemediations",
			"ErrorCode:31 GPU:0 Recommended Action=CONTACT_SUPPORT;", "MultipleRemediationsIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestMultipleRemediationsNotTriggered(t *testing.T) {
	feature := features.New("TestMultipleRemediationsNotTriggered").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", "")

		t.Log("Waiting 90 seconds for the MultipleRemediations rule time window to complete")
		time.Sleep(90 * time.Second)

		gpuNodeName := testCtx.NodeName

		t.Logf("Injecting non-fatal events to node %s", gpuNodeName)
		for i := 0; i < 5; i++ {
			event := helpers.NewHealthEvent(gpuNodeName).
				WithFatal(false).
				WithErrorCode(helpers.ERRORCODE_13).
				WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))

			helpers.SendHealthEvent(ctx, t, event)

			helpers.SendHealthyEvent(ctx, t, gpuNodeName)
		}

		return newCtx
	})

	feature.Assess("Check if MultipleRemediations node condition is NOT added for non-fatal events", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		gpuNodeName := testCtx.NodeName

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		event := helpers.NewHealthEvent(gpuNodeName).
			WithFatal(false).
			WithErrorCode(helpers.ERRORCODE_13).
			WithRecommendedAction(int(pb.RecommendedAction_RESTART_VM))
		helpers.SendHealthEvent(ctx, t, event)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, gpuNodeName, "MultipleRemediations")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXIDOnSameGPU(t *testing.T) {
	// Works with both MongoDB ($setWindowFields pipeline) and PostgreSQL (XidBurstDetector).
	feature := features.New("TestRepeatedXIDOnSameGPU").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Waiting 190 seconds for the RepeatedXIDErrorOnSameGPU rule time window to complete")
		time.Sleep(190 * time.Second)

		return ctx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)

		// Burst 1: 5 events within 10s gaps (same burst)
		// Burst 1 contents: XID 119 (x2), 120, 48, 31
		// Expectations: No trigger yet (need at least 2 bursts to trigger)
		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 119, pid=1512646, name=kit, Timeout after 45s of waiting for RPC response from GPU2 GSP! Expected function 10 (FREE) (0x5c00014d 0x0)\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 120, pid=3110652, name=pt_main_thread, Ch 00000005",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 48, pid=3110652, name=pt_main_thread, Ch 00000008",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 119, pid=1512646, name=kit, Timeout after 45s of waiting for RPC response from GPU2 GSP! Expected function 10 (FREE) (0x5c00014d 0x0)\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU")

		t.Log("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 2: XID 120 (non-sticky) creates new burst after 12s gap
		// Burst 2 initial contents: XID 120, 79
		// Expectations: XID 120 triggers (appears in Burst 1 and Burst 2)
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 120, pid=3110652, name=pt_main_thread, Ch 00000005",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 79, pid=3110652, name=pt_main_thread, Ch 00000008",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		message := fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_120)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 2 (continued): XID 119 (sticky) arrives but merges into existing Burst 2
		// because XID 79 (sticky) occurred 12s ago (within 20s window)
		// Burst 2 final contents: XID 120, 79, 119, 48
		// Expectations: 119 and 48 trigger (both appear in Burst 1 and Burst 2)
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 119, pid=1512646, name=kit, Timeout after 45s of waiting for RPC response from GPU2 GSP! Expected function 10 (FREE) (0x5c00014d 0x0)\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 48, pid=3110652, name=pt_main_thread, Ch 00000008",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		t.Logf("Verifying RepeatedXIDErrorOnSameGPU condition exists after events merged into Burst 2")
		message += fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_119)
		message += fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 Recommended Action=CONTACT_SUPPORT;", helpers.ERRORCODE_48)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 3: XID 13 (non-sticky) creates new burst after 12s gap
		// Burst 3 contents: XID 13, 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 3)
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid=3110652, name=pt_main_thread, Ch 00000008",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		time.Sleep(5 * time.Second)

		// Burst 3 (continued): XID 13 arrives again after 5s gap (< 10s), stays in same burst
		// Burst 3 final contents: XID 13 (x2), 31 (x1)
		// Expectations: XID 13 will NOT trigger (only appears in Burst 3, and targetXidCount=2 in maxBurst),
		// 				 XID 31 will also not trigger as we are excluding XID 31 from RepeatedXIDErrorOnSameGPU rule
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid=3110652, name=pt_main_thread, Ch 00000008",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXIDErrorOnSameGPU",
			message, "RepeatedXIDErrorOnSameGPUIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, nodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXID31OnSameGPU(t *testing.T) {
	feature := features.New("TestRepeatedXID31OnSameGPU").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {

		t.Log("Waiting 130 seconds for the RepeatedXID31OnSameGPU rule time window to complete")
		time.Sleep(130 * time.Second)

		return ctx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)

		// Burst 1: 5 events within 10s gaps (same burst)
		// Burst 1 contents: XID 119, 31
		// Expectations: No trigger yet (need at least 2 bursts to trigger)
		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 119, pid=1512646, name=kit, Timeout after 45s of waiting for RPC response from GPU2 GSP! Expected function 10 (FREE) (0x5c00014d 0x0)\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID31OnDifferentGPU")

		t.Log("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 2: XID 31 (non-sticky) creates new burst after 12s gap
		// Burst 2 initial contents: XID 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 2 but with different PCI addresses)
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		expectedEvent := v1.Event{
			Type:    "RepeatedXID31OnDifferentGPU",
			Reason:  "RepeatedXID31OnDifferentGPUIsNotHealthy",
			Message: "ErrorCode:31 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 App passing bad data or using incorrect GPU methods; check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID31OnSameGPU")

		t.Logf("Waiting 12s to create burst gap")
		time.Sleep(12 * time.Second)

		// Burst 3: XID 13 (non-sticky) creates new burst after 12s gap
		// Burst 3 contents: XID 13, 31
		// Expectations: XID 31 triggers (appears in Burst 1 and Burst 3)
		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid=3110652, name=pt_main_thread, Ch 00000008",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		message := fmt.Sprintf("ErrorCode:%s PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 if DCGM EUD tests pass, run field diagnostics Recommended Action=RUN_DCGMEUD;", helpers.ERRORCODE_31)
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXID31OnSameGPU",
			message, "RepeatedXID31OnSameGPUIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, nodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestRepeatedXID31OnDifferentGPU(t *testing.T) {
	feature := features.New("TestRepeatedXID31OnDifferentGPU").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {

		t.Log("Waiting 70 seconds for the RepeatedXID31OnDifferentGPU rule time window to complete")
		time.Sleep(70 * time.Second)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)

		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 2): Out Of Range Address\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 31, pid=2079991, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID31OnDifferentGPU")

		t.Log("Waiting 5s to create burst gap")
		time.Sleep(5 * time.Second)

		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 31, pid=2079992, name=pt_main_thread, Ch 00000007, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_6 faulted @ 0x7f5a_e7504000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		return ctx
	})

	feature.Assess("Inject multiple XID errors and check if node condition is added if required", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(keyNodeName).(string)

		expectedEvent := v1.Event{
			Type:    "RepeatedXID31OnDifferentGPU",
			Reason:  "RepeatedXID31OnDifferentGPUIsNotHealthy",
			Message: "ErrorCode:31 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 App passing bad data or using incorrect GPU methods; check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, nodeName, expectedEvent)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, nodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestXIDErrorOnGPCAndTPC(t *testing.T) {
	feature := features.New("TestXIDErrorOnSameGPCAndTPC").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Waiting 190 seconds for the RepeatedXID13OnSameGPCAndTPC rule time window to complete")
		time.Sleep(190 * time.Second)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)

		// STEP 1: Inject two XID 13 errors on GPC:0, TPC:1, SM:0
		// EXPECTED: This alone won't trigger the "same" rule yet as it needs multiple occurrences
		// on the same GPC/TPC combination.
		t.Log("Inject XID 13 events on GPC: 0, TPC: 1, SM: 0")
		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 0): Out Of Range Address\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 0): Out Of Range Address\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID13OnDifferentGPCAndTPC")

		t.Log("Waiting 5s to create burst gap")
		time.Sleep(5 * time.Second)

		// STEP 2: Inject XID 13 error on GPC:0, TPC:0, SM:1
		// EXPECTED: This differs from the previous errors which were on GPC:0, TPC:1.
		// This should trigger the "RepeatedXID13OnDifferentGPCAndTPC" condition
		// because we have errors occurring on different processing clusters, indicating
		// a potentially broader GPU issue rather than a localized problem.
		t.Log("Inject XID 13 events on GPC: 0, TPC: 0, SM: 1")

		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 0, SM 1): Out Of Range Address\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		expectedEvent := v1.Event{
			Type:    "RepeatedXID13OnDifferentGPCAndTPC",
			Reason:  "RepeatedXID13OnDifferentGPCAndTPCIsNotHealthy",
			Message: "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:0 SM:1 App passing bad data or using incorrect GPU methods; check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		// EXPECTED: RepeatedXID13OnSameGPCAndTPC is not present.
		// Burst 1: XID 13 on GPC: 0, TPC: 1, SM: 0
		//          XID 13 on GPC: 0, TPC: 0, SM: 1
		// Errors 1 and 2 are combined into a single burst and thus counted only once.
		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testNodeName, "RepeatedXID13OnSameGPCAndTPC")

		t.Log("Waiting 5s to create burst gap")
		time.Sleep(5 * time.Second)

		// STEP 3: Inject XID 13 error on GPC:0, TPC:1, SM:1
		// EXPECTED: Now we have errors on two different GPC/TPC combinations:
		//   - GPC:0, TPC:0 (from Step 2)
		//   - GPC:0, TPC:1 (from Step 3)
		// This should trigger the "RepeatedXID13OnDifferentGPCAndTPC" condition
		// because we have errors occurring on different processing clusters, indicating
		// a potentially broader GPU issue rather than a localized problem.
		t.Log("Inject XID 13 events on GPC: 0, TPC: 1, SM: 1")

		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 1): Out Of Range Address\n",
		}
		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		expectedEvent = v1.Event{
			Type:    "RepeatedXID13OnDifferentGPCAndTPC",
			Reason:  "RepeatedXID13OnDifferentGPCAndTPCIsNotHealthy",
			Message: "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:1 SM:1 App passing bad data or using incorrect GPU methods; check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, testNodeName, expectedEvent)

		return ctx
	})

	feature.Assess("Check if RepeatedXID13OnSameGPCAndTPC node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		// EXPECTED: RepeatedXID13OnSameGPCAndTPC is present.
		// Even though we have injected three errors on the same GPC = 0, TPC = 1,
		// Burst 1: XID 13 on GPC: 0, TPC: 1, SM: 0
		//          XID 13 on GPC: 0, TPC: 1, SM: 0
		// Burst 3: XID 13 on GPC: 0, TPC: 1, SM: 1
		// Errors 1 and 2 are combined into a single burst and thus counted only once.
		message := "ErrorCode:13 PCI:0001:00:00 GPU_UUID:GPU-11111111-1111-1111-1111-111111111111 GPC:0 TPC:1 SM:1 if DCGM EUD tests pass, run field diagnostics Recommended Action=RUN_DCGMEUD;"
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testNodeName, "RepeatedXID13OnSameGPCAndTPC",
			message, "RepeatedXID13OnSameGPCAndTPCIsNotHealthy", v1.ConditionTrue)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testNodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}

func TestSoloNoBurstRule(t *testing.T) {
	feature := features.New("TestSoloNoBurstRule").
		WithLabel("suite", "health-event-analyzer")

	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var testNodeName string
	var syslogPod *v1.Pod

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {

		t.Log("Waiting 70 seconds for the XIDErrorSoloNoBurst rule time window to complete")
		time.Sleep(70 * time.Second)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		syslogPod, err = helpers.GetPodOnWorkerNode(ctx, t, client, helpers.NVSentinelNamespace, "syslog-health-monitor")
		require.NoError(t, err, "failed to find syslog health monitor pod")
		require.NotNil(t, syslogPod, "syslog health monitor pod should exist")

		testNodeName = syslogPod.Spec.NodeName
		t.Logf("Using syslog health monitor pod: %s on node: %s", syslogPod.Name, testNodeName)

		_, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c, "data/health-events-analyzer-config.yaml", "health-events-analyzer-test", testNodeName)

		metadata := helpers.CreateTestMetadata(testNodeName)
		helpers.InjectMetadata(t, ctx, client, syslogPod.Namespace, testNodeName, metadata)

		t.Logf("Setting up port-forward to pod %s on port %d", syslogPod.Name, stubJournalHTTPPort)
		stopChan, readyChan := helpers.PortForwardPod(
			ctx,
			client.RESTConfig(),
			syslogPod.Namespace,
			syslogPod.Name,
			stubJournalHTTPPort,
			stubJournalHTTPPort,
		)
		<-readyChan
		t.Log("Port-forward ready")

		ctx = context.WithValue(ctx, keyNodeName, testNodeName)
		ctx = context.WithValue(ctx, keySyslogPodName, syslogPod.Name)
		ctx = context.WithValue(ctx, keyStopChan, stopChan)

		xidMessages := []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 2): Out Of Range Address\n",
			"kernel: [16450076.435595] NVRM: Xid (PCI:0001:00:00): 13, pid='<unknown>', name=<unknown>, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 2): Out Of Range Address\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		t.Log("Waiting 5s to create burst gap")
		time.Sleep(5 * time.Second)

		xidMessages = []string{
			"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 13, pid='<unknown>', name=<unknown>, Graphics Exception: ESR 0x50df30=0x11f000e 0x50df34=0x20 0x50df28=0xf81eb60 0x50df2c=0x1174\n",
		}

		helpers.InjectSyslogMessages(t, stubJournalHTTPPort, xidMessages)

		return ctx
	})

	feature.Assess("Check if XIDErrorSoloNoBurst node condition is added", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		nodeName := ctx.Value(keyNodeName).(string)

		expectedEvent := v1.Event{
			Type:    "XIDErrorSoloNoBurst",
			Reason:  "XIDErrorSoloNoBurstIsNotHealthy",
			Message: "ErrorCode:13 PCI:0002:00:00 GPU_UUID:GPU-22222222-2222-2222-2222-222222222222 App passing bad data or using incorrect GPU methods; check error PID to identify source of the problem, if application is known good and problem persists, then contact support Recommended Action=NONE;",
		}

		helpers.WaitForNodeEvent(ctx, t, client, nodeName, expectedEvent)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if stopChanVal := ctx.Value(keyStopChan); stopChanVal != nil {
			t.Log("Stopping port-forward")
			close(stopChanVal.(chan struct{}))
		}

		client, err := c.NewClient()
		if err != nil {
			t.Logf("Warning: failed to create client for teardown: %v", err)
			return ctx
		}

		nodeNameVal := ctx.Value(keyNodeName)
		if nodeNameVal == nil {
			t.Log("Skipping teardown: nodeName not set (setup likely failed early)")
			return ctx
		}
		nodeName := nodeNameVal.(string)

		podNameVal := ctx.Value(keySyslogPodName)
		if podNameVal != nil {
			podName := podNameVal.(string)
			t.Logf("Restarting syslog-health-monitor pod %s to clear conditions", podName)
			err = helpers.DeletePod(ctx, client, helpers.NVSentinelNamespace, podName)
			if err != nil {
				t.Logf("Warning: failed to delete pod: %v", err)
			} else {
				t.Logf("Waiting for SysLogsXIDError condition to be cleared from node %s", nodeName)
				require.Eventually(t, func() bool {
					condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName,
						"SysLogsXIDError", "SysLogsXIDErrorIsHealthy")
					if err != nil {
						return false
					}
					return condition != nil && condition.Status == v1.ConditionFalse
				}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "SysLogsXIDError condition should be cleared")
			}
		}

		t.Logf("Cleaning up metadata from node %s", nodeName)
		helpers.DeleteMetadata(t, ctx, client, helpers.NVSentinelNamespace, nodeName)

		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, nodeName, testCtx.ConfigMapBackup)
	})

	testEnv.Test(t, feature.Feature())
}
