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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var nicDriverEventCounter = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "syslog_health_monitor_nic_driver_errors",
		Help: "Total number of NIC driver/firmware errors detected from kernel logs",
	},
	[]string{"node", "event", "severity"},
)

// PreInitialize materializes zero-value counters for all loaded patterns so
// backends that establish a baseline from the first ingested sample (e.g.
// Google Managed Prometheus) do not drop the first real occurrence.
func PreInitialize(nodeName string, patterns []CompiledPattern) {
	for _, p := range patterns {
		sev := "non_fatal"
		if p.IsFatal {
			sev = "fatal"
		}

		nicDriverEventCounter.WithLabelValues(nodeName, p.Name, sev).Add(0)
	}
}
