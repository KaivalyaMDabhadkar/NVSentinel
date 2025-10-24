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

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/metrics"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/mongodb"
	storeconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnectorprotos "github.com/nvidia/nvsentinel/platform-connectors/pkg/protos"
	"github.com/nvidia/nvsentinel/statemanager"
	"go.mongodb.org/mongo-driver/bson/primitive"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type ReconcilerConfig struct {
	TomlConfig            config.TomlConfig
	DryRun                bool
	CircuitBreakerEnabled bool
}

type rulesetsConfig struct {
	TaintConfigMap     map[string]*config.Taint
	CordonConfigMap    map[string]bool
	RuleSetPriorityMap map[string]int
}

// keyValTaint represents a taint key-value pair used for deduplication and priority tracking
type keyValTaint struct {
	Key   string
	Value string
}

type Reconciler struct {
	config                ReconcilerConfig
	k8sClient             informer.K8sClientInterface
	nodeInformer          *informer.NodeInformer
	lastProcessedObjectID atomic.Value
	cb                    breaker.CircuitBreaker
	eventWatcher          mongodb.EventWatcherInterface
	taintInitKeys         []keyValTaint // Pre-computed taint keys for map initialization

	// Label keys
	cordonedByLabelKey        string
	cordonedReasonLabelKey    string
	cordonedTimestampLabelKey string

	uncordonedByLabelKey        string
	uncordonedReasonLabelKey    string
	uncordonedTimestampLabelKey string
}

var (
	// Compile regex once at package initialization for efficiency
	labelValueRegex = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

	// Sentinel errors for better error handling
	errNoQuarantineAnnotation = fmt.Errorf("no quarantine annotation")
)

func NewReconciler(
	cfg ReconcilerConfig,
	k8sClient informer.K8sClientInterface,
	circuitBreaker breaker.CircuitBreaker,
	nodeInformer *informer.NodeInformer,
) *Reconciler {
	r := &Reconciler{
		config:       cfg,
		k8sClient:    k8sClient,
		cb:           circuitBreaker,
		nodeInformer: nodeInformer,
	}

	return r
}

func (r *Reconciler) SetLabelKeys(labelKeyPrefix string) {
	r.cordonedByLabelKey = labelKeyPrefix + "cordon-by"
	r.cordonedReasonLabelKey = labelKeyPrefix + "cordon-reason"
	r.cordonedTimestampLabelKey = labelKeyPrefix + "cordon-timestamp"

	r.uncordonedByLabelKey = labelKeyPrefix + "uncordon-by"
	r.uncordonedReasonLabelKey = labelKeyPrefix + "uncordon-reason"
	r.uncordonedTimestampLabelKey = labelKeyPrefix + "uncordon-timestamp"
}

func (r *Reconciler) StoreLastProcessedObjectID(objID primitive.ObjectID) {
	r.lastProcessedObjectID.Store(objID)
}

func (r *Reconciler) LoadLastProcessedObjectID() (primitive.ObjectID, bool) {
	lastObjID := r.lastProcessedObjectID.Load()
	if lastObjID == nil {
		return primitive.ObjectID{}, false
	}

	objID, ok := lastObjID.(primitive.ObjectID)

	return objID, ok
}

func (r *Reconciler) SetEventWatcher(eventWatcher mongodb.EventWatcherInterface) {
	r.eventWatcher = eventWatcher
}

func (r *Reconciler) Start(ctx context.Context) {
	r.setupNodeInformerCallbacks()

	ruleSetEvals := r.initializeRuleSetEvaluators()

	r.setupLabelKeys()

	rulesetsConfig := r.buildRulesetsConfig()

	r.precomputeTaintInitKeys(ruleSetEvals, rulesetsConfig)

	if !r.nodeInformer.WaitForSync(ctx) {
		klog.Fatal("Failed to sync NodeInformer cache, cannot proceed with event processing")
	}

	r.initializeQuarantineMetrics()

	if shouldHalt := r.checkCircuitBreakerAtStartup(ctx); shouldHalt {
		return
	}

	r.eventWatcher.SetProcessEventCallback(
		func(ctx context.Context, event *storeconnector.HealthEventWithStatus) *storeconnector.Status {
			return r.ProcessEvent(ctx, event, ruleSetEvals, rulesetsConfig)
		},
	)

	if err := r.eventWatcher.Start(ctx); err != nil {
		klog.Fatalf("Event watcher failed with error: %v", err)
	}

	klog.Info("Event watcher stopped, exiting fault-quarantine reconciler.")
}

// setupNodeInformerCallbacks configures callbacks on the already-created node informer
func (r *Reconciler) setupNodeInformerCallbacks() {
	r.nodeInformer.SetOnQuarantinedNodeDeletedCallback(func(nodeName string) {
		metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
		klog.Infof("Set currentQuarantinedNodes to 0 for deleted quarantined node: %s", nodeName)
	})

	r.nodeInformer.SetOnManualUncordonCallback(r.handleManualUncordon)
}

