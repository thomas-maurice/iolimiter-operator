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

// TestPodRecreatedGetsNewPodIOLimit proves the stale-UID cleanup path
// (§6.2 PodReconciler step 2, D18): deleting a StatefulSet pod (its
// controller recreates it, same name, new UID) must release the old
// PodIOLimit (D9's dead path: no live cgroup to hand off to) and get a
// brand new one for the new UID, not reuse or patch the old object.
func TestPodRecreatedGetsNewPodIOLimit(t *testing.T) {
	ns := harness.NewTestNamespace(t, "pod-recreated")
	ctx := context.Background()

	dir := harness.MkTestDir(t, "disk0", "pod-recreated")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	const stsName = "app"
	const volTemplate = "data"
	sts := harness.NewStatefulSet(ns, stsName, volTemplate, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, sts))

	podName := harness.StatefulSetPodName(stsName)
	pod := harness.WaitPodRunning(t, ctx, ns, podName, 3*time.Minute)
	oldUID := pod.UID

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": stsName}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit(volTemplate, 0, 4*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	oldPIL, err := harness.FindPodIOLimit(ctx, ns, podName, oldUID)
	require.NoError(t, err)
	require.NotNil(t, oldPIL)
	oldPILName := oldPIL.Name

	require.NoError(t, harness.K8sClient.Delete(ctx, pod))

	// Don't wait for a NotFound gap: a StatefulSet controller can recreate
	// ordinal 0 fast enough that this suite's poll interval never actually
	// observes a 404 in between (it just sees the old UID, then the new
	// one). What the test needs is a *different* UID under the same name,
	// running -- not a literal deletion window.
	recreated := harness.WaitPodRecreated(t, ctx, ns, podName, oldUID, 3*time.Minute)

	// The old PodIOLimit must be released and gone (D9's dead path: no
	// live cgroup for the gone UID).
	harness.WaitForPodIOLimitGone(t, ctx, ns, podName, oldUID, 2*time.Minute)

	// A brand new PodIOLimit, for the new UID, must appear and apply.
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")
	newPIL, err := harness.FindPodIOLimit(ctx, ns, podName, recreated.UID)
	require.NoError(t, err)
	require.NotNil(t, newPIL)
	assert.NotEqual(t, oldPILName, newPIL.Name, "the recreated pod must get a differently-named PodIOLimit (D18 name includes the UID)")

	applied := harness.WaitForPILReady(t, ctx, ns, newPIL.Name, 2*time.Minute, "Applied")
	require.Len(t, applied.Status.Devices, 1)
}
