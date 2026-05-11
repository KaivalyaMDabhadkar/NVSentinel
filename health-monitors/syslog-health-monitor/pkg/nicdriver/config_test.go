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

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func writeTOML(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))

	return path
}

func TestLoadConfig_ValidPatterns(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "cmd_exec_timeout"
enabled = true
processingStrategy = "EXECUTE_REMEDIATION"

[[nicDriverDetection.patterns]]
name = "pci_power_insufficient"
enabled = true
processingStrategy = "STORE_ONLY"
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 2)

	assert.Equal(t, "cmd_exec_timeout", patterns[0].Name)
	assert.True(t, patterns[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, patterns[0].RecommendedAction)
	assert.True(t, patterns[0].HasProcessingStrategy)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, patterns[0].ProcessingStrategy)
	assert.True(t, patterns[0].Re.MatchString(
		"mlx5_core 0000:03:00.0: wait_func:1195:(pid 1967079): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource"))

	assert.Equal(t, "pci_power_insufficient", patterns[1].Name)
	assert.False(t, patterns[1].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, patterns[1].RecommendedAction)
	assert.True(t, patterns[1].HasProcessingStrategy)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, patterns[1].ProcessingStrategy)
	assert.True(t, patterns[1].Re.MatchString(
		"mlx5_core 0000:12:00.0: mlx5_pcie_event:299: Detected insufficient power on the PCIe slot (27W)."))
}

func TestLoadConfig_DisabledPatternsFiltered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "module_unplugged"
enabled = true

[[nicDriverDetection.patterns]]
name = "unknown_disabled"
enabled = false
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.Equal(t, "module_unplugged", patterns[0].Name)
}

func TestLoadConfig_EmptyPatternsAllowed(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[nicDriverDetection]
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, patterns)
}

func TestLoadConfig_DuplicateNamesRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "cmd_exec_timeout"
enabled = true

[[nicDriverDetection.patterns]]
name = "cmd_exec_timeout"
enabled = true
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate pattern name")
}

func TestLoadConfig_UnknownPatternNameRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "custom_regex"
enabled = true
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestLoadConfig_UnknownProcessingStrategyRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "module_unplugged"
enabled = true
processingStrategy = "TYPO_STRATEGY"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known ProcessingStrategy")
}

func TestLoadConfig_MissingProcessingStrategyAllowed(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "netdev_watchdog"
enabled = true
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.False(t, patterns[0].HasProcessingStrategy)
}

func TestLoadConfig_DefinitionWithApostropheRegex(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "health_poll_failed"
enabled = true
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.True(t, patterns[0].Re.MatchString(
		"mlx5_core 0000:d2:00.0: poll_health:825: device's health compromised - reached miss count."))
}
