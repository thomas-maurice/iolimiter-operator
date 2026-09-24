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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// TestIOLimiterReconciler_AllApplied proves the D20 happy path: a matched
// pod whose PodIOLimit already reports every relevant volume Applied
// yields Ready=True/AllApplied and the correct counts -- this is the state
// `kubectl get iol` must show once everything has converged.
func TestIOLimiterReconciler_AllApplied(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: ns, Generation: 1},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "web-0", PodUID: pod.UID, QOSClass: corev1.PodQOSBurstable,
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "data", KubeletDirName: "pv-1", Sources: []string{"postgres-data"}}},
		},
		Status: storagev1alpha1.PodIOLimitStatus{
			ObservedGeneration: 1,
			Volumes:            []storagev1alpha1.VolumeStatus{{Name: "data", State: "Applied"}},
		},
	}

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, pod, pvc, l, pil)

	ctx, lines := capturingLogger(1)
	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 1, out.Status.MatchedPods)
	assert.EqualValues(t, 1, out.Status.AppliedPods)
	assert.EqualValues(t, 0, out.Status.FailedPods)
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, "AllApplied", ready.Reason)
	assert.EqualValues(t, out.Generation, out.Status.ObservedGeneration)

	assert.True(t, containsAll(*lines, "IOLimiter Ready transition", "AllApplied"))
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "Ready")
	default:
		t.Fatal("expected a Ready event")
	}
}

// TestIOLimiterReconciler_NoMatchingPods proves the explicit D20/§4.1
// reason for an idle limiter: zero matched pods must still be Ready=True
// (nothing is wrong, there's simply nothing to do), not stuck Pending
// forever.
func TestIOLimiterReconciler_NoMatchingPods(t *testing.T) {
	ns := "apps"
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	r := newIOLimiterReconciler(t, nil, l)

	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, "NoMatchingPods", ready.Reason)
}

// TestIOLimiterReconciler_UnsupportedVolume_FailedAndEvent proves D20's
// controller-side-issue path end to end: an emptyDir volume the limiter
// names must show up as a failed pod on the limiter (never silently
// dropped) and emit the UnsupportedVolume warning event the first time it
// appears.
func TestIOLimiterReconciler_UnsupportedVolume_FailedAndEvent(t *testing.T) {
	ns := "apps"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: ns, Labels: map[string]string{"app": "postgres"}},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes:  []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, pod, l)

	ctx, lines := capturingLogger(1)
	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 1, out.Status.MatchedPods)
	assert.EqualValues(t, 1, out.Status.FailedPods)
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "Failed", ready.Reason)
	assert.Contains(t, ready.Message, "web-0")
	assert.Contains(t, ready.Message, "UnsupportedVolumeType")

	assert.True(t, containsAll(*lines, "Controller-side issue", "UnsupportedVolumeType"))

	var sawWarning bool
drain:
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, "UnsupportedVolumeType") {
				sawWarning = true
			}
		default:
			break drain
		}
	}
	assert.True(t, sawWarning, "expected an UnsupportedVolumeType warning event")
}

// TestIOLimiterReconciler_SelectedPodWithoutVolumes_ReportedNotFailed proves
// D38: a pod the podSelector matches but that has none of the limiter's
// listed volumes (evasion via a renamed volume, or simply an unrelated pod
// sharing the label) is neither Failed nor counted in matchedPods -- it's
// surfaced separately via selectedPodsWithoutVolumes, the Ready message,
// and a one-time SelectedPodWithoutVolumes warning event, so an admin can
// tell "the ceiling is not applying to this pod" apart from "nothing here
// matches at all" (NoMatchingPods).
func TestIOLimiterReconciler_SelectedPodWithoutVolumes_ReportedNotFailed(t *testing.T) {
	ns := "apps"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "escapee", Namespace: ns, Labels: map[string]string{"app": "postgres"}},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes:  []corev1.Volume{{Name: "renamed", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, pod, l)

	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 0, out.Status.MatchedPods, "a pod without any listed volume must not count as matched")
	assert.EqualValues(t, 0, out.Status.FailedPods, "must not be treated as a failure")
	assert.EqualValues(t, 1, out.Status.SelectedPodsWithoutVolumes)

	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status, "D38 doesn't change Ready semantics beyond the message")
	assert.Equal(t, "NoMatchingPods", ready.Reason)
	assert.Contains(t, ready.Message, "escapee")
	assert.Contains(t, ready.Message, "SelectedPodWithoutVolumes")

	var sawWarning bool
