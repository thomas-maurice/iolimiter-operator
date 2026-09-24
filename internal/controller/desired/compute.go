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

// Package desired implements the controller's pure core (SPEC.md §6.2):
// Compute merges every IOLimiter targeting a pod into the fully resolved
// PodIOLimit volumes plus a list of controller-side issues (D20). It is
// used by both PodReconciler (to build PodIOLimit.spec) and
// IOLimiterReconciler (to build IOLimiter.status), so the two can never
// disagree about what a given (pod, limiters) pair means.
package desired

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

// Issue reasons, per SPEC.md §4.2/§6.3. VolumeNotFound, UnsupportedVolumeType
// and PVCNotBound are the D19/D20 controller-side problems. InvalidLimit is
// the C0 deviation note's controller-side range/whole-number check on the
// Quantity bps fields (CEL can't afford it, see SPEC.md C0 deviation and C4
// deviation note).
const (
	ReasonVolumeNotFound    = "VolumeNotFound"
	ReasonUnsupportedVolume = "UnsupportedVolumeType"
	ReasonPVCNotBound       = "PVCNotBound"
	ReasonInvalidLimit      = "InvalidLimit"
)

// Issue is one controller-side problem, attributed to the specific
// IOLimiter that caused it so IOLimiterReconciler can surface it on that
// limiter's status without re-deriving anything (D20).
type Issue struct {
	// Volume is the pod volume name the issue concerns.
	Volume string
	// LimiterName is the IOLimiter this issue is attributed to.
	LimiterName string
	// Reason is one of the Reason* constants above.
	Reason string
	// Message is a human-readable explanation.
	Message string
}

// PVCLookup resolves a PersistentVolumeClaim by name in the pod's
// namespace. Compute stays pure (no client, no context): callers build this
// from whatever cache they already hold (D16: PVCs are cluster-wide cached).
type PVCLookup func(claimName string) (*corev1.PersistentVolumeClaim, bool)

// PVLookup resolves a (cluster-scoped) PersistentVolume by name. Compute
// stays pure; callers build this from whatever cache they already hold
// (F4: the controller now also caches PersistentVolumes). A PV that isn't
// found (not yet synced, or genuinely absent) is treated as "nothing to
// reject" -- resolvePVCClaim falls through to its normal PVC-derived
// kubeletDirName, matching this codebase's existing fail-open stance on
// cache lag (mirrors PVCLookup's own not-found handling).
type PVLookup func(pvName string) (*corev1.PersistentVolume, bool)

// oneKi and onePi bound the controller-side whole-number-of-bytes check on
// IOLimits.readBytesPerSecond/writeBytesPerSecond (SPEC.md C0 deviation
// note: the equivalent CEL rule always exceeds the apiserver's cost budget
// once volumes[] carries MaxItems=16, so this check moves here).
const (
	oneKi = int64(1024)
	onePi = int64(1024) * 1024 * 1024 * 1024 * 1024
)

