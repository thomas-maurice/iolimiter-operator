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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestAgentRestartKeepsRules proves D8's "no startup wipe": a rolling
// restart of the agent DaemonSet must never make the rule already written
// to the kernel disappear, sampled every 200ms across the whole restart so
// a narrow window can't be stepped over by a coarser poll.
//
// Kept agent-only (hand-crafted PodIOLimit, controller-manager at 0): this
// exercises the agent's own startup/reconcile robustness, independent of
// how the PodIOLimit got there -- see the note in
// test/e2e/agent/harness_test.go.
func TestAgentRestartKeepsRules(t *testing.T) {
	ns := harness.NewTestNamespace(t, "agent-restart")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "agent-restart")
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
	harness.CreatePodIOLimit(t, ctx, pil)

	applied := harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")
	dev := applied.Status.Devices[0].Device
	cgroupPath := applied.Status.CgroupPath
	want := harness.ExpectedIOMaxLine(dev, limits)
	require.Equal(t, want, harness.NodeIOMax(t, cgroupPath)[dev])

	stop := make(chan struct{})
	var missing atomic.Int32
	var samples atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				samples.Add(1)
				content, err := harness.NodeIOMaxRaw(cgroupPath)
				if err != nil {
					// Transient docker-exec hiccup during the restart, not
					// evidence the rule is gone: only a successfully-read
					// io.max missing the line counts.
					continue
				}
				if !harness.ContainsLine(content, want) {
					missing.Add(1)
					t.Logf("sample: rule absent from node io.max during agent restart: %q", content)
				}
			}
		}
	}()

	harness.RestartAgentDaemonSet(t)
	close(stop)
	<-done

	assert.Greaterf(t, samples.Load(), int32(3), "sampler should have run several times across the restart")
	assert.Zerof(t, missing.Load(), "rule must never be absent from the node's io.max across an agent restart (D8: no startup wipe)")

	// Final state must still be correct post-restart.
	assert.Equal(t, want, harness.NodeIOMax(t, cgroupPath)[dev])
}
