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

// Package statefile manages the NIC Health Monitor's persistent state
// file. The file is a single JSON document storing port snapshots,
// known devices, counter snapshots, and breach flags.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SchemaVersion is the current version of MonitorState. Bump on
	// breaking field removals or type changes. Additive changes (new
	// fields) do not require a bump; readers tolerate unknown fields.
	SchemaVersion = 2

	// DefaultStateFilePath is the on-host location of the state file.
	// It matches the hostPath volume mount in the DaemonSet manifest.
	DefaultStateFilePath = "/var/run/nic_health_monitor/state.json"

	// DefaultBootIDPath is the sysfs node that exposes the kernel's
	// random boot ID. It is read once at startup and used to detect host
	// reboots (see Load). The DaemonSet bind-mounts /proc into
	// /nvsentinel/proc so tests can point at an alternate path.
	DefaultBootIDPath = "/nvsentinel/proc/sys/kernel/random/boot_id"
)

// MonitorState is the on-disk schema. Only fields defined in this
// struct survive a Load/Save cycle; unknown JSON fields are dropped.
type MonitorState struct {
	Version int    `json:"version"`
	BootID  string `json:"boot_id,omitempty"`

	// State detection state — produced by InfiniBandStateCheck and
	// EthernetStateCheck. Keys follow the `<device>_<port>` convention.
	PortStates   map[string]PortStateSnapshot `json:"port_states,omitempty"`
	KnownDevices []string                     `json:"known_devices,omitempty"`

	// Counter detection state — produced by InfiniBandDegradationCheck
	// and EthernetDegradationCheck. Both maps key on
	// `<device>:<port>:<counter_name>` so the IB and Ethernet checks
	// keep distinct entries even when they share a state file.
	CounterSnapshots map[string]CounterSnapshot   `json:"counter_snapshots,omitempty"`
	BreachFlags      map[string]CounterBreachFlag `json:"breach_flags,omitempty"`
}

// PortStateSnapshot captures the last-known state of a port. LinkLayer
// lets each check filter the global map to its own ports (IB vs
// Ethernet) when seeding in-memory previous-state maps.
type PortStateSnapshot struct {
	Device        string `json:"device"`
	Port          int    `json:"port"`
	State         string `json:"state"`
	PhysicalState string `json:"physical_state"`
	LinkLayer     string `json:"link_layer,omitempty"`
}

// CounterSnapshot stores the value and wall-clock timestamp of a counter
// reading. For delta thresholds the snapshot is updated every poll;
// for velocity thresholds it is held for the configured velocityUnit
// window so the rate can be computed over real elapsed time.
type CounterSnapshot struct {
	Value     uint64    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// CounterBreachFlag tracks whether a counter is currently in a breached
// state. Breach is latching: once set, it stays set until the counter
// is reset (admin clear) or the host reboots. The CheckName and IsFatal
// fields preserve the original event's identity so the recovery event
// clears the same condition on the platform.
type CounterBreachFlag struct {
	Breached  bool      `json:"breached"`
	CheckName string    `json:"check_name,omitempty"`
	IsFatal   bool      `json:"is_fatal,omitempty"`
	Since     time.Time `json:"since,omitempty"`
}

// Manager coordinates reads and writes to the shared state file. A
// single Manager instance is shared between all checks; its internal
// mutex keeps concurrent writes from corrupting the on-disk file.
type Manager struct {
	mu         sync.Mutex
	path       string
	bootIDPath string
	state      MonitorState
	loaded     bool

	// bootIDChanged captures the result of the most recent Load call so
	// callers that need to differentiate "fresh node or host reboot"
	// from "pod restart with persisted state" can query it.
	bootIDChanged bool
}

// NewManager constructs a Manager backed by the default on-host paths.
func NewManager() *Manager {
	return NewManagerWithPaths(DefaultStateFilePath, DefaultBootIDPath)
}

// NewManagerWithPaths constructs a Manager with explicit paths, used by
// tests to redirect to tempdir-backed files.
func NewManagerWithPaths(statePath, bootIDPath string) *Manager {
	return &Manager{
		path:       statePath,
		bootIDPath: bootIDPath,
		state:      MonitorState{Version: SchemaVersion},
	}
}

// Path returns the state file path the Manager is configured to write.
func (m *Manager) Path() string {
	return m.path
}

// Paths returns the state file path and boot-ID file path.
func (m *Manager) Paths() (string, string) {
	return m.path, m.bootIDPath
}

// Load reads the persisted state file, compares its boot ID against the
// current kernel boot ID, and seeds the Manager's in-memory state. The
// returned error is non-nil only on I/O or JSON-parse failures that the
// caller should surface; "file missing", "file corrupt", and "boot ID
// changed" are all treated as recoverable conditions that reset the
// state to empty and log a warning.
//
// After Load, BootIDChanged reports whether the persisted state was
// discarded for any of the reasons above. Callers that drive the
// "first poll after boot" healthy-baseline behaviour should consult it
// exactly once at startup.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentBootID, err := m.readBootID()
	if err != nil {
		// Without a boot ID we can't reason about reboots. Start empty
		// and treat every restart as a fresh one (safe direction).
		slog.Warn("Could not read boot ID, treating startup as fresh boot",
			"path", m.bootIDPath, "error", err)

		m.resetStateLocked("")

		return nil
	}

	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("State file does not exist, starting with empty state",
				"path", m.path)
		} else {
			slog.Warn("Could not read state file, starting with empty state",
				"path", m.path, "error", err)
		}

		m.resetStateLocked(currentBootID)

		return nil
	}

	var loaded MonitorState
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("State file is corrupt, discarding contents",
			"path", m.path, "error", err)

		m.resetStateLocked(currentBootID)

		return nil
	}

	if loaded.BootID != currentBootID {
		slog.Info("Boot ID changed, resetting persisted state",
			"previous_boot_id", loaded.BootID,
			"current_boot_id", currentBootID,
		)

		m.resetStateLocked(currentBootID)

		return nil
	}

	if loaded.Version != SchemaVersion {
		slog.Info("Schema version changed, discarding stale state",
			"file_version", loaded.Version,
			"current_version", SchemaVersion,
		)

		m.resetStateLocked(currentBootID)

		return nil
	}

	m.state = loaded
	m.loaded = true
	m.bootIDChanged = false

	slog.Info("Loaded persisted state",
		"path", m.path,
		"known_devices", len(loaded.KnownDevices),
		"port_states", len(loaded.PortStates),
	)

	return nil
}

