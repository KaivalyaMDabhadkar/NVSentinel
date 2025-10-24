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

package informer

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
)

// other modules may also update the node, so we need to make sure that we retry on conflict
var customBackoff = wait.Backoff{
	Steps:    10,
	Duration: 10 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

type FaultQuarantineClient struct {
	Clientset                kubernetes.Interface
	DryRunMode               bool
	nodeInformer             *NodeInformer
	cordonedReasonLabelKey   string
	uncordonedReasonLabelKey string
}

func NewFaultQuarantineClient(kubeconfig string, dryRun bool) (*FaultQuarantineClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	client := &FaultQuarantineClient{
		Clientset:  clientset,
		DryRunMode: dryRun,
	}

	return client, nil
}

func (c *FaultQuarantineClient) GetK8sClient() kubernetes.Interface {
	return c.Clientset
}

func (c *FaultQuarantineClient) EnsureCircuitBreakerConfigMap(ctx context.Context,
	name, namespace string, initialStatus string) error {
	klog.Infof("Ensuring circuit breaker config map %s in namespace %s with initial status %s",
		name, namespace, initialStatus)

	cmClient := c.Clientset.CoreV1().ConfigMaps(namespace)

	_, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		klog.Infof("Circuit breaker config map %s in namespace %s already exists", name, namespace)
		return nil
	}

	if !errors.IsNotFound(err) {
		klog.Errorf("Error getting circuit breaker config map %s in namespace %s: %v", name, namespace, err)
		return fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"status": initialStatus},
	}

	_, err = cmClient.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Error creating circuit breaker config map %s in namespace %s: %v", name, namespace, err)
		return fmt.Errorf("failed to create config map %s in namespace %s: %w", name, namespace, err)
	}

	return nil
}

func (c *FaultQuarantineClient) GetTotalNodes(ctx context.Context) (int, error) {
	totalNodes, _, err := c.nodeInformer.GetNodeCounts()
	if err != nil {
		return 0, fmt.Errorf("failed to get node counts from informer: %w", err)
	}

	klog.V(4).Infof("Got %d total nodes from NodeInformer cache", totalNodes)

	return totalNodes, nil
}

func (c *FaultQuarantineClient) SetNodeInformer(nodeInformer NodeInfoProvider) {
	if ni, ok := nodeInformer.(*NodeInformer); ok {
		c.nodeInformer = ni
	}
}

func (c *FaultQuarantineClient) SetLabelKeys(cordonedReasonKey, uncordonedReasonKey string) {
	c.cordonedReasonLabelKey = cordonedReasonKey
	c.uncordonedReasonLabelKey = uncordonedReasonKey
}

func (c *FaultQuarantineClient) ReadCircuitBreakerState(ctx context.Context, name, namespace string) (string, error) {
	klog.Infof("Reading circuit breaker state from config map %s in namespace %s", name, namespace)

	cm, err := c.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	if cm.Data == nil {
		return "", nil
	}

	return cm.Data["status"], nil
}

func (c *FaultQuarantineClient) WriteCircuitBreakerState(ctx context.Context, name, namespace, status string) error {
	cmClient := c.Clientset.CoreV1().ConfigMaps(namespace)

	return retry.OnError(customBackoff, errors.IsConflict, func() error {
		cm, err := cmClient.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Error getting circuit breaker config map %s in namespace %s: %v", name, namespace, err)
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}

		cm.Data["status"] = status

		_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Error updating circuit breaker config map %s in namespace %s: %v", name, namespace, err)
		}

		return err
	})
}

