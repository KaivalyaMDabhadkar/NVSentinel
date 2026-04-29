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

package helpers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	NICHealthMonitorDaemonSetName = "nic-health-monitor"
	NICHealthMonitorContainerName = "nic-health-monitor"
	NICHealthMonitorConfigMapName = "nic-health-monitor-config"

	FakeSysfsIBPath  = "/var/lib/nvsentinel/fake-ib"
	FakeSysfsNetPath = "/var/lib/nvsentinel/fake-net"
	FakeBootIDPath   = "/var/lib/nvsentinel/fake-proc/sys/kernel/random/boot_id"

	fakeSysfsHostBase = "/var/lib/nvsentinel"
	busyboxImage      = "busybox:latest"
)

// NICTestState bundles everything needed to tear down a NIC test cleanly.
type NICTestState struct {
	NodeName       string
	PodName        string
	OriginalArgs   []string
	ConfigSnapshot *NICMonitorConfigMapSnapshot
}

// SetUpNICHealthMonitor finds the NIC monitor DaemonSet pod, creates the
// fake sysfs tree on the same node, injects NIC-specific metadata, updates
// the DaemonSet args to use the fake paths, and waits for the rollout.
// Returns the node name, current pod, and a NICTestState for teardown.
func SetUpNICHealthMonitor(ctx context.Context, t *testing.T,
	client klient.Client,
) (string, *corev1.Pod, *NICTestState) {
	t.Helper()

	nicPod, err := GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor")
	require.NoError(t, err, "failed to get NIC health monitor pod on worker node")
	require.NotNil(t, nicPod, "NIC health monitor pod should exist on worker node")

	testNodeName := nicPod.Spec.NodeName
	t.Logf("Using NIC health monitor pod: %s on node: %s", nicPod.Name, testNodeName)

	// Remove any stale NIC conditions left from previous runs on this node.
	t.Logf("Clearing stale NIC conditions from node %s", testNodeName)
	removeNodeConditions(ctx, t, client, testNodeName,
		"InfiniBandStateCheck", "EthernetStateCheck")

	configSnapshot := BackupNICConfigMap(t, ctx, client)

	CreateFakeSysfsTree(t, ctx, client, NVSentinelNamespace, testNodeName)

	metadata := CreateNICTestMetadata(testNodeName)
	InjectMetadata(t, ctx, client, NVSentinelNamespace, testNodeName, metadata)

	// updateNICMonitorForFakeSysfs updates the ConfigMap and DaemonSet args,
	// which triggers a rolling update. However, if the args haven't changed
	// since a previous run, the rollout may be a no-op and existing pods
	// keep the old cached config. Explicitly delete the pod on the target
	// node to guarantee a fresh start with the new ConfigMap.
	originalArgs := updateNICMonitorForFakeSysfs(t, ctx, client)

	// Write a unique boot_id so the replacement pod detects
	// boot_id_changed=true and emits fresh healthy baselines,
	// regardless of any stale persisted state file.
	newBootID := fmt.Sprintf("test-boot-id-%d", time.Now().UnixNano())
	t.Logf("Writing new boot_id %s to force baseline emission", newBootID)
	MutateBootID(t, ctx, client, NVSentinelNamespace, testNodeName, newBootID)

	t.Logf("Deleting NIC monitor pod on node %s to force config reload", testNodeName)
	nicPod, err = GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor", testNodeName)
	require.NoError(t, err, "failed to find NIC monitor pod to restart")
	err = DeletePod(ctx, t, client, NVSentinelNamespace, nicPod.Name, true)
	require.NoError(t, err, "failed to delete NIC monitor pod")

	t.Logf("Waiting for replacement NIC monitor pod on node %s", testNodeName)
	nicPod, err = GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor", testNodeName)
	require.NoError(t, err, "failed to get replacement NIC health monitor pod on node %s", testNodeName)
	t.Logf("NIC health monitor pod ready: %s on node: %s", nicPod.Name, nicPod.Spec.NodeName)

	t.Logf("Setting ManagedByNVSentinel=true on node %s", testNodeName)
	err = SetNodeManagedByNVSentinel(ctx, client, testNodeName, true)
	require.NoError(t, err, "failed to set ManagedByNVSentinel label")

	state := &NICTestState{
		NodeName:       testNodeName,
		PodName:        nicPod.Name,
		OriginalArgs:   originalArgs,
		ConfigSnapshot: configSnapshot,
	}

	return testNodeName, nicPod, state
}

