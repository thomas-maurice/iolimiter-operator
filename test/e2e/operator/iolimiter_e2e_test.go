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
	corev1 "k8s.io/api/core/v1"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// e2eWriteLimitBytes/e2eWriteLimitIOPS is the rule this file's flagship
// test applies: 5Mi/s write bandwidth + 50 write IOPS on one volume.
const (
	e2eWriteLimitBytes = 5 * 1024 * 1024
	e2eWriteLimitIOPS  = 50
)

// TestIOLimiterEndToEnd proves the whole controller-driven path (§6.2)
// end to end on a real kernel: a StatefulSet with a volumeClaimTemplate on
// blkio-local, matched by a real IOLimiter, gets a controller-compiled
// PodIOLimit that the agent applies exactly (io.max line, throttled
// throughput); deleting the limiter releases and removes it, and
// throughput returns to baseline. It also asserts the full greppable
// narrative (§11 C5 acceptance) and the IOLimiter/Pod events.
func TestIOLimiterEndToEnd(t *testing.T) {
	ns := harness.NewTestNamespace(t, "iolimiter-e2e")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "iolimiter-e2e")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	const stsName = "app"
	const volTemplate = "data"
	sts := harness.NewStatefulSet(ns, stsName, volTemplate, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, sts))

	podName := harness.StatefulSetPodName(stsName)
	pod := harness.WaitPodRunning(t, ctx, ns, podName, 3*time.Minute)

	// Baseline throughput, unthrottled: must be at least 3x the limit or
	// the environment can't prove throttling at all (§9).
	baseline := harness.DDThroughput(t, ns, podName, "/data", 32, "write")
	baselineBPS := float64(baseline.Bytes) / baseline.Elapsed.Seconds()
	t.Logf("baseline: %d bytes in %s = %.0f B/s", baseline.Bytes, baseline.Elapsed, baselineBPS)
	require.GreaterOrEqualf(t, baselineBPS, 3*float64(e2eWriteLimitBytes),
		"baseline throughput %.0f B/s is not >= 3x the %d B/s limit: can't prove throttling on this runner", baselineBPS, e2eWriteLimitBytes)

	const limiterName = "limit"
	limiter := harness.NewIOLimiter(ns, limiterName, map[string]string{"app": stsName}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit(volTemplate, 0, e2eWriteLimitBytes, 0, e2eWriteLimitIOPS),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))

	ready := harness.WaitForIOLimiterReady(t, ctx, ns, limiterName, 2*time.Minute, "AllApplied")
	assert.EqualValues(t, 1, ready.Status.MatchedPods)
	assert.EqualValues(t, 1, ready.Status.AppliedPods)
	assert.EqualValues(t, 0, ready.Status.FailedPods)

	pil, err := harness.FindPodIOLimit(ctx, ns, podName, pod.UID)
	require.NoError(t, err)
	require.NotNilf(t, pil, "controller must have compiled a PodIOLimit for %s/%s", ns, podName)
	require.Len(t, pil.Status.Devices, 1)
	dev := pil.Status.Devices[0].Device
	cgroupPath := pil.Status.CgroupPath
	wantLimits := harness.DeviceLimits(0, e2eWriteLimitBytes, 0, e2eWriteLimitIOPS)

	lines := harness.NodeIOMax(t, cgroupPath)
	assert.Equalf(t, harness.ExpectedIOMaxLine(dev, wantLimits), lines[dev], "node io.max line for %s", dev)
	assert.Contains(t, lines[dev], "rbps=max", "unset readBPS must render as max (K2)")

	// Throttled throughput: with the page cache bypassed (oflag=direct),
	// writing e2eWriteLimitBytes bytes should take at least
	// bytes/limit seconds; allow 20% slack either way per §9.
	sizeMB := 2
	sizeBytes := int64(sizeMB) * 1024 * 1024
	wantElapsed := time.Duration(float64(sizeBytes)/float64(e2eWriteLimitBytes)) * time.Second
	throttled := harness.DDThroughput(t, ns, podName, "/data", sizeMB, "write")
	t.Logf("throttled: %d bytes in %s (expected >= %s)", throttled.Bytes, throttled.Elapsed, wantElapsed)
	assert.GreaterOrEqualf(t, throttled.Elapsed, time.Duration(0.8*float64(wantElapsed)),
		"throttled write of %d bytes took %s, wanted >= 0.8x expected %s", sizeBytes, throttled.Elapsed, wantElapsed)

	// Events: IOLimiter gets PodIOLimitCreated + Ready; Pod gets IOLimitApplied.
	limiterEvents, err := harness.ListIOLimiterEvents(ctx, ns, limiterName)
	require.NoError(t, err)
	assert.True(t, hasReason(limiterEvents, "PodIOLimitCreated"), "expected a PodIOLimitCreated event on IOLimiter %s/%s", ns, limiterName)
	assert.True(t, hasReason(limiterEvents, "Ready"), "expected a Ready event on IOLimiter %s/%s", ns, limiterName)

	podEvents, err := harness.ListPodEvents(ctx, ns, podName)
	require.NoError(t, err)
	assert.True(t, hasReason(podEvents, "IOLimitApplied"), "expected an IOLimitApplied event on pod %s/%s", ns, podName)

	// Delete the limiter: the controller must release the PodIOLimit
	// (agent resets the device, finalizer removed, object gone) and the
	// node's io.max line for dev must disappear.
	require.NoError(t, harness.K8sClient.Delete(ctx, limiter))
	harness.WaitForPodIOLimitGone(t, ctx, ns, podName, pod.UID, 2*time.Minute)

	linesAfter := harness.NodeIOMax(t, cgroupPath)
	_, stillPresent := linesAfter[dev]
	assert.False(t, stillPresent, "io.max line for %s must be gone after the limiter is deleted", dev)

	after := harness.DDThroughput(t, ns, podName, "/data", sizeMB, "write")
	afterBPS := float64(after.Bytes) / after.Elapsed.Seconds()
	t.Logf("post-delete: %d bytes in %s = %.0f B/s (throttled took %s)", after.Bytes, after.Elapsed, afterBPS, throttled.Elapsed)
	assert.GreaterOrEqualf(t, afterBPS, 3*float64(e2eWriteLimitBytes),
		"post-delete throughput %.0f B/s must clear the same >= 3x-limit bar as the baseline, proving the limit is really gone", afterBPS)

	// Logging narrative (§11 C5 acceptance): the union of "grep <ns>/<pod>"
	// on the controller+agent logs and "grep <ns>/<iolimiter>" on the
	// controller log must carry the whole story. The controller's
	// PodReconciler lines (create/delete/finalizer) are keyed by "pod", not
	// "iolimiter" (SPEC.md §6.3's own vocabulary table); this test greps
	// the controller log by both keys rather than only by iolimiter, since
	// only their union actually covers every element C4's already-shipped
	// logging emits.
	controllerLogs, err := harness.ControllerPodLogs(4000)
	require.NoError(t, err)
	agentLogs, err := harness.AgentPodLogs(4000)
	require.NoError(t, err)

	podKey := ns + "/" + podName
	limiterKey := ns + "/" + limiterName
	ctrlByPod := harness.GrepLines(controllerLogs, podKey)
	ctrlByLimiter := harness.GrepLines(controllerLogs, limiterKey)
	agentByPod := harness.GrepLines(agentLogs, podKey)

	assert.Contains(t, ctrlByPod, "PodIOLimit created", "controller log (by pod) must show the PodIOLimit creation")
	assert.Contains(t, ctrlByLimiter, "IOLimiter Ready transition", "controller log (by iolimiter) must show the Ready transition")
	assert.Contains(t, ctrlByLimiter, "AllApplied", "controller log (by iolimiter) must show the AllApplied reason")
	assert.Contains(t, agentByPod, "Volume resolution", "agent log (by pod) must show volume resolution")
	assert.Contains(t, agentByPod, "Recording device intent before kernel write", "agent log (by pod) must show the D8 intent record before the write")
	assert.Contains(t, agentByPod, `"op": "apply"`, "agent log (by pod) must show the apply write")
	assert.Contains(t, agentByPod, `"op": "reset"`, "agent log (by pod) must show a reset write (release)")
	assert.Contains(t, agentByPod, "Release finished", "agent log (by pod) must show the release completing")
	assert.Contains(t, ctrlByPod, "Deleting PodIOLimit", "controller log (by pod) must show the delete once the limiter is gone")
	assert.Contains(t, ctrlByPod, "Finalizer removed", "controller log (by pod) must show the finalizer removal")
}

func hasReason(events []corev1.Event, reason string) bool {
	for _, e := range events {
		if e.Reason == reason {
			return true
		}
	}
	return false
}
