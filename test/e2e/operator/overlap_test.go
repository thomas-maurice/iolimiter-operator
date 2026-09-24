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

	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestOverlappingLimitersMostRestrictive proves D2 through the real
// controller: two IOLimiters naming the same volume must merge to the
// per-field minimum, sourced from both, and the merged rule is what
// actually lands in the kernel.
func TestOverlappingLimitersMostRestrictive(t *testing.T) {
	ns := harness.NewTestNamespace(t, "overlap")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "overlap")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	// wide: wbps=8Mi, riops=200. narrow: wbps=4Mi, riops=500. Per-field
	// min must pick wbps=4Mi (narrow) and riops=200 (wide): the minimum is
	// computed independently per field, not "whichever limiter is more
	// restrictive overall".
	wide := harness.NewIOLimiter(ns, "wide", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 8*1024*1024, 200, 0),
	})
	narrow := harness.NewIOLimiter(ns, "narrow", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 4*1024*1024, 500, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, wide))
	require.NoError(t, harness.K8sClient.Create(ctx, narrow))

	harness.WaitForIOLimiterReady(t, ctx, ns, wide.Name, 2*time.Minute, "AllApplied")
	harness.WaitForIOLimiterReady(t, ctx, ns, narrow.Name, 2*time.Minute, "AllApplied")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)
	require.Len(t, pil.Spec.Volumes, 1)
	got := pil.Spec.Volumes[0]
	require.NotNil(t, got.Limits.WriteBPS)
	require.NotNil(t, got.Limits.ReadIOPS)
	require.Equalf(t, int64(4*1024*1024), *got.Limits.WriteBPS, "merged writeBPS must be the minimum of the two limiters (D2)")
	require.Equalf(t, int64(200), *got.Limits.ReadIOPS, "merged readIOPS must be the minimum of the two limiters (D2)")
	require.ElementsMatch(t, []string{"narrow", "wide"}, got.Sources, "sources must list both contributing limiters, sorted")

	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	dev := applied.Status.Devices[0].Device
	lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
	require.Equal(t, harness.ExpectedIOMaxLine(dev, got.Limits), lines[dev])
}