drain:
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, "SelectedPodWithoutVolumes") {
				sawWarning = true
			}
		default:
			break drain
		}
	}
	assert.True(t, sawWarning, "expected a SelectedPodWithoutVolumes warning event")

	// Second reconcile: the same pod is still without volumes, so no new
	// event should fire (D26: only on new appearance/transition).
	rec2 := events.NewFakeRecorder(10)
	r.Recorder = rec2
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)
	select {
	case ev := <-rec2.Events:
		t.Fatalf("expected no new event on the unchanged transition, got: %s", ev)
	default:
	}
}

// TestIOLimiterReconciler_InvalidSelector_ReportedNotRetried proves D37's
// controller-side fallback: a podSelector that exceeds the bound must not
// be silently ignored or retried forever as an error (metav1's own
// LabelSelectorAsSelector would otherwise happily accept it and let a
// hostile selector reach per-pod matching) -- it's reported as
// Ready=False/InvalidSelector, contributes to no pods, and fires exactly
// one warning event per message.
func TestIOLimiterReconciler_InvalidSelector_ReportedNotRetried(t *testing.T) {
	ns := "apps"
	l := newLimiter(ns, "huge-selector", nil,
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	l.Spec.PodSelector.MatchLabels = make(map[string]string, maxSelectorMatchLabels+1)
	for i := range maxSelectorMatchLabels + 1 {
		l.Spec.PodSelector.MatchLabels[fmt.Sprintf("k%d", i)] = "v"
	}

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, l)

	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "huge-selector"))
	require.NoError(t, err, "an invalid selector is reported on status, never returned as a reconcile error")

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "huge-selector").NamespacedName, &out))
	assert.EqualValues(t, 0, out.Status.MatchedPods)
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "InvalidSelector", ready.Reason)
	assert.Contains(t, ready.Message, "matchLabels")

	var sawWarning bool
drain:
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, "InvalidSelector") {
				sawWarning = true
			}
		default:
			break drain
		}
	}
	assert.True(t, sawWarning, "expected an InvalidSelector warning event")

	// Second reconcile: same message, no new event.
	rec2 := events.NewFakeRecorder(10)
	r.Recorder = rec2
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "huge-selector"))
	require.NoError(t, err)
	select {
	case ev := <-rec2.Events:
		t.Fatalf("expected no new event on the unchanged transition, got: %s", ev)
	default:
	}
}

// TestIOLimiterReconciler_AgentDown_Pending proves D20's explicit
// requirement: "when an agent is down, its pods stay Pending, which is
// visible" -- a PodIOLimit that exists but hasn't been touched by any
// agent (no volume status at all) must count as Pending, not Applied and
// not Failed, so operators can tell "agent hasn't gotten to it" apart from
// both a real failure and success.
func TestIOLimiterReconciler_AgentDown_Pending(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: ns, Generation: 1},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "web-0", PodUID: pod.UID, QOSClass: corev1.PodQOSBurstable,
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "data", KubeletDirName: "pv-1", Sources: []string{"postgres-data"}}},
		},
		// No status written yet: the agent hasn't reconciled it.
	}

	r := newIOLimiterReconciler(t, nil, pod, pvc, l, pil)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 1, out.Status.MatchedPods)
	assert.EqualValues(t, 0, out.Status.AppliedPods)
	assert.EqualValues(t, 0, out.Status.FailedPods)
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "Pending", ready.Reason)
}

