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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestLimiterUpdatePropagates proves an IOLimiter edit propagates all the
// way to the kernel: patching spec.volumes[].limits bumps the
// IOLimiter's generation, the controller patches the PodIOLimit spec (not
// a full recreate), and the agent rewrites io.max to match.
func TestLimiterUpdatePropagates(t *testing.T) {
	ns := harness.NewTestNamespace(t, "update-propagates")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "update-propagates")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 6*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)
	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	dev := applied.Status.Devices[0].Device
	firstLimits := harness.DeviceLimits(0, 6*1024*1024, 0, 0)
	require.Equal(t, harness.ExpectedIOMaxLine(dev, firstLimits), harness.NodeIOMax(t, applied.Status.CgroupPath)[dev])

	// Edit the limiter: writeBPS 6Mi -> 3Mi.
	patch := client.MergeFrom(limiter.DeepCopy())
	limiter.Spec.Volumes[0].Limits = harness.VolumeLimit("data", 0, 3*1024*1024, 0, 0).Limits
	require.NoError(t, harness.K8sClient.Patch(ctx, limiter, patch))

	secondLimits := harness.DeviceLimits(0, 3*1024*1024, 0, 0)
	harness.PollUntil(t, time.Minute, "PodIOLimit spec reflects the edited limiter", func() (bool, string) {
		got, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
		if err != nil || got == nil || len(got.Spec.Volumes) == 0 || got.Spec.Volumes[0].Limits.WriteBPS == nil {
			return false, "not yet"
		}
		return *got.Spec.Volumes[0].Limits.WriteBPS == 3*1024*1024, "writeBPS=" + strconv.FormatInt(*got.Spec.Volumes[0].Limits.WriteBPS, 10)
	})

	// It's a patch, not a recreate: the PodIOLimit's UID and creation
	// timestamp must be unchanged.
	afterEdit, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	assert.Equal(t, pil.UID, afterEdit.UID, "the same PodIOLimit object must be patched, not recreated")

	harness.PollUntil(t, time.Minute, "io.max reflects the edited limiter", func() (bool, string) {
		lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
		line, ok := lines[dev]
		return ok && line == harness.ExpectedIOMaxLine(dev, secondLimits), "line=" + line
	})
}
