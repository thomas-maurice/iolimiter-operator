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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// capturingLogger returns a funcr-backed logr.Logger and the slice its
// lines land in, so tests can assert exactly what was logged at which
// level (D26's acceptance: capturing-logger assertions on keys/levels).
func capturingLogger(verbosity int) (context.Context, *[]string) {
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{Verbosity: verbosity})
	return logf.IntoContext(context.Background(), logger), &lines
}

func containsAll(lines []string, substrs ...string) bool {
	for _, l := range lines {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(l, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TestPodReconciler_CreatesPodIOLimit_D18Name proves the happy path end to
// end: a live pod matching a limiter gets exactly one PodIOLimit, named per
// D18, owned by the pod (controller:true, blockOwnerDeletion:false), with
// the D9 finalizer already attached and the merged limits from the limiter.
// This is the base case every other PodReconciler test builds on.
func TestPodReconciler_CreatesPodIOLimit_D18Name(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	rec := events.NewFakeRecorder(10)
	r := newPodReconciler(t, rec, pod, pvc, l)
	ctx, lines := capturingLogger(1)

	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.Equal(t, "node-a", pil.Spec.NodeName)
	assert.Equal(t, "web-0", pil.Spec.PodName)
	assert.Equal(t, pod.UID, pil.Spec.PodUID)
	require.Len(t, pil.Spec.Volumes, 1)
	assert.Equal(t, "pv-1", pil.Spec.Volumes[0].KubeletDirName)
	require.NotNil(t, pil.Spec.Volumes[0].Limits.ReadBPS)
	assert.EqualValues(t, 10*1024*1024, *pil.Spec.Volumes[0].Limits.ReadBPS)
	assert.True(t, controllerutil.ContainsFinalizer(pil, finalizerName))
	require.Len(t, pil.OwnerReferences, 1)
	assert.Equal(t, "Pod", pil.OwnerReferences[0].Kind)
	assert.True(t, *pil.OwnerReferences[0].Controller)
	assert.False(t, *pil.OwnerReferences[0].BlockOwnerDeletion, "D18: blockOwnerDeletion must be false")
	assert.Equal(t, managedByValue, pil.Labels[managedByLabel])

	assert.True(t, containsAll(*lines, "PodIOLimit created", "apps/web-0"), "create must be logged at Info with the pod key; got: %v", *lines)

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "PodIOLimitCreated")
	default:
		t.Fatal("expected a PodIOLimitCreated event on the IOLimiter")
	}
}

// TestPodReconciler_NoOpReconcile_LogsNothingAtInfo proves D26's "a
// replayed identical reconcile logs nothing at Info" acceptance line: once
// a PodIOLimit already matches the desired spec, re-running Reconcile must
// not touch the API or produce any Info-level noise (only a V(1) no-op
// line), or every resync period would flood the logs with lines that carry
// no new information.
func TestPodReconciler_NoOpReconcile_LogsNothingAtInfo(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	ctx, lines := capturingLogger(1)
	_, err = r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	for _, l := range *lines {
		assert.NotContains(t, l, `"level"=0`, "an Info line was emitted on a no-op reconcile: %s", l)
	}
	assert.True(t, containsAll(*lines, "No-op reconcile"), "expected the V(1) no-op line; got: %v", *lines)
}

// TestPodReconciler_LimiterEdit_PatchesNotFullUpdate proves D17: an edited
// limiter's tightened value reaches the PodIOLimit via a merge patch, and
// the diff summary names the changed volume -- getting this wrong (a full
// Update) is exactly the reference project's CEL round-tripping gotcha
// D17 calls out.
func TestPodReconciler_LimiterEdit_PatchesNotFullUpdate(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	before := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	rvBefore := before.ResourceVersion

	l.Spec.Volumes[0].Limits.ReadBytesPerSecond = quantityPtr("5Mi")
	require.NoError(t, r.Update(context.Background(), l))

	ctx, lines := capturingLogger(1)
	_, err = r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	after := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	require.NotNil(t, after.Spec.Volumes[0].Limits.ReadBPS)
	assert.EqualValues(t, 5*1024*1024, *after.Spec.Volumes[0].Limits.ReadBPS)
	assert.NotEqual(t, rvBefore, after.ResourceVersion)
	assert.True(t, containsAll(*lines, "PodIOLimit patched", "volumesChanged"), "expected a diff-summary patch log; got: %v", *lines)
}

// TestPodReconciler_DeleteLimiter_KeepsPodIOLimitFinalizerForAgent proves
// D9/D10's core invariant: deleting the last IOLimiter targeting a pod must
// delete the PodIOLimit (nothing desired any more) but must NOT strip its
// finalizer -- the agent still has to reset the kernel rule. Removing the
// finalizer here would leave a stale io.max rule with no record anywhere.
func TestPodReconciler_DeleteLimiter_KeepsPodIOLimitFinalizerForAgent(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	require.NoError(t, r.Delete(context.Background(), l))

	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.NotNil(t, pil.DeletionTimestamp, "the PodIOLimit must be Terminating")
	assert.True(t, controllerutil.ContainsFinalizer(pil, finalizerName), "the finalizer must survive until the agent releases it")
}

// TestPodReconciler_TerminatingPodIOLimit_ReleasedOnceDevicesEmpty proves
// the other half of D9: once the agent has reset every device (status
// .devices empty), the controller must release the finalizer itself --
// this is the only place that ever removes it, so if this doesn't fire the
// object is stuck forever.
func TestPodReconciler_TerminatingPodIOLimit_ReleasedOnceDevicesEmpty(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NoError(t, r.Delete(context.Background(), l))
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	// Simulate the agent finishing the release (D9 step 11): devices: [].
	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	pil.Status.Devices = nil
	require.NoError(t, r.Status().Update(context.Background(), pil))

	ctx, lines := capturingLogger(1)
	_, err = r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &storagev1alpha1.PodIOLimit{})
	assert.True(t, apiIsNotFound(getErr), "the object must be fully gone once GC processes the last finalizer removal")
	assert.True(t, containsAll(*lines, "Finalizer removed", "agent released"))
}

