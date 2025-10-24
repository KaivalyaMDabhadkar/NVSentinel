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

package mongodb

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/metrics"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog/v2"
)

type EventWatcher struct {
	mongoConfig          storewatcher.MongoDBConfig
	tokenConfig          storewatcher.TokenConfig
	mongoPipeline        mongo.Pipeline
	collection           *mongo.Collection
	watcher              *storewatcher.ChangeStreamWatcher
	processEventCallback func(
		ctx context.Context,
		event *storeconnector.HealthEventWithStatus,
	) *storeconnector.Status
	unprocessedEventsMetricUpdateInterval time.Duration
	lastProcessedObjectID                 LastProcessedObjectIDStore
}

type LastProcessedObjectIDStore interface {
	StoreLastProcessedObjectID(objID primitive.ObjectID)
	LoadLastProcessedObjectID() (primitive.ObjectID, bool)
}

type EventWatcherInterface interface {
	Start(ctx context.Context) error
	SetProcessEventCallback(
		callback func(
			ctx context.Context,
			event *storeconnector.HealthEventWithStatus,
		) *storeconnector.Status,
	)
}

func NewEventWatcher(
	mongoConfig storewatcher.MongoDBConfig,
	tokenConfig storewatcher.TokenConfig,
	mongoPipeline mongo.Pipeline,
	collection *mongo.Collection,
	unprocessedEventsMetricUpdateInterval time.Duration,
	lastProcessedObjectID LastProcessedObjectIDStore,
) *EventWatcher {
	return &EventWatcher{
		mongoConfig:                           mongoConfig,
		tokenConfig:                           tokenConfig,
		mongoPipeline:                         mongoPipeline,
		collection:                            collection,
		unprocessedEventsMetricUpdateInterval: unprocessedEventsMetricUpdateInterval,
		lastProcessedObjectID:                 lastProcessedObjectID,
	}
}

func (w *EventWatcher) SetProcessEventCallback(
	callback func(
		ctx context.Context,
		event *storeconnector.HealthEventWithStatus,
	) *storeconnector.Status,
) {
	w.processEventCallback = callback
}

func (w *EventWatcher) Start(ctx context.Context) error {
	klog.Info("Starting MongoDB event watcher")

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, w.mongoConfig, w.tokenConfig, w.mongoPipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}
	defer watcher.Close(ctx)

	w.watcher = watcher

	watcher.Start(ctx)
	klog.Info("MongoDB change stream watcher started successfully")

	go w.updateUnprocessedEventsMetric(ctx, watcher)

	go func() {
		w.watchEvents(ctx, watcher)
		klog.Fatalf("MongoDB event watcher goroutine exited unexpectedly, event processing has stopped")
	}()

	<-ctx.Done()
	klog.Info("Context cancelled, stopping MongoDB event watcher")

	return nil
}

func (w *EventWatcher) watchEvents(ctx context.Context, watcher *storewatcher.ChangeStreamWatcher) {
	for event := range watcher.Events() {
		metrics.TotalEventsReceived.Inc()

		if processErr := w.processEvent(ctx, event); processErr != nil {
			klog.Errorf("Event processing failed, but still marking as processed to proceed ahead: %v", processErr)
		}

		if err := w.watcher.MarkProcessed(ctx); err != nil {
			metrics.ProcessingErrors.WithLabelValues("mark_processed_error").Inc()
			klog.Fatalf("Error updating resume token: %v", err)
		}
	}
}

func (w *EventWatcher) processEvent(ctx context.Context, event bson.M) error {
	healthEventWithStatus := storeconnector.HealthEventWithStatus{}
	err := storewatcher.UnmarshalFullDocumentFromEvent(
		event,
		&healthEventWithStatus,
	)

	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("unmarshal_error").Inc()
		return fmt.Errorf("failed to unmarshal event: %w", err)
	}

	klog.V(3).Infof("Processing event: %+v", healthEventWithStatus)
	w.storeEventObjectID(event)

	startTime := time.Now()
	status := w.processEventCallback(ctx, &healthEventWithStatus)

	if status != nil {
		if err := w.updateNodeQuarantineStatus(ctx, event, status); err != nil {
			metrics.ProcessingErrors.WithLabelValues("update_quarantine_status_error").Inc()
			return fmt.Errorf("failed to update node quarantine status: %w", err)
		}
	}

	duration := time.Since(startTime).Seconds()
	metrics.EventHandlingDuration.Observe(duration)

	return nil
}

func (w *EventWatcher) storeEventObjectID(eventBson bson.M) {
	if fullDoc, ok := eventBson["fullDocument"].(bson.M); ok {
		if objID, ok := fullDoc["_id"].(primitive.ObjectID); ok {
			w.lastProcessedObjectID.StoreLastProcessedObjectID(objID)
		}
	}
}

func (w *EventWatcher) updateUnprocessedEventsMetric(ctx context.Context,
	watcher *storewatcher.ChangeStreamWatcher) {
	ticker := time.NewTicker(w.unprocessedEventsMetricUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			objID, ok := w.lastProcessedObjectID.LoadLastProcessedObjectID()
			if !ok {
				continue
			}

			unprocessedCount, err := watcher.GetUnprocessedEventCount(ctx, objID)
			if err != nil {
				klog.V(3).Infof("Failed to get unprocessed event count: %v", err)
				continue
			}

			metrics.EventBacklogSize.Set(float64(unprocessedCount))
			klog.V(3).Infof("Updated unprocessed events metric: %d events after ObjectID %v",
				unprocessedCount, objID.Hex())
		}
	}
}

func (w *EventWatcher) updateNodeQuarantineStatus(
	ctx context.Context,
	event bson.M,
	nodeQuarantinedStatus *storeconnector.Status,
) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event")
	}

	filter := bson.M{"_id": document["_id"]}

	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.nodequarantined": *nodeQuarantinedStatus,
		},
	}

	if _, err := w.collection.UpdateOne(ctx, filter, update); err != nil {
		return fmt.Errorf("error updating document with _id: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Document with _id: %v has been updated with status %s", document["_id"], *nodeQuarantinedStatus)

	return nil
}
