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

package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// countingConnector returns a connector over a fake clientset that counts node
// status updates, so the tests below can tell an update call from a skipped
// one.
func countingConnector(t *testing.T, onlyOnChange bool) (*K8sConnector, *int) {
	t.Helper()

	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	statusUpdates := 0

	clientset.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			statusUpdates++
		}

		return false, nil, nil
	})
	connector := NewK8sConnector(clientset, nil, nil, context.Background(), K8sConnectorConfig{
		MaxNodeConditionMessageLength: 1024,
		CompactedHealthEventMsgLen:    72,
		UpdateOnlyOnChange:            onlyOnChange,
	})

	return connector, &statusUpdates
}

func xidEvent(at time.Time, healthy bool) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          healthy,
		IsFatal:            !healthy,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"79"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
		Message:            "XID 79 on GPU 0",
		NodeName:           "node-a",
	}
}

func batch(events ...*protos.HealthEvent) *protos.HealthEvents {
	return &protos.HealthEvents{Version: 1, Events: events}
}

// TestUpdateOnlyOnChange_SkipsRepeats: the first fault is a transition and
// updates the node; the same fault again (a repeat, or a resent batch) changes
// nothing the node shows and must not cost an update call; a recovery is a
// transition again.
func TestUpdateOnlyOnChange_SkipsRepeats(t *testing.T) {
	connector, statusUpdates := countingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now, false))))
	require.Equal(t, 1, *statusUpdates, "the first fault is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now, false))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now.Add(time.Minute), false))))
	require.Equal(t, 1, *statusUpdates, "a repeat of the same fault changes nothing and is skipped")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now.Add(2*time.Minute), true))))
	require.Equal(t, 2, *statusUpdates, "the recovery is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now.Add(3*time.Minute), true))))
	require.Equal(t, 2, *statusUpdates, "healthy again is a repeat")
}

// TestUpdateOnlyOnChange_SaturatedMessageIsStillARepeat: when a node's
// condition message would exceed its length cap the stored entries are
// compacted, so their text never equals a repeat's full text again; the repeat
// must still count as no change, or exactly the busiest nodes would pay a
// status update on every repeat.
func TestUpdateOnlyOnChange_SaturatedMessageIsStillARepeat(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	statusUpdates := 0

	clientset.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			statusUpdates++
		}

		return false, nil, nil
	})
	connector := NewK8sConnector(clientset, nil, nil, context.Background(), K8sConnectorConfig{
		// Tight enough that six full messages do not fit and are compacted,
		// wide enough that the six compacted ones do.
		MaxNodeConditionMessageLength: 700,
		CompactedHealthEventMsgLen:    40,
		UpdateOnlyOnChange:            true,
	})
	ctx := context.Background()
	now := time.Now()

	faults := make([]*protos.HealthEvent, 0, 6)
	for gpu := range 6 {
		fault := xidEvent(now.Add(time.Duration(gpu)*time.Second), false)
		fault.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprint(gpu)}}
		fault.Message = fmt.Sprintf("XID 79 on GPU %d: %s", gpu, strings.Repeat("diagnostic detail ", 8))
		faults = append(faults, fault)
	}

	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))
	updatesAfterFirst := statusUpdates
	require.GreaterOrEqual(t, updatesAfterFirst, 1)

	// The same faults again, as one batch and one by one: nothing changed.
	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))

	for _, fault := range faults {
		require.NoError(t, connector.ProcessBatch(ctx, batch(fault)))
	}

	require.Equal(t, updatesAfterFirst, statusUpdates, "repeats of compacted faults are no change")
}

// TestUpdateOnlyOnChange_NewFaultJoiningIsAChange: a second fault on the same
// check adds a message, which the node does not show yet.
func TestUpdateOnlyOnChange_NewFaultJoiningIsAChange(t *testing.T) {
	connector, statusUpdates := countingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now, false))))

	second := xidEvent(now.Add(time.Second), false)
	second.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	require.Equal(t, 2, *statusUpdates, "a new fault joining an existing one changes the message")

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	require.Equal(t, 2, *statusUpdates)
}

// TestUpdateOnlyOnChange_OffKeepsHeartbeatUpdates: the DaemonSet keeps
// today's behavior, one status update per batch.
func TestUpdateOnlyOnChange_OffKeepsHeartbeatUpdates(t *testing.T) {
	connector, statusUpdates := countingConnector(t, false)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now, false))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(now, false))))
	require.Equal(t, 2, *statusUpdates)
}

