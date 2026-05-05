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

package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

const (
	thresholdTypeDelta    = "delta"
	thresholdTypeVelocity = "velocity"

	velocityUnitSecond = "second"
	velocityUnitMinute = "minute"
	velocityUnitHour   = "hour"

	recommendedActionMarker = "Recommended Action="
)

var supportedCounterPathPrefixes = []string{
	"counters/",
	"hw_counters/",
	"statistics/",
}

// Config represents the NIC Health Monitor configuration loaded from TOML.
type Config struct {
	// NicExclusionRegex contains comma-separated regex patterns for NICs to exclude
	NicExclusionRegex string `toml:"nicExclusionRegex"`

	// NicInclusionRegexOverride, when non-empty, bypasses automatic device discovery
	// and monitors only NIC devices whose names match these comma-separated regex patterns.
	NicInclusionRegexOverride string `toml:"nicInclusionRegexOverride"`

	// SysClassNetPath is the sysfs path for network interfaces (container mount point)
	SysClassNetPath string `toml:"sysClassNetPath"`

	// SysClassInfinibandPath is the sysfs path for InfiniBand devices (container mount point)
	SysClassInfinibandPath string `toml:"sysClassInfinibandPath"`

	// CounterDetection contains counter monitoring configuration
	CounterDetection CounterDetectionConfig `toml:"counterDetection"`
}

// CounterDetectionConfig contains the configuration for counter-based monitoring.
type CounterDetectionConfig struct {
	Enabled  bool            `toml:"enabled"`
	Counters []CounterConfig `toml:"counters"`
}

// CounterConfig defines a single counter to monitor.
type CounterConfig struct {
	Name          string  `toml:"name"`
	Path          string  `toml:"path"`
	Enabled       bool    `toml:"enabled"`
	IsFatal       bool    `toml:"isFatal"`
	ThresholdType string  `toml:"thresholdType"`
	Threshold     float64 `toml:"threshold"`
	VelocityUnit  string  `toml:"velocityUnit,omitempty"`
	Description   string  `toml:"description"`
}

// LoadConfig reads and parses the TOML configuration file.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if err := configmanager.LoadTOMLConfig(path, cfg); err != nil {
		return nil, err
	}

	if cfg.SysClassNetPath == "" {
		cfg.SysClassNetPath = "/nvsentinel/sys/class/net"
	}

	if cfg.SysClassInfinibandPath == "" {
		cfg.SysClassInfinibandPath = "/nvsentinel/sys/class/infiniband"
	}

	if err := validateRegexList(cfg.NicExclusionRegex); err != nil {
		return nil, fmt.Errorf("invalid nicExclusionRegex: %w", err)
	}

	if err := validateRegexList(cfg.NicInclusionRegexOverride); err != nil {
		return nil, fmt.Errorf("invalid nicInclusionRegexOverride: %w", err)
	}

	if err := validateCounterDetection(&cfg.CounterDetection); err != nil {
		return nil, fmt.Errorf("invalid counterDetection: %w", err)
	}

	return cfg, nil
}

func validateRegexList(commaSeparated string) error {
	if commaSeparated == "" {
		return nil
	}

	for _, pat := range strings.Split(commaSeparated, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}

		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
	}

	return nil
}

func validateCounterDetection(cd *CounterDetectionConfig) error {
	if !cd.Enabled {
		return nil
	}

	seen := make(map[string]struct{})

	for i, c := range cd.Counters {
		if !c.Enabled {
			continue
		}

		if err := validateCounter(c); err != nil {
			return fmt.Errorf("counters[%d] (%q): %w", i, c.Name, err)
		}

		if _, exists := seen[c.Name]; exists {
			return fmt.Errorf("counters[%d]: duplicate counter name %q", i, c.Name)
		}

		seen[c.Name] = struct{}{}
	}

	return nil
}

var validVelocityUnits = map[string]struct{}{
	velocityUnitSecond: {},
	velocityUnitMinute: {},
	velocityUnitHour:   {},
}

func validateCounter(c CounterConfig) error {
	if c.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	if c.Path == "" {
		return fmt.Errorf("path must not be empty")
	}

	if err := validateCounterPath(c.Path); err != nil {
		return fmt.Errorf("path: %w", err)
	}

	switch c.ThresholdType {
	case thresholdTypeDelta:
		// velocityUnit is ignored for delta counters
	case thresholdTypeVelocity:
		if _, ok := validVelocityUnits[c.VelocityUnit]; !ok {
			return fmt.Errorf("velocityUnit %q is invalid; must be one of: second, minute, hour", c.VelocityUnit)
		}
	default:
		return fmt.Errorf("thresholdType %q is invalid; must be one of: delta, velocity", c.ThresholdType)
	}

	if err := validateDescription(c.Description); err != nil {
		return fmt.Errorf("description: %w", err)
	}

	return nil
}

func validateCounterPath(path string) error {
	for _, prefix := range supportedCounterPathPrefixes {
		if strings.HasPrefix(path, prefix) && len(path) > len(prefix) {
			return nil
		}
	}

	return fmt.Errorf(
		"%q is invalid; must start with one of: %s",
		path, strings.Join(supportedCounterPathPrefixes, ", "),
	)
}

func validateDescription(desc string) error {
	if desc == "" {
		return fmt.Errorf("must not be empty")
	}

	if !utf8.ValidString(desc) {
		return fmt.Errorf("contains invalid UTF-8")
	}

	if strings.Contains(desc, ";") {
		return fmt.Errorf("must not contain %q (used as message delimiter by platform-connectors)", ";")
	}

	if strings.Contains(desc, recommendedActionMarker) {
		return fmt.Errorf(
			"must not contain %q (used as message parser marker by platform-connectors)",
			recommendedActionMarker,
		)
	}

	return nil
}