// TestPodReconciler_TerminatingPodIOLimit_WaitsWhileDevicesOwned proves the
// converse: while the agent still owns devices, the controller must not
// touch the finalizer, or a rule could be stranded in the kernel with
// nothing left to prove it was ever ours.
func TestPodReconciler_TerminatingPodIOLimit_WaitsWhileDevicesOwned(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NoError(t, r.Delete(context.Background(), l))
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	pil.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=max wbps=max riops=max wiops=max", State: "Applied"}}
	require.NoError(t, r.Status().Update(context.Background(), pil))

	res, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	assert.Greater(t, res.RequeueAfter.Nanoseconds(), int64(0), "must requeue instead of touching the finalizer")

	still := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.True(t, controllerutil.ContainsFinalizer(still, finalizerName))
}

// TestPodReconciler_PodRecreated_ReleasesOldCreatesNew proves D18's
// recreated-pod invariant: a StatefulSet pod deleted and recreated gets a
// new UID, so the old PodIOLimit (now orphaned -- its cgroup is gone) must
// be released directly (no agent handshake needed, D9's dead-node path),
// and a fresh PodIOLimit created for the new UID, never reusing or
// colliding with the old one.
func TestPodReconciler_PodRecreated_ReleasesOldCreatesNew(t *testing.T) {
	ns := "apps"
	oldUID := "aaaaaaaa-0000-0000-0000-000000000000"
	pod := scheduledPod(ns, "web-0", oldUID, "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	oldName := "web-0-aaaaaaaa"
	getPodIOLimit(t, r.Client, ns, oldName) // sanity: exists

	// Recreate the pod with a new UID (simulate StatefulSet pod recreate).
	require.NoError(t, r.Delete(context.Background(), pod))
	newUID := "bbbbbbbb-0000-0000-0000-000000000000"
	newPod := scheduledPod(ns, "web-0", newUID, "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	require.NoError(t, r.Create(context.Background(), newPod))

	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	newName := "web-0-bbbbbbbb"
	newPil := getPodIOLimit(t, r.Client, ns, newName)
	assert.Equal(t, types.UID(newUID), newPil.Spec.PodUID)

	oldErr := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: oldName}, &storagev1alpha1.PodIOLimit{})
	assert.True(t, apiIsNotFound(oldErr), "the old PodIOLimit must be gone (finalizer released directly, no agent handshake)")
}

// TestPodReconciler_PodGone_ReleasesFinalizerDirectly proves D9's other
// dead-node path: once the pod itself is gone, its PodIOLimit's finalizer
// must be released immediately, without ever waiting on status.devices --
// there is no cgroup left for any agent to reset.
func TestPodReconciler_PodGone_ReleasesFinalizerDirectly(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	// Give the PodIOLimit non-empty status.devices to prove the direct
	// release does NOT wait on it (unlike the normal D9 handshake).
	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	pil.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=max wbps=max riops=max wiops=max", State: "Applied"}}
	require.NoError(t, r.Status().Update(context.Background(), pil))

	require.NoError(t, r.Delete(context.Background(), pod))

	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &storagev1alpha1.PodIOLimit{})
	assert.True(t, apiIsNotFound(getErr), "the PodIOLimit must be released immediately once the pod is gone")
}

