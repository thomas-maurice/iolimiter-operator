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

package desired

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

func quantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func limiter(name string, volumes ...storagev1alpha1.VolumeLimit) storagev1alpha1.IOLimiter {
	return storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec:       storagev1alpha1.IOLimiterSpec{Volumes: volumes},
	}
}

func boundFilesystemPVC(name, pvName string) *corev1.PersistentVolumeClaim {
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName, VolumeMode: &mode},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func lookupOf(pvcs ...*corev1.PersistentVolumeClaim) PVCLookup {
	return func(claimName string) (*corev1.PersistentVolumeClaim, bool) {
		for _, p := range pvcs {
			if p.Name == claimName {
				return p, true
			}
		}
		return nil, false
	}
}

func pvLookupOf(pvs ...*corev1.PersistentVolume) PVLookup {
	return func(pvName string) (*corev1.PersistentVolume, bool) {
		for _, pv := range pvs {
			if pv.Name == pvName {
				return pv, true
			}
		}
		return nil, false
	}
}

func hostPathPV(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
			},
		},
	}
}

func localPV(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{Path: "/mnt/disk0"},
			},
		},
	}
}

func podWithPVC(volumeName, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
					},
				},
			},
		},
	}
}

// TestCompute_OverlappingLimiters_PerFieldMinimum proves D2: two limiters
// on the same volume tighten each other per-field (never loosen), and
// sources is the sorted union of every limiter that actually contributed a
// field -- this is what lets an operator read "sources" on a PodIOLimit and
// know exactly which IOLimiter to edit to loosen a given field.
func TestCompute_OverlappingLimiters_PerFieldMinimum(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	limiters := []storagev1alpha1.IOLimiter{
		limiter("strict", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			ReadBytesPerSecond: quantity("10Mi"), ReadIOPS: ptr.To[int64](100),
		}}),
		limiter("looser", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			ReadBytesPerSecond: quantity("20Mi"), WriteBytesPerSecond: quantity("5Mi"),
		}}),
	}

	out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())

	require.Empty(t, issues)
	require.Len(t, out, 1)
	v := out[0]
	assert.Equal(t, "data", v.Name)
	assert.Equal(t, "pvc-1234", v.KubeletDirName)
	// The minimum of 10Mi and 20Mi is 10Mi: the stricter limiter must win,
	// never the looser one (D2's whole point).
	assert.EqualValues(t, 10*1024*1024, *v.Limits.ReadBPS)
	assert.EqualValues(t, 5*1024*1024, *v.Limits.WriteBPS)
	require.NotNil(t, v.Limits.ReadIOPS)
	assert.EqualValues(t, 100, *v.Limits.ReadIOPS)
	assert.Nil(t, v.Limits.WriteIOPS)
	assert.Equal(t, []string{"looser", "strict"}, v.Sources, "sources must be sorted and list only contributing limiters")
	require.Len(t, v.Contributions, 2, "D36: each contributing limiter gets its own contribution entry")
	assert.Equal(t, "looser", v.Contributions[0].Source)
	assert.Equal(t, "strict", v.Contributions[1].Source)
}

// TestCompute_VolumeAbsentFromPod_NotOutputNotIssue proves the C4
// acceptance line's case: a pod volume that no limiter names at all is
// simply irrelevant to Compute -- it must never show up in the output and
// must never generate a controller-side issue (it isn't a problem, nobody
// asked to limit it).
func TestCompute_VolumeAbsentFromPod_NotOutputNotIssue(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	// No limiter at all -- "scratch" is never referenced.
	out, issues := Compute(pod, nil, nil, lookupOf(), pvLookupOf())
	assert.Empty(t, out)
	assert.Empty(t, issues)
}

