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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/internal/iomax"
)

func testGetCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}

// testUID must be a real UUID (D39: the agent now rejects a non-UUID
// podUID with Failed/InvalidSpec before it ever reaches cgroup path math).
const testUID = "abcabcab-1234-5678-9abc-def012345678"

func oneVolumeLimit(name, dirName string, readBPS int64) storagev1alpha1.PodVolumeLimit {
	return storagev1alpha1.PodVolumeLimit{
		Name:           name,
		KubeletDirName: dirName,
		Limits:         storagev1alpha1.DeviceLimits{ReadBPS: new(readBPS)},
	}
}

// TestReconcile_NewPodIOLimit_IntentBeforeKernelWrite proves D8: the
// device is recorded Pending in status BEFORE the fake io.max changes.
// The kernel is only touched on THIS reconcile's second half, so we assert
// the final state (Applied) and that the intent line was logged before the
// apply line, in the same reconcile.
func TestReconcile_NewPodIOLimit_IntentBeforeKernelWrite(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)

	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)

	intentIdx, applyIdx := -1, -1
	for i, l := range lines {
		if intentIdx == -1 && containsAll([]string{l}, "Recording device intent", `"state"="Pending"`) {
			intentIdx = i
		}
		if applyIdx == -1 && containsAll([]string{l}, `"op"="apply"`) {
			applyIdx = i
		}
	}
	require.NotEqual(t, -1, intentIdx, "intent line must be logged; got: %v", lines)
	require.NotEqual(t, -1, applyIdx, "apply line must be logged; got: %v", lines)
	assert.Less(t, intentIdx, applyIdx, "intent must be logged before the kernel write")

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Devices, 1)
	assert.Equal(t, "8:0", got.Status.Devices[0].Device)
	assert.Equal(t, "Applied", got.Status.Devices[0].State)
	assert.Contains(t, got.Status.Devices[0].Rule, "rbps=1048576")

	line := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.Contains(t, line, "8:0 rbps=1048576 wbps=max riops=max wiops=max")

	select {
	case ev := <-f.recorder.Events:
		assert.Contains(t, ev, "IOLimitApplied")
	default:
		t.Fatal("expected an IOLimitApplied event")
	}
}

// TestReconcile_IntentWriteBeforeKernelWrite_ProvenByOrdering strengthens
// F16 item 3: the log-line-order assertion in
// TestReconcile_NewPodIOLimit_IntentBeforeKernelWrite can't tell a real
// ordering guarantee from a log statement that happens to be typed first
// but whose write moved elsewhere. This proves the actual API call (the
// intent Status().Update) happens-before the actual kernel call (ioMaxWrite)
// by recording both into one shared, monotonically increasing sequence
// counter from inside the client interceptor and the ioMaxWrite seam
// themselves -- not from anything logged.
func TestReconcile_IntentWriteBeforeKernelWrite_ProvenByOrdering(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	var seq atomic.Int32
	var intentSeq, kernelSeq atomic.Int32
	intentSeq.Store(-1)
	kernelSeq.Store(-1)

	origWrite := ioMaxWrite
	defer func() { ioMaxWrite = origWrite }()
	ioMaxWrite = func(path, dev string, rule iomax.Rule) (iomax.WriteResult, error) {
		kernelSeq.Store(seq.Add(1))
		return origWrite(path, dev, rule)
	}

	subUpdateCount := 0
	f.reconciler.Client = interceptor.NewClient(f.client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			subUpdateCount++
			if subUpdateCount == 1 {
				// The first SubResource("status").Update of a fresh
				// PodIOLimit is always the intent write (§6.1 step 7):
				// it happens before any kernel write is attempted.
				intentSeq.Store(seq.Add(1))
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)

	require.NotEqual(t, int32(-1), intentSeq.Load(), "intent status write must have happened")
	require.NotEqual(t, int32(-1), kernelSeq.Load(), "kernel write must have happened")
	assert.Less(t, intentSeq.Load(), kernelSeq.Load(), "D8: the API status write recording intent must happen before the kernel write, not just be logged first")
}

// TestReconcile_StatusConflict_KernelUntouched proves D8's optimistic-lock
// requirement: when the intent Status().Update conflicts, Reconcile
// requeues and never reaches the kernel. The conflict is injected via an
// interceptor on the underlying client's first SubResource("status").Update
// call, since Reconcile always Gets a fresh copy at the top and a
// single-threaded test can't otherwise race it against itself.
func TestReconcile_StatusConflict_KernelUntouched(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	conflicted := false
	f.reconciler.Client = interceptor.NewClient(f.client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !conflicted {
				conflicted = true
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName(), errors.New("simulated conflict"))
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	res, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, requeueSoon, res.RequeueAfter)
	assert.True(t, containsAll(lines, "Status conflict recording device intent"), "got: %v", lines)
	for _, l := range lines {
		assert.NotContains(t, l, `"op"="apply"`, "kernel must not be written on a status conflict")
	}

	assert.Empty(t, readIOMax(t, filepath.Join(podCg, "io.max")), "no kernel write must have reached io.max")
}

// TestReconcile_FirstApply_FinalStatusWriteNeverConflicts proves F6: the
// final writeStatusIfChanged must not overwrite the fresh resourceVersion
// recordDeviceIntent's own live Status().Update already set on pil with a
// stale one re-read through the (simulated-lagging) cache. The plain Get
// used by writeStatusIfChanged is intercepted to return an artificially
// stale ResourceVersion, standing in for real informer cache lag -- with
// F6's fix, that stale value is only used for the semantic-equality
// comparison, never written back onto pil, so the second (final)
// SubResource("status").Update must never conflict.
func TestReconcile_FirstApply_FinalStatusWriteNeverConflicts(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	getCount := 0
	conflictCount := 0
	f.reconciler.Client = interceptor.NewClient(f.client, interceptor.Funcs{
		Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cli.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			getCount++
			// The first Get is Reconcile's own top-of-function fetch;
			// only fudge the SECOND one (writeStatusIfChanged's re-Get)
			// to simulate a cache that lags behind the live write
			// recordDeviceIntent just made.
			if getCount == 2 {
				if p, ok := obj.(*storagev1alpha1.PodIOLimit); ok {
					p.ResourceVersion = "1"
				}
			}
			return nil
		},
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			err := cli.SubResource(subResourceName).Update(ctx, obj, opts...)
			if apierrors.IsConflict(err) {
				conflictCount++
			}
			return err
		},
	})

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, 0, conflictCount, "a first apply must not 409 on the final status write")

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Devices, 1)
	assert.Equal(t, "Applied", got.Status.Devices[0].State)
}

