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

package disruption

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	provisioningdynamic "sigs.k8s.io/karpenter/pkg/controllers/provisioning/dynamic"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

// Overprovisioned is a subreconciler that deletes candidates that extend beyond a static NodePool's desired replicas.
type Overprovisioned struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioningdynamic.Provisioner
	recorder    events.Recorder
}

func NewOverprovisioned(kubeClient client.Client, cluster *state.Cluster, controller *provisioningdynamic.Provisioner, recorder events.Recorder) *Overprovisioned {
	return &Overprovisioned{
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: controller,
		recorder:    recorder,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (o *Overprovisioned) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	if c.NodePool.Spec.Replicas == nil {
		return false
	}
	nodeQuantity := o.cluster.NodePoolResourcesFor(c.NodePool.Name)[resources.Node]
	return nodeQuantity.Value() > lo.FromPtr(c.NodePool.Spec.Replicas)
}

func (o *Overprovisioned) ComputeCommand(ctx context.Context, disruptionBudgetMapping map[string]int, candidates ...*Candidate) (Command, error) {
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.CreationTimestamp.Before(&candidates[j].NodeClaim.CreationTimestamp)
	})

	var nonReschedulableCandidates []*Candidate

	// For Scale-down we will not consider disruption-budgets
	// We will scale down static NodeClaims by choosing the best candidate each time
	for _, candidate := range candidates {
		// We dont expect candidates to be empty as emptiness would have already handled it
		// If not then lets leave it for emptiness as it supersedes overprovisioned scale down
		if len(candidate.reschedulablePods) == 0 {
			continue
		}

		results, err := SimulateScheduling(ctx, o.kubeClient, o.cluster, o.provisioner, candidate)
		if err != nil {
			nonReschedulableCandidates = append(nonReschedulableCandidates, candidate)
			continue
		}

		// If candidates pods can be rescheduled and Scheduling Simulation logic determines that we dont need to launch another NodeClaim for the candidate pods
		// then we can deprovision the candidate.
		// Else we pick the oldest candidate based on the CreationTimestamp for deprovisioning
		if results.AllNonPendingPodsScheduled() && len(results.NewNodeClaims) == 0 {
			// All pods can be rescheduled then pick the candidate for deprovisioning
			return Command{
				Candidates: []*Candidate{candidate},
			}, nil
		} else {
			nonReschedulableCandidates = append(nonReschedulableCandidates, candidate)
		}
	}

	if !(len(nonReschedulableCandidates) > 0) {
		return Command{}, fmt.Errorf("no candidates to deprovision")
	}

	return Command{
		Candidates: []*Candidate{nonReschedulableCandidates[0]},
	}, nil
}

func (o *Overprovisioned) Reason() v1.DisruptionReason {
	return v1.DisruptionReasonOverprovisioned
}

func (o *Overprovisioned) Class() string {
	return EventualDisruptionClass
}

func (o *Overprovisioned) ConsolidationType() string {
	return ""
}