// TestCompute_VolumeNotFoundInPod: a limiter names a volume the pod simply
// doesn't have (D20's "volume missing in pod"). This must surface as an
// issue attributed to the limiter -- silently dropping it would hide a
// typo'd volume name from the user forever.
func TestCompute_VolumeNotFoundInPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"}}
	limiters := []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
	}
	out, issues := Compute(pod, limiters, nil, lookupOf(), pvLookupOf())
	assert.Empty(t, out)
	require.Len(t, issues, 1)
	assert.Equal(t, "data", issues[0].Volume)
	assert.Equal(t, "l1", issues[0].LimiterName)
	assert.Equal(t, ReasonVolumeNotFound, issues[0].Reason)
}

// TestCompute_UnsupportedVolumeTypes proves D19: hostPath, emptyDir and a
// block-mode PVC are all explicitly out of scope (K8: they aren't mounted
// under the kubelet pod dir the agent can resolve, or they aren't
// filesystem-throttleable) and must be reported, not silently skipped or
// (worse) silently limited on the wrong device.
func TestCompute_UnsupportedVolumeTypes(t *testing.T) {
	blockMode := corev1.PersistentVolumeBlock
	tests := []struct {
		name string
		vol  corev1.Volume
		pvcs []*corev1.PersistentVolumeClaim
	}{
		{
			name: "hostPath",
			vol:  corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}},
		},
		{
			name: "emptyDir",
			vol:  corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		{
			name: "block-mode PVC",
			vol: corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "blockclaim"},
			}},
			pvcs: []*corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "blockclaim", Namespace: "apps"},
				Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-block", VolumeMode: &blockMode},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"},
				Spec:       corev1.PodSpec{Volumes: []corev1.Volume{tc.vol}},
			}
			limiters := []storagev1alpha1.IOLimiter{
				limiter("l1", storagev1alpha1.VolumeLimit{Name: "v", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
			}
			out, issues := Compute(pod, limiters, nil, lookupOf(tc.pvcs...), pvLookupOf())
			assert.Empty(t, out)
			require.Len(t, issues, 1)
			assert.Equal(t, ReasonUnsupportedVolume, issues[0].Reason)
		})
	}
}

// TestCompute_GenericEphemeral_ClaimNaming proves D19's generic ephemeral
// naming convention: the backing PVC kubelet creates is "<pod>-<volume>",
// not the volume name itself. Getting this wrong means the controller
// silently never resolves any generic ephemeral volume.
func TestCompute_GenericEphemeral_ClaimNaming(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
			},
		},
	}
	pvc := boundFilesystemPVC("web-0-scratch", "pv-eph-1")
	limiters := []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "scratch", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantity("1Mi")}}),
	}
	out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())
	require.Empty(t, issues)
	require.Len(t, out, 1)
	assert.Equal(t, "pv-eph-1", out[0].KubeletDirName)
}

// TestCompute_UnboundPVC proves D19: a PVC that hasn't bound yet must not
// be treated as resolvable (there is no PV name, hence no kubelet dir to
// throttle) -- it must be reported as PVCNotBound and excluded from the
// output, not crash or silently produce an empty kubeletDirName.
func TestCompute_UnboundPVC(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-web-0", Namespace: "apps"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	limiters := []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
	}
	out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())
	assert.Empty(t, out)
	require.Len(t, issues, 1)
	assert.Equal(t, ReasonPVCNotBound, issues[0].Reason)
}