// TestReconcile_EventsOnlyAfterSuccessfulStatusWrite proves D26/F6: a
// forced conflict on the FINAL status write must produce zero Pod events,
// even though the reconcile computed a real Applied transition -- the
// transition never became visible server-side, so nothing should have
// been announced yet either. The retried reconcile (not exercised by this
// test) is what actually emits it once the write succeeds.
func TestReconcile_EventsOnlyAfterSuccessfulStatusWrite(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	subUpdateCount := 0
	f.reconciler.Client = interceptor.NewClient(f.client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			subUpdateCount++
			// Let the intent write (call 1) through; conflict the final
			// status write (call 2).
			if subUpdateCount == 2 {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName(), errors.New("simulated conflict"))
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	res, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, requeueSoon, res.RequeueAfter)

	select {
	case ev := <-f.recorder.Events:
		t.Fatalf("no event must be emitted when the status write conflicts, got: %s", ev)
	default:
	}
}

// TestReconcile_ConflictedFinalWrite_EventReemittedExactlyOnceOnRetry is the
// event-loss regression this fix batch addresses: a conflict on the final
// status write (proven event-free by
// TestReconcile_EventsOnlyAfterSuccessfulStatusWrite above) must not lose
// the transition forever. The kernel write already landed before the
// conflicted write (D8), so the retried reconcile finds curLine == line --
// before this fix, applyDevice treated that as a pure no-op resync and
// never emitted IOLimitApplied at all. This proves the retry emits the
// event exactly once, and a further steady-state resync emits nothing.
func TestReconcile_ConflictedFinalWrite_EventReemittedExactlyOnceOnRetry(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	subUpdateCount := 0
	f.reconciler.Client = interceptor.NewClient(f.client, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			subUpdateCount++
			// Let the intent write (call 1) through; conflict the final
			// status write (call 2) -- the kernel write already happened.
			if subUpdateCount == 2 {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName(), errors.New("simulated conflict"))
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	res, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, requeueSoon, res.RequeueAfter)
	select {
	case ev := <-f.recorder.Events:
		t.Fatalf("no event must be emitted when the status write conflicts, got: %s", ev)
	default:
	}

	// Retry, plain client this time: kernel already satisfies the rule, but
	// persisted status never recorded it (the conflict wiped that out).
	f.reconciler.Client = f.client
	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "Kernel already satisfies desired rule"), "got: %v", lines)

	evs := drainEvents(f.recorder)
	require.Len(t, evs, 1, "the lost transition must be re-emitted exactly once, got: %v", evs)
	// Applied vs Updated is decided by whether the device was already
	// recorded as owned (ownedBefore.Device != "") -- here it was, since
	// the conflicted reconcile's own (successful) intent write already
	// recorded it Pending before the final write conflicted, so this
	// reads as IOLimitUpdated rather than IOLimitApplied; either way, the
	// point this test proves is that exactly one event fires, not zero.
	assert.True(t, strings.Contains(evs[0], "IOLimitApplied") || strings.Contains(evs[0], "IOLimitUpdated"), "got: %s", evs[0])

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Devices, 1)
	assert.Equal(t, "Applied", got.Status.Devices[0].State)

	// Steady-state resync: no further events.
	_, _, err = f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Empty(t, drainEvents(f.recorder), "a genuine no-op resync must emit nothing")
}

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestReconcile_TwoVolumesTwoDevices_TwoWrites is the K1 regression: each
// device gets its own write() call with its own rule, not one joined
// multi-line write. Each write's own before/after read-back (captured in
// its log line, taken at the moment of that write) is the reliable signal
// here: io.max is a real cgroupfs file backed by kernel-maintained
// per-device state, where a later write() to a different device doesn't
// erase an earlier one -- a fixture built from a plain OS file can't
// reproduce that multi-device aggregation (iomax.Write, unchanged from C1,
// issues one bare O_WRONLY write() per call, matching the kernel's actual
// contract; see C1's own test plan, which defers multi-device read-back to
// C3's real-kernel e2e). So this test asserts per-write correctness via the
// log lines and via status, not by re-reading a joint two-device file.
func TestReconcile_TwoVolumesTwoDevices_TwoWrites(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024),
		oneVolumeLimit("wal", "pv-2", 2*1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	f.addMount(t, f.mountinfoSoFar(t), testUID, "kubernetes.io~csi", "pv-2", "8:16")

	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)

	assert.True(t, containsAll(lines, `"op"="apply"`, `"device"="8:0"`, `"rule"="8:0 rbps=1048576 wbps=max riops=max wiops=max"`, `"after"="8:0 rbps=1048576 wbps=max riops=max wiops=max"`),
		"got: %v", lines)
	assert.True(t, containsAll(lines, `"op"="apply"`, `"device"="8:16"`, `"rule"="8:16 rbps=2097152 wbps=max riops=max wiops=max"`, `"after"="8:16 rbps=2097152 wbps=max riops=max wiops=max"`),
		"got: %v", lines)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Devices, 2)
	require.Len(t, got.Status.Volumes, 2)
	for _, v := range got.Status.Volumes {
		assert.Equal(t, "Applied", v.State)
	}
	for _, d := range got.Status.Devices {
		assert.Equal(t, "Applied", d.State)
	}
}

