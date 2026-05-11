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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validDeltaCounter() CounterConfig {
	return CounterConfig{
		Name:          "link_downed",
		Path:          "counters/link_downed",
		Enabled:       true,
		ThresholdType: "delta",
		Threshold:     0,
		Description:   "Port Training State Machine failed",
	}
}

func validVelocityCounter() CounterConfig {
	return CounterConfig{
		Name:          "symbol_error",
		Path:          "counters/symbol_error",
		Enabled:       true,
		ThresholdType: "velocity",
		VelocityUnit:  "second",
		Threshold:     10.0,
		Description:   "PHY bit errors before FEC",
	}
}

func counterDetection(counters ...CounterConfig) CounterDetectionConfig {
	return CounterDetectionConfig{
		Enabled:  true,
		Counters: counters,
	}
}

func TestValidateCounterDetection_DisabledSkipsValidation(t *testing.T) {
	cd := CounterDetectionConfig{
		Enabled:  false,
		Counters: []CounterConfig{{Name: ""}},
	}
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_DisabledCounterSkipsValidation(t *testing.T) {
	c := validDeltaCounter()
	c.Enabled = false
	c.Name = ""
	cd := counterDetection(c)
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_ValidDeltaCounter(t *testing.T) {
	cd := counterDetection(validDeltaCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_ValidVelocityCounter(t *testing.T) {
	cd := counterDetection(validVelocityCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_AllVelocityUnits(t *testing.T) {
	for _, unit := range []string{"second", "minute", "hour"} {
		c := validVelocityCounter()
		c.VelocityUnit = unit
		cd := counterDetection(c)
		assert.NoError(t, validateCounterDetection(&cd), "velocityUnit %q should be valid", unit)
	}
}

func TestValidateCounter_EmptyName(t *testing.T) {
	c := validDeltaCounter()
	c.Name = ""
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name must not be empty")
}

func TestValidateCounter_EmptyPath(t *testing.T) {
	c := validDeltaCounter()
	c.Path = ""
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path must not be empty")
}

func TestValidateCounter_UnsupportedPath(t *testing.T) {
	c := validDeltaCounter()
	c.Path = "ports/1/counters/link_downed"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path")
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateCounter_UnknownCounterName(t *testing.T) {
	c := validDeltaCounter()
	c.Name = "custom_vendor_error"
	c.Path = "hw_counters/local_ack_timeout_err"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "counter name")
	assert.Contains(t, err.Error(), "not allowed")
}

func TestValidateCounter_NamePathMismatch(t *testing.T) {
	c := validDeltaCounter()
	c.Name = "link_downed"
	c.Path = "counters/symbol_error"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path")
	assert.Contains(t, err.Error(), "allowed path(s)")
}

func TestValidateCounter_PathPrefixOnlyRejected(t *testing.T) {
	for _, prefix := range []string{"counters/", "hw_counters/", "statistics/"} {
		c := validDeltaCounter()
		c.Path = prefix
		err := validateCounter(c)
		require.Error(t, err, "bare prefix %q should be rejected (no counter name)", prefix)
		assert.Contains(t, err.Error(), "path")
	}
}

func TestValidateCounter_AllowedCounterSelections(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"link_downed", "counters/link_downed"},
		{"symbol_error", "counters/symbol_error"},
		{"symbol_error_fatal", "counters/symbol_error"},
		{"port_xmit_wait", "counters/port_xmit_wait"},
		{"rnr_nak_retry_err", "hw_counters/rnr_nak_retry_err"},
		{"req_transport_retries_exceeded", "hw_counters/req_transport_retries_exceeded"},
		{"roce_slow_restart_trans", "hw_counters/roce_slow_restart_trans"},
		{"carrier_changes", "statistics/carrier_changes"},
		{"rx_missed_errors", "statistics/rx_missed_errors"},
		{"tx_carrier_errors", "statistics/tx_carrier_errors"},
	} {
		c := validDeltaCounter()
		c.Name = tc.name
		c.Path = tc.path
		assert.NoError(t, validateCounter(c), "counter %q path %q should be valid", tc.name, tc.path)
	}
}

func TestValidateCounter_InvalidThresholdType(t *testing.T) {
	c := validDeltaCounter()
	c.ThresholdType = "absolute"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "thresholdType")
}

func TestValidateCounter_VelocityMissingUnit(t *testing.T) {
	c := validVelocityCounter()
	c.VelocityUnit = ""
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "velocityUnit")
}

func TestValidateCounter_VelocityInvalidUnit(t *testing.T) {
	c := validVelocityCounter()
	c.VelocityUnit = "day"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "velocityUnit")
}

func TestValidateCounter_EmptyDescription(t *testing.T) {
	c := validDeltaCounter()
	c.Description = ""
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "description")
}

func TestValidateDescription_SemicolonRejected(t *testing.T) {
	err := validateDescription("error; see logs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not contain \";\"")
}

func TestValidateDescription_RecommendedActionMarkerRejected(t *testing.T) {
	err := validateDescription("see Recommended Action=REPLACE_VM for details")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Recommended Action=")
}

func TestValidateDescription_InvalidUTF8Rejected(t *testing.T) {
	err := validateDescription("bad\xff\xfebytes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid UTF-8")
}

func TestValidateDescription_ValidStrings(t *testing.T) {
	valid := []string{
		"Port Training State Machine failed - QP disconnect",
		"PHY bit errors before FEC - physical layer degradation",
		"Link retraining events - micro-flapping",
		"Malformed packets received",
		"ACK timeout - potential fabric black hole",
	}

	for _, desc := range valid {
		assert.NoError(t, validateDescription(desc), "description %q should be valid", desc)
	}
}

func TestValidateCounterDetection_DuplicateNames(t *testing.T) {
	c1 := validDeltaCounter()
	c2 := validDeltaCounter()
	cd := counterDetection(c1, c2)
	err := validateCounterDetection(&cd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate counter name")
}

func TestValidateCounterDetection_DuplicateNamesSkipsDisabled(t *testing.T) {
	c1 := validDeltaCounter()
	c2 := validDeltaCounter()
	c2.Enabled = false
	cd := counterDetection(c1, c2)
	assert.NoError(t, validateCounterDetection(&cd))
}

func TestValidateCounterDetection_MultipleValidCounters(t *testing.T) {
	cd := counterDetection(validDeltaCounter(), validVelocityCounter())
	assert.NoError(t, validateCounterDetection(&cd))
}