// Compute merges every limiter targeting pod into the fully resolved
// PodIOLimit volumes (D2: per-field minimum, sorted sources) plus the
// controller-side issues found along the way (D20). limiters is assumed to
// already be selector-filtered by the caller (both reconcilers share a
// selector-match helper); Compute only merges and resolves volumes.
//
// A pod volume never referenced by any limiter is never inspected: it is
// simply not in the output and never generates an issue.
//
// existing is the pod's current/previous PodIOLimit spec volumes (nil if
// none exists yet, e.g. a brand-new pod) -- D36's freeze-don't-loosen input
// (D36 revises D35: per-limiter contributions, not a whole-volume floor).
// Each volume in the output carries a Contributions list, one entry per
// contributing limiter, holding exactly that limiter's own limits for the
// volume; Limits is the per-field minimum (D2) across those contributions
// and Sources is derived from them. If a limiter's row for a volume has an
// invalid byte field (InvalidLimit), that limiter reuses ONLY its own
// previous contribution entry for that exact volume from existing (looked
// up by source name, never another limiter's), discarding any newly-valid
// fields the same row also set -- deterministic, and it can never pick up
// another limiter's value. A limiter that was never previously a
// contributing source for that volume (a brand-new match, or a volume it
// never touched before) has nothing to freeze: it falls back to
// contributing whatever fields in the row ARE valid (the pre-D35 per-field
// behaviour), same as an ordinary invalid row with no freeze available. A
// limiter that's deleted or no longer matches the pod is simply absent from
// limiters, so it generates no contribution and no entry, regardless of
// what existing says. Either way the InvalidLimit issue is always recorded,
// and its message notes when a previous contribution was kept.
func Compute(pod *corev1.Pod, limiters []storagev1alpha1.IOLimiter, existing []storagev1alpha1.PodVolumeLimit, pvcLookup PVCLookup, pvLookup PVLookup) ([]storagev1alpha1.PodVolumeLimit, []Issue) {
	// volContribs[volume][limiterName] = that limiter's own contribution.
	volContribs := make(map[string]map[string]storagev1alpha1.DeviceLimits)
	var issues []Issue

	existingByVolume := make(map[string]map[string]storagev1alpha1.DeviceLimits, len(existing))
	for _, v := range existing {
		m := make(map[string]storagev1alpha1.DeviceLimits, len(v.Contributions))
		for _, c := range v.Contributions {
			m[c.Source] = c.Limits
		}
		existingByVolume[v.Name] = m
	}

	for _, limiter := range limiters {
		for _, vl := range limiter.Spec.Volumes {
			contrib, rowIssues, ok := mergeContribution(vl, limiter.Name, existingByVolume[vl.Name])
			issues = append(issues, rowIssues...)
			if !ok {
				continue
			}
			m, exists := volContribs[vl.Name]
			if !exists {
				m = make(map[string]storagev1alpha1.DeviceLimits)
				volContribs[vl.Name] = m
			}
			m[limiter.Name] = contrib
		}
	}

	names := make([]string, 0, len(volContribs))
	for name := range volContribs {
		names = append(names, name)
	}
	slices.Sort(names)

	var out []storagev1alpha1.PodVolumeLimit
	for _, name := range names {
		contribMap := volContribs[name]
		sources := make([]string, 0, len(contribMap))
		for s := range contribMap {
			sources = append(sources, s)
		}
		slices.Sort(sources)

		vol, found := findPodVolume(pod, name)
		if !found {
			for _, s := range sources {
				issues = append(issues, Issue{
					Volume: name, LimiterName: s, Reason: ReasonVolumeNotFound,
					Message: fmt.Sprintf("pod has no volume named %q", name),
				})
			}
			continue
		}

		dirName, reason, message := resolveKubeletDir(pod, vol, pvcLookup, pvLookup)
		if reason != "" {
			for _, s := range sources {
				issues = append(issues, Issue{Volume: name, LimiterName: s, Reason: reason, Message: message})
			}
			continue
		}

		contributions := make([]storagev1alpha1.VolumeContribution, 0, len(sources))
		var merged storagev1alpha1.DeviceLimits
		for _, s := range sources {
			c := contribMap[s]
			contributions = append(contributions, storagev1alpha1.VolumeContribution{Source: s, Limits: c})
			mergeMin(&merged, c)
		}

		out = append(out, storagev1alpha1.PodVolumeLimit{
			Name:           name,
			KubeletDirName: dirName,
			Limits:         merged,
			Sources:        sources,
			Contributions:  contributions,
		})
	}

	return out, issues
}

// mergeContribution computes one limiter's own contribution for one volume
// row (D2/D36): every field it validly sets. existingForVolume is that
// volume's existing contributions keyed by source (nil map if there wasn't
// an existing PodIOLimit, or the volume is new). Returns ok=false when the
// limiter ends up contributing nothing at all for this volume (drop it like
// it was never named).
func mergeContribution(vl storagev1alpha1.VolumeLimit, limiterName string, existingForVolume map[string]storagev1alpha1.DeviceLimits) (storagev1alpha1.DeviceLimits, []Issue, bool) {
	var issues []Issue
	var contrib storagev1alpha1.DeviceLimits
	rowHasInvalid := false
	contributed := false

	if vl.Limits.ReadBytesPerSecond != nil {
		if v, ok := validateBPS(vl.Limits.ReadBytesPerSecond); ok {
			contrib.ReadBPS = &v
			contributed = true
		} else {
			rowHasInvalid = true
			issues = append(issues, invalidLimitIssue(vl.Name, limiterName, "readBytesPerSecond", vl.Limits.ReadBytesPerSecond))
		}
	}
	if vl.Limits.WriteBytesPerSecond != nil {
		if v, ok := validateBPS(vl.Limits.WriteBytesPerSecond); ok {
			contrib.WriteBPS = &v
			contributed = true
		} else {
			rowHasInvalid = true
			issues = append(issues, invalidLimitIssue(vl.Name, limiterName, "writeBytesPerSecond", vl.Limits.WriteBytesPerSecond))
		}
	}
	// IOPS bounds ([2, 4294967295]) are already enforced by the CRD schema's
	// Minimum/Maximum markers (proven in
	// TestIOLimiter_CEL_QuantityLimits_Envtest), so no controller-side
	// re-check is needed here (per the C4 chunk spec's instruction to check
	// what the CRD actually does before adding one).
	if vl.Limits.ReadIOPS != nil {
		v := *vl.Limits.ReadIOPS
		contrib.ReadIOPS = &v
		contributed = true
	}
	if vl.Limits.WriteIOPS != nil {
		v := *vl.Limits.WriteIOPS
		contrib.WriteIOPS = &v
		contributed = true
	}

	if !rowHasInvalid {
		return contrib, nil, contributed
	}

	if prev, ok := existingForVolume[limiterName]; ok {
		// D36: reuse ONLY this limiter's own previous contribution,
		// discarding any newly-valid fields the same row also carried --
		// deterministic, and it can never pick up another limiter's value.
		for i := range issues {
			issues[i].Message += "; previous limits kept"
		}
		return prev, issues, true
	}

	// Nothing to freeze: fall back to whatever fields in this row ARE
	// valid, same as an ordinary invalid row with no prior contribution.
	return contrib, issues, contributed
}