// TestReconcile_TwoVolumesOneDevice_SharedDeviceMerge proves D7: one
// min-merged rule, both volume statuses name the shared device.
func TestReconcile_TwoVolumesOneDevice_SharedDeviceMerge(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 5*1024*1024),
		oneVolumeLimit("wal", "pv-2", 2*1024*1024))
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	f.addMount(t, f.mountinfoSoFar(t), testUID, "kubernetes.io~csi", "pv-2", "8:0")

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)

	content := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.Contains(t, content, "8:0 rbps=2097152 wbps=max riops=max wiops=max", "min of 5Mi/2Mi must be 2Mi")

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Devices, 1)
	require.Len(t, got.Status.Volumes, 2)
	for _, v := range got.Status.Volumes {
		assert.Equal(t, "Applied", v.State)
		assert.Equal(t, "SharedDevice", v.Reason)
		assert.Equal(t, "8:0", v.Device)
	}
}

// TestReconcile_VolumeRemoved_OnlyThatDeviceReset proves D8's "only reset
// what we own": removing one volume from spec resets only its device, and
// a device already in io.max but never listed in status.devices is never
// written or reset by this PodIOLimit. "Never touched" is asserted by
// absence from the log (no apply/reset line ever names "8:99"), not by
// re-reading a joint multi-device file -- see
// TestReconcile_TwoVolumesTwoDevices_TwoWrites's comment on why a plain
// file can't stand in for the kernel's per-device io.max state once more
// than one write() with different content has landed on it.
func TestReconcile_VolumeRemoved_OnlyThatDeviceReset(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024),
		oneVolumeLimit("wal", "pv-2", 2*1024*1024))
	f := newFixture(t, pil)
	// A device pre-exists in io.max, owned by "someone else" (never in
	// this PodIOLimit's status.devices).
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "8:99 rbps=999 wbps=max riops=max wiops=max\n")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	f.addMount(t, f.mountinfoSoFar(t), testUID, "kubernetes.io~csi", "pv-2", "8:16")

	_, lines1, err := f.reconcile(t, pil)
	require.NoError(t, err)
	for _, l := range lines1 {
		assert.NotContains(t, l, "8:99", "an untracked device must never appear in a write/reset log line")
	}

	// Now remove "wal" from spec and reconcile again.
	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	got.Spec.Volumes = got.Spec.Volumes[:1]
	require.NoError(t, f.client.Update(t.Context(), got))

	_, lines2, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.True(t, containsAll(lines2, `"op"="reset"`, `"device"="8:16"`), "reset line for 8:16 must be logged; got: %v", lines2)
	for _, l := range lines2 {
		assert.NotContains(t, l, "8:99", "an untracked device must never appear in a write/reset log line")
	}
	// 8:0 is not asserted "unwritten" here: the fixture's plain-file io.max
	// already lost its 8:0 line to the second apply() in the previous
	// reconcile (same file-corruption caveat as
	// TestReconcile_TwoVolumesTwoDevices_TwoWrites), so a resync-driven
	// reapply of 8:0 on this pass is an artifact of the test double, not a
	// behavior this test can distinguish from a real drift correction.
	// What's actually under test -- only 8:16 gets reset, 8:99 is never
	// touched, and 8:0 ends up Applied in status -- is asserted above/below.

	final := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, final.Status.Devices, 1)
	assert.Equal(t, "8:0", final.Status.Devices[0].Device)
	assert.Equal(t, "Applied", final.Status.Devices[0].State)
}

