//go:build e2e

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

package operator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// setUpLimitedNamespace creates a namespace with a real, applied,
// controller-managed limited pod: a local PV/PVC, a pod, and an IOLimiter
// that matches it, waiting for the IOLimiter to report AllApplied. Shared
// by both tests in this file, which only differ in what happens to the
// controller before/during the namespace delete.
func setUpLimitedNamespace(t *testing.T, ctx context.Context, prefix string) (ns string, pod *corev1.Pod) {
	t.Helper()
	ns = harness.NewTestNamespace(t, prefix)

	dir := harness.MkTestDir(t, "disk0", prefix)
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	p := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, p))
	p = harness.WaitPodRunning(t, ctx, ns, p.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 4*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	return ns, p
}

func namespacePhase(ctx context.Context, name string) (corev1.NamespacePhase, error) {
	var got corev1.Namespace
	if err := harness.K8sClient.Get(ctx, client.ObjectKey{Name: name}, &got); err != nil {
		return "", err
	}
	return got.Status.Phase, nil
}

// TestNamespaceDeleteConvergesWithControllerUp proves the normal case: a
// namespace holding a real limited pod deletes cleanly and promptly with
// the controller running throughout -- it must never hang Terminating.
func TestNamespaceDeleteConvergesWithControllerUp(t *testing.T) {
	ctx := context.Background()
	ns, _ := setUpLimitedNamespace(t, ctx, "ns-delete-up")

	require.NoError(t, harness.K8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	harness.PollUntil(t, 2*time.Minute, "namespace fully deleted (controller up throughout)", func() (bool, string) {
		var got corev1.Namespace
		err := harness.K8sClient.Get(ctx, client.ObjectKey{Name: ns}, &got)
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase=" + string(got.Status.Phase)
	})
}

// TestNamespaceDeleteConvergesOnceControllerRestored is the D27/D30 e2e
// regression for the reported bug: with the controller-manager scaled to
// 0, deleting a namespace holding a real limited pod must not hang forever
// -- it stays Terminating only because the D9 finalizer on its PodIOLimit
// has nobody to remove it (the controller is down), never because the VAP
// blocks Kubernetes' own namespace-controller/garbage-collector deletes
// (that restriction is what this fix batch removed). Scaling the
// controller back up must let it converge without any further
// intervention.
func TestNamespaceDeleteConvergesOnceControllerRestored(t *testing.T) {
	ctx := context.Background()
	ns, pod := setUpLimitedNamespace(t, ctx, "ns-delete-down")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)

	require.NoError(t, harness.ScaleControllerManager(ctx, 0))
	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := harness.ScaleControllerManager(restoreCtx, 1); err != nil {
			t.Logf("cleanup: restoring controller-manager to 1: %v", err)
		}
	})

	require.NoError(t, harness.K8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	// It must reach Terminating (the namespace controller's own sweep,
	// and GC, both now free to run since DELETE is no longer VAP-blocked)
	// but *stay* there: with the controller down, nothing ever removes
	// the D9 finalizer on the PodIOLimit.
	harness.PollUntil(t, 30*time.Second, "namespace reaches Terminating", func() (bool, string) {
		phase, err := namespacePhase(ctx, ns)
		if err != nil {
			return false, err.Error()
		}
		return phase == corev1.NamespaceTerminating, "phase=" + string(phase)
	})

	time.Sleep(10 * time.Second)
	phase, err := namespacePhase(ctx, ns)
	require.NoError(t, err)
	assert.Equal(t, corev1.NamespaceTerminating, phase, "must still be stuck Terminating: the controller is down, nobody can remove the finalizer")

	require.NoError(t, harness.ScaleControllerManager(ctx, 1))
	harness.PollUntil(t, 2*time.Minute, "namespace converges once the controller is back", func() (bool, string) {
		var got corev1.Namespace
		err := harness.K8sClient.Get(ctx, client.ObjectKey{Name: ns}, &got)
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase=" + string(got.Status.Phase)
	})
}
