// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configmanager

import (
	"fmt"
	"math"
	"strconv"
	"testing"
)

func TestReadEnvVars(t *testing.T) {
	t.Setenv("TEST_VAR_1", "value1")
	t.Setenv("TEST_VAR_2", "value2")

	specs := []EnvVarSpec{
		{
			Name: "TEST_VAR_1", // Required by default
		},
		{
			Name:     "TEST_VAR_2",
			Optional: true, // Explicitly optional
		},
		{
			Name:         "TEST_VAR_3",
			Optional:     true, // Optional with default
			DefaultValue: "default",
		},
		{
			Name: "TEST_VAR_4", // Required by default, but missing
		},
	}

	results, errors := ReadEnvVars(specs)

	if len(errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(errors))
	}

	if results["TEST_VAR_1"] != "value1" {
		t.Errorf("expected value1, got %s", results["TEST_VAR_1"])
	}

	if results["TEST_VAR_2"] != "value2" {
		t.Errorf("expected value2, got %s", results["TEST_VAR_2"])
	}

	if results["TEST_VAR_3"] != "default" {
		t.Errorf("expected default, got %s", results["TEST_VAR_3"])
	}

	if _, exists := results["TEST_VAR_4"]; exists {
		t.Error("TEST_VAR_4 should not be in results map when required and missing")
	}
}

func TestReadEnvVarsOptionalWithEmptyDefault(t *testing.T) {
	specs := []EnvVarSpec{
		{
			Name:         "MISSING_OPTIONAL_EMPTY_DEFAULT",
			Optional:     true,
			DefaultValue: "",
		},
		{
			Name:         "MISSING_OPTIONAL_WITH_DEFAULT",
			Optional:     true,
			DefaultValue: "some_value",
		},
	}

	results, errors := ReadEnvVars(specs)

	if len(errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errors))
	}

	if _, exists := results["MISSING_OPTIONAL_EMPTY_DEFAULT"]; exists {
		t.Error("optional var with empty default should not be in results map")
	}

	if val, exists := results["MISSING_OPTIONAL_WITH_DEFAULT"]; !exists || val != "some_value" {
		t.Errorf("optional var with non-empty default should be in results map with value 'some_value', got %v", val)
	}
}

func TestGetEnvVar(t *testing.T) {
	t.Run("required with validation", func(t *testing.T) {
		t.Setenv("TEST_REQUIRED", "42")

		value, err := GetEnvVar("TEST_REQUIRED", nil, func(v int) error {
			if v <= 0 {
				return fmt.Errorf("must be positive")
			}
			return nil
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 42 {
			t.Errorf("expected 42, got %d", value)
		}
	})

	t.Run("missing required returns error", func(t *testing.T) {
		_, err := GetEnvVar[int]("TEST_MISSING_REQUIRED", nil, nil)
		if err == nil {
			t.Error("expected error for missing env var but got none")
		}
	})

	t.Run("with default value", func(t *testing.T) {
		defaultVal := 99
		value, err := GetEnvVar("TEST_WITH_DEFAULT", &defaultVal, nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 99 {
			t.Errorf("expected 99, got %d", value)
		}
	})

	t.Run("with default and validation", func(t *testing.T) {
		t.Setenv("TEST_DEFAULT_VAL", "42")

		defaultVal := 10
		value, err := GetEnvVar("TEST_DEFAULT_VAL", &defaultVal, func(v int) error {
			if v <= 0 {
				return fmt.Errorf("must be positive")
			}
			return nil
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 42 {
			t.Errorf("expected 42, got %d", value)
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		t.Setenv("TEST_VALIDATION_FAIL", "5")

		_, err := GetEnvVar("TEST_VALIDATION_FAIL", nil, func(v int) error {
			if v <= 10 {
				return fmt.Errorf("must be greater than 10")
			}
			return nil
		})
		if err == nil {
			t.Error("expected validation error but got none")
		}
	})

	t.Run("different types work", func(t *testing.T) {
		t.Setenv("TEST_STRING", "hello")
		t.Setenv("TEST_BOOL", "true")
		t.Setenv("TEST_FLOAT", "3.14")

		strVal, err := GetEnvVar[string]("TEST_STRING", nil, nil)
		if err != nil || strVal != "hello" {
			t.Errorf("string test failed: %v, got %s", err, strVal)
		}

		boolVal, err := GetEnvVar[bool]("TEST_BOOL", nil, nil)
		if err != nil || !boolVal {
			t.Errorf("bool test failed: %v, got %v", err, boolVal)
		}

		floatVal, err := GetEnvVar[float64]("TEST_FLOAT", nil, nil)
		if err != nil || floatVal != 3.14 {
			t.Errorf("float test failed: %v, got %f", err, floatVal)
		}
	})
}

func TestGetEnvVarAllSupportedTypes(t *testing.T) {
	t.Run("int type", func(t *testing.T) {
		t.Setenv("TEST_INT", "42")

		value, err := GetEnvVar[int]("TEST_INT", nil, nil)
		if err != nil || value != 42 {
			t.Errorf("int test failed: %v, got %d", err, value)
		}
	})

	t.Run("uint type", func(t *testing.T) {
		t.Setenv("TEST_UINT", "4294967295")

		value, err := GetEnvVar[uint]("TEST_UINT", nil, nil)
		if err != nil || value != 4294967295 {
			t.Errorf("uint test failed: %v, got %d", err, value)
		}
	})

	t.Run("float64 type", func(t *testing.T) {
		t.Setenv("TEST_FLOAT64", "3.14159")

		value, err := GetEnvVar[float64]("TEST_FLOAT64", nil, nil)
		if err != nil || value != 3.14159 {
			t.Errorf("float64 test failed: %v, got %f", err, value)
		}
	})

	t.Run("bool type", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "true")

		value, err := GetEnvVar[bool]("TEST_BOOL", nil, nil)
		if err != nil || !value {
			t.Errorf("bool test failed: %v, got %v", err, value)
		}
	})

	t.Run("string type", func(t *testing.T) {
		t.Setenv("TEST_STRING_TYPE", "hello world")

		value, err := GetEnvVar[string]("TEST_STRING_TYPE", nil, nil)
		if err != nil || value != "hello world" {
			t.Errorf("string test failed: %v, got %s", err, value)
		}
	})
}

func TestParseIntBoundsChecking(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectError bool
	}{
		{
			name:        "valid positive int",
			input:       "42",
			expectError: false,
		},
		{
			name:        "valid negative int",
			input:       "-42",
			expectError: false,
		},
		{
			name:        "max int",
			input:       strconv.FormatInt(int64(math.MaxInt), 10),
			expectError: false,
		},
		{
			name:        "min int",
			input:       strconv.FormatInt(int64(math.MinInt), 10),
			expectError: false,
		},
		{
			name:        "invalid non-numeric string",
			input:       "not-a-number",
			expectError: true,
		},
		{
			name:        "empty string",
			input:       "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseInt(tt.input)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none for input %q", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for input %q: %v", tt.input, err)
				}
				expectedInt, parseErr := strconv.Atoi(tt.input)
				if parseErr != nil {
					t.Fatalf("test setup error: strconv.Atoi failed for valid input %q: %v", tt.input, parseErr)
				}
				if result != expectedInt {
					t.Errorf("expected %d, got %d", expectedInt, result)
				}
			}
		})
	}
}

