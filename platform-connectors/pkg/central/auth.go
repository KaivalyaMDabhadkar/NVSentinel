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

package central

import (
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/kubeconfig"
)

// newValidator builds the TokenReview validator that authenticates callers.
// Its Kubernetes client is sized for a call on the path of every batch of the
// fleet, not for a controller that writes occasionally, and it is not wrapped
// with the audit logger: a TokenReview is a question, not a change to the
// cluster, and its request body is the caller's token.
func newValidator(cfg *config) (*grpcauth.Validator, error) {
	restConfig, err := kubeconfig.Load("")
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster kubernetes config: %w", err)
	}

	restConfig.QPS = cfg.tokenReviewQPS
	restConfig.Burst = cfg.tokenReviewBurst
	restConfig.Timeout = 10 * time.Second

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	// Every batch of the fleet authenticates; a line per success would be
	// most of the log.
	opts := []grpcauth.ValidatorOption{grpcauth.WithSuccessLogLevel(slog.LevelDebug)}

	if cfg.tokenCacheSize > 0 {
		opts = append(opts, grpcauth.WithCacheSize(cfg.tokenCacheSize))
	}

	return grpcauth.NewValidator(clientSet, cfg.audience, opts...)
}

// newAuthInterceptor builds caller authentication for the deployment role
// from the node-binding interceptor the DaemonSet uses, in its configuration
// without a local node: every caller must present a pod-bound token, its
// events are pinned to the node the token claims, the listed cross-node
// publishers may name any node, and only the allowed publishers may call at
// all. The interceptor validates both lists: a malformed username, or a
// cross-node entry missing from the allowlist, refuses to start.
func newAuthInterceptor(cfg *config, validator auth.TokenValidator) (grpc.UnaryServerInterceptor, error) {
	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		Validator:                validator,
		AllowedServiceAccounts:   cfg.allowedPublishers,
		CrossNodeServiceAccounts: cfg.crossNodePublishers,
		Mode:                     auth.ModeEnforce,
	})
	if err != nil {
		return nil, fmt.Errorf("ALLOWED_PUBLISHERS or CROSS_NODE_PUBLISHERS: %w", err)
	}

	return interceptor, nil
}