// TestCompute_InvalidLimit_100m_512 is the C4 deviation note's regression
// test: since CEL cannot afford the 1Ki..1Pi whole-byte range check (C0
// deviation), the apiserver accepts a Quantity like "100m" (not a whole
// number of bytes) or "512" (below 1Ki) at admission time. Compute is the
// backstop: it must flag both as InvalidLimit and, per the deviation note,
// treat the limiter as having contributed nothing for that field -- so an
// operator who fat-fingers a millibyte value gets a visible warning instead
// of either a rejected CR or a silently-wrong kernel rule.
func TestCompute_InvalidLimit_100m_512(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "non-integer bytes (100m)", value: "100m"},
		{name: "below 1Ki (512)", value: "512"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := podWithPVC("data", "data-web-0")
			pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
			limiters := []storagev1alpha1.IOLimiter{
				limiter("bad", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
					ReadBytesPerSecond: quantity(tc.value),
				}}),
			}
			out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())
			// The only field the limiter set was invalid, so it contributed
			// nothing at all: the volume must not appear in the output.
			assert.Empty(t, out, "an all-invalid limiter must contribute nothing, so the volume is dropped like it was never targeted")
			require.Len(t, issues, 1)
			assert.Equal(t, ReasonInvalidLimit, issues[0].Reason)
			assert.Equal(t, "bad", issues[0].LimiterName)
			assert.Equal(t, "data", issues[0].Volume)
		})
	}
}

// TestCompute_InvalidLimit_DoesNotBlockOtherValidFields proves the "an
// invalid limit contributes nothing" rule is per-field, not per-limiter:
// a limiter with one bad field and one good field still contributes the
// good field, and a second limiter's valid fields on the same volume are
// completely unaffected.
func TestCompute_InvalidLimit_DoesNotBlockOtherValidFields(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	limiters := []storagev1alpha1.IOLimiter{
		limiter("bad-read-good-write", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			ReadBytesPerSecond:  quantity("100m"),
			WriteBytesPerSecond: quantity("2Mi"),
		}}),
	}
	out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())
	require.Len(t, issues, 1)
	assert.Equal(t, ReasonInvalidLimit, issues[0].Reason)
	require.Len(t, out, 1)
	assert.Nil(t, out[0].Limits.ReadBPS, "the invalid field must not appear as a limit")
	require.NotNil(t, out[0].Limits.WriteBPS)
	assert.EqualValues(t, 2*1024*1024, *out[0].Limits.WriteBPS)
	assert.Equal(t, []string{"bad-read-good-write"}, out[0].Sources, "the limiter still contributed via its valid field")
}

// TestCompute_InvalidLimit_IntegerFormAccepted proves validateBPS accepts
// the plain-integer Quantity form (already proven admitted by CEL in
// TestIOLimiter_CEL_QuantityLimits_Envtest) whenever it's a whole number of
// bytes in range, so the controller-side check doesn't reject anything CEL
// was always meant to accept.
// TestCompute_HostPathPV_UnsupportedVolumeType proves F4: a PVC bound to a
// hostPath-source PersistentVolume (the k3s/Rancher local-path-provisioner
// default) is never mounted by kubelet under pods/<uid>/volumes/ (K8), so
// the agent could never resolve a device for it -- it must be reported as
// UnsupportedVolumeType, naming the PV, rather than left to poll Pending
// forever with no indication why.
func TestCompute_HostPathPV_UnsupportedVolumeType(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pv-hostpath-1")
	pv := hostPathPV("pv-hostpath-1")

	out, issues := Compute(pod, []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
	}, nil, lookupOf(pvc), pvLookupOf(pv))

	assert.Empty(t, out)
	require.Len(t, issues, 1)
	assert.Equal(t, ReasonUnsupportedVolume, issues[0].Reason)
	assert.Contains(t, issues[0].Message, "pv-hostpath-1")
	assert.Contains(t, issues[0].Message, "hostPath")
}

// TestCompute_LocalPV_Supported proves the other half of F4's decision: a
// `local` PV is mounted by kubelet under
// pods/<uid>/volumes/kubernetes.io~local-volume/<name> like any other
// plugin (internal/mountinfo.FindKubeletVolume matches structurally, not by
// plugin name) -- the kind e2e's storage class produces exactly this, and
// it must resolve normally, not be rejected alongside hostPath.
func TestCompute_LocalPV_Supported(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pv-local-1")
	pv := localPV("pv-local-1")

	out, issues := Compute(pod, []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
	}, nil, lookupOf(pvc), pvLookupOf(pv))

	require.Empty(t, issues)
	require.Len(t, out, 1)
	assert.Equal(t, "pv-local-1", out[0].KubeletDirName)
}