// TestIOLimiterReconciler_VolumeNotFound_DoesNotFailPod_PostgresDataWalShape
// proves D31/F11: the shipped Postgres sample lists "data"+"wal", but a
// replica with only a "data" volume (single PVC template, WAL on the same
// volume -- the common case) must count as Applied, not Failed, once its
// "data" rule is live: a VolumeNotFound for the volume the pod doesn't have
// is a note in the message, not a reason to report a live kernel rule as
// broken (the exact regression the coordinator called out by name).
func TestIOLimiterReconciler_VolumeNotFound_DoesNotFailPod_PostgresDataWalShape(t *testing.T) {
	ns := "apps"
	// Only "data", matching a real Postgres StatefulSet with no separate
	// WAL volume -- not "wal", which desired.Compute would report missing.
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("20Mi")}},
		storagev1alpha1.VolumeLimit{Name: "wal", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}},
	)
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: ns, Generation: 1},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "web-0", PodUID: pod.UID, QOSClass: corev1.PodQOSBurstable,
			// desired.Compute never emits a "wal" entry: the pod has none.
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "data", KubeletDirName: "pv-1", Sources: []string{"postgres-data"}}},
		},
		Status: storagev1alpha1.PodIOLimitStatus{
			ObservedGeneration: 1,
			Volumes:            []storagev1alpha1.VolumeStatus{{Name: "data", State: "Applied"}},
		},
	}

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, pod, pvc, l, pil)

	ctx, lines := capturingLogger(1)
	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 1, out.Status.MatchedPods)
	assert.EqualValues(t, 1, out.Status.AppliedPods, "the pod's only present volume is Applied: it must count as applied")
	assert.EqualValues(t, 0, out.Status.FailedPods, "a volume the pod doesn't have must never fail it")

	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, "AllApplied", ready.Reason)
	assert.Contains(t, ready.Message, "wal", "the missing volume must still be visible as a note")
	assert.Contains(t, ready.Message, "VolumeNotFound")

	var sawWarning bool
drain:
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, "VolumeNotFound") {
				sawWarning = true
			}
		default:
			break drain
		}
	}
	assert.True(t, sawWarning, "expected one VolumeNotFound warning event")
	assert.True(t, containsAll(*lines, "Controller-side issue", "VolumeNotFound"))
}

// TestIOLimiterReconciler_VolumeNotFound_StillFailsIfNoVolumeApplied proves
// the other half of D31: if the pod has *none* of the limiter's listed
// volumes applied (e.g. the one volume it does have hasn't been picked up
// by the agent yet), the pod must not be silently reported Applied just
// because the only issue was VolumeNotFound-shaped.
func TestIOLimiterReconciler_VolumeNotFound_StillFailsIfNoVolumeApplied(t *testing.T) {
	ns := "apps"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: ns, Labels: map[string]string{"app": "postgres"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"}, // no volumes at all
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "wal", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	// podHasAnyListedVolume requires the pod to have at least one listed
	// volume name to count as matched at all -- it has none here, so this
	// pod is simply not matched. This documents that boundary rather than
	// asserting a Failed pod (D20/§6.2's own definition of "matched").
	r := newIOLimiterReconciler(t, nil, pod, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 0, out.Status.MatchedPods)
}

// TestIOLimiterReconciler_PodIOLimitWriteFailed_FoldedIntoFailed proves
// F7's IOLimiterReconciler half: a PodIOLimitWriteFailed condition (set
// directly by PodReconciler on a Create/Patch denial) must be folded into
// the aggregated Failed count and message -- otherwise the pod for which
// nothing could even be created would silently read as Pending forever,
// exactly the opacity F7/F8 exist to fix.
func TestIOLimiterReconciler_PodIOLimitWriteFailed_FoldedIntoFailed(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{
		Type: conditionPodIOLimitWriteFailed, Status: metav1.ConditionTrue,
		ObservedGeneration: l.Generation, Reason: "PodIOLimitCreateFailed",
		Message: "pod web-0: PodIOLimitCreateFailed: admission denied",
	})

	r := newIOLimiterReconciler(t, nil, pod, pvc, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &out))
	assert.EqualValues(t, 1, out.Status.FailedPods)
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, "Failed", ready.Reason)
	assert.Contains(t, ready.Message, "admission denied")
}

