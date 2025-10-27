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

package v1alpha1

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
)

// SetupRebootNodeWebhookWithManager registers the webhook for RebootNode.
func SetupRebootNodeWebhookWithManager(mgr ctrl.Manager, cfg *config.RebootNodeControllerConfig) error {
	validator := &RebootNodeValidator{
		Client: mgr.GetClient(),
		Config: cfg,
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&janitordgxcnvidiacomv1alpha1.RebootNode{}).
		WithValidator(validator).
		Complete()
}

// +kubebuilder:webhook:path=/validate-janitor-dgxc-nvidia-com-v1alpha1-rebootnode,mutating=false,failurePolicy=fail,sideEffects=None,groups=janitor.dgxc.nvidia.com,resources=rebootnodes,verbs=create;update,versions=v1alpha1,name=vrebootnode-v1alpha1.kb.io,admissionReviewVersions=v1
// nolint:lll

// RebootNodeValidator validates RebootNode resources.
// +kubebuilder:object:generate=false
type RebootNodeValidator struct {
	Client client.Client
	Config *config.RebootNodeControllerConfig
}

var _ webhook.CustomValidator = &RebootNodeValidator{}

// ValidateCreate implements webhook.CustomValidator.
func (v *RebootNodeValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	logger := log.FromContext(ctx)

	rebootNode, ok := obj.(*janitordgxcnvidiacomv1alpha1.RebootNode)
	if !ok {
		return nil, fmt.Errorf("expected RebootNode object but got %T", obj)
	}

	logger.Info("Validating RebootNode creation", "name", rebootNode.Name, "nodeName", rebootNode.Spec.NodeName)

	// Validate that the node exists and get the node object
	node, err := v.validateNodeExists(ctx, rebootNode.Spec.NodeName)
	if err != nil {
		logger.Info("Node validation failed", "nodeName", rebootNode.Spec.NodeName, "error", err.Error())
		return nil, err
	}

	// Check if the node is in the exclusions list
	if err := v.validateNodeNotInExclusions(node); err != nil {
		logger.Info("Node exclusion validation failed", "nodeName", rebootNode.Spec.NodeName, "error", err.Error())
		return nil, err
	}

	// Check if there's already an active RebootNode for this node
	if err := v.validateNoActiveReboot(ctx, rebootNode.Spec.NodeName); err != nil {
		logger.Info("Active reboot validation failed", "nodeName", rebootNode.Spec.NodeName, "error", err.Error())
		return nil, err
	}

	logger.Info("RebootNode validation passed", "name", rebootNode.Name)

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *RebootNodeValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	logger := log.FromContext(ctx)

	newRebootNode, ok := newObj.(*janitordgxcnvidiacomv1alpha1.RebootNode)
	if !ok {
		return nil, fmt.Errorf("expected RebootNode object but got %T", newObj)
	}

	oldRebootNode, ok := oldObj.(*janitordgxcnvidiacomv1alpha1.RebootNode)
	if !ok {
		return nil, fmt.Errorf("expected RebootNode object but got %T", oldObj)
	}

	logger.Info("Validating RebootNode update", "name", newRebootNode.Name)

	// Prevent changes to nodeName
	if oldRebootNode.Spec.NodeName != newRebootNode.Spec.NodeName {
		return nil, fmt.Errorf("nodeName cannot be changed after creation")
	}

	// Validate that the node still exists
	if _, err := v.validateNodeExists(ctx, newRebootNode.Spec.NodeName); err != nil {
		logger.Info("Node validation failed", "nodeName", newRebootNode.Spec.NodeName, "error", err.Error())
		return nil, err
	}

	logger.Info("RebootNode update validation passed", "name", newRebootNode.Name)

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator.
func (v *RebootNodeValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	logger := log.FromContext(ctx)

	rebootNode, ok := obj.(*janitordgxcnvidiacomv1alpha1.RebootNode)
	if !ok {
		return nil, fmt.Errorf("expected RebootNode object but got %T", obj)
	}

	logger.Info("Validating RebootNode deletion", "name", rebootNode.Name)

	// Allow deletion - no additional validation needed

	return nil, nil
}

// validateNodeExists checks if the specified node exists in the cluster.
// Returns the node object if found.
func (v *RebootNodeValidator) validateNodeExists(ctx context.Context, nodeName string) (*corev1.Node, error) {
	if v.Client == nil {
		return nil, fmt.Errorf("kubernetes client not available for node validation")
	}

	var node corev1.Node
	if err := v.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return nil, fmt.Errorf("node '%s' does not exist in the cluster: %w", nodeName, err)
	}

	return &node, nil
}

// validateNodeNotInExclusions checks if the node matches any exclusion label selectors.
func (v *RebootNodeValidator) validateNodeNotInExclusions(node *corev1.Node) error {
	if v.Config == nil || len(v.Config.NodeExclusions) == 0 {
		// No exclusions configured, allow all nodes
		return nil
	}

	for _, exclusion := range v.Config.NodeExclusions {
		selector, err := metav1.LabelSelectorAsSelector(&exclusion)
		if err != nil {
			return fmt.Errorf("invalid exclusion label selector '%s': %w", exclusion.String(), err)
		}

		if selector.Matches(labels.Set(node.Labels)) {
			return fmt.Errorf(
				"node '%s' is excluded from janitor operations due to label matching exclusion selector '%s'",
				node.Name,
				selector.String(),
			)
		}
	}

	return nil
}

// validateNoActiveReboot checks if there's already an active reboot for the node.
func (v *RebootNodeValidator) validateNoActiveReboot(ctx context.Context, nodeName string) error {
	if v.Client == nil {
		return fmt.Errorf("kubernetes client not available for reboot validation")
	}

	var rebootNodeList janitordgxcnvidiacomv1alpha1.RebootNodeList
	if err := v.Client.List(ctx, &rebootNodeList); err != nil {
		return fmt.Errorf("failed to list RebootNode resources: %w", err)
	}

	for _, rebootNode := range rebootNodeList.Items {
		// Skip if it's for a different node
		if rebootNode.Spec.NodeName != nodeName {
			continue
		}

		// Skip if it's already completed
		if rebootNode.Status.CompletionTime != nil {
			continue
		}

		// Found an active reboot for this node
		return fmt.Errorf(
			"node '%s' already has an active reboot in progress (RebootNode: %s)",
			nodeName,
			rebootNode.Name,
		)
	}

	return nil
}
