// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package state

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// TestEthState_FirstPollDownSingletonStorageIsSuppressed reproduces the
// OCI BM.GPU.H100.8 false positive: the Prime frontend NIC carries the
// default route and is excluded as Management, leaving its Aux twin
// (intentionally Disabled, no VNIC) as a singleton Storage card. With no
// peers to compare against there is no evidence the port should be up,
// so the first-poll DOWN must stay local to monitor logs — no HealthEvent.
func TestEthState_FirstPollDownSingletonStorageIsSuppressed(t *testing.T) {
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{ // compute fabric NIC (IB — ignored by eth check)
			pciAddress: "0000:18:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_2", &stubDevice{ // Prime frontend port, excluded via default route
			pciAddress: "0000:9a:00.0", numaNode: 0, netDev: "eth0",
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_11", &stubDevice{ // lone Aux frontend port, Disabled by design
			pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
		})
	node.nets["eth0"] = "up"
	node.nets["eth1"] = "down"

	reader := node.reader()
	routePath := writeProcNetRoute(t, "eth0")
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_2": {"NODE"}, "mlx5_11": {"NODE"}},
		routePath,
	)
	require.Equal(t, topology.RoleManagement, classifier.RoleOf("mlx5_2"))
	require.Equal(t, topology.RoleStorage, classifier.RoleOf("mlx5_11"))

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "singleton storage card has no peer evidence; first-poll DOWN must be suppressed")
}

// TestEthState_RuntimeActiveToDownStaysFatal guards the other side of the
// first-poll rule: a port that was observed healthy and then goes DOWN is
// a real failure and must stay fatal regardless of peer-group size.
func TestEthState_RuntimeActiveToDownStaysFatal(t *testing.T) {
	node := newStubNode().addIB("mlx5_11", &stubDevice{
		pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	node.nets["eth1"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_11": {"NODE"}},
	)

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	// Poll 1: healthy, first-seen — no events.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Port drops at runtime.
	node.ib["mlx5_11"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
	node.nets["eth1"] = "down"

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "runtime ACTIVE→DOWN is a real failure and must stay fatal")
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
}

func TestEthState_UnhealthySeverityEscalationEmitsFatal(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0, netDev: "eth0",
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	node.nets["eth0"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Ethernet intentionally suppresses transient INIT.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "INIT", physState: "LinkUp", linkLayer: "Ethernet"}
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Escalation inside the unhealthy region must not be swallowed.
	node.ib["mlx5_0"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
	node.nets["eth0"] = "down"
	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
}

// TestEthState_IBOnlyDeviceLifecycleIgnored verifies that the Ethernet
// check does not track — and therefore cannot latch — a device that
// exposes only InfiniBand ports. Before device tracking was scoped per
// link layer, removing an IB-only NIC produced a spurious "RoCE device
// disappeared" FATAL whose latch could never be consumed (the device has
// no Ethernet ports to turn healthy), permanently wedging the node.
func TestEthState_IBOnlyDeviceLifecycleIgnored(t *testing.T) {
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{ // IB-only fabric NIC
			pciAddress: "0000:47:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_8", &stubDevice{ // RoCE NIC owned by this check
			pciAddress: "0000:51:00.0", numaNode: 0, netDev: "eth0",
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		})
	node.nets["eth0"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_8": {"NODE"}})
	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Remove the IB-only device and poll past the disappearance debounce.
	removed := node.ib["mlx5_0"]
	delete(node.ib, "mlx5_0")

	for i := 0; i < 4; i++ {
		events, err = check.Run()
		require.NoError(t, err)
		assert.Empty(t, events,
			"the Ethernet check must not report lifecycle events for an IB-only device (poll %d)", i)
	}

	// Re-enumeration is equally invisible to this check.
	node.ib["mlx5_0"] = removed
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}

// TestEthState_LatchedDeviceReenumeratedWithoutEthPorts_EmitsDeviceRecovery
// covers the residual cross-layer path: a device the Ethernet check
// legitimately latched (it had Ethernet ports when it disappeared)
// re-enumerates with InfiniBand ports only. The per-port recovery path
// can never reach it, so the check must emit a device-level healthy
// event and clear the latch instead of orphaning the downstream FATAL.
func TestEthState_LatchedDeviceReenumeratedWithoutEthPorts_EmitsDeviceRecovery(t *testing.T) {
	node := newStubNode().addIB("mlx5_0", &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0, netDev: "eth0",
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	node.nets["eth0"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Confirmed disappearance: two debounced misses, FATAL on the third.
	delete(node.ib, "mlx5_0")

	for i := 0; i < 2; i++ {
		events, err = check.Run()
		require.NoError(t, err)
		assert.Empty(t, events, "miss %d must be debounced", i+1)
	}

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal)
	assert.Contains(t, events[0].Message, "disappeared")

	// The device returns reflashed to InfiniBand: no Ethernet ports.
	node.ib["mlx5_0"] = &stubDevice{
		pciAddress: "0000:47:00.0", numaNode: 0,
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
	}

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1, "re-enumeration without Ethernet ports must emit a device-level recovery")
	assert.True(t, events[0].IsHealthy)
	assert.Contains(t, events[0].Message, "re-enumerated")

	found := false

	for _, e := range events[0].EntitiesImpacted {
		if e.EntityType == checks.EntityTypeNIC && e.EntityValue == "mlx5_0" {
			found = true
		}
	}

	assert.True(t, found, "device recovery must carry the NIC entity so the downstream FATAL clears")

	// Latch consumed: subsequent polls stay silent.
	events, err = check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)
}
