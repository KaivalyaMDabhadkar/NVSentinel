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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractBDF(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantBDF string
		wantOK  bool
	}{
		{
			name:    "mlx5_core with BDF",
			line:    "mlx5_core 0000:3b:00.0: wait_func:1195: timeout. Will cause a leak",
			wantBDF: "0000:3b:00.0",
			wantOK:  true,
		},
		{
			name:    "pcieport with BDF",
			line:    "pcieport 0000:50:00.0: AER: PCIe Bus Error: severity=Uncorrectable (Fatal)",
			wantBDF: "0000:50:00.0",
			wantOK:  true,
		},
		{
			name:    "no BDF in line",
			line:    "mlx5_core: INFO: synd 0x8: unrecoverable hardware error.",
			wantBDF: "",
			wantOK:  false,
		},
		{
			name:    "uppercase hex BDF",
			line:    "mlx5_core 0000:AB:CD.E: some message",
			wantBDF: "0000:AB:CD.E",
			wantOK:  true,
		},
		{
			name:    "NETDEV WATCHDOG has no BDF",
			line:    "NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue 0 timed out",
			wantBDF: "",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bdf, ok := extractBDF(tc.line)
			assert.Equal(t, tc.wantOK, ok)

			if ok {
				assert.Equal(t, tc.wantBDF, bdf)
			}
		})
	}
}
