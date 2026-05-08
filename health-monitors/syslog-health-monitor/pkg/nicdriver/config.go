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
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const recommendedActionMarker = "Recommended Action="

// Config is the top-level TOML structure for NIC driver syslog patterns.
type Config struct {
	NICDriverDetection PatternDetectionConfig `toml:"nicDriverDetection"`
}

// PatternDetectionConfig wraps the configurable pattern list.
type PatternDetectionConfig struct {
	Patterns []PatternConfig `toml:"patterns"`
}

// PatternConfig defines a single kernel log pattern to match.
type PatternConfig struct {
	Name               string `toml:"name"`
	Regex              string `toml:"regex"`
	Enabled            bool   `toml:"enabled"`
	IsFatal            bool   `toml:"isFatal"`
	RecommendedAction  string `toml:"recommendedAction"`
	ProcessingStrategy string `toml:"processingStrategy"`
	Description        string `toml:"description"`
}

// CompiledPattern is a validated, ready-to-evaluate pattern produced by LoadConfig.
type CompiledPattern struct {
	Name                  string
	Re                    *regexp.Regexp
	IsFatal               bool
	RecommendedAction     pb.RecommendedAction
	ProcessingStrategy    pb.ProcessingStrategy
	HasProcessingStrategy bool
	Description           string
}

// LoadConfig reads and validates the TOML configuration, returning only the
// enabled patterns with pre-compiled regexes and resolved proto enums.
func LoadConfig(path string) ([]CompiledPattern, error) {
	cfg := &Config{}
	if err := configmanager.LoadTOMLConfig(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to load NIC driver config: %w", err)
	}

	return compilePatterns(cfg.NICDriverDetection.Patterns)
}

func compilePatterns(raw []PatternConfig) ([]CompiledPattern, error) {
	seen := make(map[string]struct{})

	var compiled []CompiledPattern

	for i, p := range raw {
		if !p.Enabled {
			continue
		}

		if err := validatePattern(p); err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): %w", i, p.Name, err)
		}

		if _, exists := seen[p.Name]; exists {
			return nil, fmt.Errorf("patterns[%d]: duplicate pattern name %q", i, p.Name)
		}

		seen[p.Name] = struct{}{}

		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): invalid regex: %w", i, p.Name, err)
		}

		action, err := resolveAction(p.RecommendedAction)
		if err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): %w", i, p.Name, err)
		}

		processingStrategy, hasProcessingStrategy, err := resolveProcessingStrategy(p.ProcessingStrategy)
		if err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): %w", i, p.Name, err)
		}

		compiled = append(compiled, CompiledPattern{
			Name:                  p.Name,
			Re:                    re,
			IsFatal:               p.IsFatal,
			RecommendedAction:     action,
			ProcessingStrategy:    processingStrategy,
			HasProcessingStrategy: hasProcessingStrategy,
			Description:           p.Description,
		})
	}

	return compiled, nil
}

func validatePattern(p PatternConfig) error {
	if p.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	if p.Regex == "" {
		return fmt.Errorf("regex must not be empty")
	}

	if err := validateDescription(p.Description); err != nil {
		return fmt.Errorf("description: %w", err)
	}

	return nil
}

// resolveAction is intentionally strict: configuration typos should fail
// startup instead of being coerced to a different remediation action.
func resolveAction(action string) (pb.RecommendedAction, error) {
	upper := strings.ToUpper(strings.TrimSpace(action))

	val, ok := pb.RecommendedAction_value[upper]
	if !ok {
		return 0, fmt.Errorf("recommendedAction %q is not a known RecommendedAction enum value", action)
	}

	return pb.RecommendedAction(val), nil
}

func resolveProcessingStrategy(strategy string) (pb.ProcessingStrategy, bool, error) {
	if strings.TrimSpace(strategy) == "" {
		return 0, false, nil
	}

	upper := strings.ToUpper(strings.TrimSpace(strategy))

	val, ok := pb.ProcessingStrategy_value[upper]
	if !ok {
		return 0, false, fmt.Errorf("processingStrategy %q is not a known ProcessingStrategy enum value", strategy)
	}

	return pb.ProcessingStrategy(val), true, nil
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
