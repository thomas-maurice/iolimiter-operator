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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestInvalidLimitFreezesInsteadOfLoosening is D35's e2e regression test
// (chaos test S7, coordinator decision 2026-09-24): editing a live
// IOLimiter's writeBytesPerSecond to an invalid value ("100m", not a whole
// number of bytes) must not loosen the node's io.max -- the last valid
// rule stays live, and the IOLimiter reports InvalidLimit instead of
// silently dropping the throttle (the observed bug: 5Mi -> "100m" turned
// wbps into max within seconds).
func TestInvalidLimitFreezesInsteadOfLoosening(t *testing.T) {
	ns := harness.NewTestNamespace(t, "invalid-limit-freeze")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "invalid-limit-freeze")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 5*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)
	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	dev := applied.Status.Devices[0].Device
	goodLimits := harness.DeviceLimits(0, 5*1024*1024, 0, 0)
	require.Equal(t, harness.ExpectedIOMaxLine(dev, goodLimits), harness.NodeIOMax(t, applied.Status.CgroupPath)[dev])

	// Edit the limiter to an invalid value -- an admin typo (a millibyte
	// Quantity CEL can't afford to reject at admission, per the C0
	// deviation note): "100m" is not a whole number of bytes.
	invalidQ := resource.MustParse("100m")
	patch := client.MergeFrom(limiter.DeepCopy())
	limiter.Spec.Volumes[0].Limits.WriteBytesPerSecond = &invalidQ
	require.NoError(t, harness.K8sClient.Patch(ctx, limiter, patch))

	// The IOLimiter must surface InvalidLimit.
	invalid := harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "Failed")
	ready := meta.FindStatusCondition(invalid.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Contains(t, ready.Message, "InvalidLimit")

	// io.max must keep the previous 5Mi wbps -- sustained poll, not a
	// single snapshot, to rule out a race where it briefly loosens before
	// settling back.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := harness.NodeIOMax(t, applied.Status.CgroupPath)[dev]
		require.Equal(t, harness.ExpectedIOMaxLine(dev, goodLimits), got, "io.max must never loosen while the limiter is invalid")
		time.Sleep(harness.PollInterval)
	}

	// The PodIOLimit spec itself must also have stayed at 5Mi.
	stillFrozen, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, stillFrozen.Spec.Volumes[0].Limits.WriteBPS)
	require.EqualValues(t, 5*1024*1024, *stillFrozen.Spec.Volumes[0].Limits.WriteBPS)

	// Fix it: reconciliation must resume normally.
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKeyFromObject(limiter), limiter))
	patch2 := client.MergeFrom(limiter.DeepCopy())
	limiter.Spec.Volumes[0].Limits = harness.VolumeLimit("data", 0, 8*1024*1024, 0, 0).Limits
	require.NoError(t, harness.K8sClient.Patch(ctx, limiter, patch2))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	fixedLimits := harness.DeviceLimits(0, 8*1024*1024, 0, 0)
	harness.PollUntil(t, time.Minute, "io.max reflects the fixed limiter", func() (bool, string) {
		lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
		line, ok := lines[dev]
		return ok && line == harness.ExpectedIOMaxLine(dev, fixedLimits), "line=" + line
	})
}