// TestPodReconciler_UnscheduledPod_NoPodIOLimit proves step 3: a pod with
// no nodeName yet has no cgroup for any agent to resolve, so no PodIOLimit
// must be created for it even if it already matches a limiter.
func TestPodReconciler_UnscheduledPod_NoPodIOLimit(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	assert.Empty(t, listPodIOLimits(t, r.Client, ns))
}

// TestPodReconciler_FinalizerRemovalConflict_Requeues is the D9 regression
// test: an optimistic-lock finalizer removal against a stale in-memory
// copy must conflict (and requeue), never silently overwrite a concurrent
// write (e.g. the agent's own status update landing in between). The
// conflict must requeue without an error, log only at V(1) (never Info,
// never an error-level line), and must not emit any event -- a conflict
// means "retry on a fresh copy", not "something happened to release".
func TestPodReconciler_FinalizerRemovalConflict_Requeues(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NoError(t, r.Delete(context.Background(), l))
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	require.NotNil(t, pil.DeletionTimestamp, "setup must leave the PodIOLimit Terminating before the conflict is exercised")
	require.Empty(t, pil.Status.Devices, "status.devices must already be empty so the next reconcile attempts the finalizer removal")

	// From here on, every Patch on this PodIOLimit fails with a Conflict:
	// this is what a stale in-memory copy racing a concurrent write (e.g.
	// the agent's own status update landing between Get and Patch) looks
	// like from the reconciler's side.
	isThisPIL := func(obj client.Object) bool {
		pil, ok := obj.(*storagev1alpha1.PodIOLimit)
		return ok && pil.Name == "web-0-abcd1234" && pil.Namespace == ns
	}
	r.Client = newConflictOnPatchClient(t, isThisPIL, pod, pvc, pil)
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec

	ctx, lines := capturingLogger(1)
	res, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err, "a conflict must never surface as a reconcile error")
	assert.Greater(t, res.RequeueAfter.Nanoseconds(), int64(0), "a conflict must requeue")

	still := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.True(t, controllerutil.ContainsFinalizer(still, finalizerName),
		"a conflicted patch must never silently succeed: the finalizer must survive")

	for _, l := range *lines {
		assert.NotContains(t, l, `"level"=0`, "a conflict must never log at Info: %s", l)
	}
	assert.True(t, containsAll(*lines, "Finalizer removal conflict"), "the conflict must be logged at V(1); got: %v", *lines)
	for _, l := range *lines {
		assert.NotContains(t, l, "Finalizer removed", "a conflict must not log a release: %s", l)
	}

	select {
	case ev := <-rec.Events:
		t.Fatalf("expected no event on a conflicted finalizer removal, got: %s", ev)
	default:
	}
}

func apiIsNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}

// TestPodReconciler_FinalizerRemovalNotFound_TreatedAsSuccess is item 5's
// chaos-finding regression test: a pod deleted while the agent was down
// races ownerRef GC's own delete of the PodIOLimit against this
// reconciler's dead-pod release path. removeFinalizerOptimistic's initial
// Get already treats a NotFound PodIOLimit as "already gone" -- but if the
// object is deleted by something else *between* that Get and the Patch
// below it, the Patch itself 404s. Before this fix that propagated all the
// way up as a reconcile error (matching the reported log line
// `podiolimits... not found`) instead of being treated as success.
func TestPodReconciler_FinalizerRemovalNotFound_TreatedAsSuccess(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NoError(t, r.Delete(context.Background(), l))
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	require.NotNil(t, pil.DeletionTimestamp, "setup must leave the PodIOLimit Terminating before the race is exercised")
	require.Empty(t, pil.Status.Devices, "status.devices must already be empty so the next reconcile attempts the finalizer removal")

	// Simulate the race: the object is genuinely gone (e.g. GC's own delete
	// won) by the time the Patch reaches the apiserver, even though the Get
	// just above still saw it.
	isThisPIL := func(obj client.Object) bool {
		pil, ok := obj.(*storagev1alpha1.PodIOLimit)
		return ok && pil.Name == "web-0-abcd1234" && pil.Namespace == ns
	}
	base := newFakeClient(t, pod, pvc, pil)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cli client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if isThisPIL(obj) {
				return apierrors.NewNotFound(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName())
			}
			return cli.Patch(ctx, obj, patch, opts...)
		},
	})
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec

	ctx, lines := capturingLogger(1)
	res, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err, "a NotFound race must never surface as a reconcile error with backoff")
	assert.Zero(t, res.RequeueAfter, "already gone is success, not a requeue")
	assert.True(t, containsAll(*lines, "already gone"), "got: %v", *lines)
}

