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

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestAgentPartialUpdateResetsOtherKeys proves K2 on a real kernel: writing
// a rule with only riops set, then patching the PodIOLimit to a rule with
// only wbps set, must leave riops=max on the node -- not the kernel's
// previous riops value. The legacy code's single multi-line write would
// have made this impossible to even attempt (K1); the agent's one-write
// covers the previous value being explicitly replaced with "max" per K2.
//
// This is the one C3 test §11 C5 keeps as a lean, hand-crafted-PodIOLimit,
// agent-only test (controller-manager scaled to 0 for the package): it
// exercises a single-write kernel-rule mechanism, not a user-facing
// IOLimiter flow, and TestMain's controller-off invariant is exactly what
// lets it drive PodIOLimit.spec directly.
func TestAgentPartialUpdateResetsOtherKeys(t *testing.T) {
	ns := harness.NewTestNamespace(t, "partial-update")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "partial-update")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	firstLimits := harness.DeviceLimits(0, 0, 100, 0) // riops=100 only
	pil := harness.NewPodIOLimit(pod, []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: pv.Name, Limits: firstLimits},
	})
	harness.CreatePodIOLimit(t, ctx, pil)

	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	require.Len(t, applied.Status.Devices, 1)
	dev := applied.Status.Devices[0].Device

	lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
	require.Equal(t, harness.ExpectedIOMaxLine(dev, firstLimits), lines[dev])
	assert.Contains(t, lines[dev], "riops=100")

	secondLimits := harness.DeviceLimits(0, 3*1024*1024, 0, 0) // wbps=3Mi only; riops now unset.
	patch := client.MergeFrom(pil.DeepCopy())
	pil.Spec.Volumes[0].Limits = secondLimits
	// C7/D27: patching the main resource's spec needs the controller SA.
	require.NoError(t, harness.ControllerClient.Patch(ctx, pil, patch))

	harness.PollUntil(t, time.Minute, "io.max reflects the partial update", func() (bool, string) {
		lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
		line, ok := lines[dev]
		return ok && line == harness.ExpectedIOMaxLine(dev, secondLimits), "line=" + line
	})

	lines = harness.NodeIOMax(t, applied.Status.CgroupPath)
	assert.Equal(t, harness.ExpectedIOMaxLine(dev, secondLimits), lines[dev])
	assert.Contains(t, lines[dev], "riops=max", "K2: an unset key in a new write must reset to max, not keep the kernel's previous value")
	assert.NotContains(t, lines[dev], "riops=100")
}
