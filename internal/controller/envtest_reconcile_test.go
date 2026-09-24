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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

// startManagerClient starts a real controller-runtime manager against
// envtest (no reconcilers registered: these tests drive Reconcile
// directly, so assertions aren't racing a running controller) with the D16
// field indexes registered, and returns its cache-backed client once the
// cache has synced. PodReconciler/IOLimiterReconciler both rely on
// client.MatchingFields, which only ever works through a cache -- a raw
// client.New forwards MatchingFields as an apiserver fieldSelector, which
// the apiserver rejects for anything but the one CRD-registered
// selectable field (spec.nodeName on PodIOLimit).
func startManagerClient(t *testing.T) client.Client {
	t.Helper()
	cfg := startEnvtestConfig(t)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme(t),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	require.NoError(t, err)
	require.NoError(t, SetupIndexes(context.Background(), mgr.GetFieldIndexer()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = mgr.Start(ctx)
	}()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "manager cache must sync before use")

	return mgr.GetClient()
}

// waitFor polls cond up to 5s: the manager-backed client's writes go
// through the apiserver and its cache read path lags slightly behind, so
// a Get immediately after a Create/Update can legitimately race the
// informer.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, cond(), "condition never became true")
}

func envtestPod(ns, name string, uid types.UID, node string, labels map[string]string, volumes ...corev1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node, Volumes: volumes, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
}

// TestPodReconciler_Envtest_CreateEditDeleteFinalizerHandshake exercises
// the full PodReconciler lifecycle against a real apiserver: create (D18
// name/ownerRef/finalizer/merged limits/kubeletDirName=PV name), an edit
// propagating as a patch (generation bumps, no full Update trips CEL
// identity immutability), and the D9 delete handshake (finalizer held
// while status.devices is non-empty, released once it's empty).
func TestPodReconciler_Envtest_CreateEditDeleteFinalizerHandshake(t *testing.T) {
	c := startManagerClient(t)
	ctx := context.Background()
	ns := "pod-envtest"
	newTestNamespace(t, c, ns)

	r := &PodReconciler{Client: c, Recorder: events.NewFakeRecorder(20)}

	// F4: a Bound PVC's PV must actually exist -- resolvePVCClaim now reads
	// it (RBAC persistentvolumes get/list/watch, cached, D16) to detect a
	// hostPath-source PV, so a fixture with a dangling pvc.Spec.VolumeName
	// and no real PersistentVolume object no longer reflects reality.
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resourceQty("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "example.csi.k8s.io", VolumeHandle: "vol-1"},
			},
		},
	}
	require.NoError(t, c.Create(ctx, pv))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-web-0", Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resourceQty("1Gi")}},
		},
	}
	require.NoError(t, c.Create(ctx, pvc))
	pvc.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}
	fsMode := corev1.PersistentVolumeFilesystem
	pvc.Spec.VolumeName = pv.Name
	pvc.Spec.VolumeMode = &fsMode
	require.NoError(t, c.Update(ctx, pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Status().Update(ctx, pvc))

	pod := envtestPod(ns, "web-0", "", "node-a", map[string]string{"app": "postgres"},
		corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-web-0"}}})
	require.NoError(t, c.Create(ctx, pod))

	limiter := baseIOLimiter(ns, "postgres-data")
	limiter.Spec.Volumes[0].Name = "data"
	limiter.Spec.Volumes[0].Limits = storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}
	require.NoError(t, c.Create(ctx, limiter))

	waitFor(t, func() bool {
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &p); err != nil {
			return false
		}
		return true
	})

	pilName := podIOLimitName("web-0", pod.UID)
	var pil storagev1alpha1.PodIOLimit
	waitFor(t, func() bool {
		if _, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0")); err != nil {
			return false
		}
		return c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pilName}, &pil) == nil
	})
	assert.True(t, controllerutil.ContainsFinalizer(&pil, finalizerName))
	require.Len(t, pil.OwnerReferences, 1)
	assert.Equal(t, pod.Name, pil.OwnerReferences[0].Name)
	require.Len(t, pil.Spec.Volumes, 1)
	assert.Equal(t, "pv-1", pil.Spec.Volumes[0].KubeletDirName)
	require.NotNil(t, pil.Spec.Volumes[0].Limits.ReadBPS)
	assert.EqualValues(t, 10*1024*1024, *pil.Spec.Volumes[0].Limits.ReadBPS)

	// Edit the limiter: the PodIOLimit spec must be patched (never a full
	// Update, which would round-trip and could trip the identity CEL rule
	// if it ever touched those fields).
	var freshLimiter storagev1alpha1.IOLimiter
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(limiter), &freshLimiter))
	freshLimiter.Spec.Volumes[0].Limits.ReadBytesPerSecond = quantityPtr("5Mi")
	require.NoError(t, c.Update(ctx, &freshLimiter))
	assert.Greater(t, freshLimiter.Generation, int64(1))

	waitFor(t, func() bool {
		if _, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0")); err != nil {
			return false
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pilName}, &pil); err != nil {
			return false
		}
		return pil.Spec.Volumes[0].Limits.ReadBPS != nil && *pil.Spec.Volumes[0].Limits.ReadBPS == 5*1024*1024
	})

	// Delete: finalizer must be kept while status.devices is non-empty.
	pil.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=max wbps=max riops=max wiops=max", State: "Applied"}}
	require.NoError(t, c.Status().Update(ctx, &pil))
	require.NoError(t, c.Delete(ctx, limiter))

	// The manager's cache lags the Delete above by a beat, so retry
	// Reconcile until it observes the limiter gone and deletes the
	// PodIOLimit accordingly.
	waitFor(t, func() bool {
		if _, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0")); err != nil {
			return false
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pilName}, &pil); err != nil {
			return false
		}
		return pil.DeletionTimestamp != nil
	})
	assert.True(t, controllerutil.ContainsFinalizer(&pil, finalizerName), "finalizer must survive while status.devices is non-empty")

	// The agent finishes releasing: devices: [].
	pil.Status.Devices = nil
	require.NoError(t, c.Status().Update(ctx, &pil))

	waitFor(t, func() bool {
		if _, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0")); err != nil {
			return false
		}
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pilName}, &pil)
		return apierrors.IsNotFound(err)
	})
}

