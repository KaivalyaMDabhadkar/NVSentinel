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

// Package pipeline provides a transformer pipeline for processing health events.
// It includes a registry-based factory for creating transformers from configuration.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type Transformer interface {
	Transform(ctx context.Context, event *pb.HealthEvent) error
	Name() string
}

// Prewarmer is a transformer that can prepare for a whole batch at once, for
// example by reading every distinct node of the batch concurrently, so the
// per-event pass does not pay for misses one after another. The error reports
// work the batch budget cut short, so the caller can defer the batch instead
// of processing events whose gate could not be evaluated.
type Prewarmer interface {
	Prewarm(ctx context.Context, events []*pb.HealthEvent) error
}

// BatchScope holds what transformers prepared for one batch, so that what
// the preparation read is what the transformers then use, even if a shared
// cache has dropped it in between. One scope lives for one ProcessBatch call.
type BatchScope struct {
	mu     sync.Mutex
	values map[string]any
}

// Store keeps a prepared value under key for the rest of the batch.
func (s *BatchScope) Store(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.values == nil {
		s.values = map[string]any{}
	}

	s.values[key] = value
}

// Load returns the value prepared under key for this batch, if any.
func (s *BatchScope) Load(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value, ok := s.values[key]

	return value, ok
}

type batchScopeKey struct{}

// WithBatchScope returns ctx carrying a fresh BatchScope. ProcessBatch does
// this for every batch; it is exported for tests of transformers.
func WithBatchScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchScopeKey{}, &BatchScope{})
}

// BatchScopeFromContext returns the batch's scope, or nil when the events are
// not being processed as a batch.
func BatchScopeFromContext(ctx context.Context) *BatchScope {
	scope, _ := ctx.Value(batchScopeKey{}).(*BatchScope)

	return scope
}

// Pipeline runs configured transformers for each event.
type Pipeline struct {
	transformers []Transformer
	// batchBudget bounds ProcessBatch for one batch as a whole, preparation
	// included. Zero, the node-local role, skips the preparation and runs the
	// transformers on the caller's context as it is.
	batchBudget time.Duration
}

func New(transformers ...Transformer) *Pipeline {
	return &Pipeline{transformers: transformers}
}

// WithBatchBudget bounds ProcessBatch for one batch as a whole and turns on
// the preparation step; see ProcessBatch.
func (p *Pipeline) WithBatchBudget(budget time.Duration) *Pipeline {
	p.batchBudget = budget

	return p
}

// Close releases resources owned by transformers that expose a Close method.
func (p *Pipeline) Close() {
	for _, t := range p.transformers {
		closer, ok := t.(interface{ Close() error })
		if !ok {
			continue
		}

		if err := closer.Close(); err != nil {
			slog.Warn("Failed to close pipeline transformer", "transformer", t.Name(), "error", err)
		}
	}
}

// Prewarm lets every transformer that can prepare for the batch do so, before
// the events are processed one by one. The returned error joins what they
// could not finish inside the budget; nil in the common case.
func (p *Pipeline) Prewarm(ctx context.Context, events []*pb.HealthEvent) error {
	var errs []error

	for _, t := range p.transformers {
		if prewarmer, ok := t.(Prewarmer); ok {
			if err := prewarmer.Prewarm(ctx, events); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// ProcessBatch runs one batch through the pipeline. With a batch budget it
// first lets the transformers prepare for the whole batch inside that budget
// and reports what they could not finish, before any transformer has seen an
// event, so the caller can defer the batch while nothing remembers it. Then
// every event is processed, inside the same budget. Without a budget the
// events are processed as they are.
func (p *Pipeline) ProcessBatch(ctx context.Context, events []*pb.HealthEvent) error {
	ctx = WithBatchScope(ctx)

	if p.batchBudget > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, p.batchBudget)
		defer cancel()

		if err := p.Prewarm(ctx, events); err != nil {
			return err
		}
	}

	for _, event := range events {
		p.Process(ctx, event)
	}

	return nil
}

// Process applies the pipeline to the event.
func (p *Pipeline) Process(ctx context.Context, event *pb.HealthEvent) {
	ctx, span := tracing.StartSpan(ctx, "platform_connector.pipeline.process")
	defer span.End()

	var failedCount int

	for _, t := range p.transformers {
		if err := t.Transform(ctx, event); err != nil {
			failedCount++

			slog.WarnContext(ctx, "Transformer failed",
				"transformer", t.Name(),
				"node", event.NodeName,
				"error", err)
			tracing.RecordError(span, err)
			span.AddEvent("platform_connector.pipeline.transformer_failed", trace.WithAttributes(
				attribute.String("platform_connector.pipeline.failed_transformer", t.Name()),
				attribute.String("platform_connector.pipeline.error.type", "running_transformer_failed"),
				attribute.String("platform_connector.pipeline.error.message", err.Error()),
			))
		}
	}

	span.SetAttributes(attribute.Int("platform_connector.pipeline.failed_stage_count", failedCount))
}