// TestPodReconciler_OrphanPodIOLimit_NoOwnerRef_Released is the D30
// regression test for the reported bug: a PodIOLimit with no ownerRef at
// all, pointing at a pod name/UID that doesn't exist, must still be
// reconciled and released -- it must not require an ownerRef-driven
// Owns() event to ever be noticed. reconcileRequestFor uses the same
// (namespace, spec.podName) key mapPodIOLimitToPod would have produced
// from the object's own watch event, proving that path (not Owns()) is
// what makes this reachable at all.
func TestPodReconciler_OrphanPodIOLimit_NoOwnerRef_Released(t *testing.T) {
	ns := "apps"
	orphan := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ghost-11112222",
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			// Deliberately no OwnerReferences: this is exactly the shape
			// of the reported leftover.
		},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a",
			PodName:  "ghost",
			PodUID:   types.UID("11112222-0000-0000-0000-000000000000"),
			QOSClass: "BestEffort",
			Volumes: []storagev1alpha1.PodVolumeLimit{{
				Name: "data", KubeletDirName: "data",
			}},
		},
	}

	r := newPodReconciler(t, nil, orphan)
	ctx, lines := capturingLogger(1)
	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "ghost"))
	require.NoError(t, err)

	getErr := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "ghost-11112222"}, &storagev1alpha1.PodIOLimit{})
	assert.True(t, apiIsNotFound(getErr), "an orphan PodIOLimit (no ownerRef, pod gone) must be released and deleted")
	assert.True(t, containsAll(*lines, "Finalizer removed", "stale UID"),
		"must go through the existing D9 dead-pod release path (no agent wait: there is no live cgroup); got: %v", *lines)
}

// TestPodReconciler_LiveReadAvoidsMisidentifyingNewPodAsOrphan is D30's
// race-fix regression test: if the *cached* client hasn't synced a pod yet
// (simulating the PodIOLimit informer's watch event arriving before the
// Pod informer's own cache is populated), the reconciler must fall back to
// a live read via APIReader before concluding the pod is gone -- otherwise
// a legitimate, freshly created PodIOLimit would be misidentified as an
// orphan and released the moment its owning controller (or, here, a
// hand-crafted equivalent) creates it.
func TestPodReconciler_LiveReadAvoidsMisidentifyingNewPodAsOrphan(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	// First, build the canonical PodIOLimit the normal (pod-in-cache) path
	// would produce, so this test's "no-op" assertion below isn't just
	// coincidentally true.
	setup := newPodReconciler(t, nil, pod, pvc, l)
	_, err := setup.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	canonical := getPodIOLimit(t, setup.Client, ns, "web-0-abcd1234")

	// Now reconcile again from scratch: the cached client has the
	// PodIOLimit, the PVC and the limiter, but deliberately NOT the pod
	// (simulating the Pod informer's cache not having synced it yet). Only
	// the separate live APIReader has the pod.
	r := newPodReconcilerWithAPIReader(t, nil,
		[]client.Object{canonical, pvc, l},
		[]client.Object{pod},
	)
	ctx, lines := capturingLogger(1)
	_, err = r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	still := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.Equal(t, canonical.Spec, still.Spec, "the PodIOLimit must be left exactly as-is: the live read must have found the pod")
	assert.Contains(t, still.Finalizers, finalizerName, "must not have been released: the pod is not actually gone")
	assert.True(t, containsAll(*lines, "No-op reconcile"),
		"a legitimate PodIOLimit whose pod only the live read can see must reconcile as a no-op, not a release; got: %v", *lines)
	for _, line := range *lines {
		assert.NotContains(t, line, "stale UID", "must never take the orphan-release path when the live read finds the pod: %s", line)
	}
}