func (c *FaultQuarantineClient) TaintAndCordonNodeAndSetAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isCordon bool,
	annotations map[string]string,
	labels map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if err := c.applyTaints(node, taints, nodename); err != nil {
			return fmt.Errorf("failed to apply taints to node %s: %w", nodename, err)
		}

		if shouldSkip := c.handleCordon(node, isCordon, nodename); shouldSkip {
			return nil
		}

		c.applyAnnotations(node, annotations, nodename)

		c.applyLabels(node, labels, nodename)

		return nil
	}

	return c.nodeInformer.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) applyTaints(node *v1.Node, taints []config.Taint, nodename string) error {
	if len(taints) == 0 {
		return nil
	}

	existingTaints := make(map[config.Taint]v1.Taint)
	for _, taint := range node.Spec.Taints {
		existingTaints[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = taint
	}

	for _, taintConfig := range taints {
		key := config.Taint{Key: taintConfig.Key, Value: taintConfig.Value, Effect: string(taintConfig.Effect)}

		if _, exists := existingTaints[key]; !exists {
			klog.Infof("Tainting node %s with taint config: %+v", nodename, taintConfig)
			existingTaints[key] = v1.Taint{
				Key:    taintConfig.Key,
				Value:  taintConfig.Value,
				Effect: v1.TaintEffect(taintConfig.Effect),
			}
		}
	}

	node.Spec.Taints = []v1.Taint{}
	for _, taint := range existingTaints {
		node.Spec.Taints = append(node.Spec.Taints, taint)
	}

	return nil
}

func (c *FaultQuarantineClient) handleCordon(node *v1.Node, isCordon bool, nodename string) bool {
	if !isCordon {
		return false
	}

	_, exist := node.Annotations[common.QuarantineHealthEventAnnotationKey]
	if node.Spec.Unschedulable {
		if exist {
			klog.Infof("Node %s already cordoned by FQM; skipping taint/annotation updates", nodename)
			return true
		}

		klog.Infof("Node %s is cordoned manually; applying FQM taints/annotations", nodename)
	} else {
		klog.Infof("Cordoning node %s", nodename)

		if !c.DryRunMode {
			node.Spec.Unschedulable = true
		}
	}

	return false
}

func (c *FaultQuarantineClient) applyAnnotations(node *v1.Node, annotations map[string]string, nodename string) {
	if len(annotations) == 0 {
		return
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	klog.Infof("Setting annotations %+v on node %s", annotations, nodename)

	for annotationKey, annotationValue := range annotations {
		node.Annotations[annotationKey] = annotationValue
	}
}

func (c *FaultQuarantineClient) applyLabels(node *v1.Node, labels map[string]string, nodename string) {
	if len(labels) == 0 {
		return
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	klog.Infof("Adding labels on node %s", nodename)

	for k, v := range labels {
		node.Labels[k] = v
	}
}

func (c *FaultQuarantineClient) UnTaintAndUnCordonNodeAndRemoveAnnotations(
	ctx context.Context,
	nodename string,
	taints []config.Taint,
	isUnCordon bool,
	annotationKeys []string,
	labelsToRemove []string,
	labels map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if shouldReturn := c.removeTaints(node, taints, nodename); shouldReturn {
			return nil
		}

		c.handleUncordon(node, isUnCordon, labels, nodename)

		c.removeAnnotations(node, annotationKeys, nodename)

		c.removeLabels(node, labelsToRemove, nodename)

		return nil
	}

	return c.nodeInformer.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) removeTaints(node *v1.Node, taints []config.Taint, nodename string) bool {
	if len(taints) == 0 {
		return false
	}

	taintsAlreadyPresentOnNodeMap := map[config.Taint]bool{}
	for _, taint := range node.Spec.Taints {
		taintsAlreadyPresentOnNodeMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] = true
	}

	taintsToActuallyRemove := []config.Taint{}

	for _, taintConfig := range taints {
		key := config.Taint{
			Key:    taintConfig.Key,
			Value:  taintConfig.Value,
			Effect: taintConfig.Effect,
		}

		found := taintsAlreadyPresentOnNodeMap[key]
		if !found {
			klog.Infof("Node %s already does not have the taint: %+v", nodename, taintConfig)
		} else {
			taintsToActuallyRemove = append(taintsToActuallyRemove, taintConfig)
		}
	}

	if len(taintsToActuallyRemove) == 0 {
		return true
	}

	klog.Infof("Untainting node %s with taint config: %+v", nodename, taintsToActuallyRemove)

	c.removeNodeTaints(node, taintsToActuallyRemove)

	return false
}

func (c *FaultQuarantineClient) handleUncordon(
	node *v1.Node, isUnCordon bool, labels map[string]string, nodename string,
) {
	if !isUnCordon {
		return
	}

	klog.Infof("Uncordoning node %s", nodename)

	if !c.DryRunMode {
		node.Spec.Unschedulable = false
	}

	if len(labels) > 0 {
		c.applyLabels(node, labels, nodename)

		uncordonReason := node.Labels[c.cordonedReasonLabelKey]

		if uncordonReason != "" {
			if len(uncordonReason) > 55 {
				uncordonReason = uncordonReason[:55]
			}

			node.Labels[c.uncordonedReasonLabelKey] = uncordonReason + "-removed"
		}
	}
}

func (c *FaultQuarantineClient) removeAnnotations(node *v1.Node, annotationKeys []string, nodename string) {
	if len(annotationKeys) == 0 || node.Annotations == nil {
		return
	}

	for _, annotationKey := range annotationKeys {
		klog.Infof("Removing annotation key %s from node %s", annotationKey, nodename)
		delete(node.Annotations, annotationKey)
	}
}

func (c *FaultQuarantineClient) removeLabels(node *v1.Node, labelsToRemove []string, nodename string) {
	if len(labelsToRemove) == 0 {
		return
	}

	for _, labelKey := range labelsToRemove {
		klog.Infof("Removing label key %s from node %s", labelKey, nodename)
		delete(node.Labels, labelKey)
	}
}

// UpdateNodeAnnotations updates only the specified annotations on a node without affecting other properties
// Uses eventual consistency from the local node informer
func (c *FaultQuarantineClient) UpdateNodeAnnotations(
	ctx context.Context,
	nodename string,
	annotations map[string]string,
) error {
	err := c.nodeInformer.UpdateNodeAnnotations(ctx, nodename, annotations)
	if err != nil {
		klog.Errorf("Failed to update annotations for node %s: %v", nodename, err)
		return fmt.Errorf("failed to update annotations for node %s: %w", nodename, err)
	}

	klog.Infof("Successfully updated annotations for node %s", nodename)

	return nil
}

// HandleManualUncordonCleanup atomically removes FQ annotations/taints/labels and adds manual uncordon annotation
// This is used when a node is manually uncordoned while having FQ quarantine state
func (c *FaultQuarantineClient) HandleManualUncordonCleanup(
	ctx context.Context,
	nodename string,
	taintsToRemove []config.Taint,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
	labelsToRemove []string,
) error {
	updateFn := c.createManualUncordonUpdateFn(taintsToRemove, annotationsToRemove, annotationsToAdd, labelsToRemove)

	return c.nodeInformer.UpdateNode(ctx, nodename, updateFn)
}

func (c *FaultQuarantineClient) createManualUncordonUpdateFn(
	taintsToRemove []config.Taint,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
	labelsToRemove []string,
) func(*v1.Node) error {
	return func(node *v1.Node) error {
		c.removeNodeTaints(node, taintsToRemove)
		c.updateNodeAnnotationsForManualUncordon(node, annotationsToRemove, annotationsToAdd)
		c.removeNodeLabels(node, labelsToRemove)

		return nil
	}
}

func (c *FaultQuarantineClient) removeNodeTaints(node *v1.Node, taintsToRemove []config.Taint) {
	if len(taintsToRemove) == 0 {
		return
	}

	taintsToRemoveMap := make(map[config.Taint]bool)
	for _, taint := range taintsToRemove {
		taintsToRemoveMap[taint] = true
	}

	newTaints := []v1.Taint{}

	for _, taint := range node.Spec.Taints {
		if !taintsToRemoveMap[config.Taint{Key: taint.Key, Value: taint.Value, Effect: string(taint.Effect)}] {
			newTaints = append(newTaints, taint)
		}
	}

	node.Spec.Taints = newTaints
}

func (c *FaultQuarantineClient) updateNodeAnnotationsForManualUncordon(
	node *v1.Node,
	annotationsToRemove []string,
	annotationsToAdd map[string]string,
) {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	for _, key := range annotationsToRemove {
		delete(node.Annotations, key)
	}

	for key, value := range annotationsToAdd {
		node.Annotations[key] = value
	}
}

func (c *FaultQuarantineClient) removeNodeLabels(node *v1.Node, labelsToRemove []string) {
	if node.Labels == nil {
		return
	}

	for _, key := range labelsToRemove {
		delete(node.Labels, key)
	}
}