// TestReconcile_Deletion_ResetsOwnedThenReleased proves §6.1 step 11.
func TestReconcile_Deletion_ResetsOwnedThenReleased(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	pil.Finalizers = []string{"storage.maurice.fr/io-max-reset"}
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	require.NoError(t, f.client.Delete(t.Context(), pil))

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.NotNil(t, got.DeletionTimestamp)

	_, lines, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "Release starting"), "got: %v", lines)
	assert.True(t, containsAll(lines, `"op"="reset"`, `"device"="8:0"`), "got: %v", lines)
	assert.True(t, containsAll(lines, "Release finished"), "got: %v", lines)

	content := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.NotContains(t, content, "rbps=1048576", "the applied limit must be gone after reset")
	assert.Contains(t, content, "8:0 rbps=max wbps=max riops=max wiops=max", "K3: the reset line is all-max")

	final := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	assert.Empty(t, final.Status.Devices)
	ready := findReady(final.Status.Conditions)
	require.NotNil(t, ready)
	assert.Equal(t, "Released", ready.Reason)

	assert.True(t, drainEventsContain(f.recorder, "IOLimitReset"), "expected an IOLimitReset event on release")
}

// TestReconcile_Deletion_MissingCgroup_ReleasedWithoutError proves
// iomax.Reset's ENOENT handling covers "missing cgroup counts as reset".
func TestReconcile_Deletion_MissingCgroup_ReleasedWithoutError(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	pil.Finalizers = []string{"storage.maurice.fr/io-max-reset"}
	pil.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "8:0 rbps=1048576 wbps=max riops=max wiops=max", State: "Applied"}}
	f := newFixture(t, pil)
	require.NoError(t, f.client.Status().Update(t.Context(), pil))
	require.NoError(t, f.client.Delete(t.Context(), pil))
	// No pod cgroup directory is ever created: it's already gone.

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	_, _, err := f.reconcile(t, got)
	require.NoError(t, err)

	final := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	assert.Empty(t, final.Status.Devices)
	ready := findReady(final.Status.Conditions)
	require.NotNil(t, ready)
	assert.Equal(t, "Released", ready.Reason)
}

// TestReconcile_Deletion_ZeroDevices_StillWritesReleased proves the D9
// consistency fix: a PodIOLimit deleted before it ever owned a device
// still ends up Ready=False/Released, not silently untouched status.
func TestReconcile_Deletion_ZeroDevices_StillWritesReleased(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	pil.Finalizers = []string{"storage.maurice.fr/io-max-reset"}
	f := newFixture(t, pil)
	require.NoError(t, f.client.Delete(t.Context(), pil))

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Empty(t, got.Status.Devices)

	_, _, err := f.reconcile(t, got)
	require.NoError(t, err)

	final := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	ready := findReady(final.Status.Conditions)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "Released", ready.Reason)
}

// TestReconcile_Drift_RewrittenAndCounted proves D11: an external write to
// io.max is corrected on resync and increments the drift metric.
func TestReconcile_Drift_RewrittenAndCounted(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)

	// External drift: something else rewrites the line.
	writeFile(t, filepath.Join(podCg, "io.max"), "8:0 rbps=1 wbps=max riops=max wiops=max\n")

	before := testGetCounterValue(t, driftCorrectionsTotal)
	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	_, lines, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "Drift corrected", "8:0"), "got: %v", lines)

	content := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.Contains(t, content, "rbps=1048576")
	after := testGetCounterValue(t, driftCorrectionsTotal)
	assert.Equal(t, before+1, after)
}

