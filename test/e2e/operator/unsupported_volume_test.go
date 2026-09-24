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

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/controller/desired"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestUnsupportedVolumeReported proves an emptyDir volume is reported as
// unsupported all the way up to the IOLimiter, controller-side (desired.Compute
// flags it UnsupportedVolumeType and drops it before a PodIOLimit is even
// built, C4's "volume type unsupported" issue path -- distinct from the
// agent-side NoBlockDevice path, which only ever sees a volume the
// controller already resolved to a kubeletDirName): failedPods=1, the
// Ready message names the pod, and an UnsupportedVolumeType warning event
// fires on the IOLimiter.
func TestUnsupportedVolumeReported(t *testing.T) {
	ns := harness.NewTestNamespace(t, "unsupported-volume")
	ctx := context.Background()

	pod := harness.NewEmptyDirPod(ns, "app", "data", nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 4*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))

	ready := harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "Failed")
	assert.EqualValues(t, 1, ready.Status.MatchedPods)
	assert.EqualValues(t, 0, ready.Status.AppliedPods)
	assert.EqualValues(t, 1, ready.Status.FailedPods)

	found := false
	var message string
	for _, c := range ready.Status.Conditions {
		if c.Type == "Ready" {
			found = true
			message = c.Message
		}
	}
	require.True(t, found, "Ready condition must be set")
	assert.Contains(t, message, pod.Name, "the Ready message must name the failing pod")

	// The volume never resolves controller-side, so no PodIOLimit is ever
	// built for it (desired.Compute drops it from the output entirely).
	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	assert.Nilf(t, pil, "an emptyDir-only pod must never get a PodIOLimit: the volume is rejected controller-side before one is built")

	events, err := harness.ListIOLimiterEvents(ctx, ns, limiter.Name)
	require.NoError(t, err)
	var warningMsg string
	var sawWarning bool
	for _, e := range events {
		if e.Reason == desired.ReasonUnsupportedVolume {
			warningMsg = e.Message
			sawWarning = true
			break
		}
	}
	require.Truef(t, sawWarning, "expected a %s warning event on IOLimiter %s/%s", desired.ReasonUnsupportedVolume, ns, limiter.Name)
	assert.Contains(t, warningMsg, "data", "the warning event message should name the volume")
}
