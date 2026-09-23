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

package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	agentName      = "preflight-nccl-loopback"
	componentClass = "Node"
	checkName      = "NCCLLoopbackTest"

	// envPublishTarget is the healthpub variable that switches on direct
	// mode. It is read here only to remember which mode the reporter runs in.
	envPublishTarget = "HEALTH_PUBLISH_TARGET"
)

// Socket mode timing. These are variables so tests can shorten them.
var (
	// socketWaitTimeout bounds how long socket mode waits for the node-local
	// platform connector socket file, before dialing and again when the
	// socket is missing at send time. It keeps the tolerance of the old
	// reporter, which retried for about 17 seconds, for a node whose
	// DaemonSet pod is still starting or restarting.
	socketWaitTimeout  = 20 * time.Second
	socketPollInterval = 500 * time.Millisecond

	// socketSendTimeout bounds one socket mode Publish call. healthpub sets
	// no deadline on the socket path, so without this a handler that hangs
	// would block the check forever. Three minutes covers healthpub's five
	// attempts of 30 seconds each plus the backoff between them.
	socketSendTimeout = 3 * time.Minute
)

// Reporter sends the check's health event to the platform connector through
// the shared healthpub client. With HEALTH_PUBLISH_TARGET set it publishes
// directly to the deployment platform connector; otherwise it uses the
// node-local Unix socket as before.
type Reporter struct {
	publisher          *healthpub.Publisher
	nodeName           string
	processingStrategy pb.ProcessingStrategy

	// direct is true when HEALTH_PUBLISH_TARGET was set at NewReporter.
	// Direct mode leaves timeouts and retries to healthpub; socket mode
	// bounds each send and waits for a missing socket itself.
	direct     bool
	socketPath string
}

// NewReporter dials the platform connector and builds a Reporter. socketPath
// is the node-local Unix socket used in socket mode. tokenPath is the
// optional file path of a projected ServiceAccount token to present as a
// Bearer credential on every socket mode call; empty disables token metadata.
// Direct mode reads its own token path from HEALTH_PUBLISH_TOKEN_PATH. A
// direct mode environment that is incomplete or invalid is returned as an
// error, so the caller can treat it like any other configuration error.
func NewReporter(
	ctx context.Context, socketPath, nodeName string, strategy pb.ProcessingStrategy, tokenPath string,
) (*Reporter, error) {
	// Remove unix:// prefix if present so the target is built the same way
	// for both forms of the socket setting.
	socketPath = strings.TrimPrefix(socketPath, "unix://")
	target := "unix://" + socketPath

	_, client, opt, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		if waitErr := waitForSocket(ctx, socketPath); waitErr != nil {
			return nil, waitErr
		}

		return grpc.NewClient(target, grpcclient.InsecureDialOptions(tokenPath)...)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	return &Reporter{
		publisher:          healthpub.New(client, target, agentName, opt),
		nodeName:           nodeName,
		processingStrategy: strategy,
		direct:             os.Getenv(envPublishTarget) != "",
		socketPath:         socketPath,
	}, nil
}

// waitForSocket waits up to socketWaitTimeout for the socket file to exist.
// It runs before the lazy dial and again when a send finds the socket
// missing. A socket that is still missing at the end is not an error here:
// the following Publish reports the connector as unavailable, which keeps the
// send failure exit code of the old reporter. Only a finished ctx is an error.
func waitForSocket(ctx context.Context, socketPath string) error {
	err := wait.PollUntilContextTimeout(ctx, socketPollInterval, socketWaitTimeout, true,
		func(context.Context) (bool, error) {
			_, statErr := os.Stat(socketPath)

			return statErr == nil, nil
		})
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("waiting for platform connector socket %s: %w", socketPath, ctx.Err())
	}

	slog.Warn("Platform connector socket not found after waiting; dialing anyway",
		"socket", socketPath, "waited", socketWaitTimeout)

	return nil
}

// Close releases the connection to the platform connector.
func (r *Reporter) Close() {
	r.publisher.CloseOrWarn()
}

func (r *Reporter) SendEvent(ctx context.Context, isHealthy, isFatal bool, message string, errorCode string) error {
	recommendedAction := pb.RecommendedAction_NONE
	if !isHealthy {
		recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
	}

	var errorCodes []string
	if errorCode != "" {
		errorCodes = []string{errorCode}
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		ComponentClass:     componentClass,
		CheckName:          checkName,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		Message:            message,
		RecommendedAction:  recommendedAction,
		ErrorCode:          errorCodes,
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           r.nodeName,
		ProcessingStrategy: r.processingStrategy,
		EntitiesImpacted:   []*pb.Entity{},
	}

	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}

	slog.Info("Sending health event",
		"is_healthy", isHealthy,
		"is_fatal", isFatal,
		"message", message,
		"error_code", errorCode,
		"recommended_action", pb.RecommendedAction_name[int32(recommendedAction)])

	// Every error, including healthpub.ErrPlatformConnectorUnavailable, is a
	// send failure: a one-shot check has no later poll to re-emit the event.
	if err := r.publish(ctx, healthEvents); err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	slog.Info("Health event sent successfully")

	return nil
}

// publish hands the batch to healthpub. Direct mode keeps the caller's
// context because healthpub applies its own retry window there. Socket mode
// bounds the call and, when the socket file is missing (the node-local
// platform connector is restarting during the benchmark), waits for it once
// with the same bounded wait used at startup and retries the send once.
func (r *Reporter) publish(ctx context.Context, events *pb.HealthEvents) error {
	if r.direct {
		return r.publisher.Publish(ctx, events)
	}

	err := r.publishOverSocket(ctx, events)
	if !errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
		return err
	}

	slog.Warn("Platform connector socket missing at send time; waiting for it to come back",
		"socket", r.socketPath, "wait", socketWaitTimeout)

	if waitErr := waitForSocket(ctx, r.socketPath); waitErr != nil {
		return waitErr
	}

	return r.publishOverSocket(ctx, events)
}

// publishOverSocket runs one socket mode Publish under socketSendTimeout.
func (r *Reporter) publishOverSocket(ctx context.Context, events *pb.HealthEvents) error {
	sendCtx, cancel := context.WithTimeout(ctx, socketSendTimeout)
	defer cancel()

	return r.publisher.Publish(sendCtx, events)
}
