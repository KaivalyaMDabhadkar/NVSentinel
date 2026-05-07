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

package nicdriver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupSysfs(t *testing.T, root, bdf, driver string, ibDevices, netDevices []string) {
	t.Helper()

	deviceDir := filepath.Join(root, "bus", "pci", "devices", bdf)
	require.NoError(t, os.MkdirAll(deviceDir, 0755))

	if driver != "" {
		driverTarget := filepath.Join(root, "drivers", driver)
		require.NoError(t, os.MkdirAll(driverTarget, 0755))
		require.NoError(t, os.Symlink(driverTarget, filepath.Join(deviceDir, "driver")))
	}

	if len(ibDevices) > 0 {
		ibDir := filepath.Join(deviceDir, "infiniband")
		require.NoError(t, os.MkdirAll(ibDir, 0755))

		for _, dev := range ibDevices {
			require.NoError(t, os.MkdirAll(filepath.Join(ibDir, dev), 0755))
		}
	}

	if len(netDevices) > 0 {
		netDir := filepath.Join(deviceDir, "net")
		require.NoError(t, os.MkdirAll(netDir, 0755))

		for _, dev := range netDevices {
			require.NoError(t, os.MkdirAll(filepath.Join(netDir, dev), 0755))
		}
	}
}

func TestSysfsResolver_Mlx5WithIB(t *testing.T) {
	root := t.TempDir()
	setupSysfs(t, root, "0000:3b:00.0", "mlx5_core", []string{"mlx5_0"}, nil)

	r := NewSysfsResolver(root)
	driver, device, ok := r.Resolve("0000:3b:00.0")

	assert.True(t, ok)
	assert.Equal(t, "mlx5_core", driver)
	assert.Equal(t, "mlx5_0", device)
}

func TestSysfsResolver_Mlx5WithNet(t *testing.T) {
	root := t.TempDir()
	setupSysfs(t, root, "0000:3b:00.0", "mlx5_core", nil, []string{"eth0"})

	r := NewSysfsResolver(root)
	driver, device, ok := r.Resolve("0000:3b:00.0")

	assert.True(t, ok)
	assert.Equal(t, "mlx5_core", driver)
	assert.Equal(t, "eth0", device)
}

func TestSysfsResolver_Mlx5WithBothPreferIB(t *testing.T) {
	root := t.TempDir()
	setupSysfs(t, root, "0000:3b:00.0", "mlx5_core", []string{"mlx5_2"}, []string{"ib0"})

	r := NewSysfsResolver(root)
	_, device, ok := r.Resolve("0000:3b:00.0")

	assert.True(t, ok)
	assert.Equal(t, "mlx5_2", device)
}

func TestSysfsResolver_NvidiaGPU(t *testing.T) {
	root := t.TempDir()
	setupSysfs(t, root, "0000:b3:00.0", "nvidia", nil, nil)

	r := NewSysfsResolver(root)
	driver, _, ok := r.Resolve("0000:b3:00.0")

	assert.True(t, ok)
	assert.Equal(t, "nvidia", driver)
}

func TestSysfsResolver_MissingDriverSymlink(t *testing.T) {
	root := t.TempDir()
	deviceDir := filepath.Join(root, "bus", "pci", "devices", "0000:00:00.0")
	require.NoError(t, os.MkdirAll(deviceDir, 0755))

	r := NewSysfsResolver(root)
	_, _, ok := r.Resolve("0000:00:00.0")

	assert.False(t, ok)
}

func TestSysfsResolver_NonExistentBDF(t *testing.T) {
	root := t.TempDir()

	r := NewSysfsResolver(root)
	_, _, ok := r.Resolve("0000:ff:ff.f")

	assert.False(t, ok)
}

func TestSysfsResolver_Mlx5NoSubdevices(t *testing.T) {
	root := t.TempDir()
	setupSysfs(t, root, "0000:3b:00.0", "mlx5_core", nil, nil)

	r := NewSysfsResolver(root)
	driver, device, ok := r.Resolve("0000:3b:00.0")

	assert.True(t, ok)
	assert.Equal(t, "mlx5_core", driver)
	assert.Empty(t, device)
}
