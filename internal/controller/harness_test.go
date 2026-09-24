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
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, storagev1alpha1.AddToScheme(s))
	return s
}

// newFakeClient builds a fake client with every field index this package
// registers on a real manager (index.go), the PodIOLimit/IOLimiter status
// subresources wired up, and the given objects preloaded.
func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&storagev1alpha1.PodIOLimit{}, &storagev1alpha1.IOLimiter{}).
		WithIndex(&storagev1alpha1.PodIOLimit{}, indexPodIOLimitByPodName, indexPodIOLimitByPodNameFunc).
		WithIndex(&storagev1alpha1.PodIOLimit{}, indexPodIOLimitBySource, indexPodIOLimitBySourceFunc).
		WithIndex(&corev1.Pod{}, indexPodByPVCClaimName, indexPodByPVCClaimNameFunc).
		WithObjects(objs...).
		Build()
}

// newConflictOnPatchClient wraps a fresh fake client (preloaded with objs)
// so every Patch call whose object satisfies match fails with a 409
// Conflict, simulating a stale in-memory copy racing a concurrent write
// (D9/D17's optimistic-lock regression coverage). Every other call (Get,
// List, Create, Delete, Status()...) passes through to the fake client
// unchanged.
func newConflictOnPatchClient(t *testing.T, match func(client.Object) bool, objs ...client.Object) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(newFakeClient(t, objs...), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if match(obj) {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"},
					obj.GetName(), errStaleResourceVersion)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

// newConflictOnStatusUpdateClient wraps a fresh fake client (preloaded with
// objs) so every Status().Update call whose object satisfies match fails
// with a 409 Conflict.
func newConflictOnStatusUpdateClient(t *testing.T, match func(client.Object) bool, objs ...client.Object) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(newFakeClient(t, objs...), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" && match(obj) {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "storage.maurice.fr", Resource: "iolimiters"},
					obj.GetName(), errStaleResourceVersion)
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
}

// errStaleResourceVersion is the injected cause behind every conflict the
// interceptor helpers above manufacture.
var errStaleResourceVersion = errors.New("stale resourceVersion")

// errAdmissionDenied is the injected cause standing in for a
// ValidatingAdmissionPolicy denial (F7's motivating example) in the two
// interceptors below.
var errAdmissionDenied = errors.New("admission webhook \"podiolimit-write-restricted\" denied the request")

// newCreateDeniedClient wraps a fresh fake client (preloaded with objs) so
// every Create call whose object satisfies match fails with a Forbidden
// error, simulating a VAP denial (F7's controller-half regression
// coverage). Every other call passes through unchanged.
func newCreateDeniedClient(t *testing.T, match func(client.Object) bool, objs ...client.Object) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(newFakeClient(t, objs...), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if match(obj) {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"},
					obj.GetName(), errAdmissionDenied)
			}
			return c.Create(ctx, obj, opts...)
		},
	})
}

// newPatchDeniedClient is newCreateDeniedClient's Patch equivalent.
func newPatchDeniedClient(t *testing.T, match func(client.Object) bool, objs ...client.Object) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(newFakeClient(t, objs...), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if match(obj) {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: "storage.maurice.fr", Resource: "podiolimits"},
					obj.GetName(), errAdmissionDenied)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

func newPodReconciler(t *testing.T, recorder events.EventRecorder, objs ...client.Object) *PodReconciler {
	t.Helper()
	return &PodReconciler{
		Client:   newFakeClient(t, objs...),
		Scheme:   testScheme(t),
		Recorder: recorder,
	}
}

// newPodReconcilerWithAPIReader builds a PodReconciler whose cached Client
// and live APIReader are backed by two *separate* fake clients (cacheObjs,
// liveObjs), so a test can simulate "the cache hasn't synced this pod yet,
// but a live read would find it" (D30's race fix) without the two ever
// silently sharing state.
func newPodReconcilerWithAPIReader(t *testing.T, recorder events.EventRecorder, cacheObjs []client.Object, liveObjs []client.Object) *PodReconciler {
	t.Helper()
	return &PodReconciler{
		Client:    newFakeClient(t, cacheObjs...),
		Scheme:    testScheme(t),
		Recorder:  recorder,
		APIReader: newFakeClient(t, liveObjs...),
	}
}

func newIOLimiterReconciler(t *testing.T, recorder events.EventRecorder, objs ...client.Object) *IOLimiterReconciler {
	t.Helper()
	return &IOLimiterReconciler{
		Client:   newFakeClient(t, objs...),
		Scheme:   testScheme(t),
		Recorder: recorder,
	}
}

func reconcileRequestFor(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

// scheduledPod builds a live (scheduled, running, non-terminal) pod with a
// deterministic UID, ready to be matched by a selector and to reference
// volumes.
func scheduledPod(ns, name, uid, node string, labels map[string]string, volumes ...corev1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node, Volumes: volumes},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable},
	}
}

func pvcVolume(name, claim string) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		},
	}
}

func boundPVC(ns, name, pvName string) *corev1.PersistentVolumeClaim {
	mode := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName, VolumeMode: &mode},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// nextLimiterUID hands out a fresh UID per newLimiter call. Required so
// D37's globalSelectorCache (keyed by UID+generation, and shared package-
// wide -- including across every test in this binary) never conflates two
// different tests' same-named ("apps"/"postgres-data" is a very common
// fixture name across this file) but differently-selectored limiters, both
// left at their zero-value Generation: a fake client never assigns a real
// UID, so without this every such limiter would collide on UID="".
var nextLimiterUID atomic.Uint64

func newLimiter(ns, name string, selector map[string]string, volumes ...storagev1alpha1.VolumeLimit) *storagev1alpha1.IOLimiter {
	return &storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Generation: 1,
			UID: types.UID(fmt.Sprintf("test-limiter-uid-%d", nextLimiterUID.Add(1))),
		},
		Spec: storagev1alpha1.IOLimiterSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selector},
			Volumes:     volumes,
		},
	}
}

func getPodIOLimit(t *testing.T, c client.Client, ns, name string) *storagev1alpha1.PodIOLimit {
	t.Helper()
	var pil storagev1alpha1.PodIOLimit
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &pil))
	return &pil
}

func listPodIOLimits(t *testing.T, c client.Client, ns string) []storagev1alpha1.PodIOLimit {
	t.Helper()
	var list storagev1alpha1.PodIOLimitList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(ns)))
	return list.Items
}
