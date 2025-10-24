// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package informer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	// quarantineAnnotationIndexName is the index name for quarantined nodes
	quarantineAnnotationIndexName = "quarantineAnnotation"
)

// NodeInfoProvider defines the interface for getting node counts.
type NodeInfoProvider interface {
	// GetNodeCounts returns the total number of nodes and the number of those nodes
	// that are currently quarantined (have quarantine annotation).
	GetNodeCounts() (totalNodes int, quarantinedNodesMap map[string]bool, err error)
	// HasSynced returns true if the underlying informer cache has synced.
	HasSynced() bool
}

// NodeInformer watches specific nodes and provides counts.
type NodeInformer struct {
	clientset      kubernetes.Interface
	informer       cache.SharedIndexInformer
	lister         corelisters.NodeLister
	informerSynced cache.InformerSynced

	// Mutex protects access to the counts below
	mutex      sync.RWMutex
	totalNodes int

	// operationMutex provides per-node locking for thread-safe updates
	operationMutex sync.Map // map[string]*sync.Mutex

	// onQuarantinedNodeDeleted is called when a quarantined node with annotations is deleted
	onQuarantinedNodeDeleted func(nodeName string)

	// onManualUncordon is called when a node is manually uncordoned while having FQ annotations
	onManualUncordon func(nodeName string) error
}

// Lister returns the informer's node lister.
func (ni *NodeInformer) Lister() corelisters.NodeLister {
	return ni.lister
}

// GetInformer returns the underlying SharedIndexInformer.
func (ni *NodeInformer) GetInformer() cache.SharedIndexInformer {
	return ni.informer
}

// NewNodeInformer creates a new NodeInformer that watches all nodes.
func NewNodeInformer(clientset kubernetes.Interface,
	resyncPeriod time.Duration) (*NodeInformer, error) {
	ni := &NodeInformer{
		clientset: clientset,
	}

	informerFactory := informers.NewSharedInformerFactory(clientset, resyncPeriod)

	nodeInformerObj := informerFactory.Core().V1().Nodes()
	ni.informer = nodeInformerObj.Informer()
	ni.lister = nodeInformerObj.Lister()
	ni.informerSynced = nodeInformerObj.Informer().HasSynced

	err := ni.informer.AddIndexers(cache.Indexers{
		quarantineAnnotationIndexName: quarantineAnnotationIndexFunc,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add quarantine annotation indexer: %w", err)
	}

	_, err = ni.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ni.handleAddNode,
		UpdateFunc: ni.handleUpdateNodeWrapper,
		DeleteFunc: ni.handleDeleteNode,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler: %w", err)
	}

	klog.Info("NodeInformer created, watching all nodes")

	return ni, nil
}

// Run starts the informer and waits for cache sync.
func (ni *NodeInformer) Run(stopCh <-chan struct{}) error {
	klog.Info("Starting NodeInformer")

	go ni.informer.Run(stopCh)

	klog.Info("Waiting for NodeInformer cache to sync...")

	if ok := cache.WaitForCacheSync(stopCh, ni.informerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("NodeInformer cache synced")

	return nil
}

// HasSynced checks if the informer's cache has been synchronized.
func (ni *NodeInformer) HasSynced() bool {
	return ni.informerSynced()
}

// WaitForSync waits for the informer cache to sync with context cancellation support.
func (ni *NodeInformer) WaitForSync(ctx context.Context) bool {
	klog.Info("Waiting for NodeInformer cache to sync...")

	if ok := cache.WaitForCacheSync(ctx.Done(), ni.informerSynced); !ok {
		klog.Warning("NodeInformer cache sync failed or context cancelled")
		return false
	}

	klog.Info("NodeInformer cache synced")

	return true
}

// quarantineAnnotationIndexFunc is the indexer function for quarantined nodes
func quarantineAnnotationIndexFunc(obj interface{}) ([]string, error) {
	node, ok := obj.(*v1.Node)
	if !ok {
		return nil, fmt.Errorf("expected node object, got %T", obj)
	}

	if _, exists := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		return []string{"quarantined"}, nil
	}

	return []string{}, nil
}

// GetNodeCounts returns the current counts of total nodes and quarantined nodes.
func (ni *NodeInformer) GetNodeCounts() (totalNodes int, quarantinedNodesMap map[string]bool, err error) {
	if !ni.HasSynced() {
		return 0, nil, fmt.Errorf("node informer cache not synced yet")
	}

	ni.mutex.RLock()
	total := ni.totalNodes
	ni.mutex.RUnlock()

	// Use indexer to efficiently get quarantined nodes
	quarantinedObjs, err := ni.informer.GetIndexer().ByIndex(quarantineAnnotationIndexName, "quarantined")
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get quarantined nodes from index: %w", err)
	}

	quarantinedMap := make(map[string]bool, len(quarantinedObjs))

	for _, obj := range quarantinedObjs {
		if node, ok := obj.(*v1.Node); ok {
			quarantinedMap[node.Name] = true
		}
	}

	return total, quarantinedMap, nil
}

// GetNode retrieves a node from the informer's cache.
func (ni *NodeInformer) GetNode(name string) (*v1.Node, error) {
	return ni.lister.Get(name)
}

// ListNodes lists all nodes from the informer's cache.
func (ni *NodeInformer) ListNodes() ([]*v1.Node, error) {
	return ni.lister.List(labels.Everything())
}

