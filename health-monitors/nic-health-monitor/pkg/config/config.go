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
	"sort"
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

var allowedCounterPathsByName = map[string][]string{
	// /sys/class/infiniband/<dev>/ports/<port>/counters/
	"excessive_buffer_overrun_errors": {"counters/excessive_buffer_overrun_errors"},
	"link_downed":                     {"counters/link_downed"},
	"link_error_recovery":             {"counters/link_error_recovery"},
	"local_link_integrity_errors":     {"counters/local_link_integrity_errors"},
	"port_rcv_constraint_errors":      {"counters/port_rcv_constraint_errors"},
	"port_rcv_discards":               {"counters/port_rcv_discards"},
	"port_rcv_errors":                 {"counters/port_rcv_errors"},
	"port_rcv_remote_physical_errors": {"counters/port_rcv_remote_physical_errors"},
	"port_rcv_switch_relay_errors":    {"counters/port_rcv_switch_relay_errors"},
	"port_xmit_constraint_errors":     {"counters/port_xmit_constraint_errors"},
	"port_xmit_discards":              {"counters/port_xmit_discards"},
	"port_xmit_wait":                  {"counters/port_xmit_wait"},
	"symbol_error":                    {"counters/symbol_error"},
	"symbol_error_fatal":              {"counters/symbol_error"},

	// /sys/class/infiniband/<dev>/ports/<port>/hw_counters/
	"duplicate_request":              {"hw_counters/duplicate_request"},
	"implied_nak_seq_err":            {"hw_counters/implied_nak_seq_err"},
	"local_ack_timeout_err":          {"hw_counters/local_ack_timeout_err"},
	"out_of_sequence":                {"hw_counters/out_of_sequence"},
	"packet_seq_err":                 {"hw_counters/packet_seq_err"},
	"req_cqe_error":                  {"hw_counters/req_cqe_error"},
	"req_cqe_flush_error":            {"hw_counters/req_cqe_flush_error"},
	"req_remote_access_errors":       {"hw_counters/req_remote_access_errors"},
	"req_remote_invalid_request":     {"hw_counters/req_remote_invalid_request"},
	"req_rnr_retries_exceeded":       {"hw_counters/req_rnr_retries_exceeded"},
	"req_transport_retries_exceeded": {"hw_counters/req_transport_retries_exceeded"},
	"resp_cqe_error":                 {"hw_counters/resp_cqe_error"},
	"resp_cqe_flush_error":           {"hw_counters/resp_cqe_flush_error"},
	"resp_local_length_error":        {"hw_counters/resp_local_length_error"},
	"resp_remote_access_errors":      {"hw_counters/resp_remote_access_errors"},
	"rnr_nak_retry_err":              {"hw_counters/rnr_nak_retry_err"},
	"roce_adp_retrans":               {"hw_counters/roce_adp_retrans"},
	"roce_adp_retrans_to":            {"hw_counters/roce_adp_retrans_to"},
	"roce_slow_restart":              {"hw_counters/roce_slow_restart"},
	"roce_slow_restart_cnps":         {"hw_counters/roce_slow_restart_cnps"},
	"roce_slow_restart_trans":        {"hw_counters/roce_slow_restart_trans"},
	"rp_cnp_handled":                 {"hw_counters/rp_cnp_handled"},
	"rp_cnp_ignored":                 {"hw_counters/rp_cnp_ignored"},
	"np_cnp_sent":                    {"hw_counters/np_cnp_sent"},
	"np_ecn_marked_roce_packets":     {"hw_counters/np_ecn_marked_roce_packets"},

	// /sys/class/net/<iface>/statistics/
	"carrier_changes":     {"statistics/carrier_changes"},
	"collisions":          {"statistics/collisions"},
	"rx_crc_errors":       {"statistics/rx_crc_errors"},
	"rx_dropped":          {"statistics/rx_dropped"},
	"rx_errors":           {"statistics/rx_errors"},
	"rx_fifo_errors":      {"statistics/rx_fifo_errors"},
	"rx_frame_errors":     {"statistics/rx_frame_errors"},
	"rx_length_errors":    {"statistics/rx_length_errors"},
	"rx_missed_errors":    {"statistics/rx_missed_errors"},
	"rx_nohandler":        {"statistics/rx_nohandler"},
	"rx_over_errors":      {"statistics/rx_over_errors"},
	"tx_aborted_errors":   {"statistics/tx_aborted_errors"},
	"tx_carrier_errors":   {"statistics/tx_carrier_errors"},
	"tx_dropped":          {"statistics/tx_dropped"},
	"tx_errors":           {"statistics/tx_errors"},
	"tx_fifo_errors":      {"statistics/tx_fifo_errors"},
	"tx_heartbeat_errors": {"statistics/tx_heartbeat_errors"},
	"tx_window_errors":    {"statistics/tx_window_errors"},
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

	if err := validateCounterSelection(c.Name, c.Path); err != nil {
		return err
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

func validateCounterSelection(name, path string) error {
	allowedPaths, ok := allowedCounterPathsByName[name]
	if !ok {
		return fmt.Errorf(
			"counter name %q is not allowed; allowed counters: %s",
			name, strings.Join(allowedCounterNames(), ", "),
		)
	}

	if containsString(allowedPaths, path) {
		return nil
	}

	return fmt.Errorf(
		"path %q is not allowed for counter %q; allowed path(s): %s",
		path, name, strings.Join(sortedStrings(allowedPaths), ", "),
	)
}

func allowedCounterNames() []string {
	names := make([]string, 0, len(allowedCounterPathsByName))
	for name := range allowedCounterPathsByName {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)

	return out
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