// TestPodReconciler_TerminatingPod_NoNewPodIOLimitCreated proves F17's
// podLive/DeletionTimestamp fix: a pod already mid graceful termination
// (DeletionTimestamp set, phase still Running -- its cgroup is still very
// much alive) that has no PodIOLimit yet must not get a brand new one
// created for it. Creating one here would only be reset again moments
// later once the pod actually finishes terminating: a full create -> apply
// -> release round trip for nothing.
func TestPodReconciler_TerminatingPod_NoNewPodIOLimitCreated(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now
	// The fake client (matching real apiserver behavior) refuses to store
	// an object with a deletionTimestamp but no finalizers; kubelet always
	// has one on a real pod during termination.
	pod.Finalizers = []string{"kubernetes"}
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newPodReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	var pilList storagev1alpha1.PodIOLimitList
	require.NoError(t, r.List(context.Background(), &pilList, client.InNamespace(ns)))
	assert.Empty(t, pilList.Items, "no PodIOLimit must be created for a pod already terminating")
}

// TestPodReconciler_TerminatingPod_ExistingPodIOLimitStillReleasedByAgent
// proves the safety half of F17's fix: an *existing* PodIOLimit for a pod
// that starts graceful termination must NOT be released directly the
// moment DeletionTimestamp appears -- its cgroup (and the kernel rule) is
// still there for the whole grace period, so D9's finalizer must keep
// waiting for the agent's own reset (status.devices going empty), exactly
// as it does for any other Terminating PodIOLimit. Flipping podLive itself
// to treat DeletionTimestamp as "not live" would instead take the
// immediate releaseDirect path and strip the finalizer with no wait --
// which is precisely the D8/D9 invariant this test guards against.
func TestPodReconciler_TerminatingPod_ExistingPodIOLimitStillReleasedByAgent(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	setup := newPodReconciler(t, nil, pod, pvc, l)
	_, err := setup.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	existing := getPodIOLimit(t, setup.Client, ns, "web-0-abcd1234")
	// The agent still owns a live device: the cgroup exists and the kernel
	// rule is applied.
	existing.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=10485760 wbps=max riops=max wiops=max", State: "Applied"}}
	require.NoError(t, setup.Status().Update(context.Background(), existing))

	// The pod now starts graceful termination, but the limiter is deleted
	// too (desired volumes drop to none) -- the realistic trigger for this
	// PodIOLimit to start being deleted while the pod is still Terminating.
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kubernetes"}

	r := newPodReconciler(t, nil, pod, pvc, existing)
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	// The limiter disappearing means desired volumes drop to none, so the
	// PodIOLimit legitimately starts being deleted (D9's normal "nothing
	// left to limit" path) -- that part is unrelated to the pod's own
	// DeletionTimestamp. What matters, and what F17's fix must not break,
	// is that this is still the *finalizer-holding* D9 handshake, not the
	// unconditional releaseDirect path: the finalizer must survive because
	// status.devices is still non-empty (the agent hasn't reset yet).
	still := getPodIOLimit(t, r.Client, ns, "web-0-abcd1234")
	assert.True(t, controllerutil.ContainsFinalizer(still, finalizerName),
		"the finalizer must survive: the pod's cgroup (and its kernel rule) still exists, so the agent's own reset must be waited on")
}

// TestPodReconciler_CreateDenied_SurfacedOnIOLimiter proves F7's
// controller-half: a Create failure (e.g. a VAP denial) must not only end
// up as the reconcile's returned error (logged once by controller-runtime
// and otherwise invisible) -- it must show up on the IOLimiter that would
// have contributed to the PodIOLimit, as both a condition and a Warning
// event, so `kubectl get iol` reflects it instead of just Pending forever.
func TestPodReconciler_CreateDenied_SurfacedOnIOLimiter(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	rec := events.NewFakeRecorder(10)
	denyEverything := func(client.Object) bool { return true }
	r := &PodReconciler{
		Client:   newCreateDeniedClient(t, denyEverything, pod, pvc, l),
		Recorder: rec,
	}

	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.Error(t, err, "the denied create must still surface as a reconcile error (controller-runtime's own backoff)")

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &out))
	cond := meta.FindStatusCondition(out.Status.Conditions, conditionPodIOLimitWriteFailed)
	require.NotNil(t, cond, "expected a PodIOLimitWriteFailed condition on the IOLimiter")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "PodIOLimitCreateFailed", cond.Reason)
	assert.Contains(t, cond.Message, "web-0")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "PodIOLimitCreateFailed")
	default:
		t.Fatal("expected a PodIOLimitCreateFailed warning event on the IOLimiter")
	}
}