// handleAddNode recalculates counts when a node is added.
func (ni *NodeInformer) handleAddNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		klog.Errorf("Add event: expected Node object, got %T", obj)
		return
	}

	klog.V(4).Infof("Node added: %s", node.Name)

	ni.mutex.Lock()
	ni.totalNodes++
	ni.mutex.Unlock()
}

// handleUpdateNodeWrapper is a wrapper for handleUpdateNode that converts interface{} to *v1.Node.
func (ni *NodeInformer) handleUpdateNodeWrapper(oldObj, newObj interface{}) {
	oldNode, okOld := oldObj.(*v1.Node)
	newNode, okNew := newObj.(*v1.Node)

	if !okOld || !okNew {
		klog.Errorf("Update event: expected Node objects, got %T and %T", oldObj, newObj)
		return
	}

	ni.handleUpdateNode(oldNode, newNode)
}

// detectAndHandleManualUncordon checks if a node was manually uncordoned and handles it
func (ni *NodeInformer) detectAndHandleManualUncordon(oldNode, newNode *v1.Node) bool {
	// Check if node transitioned from unschedulable to schedulable
	if !(oldNode.Spec.Unschedulable && !newNode.Spec.Unschedulable) {
		return false
	}

	// Check if node has FQ quarantine annotations
	_, hasCordonAnnotation := newNode.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if !hasCordonAnnotation {
		return false
	}

	klog.Infof("Detected manual uncordon of FQ-quarantined node: %s", newNode.Name)

	// Call the manual uncordon handler if registered
	if ni.onManualUncordon != nil {
		if err := ni.onManualUncordon(newNode.Name); err != nil {
			klog.Errorf("Failed to handle manual uncordon for node %s: %v", newNode.Name, err)
		}
	} else {
		klog.Warningf("Manual uncordon callback not registered for node %s - manual uncordon will not be handled",
			newNode.Name)
	}

	return true
}

// handleUpdateNode recalculates counts when a node is updated.
func (ni *NodeInformer) handleUpdateNode(oldNode, newNode *v1.Node) {
	// Check for manual uncordon and handle it
	if ni.detectAndHandleManualUncordon(oldNode, newNode) {
		// Return early as the manual uncordon handler will take care of everything
		return
	}

	if oldNode.Spec.Unschedulable != newNode.Spec.Unschedulable ||
		oldNode.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] !=
			newNode.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] {
		klog.V(4).Infof("Node updated: %s (Unschedulable: %t -> %t)", newNode.Name,
			oldNode.Spec.Unschedulable, newNode.Spec.Unschedulable)
	} else {
		klog.V(4).Infof("Node update ignored (no relevant change): %s", newNode.Name)
	}
}

// SetOnQuarantinedNodeDeletedCallback sets the callback function for when a quarantined node is deleted
func (ni *NodeInformer) SetOnQuarantinedNodeDeletedCallback(callback func(nodeName string)) {
	ni.onQuarantinedNodeDeleted = callback
}

// SetOnManualUncordonCallback sets the callback function for when a node is manually uncordoned
func (ni *NodeInformer) SetOnManualUncordonCallback(callback func(nodeName string) error) {
	ni.onManualUncordon = callback
}

// handleDeleteNode recalculates counts when a node is deleted.
func (ni *NodeInformer) handleDeleteNode(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Delete event: expected Node object or DeletedFinalStateUnknown, got %T", obj)
			return
		}

		node, ok = tombstone.Obj.(*v1.Node)
		if !ok {
			klog.Errorf("Delete event: DeletedFinalStateUnknown contained non-Node object %T", tombstone.Obj)
			return
		}
	}

	klog.Infof("Node deleted: %s", node.Name)

	ni.mutex.Lock()
	ni.totalNodes--
	ni.mutex.Unlock()

	// Check if the node had the quarantine annotation
	_, hadQuarantineAnnotation := node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]

	// If the node was quarantined and had the annotation, call the callback so that
	// currentQuarantinedNodes metric is decremented
	if hadQuarantineAnnotation && ni.onQuarantinedNodeDeleted != nil {
		ni.onQuarantinedNodeDeleted(node.Name)
	}
}

// UpdateNode atomically updates a node using eventual consistency.
// The updateFn is applied to the latest node version from the API server.
// Per-node locking ensures thread-safe updates on the same node.
func (ni *NodeInformer) UpdateNode(ctx context.Context, nodeName string, updateFn func(*v1.Node) error) error {
	// Lock per-node to ensure atomic operations on the same node
	mu, _ := ni.operationMutex.LoadOrStore(nodeName, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	return retry.OnError(retry.DefaultBackoff, errors.IsConflict, func() error {
		// Get the latest version from API server
		node, err := ni.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get node: %w", err)
		}

		if err := updateFn(node); err != nil {
			return fmt.Errorf("update function failed: %w", err)
		}

		_, err = ni.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update node: %w", err)
		}

		klog.V(3).Infof("Updated node %s (eventual consistency)", nodeName)

		return nil
	})
}

// UpdateNodeAnnotations atomically updates node annotations using eventual consistency.
func (ni *NodeInformer) UpdateNodeAnnotations(
	ctx context.Context,
	nodeName string,
	annotations map[string]string,
) error {
	updateFn := func(node *v1.Node) error {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		for key, value := range annotations {
			node.Annotations[key] = value
		}

		klog.V(3).Infof("Updating annotations for node %s: %v", nodeName, annotations)

		return nil
	}

	return ni.UpdateNode(ctx, nodeName, updateFn)
}
