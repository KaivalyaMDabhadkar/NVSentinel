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

// Package nicdriver implements syslog-based detection of mlx5_core NIC
// driver and firmware errors. It loads configurable TOML patterns, matches
// them against kernel log lines, and emits structured HealthEvents with
// optional NIC entity enrichment via sysfs BDF resolution.
package nicdriver

import (
	"os"
	"path/filepath"
)

// Resolver maps a PCI BDF address to its driver name and Linux NIC identifier.
type Resolver interface {
	// Resolve returns the driver name (e.g. "mlx5_core"), the InfiniBand
	// device or net interface name (e.g. "mlx5_0" or "eth0"), and whether
	// the lookup succeeded.
	// A missing or broken driver symlink returns ok=false.
	Resolve(bdf string) (driver string, device string, ok bool)
}

// NewSysfsResolver returns a Resolver backed by a real sysfs tree.
// sysfsRoot is typically "/nvsentinel/sys" in containerized deployments.
func NewSysfsResolver(sysfsRoot string) Resolver {
	return &sysfsResolver{root: sysfsRoot}
}

type sysfsResolver struct {
	root string
}

func (r *sysfsResolver) Resolve(bdf string) (string, string, bool) {
	deviceDir := filepath.Join(r.root, "bus", "pci", "devices", bdf)

	driverName, ok := readDriverName(filepath.Join(deviceDir, "driver"))
	if !ok {
		return "", "", false
	}

	device := resolveDeviceName(deviceDir)

	return driverName, device, true
}

// readDriverName resolves the "driver" symlink and returns the basename
// (e.g. "mlx5_core"). Returns ok=false if the symlink is missing or broken.
func readDriverName(driverSymlink string) (string, bool) {
	target, err := os.Readlink(driverSymlink)
	if err != nil {
		return "", false
	}

	return filepath.Base(target), true
}

// resolveDeviceName attempts to find the Linux NIC identifier for the BDF.
// Precedence: infiniband/ dir → net/ dir → empty string.
func resolveDeviceName(deviceDir string) string {
	if name := firstEntry(filepath.Join(deviceDir, "infiniband")); name != "" {
		return name
	}

	if name := firstEntry(filepath.Join(deviceDir, "net")); name != "" {
		return name
	}

	return ""
}

func firstEntry(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return ""
	}

	return entries[0].Name()
}
