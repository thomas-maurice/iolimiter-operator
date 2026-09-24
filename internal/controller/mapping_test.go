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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/stretchr/testify/assert"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

func reqFor(ns, name string) ctrl.Request { return reconcileRequestFor(ns, name) }

// TestMapPodToLimiters proves the IOLimiterReconciler Pod watch mapping:
// every IOLimiter in the pod's namespace is enqueued (Reconcile itself
// decides relevance via the selector, D6.2), namespaces are never crossed,
// and a non-Pod object is a safe no-op rather than a panic.
func TestMapPodToLimiters(t *testing.T) {
	ns := "apps"
	l1 := newLimiter(ns, "l1", map[string]string{"app": "postgres"})
	l2 := newLimiter(ns, "l2", map[string]string{"app": "redis"})
	other := newLimiter("other-ns", "l3", map[string]string{"app": "postgres"})
	pod := scheduledPod(ns, "web-0", "abcd1234", "node-a", map[string]string{"app": "postgres"})

	c := newFakeClient(t, l1, l2, other, pod)
	fn := mapPodToLimiters(c)

	got := fn(context.Background(), pod)
	assert.ElementsMatch(t, []ctrl.Request{reqFor(ns, "l1"), reqFor(ns, "l2")}, got,
		"every IOLimiter in the pod's own namespace must be enqueued, and no other namespace's")

	assert.Nil(t, fn(context.Background(), &corev1.PersistentVolumeClaim{}), "a non-Pod object must be a no-op, not a panic")
}

// TestMapPodIOLimitToLimiters proves the IOLimiterReconciler PodIOLimit
// watch mapping: every distinct IOLimiter name across every volume's
// sources is enqueued exactly once (duplicates across volumes must be
// deduplicated), and a non-PodIOLimit object is a safe no-op.
func TestMapPodIOLimitToLimiters(t *testing.T) {
	ns := "apps"
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: ns},
		Spec: storagev1alpha1.PodIOLimitSpec{
			Volumes: []storagev1alpha1.PodVolumeLimit{
				{Name: "data", Sources: []string{"l1", "l2"}},
				{Name: "wal", Sources: []string{"l1"}}, // "l1" repeated: must not duplicate the request
			},
		},
	}

	fn := mapPodIOLimitToLimiters()
	got := fn(context.Background(), pil)
	assert.ElementsMatch(t, []ctrl.Request{reqFor(ns, "l1"), reqFor(ns, "l2")}, got)

	assert.Nil(t, fn(context.Background(), &corev1.Pod{}), "a non-PodIOLimit object must be a no-op, not a panic")
}

// TestMapIOLimiterToPods_SelectorMatch proves the ordinary case: every pod
// in the limiter's namespace whose labels match its podSelector is
// enqueued, and pods that don't match or live in another namespace are
// not.
func TestMapIOLimiterToPods_SelectorMatch(t *testing.T) {
	ns := "apps"
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"})
	matching := scheduledPod(ns, "web-0", "aaaaaaaa", "node-a", map[string]string{"app": "postgres"})
	other := scheduledPod(ns, "cache-0", "bbbbbbbb", "node-a", map[string]string{"app": "redis"})
	otherNS := scheduledPod("other-ns", "web-0", "cccccccc", "node-a", map[string]string{"app": "postgres"})

	c := newFakeClient(t, l, matching, other, otherNS)
	got := mapIOLimiterToPods(c)(context.Background(), l)

	assert.ElementsMatch(t, []ctrl.Request{reqFor(ns, "web-0")}, got)
}

