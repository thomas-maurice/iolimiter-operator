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
	"fmt"
	"net/http"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// health implements SPEC.md D24's agent liveness contract: a restart is
// harmless (D8: no startup wipe, kernel rules persist), so liveness can be
// strict about actual loop progress rather than a ping.
//
// now is a seam so unit tests can drive elapsed time without a real sleep.
type health struct {
	mu sync.Mutex

	now func() time.Time

	resyncPeriod time.Duration

	selfCheckOK   bool
	lastSelfCheck time.Time
	cacheSynced   bool

	hasObjects        bool
	lastReconcileDone time.Time
}

func newHealth(resyncPeriod time.Duration) *health {
	return &health{now: time.Now, resyncPeriod: resyncPeriod}
}

// recordSelfCheck records the outcome of the periodic self-check (cgroup
// root + kubepods dir present, proc-root mountinfo readable).
func (h *health) recordSelfCheck(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.selfCheckOK = ok
	h.lastSelfCheck = h.now()
}

// recordCacheSynced records that the manager's cache finished its initial
// sync (§6.1 startup: "readyz fails until these checks pass and the cache
// has synced").
func (h *health) recordCacheSynced(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSynced = ok
}

// recordReconcile records that a reconcile completed (success or a handled
// error -- anything other than the process crashing), and whether the
// cache held at least one PodIOLimit at the time.
func (h *health) recordReconcile(hasObjects bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hasObjects = hasObjects
	h.lastReconcileDone = h.now()
}

// livezCheck implements D24: fails if (a) the self-check hasn't passed
// within 3x resyncPeriod, or (b) the cache holds >= 1 PodIOLimit and no
// reconcile has completed within 3x resyncPeriod. A permanently Failed
// volume is not unhealthy: that's a resolved/verified reconcile, tracked
// separately from progress.
func (h *health) livezCheck(_ *http.Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	threshold := 3 * h.resyncPeriod
	now := h.now()

	if h.lastSelfCheck.IsZero() || now.Sub(h.lastSelfCheck) > threshold {
		return fmt.Errorf("self-check has not passed within %s (last: %s)", threshold, sinceStr(h.lastSelfCheck, now))
	}
	if !h.selfCheckOK {
		return fmt.Errorf("latest self-check (at %s) failed", h.lastSelfCheck)
	}
	if h.hasObjects && (h.lastReconcileDone.IsZero() || now.Sub(h.lastReconcileDone) > threshold) {
		return fmt.Errorf("no reconcile has completed within %s (last: %s)", threshold, sinceStr(h.lastReconcileDone, now))
	}
	return nil
}

func sinceStr(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return now.Sub(t).String() + " ago"
}

// readyzCheck reports ready once the self-check has passed at least once.
// The manager's own cache-sync gate (mgr.AddReadyzCheck default) covers
// "the cache has synced"; this covers §6.1's startup preflight.
func (h *health) readyzCheck(_ *http.Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.selfCheckOK {
		return fmt.Errorf("self-check has not passed yet")
	}
	if !h.cacheSynced {
		return fmt.Errorf("cache has not synced yet")
	}
	return nil
}

var _ healthz.Checker = (*health)(nil).livezCheck
