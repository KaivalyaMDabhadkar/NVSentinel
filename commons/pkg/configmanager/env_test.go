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
	"os"
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
}

func TestGetEnvVar(t *testing.T) {
	t.Run("required with validation", func(t *testing.T) {
		t.Setenv("TEST_REQUIRED", "42")

		value, err := GetEnvVar[int]("TEST_REQUIRED", func(v int) error {
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
		_, err := GetEnvVar[int]("TEST_MISSING_REQUIRED")
		if err == nil {
			t.Error("expected error for missing env var but got none")
		}
	})

	t.Run("with default value", func(t *testing.T) {
		value, err := GetEnvVar[int]("TEST_WITH_DEFAULT", 99)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if value != 99 {
			t.Errorf("expected 99, got %d", value)
		}
	})

	t.Run("with default and validation", func(t *testing.T) {
		t.Setenv("TEST_DEFAULT_VAL", "42")

		value, err := GetEnvVar[int]("TEST_DEFAULT_VAL", 10, func(v int) error {
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

		_, err := GetEnvVar[int]("TEST_VALIDATION_FAIL", func(v int) error {
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

		strVal, err := GetEnvVar[string]("TEST_STRING")
		if err != nil || strVal != "hello" {
			t.Errorf("string test failed: %v, got %s", err, strVal)
		}

		boolVal, err := GetEnvVar[bool]("TEST_BOOL")
		if err != nil || !boolVal {
			t.Errorf("bool test failed: %v, got %v", err, boolVal)
		}

		floatVal, err := GetEnvVar[float64]("TEST_FLOAT")
		if err != nil || floatVal != 3.14 {
			t.Errorf("float test failed: %v, got %f", err, floatVal)
		}
	})
}

func TestGetEnvVarAllSupportedTypes(t *testing.T) {
	t.Run("int type", func(t *testing.T) {
		t.Setenv("TEST_INT", "42")

		value, err := GetEnvVar[int]("TEST_INT")
		if err != nil || value != 42 {
			t.Errorf("int test failed: %v, got %d", err, value)
		}
	})

	t.Run("uint type", func(t *testing.T) {
		t.Setenv("TEST_UINT", "4294967295")

		value, err := GetEnvVar[uint]("TEST_UINT")
		if err != nil || value != 4294967295 {
			t.Errorf("uint test failed: %v, got %d", err, value)
		}
	})

	t.Run("float64 type", func(t *testing.T) {
		t.Setenv("TEST_FLOAT64", "3.14159")

		value, err := GetEnvVar[float64]("TEST_FLOAT64")
		if err != nil || value != 3.14159 {
			t.Errorf("float64 test failed: %v, got %f", err, value)
		}
	})

	t.Run("bool type", func(t *testing.T) {
		t.Setenv("TEST_BOOL", "true")

		value, err := GetEnvVar[bool]("TEST_BOOL")
		if err != nil || !value {
			t.Errorf("bool test failed: %v, got %v", err, value)
		}
	})

	t.Run("string type", func(t *testing.T) {
		t.Setenv("TEST_STRING_TYPE", "hello world")

		value, err := GetEnvVar[string]("TEST_STRING_TYPE")
		if err != nil || value != "hello world" {
			t.Errorf("string test failed: %v, got %s", err, value)
		}
	})
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