// TestCompute_PVNotYetCached_FailsOpen proves the documented fail-open
// stance on PV cache lag: pvLookup reporting "not found" (e.g. the PV
// informer hasn't synced it yet) must not block resolution -- it falls
// through exactly as if no PV had been inspected at all, matching
// PVCLookup's own not-found handling elsewhere in this file.
func TestCompute_PVNotYetCached_FailsOpen(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pv-not-cached")

	out, issues := Compute(pod, []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantity("10Mi")}}),
	}, nil, lookupOf(pvc), pvLookupOf())

	require.Empty(t, issues)
	require.Len(t, out, 1)
	assert.Equal(t, "pv-not-cached", out[0].KubeletDirName)
}

func TestCompute_InvalidLimit_IntegerFormAccepted(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	limiters := []storagev1alpha1.IOLimiter{
		limiter("l1", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			ReadBytesPerSecond: quantity("1048576"),
		}}),
	}
	out, issues := Compute(pod, limiters, nil, lookupOf(pvc), pvLookupOf())
	assert.Empty(t, issues)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Limits.ReadBPS)
	assert.EqualValues(t, 1048576, *out[0].Limits.ReadBPS)
}

// TestCompute_D36_InvalidLimit_FreezesOwnPreviousContribution proves D36: a
// limiter that was previously a valid, contributing source for a volume
// must not have its own contribution dropped just because it now carries an
// InvalidLimit field -- its own prior contribution is kept, and it stays
// listed in sources so status still names it. The message must say so.
func TestCompute_D36_InvalidLimit_FreezesOwnPreviousContribution(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: "pvc-1234",
			Limits:  storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)},
			Sources: []string{"admin"},
			Contributions: []storagev1alpha1.VolumeContribution{
				{Source: "admin", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)}},
			}},
	}
	limiters := []storagev1alpha1.IOLimiter{
		limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			WriteBytesPerSecond: quantity("100m"), // a fat-fingered edit, invalid
		}}),
	}

	out, issues := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())

	require.Len(t, out, 1, "the volume must not be dropped: admin has a frozen contribution")
	require.NotNil(t, out[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *out[0].Limits.WriteBPS, "the previous 5Mi must be kept, not loosened to max")
	assert.Equal(t, []string{"admin"}, out[0].Sources, "the invalid-but-previously-contributing limiter stays listed")
	require.Len(t, out[0].Contributions, 1)
	assert.Equal(t, "admin", out[0].Contributions[0].Source)
	require.NotNil(t, out[0].Contributions[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *out[0].Contributions[0].Limits.WriteBPS)

	require.Len(t, issues, 1)
	assert.Equal(t, ReasonInvalidLimit, issues[0].Reason)
	assert.Contains(t, issues[0].Message, "previous limits kept")
}

// TestCompute_D36_Ratchet_RelaxingAValidLimiterTakesEffectDespiteAnotherInvalid
// is the review scenario D36 exists to fix: D35's whole-volume floor reused
// the ENTIRE previous merged Limits as the invalid limiter's floor, so
// relaxing an unrelated, still-valid limiter on the same volume had no
// effect as long as any limiter on that volume stayed invalid (the frozen
// floor kept including the valid limiter's own now-stale value). With
// per-limiter contributions, the invalid limiter only freezes its own prior
// field(s), so relaxing the valid limiter must actually widen the effective
// limit.
func TestCompute_D36_Ratchet_RelaxingAValidLimiterTakesEffectDespiteAnotherInvalid(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	// Previously: "quota" contributed rbps=5Mi (about to go invalid),
	// "admin" contributed wbps=10Mi (valid, and about to be relaxed).
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: "pvc-1234",
			Limits: storagev1alpha1.DeviceLimits{
				ReadBPS: ptr.To[int64](5 * 1024 * 1024), WriteBPS: ptr.To[int64](10 * 1024 * 1024),
			},
			Sources: []string{"admin", "quota"},
			Contributions: []storagev1alpha1.VolumeContribution{
				{Source: "admin", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](10 * 1024 * 1024)}},
				{Source: "quota", Limits: storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](5 * 1024 * 1024)}},
			}},
	}
	limiters := []storagev1alpha1.IOLimiter{
		// quota's field is now invalid (fat-fingered): freezes its own
		// rbps=5Mi contribution, untouched by admin.
		limiter("quota", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			ReadBytesPerSecond: quantity("100m"),
		}}),
		// admin relaxes its own wbps from 10Mi to 20Mi.
		limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			WriteBytesPerSecond: quantity("20Mi"),
		}}),
	}

	out, _ := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())

	require.Len(t, out, 1)
	require.NotNil(t, out[0].Limits.ReadBPS)
	assert.EqualValues(t, 5*1024*1024, *out[0].Limits.ReadBPS, "quota's frozen rbps must still apply")
	require.NotNil(t, out[0].Limits.WriteBPS)
	assert.EqualValues(t, 20*1024*1024, *out[0].Limits.WriteBPS,
		"admin's relax to 20Mi must take effect: quota's frozen contribution never touched wbps")
}

