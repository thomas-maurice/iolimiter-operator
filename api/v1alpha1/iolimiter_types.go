/*
Copyright 2026.

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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// IOLimiterSpec defines the desired state of IOLimiter.
type IOLimiterSpec struct {
	// podSelector selects pods in this namespace. {} selects every pod.
	// Bounded controller-side, not by CEL (D37, security review: a tenant
	// with IOLimiter create could otherwise OOM/CPU-starve the
	// cluster-wide controller by parsing an enormous selector on every pod
	// event): at most 16 matchExpressions, each with at most 64 values, at
	// most 16 matchLabels, and every key/value capped at 63 characters (the
	// same bound Kubernetes itself enforces on label keys/values). CEL was
	// tried first and measured against the apiserver's cost budget in
	// envtest (TestIOLimiter_CEL_PodSelectorBounds_ExceedsCostBudget_Envtest):
	// LabelSelectorRequirement's key/values have no maxLength the schema
	// can declare (it's the upstream metav1 type, no controller-gen marker
	// applies to it), so the cost estimator assumes unbounded strings and
	// rejects the rule set at "more than 100x" the budget -- worse than the
	// single quantity() call that was already too expensive for
	// IOLimits (see that field's own deviation note). A selector that
	// exceeds these bounds is reported as InvalidSelector on the
	// IOLimiter's status and contributes nothing (matches no pods), mirroring
	// how InvalidLimit is handled per-field.
	// +required
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// volumes lists the pod volumes (pod.spec.volumes[].name) this limiter
	// applies to, and the limits for each.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +required
	Volumes []VolumeLimit `json:"volumes"`
}

// VolumeLimit names a pod volume and the IO limits to apply to the
// whole-disk device backing it.
type VolumeLimit struct {
	// name of the pod volume (pod.spec.volumes[].name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// limits caps the pod's IO on the device backing this volume.
	// +required
	Limits IOLimits `json:"limits"`
}

// IOLimits caps the pod's IO on the whole-disk device backing the volume
// (cgroup v2 io.max). Unset = unlimited.
// +kubebuilder:validation:XValidation:rule="has(self.readBytesPerSecond) || has(self.writeBytesPerSecond) || has(self.readIOPS) || has(self.writeIOPS)",message="at least one limit must be set"
type IOLimits struct {
	// readBytesPerSecond caps read bandwidth on the device. Should be a
	// whole number of bytes between 1Ki and 1Pi; the range/integer check is
	// enforced controller-side, not by CEL (see SPEC.md C0 deviation note:
	// a quantity()/isQuantity() CEL call on this field always exceeds the
	// apiserver's CEL cost budget once volumes[] has MaxItems=16).
	// +optional
	ReadBytesPerSecond *resource.Quantity `json:"readBytesPerSecond,omitempty"`

	// writeBytesPerSecond caps write bandwidth on the device. Should be a
	// whole number of bytes between 1Ki and 1Pi; the range/integer check is
	// enforced controller-side, not by CEL (see SPEC.md C0 deviation note:
	// a quantity()/isQuantity() CEL call on this field always exceeds the
	// apiserver's CEL cost budget once volumes[] has MaxItems=16).
	// +optional
	WriteBytesPerSecond *resource.Quantity `json:"writeBytesPerSecond,omitempty"`

	// readIOPS caps read IOPS on the device. Values <= 1 are ignored by the
	// kernel (tg_set_limit), so the minimum accepted here is 2.
	// +optional
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=4294967295
	ReadIOPS *int64 `json:"readIOPS,omitempty"`

	// writeIOPS caps write IOPS on the device. Values <= 1 are ignored by
	// the kernel (tg_set_limit), so the minimum accepted here is 2.
	// +optional
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=4294967295
	WriteIOPS *int64 `json:"writeIOPS,omitempty"`
}

// IOLimiterStatus defines the observed state of IOLimiter.
type IOLimiterStatus struct {
	// observedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// matchedPods is the count of scheduled, non-terminal pods selected by
	// podSelector that have at least one listed volume.
	// +optional
	MatchedPods int32 `json:"matchedPods"`

	// appliedPods is the count of matched pods with every listed volume Applied.
	// +optional
	AppliedPods int32 `json:"appliedPods"`

	// failedPods is the count of matched pods with at least one listed
	// volume Failed or Unsupported.
	// +optional
	FailedPods int32 `json:"failedPods"`

	// selectedPodsWithoutVolumes is the count of scheduled, non-terminal
	// pods selected by podSelector that have NONE of the listed volumes
	// (D38): evasion is surfaced, not prevented -- renaming a volume or
	// relabeling a pod escapes this limiter's ceiling, and this count (plus
	// up to 5 pod names in the Ready message, and a SelectedPodWithoutVolumes
	// warning event) is how an admin sees the escape instead of it silently
	// looking like NoMatchingPods.
	// +optional
	SelectedPodsWithoutVolumes int32 `json:"selectedPodsWithoutVolumes"`

	// conditions represent the current state of the IOLimiter resource.
	//
	// Ready: True/AllApplied | True/NoMatchingPods | False/Pending | False/Failed.
	// The message names up to 5 failing pods:
	// "<pod>: volume <v>: <reason>: <msg>".
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=iol
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=".status.matchedPods"
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=".status.appliedPods"
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=".status.failedPods"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="WithoutVolumes",type=integer,JSONPath=".status.selectedPodsWithoutVolumes",priority=1,description="Selected pods that have none of the listed volumes (D38): the limiter is not enforcing anything on them."

// IOLimiter is the Schema for the iolimiters API
type IOLimiter struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IOLimiter
	// +required
	Spec IOLimiterSpec `json:"spec"`

	// status defines the observed state of IOLimiter
	// +optional
	Status IOLimiterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IOLimiterList contains a list of IOLimiter
type IOLimiterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IOLimiter `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IOLimiter{}, &IOLimiterList{})
		return nil
	})
}
