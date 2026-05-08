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
name = "test_fatal"
regex = '''mlx5_core.*timeout\. Will cause a leak'''
enabled = true
isFatal = true
recommendedAction = "REPLACE_VM"
processingStrategy = "EXECUTE_REMEDIATION"
description = "test fatal pattern"

[[nicDriverDetection.patterns]]
name = "test_nonfatal"
regex = '''Detected insufficient power'''
enabled = true
isFatal = false
recommendedAction = "NONE"
processingStrategy = "STORE_ONLY"
description = "test non-fatal pattern"
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 2)

	assert.Equal(t, "test_fatal", patterns[0].Name)
	assert.True(t, patterns[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, patterns[0].RecommendedAction)
	assert.True(t, patterns[0].HasProcessingStrategy)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, patterns[0].ProcessingStrategy)

	assert.Equal(t, "test_nonfatal", patterns[1].Name)
	assert.False(t, patterns[1].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, patterns[1].RecommendedAction)
	assert.True(t, patterns[1].HasProcessingStrategy)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, patterns[1].ProcessingStrategy)
}

func TestLoadConfig_DisabledPatternsFiltered(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "enabled_one"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "enabled"

[[nicDriverDetection.patterns]]
name = "disabled_one"
regex = "test2"
enabled = false
isFatal = false
recommendedAction = "NONE"
description = "disabled"
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.Equal(t, "enabled_one", patterns[0].Name)
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
name = "dup"
regex = "a"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "first"

[[nicDriverDetection.patterns]]
name = "dup"
regex = "b"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "second"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate pattern name")
}

func TestLoadConfig_InvalidRegexRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "bad_regex"
regex = "[invalid"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "bad regex"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestLoadConfig_UnknownActionRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "bad_action"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "TYPO_ACTION"
description = "unknown action"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known RecommendedAction")
}

func TestLoadConfig_UnknownProcessingStrategyRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "bad_strategy"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "NONE"
processingStrategy = "TYPO_STRATEGY"
description = "unknown strategy"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known ProcessingStrategy")
}

func TestLoadConfig_MissingProcessingStrategyAllowed(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "missing_strategy"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "missing strategy"
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.False(t, patterns[0].HasProcessingStrategy)
}

func TestLoadConfig_EmptyDescriptionRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "no_desc"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = ""
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "description")
}

func TestLoadConfig_SemicolonInDescriptionRejected(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "semi"
regex = "test"
enabled = true
isFatal = false
recommendedAction = "NONE"
description = "bad;desc"
`)

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ";")
}

func TestLoadConfig_ApostropheInRegex(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, `
[[nicDriverDetection.patterns]]
name = "health_poll"
regex = '''mlx5_core.*device's health compromised.*reached miss count'''
enabled = true
isFatal = true
recommendedAction = "REPLACE_VM"
description = "health poll with apostrophe in regex"
`)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	assert.True(t, patterns[0].Re.MatchString(
		"mlx5_core 0000:d2:00.0: poll_health:825: device's health compromised - reached miss count."))
}

func TestResolveAction_CaseInsensitive(t *testing.T) {
	action, err := resolveAction("replace_vm")
	require.NoError(t, err)
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, action)
}