// initializeRuleSetEvaluators initializes all rule set evaluators from config
func (r *Reconciler) initializeRuleSetEvaluators() []evaluator.RuleSetEvaluatorIface {
	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(r.config.TomlConfig.RuleSets, r.nodeInformer)
	if err != nil {
		klog.Fatalf("failed to initialize all rule set evaluators: %+v", err)
	}

	return ruleSetEvals
}

// setupLabelKeys configures label keys for cordon/uncordon tracking
func (r *Reconciler) setupLabelKeys() {
	r.SetLabelKeys(r.config.TomlConfig.LabelPrefix)

	if fqClient, ok := r.k8sClient.(*informer.FaultQuarantineClient); ok {
		fqClient.SetLabelKeys(r.cordonedReasonLabelKey, r.uncordonedReasonLabelKey)
	}
}

// buildRulesetsConfig builds the rulesets configuration maps from TOML config
func (r *Reconciler) buildRulesetsConfig() rulesetsConfig {
	taintConfigMap := make(map[string]*config.Taint)
	cordonConfigMap := make(map[string]bool)
	ruleSetPriorityMap := make(map[string]int)

	for _, ruleSet := range r.config.TomlConfig.RuleSets {
		if ruleSet.Taint.Key != "" {
			taintConfigMap[ruleSet.Name] = &ruleSet.Taint
		}

		if ruleSet.Cordon.ShouldCordon {
			cordonConfigMap[ruleSet.Name] = true
		}

		if ruleSet.Priority > 0 {
			ruleSetPriorityMap[ruleSet.Name] = ruleSet.Priority
		}
	}

	return rulesetsConfig{
		TaintConfigMap:     taintConfigMap,
		CordonConfigMap:    cordonConfigMap,
		RuleSetPriorityMap: ruleSetPriorityMap,
	}
}

// precomputeTaintInitKeys pre-computes taint keys from rulesets for efficient map initialization
func (r *Reconciler) precomputeTaintInitKeys(
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) {
	r.taintInitKeys = make([]keyValTaint, 0, len(ruleSetEvals))

	for _, eval := range ruleSetEvals {
		taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
		if taintConfig != nil {
			keyVal := keyValTaint{
				Key:   taintConfig.Key,
				Value: taintConfig.Value,
			}
			r.taintInitKeys = append(r.taintInitKeys, keyVal)
		}
	}

	klog.Infof("Pre-computed %d taint initialization keys", len(r.taintInitKeys))
}

// initializeQuarantineMetrics initializes metrics for already quarantined nodes
func (r *Reconciler) initializeQuarantineMetrics() {
	totalNodes, quarantinedNodesMap, err := r.nodeInformer.GetNodeCounts()
	if err != nil {
		klog.Errorf("Failed to get initial node counts: %v", err)
		return
	}

	for nodeName := range quarantinedNodesMap {
		metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(1)
	}

	klog.Infof("Initial state: Total Nodes=%d, Quarantined Nodes=%d, Map: %+v",
		totalNodes, len(quarantinedNodesMap), quarantinedNodesMap)
}

// checkCircuitBreakerAtStartup checks if circuit breaker is tripped at startup
// Returns true if processing should halt
func (r *Reconciler) checkCircuitBreakerAtStartup(ctx context.Context) bool {
	// If breaker is enabled and already tripped at startup, halt until restart/manual close
	if !r.config.CircuitBreakerEnabled {
		return false
	}

	tripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		klog.Errorf("Error checking if circuit breaker is tripped: %v", err)
		<-ctx.Done()

		return true
	}

	if tripped {
		klog.Errorf("Fault Quarantine circuit breaker is TRIPPED. Halting event dequeuing indefinitely.")
		<-ctx.Done()

		return true
	}

	klog.Info("Listening for events on the channel...")

	return false
}

// ProcessEvent processes a single health event
func (r *Reconciler) ProcessEvent(
	ctx context.Context,
	event *storeconnector.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) *storeconnector.Status {
	if shouldHalt := r.checkCircuitBreakerAndHalt(ctx); shouldHalt {
		return nil
	}

	klog.V(3).Infof("Processing event %s", event.HealthEvent.CheckName)

	isNodeQuarantined := r.handleEvent(ctx, event, ruleSetEvals, rulesetsConfig)

	if isNodeQuarantined == nil {
		// Event was skipped (no quarantine action taken)
		klog.V(2).Infof("Skipped processing event for node %s, no status update needed",
			event.HealthEvent.NodeName)
		metrics.TotalEventsSkipped.Inc()
	} else if *isNodeQuarantined == storeconnector.Quarantined ||
		*isNodeQuarantined == storeconnector.UnQuarantined ||
		*isNodeQuarantined == storeconnector.AlreadyQuarantined {
		metrics.TotalEventsSuccessfullyProcessed.Inc()
	}

	return isNodeQuarantined
}

