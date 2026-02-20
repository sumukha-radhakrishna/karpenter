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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CapacityBuffer is the configuration that an autoscaler can use to provision buffer capacity within a cluster.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=capacitybuffers,scope=Namespaced,shortName=cb,categories=karpenter
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type CapacityBuffer struct {
	metav1.TypeMeta `json:",inline"`
	//nolint:kubeapilinter
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired characteristics of the buffer.
	// +optional
	Spec *CapacityBufferSpec `json:"spec,omitempty"`

	// status represents the current state of the buffer.
	// +optional
	Status CapacityBufferStatus `json:"status,omitempty"` //nolint:kubeapilinter
}

// CapacityBufferList contains a list of CapacityBuffer objects.
// +kubebuilder:object:root=true
type CapacityBufferList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapacityBuffer `json:"items"`
}

// LocalObjectRef contains the name of the object being referred to.
type LocalObjectRef struct {
	// name of the object.
	// +required
	Name *string `json:"name,omitempty"`
}

// ScalableRef contains name, kind and API group of an object that can be scaled.
type ScalableRef struct {
	// apiGroup of the scalable object.
	// +optional
	APIGroup *string `json:"apiGroup,omitempty"`
	// kind of the scalable object (e.g., "Deployment", "StatefulSet").
	// +required
	Kind *string `json:"kind,omitempty"`
	// name of the scalable object.
	// +required
	Name *string `json:"name,omitempty"`
}

// CapacityBufferSpec defines the desired state of CapacityBuffer.
type CapacityBufferSpec struct {
	// provisioningStrategy defines how the buffer is utilized.
	// +kubebuilder:default="buffer.x-k8s.io/active-capacity"
	// +optional
	ProvisioningStrategy *string `json:"provisioningStrategy,omitempty"`

	// podTemplateRef is a reference to a PodTemplate resource in the same namespace.
	// +optional
	PodTemplateRef *LocalObjectRef `json:"podTemplateRef,omitempty"`

	// scalableRef is a reference to an object that has a scale subresource.
	// +optional
	ScalableRef *ScalableRef `json:"scalableRef,omitempty"`

	// replicas defines the desired number of buffer chunks to provision.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// percentage defines the desired buffer capacity as a percentage of the scalableRef's current replicas.
	// +optional
	Percentage *int32 `json:"percentage,omitempty"`

	// limits will limit the number of chunks created for this buffer based on total resource requests.
	// +optional
	Limits *Limits `json:"limits,omitempty"`
}

type Limits v1.ResourceList
type AllocatedResources v1.ResourceList

// CapacityBufferStatus defines the observed state of CapacityBuffer.
type CapacityBufferStatus struct {
	// conditions provide a standard mechanism for reporting the buffer's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// podTemplateRef is the observed reference to the PodTemplate.
	// +optional
	PodTemplateRef *LocalObjectRef `json:"podTemplateRef,omitempty"`

	// replicas is the actual number of buffer chunks currently provisioned.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// podTemplateGeneration is the observed generation of the PodTemplate.
	// +optional
	PodTemplateGeneration *int64 `json:"podTemplateGeneration,omitempty"`

	// provisioningStrategy defines how the buffer should be utilized.
	// +optional
	ProvisioningStrategy *string `json:"provisioningStrategy,omitempty"`

	// allocatedResources represents the total resources allocated to buffer capacity.
	// +optional
	AllocatedResources *AllocatedResources `json:"allocatedResources,omitempty"`
}

// Helper methods for resource calculations
func (cb *CapacityBuffer) GetDesiredReplicas() int32 {
	if cb.Spec.Replicas != nil {
		return *cb.Spec.Replicas
	}
	return 0
}

func (cb *CapacityBuffer) CalculateResourcesPerReplica(podTemplate *v1.PodTemplate) v1.ResourceList {
	resources := v1.ResourceList{}
	for _, container := range podTemplate.Template.Spec.Containers {
		for resourceName, quantity := range container.Resources.Requests {
			if existing, ok := resources[resourceName]; ok {
				existing.Add(quantity)
				resources[resourceName] = existing
			} else {
				resources[resourceName] = quantity.DeepCopy()
			}
		}
	}
	return resources
}

func (cb *CapacityBuffer) CalculateTotalResources(podTemplate *v1.PodTemplate) v1.ResourceList {
	perReplica := cb.CalculateResourcesPerReplica(podTemplate)
	replicas := cb.GetDesiredReplicas()

	total := v1.ResourceList{}
	for resourceName, quantity := range perReplica {
		scaled := quantity.DeepCopy()
		scaled.Set(quantity.Value() * int64(replicas))
		total[resourceName] = scaled
	}
	return total
}

func (cb *CapacityBuffer) IsWithinLimits(podTemplate *v1.PodTemplate, currentReplicas int32) bool {
	if len(cb.Spec.Limits) == 0 {
		return true
	}

	perReplica := cb.CalculateResourcesPerReplica(podTemplate)

	for resourceName, limit := range cb.Spec.Limits {
		if perReplicaQty, ok := perReplica[resourceName]; ok {
			totalNeeded := perReplicaQty.DeepCopy()
			totalNeeded.Set(perReplicaQty.Value() * int64(currentReplicas))

			if totalNeeded.Cmp(limit) > 0 {
				return false
			}
		}
	}
	return true
}

// Condition types
const (
	ConditionTypeReady                = "Ready"
	ConditionTypeReadyForProvisioning = "ReadyForProvisioning"
	ConditionTypeLimitedByQuotas      = "LimitedByQuotas"
	ConditionTypeProvisioning         = "Provisioning"
)

// Condition reasons
const (
	ReasonReady                  = "Ready"
	ReasonPodTemplateNotFound    = "PodTemplateNotFound"
	ReasonScalableRefNotFound    = "ScalableRefNotFound"
	ReasonInvalidConfiguration   = "InvalidConfiguration"
	ReasonProvisioningInProgress = "ProvisioningInProgress"
	ReasonProvisioningComplete   = "ProvisioningComplete"
)

// SetCondition sets or updates a condition
func (cb *CapacityBuffer) SetCondition(conditionType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()

	for i, condition := range cb.Status.Conditions {
		if condition.Type == conditionType {
			if condition.Status != status || condition.Reason != reason {
				cb.Status.Conditions[i].Status = status
				cb.Status.Conditions[i].Reason = reason
				cb.Status.Conditions[i].Message = message
				cb.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}

	// Condition doesn't exist, add it
	cb.Status.Conditions = append(cb.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// GetCondition returns a condition by type
func (cb *CapacityBuffer) GetCondition(conditionType string) *metav1.Condition {
	for _, condition := range cb.Status.Conditions {
		if condition.Type == conditionType {
			return &condition
		}
	}
	return nil
}

// IsReady returns true if the buffer is ready for provisioning
func (cb *CapacityBuffer) IsReady() bool {
	condition := cb.GetCondition(ConditionTypeReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// StatusConditions returns the status condition set for the CapacityBuffer
func (cb *CapacityBuffer) GetConditions() []metav1.Condition {
	return cb.Status.Conditions
}

// SetConditions sets the status conditions for the CapacityBuffer
func (cb *CapacityBuffer) SetConditions(conditions []metav1.Condition) {
	cb.Status.Conditions = conditions
}
