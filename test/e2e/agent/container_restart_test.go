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
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestAgentSurvivesContainerRestart proves D5 on a real kernel: the limit
// lives on the pod-level cgroup, which outlives a container restart, so
// stopping the container directly at the runtime (kubelet restarts it,
// restartPolicy Always) must never disturb the rule already on the node,
// and must not churn PodIOLimit.status (no re-apply needed: nothing
// changed).
//
// Deviation from the chunk description's "kill the container": sending
// SIGKILL to the container's own pid 1 from a kubectl exec in the same pid
// namespace (busybox `sleep infinity`, no trap) was tried first and does
// NOT terminate it -- verified interactively against this exact image;
// pid 1, containerID and restartCount are all unchanged a second later.
// `crictl stop` on the node against the container's own ID (read from
// status.containerStatuses[0].containerID) reliably stops it and lets
// kubelet's restartPolicy: Always recreate it, which is what this test
// actually needs (a container restart with the pod, and its cgroup,
// untouched).
//
// Kept agent-only (hand-crafted PodIOLimit, controller-manager at 0): the
// assertion is entirely about the agent's own status/kernel-state
// stability across a runtime-level event, independent of how the
// PodIOLimit got there -- see the note in test/e2e/agent/harness_test.go.
func TestAgentSurvivesContainerRestart(t *testing.T) {
	ns := harness.NewTestNamespace(t, "container-restart")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "container-restart")
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
	beforeLine := harness.NodeIOMax(t, cgroupPath)[dev]
	require.Equal(t, harness.ExpectedIOMaxLine(dev, limits), beforeLine)
	restartCountBefore := containerRestartCount(t, ctx, ns, pod.Name)

	// Stop the container at the runtime directly: kubelet restarts it
	// (restartPolicy Always), the pod (and its cgroup) is untouched.
	containerID := containerRuntimeID(t, ctx, ns, pod.Name)
	out, err := exec.Command("docker", "exec", harness.WorkerNode, "crictl", "stop", containerID).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	require.NoErrorf(t, err, "crictl stop %s: %s", containerID, out)

	harness.PollUntil(t, 2*time.Minute, "container restarted", func() (bool, string) {
		n := containerRestartCount(t, ctx, ns, pod.Name)
		return n > restartCountBefore, "restartCount=" + strconv.Itoa(int(n))
	})

	// Give the agent a moment to reconcile (it watches the object, not the
	// pod, so a container restart alone shouldn't even trigger a reconcile;
	// this just proves nothing regressed the rule).
	time.Sleep(3 * time.Second)

	after := harness.NodeIOMax(t, cgroupPath)
	assert.Equal(t, harness.ExpectedIOMaxLine(dev, limits), after[dev], "rule must survive a container restart (pod-level cgroup, D5)")

	var final storagev1alpha1.PodIOLimit
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKeyFromObject(pil), &final))
	assert.Equal(t, applied.Status.ObservedGeneration, final.Status.ObservedGeneration, "no spec change occurred, status should not have churned")
	assert.Equal(t, "Applied", harness.ReadyReasonOf(&final))
}

func containerRestartCount(t *testing.T, ctx context.Context, ns, name string) int32 {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod))
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "app" {
			return cs.RestartCount
		}
	}
	return 0
}

// containerRuntimeID returns the "app" container's containerd ID (without
// the "containerd://" URL prefix), as crictl on the node expects it.
func containerRuntimeID(t *testing.T, ctx context.Context, ns, name string) string {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod))
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "app" {
			id := cs.ContainerID
			if idx := strings.Index(id, "://"); idx >= 0 {
				id = id[idx+3:]
			}
			require.NotEmpty(t, id, "container %s/%s[app] has no containerID yet", ns, name)
			return id
		}
	}
	t.Fatalf("pod %s/%s has no container status for %q", ns, name, "app")
	return ""
}