func TestParseUintBoundsChecking(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectError bool
	}{
		{
			name:        "valid uint",
			input:       "42",
			expectError: false,
		},
		{
			name:        "zero",
			input:       "0",
			expectError: false,
		},
		{
			name:        "max uint",
			input:       strconv.FormatUint(uint64(math.MaxUint), 10),
			expectError: false,
		},
		{
			name:        "negative number",
			input:       "-1",
			expectError: true,
		},
		{
			name:        "invalid non-numeric string",
			input:       "not-a-number",
			expectError: true,
		},
		{
			name:        "empty string",
			input:       "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseUint(tt.input)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none for input %q", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for input %q: %v", tt.input, err)
				}
				expectedUint, parseErr := strconv.ParseUint(tt.input, 10, 64)
				if parseErr != nil {
					t.Fatalf("test setup error: strconv.ParseUint failed for valid input %q: %v", tt.input, parseErr)
				}
				if result != uint(expectedUint) {
					t.Errorf("expected %d, got %d", expectedUint, result)
				}
			}
		})
	}
}

func TestGetEnvVarBoundsValidation(t *testing.T) {
	t.Run("int within valid range", func(t *testing.T) {
		t.Setenv("TEST_VALID_INT", "100")
		value, err := GetEnvVar[int]("TEST_VALID_INT", nil, nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 100 {
			t.Errorf("expected 100, got %d", value)
		}
	})

	t.Run("uint within valid range", func(t *testing.T) {
		t.Setenv("TEST_VALID_UINT", "100")
		value, err := GetEnvVar[uint]("TEST_VALID_UINT", nil, nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 100 {
			t.Errorf("expected 100, got %d", value)
		}
	})

	t.Run("negative uint should fail", func(t *testing.T) {
		t.Setenv("TEST_NEGATIVE_UINT", "-1")
		_, err := GetEnvVar[uint]("TEST_NEGATIVE_UINT", nil, nil)
		if err == nil {
			t.Error("expected error for negative uint but got none")
		}
	})

	t.Run("invalid int string should fail", func(t *testing.T) {
		t.Setenv("TEST_INVALID_INT", "not-a-number")
		_, err := GetEnvVar[int]("TEST_INVALID_INT", nil, nil)
		if err == nil {
			t.Error("expected error for invalid int string but got none")
		}
	})

	t.Run("default value validation failure", func(t *testing.T) {
		invalidDefault := -5
		_, err := GetEnvVar("TEST_MISSING_WITH_INVALID_DEFAULT", &invalidDefault, func(v int) error {
			if v <= 0 {
				return fmt.Errorf("must be positive")
			}
			return nil
		})
		if err == nil {
			t.Error("expected validation error for invalid default value but got none")
		}
	})
}