// BootIDChanged reports whether the most recent Load treated this as a
// fresh boot. Must be called after Load.
func (m *Manager) BootIDChanged() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.bootIDChanged
}

// PortStatesFor returns a copy of persisted port snapshots whose
// LinkLayer matches one of the given layers (case-insensitive). An
// empty layers slice returns every entry. The returned map is safe for
// the caller to mutate.
func (m *Manager) PortStatesFor(layers ...string) map[string]PortStateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]PortStateSnapshot, len(m.state.PortStates))

	for k, v := range m.state.PortStates {
		if !matchesLayer(v.LinkLayer, layers) {
			continue
		}

		out[k] = v
	}

	return out
}

// KnownDevices returns a copy of the persisted KnownDevices list. The
// state checks merge this with the devices they discover live on each
// poll to detect disappearance across pod restarts.
func (m *Manager) KnownDevices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.state.KnownDevices...)
}

// UpdatePortStates merges per-check port state into the shared map,
// replacing any existing entries that match the provided LinkLayer(s).
// Entries with a different LinkLayer (written by the sibling check) are
// preserved. knownDevices is unioned with the persisted list so the
// state checks do not clobber each other's device sets.
// UpdatePortStates merges per-check port state into the shared map,
// replacing any existing entries that match the provided LinkLayer(s).
// Returns true if the state was modified (caller should Save).
func (m *Manager) UpdatePortStates(
	portStates map[string]PortStateSnapshot,
	knownDevices []string,
	layers ...string,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.PortStates == nil {
		m.state.PortStates = make(map[string]PortStateSnapshot, len(portStates))
	}

	if !m.portStatesChanged(portStates, layers) {
		return false
	}

	for k, v := range m.state.PortStates {
		if matchesLayer(v.LinkLayer, layers) {
			delete(m.state.PortStates, k)
		}
	}

	for k, v := range portStates {
		m.state.PortStates[k] = v
	}

	// Rebuild KnownDevices from the current PortStates rather than
	// merging with stale entries. This ensures disappeared devices are
	// removed from the persisted list.
	seen := make(map[string]bool)
	for _, v := range m.state.PortStates {
		seen[v.Device] = true
	}

	devices := make([]string, 0, len(seen))
	for d := range seen {
		devices = append(devices, d)
	}

	sort.Strings(devices)
	m.state.KnownDevices = devices

	return true
}

// portStatesChanged reports whether the incoming port states differ from
// the currently persisted entries for the given link layers.
func (m *Manager) portStatesChanged(
	incoming map[string]PortStateSnapshot, layers []string,
) bool {
	for k, old := range m.state.PortStates {
		if !matchesLayer(old.LinkLayer, layers) {
			continue
		}

		if newSnap, exists := incoming[k]; !exists || old != newSnap {
			return true
		}
	}

	for k := range incoming {
		if _, exists := m.state.PortStates[k]; !exists {
			return true
		}
	}

	return false
}