// checkCircuitBreakerAndHalt checks if circuit breaker is tripped and returns true if processing should halt
func (r *Reconciler) checkCircuitBreakerAndHalt(ctx context.Context) bool {
	if !r.config.CircuitBreakerEnabled {
		return false
	}

	tripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		klog.Errorf("Error checking if circuit breaker is tripped: %v", err)
		<-ctx.Done()

		return true
	}

	if tripped {
		klog.Errorf("Circuit breaker TRIPPED. Halting event processing until restart and breaker reset.")
		<-ctx.Done()

		return true
	}

	return false
}

func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *storeconnector.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) *storeconnector.Status {
	annotations, quarantineAnnotationExists := r.hasExistingQuarantine(event.HealthEvent.NodeName)

	if quarantineAnnotationExists {
		// The node was already quarantined by FQM earlier. Delegate to the
		// specialized handler which decides whether to keep it quarantined or
		// un-quarantine based on the incoming event.
		return r.handleAlreadyQuarantinedNode(ctx, event.HealthEvent, ruleSetEvals)
	}

	// For healthy events, if there's no existing quarantine annotation,
	// skip processing as there's no transition from unhealthy to healthy
	if event.HealthEvent.IsHealthy {
		klog.Infof("Skipping healthy event for node %s as there's no existing quarantine annotation, Event: %+v",
			event.HealthEvent.NodeName, event.HealthEvent)

		return nil
	}

	var taintAppliedMap sync.Map

	var labelsMap sync.Map

	var isCordoned atomic.Bool

	var taintEffectPriorityMap sync.Map

	// Initialize taint maps using pre-computed keys
	for _, keyVal := range r.taintInitKeys {
		taintAppliedMap.Store(keyVal, "")
		taintEffectPriorityMap.Store(keyVal, -1)
	}

	r.evaluateRulesets(
		event, ruleSetEvals, rulesetsConfig,
		&taintAppliedMap, &labelsMap, &isCordoned, &taintEffectPriorityMap,
	)

	taintsToBeApplied := r.collectTaintsToApply(&taintAppliedMap)

	annotationsMap := r.prepareAnnotations(taintsToBeApplied, &labelsMap, &isCordoned)

	isNodeQuarantined := len(taintsToBeApplied) > 0 || isCordoned.Load()
	if !isNodeQuarantined {
		return nil
	}

	return r.applyQuarantine(ctx, event, annotations, taintsToBeApplied, annotationsMap, &labelsMap, &isCordoned)
}

func (r *Reconciler) hasExistingQuarantine(nodeName string) (map[string]string, bool) {
	annotations, annErr := r.getNodeQuarantineAnnotations(nodeName)
	if annErr != nil {
		klog.Errorf("failed to fetch annotations for node %s: %v", nodeName, annErr)
		return annotations, false
	}

	if annotations == nil {
		return annotations, false
	}

	annotationVal, exists := annotations[common.QuarantineHealthEventAnnotationKey]

	return annotations, exists && annotationVal != ""
}

// handleAlreadyQuarantinedNode handles events for nodes that are already quarantined
func (r *Reconciler) handleAlreadyQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) *storeconnector.Status {
	var status storeconnector.Status

	// Delegate to handleQuarantinedNode which decides whether to keep the node
	// quarantined or un-quarantine based on the incoming event
	if r.handleQuarantinedNode(ctx, event, ruleSetEvals) {
		status = storeconnector.AlreadyQuarantined
	} else {
		status = storeconnector.UnQuarantined
	}

	return &status
}

// evaluateRulesets evaluates all rulesets against the health event in parallel
func (r *Reconciler) evaluateRulesets(
	event *storeconnector.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
	taintAppliedMap *sync.Map,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
	taintEffectPriorityMap *sync.Map,
) {
	// Handle quarantine override (force quarantine without rule evaluation)
	if event.HealthEvent.QuarantineOverrides != nil && event.HealthEvent.QuarantineOverrides.Force {
		isCordoned.Store(true)

		creatorID := event.HealthEvent.Metadata["creator_id"]
		labelsMap.LoadOrStore(r.cordonedByLabelKey, event.HealthEvent.Agent+"-"+creatorID)
		labelsMap.Store(r.cordonedReasonLabelKey,
			formatCordonOrUncordonReasonValue(event.HealthEvent.Message, 63))

		return
	}

	var wg sync.WaitGroup

	// Evaluate each ruleset in parallel
	for _, eval := range ruleSetEvals {
		wg.Add(1)

		go func(eval evaluator.RuleSetEvaluatorIface) {
			defer wg.Done()

			klog.Infof("Handling event: %+v for ruleset: %+v", event, eval.GetName())

			metrics.RulesetEvaluations.WithLabelValues(eval.GetName()).Inc()

			ruleEvaluatedResult, err := eval.Evaluate(event.HealthEvent)

			switch {
			case ruleEvaluatedResult == common.RuleEvaluationSuccess:
				r.handleSuccessfulRuleEvaluation(
					eval, rulesetsConfig, labelsMap, isCordoned, taintAppliedMap, taintEffectPriorityMap)
			case err != nil:
				r.handleRuleEvaluationError(event.HealthEvent, eval.GetName(), err)
			default:
				metrics.RulesetFailed.WithLabelValues(eval.GetName()).Inc()
			}
		}(eval)
	}

	wg.Wait()
}

