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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock lets tests advance health's notion of "now" without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newHealthWithFakeClock(resync time.Duration) (*health, *fakeClock) {
	c := &fakeClock{t: time.Unix(0, 0)}
	h := &health{now: c.now, resyncPeriod: resync}
	return h, c
}

// TestHealth_Livez_FailsAfter3PeriodsNoReconcile_WithObjects proves D24(b):
// once the cache holds >=1 PodIOLimit, silence for 3x resyncPeriod is
// unhealthy (a wedged worker never requeues on its own).
func TestHealth_Livez_FailsAfter3PeriodsNoReconcile_WithObjects(t *testing.T) {
	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	h.recordReconcile(true) // cache has objects, last reconcile "now".

	require.NoError(t, h.livezCheck(nil))

	clk.advance(29 * time.Second) // < 3x30s threshold... wait resync=10s -> threshold=30s
	assert.NoError(t, h.livezCheck(nil), "still within 3x resyncPeriod")

	clk.advance(2 * time.Second) // now 31s since last reconcile, > 30s threshold
	assert.Error(t, h.livezCheck(nil), "no reconcile completed within 3x resyncPeriod must be unhealthy")
}

// TestHealth_Livez_HealthyWithZeroObjects proves D24: with no PodIOLimit in
// the cache, reconcile silence is expected (there's nothing to reconcile)
// and must not fail liveness.
func TestHealth_Livez_HealthyWithZeroObjects(t *testing.T) {
	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	h.recordReconcile(false) // cache empty.

	clk.advance(time.Hour)
	h.recordSelfCheck(true) // self-check must still be recent for livez to pass.
	assert.NoError(t, h.livezCheck(nil))
}

// TestHealth_Livez_PermanentlyFailedVolume_StillHealthy proves D24: a
// permanently Failed volume is not unhealthy, as long as reconciles keep
// completing (the loop is making progress; the failure is a resolved,
// reported outcome, not a wedged worker).
func TestHealth_Livez_PermanentlyFailedVolume_StillHealthy(t *testing.T) {
	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	for range 5 {
		h.recordReconcile(true) // each "reconcile" completes even though the volume stays Failed.
		clk.advance(5 * time.Second)
		require.NoError(t, h.livezCheck(nil))
	}
}

// TestHealth_Livez_FailsIfSelfCheckStale proves D24(a).
func TestHealth_Livez_FailsIfSelfCheckStale(t *testing.T) {
	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	clk.advance(31 * time.Second)
	assert.Error(t, h.livezCheck(nil))
}

// TestHealth_Livez_FailsWhenSelfCheckFailing proves D24(a) is not dead:
// a self-check that keeps running but keeps *failing* (host mounts gone
// bad, say) must never look live just because it's recent.
func TestHealth_Livez_FailsWhenSelfCheckFailing(t *testing.T) {
	h, clk := newHealthWithFakeClock(10 * time.Second)
	h.recordSelfCheck(true)
	require.NoError(t, h.livezCheck(nil))

	for range 5 {
		clk.advance(1 * time.Second)
		h.recordSelfCheck(false)
		assert.Error(t, h.livezCheck(nil), "a recent but failing self-check must not be live")
	}
}

// TestHealth_Readyz_RequiresSelfCheckAndCacheSync.
func TestHealth_Readyz_RequiresSelfCheckAndCacheSync(t *testing.T) {
	h, _ := newHealthWithFakeClock(10 * time.Second)
	assert.Error(t, h.readyzCheck(nil), "neither self-check nor cache sync has happened yet")

	h.recordSelfCheck(true)
	assert.Error(t, h.readyzCheck(nil), "cache not synced yet")

	h.recordCacheSynced(true)
	assert.NoError(t, h.readyzCheck(nil))
}
