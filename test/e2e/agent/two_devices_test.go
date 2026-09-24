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

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestAgentTwoDevices is the regression test for the legacy K1 bug
// (internal/limiter/apply.go joined every device's rule into one write,
// which the kernel rejects wholesale because a second line's "MAJ:MIN"
// token has no "="): a pod with two volumes on two different loop devices
// (disk0, disk1) must get two independent io.max lines, both correct.
//
// Kept agent-only (hand-crafted PodIOLimit, controller-manager at 0):
// this is a kernel-write mechanism regression test, not a user-facing
// IOLimiter flow -- see the note in test/e2e/agent/harness_test.go.
func TestAgentTwoDevices(t *testing.T) {
	ns := harness.NewTestNamespace(t, "two-devices")
	ctx := context.Background()

	dir0 := harness.MkTestDir(t, "disk0", "two-devices")
	dir1 := harness.MkTestDir(t, "disk1", "two-devices")
	pv0 := harness.NewLocalPV(ns+"-pv0", dir0)
	pv1 := harness.NewLocalPV(ns+"-pv1", dir1)
	require.NoError(t, harness.K8sClient.Create(ctx, pv0))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv0) })
	require.NoError(t, harness.K8sClient.Create(ctx, pv1))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv1) })

	pvc0 := harness.NewLocalPVC(ns, "data0", pv0.Name)
	pvc1 := harness.NewLocalPVC(ns, "data1", pv1.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc0))
	require.NoError(t, harness.K8sClient.Create(ctx, pvc1))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data0", PVC: pvc0.Name}, {Name: "data1", PVC: pvc1.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limits0 := harness.DeviceLimits(0, 4*1024*1024, 0, 0)
	limits1 := harness.DeviceLimits(0, 6*1024*1024, 0, 0)
	pil := harness.NewPodIOLimit(pod, []storagev1alpha1.PodVolumeLimit{
		{Name: "data0", KubeletDirName: pv0.Name, Limits: limits0},
		{Name: "data1", KubeletDirName: pv1.Name, Limits: limits1},
	})
	harness.CreatePodIOLimit(t, ctx, pil)

	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	require.Len(t, applied.Status.Devices, 2, "two volumes on two different devices must produce two owned devices, not one dropped by K1")

	byName := map[string]storagev1alpha1.VolumeStatus{}
	for _, v := range applied.Status.Volumes {
		byName[v.Name] = v
	}
	require.Equal(t, "Applied", byName["data0"].State)
	require.Equal(t, "Applied", byName["data1"].State)
	dev0 := byName["data0"].Device
	dev1 := byName["data1"].Device
	require.NotEqual(t, dev0, dev1, "the two volumes must resolve to two different whole-disk devices")

	lines := harness.NodeIOMax(t, applied.Status.CgroupPath)
	require.Len(t, lines, 2, "node io.max must have exactly two device lines, got: %v", harness.SortedKeys(lines))
	assert.Equal(t, harness.ExpectedIOMaxLine(dev0, limits0), lines[dev0])
	assert.Equal(t, harness.ExpectedIOMaxLine(dev1, limits1), lines[dev1])
}