// TestReconcile_KernelRejected_EventOnlyOnTransition proves D26: a write
// that keeps failing the same way on every resync (the kernel keeps
// rejecting it) logs "Kernel write failed"/KernelRejected every time (so
// the narrative doesn't go silent), but emits IOLimitFailed only once, on
// the first failure -- never again while the failure is unchanged.
func TestReconcile_KernelRejected_EventOnlyOnTransition(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	orig := ioMaxWrite
	t.Cleanup(func() { ioMaxWrite = orig })
	ioMaxWrite = func(_, _ string, _ iomax.Rule) (iomax.WriteResult, error) {
		return iomax.WriteResult{Before: "<none>"}, errors.New("invalid argument")
	}

	_, lines1, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines1, "Kernel write failed", "KernelRejected"), "got: %v", lines1)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Failed", got.Status.Volumes[0].State)
	assert.Equal(t, "KernelRejected", got.Status.Volumes[0].Reason)

	// Second reconcile: the kernel rejects it again, same reason. The log
	// line must still appear (retries are real activity), but no second
	// event.
	_, lines2, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.True(t, containsAll(lines2, "Kernel write failed", "KernelRejected"), "got: %v", lines2)

	assert.Equal(t, 1, drainEventCount(f.recorder, "IOLimitFailed"), "IOLimitFailed must fire exactly once across two identical failures")
}

// TestReconcile_KernelMismatch_EventOnlyOnTransition proves the same D26
// transition-gating for K6's read-back mismatch.
func TestReconcile_KernelMismatch_EventOnlyOnTransition(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	orig := ioMaxWrite
	t.Cleanup(func() { ioMaxWrite = orig })
	ioMaxWrite = func(_, dev string, _ iomax.Rule) (iomax.WriteResult, error) {
		// The kernel silently ignored the value (K6): read-back differs
		// from what was asked for.
		return iomax.WriteResult{Before: "<none>", After: dev + " rbps=max wbps=max riops=max wiops=max"}, nil
	}

	_, lines1, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines1, "Kernel write", `"op"="apply"`), "got: %v", lines1)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Failed", got.Status.Volumes[0].State)
	assert.Equal(t, "KernelMismatch", got.Status.Volumes[0].Reason)

	_, _, err = f.reconcile(t, got)
	require.NoError(t, err)

	assert.Equal(t, 1, drainEventCount(f.recorder, "IOLimitFailed"), "IOLimitFailed must fire exactly once across two identical mismatches")
}

// TestReconcile_LogLines_CarryPodKey proves D26: every Info line the agent
// emits -- resolution, intent, op=apply, op=reset and drift -- carries
// pod=<ns>/<name>, so `grep <ns>/<pod>` reads as a complete narrative for
// one pod across every line kind, not just some of them.
func TestReconcile_LogLines_CarryPodKey(t *testing.T) {
	const podKey = `"pod"="apps/web-0"`

	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024),
		oneVolumeLimit("wal", "pv-2", 2*1024*1024))
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	f.addMount(t, f.mountinfoSoFar(t), testUID, "kubernetes.io~csi", "pv-2", "8:16")

	// Reconcile 1: resolution lines, the intent line, and two op=apply lines.
	_, lines1, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines1, "Volume resolution", podKey), "resolution line missing pod=: %v", lines1)
	assert.True(t, containsAll(lines1, "Recording device intent", podKey), "intent line missing pod=: %v", lines1)
	assert.True(t, containsAll(lines1, `"op"="apply"`, `"device"="8:0"`, podKey), "apply line (8:0) missing pod=: %v", lines1)
	assert.True(t, containsAll(lines1, `"op"="apply"`, `"device"="8:16"`, podKey), "apply line (8:16) missing pod=: %v", lines1)

	// Reconcile 2: remove "wal" -> op=reset line for 8:16.
	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	got.Spec.Volumes = got.Spec.Volumes[:1]
	require.NoError(t, f.client.Update(t.Context(), got))
	_, lines2, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.True(t, containsAll(lines2, `"op"="reset"`, `"device"="8:16"`, podKey), "reset line missing pod=: %v", lines2)

	// External drift on 8:0, then reconcile 3: drift-corrected line.
	writeFile(t, filepath.Join(podCg, "io.max"), "8:0 rbps=1 wbps=max riops=max wiops=max\n")
	got2 := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	_, lines3, err := f.reconcile(t, got2)
	require.NoError(t, err)
	assert.True(t, containsAll(lines3, "Drift corrected", podKey), "drift line missing pod=: %v", lines3)
}

