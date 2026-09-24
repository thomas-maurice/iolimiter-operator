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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// PodIOLimitSpec defines the desired state of PodIOLimit. It is fully
// resolved by the controller: the agent never derives anything from the
// cluster, only from this spec plus the host it runs on.
// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName && self.podName == oldSelf.podName && self.podUID == oldSelf.podUID && self.qosClass == oldSelf.qosClass",message="pod identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.podUID.matches('^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')",message="podUID must be a UUID"
type PodIOLimitSpec struct {
	// nodeName is the node the pod is scheduled on, set by the controller.
	// Informational/routing only: the agent DaemonSet pod on that node is
	// the only one that reconciles this object (field-selected watch on
	// this field). Not meant to be read or set by hand.
	// +required
	NodeName string `json:"nodeName"`

	// podName is the name of the pod this internal object was compiled
	// for by the controller. Change limits via the IOLimiter that selects
	// this pod, not by editing this field.
	// +required
	PodName string `json:"podName"`

	// podUID is the UID of the pod this object was compiled for. A
	// recreated pod (same name, new UID) never reuses this object. Pod UIDs
	// are always UUIDs on every supported cluster (D39): the spec-level CEL
	// rule above rejects a malformed value at admission instead of the
	// agent trusting an arbitrary string into a cgroup/filesystem path.
	// (A +kubebuilder:validation:Pattern marker directly on this field
	// doesn't work: controller-gen refuses to apply a pattern to
	// types.UID's underlying schema type, "must apply pattern to a textual
	// value, found type ''" -- CEL on the parent struct is the workaround.)
	// +required
	PodUID types.UID `json:"podUID"`

	// qosClass is the pod's QoS class, used to compute its cgroup path.
	// +kubebuilder:validation:Enum=Guaranteed;Burstable;BestEffort
	// +required
	QOSClass corev1.PodQOSClass `json:"qosClass"`

	// volumes lists the pod volumes to limit, already merged across every
	// contributing IOLimiter (D2: per-field minimum).
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +required
	Volumes []PodVolumeLimit `json:"volumes"`
}

// PodVolumeLimit is one pod volume's resolved limit.
type PodVolumeLimit struct {
	// name of the pod volume (pod.spec.volumes[].name).
	// +required
	Name string `json:"name"`

	// kubeletDirName is the directory kubelet mounts this volume under
	// (pods/<uid>/volumes/<plugin>/<dir>): the PV name for PVC/ephemeral
	// volumes, the pod volume name for inline CSI. Always a DNS-1123
	// subdomain (D39): PersistentVolume names are DNS subdomains, and the
	// inline-CSI case reuses the pod volume name, itself already
	// DNS-1123-label-shaped (VolumeLimit.Name's own pattern, stricter than
	// a subdomain).
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +required
	KubeletDirName string `json:"kubeletDirName"`

	// limits are the per-field minimum (D2) across contributions. Unset
	// field = unlimited (max). Derived from contributions; not itself a
	// source of truth.
	// +required
	Limits DeviceLimits `json:"limits"`

	// sources lists the contributing IOLimiter names, sorted. Derived from
	// contributions; not itself a source of truth.
	// +listType=set
	// +optional
	Sources []string `json:"sources,omitempty"`

	// contributions lists each contributing IOLimiter's own limits for this
	// volume (D36). limits and sources above are derived from this list:
	// limits is the per-field minimum across every entry, sources is every
	// entry's Source. An IOLimiter with an InvalidLimit issue for this
	// volume reuses its own prior entry here rather than contributing
	// nothing or loosening the effective limit (D35/D36: "an invalid limit
	// freezes, it does not loosen" -- per-limiter, not whole-volume). A
	// limiter that's deleted or no longer matches the pod simply has no
	// entry.
	// +listType=map
	// +listMapKey=source
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Contributions []VolumeContribution `json:"contributions,omitempty"`
}

// VolumeContribution is one IOLimiter's own contribution to a
// PodVolumeLimit (D36).
type VolumeContribution struct {
	// source is the contributing IOLimiter's name.
	// +required
	Source string `json:"source"`

	// limits is this limiter's own limits for the volume (not yet merged
	// with any other contribution). Unset field = this limiter set no
	// ceiling on that field.
	// +required
	Limits DeviceLimits `json:"limits"`
}