// TestPodReconciler_Envtest_PodDeleted_ReleasesWithoutStatus proves D9's
// dead-pod path against a real apiserver: once the pod itself is gone, the
// finalizer must be released immediately, with no dependency on
// status.devices at all.
func TestPodReconciler_Envtest_PodDeleted_ReleasesWithoutStatus(t *testing.T) {
	c := startManagerClient(t)
	ctx := context.Background()
	ns := "pod-envtest-gone"
	newTestNamespace(t, c, ns)
	r := &PodReconciler{Client: c}

	pod := envtestPod(ns, "web-0", "", "node-a", map[string]string{"app": "postgres"})
	require.NoError(t, c.Create(ctx, pod))

	pilName := podIOLimitName("web-0", pod.UID)
	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: pilName, Namespace: ns, Finalizers: []string{finalizerName}},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "web-0", PodUID: pod.UID, QOSClass: corev1.PodQOSBestEffort,
			Volumes: []storagev1alpha1.PodVolumeLimit{{Name: "x", KubeletDirName: "x", Limits: storagev1alpha1.DeviceLimits{ReadBPS: new(int64(1024 * 1024))}}},
		},
	}
	require.NoError(t, c.Create(ctx, pil))
	pil.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=max wbps=max riops=max wiops=max", State: "Applied"}}
	require.NoError(t, c.Status().Update(ctx, pil))

	// envtest has no kubelet to acknowledge a graceful pod termination, so
	// a normal Delete would only set deletionTimestamp and the object
	// would never actually disappear (SPEC.md §9's envtest caveat: no GC,
	// scheduler or kubelet). A grace-period-0 delete is the standard
	// envtest idiom to make the apiserver remove the object outright, so
	// the "pod truly gone" case (not merely Terminating) can be exercised.
	require.NoError(t, c.Delete(ctx, pod, client.GracePeriodSeconds(0)))

	waitFor(t, func() bool {
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pod.Name}, &corev1.Pod{})
		return apierrors.IsNotFound(err)
	})

	waitFor(t, func() bool {
		_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
		require.NoError(t, err)
		err = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pilName}, &storagev1alpha1.PodIOLimit{})
		return apierrors.IsNotFound(err)
	})
}