// eventCountingConnector returns a connector over a fake clientset that counts
// the Kubernetes Events it creates (a create answered with AlreadyExists is
// not one) and the updates it makes.
func eventCountingConnector(t *testing.T, onlyOnChange bool) (*K8sConnector, *int, *int) {
	t.Helper()

	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	creates, updates := 0, 0

	clientset.PrependReactor("create", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		require.True(t, ok)

		object, ok := create.GetObject().(metav1.Object)
		require.True(t, ok)

		if _, err := clientset.Tracker().Get(action.GetResource(), action.GetNamespace(), object.GetName()); err != nil {
			creates++
		}

		return false, nil, nil
	})
	clientset.PrependReactor("update", "events", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		updates++

		return false, nil, nil
	})

	connector := NewK8sConnector(clientset, nil, nil, context.Background(), K8sConnectorConfig{
		MaxNodeConditionMessageLength: 1024,
		CompactedHealthEventMsgLen:    72,
		UpdateOnlyOnChange:            onlyOnChange,
	})

	return connector, &creates, &updates
}

// thermalEvent is a non-fatal fault, the kind that is announced as a
// Kubernetes Event rather than a node condition.
func thermalEvent(at time.Time, healthy bool, gpu string) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuThermalWatch",
		IsHealthy:          healthy,
		IsFatal:            false,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: gpu}},
		ErrorCode:          []string{"THERMAL_WARNING"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_NONE,
		Message:            "GPU " + gpu + " is hot",
		NodeName:           "node-a",
	}
}

// TestUpdateOnlyOnChange_EventsWrittenOnChange: a fault's first report
// creates its Event; repeats cost no API call; a fault on another GPU is a
// change; after the check recovers, the same fault is announced again.
func TestUpdateOnlyOnChange_EventsWrittenOnChange(t *testing.T) {
	connector, creates, updates := eventCountingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.Equal(t, 1, *creates, "the first report of a fault creates its Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), false, "0"))))
	require.Equal(t, 1, *creates, "a repeat, or a resent batch, writes nothing")
	require.Equal(t, 0, *updates)

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), false, "1"))))
	require.Equal(t, 2, *creates, "a fault on another GPU is a change")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(2*time.Minute), true, "0"))))
	require.Equal(t, 2, *creates, "a recovery writes no Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(3*time.Minute), false, "0"))))
	require.Equal(t, 2, *creates, "the fault's return reuses its Event, which still exists in the cluster")
	require.Equal(t, 1, *updates, "the return after a recovery is announced by refreshing that Event")
}

// TestUpdateOnlyOnChange_EventRefreshedAfterInterval: once the refresh
// interval has passed, the next repeat refreshes the existing Event (count and
// timestamp) instead of creating another one.
func TestUpdateOnlyOnChange_EventRefreshedAfterInterval(t *testing.T) {
	connector, creates, updates := eventCountingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.Equal(t, 1, *creates)

	k8sEvent := connector.createK8sEvent(ctx, thermalEvent(now, false, "0"))

	connector.nodeEventMu.Lock()
	written, ok := connector.nodeEventMemory().Get(nodeCheckKey("node-a", k8sEvent.Type))
	require.True(t, ok)
	remembered := written[k8sEvent.Message]
	remembered.writtenAt = now.Add(-nodeEventRefreshInterval - time.Second)
	written[k8sEvent.Message] = remembered
	connector.nodeEventMu.Unlock()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), false, "0"))))
	require.Equal(t, 1, *creates, "the refresh reuses the existing Event")
	require.Equal(t, 1, *updates, "the refresh bumps its count")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(2*time.Minute), false, "0"))))
	require.Equal(t, 1, *updates, "the refresh restarts the interval")
}

// TestUpdateOnlyOnChange_OffBumpsEventCount: the DaemonSet keeps today's
// behavior, every repeat bumps the Event's count.
func TestUpdateOnlyOnChange_OffBumpsEventCount(t *testing.T) {
	connector, creates, updates := eventCountingConnector(t, false)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), true, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(2*time.Minute), false, "0"))))
	require.Equal(t, 1, *creates)
	require.Equal(t, 2, *updates, "repeats bump the count, also after a recovery")
}

// TestUpdateOnlyOnChange_EventsFollowTimestampOrder: a batch is processed in
// timestamp order, like the condition path, so an older recovery that arrives
// after a newer fault in the same batch does not erase the memory of that
// fault, which would announce it again on its next repeat.
func TestUpdateOnlyOnChange_EventsFollowTimestampOrder(t *testing.T) {
	connector, creates, updates := eventCountingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	// Wire order: the fault first, then a recovery that is a minute older.
	require.NoError(t, connector.ProcessBatch(ctx, batch(
		thermalEvent(now.Add(time.Minute), false, "0"),
		thermalEvent(now, true, "0"),
	)))
	require.Equal(t, 1, *creates, "the fault, the latest word on GPU 0, is announced")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(2*time.Minute), false, "0"))))
	require.Equal(t, 1, *creates)
	require.Equal(t, 0, *updates, "the fault is still remembered: the older recovery did not erase it")
}

