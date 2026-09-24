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

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestDeleteWhileAgentDown proves D9's handshake actually blocks on the
// agent: with the agent DaemonSet pinned off the worker (simulating "the
// agent is down"), deleting the IOLimiter must leave the PodIOLimit
// Terminating -- still holding its finalizer, io.max on the node
// untouched -- until the agent comes back, resets its device and the
// controller removes the finalizer.
//
// The DaemonSet nodeSelector patch is restored in t.Cleanup (this test)
// AND unconditionally in TestMain's self-heal/exit (harness_test.go): a
// killed run must never strand the agent off the worker for whatever runs
// next.
func TestDeleteWhileAgentDown(t *testing.T) {
	ns := harness.NewTestNamespace(t, "delete-agent-down")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "delete-agent-down")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 4*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)
	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	dev := applied.Status.Devices[0].Device
	cgroupPath := applied.Status.CgroupPath
	wantLine := harness.NodeIOMax(t, cgroupPath)[dev]
	require.NotEmpty(t, wantLine)

	// Belt-and-braces: restore on this test's own cleanup, on top of
	// TestMain's unconditional self-heal/exit restore.
	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := harness.RestoreAgentDaemonSet(restoreCtx); err != nil {
			t.Logf("cleanup: restoring agent DaemonSet: %v", err)
		}
	})

	require.NoError(t, harness.ExcludeWorkerFromAgentDaemonSet(ctx), "pinning the agent DaemonSet off the worker")

	require.NoError(t, harness.K8sClient.Delete(ctx, limiter))

	// The PodIOLimit must go Terminating and *stay* there: with no agent
	// on the worker, nothing ever resets status.devices, so the finalizer
	// can never be removed.
	harness.PollUntil(t, 30*time.Second, "podiolimit reaches Terminating", func() (bool, string) {
		got, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
		if err != nil || got == nil {
			return false, "not found"
		}
		return got.DeletionTimestamp != nil, "deletionTimestamp set"
	})

	// Give it a few seconds to (not) progress, then assert it's still
	// stuck: finalizer present, devices still owned, io.max unchanged.
	time.Sleep(10 * time.Second)
	stuck, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNilf(t, stuck, "podiolimit %s/%s must still exist (Terminating), not be gone", ns, pod.Name)
	assert.NotEmpty(t, stuck.Status.Devices, "the agent is down: status.devices must still be owned")
	assert.Contains(t, stuck.Finalizers, harness.FinalizerName, "the finalizer must still be present: the agent hasn't released")
	assert.Equal(t, wantLine, harness.NodeIOMax(t, cgroupPath)[dev], "io.max must be untouched while the agent is down")

	// Restore the agent: it resets the device, the controller removes the
	// finalizer, the object goes away.
	require.NoError(t, harness.RestoreAgentDaemonSet(ctx))
	harness.WaitForPodIOLimitGone(t, ctx, ns, pod.Name, pod.UID, 2*time.Minute)

	after := harness.NodeIOMax(t, cgroupPath)
	_, stillPresent := after[dev]
	assert.False(t, stillPresent, "io.max line for %s must be reset once the agent comes back", dev)
}