// mergeMin folds c into dst via D2's per-field minimum.
func mergeMin(dst *storagev1alpha1.DeviceLimits, c storagev1alpha1.DeviceLimits) {
	if c.ReadBPS != nil && (dst.ReadBPS == nil || *c.ReadBPS < *dst.ReadBPS) {
		dst.ReadBPS = c.ReadBPS
	}
	if c.WriteBPS != nil && (dst.WriteBPS == nil || *c.WriteBPS < *dst.WriteBPS) {
		dst.WriteBPS = c.WriteBPS
	}
	if c.ReadIOPS != nil && (dst.ReadIOPS == nil || *c.ReadIOPS < *dst.ReadIOPS) {
		dst.ReadIOPS = c.ReadIOPS
	}
	if c.WriteIOPS != nil && (dst.WriteIOPS == nil || *c.WriteIOPS < *dst.WriteIOPS) {
		dst.WriteIOPS = c.WriteIOPS
	}
}

// invalidLimitIssue builds the InvalidLimit Issue for one bad field. The
// caller appends "; previous limits kept" to the message itself when D36's
// freeze actually applied.
func invalidLimitIssue(volume, limiterName, field string, q *resource.Quantity) Issue {
	return Issue{
		Volume:      volume,
		LimiterName: limiterName,
		Reason:      ReasonInvalidLimit,
		Message:     fmt.Sprintf("%s %s: must be a whole number of bytes between 1Ki and 1Pi", field, q.String()),
	}
}

// validateBPS is the C0 deviation note's controller-side stand-in for the
// CEL rule the apiserver's cost budget can't afford: a whole number of
// bytes between 1Ki and 1Pi inclusive.
func validateBPS(q *resource.Quantity) (int64, bool) {
	v := q.Value()
	// Value() silently rounds a fractional quantity up to the nearest whole
	// byte (per resource.Quantity's own doc comment); comparing back against
	// the original catches anything that wasn't already a whole number
	// (e.g. "100m" = 0.1 bytes), mirroring CEL's isInteger() check.
	whole := resource.NewQuantity(v, q.Format)
	if q.Cmp(*whole) != 0 {
		return 0, false
	}
	if v < oneKi || v > onePi {
		return 0, false
	}
	return v, true
}

func findPodVolume(pod *corev1.Pod, name string) (*corev1.Volume, bool) {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i], true
		}
	}
	return nil, false
}

// resolveKubeletDir implements D19: which volume sources are supported and
// how their kubeletDirName is derived. reason == "" means ok.
func resolveKubeletDir(pod *corev1.Pod, vol *corev1.Volume, pvcLookup PVCLookup, pvLookup PVLookup) (dirName, reason, message string) {
	switch {
	case vol.PersistentVolumeClaim != nil:
		return resolvePVCClaim(vol.PersistentVolumeClaim.ClaimName, pvcLookup, pvLookup)
	case vol.Ephemeral != nil:
		// Generic ephemeral volume: kubelet creates a PVC named
		// "<pod>-<volume>" (D19).
		return resolvePVCClaim(pod.Name+"-"+vol.Name, pvcLookup, pvLookup)
	case vol.CSI != nil:
		// Inline CSI: kubeletDirName is the pod volume name itself (§4.2).
		return vol.Name, "", ""
	default:
		return "", ReasonUnsupportedVolume, fmt.Sprintf(
			"volume %q has an unsupported source (supported: persistentVolumeClaim, generic ephemeral, inline csi)", vol.Name)
	}
}

func resolvePVCClaim(claimName string, pvcLookup PVCLookup, pvLookup PVLookup) (dirName, reason, message string) {
	pvc, found := pvcLookup(claimName)
	if !found || pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return "", ReasonPVCNotBound, fmt.Sprintf("persistentVolumeClaim %q is not bound", claimName)
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return "", ReasonUnsupportedVolume, fmt.Sprintf("persistentVolumeClaim %q is block-mode, not supported", claimName)
	}
	// F4: a hostPath-source PV (e.g. k3s/Rancher local-path-provisioner) is
	// never mounted by kubelet under pods/<uid>/volumes/ -- the runtime
	// bind-mounts the host directory straight into the container instead
	// (K8), so the agent can never resolve a device for it and the volume
	// would sit Pending forever with no indication why. A `local` PV is
	// deliberately *not* rejected here: kubelet's local-volume plugin does
	// mount it under pods/<uid>/volumes/kubernetes.io~local-volume/<name>
	// like any other plugin (internal/mountinfo.FindKubeletVolume matches
	// structurally, not by plugin name), which is exactly what the kind e2e
	// exercises successfully.
	if pv, ok := pvLookup(pvc.Spec.VolumeName); ok && pv.Spec.HostPath != nil {
		return "", ReasonUnsupportedVolume, fmt.Sprintf(
			"persistentVolumeClaim %q is backed by hostPath PersistentVolume %q, which kubelet does not mount under the pod's kubelet volumes directory",
			claimName, pvc.Spec.VolumeName)
	}
	return pvc.Spec.VolumeName, "", ""
}