// TestCompute_D36_InvalidLimiterDeleted_EntryDropped proves the other half
// of D36 the whole-volume floor got wrong: once the invalid limiter is
// deleted entirely (not just fixed), its frozen contribution must not
// linger -- it's simply absent from limiters, so it contributes nothing.
func TestCompute_D36_InvalidLimiterDeleted_EntryDropped(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: "pvc-1234",
			Limits:  storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](5 * 1024 * 1024), WriteBPS: ptr.To[int64](10 * 1024 * 1024)},
			Sources: []string{"admin", "quota"},
			Contributions: []storagev1alpha1.VolumeContribution{
				{Source: "admin", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](10 * 1024 * 1024)}},
				{Source: "quota", Limits: storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](5 * 1024 * 1024)}},
			}},
	}
	// "quota" is gone entirely: only admin still names this pod/volume.
	limiters := []storagev1alpha1.IOLimiter{
		limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			WriteBytesPerSecond: quantity("10Mi"),
		}}),
	}

	out, issues := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())

	require.Empty(t, issues, "quota is gone, not invalid: nothing to report")
	require.Len(t, out, 1)
	assert.Nil(t, out[0].Limits.ReadBPS, "quota's contribution must be dropped, not frozen forever")
	require.NotNil(t, out[0].Limits.WriteBPS)
	assert.EqualValues(t, 10*1024*1024, *out[0].Limits.WriteBPS)
	assert.Equal(t, []string{"admin"}, out[0].Sources)
	require.Len(t, out[0].Contributions, 1)
	assert.Equal(t, "admin", out[0].Contributions[0].Source)
}

// TestCompute_D36_InvalidLimit_FixedResumesNormalReconciliation proves the
// other half of D36: once the limiter's field is corrected, the frozen
// contribution is no longer needed and the new value applies normally.
func TestCompute_D36_InvalidLimit_FixedResumesNormalReconciliation(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: "pvc-1234",
			Limits:  storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)},
			Sources: []string{"admin"},
			Contributions: []storagev1alpha1.VolumeContribution{
				{Source: "admin", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)}},
			}},
	}
	limiters := []storagev1alpha1.IOLimiter{
		limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
			WriteBytesPerSecond: quantity("8Mi"),
		}}),
	}

	out, issues := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())

	require.Empty(t, issues)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Limits.WriteBPS)
	assert.EqualValues(t, 8*1024*1024, *out[0].Limits.WriteBPS)
}

