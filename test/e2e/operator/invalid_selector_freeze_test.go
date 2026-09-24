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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestInvalidSelectorFreezesInsteadOfLoosening is D37's e2e regression test:
// editing a live IOLimiter's podSelector past D37's bound (an oversized
// selector, 17 matchExpressions) must not silently drop the throttle it was
// already applying. Before the fix, listMatchingLimiters simply excluded
// the limiter from the set fed to desired.Compute the moment its selector
// went invalid, so the pod's PodIOLimit lost the contribution and the
// node's io.max loosened to max -- exactly the D35 bug class D37 update is
// meant to close for selectors, not just limits.
func TestInvalidSelectorFreezesInsteadOfLoosening(t *testing.T) {
	ns := harness.NewTestNamespace(t, "invalid-selector-freeze")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "invalid-selector-freeze")
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

	// Break the selector past D37's bound (>16 matchExpressions) -- a
	// hostile or fat-fingered edit, not a limits change.
	exprs := make([]metav1.LabelSelectorRequirement, 17)
	for i := range exprs {
		exprs[i] = metav1.LabelSelectorRequirement{Key: fmt.Sprintf("k%d", i), Operator: metav1.LabelSelectorOpExists}
	}
	patch := client.MergeFrom(limiter.DeepCopy())
	limiter.Spec.PodSelector = metav1.LabelSelector{MatchExpressions: exprs}
	require.NoError(t, harness.K8sClient.Patch(ctx, limiter, patch))

	// The IOLimiter itself must report InvalidSelector.
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "InvalidSelector")

	// io.max must keep the previous 5Mi wbps rule -- sustained poll, not a
	// single snapshot, to rule out a race where it briefly loosens before
	// settling back.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := harness.NodeIOMax(t, applied.Status.CgroupPath)[dev]
		require.Equal(t, harness.ExpectedIOMaxLine(dev, goodLimits), got, "io.max must never loosen while the limiter's selector is invalid")
		time.Sleep(harness.PollInterval)
	}

	// The PodIOLimit spec itself must also have stayed at 5Mi.
	stillFrozen, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, stillFrozen.Spec.Volumes[0].Limits.WriteBPS)
	require.EqualValues(t, 5*1024*1024, *stillFrozen.Spec.Volumes[0].Limits.WriteBPS)
}