// TestReconcile_NoDriftResync_LogsNothingAtInfo proves D26: an unchanged
// resync logs nothing at Info.
func TestReconcile_NoDriftResync_LogsNothingAtInfo(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	_, lines, err := f.reconcile(t, got)
	require.NoError(t, err)
	for _, l := range lines {
		assert.NotContains(t, l, `"level":0`, "an Info line was logged on a no-op resync: %s", l)
	}
}

// TestReconcile_SharedDevice_ResolutionLoggedOnceNotEveryResync is a
// regression for the reported "Volume resolution" duplicate on a
// self-triggered resync: for volumes sharing one device (D7), the
// persisted status.volumes[].reason is "SharedDevice" (set by the apply
// loop), but a fresh resolution's own Reason is always "" (resolveVolume
// never sets one for a successfully-resolved volume) -- comparing them
// directly would never match and log Info on every single reconcile
// forever, not just once. Proves it logs "Volume resolution" at Info on
// the first reconcile only, and stays quiet (V(1)) on every resync after.
func TestReconcile_SharedDevice_ResolutionLoggedOnceNotEveryResync(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 5*1024*1024),
		oneVolumeLimit("wal", "pv-2", 2*1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	f.addMount(t, f.mountinfoSoFar(t), testUID, "kubernetes.io~csi", "pv-2", "8:0")

	_, lines1, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, 2, countOccurrences(lines1, "Volume resolution"), "both shared-device volumes resolve for the first time: got %v", lines1)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 2)
	for _, v := range got.Status.Volumes {
		require.Equal(t, "SharedDevice", v.Reason, "precondition: status must actually carry the SharedDevice reason this test is regression-testing against")
	}

	_, lines2, err := f.reconcile(t, got)
	require.NoError(t, err)
	assert.Equal(t, 0, countOccurrences(lines2, "Volume resolution"), "an unchanged shared-device resolution must not log at Info on resync: got %v", lines2)
}

func countOccurrences(lines []string, substr string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// TestReconcile_AnotherNode_Ignored proves step 1's defense in depth.
func TestReconcile_AnotherNode_Ignored(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, "node-b",
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)

	res, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	assert.Empty(t, got.Status.Devices)
	assert.Empty(t, got.Status.Volumes)
}

// TestReconcile_NonUUIDPodUID_FailedInvalidSpec proves D39: a podUID that
// isn't a UUID (an object written around the CRD pattern, or predating it)
// must be rejected before it ever reaches cgroup path math -- Layout.PodPath
// builds an arbitrary string straight into a filesystem path, so an
// unvalidated podUID is an injection surface, not just a cosmetic issue.
func TestReconcile_NonUUIDPodUID_FailedInvalidSpec(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", "not-a-uuid", corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)

	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "podUID is not a valid UUID"), "got: %v", lines)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Failed", got.Status.Volumes[0].State)
	assert.Equal(t, "InvalidSpec", got.Status.Volumes[0].Reason)
	assert.Empty(t, got.Status.Devices)

	ready := findReady(got.Status.Conditions)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
}

// TestReconcile_RootDevice_UnsupportedUnlessAllowed proves D23.
func TestReconcile_RootDevice_UnsupportedUnlessAllowed(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	// "/" is on 8:0, and the volume's kubelet mount is also on 8:0.
	content := "1 1 8:0 / / rw,relatime - ext4 /dev/sda1 rw\n"
	content = f.addMount(t, content, testUID, "kubernetes.io~csi", "pv-1", "8:0")
	writeFile(t, filepath.Join(f.procRoot, "1", "mountinfo"), content)

	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "SharedRootDevice"), "got: %v", lines)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Unsupported", got.Status.Volumes[0].State)
	assert.Equal(t, "SharedRootDevice", got.Status.Volumes[0].Reason)
	assert.Empty(t, got.Status.Devices)

	// Coordinator review (C2): every volume Unsupported must NOT report
	// Ready=True/Applied -- nothing was actually applied.
	ready := findReady(got.Status.Conditions)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "Unsupported", ready.Reason)

	select {
	case ev := <-f.recorder.Events:
		assert.Contains(t, ev, "IOLimitSkipped")
	default:
		t.Fatal("expected an IOLimitSkipped event")
	}

	// Now allow it and reconcile a fresh fixture with the same layout: it
	// must apply. (A fresh fixture, not the same one, since its
	// kubelet-root-dir path is baked into the mountinfo fixture below and
	// must match this fixture's own KubeletRootDir, not f's.)
	f2 := newFixture(t)
	f2.reconciler.AllowRootDevice = true
	podCg := f2.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	rootContent := "1 1 8:0 / / rw,relatime - ext4 /dev/sda1 rw\n"
	f2.addMount(t, rootContent, testUID, "kubernetes.io~csi", "pv-1", "8:0")
	pil2 := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	require.NoError(t, f2.client.Create(t.Context(), pil2))

	_, _, err = f2.reconcile(t, pil2)
	require.NoError(t, err)
	iomaxContent := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.Contains(t, iomaxContent, "8:0 rbps=1048576")
}