// handleSuccessfulRuleEvaluation processes a successful rule evaluation result
func (r *Reconciler) handleSuccessfulRuleEvaluation(
	eval evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
	taintAppliedMap *sync.Map,
	taintEffectPriorityMap *sync.Map,
) {
	metrics.RulesetPassed.WithLabelValues(eval.GetName()).Inc()

	// Handle cordon configuration if specified
	shouldCordon := rulesetsConfig.CordonConfigMap[eval.GetName()]
	if shouldCordon {
		isCordoned.Store(true)

		newCordonReason := eval.GetName()

		if oldReasonVal, exist := labelsMap.Load(r.cordonedReasonLabelKey); exist {
			oldCordonReason := oldReasonVal.(string)
			newCordonReason = oldCordonReason + "-" + newCordonReason
		}

		labelsMap.Store(r.cordonedReasonLabelKey, formatCordonOrUncordonReasonValue(newCordonReason, 63))
	}

	taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
	if taintConfig != nil {
		r.updateTaintMaps(eval.GetName(), taintConfig, rulesetsConfig, taintAppliedMap, taintEffectPriorityMap)
	}
}

// updateTaintMaps updates taint maps with priority-based logic to handle multiple rulesets
// affecting the same taint key-value pair
func (r *Reconciler) updateTaintMaps(
	evalName string,
	taintConfig *config.Taint,
	rulesetsConfig rulesetsConfig,
	taintAppliedMap *sync.Map,
	taintEffectPriorityMap *sync.Map,
) {
	keyVal := keyValTaint{Key: taintConfig.Key, Value: taintConfig.Value}

	currentVal, _ := taintAppliedMap.Load(keyVal)
	currentEffect := currentVal.(string)

	currentPriorityVal, _ := taintEffectPriorityMap.Load(keyVal)
	currentPriority := currentPriorityVal.(int)

	newPriority := rulesetsConfig.RuleSetPriorityMap[evalName]

	// Update if no effect set yet or new priority is higher
	if currentEffect == "" || newPriority > currentPriority {
		taintEffectPriorityMap.Store(keyVal, newPriority)
		taintAppliedMap.Store(keyVal, taintConfig.Effect)
	}
}

// handleRuleEvaluationError handles errors during rule evaluation
func (r *Reconciler) handleRuleEvaluationError(
	event *platformconnectorprotos.HealthEvent,
	evalName string,
	err error,
) {
	klog.Errorf("rule evaluation failed for ruleset %s on node %s: %v", evalName, event.NodeName, err)
	metrics.ProcessingErrors.WithLabelValues("ruleset_evaluation_error").Inc()
	metrics.RulesetFailed.WithLabelValues(evalName).Inc()
}

// collectTaintsToApply collects all taints that should be applied from the taint map
func (r *Reconciler) collectTaintsToApply(taintAppliedMap *sync.Map) []config.Taint {
	taintsToBeApplied := []config.Taint{}

	// Check the taint map and collect the taints which are to be applied
	taintAppliedMap.Range(func(k, v interface{}) bool {
		keyVal := k.(keyValTaint)
		effect := v.(string)

		if effect != "" {
			taintsToBeApplied = append(taintsToBeApplied, config.Taint{
				Key:    keyVal.Key,
				Value:  keyVal.Value,
				Effect: effect,
			})
		}

		return true
	})

	return taintsToBeApplied
}

