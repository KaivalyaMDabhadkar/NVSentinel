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
		Name:              "link_downed",
		Path:              "counters/link_downed",
		Enabled:           true,
		ThresholdType:     "delta",
		Threshold:         0,
		Description:       "Port Training State Machine failed",
		RecommendedAction: "REPLACE_VM",
	}
}

func validVelocityCounter() CounterConfig {
	return CounterConfig{
		Name:              "symbol_error",
		Path:              "counters/symbol_error",
		Enabled:           true,
		ThresholdType:     "velocity",
		VelocityUnit:      "second",
		Threshold:         10.0,
		Description:       "PHY bit errors before FEC",
		RecommendedAction: "NONE",
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

func TestValidateCounterDetection_AllRecommendedActions(t *testing.T) {
	for _, action := range []string{"NONE", "REPLACE_VM"} {
		c := validDeltaCounter()
		c.RecommendedAction = action
		cd := counterDetection(c)
		assert.NoError(t, validateCounterDetection(&cd), "recommendedAction %q should be valid", action)
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

func TestValidateCounter_InvalidRecommendedAction(t *testing.T) {
	c := validDeltaCounter()
	c.RecommendedAction = "REPLACE_vm"
	err := validateCounter(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recommendedAction")
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