// DeviceLimits are resolved, whole-number limits ready to write to
// cgroup v2 io.max. nil = max (unlimited).
type DeviceLimits struct {
	// +optional
	ReadBPS *int64 `json:"readBPS,omitempty"`
	// +optional
	WriteBPS *int64 `json:"writeBPS,omitempty"`
	// +optional
	ReadIOPS *int64 `json:"readIOPS,omitempty"`
	// +optional
	WriteIOPS *int64 `json:"writeIOPS,omitempty"`
}

// PodIOLimitStatus defines the observed state of PodIOLimit.
type PodIOLimitStatus struct {
	// observedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// cgroupPath is the pod cgroup path relative to the cgroup2 root.
	// Informational only.
	// +optional
	CgroupPath string `json:"cgroupPath,omitempty"`

	// devices is the ownership record (D8): every device a rule may have
	// been written for in this pod's cgroup. Entries are added before the
	// kernel write.
	// +listType=map
	// +listMapKey=device
	// +optional
	Devices []OwnedDevice `json:"devices,omitempty"`

	// volumes reports the resolution/apply state of each spec.volumes[] entry.
	// +listType=map
	// +listMapKey=name
	// +optional
	Volumes []VolumeStatus `json:"volumes,omitempty"`

	// conditions represent the current state of the PodIOLimit resource.
	//
	// Ready reasons: Applied | Pending | Failed | Released | Unsupported
	// (Unsupported: every listed volume resolved to Unsupported, so
	// nothing was actually applied)
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// OwnedDevice records a device this PodIOLimit owns a rule for.
type OwnedDevice struct {
	// device is the whole-disk "MAJ:MIN".
	// +required
	Device string `json:"device"`

	// rule is the four-key io.max rule ("rbps=... wbps=... riops=... wiops=...").
	// +required
	Rule string `json:"rule"`

	// +kubebuilder:validation:Enum=Pending;Applied
	// +required
	State string `json:"state"`
}

// VolumeStatus reports the resolution/apply state of one volume.
type VolumeStatus struct {
	// name of the pod volume.
	// +required
	Name string `json:"name"`

	// device is the whole-disk device throttled for this volume, if resolved.
	// +optional
	Device string `json:"device,omitempty"`

	// mountDevice is the device from mountinfo, when it was a partition of device.
	// +optional
	MountDevice string `json:"mountDevice,omitempty"`

	// +kubebuilder:validation:Enum=Applied;Pending;Unsupported;Failed
	// +required
	State string `json:"state"`

	// reason is one of: CgroupNotFound | VolumeNotMounted | NoBlockDevice |
	// SharedRootDevice | IOControllerDisabled | KernelRejected |
	// KernelMismatch | SharedDevice | InvalidSpec (D39: podUID isn't a
	// UUID -- the CRD pattern should already reject this at admission, this
	// is defense in depth for an object that predates the pattern or was
	// otherwise written some other way)
	// +optional
	Reason string `json:"reason,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pil
// +kubebuilder:selectablefield:JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=".spec.podName",description="The pod this internal object was compiled for. See IOLimiter to change limits."
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeName",priority=1,description="Node the pod is scheduled on; set by the controller, not meaningful to edit."
// +kubebuilder:printcolumn:name="Managed-By",type=string,JSONPath=".metadata.labels['app\\.kubernetes\\.io/managed-by']",priority=1

// PodIOLimit is the Schema for the podiolimits API.
//
// Internal: created and managed by the IOLimiter controller, one per
// limited pod. Do not create or edit by hand; use IOLimiter. Its spec is
// compiled by the controller from the pods, PVCs and IOLimiters it
// matches, and its status is written by the node agent that enforces it
// (SPEC.md D4). A ValidatingAdmissionPolicy (SPEC.md D27) enforces this at
// admission time: only the controller's own ServiceAccount may create or
// update it.
type PodIOLimit struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PodIOLimit
	// +required
	Spec PodIOLimitSpec `json:"spec"`

	// status defines the observed state of PodIOLimit
	// +optional
	Status PodIOLimitStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PodIOLimitList contains a list of PodIOLimit
type PodIOLimitList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PodIOLimit `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PodIOLimit{}, &PodIOLimitList{})
		return nil
	})
}