// TestCompute_D36_InvalidLimit_NeverPreviouslyASource_NoEntry proves the
// "pods newly matched while the limiter is invalid get no contribution from
// it" half of D36: there is nothing to freeze when the limiter was never a
// contributing source for this volume before (a brand-new match, or a
// volume it never touched), so an all-invalid row still contributes
// nothing and gets no entry, exactly like pre-D35 behaviour, and the
// message never claims a limit was kept.
func TestCompute_D36_InvalidLimit_NeverPreviouslyASource_NoEntry(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")

	tests := []struct {
		name     string
		existing []storagev1alpha1.PodVolumeLimit
	}{
		{name: "no existing PodIOLimit at all", existing: nil},
		{name: "existing volume, but a different source", existing: []storagev1alpha1.PodVolumeLimit{
			{Name: "data", KubeletDirName: "pvc-1234",
				Limits:  storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)},
				Sources: []string{"someone-else"},
				Contributions: []storagev1alpha1.VolumeContribution{
					{Source: "someone-else", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)}},
				}},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limiters := []storagev1alpha1.IOLimiter{
				limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
					WriteBytesPerSecond: quantity("100m"),
				}}),
			}
			out, issues := Compute(pod, limiters, tc.existing, lookupOf(pvc), pvLookupOf())
			assert.Empty(t, out, "nothing to freeze: the volume must be dropped exactly like pre-D35 behaviour")
			require.Len(t, issues, 1)
			assert.NotContains(t, issues[0].Message, "previous limits kept")
		})
	}
}

// TestCompute_D36_InvalidLimit_PerFieldMinimumAgainstFrozenContribution
// proves the frozen contribution merges via the same per-field minimum as
// every other contribution (D2), in both directions: a valid limiter
// tighter than the frozen contribution wins, and the frozen contribution
// tighter than a valid limiter also wins.
func TestCompute_D36_InvalidLimit_PerFieldMinimumAgainstFrozenContribution(t *testing.T) {
	pod := podWithPVC("data", "data-web-0")
	pvc := boundFilesystemPVC("data-web-0", "pvc-1234")
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: "pvc-1234",
			Limits:  storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)},
			Sources: []string{"admin"},
			Contributions: []storagev1alpha1.VolumeContribution{
				{Source: "admin", Limits: storagev1alpha1.DeviceLimits{WriteBPS: ptr.To[int64](5 * 1024 * 1024)}},
			}},
	}

	t.Run("valid limiter tighter than the frozen contribution wins", func(t *testing.T) {
		limiters := []storagev1alpha1.IOLimiter{
			limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
				WriteBytesPerSecond: quantity("100m"), // invalid, freezes at 5Mi
			}}),
			limiter("stricter", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
				WriteBytesPerSecond: quantity("2Mi"), // valid and tighter than the frozen 5Mi
			}}),
		}
		out, _ := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())
		require.Len(t, out, 1)
		require.NotNil(t, out[0].Limits.WriteBPS)
		assert.EqualValues(t, 2*1024*1024, *out[0].Limits.WriteBPS, "2Mi is tighter than the frozen 5Mi and must win")
	})

	t.Run("frozen contribution tighter than a valid limiter wins", func(t *testing.T) {
		limiters := []storagev1alpha1.IOLimiter{
			limiter("admin", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
				WriteBytesPerSecond: quantity("100m"), // invalid, freezes at 5Mi
			}}),
			limiter("looser", storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{
				WriteBytesPerSecond: quantity("20Mi"), // valid but looser than the frozen 5Mi
			}}),
		}
		out, _ := Compute(pod, limiters, existing, lookupOf(pvc), pvLookupOf())
		require.Len(t, out, 1)
		require.NotNil(t, out[0].Limits.WriteBPS)
		assert.EqualValues(t, 5*1024*1024, *out[0].Limits.WriteBPS, "the frozen 5Mi is tighter than 20Mi and must win")
	})
}
