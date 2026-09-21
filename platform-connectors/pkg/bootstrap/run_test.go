// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package bootstrap_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/bootstrap"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/nodelocal"
)

func freePort(t *testing.T) int {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := lis.Addr().(*net.TCPAddr).Port
	require.NoError(t, lis.Close())

	return port
}

// TestRun_ServesAndStopsCleanly: Run brings the node-local role up from
// config.json, answers a batch on its socket and the probes on the metrics port,
// and returns nil once its context ends, with the socket file gone.
func TestRun_ServesAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "pc.sock")
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"enableNodeBindingAuth": "false",
		"enableK8sPlatformConnector": "false", "enableGRPCSinkConnector": "false", "enablePromPlatformConnector": "false",
		"enableMongoDBStorePlatformConnector": "false", "enablePostgresDBStorePlatformConnector": "false",
		"K8sConnectorQps": 5.00, "K8sConnectorBurst": 10,
		"MaxNodeConditionMessageLength": 1024, "CompactedHealthEventMsgLen": 256,
		"pipeline": [{"name": "Deduplicator", "enabled": false, "config": "/nonexistent/dedup.toml"}]
	}`), 0o600))

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	role, err := nodelocal.New(nodelocal.Options{Socket: socket})
	require.NoError(t, err)

	done := make(chan error, 1)

	go func() {
		done <- bootstrap.Run(ctx, role, bootstrap.Options{ConfigPath: configPath, MetricsPort: port})
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(socket)

		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the socket appears")

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()

	_, err = pb.NewPlatformConnectorClient(conn).HealthEventOccurredV1(callCtx, &pb.HealthEvents{Version: 1})
	require.NoError(t, err, "an empty batch is accepted with no connector enabled")

	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "the probe answers on the metrics port")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a shutdown by context is a clean exit")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}

	_, err = os.Stat(socket)
	require.True(t, os.IsNotExist(err), "closing the listener removes the socket file")
}