// TestReconcile_MajorZero_Unsupported proves K5.
func TestReconcile_MajorZero_Unsupported(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "0:5")

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Unsupported", got.Status.Volumes[0].State)
	assert.Equal(t, "NoBlockDevice", got.Status.Volumes[0].Reason)
}

// TestReconcile_Partition_ResolvesToWholeDisk_LogsPartitionOf proves K4:
// a mounted partition resolves to its whole disk, and the resolution log
// line/VolumeStatus record the partition mapping (mountDevice/partitionOf).
func TestReconcile_Partition_ResolvesToWholeDisk_LogsPartitionOf(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	podCg := f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:1")
	f.partitionDevice(t, "8:0", "8:1")

	_, lines, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.True(t, containsAll(lines, "Volume resolution", `"mountDevice"="8:1"`, `"partitionOf"="8:0"`, `"device"="8:0"`),
		"got: %v", lines)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Applied", got.Status.Volumes[0].State)
	assert.Equal(t, "8:0", got.Status.Volumes[0].Device)
	assert.Equal(t, "8:1", got.Status.Volumes[0].MountDevice)

	content := readIOMax(t, filepath.Join(podCg, "io.max"))
	assert.Contains(t, content, "8:0 rbps=1048576")
}

// TestReconcile_CgroupNotFound_PendingRequeue2s proves §6.1 step 3.
func TestReconcile_CgroupNotFound_PendingRequeue2s(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	// No pod cgroup directory created at all.

	res, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, pendingRequeueInterval, res.RequeueAfter)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Pending", got.Status.Volumes[0].State)
	assert.Equal(t, "CgroupNotFound", got.Status.Volumes[0].Reason)
}

// TestReconcile_CgroupStillNotFound_RequeueGrows proves F4 end to end: a
// volume that keeps being Pending (here: the cgroup never appears) must
// see its requeue interval grow past the flat pendingRequeueInterval once
// the Ready condition's LastTransitionTime shows it's been Pending a
// while -- not by waiting for real time to pass, but by seeding that
// timestamp directly, which is exactly what a real long-Pending object's
// status already carries.
func TestReconcile_CgroupStillNotFound_RequeueGrows(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	pil.Status.Conditions = []metav1.Condition{{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "CgroupNotFound",
		LastTransitionTime: metav1.NewTime(time.Now().Add(-20 * time.Second)),
	}}
	f := newFixture(t, pil)
	// No pod cgroup directory created at all: stays Pending/CgroupNotFound,
	// so this reconcile's status write is a no-op and the seeded
	// LastTransitionTime is preserved.

	res, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter, pendingRequeueInterval, "20s of Pending must have grown the requeue past the flat 2s base")
	assert.LessOrEqual(t, res.RequeueAfter, f.reconciler.ResyncPeriod, "must never exceed --resync-period")
}

// TestReconcile_IOControllerDisabled_Failed proves §6.1 step 3's second half.
func TestReconcile_IOControllerDisabled_Failed(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	p, err := f.layout.PodPath(testUID, corev1.PodQOSBurstable)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p, 0o755)) // no io.max file.

	_, _, err = f.reconcile(t, pil)
	require.NoError(t, err)

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.Len(t, got.Status.Volumes, 1)
	assert.Equal(t, "Failed", got.Status.Volumes[0].State)
	assert.Equal(t, "IOControllerDisabled", got.Status.Volumes[0].Reason)
}

// TestReconcile_NotFound_RecordsHealth_ObjectsGoneKeepsLiveness proves F5:
// after the last PodIOLimit on this node is deleted, the NotFound
// reconcile must still record health progress (hasObjects reflecting the
// now-empty cache) so D24(b)'s liveness check doesn't later fail just
// because there is nothing left to reconcile.
func TestReconcile_NotFound_RecordsHealth_ObjectsGoneKeepsLiveness(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")

	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	f.reconciler.Health = h

	// First reconcile: the object exists, applies successfully.
	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	require.NoError(t, h.livezCheck(nil))

	// Delete it (as the controller would once the finalizer clears), then
	// reconcile the NotFound.
	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.NoError(t, f.client.Delete(t.Context(), got))
	_, _, err = f.reconcile(t, pil)
	require.NoError(t, err)

	// Without F5's fix, hasObjects/lastReconcileDone would have frozen at
	// the pre-delete reconcile's values, and this would fail livez ~3
	// periods later even though there is nothing left to reconcile.
	clk.advance(4 * 10 * time.Second)
	h.recordSelfCheck(true) // keep D24(a) satisfied; only (b) is under test.
	assert.NoError(t, h.livezCheck(nil), "no PodIOLimit left on the node must not be reported unhealthy")
}