// CounterSnapshots returns a copy of the persisted counter snapshots.
// Each evaluator seeds its in-memory snapshot map from this on startup
// so that delta and velocity windows survive pod restarts.
func (m *Manager) CounterSnapshots() map[string]CounterSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]CounterSnapshot, len(m.state.CounterSnapshots))
	for k, v := range m.state.CounterSnapshots {
		out[k] = v
	}

	return out
}

// UpdateCounterSnapshots merges the supplied counter snapshots into the
// shared map. The merge iterates only over the incoming map, so callers
// (the IB and Ethernet evaluators) must scope their input to the keys
// they actually own — otherwise one evaluator's loaded-but-not-written
// view of a sibling key could clobber the sibling's update. Returns
// true if any persisted snapshot value or timestamp actually changed;
// callers only Save when true.
func (m *Manager) UpdateCounterSnapshots(snapshots map[string]CounterSnapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.CounterSnapshots == nil {
		m.state.CounterSnapshots = make(map[string]CounterSnapshot, len(snapshots))
	}

	changed := false

	for k, v := range snapshots {
		old, exists := m.state.CounterSnapshots[k]
		if !exists || old.Value != v.Value || !old.Timestamp.Equal(v.Timestamp) {
			m.state.CounterSnapshots[k] = v
			changed = true
		}
	}

	return changed
}

// BreachFlags returns a copy of the persisted breach flags so an
// evaluator can rehydrate its in-memory view on startup.
func (m *Manager) BreachFlags() map[string]CounterBreachFlag {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]CounterBreachFlag, len(m.state.BreachFlags))
	for k, v := range m.state.BreachFlags {
		out[k] = v
	}

	return out
}

// UpdateBreachFlags merges incoming breach flags. Incoming entries with
// Breached=false delete any matching persisted entry (signalling a
// recovered counter), so callers MUST submit such entries explicitly
// rather than just dropping cleared flags from their input map — the
// merge cannot infer a deletion from an absent key. Returns true if
// any entry was added, removed, or had its fields changed.
func (m *Manager) UpdateBreachFlags(flags map[string]CounterBreachFlag) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.BreachFlags == nil {
		m.state.BreachFlags = make(map[string]CounterBreachFlag, len(flags))
	}

	changed := false

	for k, v := range flags {
		old, exists := m.state.BreachFlags[k]
		switch {
		case !v.Breached && exists:
			delete(m.state.BreachFlags, k)

			changed = true
		case v.Breached && (!exists || old != v):
			m.state.BreachFlags[k] = v
			changed = true
		}
	}

	return changed
}

// Save writes the current state to disk atomically (tmp file + rename).
// Errors are returned for the caller to log; the design explicitly
// chooses not to halt monitoring on persistence failures.
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveLocked()
}

// resetStateLocked initialises an empty state with the given boot ID
// and marks the manager as having just transitioned to a fresh boot.
// Callers must hold m.mu.
func (m *Manager) resetStateLocked(bootID string) {
	m.state = MonitorState{
		Version: SchemaVersion,
		BootID:  bootID,
	}
	m.loaded = true
	m.bootIDChanged = true
}

// saveLocked serialises m.state to disk using the atomic-rename pattern:
// write to a sibling .tmp file, fsync it, rename it onto the real path.
// Callers must hold m.mu.
func (m *Manager) saveLocked() error {
	if !m.loaded {
		return fmt.Errorf("state file not loaded; call Load before Save")
	}

	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal monitor state: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", filepath.Dir(m.path), err)
	}

	tmp := m.path + ".tmp"
	if err := writeFileAtomic(tmp, data); err != nil {
		return err
	}

	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, m.path, err)
	}

	return nil
}

// writeFileAtomic writes data to path and fsyncs the file before close,
// so a crash between WriteFile and Rename cannot leave a zero-length
// state file on the next boot.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("fsync %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}

// readBootID reads and trims the contents of the boot ID sysfs file.
func (m *Manager) readBootID() (string, error) {
	data, err := os.ReadFile(m.bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", m.bootIDPath, err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("%s contained empty boot ID", m.bootIDPath)
	}

	return id, nil
}

// matchesLayer reports whether the given port's LinkLayer matches any
// of the filter strings (case-insensitive). An empty filter matches
// everything.
func matchesLayer(portLayer string, layers []string) bool {
	if len(layers) == 0 {
		return true
	}

	lower := strings.ToLower(strings.TrimSpace(portLayer))

	for _, l := range layers {
		if strings.EqualFold(strings.TrimSpace(l), lower) {
			return true
		}
	}

	return false
}