// TearDownNICHealthMonitor restores the DaemonSet args and ConfigMap,
// cleans up fake sysfs and metadata, and removes the ManagedByNVSentinel
// label. Idempotent — safe to call even if setup failed partway through.
func TearDownNICHealthMonitor(ctx context.Context, t *testing.T,
	client klient.Client, state *NICTestState,
) {
	t.Helper()

	if state == nil {
		return
	}

	if state.ConfigSnapshot != nil {
		RestoreNICConfigMap(t, ctx, client, state.ConfigSnapshot)
	}

	if state.OriginalArgs != nil {
		RestoreDaemonSetArgs(ctx, t, client,
			NICHealthMonitorDaemonSetName, NICHealthMonitorContainerName, state.OriginalArgs)
	}

	if state.NodeName != "" {
		CleanupFakeSysfs(t, ctx, client, NVSentinelNamespace, state.NodeName)

		t.Logf("Removing stale NIC conditions from node %s", state.NodeName)
		removeNodeConditions(ctx, t, client, state.NodeName,
			"InfiniBandStateCheck", "EthernetStateCheck")
	}

	t.Logf("Removing ManagedByNVSentinel label from node %s", state.NodeName)

	if err := RemoveNodeManagedByNVSentinelLabel(ctx, client, state.NodeName); err != nil {
		t.Logf("Warning: failed to remove label: %v", err)
	}
}

// removeNodeConditions patches the node status to remove the specified
// condition types, preventing cross-test contamination from stale
// conditions that the NIC monitor can no longer clear (because the fake
// sysfs and config have been restored to defaults).
func removeNodeConditions(ctx context.Context, t *testing.T,
	client klient.Client, nodeName string, conditionTypes ...string,
) {
	t.Helper()

	exclude := make(map[string]bool, len(conditionTypes))
	for _, ct := range conditionTypes {
		exclude[ct] = true
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, getErr := GetNodeByName(ctx, client, nodeName)
		if getErr != nil {
			return getErr
		}

		filtered := make([]corev1.NodeCondition, 0, len(node.Status.Conditions))
		removed := 0

		for _, c := range node.Status.Conditions {
			if exclude[string(c.Type)] {
				removed++
				continue
			}

			filtered = append(filtered, c)
		}

		if removed == 0 {
			return nil
		}

		node.Status.Conditions = filtered

		return client.Resources().UpdateStatus(ctx, node)
	})
	if err != nil {
		t.Logf("Warning: failed to remove node conditions: %v", err)
	}
}

