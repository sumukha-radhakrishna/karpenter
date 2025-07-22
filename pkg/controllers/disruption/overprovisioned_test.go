/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package disruption_test

import (
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Overprovisioned", func() {
	var nodePool *v1.NodePool
	var nodeClaims []*v1.NodeClaim
	var nodes []*corev1.Node

	BeforeEach(func() {
		// Create a static nodepool with replicas
		nodePool = test.StaticNodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Replicas:   lo.ToPtr(int64(1)), // Static nodepool with 1 desired replica
				Disruption: v1.Disruption{},
			},
		})

		nodeClaims, nodes = test.NodeClaimsAndNodes(1, v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:        nodePool.Name,
					v1.NodeInitializedLabelKey: "true",
				},
			},
			Status: v1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10"),
					corev1.ResourceMemory: resource.MustParse("1000Mi"),
					corev1.ResourcePods:   resource.MustParse("2"),
				},
			},
		})
	})

	AfterEach(func() {
		cluster.Reset()
	})

	Context("Overprovision", func() {
		It("should correctly report elligible overprovisioned node", func() {
			// Create extra nodeclaim and node for nodepool
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(cmds[0].Candidates[0].Node))

			// 1 nodeclaim should exist
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})

		It("should not disrupt when there are no nodes", func() {
			// Apply only the nodepool without any nodes
			ExpectApplied(ctx, env.Client, nodePool)

			// Update cluster state with no nodes
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{}, []*v1.NodeClaim{})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Queue should not have any candidate for disruption
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// 1 node should exist
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
		})

		It("should not disrupt when node count equals desired replicas", func() {
			// Apply nodepool with exactly 1 replica and 1 node (matching the BeforeEach setup)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0])

			// Create a pod to make the node non-empty
			pod := test.Pod(test.PodOptions{})
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, nodes[0])

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Queue should not have any candidate for disruption
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))

			// Node should still exist
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaims[0])
		})

		It("should disrupt oldest node when all nonPending pods cannot be scheduled", func() {
			// Create two extra nodes to exceed the desired replica count
			extraNodeClaim1, extraNode1 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("1"),
					},
				},
			})

			extraNodeClaim2, extraNode2 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("1"),
					},
				},
			})

			// Create pods
			pods := test.Pods(3, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("9"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0], pods[0])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])

			// Give a sec for NC to be created to not run into flaky tests
			time.Sleep(1 * time.Second)

			ExpectApplied(ctx, env.Client, nodePool, extraNodeClaim1, extraNodeClaim2, extraNode1, extraNode2, pods[1], pods[2])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{extraNode1, extraNode2}, []*v1.NodeClaim{extraNodeClaim1, extraNodeClaim2})
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode1)
			ExpectManualBinding(ctx, env.Client, pods[2], extraNode2)

			ExpectSingletonReconciled(ctx, disruptionController)

			// Should disrupt the oldest node since pods cannot be rescheduled
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			// Process the disruption command
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			// The oldest node should be disrupted
			Expect(cmds[0].Candidates[0].NodeClaim.Name).To(Equal(nodeClaims[0].Name))
		})

		It("should disrupt node whose pods can be scheduled", func() {
			// Create one extra node to exceed the desired replica count
			// Add a unique label to the extra node
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       "test-zone-2",
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})
			nodeClaims[0].Labels[corev1.LabelTopologyZone] = "test-zone-1" // Set the label of the first node to a different zone]
			nodes[0].Labels[corev1.LabelTopologyZone] = "test-zone-1"

			// Create pod1 with nodename set
			pod1 := test.Pod(test.PodOptions{
				NodeSelector: map[string]string{
					corev1.LabelTopologyZone: "test-zone-1",
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), // Small CPU requirement
					},
				},
			})

			// Create pod2 with small CPU requirement that can be rescheduled to any node
			pod2 := test.Pod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), // Small CPU requirement - can be rescheduled
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodes[0], pod1)
			ExpectManualBinding(ctx, env.Client, pod1, nodes[0])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0]}, []*v1.NodeClaim{nodeClaims[0]})

			ExpectApplied(ctx, env.Client, nodePool, extraNodeClaim, extraNode, pod2)
			ExpectManualBinding(ctx, env.Client, pod2, extraNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{extraNode}, []*v1.NodeClaim{extraNodeClaim})

			ExpectSingletonReconciled(ctx, disruptionController)

			// Should disrupt the node with pod2 since pod2 can be rescheduled but pod1 cannot (due to node selector)
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			// Process the disruption command
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
			// Should disrupt extraNodeClaim (the node with pod2 that can be rescheduled)
			Expect(cmds[0].Candidates[0].NodeClaim.Name).To(Equal(extraNodeClaim.Name))
		})

		It("should only disrupt nodes from static nodepools", func() {
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       "test-zone-2",
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})
			// Create a dynamic nodepool (without replicas)
			dynamicNodePool := test.NodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					// No Replicas field - this makes it dynamic
					Disruption: v1.Disruption{},
				},
			})

			// Create extra nodes for the dynamic nodepool
			extraDynamicNodeClaims, extraDynamicNodes := test.NodeClaimsAndNodes(1, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            dynamicNodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			pods := test.Pods(2, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), // Small CPU requirement
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pods[0], pods[1])
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			ExpectApplied(ctx, env.Client, dynamicNodePool, extraDynamicNodeClaims[0], extraDynamicNodes[0])
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{extraDynamicNodes[0]}, []*v1.NodeClaim{extraDynamicNodeClaims[0]})

			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not disrupt dynamic nodepool nodes with overprovisioned reason
			// Nodes can be disrupted for other reasons, but not for being overprovisioned
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(2))
		})

		It("should skip empty candidates and handle them via emptiness controller", func() {
			// Create extra empty nodes (no pods)
			extraNodeClaim1, extraNode1 := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			// Apply nodes without any pods (empty nodes)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim1, nodes[0], extraNode1)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode1}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim1})

			fakeClock.Step(10 * time.Minute)

			wg := sync.WaitGroup{}
			ExpectToWait(fakeClock, &wg)
			ExpectSingletonReconciled(ctx, disruptionController)
			wg.Wait()

			metric, found := FindMetricWithLabelValues("karpenter_nodepools_allowed_disruptions", map[string]string{
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetGauge().GetValue()).To(BeNumerically("==", 1))

			// All nodes should still exist (emptiness controller would handle them)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
		})

		It("should not ignore nodes with the karpenter.sh/do-not-disrupt annotation", func() {
			// Create extra node to exceed desired replicas
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})

			// Add do-not-disrupt annotation to the extra node
			extraNode.Annotations = lo.Assign(extraNode.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})

			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pods[0], pods[1])

			// Bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not disrupt nodes with do-not-disrupt annotation
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
		})

		It("should disrupt nodes that have pods with the karpenter.sh/do-not-disrupt annotation", func() {
			// Create extra node to exceed desired replicas
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})

			// Create pods with do-not-disrupt annotation
			pod1 := test.Pod(test.PodOptions{})
			pod2 := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1.DoNotDisruptAnnotationKey: "true",
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pod1, pod2)

			// Bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pod1, nodes[0])
			ExpectManualBinding(ctx, env.Client, pod2, extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not disrupt nodes with pods that have do-not-disrupt annotation
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
		})

		It("should disrupt nodes that do not have pods with blocking PDBs when TerminationGracePeriod is set", func() {
			// Create extra node to exceed desired replicas
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				},
			})

			nodeClaims[0].Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}

			podLabels := map[string]string{"test": "value"}
			pod1 := test.Pod(test.PodOptions{})
			pod2 := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
			})

			// Create a PDB that blocks disruption
			budget := test.PodDisruptionBudget(test.PDBOptions{
				Labels: podLabels,
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pod1, pod2, budget)

			// Bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pod1, nodes[0])
			ExpectManualBinding(ctx, env.Client, pod2, extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not disrupt nodes with pods that have do-not-disrupt annotation
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
			// Should disrupt NC that have pods without PDBs
			Expect(cmds[0].Candidates[0].NodeClaim.Name).To(Equal(nodeClaims[0].Name))
		})

		It("should disrupt nodes if all candidates have pods with blocking PDBs and terminationGracePeriod is set", func() {
			// Create extra node to exceed desired replicas
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})
			nodeClaims[0].Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}
			extraNodeClaim.Spec.TerminationGracePeriod = &metav1.Duration{Duration: 5 * time.Minute}

			podLabels := map[string]string{"test": "value"}
			pods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
			})

			// Create a PDB that blocks disruption
			budget := test.PodDisruptionBudget(test.PDBOptions{
				Labels: podLabels,
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pods[0], pods[1], budget)

			// Bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			// Should not disrupt nodes with pods that have do-not-disrupt annotation
			ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, 1, map[string]string{
				"decision":          "delete",
				metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
			})

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)

			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(1))
		})

		It("should not disrupt nodes if all candidates have pods with blocking PDBs and terminationGracePeriod is not set", func() {
			// Create extra node to exceed desired replicas
			extraNodeClaim, extraNode := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: leastExpensiveInstance.Name,
						v1.CapacityTypeLabelKey:        leastExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
						corev1.LabelTopologyZone:       leastExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			podLabels := map[string]string{"test": "value"}
			pods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
			})

			// Create a PDB that blocks disruption
			budget := test.PodDisruptionBudget(test.PDBOptions{
				Labels: podLabels,
			})

			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], extraNodeClaim, nodes[0], extraNode, pods[0], pods[1], budget)

			// Bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
			ExpectManualBinding(ctx, env.Client, pods[1], extraNode)

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], extraNode}, []*v1.NodeClaim{nodeClaims[0], extraNodeClaim})

			fakeClock.Step(10 * time.Minute)
			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(0))
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(2))
		})

		It("should deprovision multiple nodeclaims to meet desired replicas", func() {
			// Set desired replicas to 2
			nodePool.Spec.Replicas = lo.ToPtr(int64(2))

			// Create 3 additional nodeclaims (total 4, desired 2, so 2 should be deprovisioned)
			nodeClaims, nodes = test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:        nodePool.Name,
						v1.NodeInitializedLabelKey: "true",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			// Create pods with small resource requirements that can be rescheduled
			pods := test.Pods(4, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), // Small CPU requirement
					},
				},
			})

			// Apply all resources
			ExpectApplied(ctx, env.Client, nodePool, nodeClaims[0], nodeClaims[1], nodeClaims[2], nodeClaims[3], nodes[0], nodes[1], nodes[2], nodes[3])

			for _, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
			}

			// Bind pods to nodes (one pod per node)
			for i, pod := range pods {
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*corev1.Node{nodes[0], nodes[1], nodes[2], nodes[3]}, []*v1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2], nodeClaims[3]})
			Expect(cluster.Nodes()).To(HaveLen(4))
			nodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, nodeClaims)).To(Succeed())
			Expect(nodeClaims.Items).To(HaveLen(4))

			// Run disruption controller multiple times to deprovision excess nodes
			// Since we have 4 nodes and want 2, we need to deprovision 2 nodes
			for i := 0; i < 2; i++ {
				ExpectSingletonReconciled(ctx, disruptionController)

				ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, float64(i+1), map[string]string{
					"decision":          "delete",
					metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
				})

				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
				ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(cmds[0].Candidates[0].Node))
			}

			// After deprovisioning, we should have exactly 2 nodeclaims (matching desired replicas)
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(Equal(2))
		})

		It("should deprovision multiple nodeclaims across multiple nodepools to meet desired replicas", func() {
			// Create a second nodepool with replicas set to 1
			nodePool2 := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas:   lo.ToPtr(int64(1)), // Static nodepool with 1 desired replica
					Disruption: v1.Disruption{},
				},
			})

			// Create a third nodepool with replicas set to 2
			nodePool3 := test.StaticNodePool(v1.NodePool{
				Spec: v1.NodePoolSpec{
					Replicas:   lo.ToPtr(int64(2)), // Static nodepool with 2 desired replicas
					Disruption: v1.Disruption{},
				},
			})

			// Set first nodepool to have 2 replicas
			nodePool.Spec.Replicas = lo.ToPtr(int64(2))

			// Create nodeclaims for first nodepool (3 nodeclaims, desired 2, so 1 should be deprovisioned)
			nodeClaims, nodes = test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:        nodePool.Name,
						v1.NodeInitializedLabelKey: "true",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			// Create nodeclaims for second nodepool (3 nodeclaims, desired 1, so 2 should be deprovisioned)
			nodePool2Claims, nodePool2Nodes := test.NodeClaimsAndNodes(3, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:        nodePool2.Name,
						v1.NodeInitializedLabelKey: "true",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			// Create nodeclaims for third nodepool (4 nodeclaims, desired 2, so 2 should be deprovisioned)
			nodePool3Claims, nodePool3Nodes := test.NodeClaimsAndNodes(4, v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:        nodePool3.Name,
						v1.NodeInitializedLabelKey: "true",
					},
				},
				Status: v1.NodeClaimStatus{
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
						corev1.ResourcePods:   resource.MustParse("2"),
					},
				},
			})

			// Create pods with small resource requirements that can be rescheduled
			// Total: 10 pods for 10 nodes (3+3+4)
			pods := test.Pods(10, test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), // Small CPU requirement
					},
				},
			})

			// Apply all nodepools
			ExpectApplied(ctx, env.Client, nodePool, nodePool2, nodePool3)

			// Apply all nodeclaims and nodes
			allNodeClaims := append(append(nodeClaims, nodePool2Claims...), nodePool3Claims...)
			allNodes := append(append(nodes, nodePool2Nodes...), nodePool3Nodes...)

			for _, nc := range allNodeClaims {
				ExpectApplied(ctx, env.Client, nc)
			}
			for _, n := range allNodes {
				ExpectApplied(ctx, env.Client, n)
			}

			// Apply all pods
			for _, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
			}

			// Bind pods to nodes (one pod per node)
			for i, pod := range pods {
				ExpectManualBinding(ctx, env.Client, pod, allNodes[i])
			}

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, allNodes, allNodeClaims)

			// Verify initial state: 10 nodes total
			Expect(cluster.Nodes()).To(HaveLen(10))
			initialNodeClaims := &v1.NodeClaimList{}
			Expect(env.Client.List(ctx, initialNodeClaims)).To(Succeed())
			Expect(initialNodeClaims.Items).To(HaveLen(10))

			// Run disruption controller multiple times to deprovision excess nodes
			// Total excess: (3-2) + (3-1) + (4-2) = 1 + 2 + 2 = 5 nodes to deprovision
			totalExpectedDeletions := 5
			for i := 0; i < totalExpectedDeletions; i++ {
				ExpectSingletonReconciled(ctx, disruptionController)

				ExpectMetricCounterValue(disruption.DecisionsPerformedTotal, float64(i+1), map[string]string{
					"decision":          "delete",
					metrics.ReasonLabel: strings.ToLower(string(v1.DisruptionReasonOverprovisioned)),
				})

				cmds := queue.GetCommands()
				Expect(cmds).To(HaveLen(1))
				ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
				ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(cmds[0].Candidates[0].Node))
			}

			// After deprovisioning, we should have exactly 5 nodeclaims total (2+1+2 = 5)
			finalNodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(len(finalNodeClaims)).To(Equal(5))

			// Verify the correct number of nodeclaims per nodepool
			nodePool1FinalClaims := lo.Filter(finalNodeClaims, func(nc *v1.NodeClaim, _ int) bool {
				return nc.Labels[v1.NodePoolLabelKey] == nodePool.Name
			})
			nodePool2FinalClaims := lo.Filter(finalNodeClaims, func(nc *v1.NodeClaim, _ int) bool {
				return nc.Labels[v1.NodePoolLabelKey] == nodePool2.Name
			})
			nodePool3FinalClaims := lo.Filter(finalNodeClaims, func(nc *v1.NodeClaim, _ int) bool {
				return nc.Labels[v1.NodePoolLabelKey] == nodePool3.Name
			})

			Expect(len(nodePool1FinalClaims)).To(Equal(2)) // Should have 2 (desired replicas)
			Expect(len(nodePool2FinalClaims)).To(Equal(1)) // Should have 1 (desired replicas)
			Expect(len(nodePool3FinalClaims)).To(Equal(2)) // Should have 2 (desired replicas)
		})
	})
})