// TestIOLimiterReconciler_ReadyTransition_SecondReconcileLogsAndEmits is a
// regression test for a pointer-aliasing bug found while writing C5's e2e
// narrative/event assertions (test/e2e/operator/TestIOLimiterEndToEnd):
// `oldReady := meta.FindStatusCondition(cr.Status.Conditions, "Ready")`
// captured a *pointer* into cr.Status.Conditions, and
// meta.SetStatusCondition (called later, via setIOLimiterReady) mutates an
// existing condition of the same Type *in place* rather than replacing it.
// So on any reconcile after the first (once a "Ready" condition already
// exists), oldReady silently aliased the just-written newReady value by
// the time the two were compared, and the transition was never logged or
// evented even though the Ready reason genuinely changed. Real symptom
// on a live cluster: an IOLimiter that went Pending -> AllApplied only
// ever logged "to": "Pending" and never emitted a Ready event.
func TestIOLimiterReconciler_ReadyTransition_SecondReconcileLogsAndEmits(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: ns, Generation: 1},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "web-0", PodUID: pod.UID, QOSClass: corev1.PodQOSBurstable,
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "data", KubeletDirName: "pv-1", Sources: []string{"postgres-data"}}},
		},
		// No status yet: the first reconcile below must land on Pending.
	}

	rec := events.NewFakeRecorder(10)
	r := newIOLimiterReconciler(t, rec, pod, pvc, l, pil)
	req := reconcileRequestFor(ns, "postgres-data")

	// First reconcile: <none> -> Pending. Drain its (NotReady) event so the
	// buffered channel only holds what the second reconcile produces.
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	select {
	case <-rec.Events:
	default:
		t.Fatal("expected a NotReady event on the first (nil -> Pending) transition")
	}

	// The agent applies the volume: flip the PodIOLimit to Applied and
	// reconcile again. The Ready reason must move Pending -> AllApplied,
	// which must be logged and evented -- this is exactly the step the
	// aliasing bug silently dropped.
	var live storagev1alpha1.PodIOLimit
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pil), &live))
	live.Status.ObservedGeneration = live.Generation
	live.Status.Volumes = []storagev1alpha1.VolumeStatus{{Name: "data", State: "Applied"}}
	require.NoError(t, r.Status().Update(context.Background(), &live))

	ctx, lines := capturingLogger(1)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	var out storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), req.NamespacedName, &out))
	ready := meta.FindStatusCondition(out.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, "AllApplied", ready.Reason)

	assert.Truef(t, containsAll(*lines, "IOLimiter Ready transition", "Pending", "AllApplied"),
		"second reconcile must log the Pending -> AllApplied transition, got: %v", *lines)
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "Ready")
	default:
		t.Fatal("expected a second (Ready) event on the Pending -> AllApplied transition")
	}
}

// TestIOLimiterReconciler_StatusConflict_RequeuesAtV1 drives Reconcile's
// own IsConflict branch directly (D17): a Status().Update conflict must
// requeue without a Go error, log only at V(1) (never Info, never an
// error-level line), and -- per the "transitions are only emitted after a
// successful status write" acceptance bullet -- must not log a Ready
// transition or emit a Ready/NotReady event, even though this reconcile
// would otherwise have caused one (0 -> 1 matched pods).
func TestIOLimiterReconciler_StatusConflict_RequeuesAtV1(t *testing.T) {
	ns := "apps"
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})

	r := newIOLimiterReconciler(t, nil, l)
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	var settled storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), reconcileRequestFor(ns, "postgres-data").NamespacedName, &settled))
	readyBefore := meta.FindStatusCondition(settled.Status.Conditions, "Ready")
	require.NotNil(t, readyBefore)
	assert.Equal(t, "NoMatchingPods", readyBefore.Reason, "setup must start Ready/NoMatchingPods so the next reconcile would otherwise transition")

	// Add a matching pod: without the conflict, the next reconcile would
	// move Ready from NoMatchingPods to Pending (matchedPods 0 -> 1). Every
	// Status().Update on this IOLimiter now fails with a Conflict.
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	isThisLimiter := func(obj client.Object) bool {
		cr, ok := obj.(*storagev1alpha1.IOLimiter)
		return ok && cr.Name == "postgres-data" && cr.Namespace == ns
	}
	r.Client = newConflictOnStatusUpdateClient(t, isThisLimiter, &settled, pod, pvc)
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec

	ctx, lines := capturingLogger(1)
	res, err := r.Reconcile(ctx, reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err, "a status conflict must never surface as a reconcile error")
	assert.Greater(t, res.RequeueAfter.Nanoseconds(), int64(0), "a conflict must requeue")

	for _, line := range *lines {
		assert.NotContains(t, line, `"level"=0`, "a conflict must never log at Info: %s", line)
	}
	assert.True(t, containsAll(*lines, "Status update conflict"), "the conflict must be logged at V(1); got: %v", *lines)
	for _, line := range *lines {
		assert.NotContains(t, line, "Ready transition", "a conflicted status write must not log a transition: %s", line)
	}

	select {
	case ev := <-rec.Events:
		t.Fatalf("expected no event on a conflicted status write, got: %s", ev)
	default:
	}

	// The stored status must be untouched by the failed write.
	var afterConflict storagev1alpha1.IOLimiter
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "postgres-data"}, &afterConflict))
	readyAfter := meta.FindStatusCondition(afterConflict.Status.Conditions, "Ready")
	require.NotNil(t, readyAfter)
	assert.Equal(t, "NoMatchingPods", readyAfter.Reason, "a conflicted write must never partially land")
}