// CreateFakeSysfsTree creates the full fake sysfs tree on the target node.
// 8 IB PFs + 1 Ethernet PF + 2 VFs, with port state files and boot_id.
func CreateFakeSysfsTree(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName string,
) {
	t.Helper()

	script := `
set -e
FAKE="/host/fake-ib"
NET="/host/fake-net"
PROC="/host/fake-proc"
rm -rf "$FAKE" "$NET" "$PROC"

create_pf() {
  dev="$1"; numa="$2"; pci="$3"; ll="$4"; hca="$5"; iface="$6"
  mkdir -p "$FAKE/$dev/ports/1" "$FAKE/$dev/device/net"
  echo "4: ACTIVE"  > "$FAKE/$dev/ports/1/state"
  echo "5: LinkUp"  > "$FAKE/$dev/ports/1/phys_state"
  echo "$ll"        > "$FAKE/$dev/ports/1/link_layer"
  echo "400 Gb/sec (4X HDR)" > "$FAKE/$dev/ports/1/rate"
  echo "$hca"       > "$FAKE/$dev/hca_type"
  echo "MT_TEST_001"> "$FAKE/$dev/board_id"
  echo "28.40.1000" > "$FAKE/$dev/fw_ver"
  echo "$numa"      > "$FAKE/$dev/device/numa_node"
  echo "PCI_SLOT_NAME=$pci" > "$FAKE/$dev/device/uevent"
  echo "0x15b3"     > "$FAKE/$dev/device/vendor"
  if [ -n "$iface" ]; then mkdir -p "$FAKE/$dev/device/net/$iface"; fi
}

# 8 IB compute NICs modeled after DGX A100 (ConnectX-6 HDR).
# GPU NUMAs: 3,3,1,1,7,7,5,5 — NICs paired on each GPU NUMA.
create_pf mlx5_0 3 0000:12:00.0 InfiniBand MT4123
create_pf mlx5_1 3 0000:12:00.1 InfiniBand MT4123
create_pf mlx5_2 1 0000:51:00.0 InfiniBand MT4123
create_pf mlx5_3 1 0000:51:00.1 InfiniBand MT4123
create_pf mlx5_4 7 0000:ca:00.0 InfiniBand MT4123
create_pf mlx5_5 7 0000:ca:00.1 InfiniBand MT4123
create_pf mlx5_6 5 0000:a1:00.0 InfiniBand MT4123
create_pf mlx5_7 5 0000:a1:00.1 InfiniBand MT4123

# Ethernet NIC on NUMA 3 (same socket as GPU0,1), carries default route via eth0
create_pf mlx5_8 3 0000:14:00.0 Ethernet MT4123 eth0

# 2 SR-IOV VFs (device/physfn symlink marks them as VFs)
for vf in mlx5_9 mlx5_10; do
  mkdir -p "$FAKE/$vf/ports/1" "$FAKE/$vf/device"
  echo "4: ACTIVE" > "$FAKE/$vf/ports/1/state"
  echo "5: LinkUp" > "$FAKE/$vf/ports/1/phys_state"
  echo "InfiniBand"> "$FAKE/$vf/ports/1/link_layer"
  echo "3"         > "$FAKE/$vf/device/numa_node"
  echo "PCI_SLOT_NAME=0000:12:00.${vf#mlx5_}" > "$FAKE/$vf/device/uevent"
  ln -s dummy "$FAKE/$vf/device/physfn"
done

# /sys/class/net: do NOT create an eth0 entry here. The KIND node's
# real /proc/net/route has eth0 as the default route; if we link
# eth0 → mlx5_8 in fake-net, the classifier would exclude mlx5_8 as
# management, leaving zero Ethernet devices for EthernetStateCheck.

mkdir -p "$PROC/sys/kernel/random"
echo "test-boot-id-00000000-0000-0000-0000-000000000001" > "$PROC/sys/kernel/random/boot_id"
echo "Fake sysfs tree created"
`
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-creator", script)
}

// MutateSysfsState writes new state and phys_state values for a single port.
func MutateSysfsState(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, device, port, state, physState string,
) {
	t.Helper()

	script := fmt.Sprintf(`
set -e
DIR="/host/fake-ib/%s/ports/%s"
echo '%s' > "$DIR/state"
echo '%s' > "$DIR/phys_state"
`, device, port, state, physState)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-mutator", script)
}

// DeleteSysfsDevice removes a single device directory from the fake sysfs tree.
func DeleteSysfsDevice(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, device string,
) {
	t.Helper()

	script := fmt.Sprintf(`rm -rf "/host/fake-ib/%s"`, device)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-sysfs-deleter", script)
}

// CleanupFakeSysfs removes the entire fake sysfs tree from the node.
func CleanupFakeSysfs(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName string,
) {
	t.Helper()
	runShellPodOnNode(t, ctx, client, namespace, nodeName,
		"nic-sysfs-cleanup", `rm -rf /host/fake-ib /host/fake-net /host/fake-proc`)
}

