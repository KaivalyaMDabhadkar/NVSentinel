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

package eventutil

import (
	"encoding/json"
	"fmt"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// ParseHealthEventFromEvent extracts and parses a health event from a database event.
// It handles both change stream events (with fullDocument) and direct document events.
// This is a shared utility used by multiple reconcilers (fault-remediation, node-drainer, etc.)
func ParseHealthEventFromEvent(event datastore.Event) (model.HealthEventWithStatus, error) {
	var healthEventWithStatus model.HealthEventWithStatus

	// Determine what to unmarshal: check if this is a change stream event with fullDocument
	var documentToUnmarshal interface{}
	if fullDoc, ok := event["fullDocument"]; ok {
		// This is a change stream event, extract the fullDocument
		documentToUnmarshal = fullDoc
	} else {
		// This is already the document itself
		documentToUnmarshal = event
	}

	// Convert to JSON to inspect structure
	jsonBytes, err := json.Marshal(documentToUnmarshal)
	if err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to marshal event to JSON: %w", err)
	}

	// Check if the data is nested inside a "document" field (PostgreSQL format)
	var tempMap map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &tempMap); err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to unmarshal to map: %w", err)
	}

	// If there's a "document" field, extract it
	if doc, ok := tempMap["document"]; ok {
		jsonBytes, err = json.Marshal(doc)
		if err != nil {
			return healthEventWithStatus, fmt.Errorf("failed to marshal document field: %w", err)
		}

		if err := json.Unmarshal(jsonBytes, &tempMap); err != nil {
			return healthEventWithStatus, fmt.Errorf("failed to unmarshal document field to map: %w", err)
		}
	}

	normalizeProtoWrapperBoolFields(tempMap)

	jsonBytes, err = json.Marshal(tempMap)
	if err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to marshal normalized health event: %w", err)
	}

	// Now unmarshal to the actual structure
	if err := json.Unmarshal(jsonBytes, &healthEventWithStatus); err != nil {
		return healthEventWithStatus, fmt.Errorf("failed to unmarshal health event: %w", err)
	}

	// Safety check - ensure HealthEvent is not nil
	if healthEventWithStatus.HealthEvent == nil {
		return healthEventWithStatus, fmt.Errorf("health event is nil after unmarshaling")
	}

	// Set default value for NodeQuarantined if nil (e.g., for new events)
	if healthEventWithStatus.HealthEventStatus.NodeQuarantined == "" {
		healthEventWithStatus.HealthEventStatus.NodeQuarantined = string(model.StatusNotStarted)
	}

	return healthEventWithStatus, nil
}

func normalizeProtoWrapperBoolFields(document map[string]interface{}) {
	for _, statusKey := range []string{"healtheventstatus", "healthEventStatus", "HealthEventStatus"} {
		status, ok := document[statusKey].(map[string]interface{})
		if !ok {
			continue
		}

		normalizeWrappedBoolField(status, "faultremediated")
		normalizeWrappedBoolField(status, "faultRemediated")
		normalizeWrappedBoolField(status, "FaultRemediated")
	}
}

func normalizeWrappedBoolField(document map[string]interface{}, field string) {
	value, ok := document[field]
	if !ok {
		return
	}

	if boolValue, ok := value.(bool); ok {
		document[field] = map[string]interface{}{"value": boolValue}
	}
}