// TestReconcile_AnotherNode_StillRecordsHealth proves F5's "unconditional"
// framing: even the defense-in-depth "ignore another node's object" path
// (which never reaches the write logic) must still record reconcile
// progress, since it's a real reconcile that completed.
func TestReconcile_AnotherNode_StillRecordsHealth(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, "other-node",
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	h, _ := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	f.reconciler.Health = h

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	require.NoError(t, h.livezCheck(nil), "a completed (ignored) reconcile must count as progress")
}

// TestReconcile_OwnedDevicesGauge_NotStaleAfterLastObjectGone proves F8:
// iolimiter_agent_owned_devices must be refreshed on the NotFound/release
// path too, sharing F5's fix (the same unconditional health defer also
// re-lists the cache and sets this gauge), so it doesn't keep reporting a
// stale non-zero count once the last PodIOLimit on the node is gone.
func TestReconcile_OwnedDevicesGauge_NotStaleAfterLastObjectGone(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)
	f.podCgroupDir(t, testUID, corev1.PodQOSBurstable, "")
	f.addMount(t, "", testUID, "kubernetes.io~csi", "pv-1", "8:0")
	h, _ := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	f.reconciler.Health = h

	_, _, err := f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, float64(1), testutil.ToFloat64(ownedDevices), "the applied device must be counted")

	got := getPodIOLimit(t, f.client, "apps", "web-0-abc")
	require.NoError(t, f.client.Delete(t.Context(), got))
	_, _, err = f.reconcile(t, pil)
	require.NoError(t, err)
	assert.Equal(t, float64(0), testutil.ToFloat64(ownedDevices), "gauge must drop to 0, not stay stuck at the last non-zero value")
}

// TestCacheHasObjects_ListError_KeepsPreviousValue proves a transient List
// error must not itself flip D24 liveness state: it must not be treated the
// same as "the cache genuinely holds zero objects" (which would wrongly
// report hasObjects=false and could mask a real liveness problem, or the
// reverse -- either way, a List error carries no information about what the
// cache actually holds, so the previous known value is kept).
func TestCacheHasObjects_ListError_KeepsPreviousValue(t *testing.T) {
	pil := basePodIOLimit("apps", "web-0-abc", "web-0", testUID, corev1.PodQOSBurstable, testNode,
		oneVolumeLimit("data", "pv-1", 1024*1024))
	f := newFixture(t, pil)

	assert.True(t, f.reconciler.cacheHasObjects(t.Context()), "the cache holds one PodIOLimit")

	failingClient := interceptor.NewClient(f.client, interceptor.Funcs{
		List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.New("simulated transient list error")
		},
	})
	f.reconciler.Client = failingClient
	assert.True(t, f.reconciler.cacheHasObjects(t.Context()), "a List error must keep the previous known value, not flip to false")
}

// TestPendingBackoff proves F4: the Pending poll must grow rather than
// stay a flat 2s forever (a volume that never mounts would otherwise poll
// -- and re-parse mountinfo -- indefinitely), while staying at the base
// interval for the first few retries and never exceeding max
// (--resync-period, which already re-checks everything).
func TestPendingBackoff(t *testing.T) {
	max := 30 * time.Second
	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"first reconcile", 0, pendingRequeueInterval},
		{"still within the first interval", pendingRequeueInterval, pendingRequeueInterval},
		{"grown once", 4 * time.Second, 4 * time.Second},
		{"grown further", 8 * time.Second, 8 * time.Second},
		{"capped at max", time.Hour, max},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pendingBackoff(tt.elapsed, max))
		})
	}
}

// TestPendingBackoff_ZeroOrSmallMaxFallsBackToBase proves pendingBackoff
// never returns a requeue interval below its own base when --resync-period
// is unset/misconfigured smaller than the base interval.
func TestPendingBackoff_ZeroOrSmallMaxFallsBackToBase(t *testing.T) {
	assert.Equal(t, pendingRequeueInterval, pendingBackoff(time.Hour, 0))
	assert.Equal(t, pendingRequeueInterval, pendingBackoff(time.Hour, time.Second))
}

// mountinfoSoFar returns the current mountinfo file content so tests can
// chain multiple addMount calls.
func (f *testFixture) mountinfoSoFar(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.procRoot, "1", "mountinfo"))
	require.NoError(t, err)
	return string(b)
}