// prepareAnnotations prepares annotations and labels to be applied if any
func (r *Reconciler) prepareAnnotations(
	taintsToBeApplied []config.Taint,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
) map[string]string {
	annotationsMap := map[string]string{}

	if len(taintsToBeApplied) > 0 {
		// Store the taints applied as an annotation
		taintsJsonStr, err := json.Marshal(taintsToBeApplied)
		if err != nil {
			klog.Errorf("failed to marshal taints for annotation: %v", err)
		} else {
			annotationsMap[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = string(taintsJsonStr)
		}
	}

	if isCordoned.Load() {
		// Store cordon as an annotation
		annotationsMap[common.QuarantineHealthEventIsCordonedAnnotationKey] =
			common.QuarantineHealthEventIsCordonedAnnotationValueTrue

		labelsMap.LoadOrStore(r.cordonedByLabelKey, common.ServiceName)
		labelsMap.Store(r.cordonedTimestampLabelKey, time.Now().UTC().Format("2006-01-02T15-04-05Z"))
		labelsMap.Store(string(statemanager.NVSentinelStateLabelKey), string(statemanager.QuarantinedLabelValue))
	}

	return annotationsMap
}

// applyQuarantine applies quarantine actions to a node (taints, cordon, annotations)
func (r *Reconciler) applyQuarantine(
	ctx context.Context,
	event *storeconnector.HealthEventWithStatus,
	annotations map[string]string,
	taintsToBeApplied []config.Taint,
	annotationsMap map[string]string,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
) *storeconnector.Status {
	// Record an event to sliding window before actually quarantining
	r.recordCordonEventInCircuitBreaker(event)

	// Create health events structure for the new quarantine with sanitized health event
	healthEvents := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	updated := healthEvents.AddOrUpdateEvent(event.HealthEvent)

	if !updated {
		klog.Infof("Health event %+v already exists for node %s, skipping quarantine",
			event.HealthEvent, event.HealthEvent.NodeName)

		return nil
	}

	if err := r.addHealthEventAnnotation(healthEvents, annotationsMap); err != nil {
		return nil
	}

	// Remove manual uncordon annotation if present before applying new quarantine
	r.cleanupManualUncordonAnnotation(ctx, event.HealthEvent.NodeName, annotations)

	if !r.config.CircuitBreakerEnabled {
		klog.Infof("Circuit breaker is disabled, proceeding with quarantine action for node %s "+
			"without circuit breaker protection", event.HealthEvent.NodeName)
	}

	// Convert sync.Map to regular map for K8s API call
	labels := make(map[string]string)

	labelsMap.Range(func(key, value any) bool {
		if strKey, ok := key.(string); ok {
			if strValue, ok := value.(string); ok {
				labels[strKey] = strValue
			}
		}

		return true
	})

	err := r.k8sClient.TaintAndCordonNodeAndSetAnnotations(
		ctx,
		event.HealthEvent.NodeName,
		taintsToBeApplied,
		isCordoned.Load(),
		annotationsMap,
		labels,
	)
	if err != nil {
		klog.Errorf("failed to taint and cordon node %s: %v", event.HealthEvent.NodeName, err)
		metrics.ProcessingErrors.WithLabelValues("taint_and_cordon_error").Inc()

		return nil
	}

	r.updateQuarantineMetrics(event.HealthEvent.NodeName, taintsToBeApplied, isCordoned)

	status := storeconnector.Quarantined

	return &status
}

// recordCordonEventInCircuitBreaker records a cordon event in the circuit breaker if enabled
func (r *Reconciler) recordCordonEventInCircuitBreaker(event *storeconnector.HealthEventWithStatus) {
	if r.config.CircuitBreakerEnabled &&
		(event.HealthEvent.QuarantineOverrides == nil || !event.HealthEvent.QuarantineOverrides.Force) {
		r.cb.AddCordonEvent(event.HealthEvent.NodeName)
	}
}

// addHealthEventAnnotation adds health event annotation to the annotations map
func (r *Reconciler) addHealthEventAnnotation(
	healthEvents *healthEventsAnnotation.HealthEventsAnnotationMap,
	annotationsMap map[string]string,
) error {
	eventJsonStr, err := json.Marshal(healthEvents)
	if err != nil {
		return fmt.Errorf("failed to marshal health events: %w", err)
	}

	annotationsMap[common.QuarantineHealthEventAnnotationKey] = string(eventJsonStr)

	return nil
}

// updateQuarantineMetrics updates Prometheus metrics after quarantining a node
func (r *Reconciler) updateQuarantineMetrics(
	nodeName string,
	taintsToBeApplied []config.Taint,
	isCordoned *atomic.Bool,
) {
	metrics.TotalNodesQuarantined.WithLabelValues(nodeName).Inc()
	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(1)

	for _, taint := range taintsToBeApplied {
		metrics.TaintsApplied.WithLabelValues(taint.Key, taint.Effect).Inc()
	}

	if isCordoned.Load() {
		metrics.CordonsApplied.Inc()
	}
}

// eventMatchesAnyRule checks if an event matches at least one configured ruleset
func (r *Reconciler) eventMatchesAnyRule(
	event *platformconnectorprotos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) bool {
	for _, eval := range ruleSetEvals {
		result, err := eval.Evaluate(event)
		if err != nil {
			continue
		}

		if result == common.RuleEvaluationSuccess {
			return true
		}
	}

	return false
}

// handleUnhealthyEventOnQuarantinedNode handles unhealthy events on already-quarantined nodes
func (r *Reconciler) handleUnhealthyEventOnQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	healthEventsAnnotationMap *healthEventsAnnotation.HealthEventsAnnotationMap,
) bool {
	// Check if this event matches any configured rules before adding to annotation
	if !r.eventMatchesAnyRule(event, ruleSetEvals) {
		klog.Infof("Unhealthy event %s on node %s doesn't match any rules, skipping annotation update",
			event.CheckName, event.NodeName)
		return true
	}

	// Handle unhealthy event - add new entity failures
	added := healthEventsAnnotationMap.AddOrUpdateEvent(event)

	if added {
		klog.Infof("Added entity failures for check %s on node %s (total tracked entities: %d)",
			event.CheckName, event.NodeName, healthEventsAnnotationMap.Count())

		if err := r.addEventToAnnotation(ctx, event); err != nil {
			klog.Errorf("Failed to update health events annotation: %v", err)
			return true
		}
	} else {
		klog.V(2).Infof("All entities already tracked for check %s on node %s",
			event.CheckName, event.NodeName)
	}

	// Node remains quarantined
	return true
}