// TestPodReconciler_PatchDenied_SurfacedOnIOLimiter is
// TestPodReconciler_CreateDenied_SurfacedOnIOLimiter's Patch-side
// equivalent (F7): once a PodIOLimit already exists, a denied Patch (e.g.
// the limiter's limits changed) must surface the same way -- a
// PodIOLimitPatchFailed condition plus a Warning event on the IOLimiter.
func TestPodReconciler_PatchDenied_SurfacedOnIOLimiter(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	setup := newPodReconciler(t, nil, pod, pvc, l)
	_, err := setup.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	existing := getPodIOLimit(t, setup.Client, ns, "web-0-abcd1234")

	// Tighten the limit: patchIfDiffer must now see a real diff.
	l.Spec.Volumes[0].Limits.ReadBytesPerSecond = quantityPtr("5Mi")

	rec := events.NewFakeRecorder(10)
	denyEverything := func(client.Object) bool { return true }
	r := &PodReconciler{
		Client:   newPatchDeniedClient(t, denyEverything, pod, pvc, l, existing),
		Recorder: rec,
	}
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.Error(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &out))
	cond := meta.FindStatusCondition(out.Status.Conditions, conditionPodIOLimitWriteFailed)
	require.NotNil(t, cond, "expected a PodIOLimitWriteFailed condition on the IOLimiter")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "PodIOLimitPatchFailed", cond.Reason)

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "PodIOLimitPatchFailed")
	default:
		t.Fatal("expected a PodIOLimitPatchFailed warning event on the IOLimiter")
	}
}

// TestPodReconciler_CreateDenied_ClearedOnSubsequentSuccess proves the
// condition F7 sets is not permanent: once a later Create for the same
// limiter succeeds, the write-failure condition must flip back to False so
// a resolved VAP misconfiguration (or a transient denial) doesn't leave a
// stale Failed signal on the IOLimiter forever.
func TestPodReconciler_CreateDenied_ClearedOnSubsequentSuccess(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	deniedOnce := newCreateDeniedClient(t, func(client.Object) bool { return true }, pod, pvc, l)
	r := &PodReconciler{Client: deniedOnce, Recorder: events.NewFakeRecorder(10)}
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.Error(t, err)

	var midway storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &midway))
	require.NotNil(t, meta.FindStatusCondition(midway.Status.Conditions, conditionPodIOLimitWriteFailed))

	// Swap in a plain (non-denying) client preloaded with the same state
	// the denied attempt left behind (the IOLimiter now carries the
	// condition), and reconcile again: the create must now succeed.
	r.Client = newFakeClient(t, pod, pvc, &midway)
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	var after storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &after))
	cond := meta.FindStatusCondition(after.Status.Conditions, conditionPodIOLimitWriteFailed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "the condition must flip to False once the create succeeds")
}

// TestPodReconciler_CreateDenied_RecordConflictRequeues proves item 2 of
// this fix batch: recordPodIOLimitWriteFailure's own Status().Update on the
// IOLimiter is D17/D26's optimistic-lock write like any other -- a conflict
// there must requeue the reconcile (V(1), not an error), not be silently
// swallowed. Before this fix, a conflict here was logged and dropped,
// meaning a denied Create could go permanently unrecorded on the IOLimiter
// if the one attempt to record it happened to race a concurrent writer.
func TestPodReconciler_CreateDenied_RecordConflictRequeues(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	c := interceptor.NewClient(newFakeClient(t, pod, pvc, l), interceptor.Funcs{
		Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName(), errAdmissionDenied)
		},
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.maurice.fr", Resource: "iolimiters"}, obj.GetName(), errStaleResourceVersion)
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r := &PodReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}

	res, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err, "a conflict recording the write failure must requeue, not surface as a reconcile error")
	assert.Equal(t, requeueSoon, res.RequeueAfter)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &out))
	assert.Nil(t, meta.FindStatusCondition(out.Status.Conditions, conditionPodIOLimitWriteFailed), "the conflicted write never landed; the retry (not exercised here) records it")
}

// TestPodReconciler_ClearWriteFailure_ConflictRequeues is
// TestPodReconciler_CreateDenied_RecordConflictRequeues's clear-side
// equivalent: once a previously-denied Create succeeds,
// clearPodIOLimitWriteFailure's own Status().Update conflicting must also
// requeue rather than leave the stale PodIOLimitWriteFailed=True condition
// stuck with no retry.
func TestPodReconciler_ClearWriteFailure_ConflictRequeues(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{
		Type: conditionPodIOLimitWriteFailed, Status: metav1.ConditionTrue,
		ObservedGeneration: l.Generation, Reason: "PodIOLimitCreateFailed", Message: "a prior denial",
	})

	c := interceptor.NewClient(newFakeClient(t, pod, pvc, l), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				return apierrors.NewConflict(schema.GroupResource{Group: "storage.maurice.fr", Resource: "iolimiters"}, obj.GetName(), errStaleResourceVersion)
			}
			return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r := &PodReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}

	res, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	assert.Equal(t, requeueSoon, res.RequeueAfter)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &out))
	cond := meta.FindStatusCondition(out.Status.Conditions, conditionPodIOLimitWriteFailed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "the conflicted clear must not have landed; it stays True until the retry succeeds")
}

