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
	"strconv"
	"strings"
)

// GetEnvVar retrieves an environment variable and converts it to type T.
// Type must be explicitly specified: GetEnvVar[int]("PORT")
// If no default value is provided, the environment variable is required.
// If a default value is provided, it will be used when the environment variable is not set.
// Optional validator function can be provided - it should return nil if validation passes, error otherwise.
//
// Supported types:
//   - int
//   - uint
//   - float64
//   - bool (accepts: "true" or "false", case-insensitive)
//   - string
//
// Example usage:
//
//	// Required env var (must specify type explicitly)
//	port, err := configmanager.GetEnvVar[int]("PORT")
//
//	// With default value (type can be inferred)
//	timeout, err := configmanager.GetEnvVar[int]("TIMEOUT", 30)
//
//	// With default and validation
//	maxConn, err := configmanager.GetEnvVar[int]("MAX_CONN", 100, func(v int) error {
//	    if v <= 0 { return fmt.Errorf("must be positive") }
//	    return nil
//	})
//
//	// Required with validation (must specify type explicitly)
//	workers, err := configmanager.GetEnvVar[int]("WORKERS", func(v int) error {
//	    if v <= 0 { return fmt.Errorf("must be positive") }
//	    return nil
//	})
func GetEnvVar[T any](name string, defaultAndValidator ...any) (T, error) {
	var zero T

	var defaultValue *T

	var validator func(T) error

	// Parse variadic arguments: [defaultValue], [validator], or [defaultValue, validator]
	for _, arg := range defaultAndValidator {
		switch v := arg.(type) {
		case func(T) error:
			validator = v
		case T:
			defaultValue = &v
		}
	}

	valueStr, exists := os.LookupEnv(name)
	if !exists {
		// If env var not set, use default if provided
		if defaultValue != nil {
			return *defaultValue, nil
		}

		return zero, fmt.Errorf("environment variable %s is not set", name)
	}

	// Convert string to type T
	value, err := parseValue[T](valueStr)
	if err != nil {
		return zero, fmt.Errorf("error converting %s: %w", name, err)
	}

	// Validate if validator is provided
	if validator != nil {
		if err := validator(value); err != nil {
			return zero, fmt.Errorf("validation failed for %s: %w", name, err)
		}
	}

	return value, nil
}

func parseValue[T any](valueStr string) (T, error) {
	var zero T

	switch any(zero).(type) {
	case string:
		return any(valueStr).(T), nil
	case int:
		return parseAndConvert[T](parseInt(valueStr))
	case uint:
		return parseAndConvert[T](parseUint(valueStr))
	case float64:
		return parseAndConvert[T](parseFloat64(valueStr))
	case bool:
		return parseAndConvert[T](parseBool(valueStr))
	default:
		return zero, fmt.Errorf("unsupported type %T", zero)
	}
}

func parseAndConvert[T any](value any, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}

	return any(value).(T), nil
}

func parseInt(valueStr string) (int, error) {
	return strconv.Atoi(valueStr)
}

func parseUint(valueStr string) (uint, error) {
	v, err := strconv.ParseUint(valueStr, 10, 0)
	if err != nil {
		return 0, err
	}

	return uint(v), nil
}

func parseFloat64(valueStr string) (float64, error) {
	return strconv.ParseFloat(valueStr, 64)
}

// parseBool parses boolean values (accepts "true" or "false")
func parseBool(valueStr string) (bool, error) {
	valueStr = strings.ToLower(strings.TrimSpace(valueStr))

	switch valueStr {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value: %s (must be 'true' or 'false')", valueStr)
	}
}

// EnvVarSpec defines a specification for reading an environment variable.
// All fields except Name are optional.
//
// Example usage:
//
//	specs := []configmanager.EnvVarSpec{
//	    {Name: "DATABASE_URL"},  // Required by default
//	    {Name: "PORT", Optional: true, DefaultValue: "5432"},  // Optional with default
//	}
//	envVars, errors := configmanager.ReadEnvVars(specs)
//	if len(errors) > 0 {
//	    return fmt.Errorf("missing required vars: %v", errors)
//	}
type EnvVarSpec struct {
	Name         string // Required: The environment variable name to read
	Optional     bool   // Optional: If true, env var is optional; if false, it's required (default: false/required)
	DefaultValue string // Optional: Value to use when env var is not set (default: "")
}

// ReadEnvVars reads multiple environment variables based on the provided specifications.
// Returns a map of environment variable names to their values and a slice of errors.
// Environment variables are required by default unless Optional is set to true.
//
// Example usage:
//
//	specs := []configmanager.EnvVarSpec{
//	    {Name: "MONGODB_URI"},
//	    {Name: "MONGODB_DATABASE_NAME"},
//	    {Name: "MONGODB_PORT", Optional: true, DefaultValue: "27017"},
//	}
//	envVars, errors := configmanager.ReadEnvVars(specs)
//	if len(errors) > 0 {
//	    log.Fatalf("Missing required environment variables: %v", errors)
//	}
//	// Use the values
//	dbURI := envVars["MONGODB_URI"]
//	dbName := envVars["MONGODB_DATABASE_NAME"]
//	dbPort := envVars["MONGODB_PORT"]  // Will be "27017" if not set
func ReadEnvVars(specs []EnvVarSpec) (map[string]string, []error) {
	results := make(map[string]string)

	var errors []error

	for _, spec := range specs {
		value, exists := os.LookupEnv(spec.Name)

		if !exists {
			value, err := handleMissingEnvVar(spec)
			if err != nil {
				errors = append(errors, err)
			}

			if value != "" || spec.Optional {
				results[spec.Name] = value
			}

			continue
		}

		results[spec.Name] = value
	}

	return results, errors
}

func handleMissingEnvVar(spec EnvVarSpec) (string, error) {
	if spec.Optional {
		return spec.DefaultValue, nil
	}

	return "", fmt.Errorf("required environment variable %s is not set", spec.Name)
}