func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) bool {
	healthEventsAnnotationMap, annotations, err := r.getHealthEventsFromAnnotation(ctx, event)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("get_node_annotations_error").Inc()
		return !errors.Is(err, errNoQuarantineAnnotation)
	}

	_, hasExistingCheck := healthEventsAnnotationMap.GetEvent(event)

	if !event.IsHealthy {
		return r.handleUnhealthyEventOnQuarantinedNode(ctx, event, ruleSetEvals, healthEventsAnnotationMap)
	}

	// Handle healthy event
	if !hasExistingCheck {
		klog.V(2).Infof("Received healthy event for untracked check %s on node %s (other checks may still be failing)",
			event.CheckName, event.NodeName)
		return true
	}

	// Remove the specific entities that have recovered
	// With entity-level tracking, each entity is handled independently
	removedCount := healthEventsAnnotationMap.RemoveEvent(event)

	if removedCount > 0 {
		klog.Infof("Removed %d recovered entities for check %s on node %s (remaining entities: %d)",
			removedCount, event.CheckName, event.NodeName, healthEventsAnnotationMap.Count())
	} else {
		klog.V(2).Infof("No matching entities to remove for check %s on node %s",
			event.CheckName, event.NodeName)
	}

	if healthEventsAnnotationMap.IsEmpty() {
		// All checks recovered - uncordon the node
		klog.Infof("All health checks recovered for node %s, proceeding with uncordon",
			event.NodeName)
		return r.performUncordon(ctx, event, annotations)
	}

	// Remove this event's entities from the node's annotation
	if err := r.removeEventFromAnnotation(ctx, event); err != nil {
		klog.Errorf("Failed to update health events annotation after recovery: %v", err)
		return true
	}

	// Node remains quarantined as there are still failing checks
	klog.Infof("Node %s remains quarantined with %d failing checks: %v",
		event.NodeName, healthEventsAnnotationMap.Count(), healthEventsAnnotationMap.GetAllCheckNames())

	return true
}

func (r *Reconciler) getHealthEventsFromAnnotation(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
) (*healthEventsAnnotation.HealthEventsAnnotationMap, map[string]string, error) {
	annotations, err := r.getNodeQuarantineAnnotations(event.NodeName)
	if err != nil {
		klog.Errorf("failed to get node annotations for node %s: %v", event.NodeName, err)
		metrics.ProcessingErrors.WithLabelValues("get_node_annotations_error").Inc()

		return nil, nil, fmt.Errorf("failed to get annotations: %w", err)
	}

	quarantineAnnotationStr, exists := annotations[common.QuarantineHealthEventAnnotationKey]
	if !exists || quarantineAnnotationStr == "" {
		klog.Infof("No quarantine annotation found for node %s", event.NodeName)
		return nil, nil, errNoQuarantineAnnotation
	}

	// Try to unmarshal as HealthEventsAnnotationMap first
	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap
	err = json.Unmarshal([]byte(quarantineAnnotationStr), &healthEventsMap)

	if err != nil {
		// Fallback: try to unmarshal as single HealthEvent for backward compatibility
		var singleHealthEvent platformconnectorprotos.HealthEvent

		if err2 := json.Unmarshal([]byte(quarantineAnnotationStr), &singleHealthEvent); err2 == nil {
			// Convert single event to health events structure for local processing
			klog.Infof("Found old format annotation for node %s, converting locally", event.NodeName)

			healthEventsMap = *healthEventsAnnotation.NewHealthEventsAnnotationMap()
			healthEventsMap.AddOrUpdateEvent(&singleHealthEvent)
		} else {
			return nil, nil, fmt.Errorf("failed to unmarshal annotation for node %s: %w", event.NodeName, err)
		}
	}

	return &healthEventsMap, annotations, nil
}

// addEventToAnnotation adds or updates a health event in the node's quarantine annotation
func (r *Reconciler) addEventToAnnotation(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
) error {
	updateFn := func(node *corev1.Node) error {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		// Parse existing annotation (with backward compatibility)
		healthEventsMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		existingAnnotation := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		if existingAnnotation != "" {
			if err := json.Unmarshal([]byte(existingAnnotation), healthEventsMap); err != nil {
				// Try old format for backward compatibility
				var singleEvent platformconnectorprotos.HealthEvent
				if err2 := json.Unmarshal([]byte(existingAnnotation), &singleEvent); err2 == nil {
					healthEventsMap.AddOrUpdateEvent(&singleEvent)
				}
			}
		}

		healthEventsMap.AddOrUpdateEvent(event)

		annotationBytes, _ := json.Marshal(healthEventsMap)
		node.Annotations[common.QuarantineHealthEventAnnotationKey] = string(annotationBytes)

		klog.V(3).Infof("Added/updated event for node %s, total entity-level events: %d",
			event.NodeName, healthEventsMap.Count())

		return nil
	}

	return r.nodeInformer.UpdateNode(ctx, event.NodeName, updateFn)
}