// MutateBootID writes a new boot_id value to simulate a host reboot.
func MutateBootID(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, bootID string,
) {
	t.Helper()

	script := fmt.Sprintf(
		`echo '%s' > /host/fake-proc/sys/kernel/random/boot_id`, bootID)
	runShellPodOnNode(t, ctx, client, namespace, nodeName, "nic-bootid-mutator", script)
}

// runShellPodOnNode creates an ephemeral busybox pod on the target node,
// runs the given shell script with a hostPath mount, waits for completion,
// and cleans up. This replaces the YAML-manifest-based approach with
// fully programmatic pod construction.
func runShellPodOnNode(t *testing.T, ctx context.Context,
	client klient.Client, namespace, nodeName, namePrefix, script string,
) {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: namePrefix + "-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeName:      nodeName,
			Containers: []corev1.Container{{
				Name:    "worker",
				Image:   busyboxImage,
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "host-run",
					MountPath: "/host",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "host-run",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: fakeSysfsHostBase,
						Type: hostPathType(corev1.HostPathDirectoryOrCreate),
					},
				},
			}},
		},
	}

	err := client.Resources().Create(ctx, pod)
	require.NoError(t, err, "failed to create %s pod", namePrefix)

	podName := pod.Name

	defer func() {
		_ = client.Resources().Delete(ctx, pod)
	}()

	require.Eventually(t, func() bool {
		var current corev1.Pod
		if getErr := client.Resources().Get(ctx, podName, namespace, &current); getErr != nil {
			t.Logf("failed to check %s pod %s: %v", namePrefix, podName, getErr)
			return false
		}

		if current.Status.Phase == corev1.PodSucceeded {
			t.Logf("%s pod %s completed", namePrefix, podName)
			return true
		}

		if current.Status.Phase == corev1.PodFailed {
			t.Errorf("%s pod %s failed", namePrefix, podName)
			return false
		}

		return false
	}, EventuallyWaitTimeout, WaitInterval, "%s pod should complete", namePrefix)
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

// updateNICMonitorForFakeSysfs patches the DaemonSet args to point the
// NIC monitor at the fake sysfs paths. Returns the original args.
func updateNICMonitorForFakeSysfs(t *testing.T, ctx context.Context,
	client klient.Client,
) []string {
	t.Helper()

	updateConfigMapSysfsPaths(t, ctx, client)

	args := map[string]string{
		"--state-file":   "/var/run/nic_health_monitor/state.json",
		"--boot-id-path": FakeBootIDPath,
		"--checks":       "InfiniBandStateCheck,EthernetStateCheck",
	}

	originalArgs, err := UpdateDaemonSetArgs(ctx, t, client,
		NICHealthMonitorDaemonSetName, NICHealthMonitorContainerName, args)
	require.NoError(t, err, "failed to update NIC health monitor DaemonSet args")

	return originalArgs
}

// updateConfigMapSysfsPaths updates the NIC monitor ConfigMap to use
// fake sysfs paths instead of the real /nvsentinel/sys/... mounts.
func updateConfigMapSysfsPaths(t *testing.T, ctx context.Context, client klient.Client) {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if getErr := client.Resources().Get(ctx, NICHealthMonitorConfigMapName,
			NVSentinelNamespace, cm); getErr != nil {
			return getErr
		}

		configTOML := cm.Data["config.toml"]
		if configTOML == "" {
			t.Fatal("NIC health monitor ConfigMap missing config.toml key")
		}

		cm.Data["config.toml"] = replaceConfigPaths(configTOML)

		return client.Resources().Update(ctx, cm)
	})
	require.NoError(t, err, "failed to update NIC health monitor ConfigMap")
	t.Logf("Updated NIC monitor ConfigMap with fake sysfs paths: ib=%s net=%s",
		FakeSysfsIBPath, FakeSysfsNetPath)
}

