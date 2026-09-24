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

package agent

import (
	"cmp"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// Volume/device state literals from SPEC.md §4.2, named once here
// (goconst) and reused across the package.
const (
	statePending     = "Pending"
	stateApplied     = "Applied"
	stateUnsupported = "Unsupported"
	stateFailed      = "Failed"

	reasonVolumeNotMounted = "VolumeNotMounted"
)

// ownedByDevice indexes pil.Status.Devices by device for O(1) lookups.
func ownedByDevice(pil *storagev1alpha1.PodIOLimit) map[string]storagev1alpha1.OwnedDevice {
	out := make(map[string]storagev1alpha1.OwnedDevice, len(pil.Status.Devices))
	for _, d := range pil.Status.Devices {
		out[d.Device] = d
	}
	return out
}

// volumeStatusByName indexes pil.Status.Volumes by volume name.
func volumeStatusByName(pil *storagev1alpha1.PodIOLimit) map[string]storagev1alpha1.VolumeStatus {
	out := make(map[string]storagev1alpha1.VolumeStatus, len(pil.Status.Volumes))
	for _, v := range pil.Status.Volumes {
		out[v.Name] = v
	}
	return out
}

// volumeOutcome is the per-volume state this reconcile decided, used to
// build the final []VolumeStatus (sorted by name for a stable/comparable
// status) once every volume has been resolved and, where applicable,
// applied.
type volumeOutcome struct {
	device      string
	mountDevice string
	state       string
	reason      string
	message     string
}

func buildVolumeStatuses(volumeOrder []string, outcomes map[string]volumeOutcome) []storagev1alpha1.VolumeStatus {
	out := make([]storagev1alpha1.VolumeStatus, 0, len(outcomes))
	for _, name := range volumeOrder {
		o, ok := outcomes[name]
		if !ok {
			continue
		}
		out = append(out, storagev1alpha1.VolumeStatus{
			Name:        name,
			Device:      o.device,
			MountDevice: o.mountDevice,
			State:       o.state,
			Reason:      o.reason,
			Message:     o.message,
		})
	}
	slices.SortFunc(out, func(a, b storagev1alpha1.VolumeStatus) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func buildDeviceList(devices map[string]storagev1alpha1.OwnedDevice) []storagev1alpha1.OwnedDevice {
	out := make([]storagev1alpha1.OwnedDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b storagev1alpha1.OwnedDevice) int { return cmp.Compare(a.Device, b.Device) })
	return out
}

// readyReason aggregates every volume's state into the PodIOLimit's Ready
// condition (SPEC.md §4.2: Applied | Pending | Failed | Released, extended
// here with Unsupported). A volume that's Unsupported and settled (no
// Pending, no Failed) doesn't block other volumes from reaching Applied --
// but if *every* volume ended up Unsupported, nothing was actually
// applied, and reporting True/Applied would be misleading (coordinator
// review, C2): this case reports False/Unsupported instead.
func readyReason(volumes []storagev1alpha1.VolumeStatus) (status metav1.ConditionStatus, reason string) {
	hasFailed, hasPending, hasApplied := false, false, false
	for _, v := range volumes {
		switch v.State {
		case stateFailed:
			hasFailed = true
		case statePending:
			hasPending = true
		case stateApplied:
			hasApplied = true
		}
	}
	switch {
	case hasFailed:
		return metav1.ConditionFalse, stateFailed
	case hasPending:
		return metav1.ConditionFalse, statePending
	case !hasApplied:
		// Every volume is Unsupported (or there are no volumes at all,
		// which CRD MinItems=1 on spec.volumes rules out in practice).
		return metav1.ConditionFalse, stateUnsupported
	default:
		return metav1.ConditionTrue, stateApplied
	}
}

func setReadyCondition(pil *storagev1alpha1.PodIOLimit, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pil.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: pil.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// statusSemanticallyEqual compares two PodIOLimitStatus values ignoring
// condition timestamps (D11: "status is written only when it semantically
// changed, ignoring condition timestamps").
func statusSemanticallyEqual(a, b storagev1alpha1.PodIOLimitStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration || a.CgroupPath != b.CgroupPath {
		return false
	}
	if !devicesEqual(a.Devices, b.Devices) || !volumesEqual(a.Volumes, b.Volumes) {
		return false
	}
	ac := meta.FindStatusCondition(a.Conditions, "Ready")
	bc := meta.FindStatusCondition(b.Conditions, "Ready")
	switch {
	case ac == nil && bc == nil:
		return true
	case ac == nil || bc == nil:
		return false
	default:
		return ac.Status == bc.Status && ac.Reason == bc.Reason && ac.Message == bc.Message
	}
}

func devicesEqual(a, b []storagev1alpha1.OwnedDevice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func volumesEqual(a, b []storagev1alpha1.VolumeStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
