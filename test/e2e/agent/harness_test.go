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

// Package agent exercises the k8s-blkio-limiter agent (SPEC.md §6.1)
// directly, hand-playing the controller (§11 C3): every test builds a
// PodIOLimit itself (finalizer, pod UID, QoS, kubeletDirName = PV name)
// and removes the finalizer itself once the agent has released.
//
// The controller-manager Deployment is scaled to 0 for the whole package
// (TestMain): left running, its PodReconciler correctly treats any
// hand-crafted PodIOLimit with no matching IOLimiter as an orphan and
// deletes it out from under this suite (§6.2 step 4). test/e2e/operator
// is the package that needs the controller running (C5); TestMain here
// always restores it to 1 replica on exit, success or failure, so a
// killed run can't strand the operator suite (or a human) with the
// controller off.
package agent

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
		fmt.Fprintf(os.Stderr, "e2e/agent: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := harness.WaitDaemonSetReady(ctx, harness.AgentNamespace, harness.AgentDaemonSet); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/agent: agent DaemonSet never became ready: %v\n", err)
		return 1
	}
	if err := harness.CheckLoopMounts(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/agent: worker loop devices not ready (run `make kind-loopdev`): %v\n", err)
		return 1
	}

	scaleCtx, scaleCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer scaleCancel()
	if err := harness.ScaleControllerManager(scaleCtx, 0); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/agent: scaling controller-manager to 0 (this suite plays the controller by hand): %v\n", err)
		return 1
	}
	// Always restore the controller to 1 on the way out, whatever happens
	// in m.Run (including a t.Fatal/panic path): the operator suite (and
	// a human running `kubectl` against the cluster afterwards) needs it
	// running, and a killed agent run must never strand it at 0.
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer restoreCancel()
		if err := harness.ScaleControllerManager(restoreCtx, 1); err != nil {
			fmt.Fprintf(os.Stderr, "e2e/agent: restoring controller-manager to 1 on exit: %v\n", err)
		}
	}()

	if err := harness.SelfHealLeftoverNamespaces(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/agent: cleaning up leftover e2e namespaces: %v\n", err)
		return 1
	}

	return m.Run()
}