// removeEventFromAnnotation removes entities from a health event in the node's quarantine annotation
func (r *Reconciler) removeEventFromAnnotation(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
) error {
	updateFn := func(node *corev1.Node) error {
		if node.Annotations == nil {
			return nil
		}

		existingAnnotation, exists := node.Annotations[common.QuarantineHealthEventAnnotationKey]
		if !exists || existingAnnotation == "" {
			return nil
		}

		// Parse existing annotation (with backward compatibility)
		healthEventsMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		if err := json.Unmarshal([]byte(existingAnnotation), healthEventsMap); err != nil {
			// Try old format for backward compatibility
			var singleEvent platformconnectorprotos.HealthEvent
			if err2 := json.Unmarshal([]byte(existingAnnotation), &singleEvent); err2 == nil {
				healthEventsMap.AddOrUpdateEvent(&singleEvent)
			} else {
				return fmt.Errorf("failed to parse annotation: %w", err)
			}
		}

		// Remove the event's entities using HealthEventsAnnotationMap logic
		healthEventsMap.RemoveEvent(event)

		annotationBytes, _ := json.Marshal(healthEventsMap)
		node.Annotations[common.QuarantineHealthEventAnnotationKey] = string(annotationBytes)

		klog.V(3).Infof("Removed entities for node %s, remaining entity-level events: %d",
			event.NodeName, healthEventsMap.Count())

		return nil
	}

	return r.nodeInformer.UpdateNode(ctx, event.NodeName, updateFn)
}

func (r *Reconciler) performUncordon(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	annotations map[string]string,
) bool {
	klog.Infof("All entities recovered for check %s on node %s - proceeding with uncordon",
		event.CheckName, event.NodeName)

	// Prepare uncordon parameters
	taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, err := r.prepareUncordonParams(
		event, annotations)
	if err != nil {
		klog.Errorf("failed to prepare uncordon params for node %s: %v", event.NodeName, err)
		return true
	}

	// Nothing to uncordon
	if len(taintsToBeRemoved) == 0 && !isUnCordon {
		return false
	}

	// Add the main quarantine annotation to removal list
	annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventAnnotationKey)

	if !r.config.CircuitBreakerEnabled {
		klog.Infof("Circuit breaker is disabled, proceeding with unquarantine action for node %s", event.NodeName)
	}

	labelsToRemove := []string{
		r.cordonedByLabelKey,
		r.cordonedReasonLabelKey,
		r.cordonedTimestampLabelKey,
		statemanager.NVSentinelStateLabelKey,
	}

	if err := r.k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
		ctx,
		event.NodeName,
		taintsToBeRemoved,
		isUnCordon,
		annotationsToBeRemoved,
		labelsToRemove,
		labelsMap,
	); err != nil {
		klog.Errorf("failed to untaint and uncordon node %s: %v", event.NodeName, err)
		metrics.ProcessingErrors.WithLabelValues("untaint_and_uncordon_error").Inc()

		return true
	}

	r.updateUncordonMetrics(event.NodeName, taintsToBeRemoved, isUnCordon)

	return false
}

// prepareUncordonParams prepares parameters for uncordoning a node
func (r *Reconciler) prepareUncordonParams(
	event *platformconnectorprotos.HealthEvent,
	annotations map[string]string,
) ([]config.Taint, []string, bool, map[string]string, error) {
	var (
		annotationsToBeRemoved = []string{}
		taintsToBeRemoved      []config.Taint
		isUnCordon             = false
		labelsMap              = map[string]string{}
	)

	quarantineAnnotationEventTaintsAppliedStr, taintsExists :=
		annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
	if taintsExists && quarantineAnnotationEventTaintsAppliedStr != "" {
		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventAppliedTaintsAnnotationKey)

		err := json.Unmarshal([]byte(quarantineAnnotationEventTaintsAppliedStr), &taintsToBeRemoved)
		if err != nil {
			return nil, nil, false, nil, fmt.Errorf("failed to unmarshal taints annotation for node %s: %w", event.NodeName, err)
		}
	}

	quarantineAnnotationEventIsCordonStr, cordonExists :=
		annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if cordonExists && quarantineAnnotationEventIsCordonStr == common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
		isUnCordon = true

		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventIsCordonedAnnotationKey)
		labelsMap[r.uncordonedByLabelKey] = common.ServiceName
		labelsMap[r.uncordonedTimestampLabelKey] = time.Now().UTC().Format("2006-01-02T15-04-05Z")
	}

	return taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, nil
}

