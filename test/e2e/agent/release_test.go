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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestAgentReleaseResetsOnlyOwned proves D8/D9 on a real kernel: a rule
// manually written on a device the agent never owned (simulating another
// tool sharing the same pod cgroup) survives the agent's release. Only the
// device the PodIOLimit actually owned is reset.
//
// Kept agent-only (hand-crafted PodIOLimit, controller-manager at 0): this
// proves D8's ownership bookkeeping directly against a foreign rule this
// suite injects by hand, which has no IOLimiter-driven equivalent -- see
// the note in test/e2e/agent/harness_test.go.
func TestAgentReleaseResetsOnlyOwned(t *testing.T) {
	ns := harness.NewTestNamespace(t, "release-owned")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "release-owned")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limits := harness.DeviceLimits(0, 4*1024*1024, 0, 0)
	pil := harness.NewPodIOLimit(pod, []storagev1alpha1.PodVolumeLimit{
		{Name: "data", KubeletDirName: pv.Name, Limits: limits},
	})
	// C7/D27: only the controller SA may write the main resource.
	require.NoError(t, harness.ControllerClient.Create(ctx, pil))

	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	require.Len(t, applied.Status.Devices, 1)
	ownedDev := applied.Status.Devices[0].Device
	cgroupPath := applied.Status.CgroupPath

	// Write a rule on disk1's device directly, simulating a rule some other
	// tool wrote in the same pod cgroup. The agent never recorded owning
	// this device (it isn't in any PodIOLimit volume), so it must survive.
	foreignDev := harness.DiskDevice(t, "disk1")
	require.NotEqual(t, ownedDev, foreignDev, "test fixture bug: owned and foreign devices must differ")
	foreignLine := foreignDev + " rbps=max wbps=2097152 riops=max wiops=max"
	harness.WriteNodeIOMax(t, cgroupPath, foreignLine)

	before := harness.NodeIOMax(t, cgroupPath)
	require.Equal(t, foreignLine, before[foreignDev], "precondition: foreign rule must be present before release")

	require.NoError(t, harness.ControllerClient.Delete(ctx, pil))

	// Wait for the agent to release: status.devices empty, Ready=False/Released.
	harness.PollUntil(t, time.Minute, "podiolimit released", func() (bool, string) {
		var got storagev1alpha1.PodIOLimit
		if err := harness.K8sClient.Get(ctx, client.ObjectKeyFromObject(pil), &got); err != nil {
			if apierrors.IsNotFound(err) {
				return true, "gone"
			}
			return false, err.Error()
		}
		return len(got.Status.Devices) == 0 && harness.ReadyReasonOf(&got) == "Released",
			fmt.Sprintf("devices=%d reason=%s", len(got.Status.Devices), harness.ReadyReasonOf(&got))
	})

	after := harness.NodeIOMax(t, cgroupPath)
	_, ownedStillThere := after[ownedDev]
	assert.False(t, ownedStillThere, "the owned device %s must be reset (line removed) on release", ownedDev)
	assert.Equal(t, foreignLine, after[foreignDev], "the foreign device %s must be untouched by release", foreignDev)

	// Finish the D9 handshake by hand (finalizer removal), as this suite
	// plays the controller.
	harness.DeletePodIOLimit(t, ctx, ns, pil.Name)
}
