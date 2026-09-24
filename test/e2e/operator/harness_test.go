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

// Package operator exercises the full controller-driven flow (SPEC.md
// §6.2, §11 C5): every test creates a real IOLimiter and lets the real
// controller-manager compile, patch and release PodIOLimits, and the real
// agent apply/reset the kernel rules -- nothing in this package hand-crafts
// a PodIOLimit.
//
// TestMain requires the controller-manager Deployment at 1 replica
// (self-healed if a previous run, e.g. test/e2e/agent, was killed before
// restoring it) and always restores it to 1 on exit. It also self-heals
// the agent DaemonSet's nodeSelector (TestDeleteWhileAgentDown patches it
// to simulate the agent being down) on entry and on exit, on top of that
// test's own t.Cleanup, so a killed run can't strand either suite.
package operator

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := harness.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/operator: %v\n", err)
		return 1
	}

	// Self-heal first: a previous killed run (this package or
	// test/e2e/agent) may have left the controller at 0 replicas or the
	// agent DaemonSet's nodeSelector patched (TestDeleteWhileAgentDown).
	selfHealCtx, selfHealCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer selfHealCancel()
	if err := harness.ScaleControllerManager(selfHealCtx, 1); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/operator: ensuring controller-manager at 1 replica: %v\n", err)
		return 1
	}
	if err := harness.RestoreAgentDaemonSet(selfHealCtx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/operator: restoring agent DaemonSet: %v\n", err)
		return 1
	}

	if err := harness.CheckLoopMounts(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/operator: worker loop devices not ready (run `make kind-loopdev`): %v\n", err)
		return 1
	}
	if err := harness.SelfHealLeftoverNamespaces(selfHealCtx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/operator: cleaning up leftover e2e namespaces: %v\n", err)
		return 1
	}

	// Always leave the cluster in its "normal" state on the way out,
	// whatever happens in m.Run: controller at 1, agent DaemonSet
	// unpatched. A killed operator run must never strand either.
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer restoreCancel()
		if err := harness.RestoreAgentDaemonSet(restoreCtx); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/operator: restoring agent DaemonSet on exit: %v\n", err)
		}
		if err := harness.ScaleControllerManager(restoreCtx, 1); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/operator: restoring controller-manager to 1 on exit: %v\n", err)
		}
	}()

	return m.Run()
}