func (r *Reconciler) updateUncordonMetrics(
	nodeName string,
	taintsToBeRemoved []config.Taint,
	isUnCordon bool,
) {
	metrics.TotalNodesUnquarantined.WithLabelValues(nodeName).Inc()
	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
	klog.Infof("Set currentQuarantinedNodes to 0 for unquarantined node: %s", nodeName)

	for _, taint := range taintsToBeRemoved {
		metrics.TaintsRemoved.WithLabelValues(taint.Key, taint.Effect).Inc()
	}

	if isUnCordon {
		metrics.CordonsRemoved.Inc()
	}
}

func formatCordonOrUncordonReasonValue(input string, length int) string {
	formatted := labelValueRegex.ReplaceAllString(input, "-")

	if len(formatted) > length {
		formatted = formatted[:length]
	}

	// Ensure it starts and ends with an alphanumeric character
	formatted = strings.Trim(formatted, "-")

	return formatted
}

// getNodeQuarantineAnnotations retrieves quarantine annotations from the informer cache
func (r *Reconciler) getNodeQuarantineAnnotations(nodeName string) (map[string]string, error) {
	node, err := r.nodeInformer.GetNode(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node from cache: %w", err)
	}

	// Extract only quarantine annotations
	quarantineAnnotations := make(map[string]string)
	quarantineKeys := []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantinedNodeUncordonedManuallyAnnotationKey,
	}

	if node.Annotations != nil {
		for _, key := range quarantineKeys {
			if value, exists := node.Annotations[key]; exists {
				quarantineAnnotations[key] = value
			}
		}
	}

	klog.V(5).Infof("Retrieved quarantine annotations for node %s from informer cache", nodeName)

	return quarantineAnnotations, nil
}

func (r *Reconciler) cleanupManualUncordonAnnotation(ctx context.Context, nodeName string,
	annotations map[string]string) {
	if annotations == nil {
		return
	}

	if _, hasManualUncordon := annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; hasManualUncordon {
		klog.Infof("Removing manual uncordon annotation from node %s before applying new quarantine", nodeName)

		// Remove the manual uncordon annotation before applying quarantine
		if err := r.k8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
			ctx,
			nodeName,
			nil,   // No taints to remove
			false, // Not uncordoning
			[]string{common.QuarantinedNodeUncordonedManuallyAnnotationKey}, // Remove manual uncordon annotation
			nil, // No labels to remove
			nil, // No labels to add
		); err != nil {
			klog.Errorf("Failed to remove manual uncordon annotation from node %s: %v", nodeName, err)
		}
	}
}

// handleManualUncordon handles the case when a node is manually uncordoned while having FQ annotations
func (r *Reconciler) handleManualUncordon(nodeName string) error {
	klog.Infof("Handling manual uncordon for node: %s", nodeName)

	annotations, err := r.getNodeQuarantineAnnotations(nodeName)
	if err != nil {
		klog.Errorf("Failed to get annotations for manually uncordoned node %s: %v", nodeName, err)
		return err
	}

	// Check which FQ annotations exist and need to be removed
	annotationsToRemove := []string{}

	var taintsToRemove []config.Taint

	// Check for taints annotation
	taintsKey := common.QuarantineHealthEventAppliedTaintsAnnotationKey
	if taintsStr, exists := annotations[taintsKey]; exists && taintsStr != "" {
		annotationsToRemove = append(annotationsToRemove, taintsKey)

		// Parse taints to remove them
		if err := json.Unmarshal([]byte(taintsStr), &taintsToRemove); err != nil {
			klog.Errorf("Failed to unmarshal taints for manually uncordoned node %s: %v", nodeName, err)
		}
	}

	// Remove all FQ-related annotations
	if _, exists := annotations[common.QuarantineHealthEventAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventAnnotationKey)
	}

	if _, exists := annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventIsCordonedAnnotationKey)
	}

	// Add the manual uncordon annotation
	newAnnotations := map[string]string{
		common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
	}

	// Atomically update the node: remove FQ annotations/taints/labels and add manual uncordon annotation
	if err := r.k8sClient.HandleManualUncordonCleanup(
		context.Background(),
		nodeName,
		taintsToRemove,
		annotationsToRemove,
		newAnnotations,
		[]string{statemanager.NVSentinelStateLabelKey},
	); err != nil {
		klog.Errorf("Failed to clean up manually uncordoned node %s: %v", nodeName, err)
		metrics.ProcessingErrors.WithLabelValues("manual_uncordon_cleanup_error").Inc()

		return err
	}

	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
	klog.Infof("Set currentQuarantinedNodes to 0 for manually uncordoned node: %s", nodeName)

	klog.Infof("Successfully handled manual uncordon for node %s", nodeName)

	return nil
}