// TestPodReconciler_Envtest_OrphanPodIOLimit_NoOwnerRef_Released is D30's
// envtest regression: a PodIOLimit created directly (no ownerRef, exactly
// the reported bug's shape -- a leftover pointing at a pod UID that
// doesn't exist) against a real apiserver is still released once
// Reconcile is invoked for it, via the same key mapPodIOLimitToPod's watch
// mapping would have produced. Proves the D9 finalizer removal (optimistic
// lock, real resourceVersions) and the CEL-validated CRD schema don't stand
// in the way of releasing an object the controller never created.
func TestPodReconciler_Envtest_OrphanPodIOLimit_NoOwnerRef_Released(t *testing.T) {
	c := startManagerClient(t)
	ctx := context.Background()
	ns := "pod-envtest-orphan"
	newTestNamespace(t, c, ns)
	r := &PodReconciler{Client: c}

	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-11112222", Namespace: ns, Finalizers: []string{finalizerName}},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a", PodName: "ghost", PodUID: types.UID("11112222-0000-0000-0000-000000000000"),
			QOSClass: corev1.PodQOSBestEffort,
			Volumes:  []storagev1alpha1.PodVolumeLimit{{Name: "x", KubeletDirName: "x"}},
		},
	}
	require.NoError(t, c.Create(ctx, pil))

	waitFor(t, func() bool {
		_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "ghost"))
		require.NoError(t, err)
		err = c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pil.Name}, &storagev1alpha1.PodIOLimit{})
		return apierrors.IsNotFound(err)
	})
}

// TestPodReconciler_Envtest_HostPathPV_NoPodIOLimitCreated is F4's envtest
// proof: against a real apiserver (real RBAC-shaped cached reads, not a
// fake client), a PVC bound to a hostPath-source PV must not produce a
// PodIOLimit volume at all -- the pure desired.Compute unit tests already
// prove the issue classification, this proves the whole read path
// (PersistentVolume cache wiring, D16) actually works end to end.
func TestPodReconciler_Envtest_HostPathPV_NoPodIOLimitCreated(t *testing.T) {
	c := startManagerClient(t)
	ctx := context.Background()
	ns := "pod-envtest-hostpath-pv"
	newTestNamespace(t, c, ns)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-hostpath-1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resourceQty("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
			},
		},
	}
	require.NoError(t, c.Create(ctx, pv))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-web-0", Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resourceQty("1Gi")}},
		},
	}
	require.NoError(t, c.Create(ctx, pvc))
	fsMode := corev1.PersistentVolumeFilesystem
	pvc.Spec.VolumeName = pv.Name
	pvc.Spec.VolumeMode = &fsMode
	require.NoError(t, c.Update(ctx, pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Status().Update(ctx, pvc))

	pod := envtestPod(ns, "web-0", "", "node-a", map[string]string{"app": "postgres"},
		corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-web-0"}}})
	require.NoError(t, c.Create(ctx, pod))

	limiter := baseIOLimiter(ns, "postgres-data")
	limiter.Spec.Volumes[0].Name = "data"
	limiter.Spec.Volumes[0].Limits = storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}
	require.NoError(t, c.Create(ctx, limiter))

	r := &PodReconciler{Client: c, Recorder: events.NewFakeRecorder(20)}
	waitFor(t, func() bool {
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &p); err != nil {
			return false
		}
		var pvcLive corev1.PersistentVolumeClaim
		if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), &pvcLive); err != nil {
			return false
		}
		return pvcLive.Status.Phase == corev1.ClaimBound
	})

	_, err := r.Reconcile(ctx, reconcileRequestFor(ns, "web-0"))
	require.NoError(t, err)

	var pilList storagev1alpha1.PodIOLimitList
	require.NoError(t, c.List(ctx, &pilList, client.InNamespace(ns)))
	assert.Empty(t, pilList.Items, "a hostPath-backed PV must never produce a PodIOLimit")
}

func resourceQty(s string) resource.Quantity { return resource.MustParse(s) }