// TestMapIOLimiterToPods_SourcesIndexFallback proves the D10 case this
// mapping exists for: even when the limiter's own selector can't be
// evaluated (e.g. a malformed/edge-case object, which is exactly what a
// deleted limiter can look like on the watch path), pods are still found
// through the PodIOLimit sources index -- D10 has no finalizer on
// IOLimiter, so this index is the only way a deleted limiter's
// already-affected pods get re-evaluated and their PodIOLimit released.
func TestMapIOLimiterToPods_SourcesIndexFallback(t *testing.T) {
	ns := "apps"
	// An invalid selector operator makes metav1.LabelSelectorAsSelector
	// return an error, so the selector-based half of the mapping can't run
	// at all -- exactly the condition the sources-index fallback exists
	// for.
	l := &storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres-data", Namespace: ns},
		Spec: storagev1alpha1.IOLimiterSpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: "NotAValidOperator"},
				},
			},
		},
	}
	if _, err := metav1.LabelSelectorAsSelector(&l.Spec.PodSelector); err == nil {
		t.Fatal("test fixture invalid: expected an unparseable selector")
	}

	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-aaaaaaaa", Namespace: ns},
		Spec: storagev1alpha1.PodIOLimitSpec{
			PodName: "web-0",
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "data", Sources: []string{"postgres-data"}}},
		},
	}
	// A pod that still exists (and would have matched, if the selector
	// could be evaluated) must not be double-counted with the index result.
	pod := scheduledPod(ns, "web-0", "aaaaaaaa", "node-a", map[string]string{"app": "postgres"})

	c := newFakeClient(t, l, pil, pod)
	got := mapIOLimiterToPods(c)(context.Background(), l)

	assert.ElementsMatch(t, []ctrl.Request{reqFor(ns, "web-0")}, got,
		"the sources index must still find the affected pod even though the selector couldn't be evaluated")
}

// TestMapPVCToPods proves the PVC watch mapping: every pod indexed as
// referencing the PVC's claim name (direct or generic-ephemeral, D16) is
// enqueued, pods referencing a different claim are not, and a non-PVC
// object is a safe no-op.
func TestMapPVCToPods(t *testing.T) {
	ns := "apps"
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	usesIt := scheduledPod(ns, "web-0", "aaaaaaaa", "node-a", nil, pvcVolume("data", "data-web-0"))
	alsoUsesIt := scheduledPod(ns, "web-1", "bbbbbbbb", "node-a", nil, pvcVolume("data", "data-web-0"))
	unrelated := scheduledPod(ns, "web-2", "cccccccc", "node-a", nil, pvcVolume("data", "some-other-claim"))

	c := newFakeClient(t, pvc, usesIt, alsoUsesIt, unrelated)
	got := mapPVCToPods(c)(context.Background(), pvc)

	assert.ElementsMatch(t, []ctrl.Request{reqFor(ns, "web-0"), reqFor(ns, "web-1")}, got)

	assert.Nil(t, mapPVCToPods(c)(context.Background(), &corev1.Pod{}), "a non-PVC object must be a no-op, not a panic")
}

// TestMapPodIOLimitToPod proves D30's fix: a PodIOLimit is mapped to its
// pod's reconcile key purely from its own spec.podName/namespace, with no
// dependency on an ownerRef -- this is what lets an orphan (no ownerRef at
// all) reach PodReconciler.Reconcile in the first place, instead of only
// ever being noticed via Owns()' ownerRef-driven watch.
func TestMapPodIOLimitToPod(t *testing.T) {
	ns := "apps"
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: ns},
		Spec:       storagev1alpha1.PodIOLimitSpec{PodName: "web-0"},
	}

	got := mapPodIOLimitToPod()(context.Background(), pil)
	assert.Equal(t, []ctrl.Request{reqFor(ns, "web-0")}, got,
		"must map to the pod named in spec.podName regardless of ownerRef")

	noPodName := &storagev1alpha1.PodIOLimit{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}}
	assert.Nil(t, mapPodIOLimitToPod()(context.Background(), noPodName), "an empty spec.podName must be a no-op")

	assert.Nil(t, mapPodIOLimitToPod()(context.Background(), &corev1.Pod{}), "a non-PodIOLimit object must be a no-op, not a panic")
}