// TestUpdateOnlyOnChange_PartialRecoveryKeepsOtherFaults: a recovery names
// the entities that recovered, so only their Events are forgotten; a fault on
// another entity of the same check stays a repeat.
func TestUpdateOnlyOnChange_PartialRecoveryKeepsOtherFaults(t *testing.T) {
	connector, creates, updates := eventCountingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now, false, "1"))))
	require.Equal(t, 2, *creates)

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), true, "0"))))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(2*time.Minute), false, "1"))))
	require.Equal(t, 2, *creates, "GPU 1 is still the same fault; GPU 0 recovering does not re-announce it")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(3*time.Minute), false, "0"))))
	require.Equal(t, 2, *creates)
	require.Equal(t, 1, *updates, "GPU 0 faulting again after its recovery is announced by refreshing its Event")

	// A recovery naming no entity clears the whole check.
	recoveredAll := thermalEvent(now.Add(4*time.Minute), true, "0")
	recoveredAll.EntitiesImpacted = nil
	require.NoError(t, connector.ProcessBatch(ctx, batch(recoveredAll)))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(now.Add(5*time.Minute), false, "1"))))
	require.Equal(t, 2, *creates)
	require.Equal(t, 2, *updates, "GPU 1 is announced again the same way")
}

// TestNodeEventMemory_BoundsMessagesPerCheck: the message is producer
// controlled, so the Events remembered for one check on one node are bounded;
// past the bound the check's memory is dropped and starts again with the
// entry being written.
func TestNodeEventMemory_BoundsMessagesPerCheck(t *testing.T) {
	connector := &K8sConnector{}

	for i := range maxRememberedMessagesPerCheck + 8 {
		connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: fmt.Sprintf("message-%d", i)}, nil)
	}

	connector.nodeEventMu.Lock()
	written, ok := connector.nodeEventMemory().Get(nodeCheckKey("node-a", "check"))
	connector.nodeEventMu.Unlock()

	require.True(t, ok)
	require.LessOrEqual(t, len(written), maxRememberedMessagesPerCheck)

	last := fmt.Sprintf("message-%d", maxRememberedMessagesPerCheck+7)
	_, ok = connector.rememberedNodeEvent("node-a", &corev1.Event{Type: "check", Message: last})
	require.True(t, ok, "the newest entry is kept")

	_, ok = connector.rememberedNodeEvent("node-a", &corev1.Event{Type: "check", Message: "message-0"})
	require.False(t, ok, "the memory was dropped when it filled up")
}

// TestNodeEventName_DerivedFromTheFault: the same fault gets the same name on
// every replica and across restarts; a different message is a different
// Event.
func TestNodeEventName_DerivedFromTheFault(t *testing.T) {
	connector, _, _ := eventCountingConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	first := connector.createK8sEvent(ctx, thermalEvent(now, false, "0"))
	again := connector.createK8sEvent(ctx, thermalEvent(now.Add(time.Hour), false, "0"))
	other := connector.createK8sEvent(ctx, thermalEvent(now, false, "1"))

	require.Equal(t, first.Name, again.Name, "the time of the report does not change the name")
	require.NotEqual(t, first.Name, other.Name, "another GPU is another Event")
	require.Regexp(t, `^node-a\.[0-9a-f]{16}$`, first.Name)
}

// TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent: a replica that
// has never seen a fault (or lost its memory of it) finds the Event another
// replica wrote and bumps it instead of writing a second one. The same holds
// for the DaemonSet after a restart.
func TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent(t *testing.T) {
	for _, onlyOnChange := range []bool{true, false} {
		clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
		cfg := K8sConnectorConfig{
			MaxNodeConditionMessageLength: 1024,
			CompactedHealthEventMsgLen:    72,
			UpdateOnlyOnChange:            onlyOnChange,
		}
		ctx := context.Background()
		now := time.Now()

		first := NewK8sConnector(clientset, nil, nil, ctx, cfg)
		require.NoError(t, first.ProcessBatch(ctx, batch(thermalEvent(now, false, "0"))))

		// Another replica, or the same process after a restart: empty memory.
		second := NewK8sConnector(clientset, nil, nil, ctx, cfg)
		require.NoError(t, second.ProcessBatch(ctx, batch(thermalEvent(now.Add(time.Minute), false, "0"))))

		events, err := clientset.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, events.Items, 1, "one Event per fault, however many replicas saw it (onlyOnChange=%v)", onlyOnChange)
		require.Equal(t, int32(2), events.Items[0].Count, "the second replica bumped the existing Event")

		_, known := second.rememberedNodeEvent("node-a", second.createK8sEvent(ctx, thermalEvent(now, false, "0")))
		require.True(t, known, "the second replica now remembers the Event it refreshed")
	}
}
