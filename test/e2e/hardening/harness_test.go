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

// Package hardening exercises SPEC.md C7: the ValidatingAdmissionPolicies
// (D27) and the narrowed agent cgroup mount (D28), against the real
// deployed manager and agent -- the controller-manager is left running at
// 1 replica for almost every test in this package, unlike test/e2e/agent
// (controller off throughout) or test/e2e/operator's TestDeleteWhileAgentDown
// (agent evicted): the whole point here is that the *real*, *unmodified*
// deployment keeps working under the VAPs, on top of the narrowed mount.
//
// The one deliberate exception (fix batch, 2026-09-23): a few tests
// hand-create a PodIOLimit pointing at a non-existent pod, which races
// D30's orphan-PodIOLimit GC -- the real controller can delete the object
// out from under the test before the assertion under test (a VAP verdict)
// ever runs, turning it into a flaky NotFound/409 instead. Those tests call
// withControllerOff, which scales the controller-manager to 0 for the
// duration of that single test only and restores it to 1 in t.Cleanup, so
// every other test keeps running against the unmodified deployment. TestMain
// below self-heals to 1 on entry (in case a previous killed run never
// reached its cleanup) and always restores 1 on exit as a second,
// independent backstop.
package hardening

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := harness.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/hardening: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Self-heal first: a previous killed run of this package may have
	// left the controller-manager at 0 replicas (withControllerOff below)
	// without reaching its own t.Cleanup.
	if err := harness.ScaleControllerManager(ctx, 1); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/hardening: ensuring controller-manager at 1 replica: %v\n", err)
		return 1
	}
	if err := harness.WaitDaemonSetReady(ctx, harness.AgentNamespace, harness.AgentDaemonSet); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/hardening: agent DaemonSet never became ready: %v\n", err)
		return 1
	}
	if err := harness.SelfHealLeftoverNamespaces(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/hardening: cleaning up leftover e2e namespaces: %v\n", err)
		return 1
	}

	// Always leave the controller-manager at 1 replica on the way out,
	// whatever happens in m.Run (including a t.Fatal/panic path in a test
	// that called withControllerOff): this package's whole premise is
	// running against the real, unmodified deployment, and a killed run
	// must never strand it at 0 for a human or another suite.
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer restoreCancel()
		if err := harness.ScaleControllerManager(restoreCtx, 1); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/hardening: restoring controller-manager to 1 on exit: %v\n", err)
		}
	}()

	return m.Run()
}

// withControllerOff scales the controller-manager to 0 for the duration of
// the calling test and restores it to 1 in t.Cleanup, so a hand-created
// PodIOLimit pointing at a non-existent pod can't be reaped by D30's orphan
// GC mid-test (see the package doc comment). t.Cleanup runs even if the
// test fails or calls t.Fatal, and TestMain's own self-heal/restore above
// is the independent backstop for a killed run.
func withControllerOff(t *testing.T, ctx context.Context) {
	t.Helper()
	require.NoError(t, harness.ScaleControllerManager(ctx, 0), "scaling controller-manager to 0")
	t.Cleanup(func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := harness.ScaleControllerManager(restoreCtx, 1); err != nil {
			t.Errorf("restoring controller-manager to 1 replica: %v", err)
		}
	})
}
