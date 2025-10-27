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

package v1alpha1_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	webhookv1alpha1 "github.com/nvidia/nvsentinel/janitor/pkg/webhook/v1alpha1"
)

var _ = Describe("RebootNodeValidator", func() {
	var (
		ctx       context.Context
		validator *webhookv1alpha1.RebootNodeValidator
		k8sClient client.Client
		testNode  *corev1.Node
		scheme    *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create scheme
		scheme = runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())

		// Create a test node
		testNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node-1",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		// Create fake client with the test node
		k8sClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testNode).
			Build()

		validator = &webhookv1alpha1.RebootNodeValidator{
			Client: k8sClient,
			Config: &config.RebootNodeControllerConfig{
				NodeExclusions: []metav1.LabelSelector{},
			},
		}
	})

	Describe("ValidateCreate", func() {
		Context("when creating a RebootNode for an existing node", func() {
			It("should allow creation", func() {
				rebootNode := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-reboot-1",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
						Force:    false,
					},
				}

				warnings, err := validator.ValidateCreate(ctx, rebootNode)
				Expect(err).ToNot(HaveOccurred())
				Expect(warnings).To(BeNil())
			})
		})

		Context("when creating a RebootNode for a non-existent node", func() {
			It("should reject creation", func() {
				rebootNode := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-reboot-2",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "non-existent-node",
						Force:    false,
					},
				}

				warnings, err := validator.ValidateCreate(ctx, rebootNode)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("does not exist"))
				Expect(warnings).To(BeNil())
			})
		})

		Context("when creating a RebootNode for a node with active reboot", func() {
			It("should reject creation", func() {
				// Create an active reboot
				activeReboot := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "active-reboot",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
					},
					Status: janitordgxcnvidiacomv1alpha1.RebootNodeStatus{
						StartTime: &metav1.Time{Time: metav1.Now().Time},
						// No CompletionTime - still active
					},
				}
				Expect(k8sClient.Create(ctx, activeReboot)).To(Succeed())

				// Try to create another reboot for the same node
				newReboot := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "new-reboot",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
					},
				}

				warnings, err := validator.ValidateCreate(ctx, newReboot)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("already has an active reboot"))
				Expect(warnings).To(BeNil())
			})
		})

		Context("when creating a RebootNode for a node with completed reboot", func() {
			It("should allow creation", func() {
				// Create a completed reboot
				completedReboot := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "completed-reboot",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
					},
					Status: janitordgxcnvidiacomv1alpha1.RebootNodeStatus{
						StartTime:      &metav1.Time{Time: metav1.Now().Time},
						CompletionTime: &metav1.Time{Time: metav1.Now().Time},
					},
				}
				Expect(k8sClient.Create(ctx, completedReboot)).To(Succeed())

				// Try to create another reboot for the same node (should succeed)
				newReboot := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "new-reboot",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
					},
				}

				warnings, err := validator.ValidateCreate(ctx, newReboot)
				Expect(err).ToNot(HaveOccurred())
				Expect(warnings).To(BeNil())
			})
		})
	})

	Describe("ValidateUpdate", func() {
		var existingReboot *janitordgxcnvidiacomv1alpha1.RebootNode

		BeforeEach(func() {
			existingReboot = &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "existing-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node-1",
					Force:    false,
				},
			}
			Expect(k8sClient.Create(ctx, existingReboot)).To(Succeed())
		})

		Context("when updating a RebootNode without changing nodeName", func() {
			It("should allow update", func() {
				updatedReboot := existingReboot.DeepCopy()
				updatedReboot.Spec.Force = true

				warnings, err := validator.ValidateUpdate(ctx, existingReboot, updatedReboot)
				Expect(err).ToNot(HaveOccurred())
				Expect(warnings).To(BeNil())
			})
		})

		Context("when trying to change nodeName", func() {
			It("should reject update", func() {
				updatedReboot := existingReboot.DeepCopy()
				updatedReboot.Spec.NodeName = "different-node"

				warnings, err := validator.ValidateUpdate(ctx, existingReboot, updatedReboot)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("nodeName cannot be changed"))
				Expect(warnings).To(BeNil())
			})
		})
	})

	Describe("ValidateDelete", func() {
		Context("when deleting a RebootNode", func() {
			It("should always allow deletion", func() {
				rebootNode := &janitordgxcnvidiacomv1alpha1.RebootNode{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-reboot",
					},
					Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
						NodeName: "test-node-1",
					},
				}

				warnings, err := validator.ValidateDelete(ctx, rebootNode)
				Expect(err).ToNot(HaveOccurred())
				Expect(warnings).To(BeNil())
			})
		})
	})
})