// TestPodReconciler_CreateAlreadyExists_NotSurfacedAsFailure is a
// regression for a concurrent-reconcile race found running the operator
// e2e suite (out of this fix batch's originally assigned scope, but
// blocking a green e2e run): a sibling reconcile for the same pod can
// create the PodIOLimit successfully while this reconcile's own cached
// List-by-podName (Step 1/2) still reads it as absent (informer cache
// propagation lag), so this reconcile's own Create then 409s
// AlreadyExists. That must not be recorded as PodIOLimitWriteFailed --
// nothing ever clears it later, since nothing about the object changes
// again to re-trigger this reconcile -- and must not surface as a
// reconcile error either.
func TestPodReconciler_CreateAlreadyExists_NotSurfacedAsFailure(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	// The object already exists (as if a sibling reconcile just created it),
	// but the reconciler's own List-by-podName index simply won't find it in
	// this fake client if it's actually there -- so match kubebuilder's real
	// cache-lag shape (List doesn't see it) by having the *Create* call
	// itself, not the List, be the one that discovers it, via an
	// interceptor that forces Create to fail AlreadyExists.
	c := interceptor.NewClient(newFakeClient(t, pod, pvc, l), interceptor.Funcs{
		Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*storagev1alpha1.PodIOLimit); ok {
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"}, obj.GetName())
			}
			return cli.Create(ctx, obj, opts...)
		},
	})
	rec := events.NewFakeRecorder(10)
	r := &PodReconciler{Client: c, Recorder: rec}

	res, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err, "AlreadyExists must never surface as a reconcile error")
	assert.Zero(t, res.RequeueAfter, "the object's own Create event already re-triggers a fresh reconcile")

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &out))
	assert.Nil(t, meta.FindStatusCondition(out.Status.Conditions, conditionPodIOLimitWriteFailed), "AlreadyExists must never be recorded as a write failure")

	select {
	case ev := <-rec.Events:
		t.Fatalf("no warning event must fire for a benign concurrent-create race, got: %s", ev)
	default:
	}
}

// TestPodReconciler_D35_InvalidLimitFreezesPodIOLimitSpec is D35's
// controller-level regression test (chaos test S7): editing a limiter's
// writeBytesPerSecond from a valid 5Mi to an invalid "100m" must not loosen
// the PodIOLimit spec (and so the kernel rule the agent applies from it) --
// the spec stays at 5Mi, and the IOLimiter reports InvalidLimit. Fixing the
// limiter to a new valid value resumes normal reconciliation.
func TestPodReconciler_D35_InvalidLimitFreezesPodIOLimitSpec(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	objs := []client.Object{pod, pvc, l}
	podR := newPodReconciler(t, nil, objs...)
	_, err := podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	pil := getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234")
	require.NotNil(t, pil.Spec.Volumes[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *pil.Spec.Volumes[0].Limits.WriteBPS)

	// Edit the limiter to an invalid value (an admin typo).
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, l))
	l.Spec.Volumes[0].Limits.WriteBytesPerSecond = quantityPtr("100m")
	require.NoError(t, podR.Update(context.Background(), l))

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	stillFrozen := getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234")
	require.NotNil(t, stillFrozen.Spec.Volumes[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *stillFrozen.Spec.Volumes[0].Limits.WriteBPS,
		"the PodIOLimit spec must stay at the last valid 5Mi, never loosened by the invalid edit")

	iolR := newIOLimiterReconciler(t, nil, pod, pvc, l, stillFrozen)
	_, err = iolR.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)
	var afterInvalid storagev1alpha1.IOLimiter
	require.NoError(t, iolR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &afterInvalid))
	assert.Contains(t, afterInvalid.Status.Conditions[0].Message, "InvalidLimit", "the IOLimiter must report InvalidLimit while frozen")

	// Fix the limiter: reconciliation must resume normally.
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, l))
	l.Spec.Volumes[0].Limits.WriteBytesPerSecond = quantityPtr("8Mi")
	require.NoError(t, podR.Update(context.Background(), l))

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	fixed := getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234")
	require.NotNil(t, fixed.Spec.Volumes[0].Limits.WriteBPS)
	assert.EqualValues(t, 8*1024*1024, *fixed.Spec.Volumes[0].Limits.WriteBPS, "once fixed, the new valid value must apply")
}