func replaceConfigPaths(configTOML string) string {
	result := ""

	for _, line := range splitLines(configTOML) {
		switch {
		case containsKey(line, "sysClassInfinibandPath"):
			result += "sysClassInfinibandPath = " + quote(FakeSysfsIBPath) + "\n"
		case containsKey(line, "sysClassNetPath"):
			result += "sysClassNetPath = " + quote(FakeSysfsNetPath) + "\n"
		default:
			result += line + "\n"
		}
	}

	return result
}

func splitLines(s string) []string {
	var lines []string

	start := 0

	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}

	if start < len(s) {
		lines = append(lines, s[start:])
	}

	return lines
}

func containsKey(line, key string) bool {
	for i := range line {
		if line[i] != ' ' && line[i] != '\t' {
			rest := line[i:]
			return len(rest) > len(key) && rest[:len(key)] == key
		}
	}

	return false
}

func quote(s string) string { return "\"" + s + "\"" }

// RestartNICMonitorPod deletes the current pod and waits for the
// replacement to become ready on the same node. Returns the new pod.
func RestartNICMonitorPod(t *testing.T, ctx context.Context,
	client klient.Client, namespace, podName, nodeName string,
) *corev1.Pod {
	t.Helper()

	err := DeletePod(ctx, t, client, namespace, podName, true)
	require.NoError(t, err, "failed to delete NIC monitor pod %s", podName)

	newPod, err := GetDaemonSetPodOnWorkerNode(ctx, t, client,
		NICHealthMonitorDaemonSetName, "nic-health-monitor", nodeName)
	require.NoError(t, err, "failed to get restarted NIC health monitor pod")
	t.Logf("Restarted NIC monitor: new pod %s on node %s", newPod.Name, nodeName)

	return newPod
}

// WaitForNICConditionHealthy waits for the given check name condition to
// show ConditionFalse (healthy) on the node.
func WaitForNICConditionHealthy(ctx context.Context, t *testing.T,
	client klient.Client, nodeName, checkName string,
) {
	t.Helper()
	WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
		checkName, "", "", corev1.ConditionFalse)
}

// WaitForNICConditionUnhealthy waits for the given check name condition
// to show ConditionTrue with a matching message substring.
func WaitForNICConditionUnhealthy(ctx context.Context, t *testing.T,
	client klient.Client, nodeName, checkName, expectedMessage string,
) {
	t.Helper()
	WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
		checkName, expectedMessage, "", corev1.ConditionTrue)
}

// NICMonitorConfigMapSnapshot stores the original ConfigMap data
// for restoration during teardown.
type NICMonitorConfigMapSnapshot struct {
	Data map[string]string
}

// BackupNICConfigMap captures the current ConfigMap data.
func BackupNICConfigMap(t *testing.T, ctx context.Context, client klient.Client) *NICMonitorConfigMapSnapshot {
	t.Helper()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NICHealthMonitorConfigMapName,
			Namespace: NVSentinelNamespace,
		},
	}
	err := client.Resources().Get(ctx, NICHealthMonitorConfigMapName, NVSentinelNamespace, cm)
	require.NoError(t, err, "failed to get NIC health monitor ConfigMap")

	dataCopy := make(map[string]string, len(cm.Data))
	for k, v := range cm.Data {
		dataCopy[k] = v
	}

	return &NICMonitorConfigMapSnapshot{Data: dataCopy}
}

// RestoreNICConfigMap restores the ConfigMap to its backed-up state.
func RestoreNICConfigMap(t *testing.T, ctx context.Context,
	client klient.Client, snapshot *NICMonitorConfigMapSnapshot,
) {
	t.Helper()

	if snapshot == nil {
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		if getErr := client.Resources().Get(ctx, NICHealthMonitorConfigMapName,
			NVSentinelNamespace, cm); getErr != nil {
			return getErr
		}

		cm.Data = snapshot.Data

		return client.Resources().Update(ctx, cm)
	})
	require.NoError(t, err, "failed to restore NIC health monitor ConfigMap")
	t.Log("Restored NIC health monitor ConfigMap to original state")
}
