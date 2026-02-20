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
	"context"
	"fmt"

	"go.uber.org/multierr"
)

// RuntimeValidate will be used to validate any part of the CRD that can not be validated at CRD creation
func (in *CapacityBuffer) RuntimeValidate(ctx context.Context) error {
	return in.Spec.validate()
}

func (in *CapacityBufferSpec) validate() error {
	return multierr.Combine(
		in.validateReplicasOrPercentage(),
		in.validatePodTemplateOrScalableRef(),
		in.validateReplicasValue(),
		in.validatePercentageValue(),
		in.validatePodTemplateRef(),
		in.validateScalableRef(),
	)
}

func (in *CapacityBufferSpec) validateReplicasOrPercentage() error {
	if in.Replicas != nil && in.Percentage != nil {
		return fmt.Errorf("cannot set both 'replicas' and 'percentage'")
	}
	if in.Replicas == nil && in.Percentage == nil {
		return fmt.Errorf("must set either 'replicas' or 'percentage'")
	}
	return nil
}

func (in *CapacityBufferSpec) validatePodTemplateOrScalableRef() error {
	if in.PodTemplateRef != nil && in.ScalableRef != nil {
		return fmt.Errorf("cannot set both 'podTemplateRef' and 'scalableRef'")
	}
	if in.PodTemplateRef == nil && in.ScalableRef == nil {
		return fmt.Errorf("must set either 'podTemplateRef' or 'scalableRef'")
	}
	return nil
}

func (in *CapacityBufferSpec) validateReplicasValue() error {
	if in.Replicas != nil && *in.Replicas < 0 {
		return fmt.Errorf("replicas must be non-negative, got %d", *in.Replicas)
	}
	return nil
}

func (in *CapacityBufferSpec) validatePercentageValue() error {
	if in.Percentage != nil && (*in.Percentage < 0 || *in.Percentage > 100) {
		return fmt.Errorf("percentage must be between 0 and 100, got %d", *in.Percentage)
	}
	return nil
}

func (in *CapacityBufferSpec) validatePodTemplateRef() error {
	if in.PodTemplateRef != nil && in.PodTemplateRef.Name == nil {
		return fmt.Errorf("podTemplateRef.name cannot be empty")
	}
	return nil
}

func (in *CapacityBufferSpec) validateScalableRef() error {
	if in.ScalableRef == nil {
		return nil
	}
	if in.ScalableRef.Kind == nil {
		return fmt.Errorf("scalableRef.kind cannot be empty")
	}
	if in.ScalableRef.Name == nil {
		return fmt.Errorf("scalableRef.name cannot be empty")
	}
	return nil
}