// TestPodReconciler_D37_InvalidSelectorKeepsPodIOLimitContribution is D37's
// controller-level regression test: a limiter that already contributes to a
// pod's PodIOLimit must not lose that contribution just because its own
// podSelector was edited into something invalid (over the D37 bound). Before
// the fix, listMatchingLimiters silently dropped the limiter from the set
// fed to desired.Compute, so its contribution -- and the throttle it
// represented -- vanished even though the limiter still exists.
func TestPodReconciler_D37_InvalidSelectorKeepsPodIOLimitContribution(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	objs := []client.Object{pod, pvc, l}
	podR := newPodReconciler(t, nil, objs...)
	_, err := podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	before := getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234")
	require.NotNil(t, before.Spec.Volumes[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *before.Spec.Volumes[0].Limits.WriteBPS)
	assert.Equal(t, []string{"postgres-data"}, before.Spec.Volumes[0].Sources)

	// Break the selector: an admin edit past D37's bound, not a limits
	// change. Bump Generation explicitly -- the fake client (unlike a real
	// apiserver) never does this on its own, and globalSelectorCache is
	// keyed by UID+generation.
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, l))
	l.Spec.PodSelector = oversizedSelector()
	l.Generation++
	require.NoError(t, podR.Update(context.Background(), l))

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	after := getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234")
	require.NotNil(t, after.Spec.Volumes[0].Limits.WriteBPS)
	assert.EqualValues(t, 5*1024*1024, *after.Spec.Volumes[0].Limits.WriteBPS,
		"an InvalidSelector edit must not drop the limiter's existing contribution")
	assert.Equal(t, []string{"postgres-data"}, after.Spec.Volumes[0].Sources)
	require.Len(t, after.Spec.Volumes[0].Contributions, 1)
	assert.Equal(t, "postgres-data", after.Spec.Volumes[0].Contributions[0].Source)
}

// TestPodReconciler_D37_SelectorFixedButNoLongerMatches_ContributionDropped
// proves the other side: once a limiter's selector is valid again but
// simply no longer matches this pod, its contribution is genuinely dropped
// -- InvalidSelector freezing must not become a permanent ratchet.
func TestPodReconciler_D37_SelectorFixedButNoLongerMatches_ContributionDropped(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	objs := []client.Object{pod, pvc, l}
	podR := newPodReconciler(t, nil, objs...)
	_, err := podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NotEmpty(t, getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234").Spec.Volumes)

	// Re-point the selector at a different, valid, non-matching app label.
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, l))
	l.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "mysql"}}
	l.Generation++
	require.NoError(t, podR.Update(context.Background(), l))

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	// D9: Delete only starts the finalizer handshake (DeletionTimestamp
	// set, spec/status otherwise untouched) -- it does not remove the
	// PodIOLimit within the same reconcile. A second reconcile (with no
	// agent-owned devices in status, since no agent ran in this test)
	// completes the release.
	var terminating storagev1alpha1.PodIOLimit
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &terminating))
	require.NotNil(t, terminating.DeletionTimestamp, "no volumes left to limit must start the release handshake")

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	var pil storagev1alpha1.PodIOLimit
	err = podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &pil)
	assert.True(t, apierrors.IsNotFound(err), "a valid selector that no longer matches must drop the contribution, releasing the PodIOLimit entirely")
}

// TestPodReconciler_D37_LimiterDeleted_ContributionDropped proves a deleted
// limiter's contribution is dropped, not kept -- InvalidSelector's freeze
// only applies to a limiter that still exists.
func TestPodReconciler_D37_LimiterDeleted_ContributionDropped(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	objs := []client.Object{pod, pvc, l}
	podR := newPodReconciler(t, nil, objs...)
	_, err := podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)
	require.NotEmpty(t, getPodIOLimit(t, podR.Client, ns, "web-0-abcd1234").Spec.Volumes)

	require.NoError(t, podR.Delete(context.Background(), l))

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	// D9 handshake, as above: Delete only starts Terminating.
	var terminating storagev1alpha1.PodIOLimit
	require.NoError(t, podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &terminating))
	require.NotNil(t, terminating.DeletionTimestamp, "no volumes left to limit must start the release handshake")

	_, err = podR.Reconcile(context.Background(), reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	var pil storagev1alpha1.PodIOLimit
	err = podR.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web-0-abcd1234"}, &pil)
	assert.True(t, apierrors.IsNotFound(err), "a deleted limiter's contribution must be dropped, releasing the PodIOLimit entirely")
}
