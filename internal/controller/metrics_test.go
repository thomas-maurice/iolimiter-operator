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

package controller

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

// readGauge reads back one (key, state) series's current value (0 if
// never set/already deleted) -- a test seam onto the package-level
// iolimiterPods GaugeVec, mirroring how a real Prometheus scrape would see
// it.
func readGauge(t *testing.T, key, state string) float64 {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, iolimiterPods.WithLabelValues(key, state).Write(m))
	return m.GetGauge().GetValue()
}

// TestIOLimiterReconciler_SetsPodsGauge proves F8's metric: after a
// reconcile, iolimiter_controller_pods{iolimiter,state} reflects exactly
// the counts just computed, so "0 pending" and "all pending" are
// distinguishable on a real Prometheus scrape, not only in the object's
// own status.
func TestIOLimiterReconciler_SetsPodsGauge(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	pvc := boundPVC(ns, "data-web-0", "pv-1")
	// No PodIOLimit at all yet: this pod counts as matched, pending.

	r := newIOLimiterReconciler(t, nil, pod, pvc, l)
	key := ns + "/postgres-data"
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	assert.Equal(t, float64(1), readGauge(t, key, "matched"))
	assert.Equal(t, float64(0), readGauge(t, key, "applied"))
	assert.Equal(t, float64(0), readGauge(t, key, "failed"))
	assert.Equal(t, float64(1), readGauge(t, key, "pending"))
}

// TestIOLimiterReconciler_DeletesPodsGaugeOnNotFound proves the gauge
// doesn't outlive the object it describes: once the IOLimiter itself is
// gone, every state's series for it must be removed, not left at a stale
// non-zero value forever.
func TestIOLimiterReconciler_DeletesPodsGaugeOnNotFound(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}})
	pvc := boundPVC(ns, "data-web-0", "pv-1")

	r := newIOLimiterReconciler(t, nil, pod, pvc, l)
	key := ns + "/postgres-data"
	_, err := r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)
	require.Equal(t, float64(1), readGauge(t, key, "matched"))

	require.NoError(t, r.Delete(context.Background(), l))
	_, err = r.Reconcile(context.Background(), reconcileRequestFor(ns, "postgres-data"))
	require.NoError(t, err)

	assert.Equal(t, float64(0), readGauge(t, key, "matched"), "the gauge must not outlive the deleted IOLimiter")
}
