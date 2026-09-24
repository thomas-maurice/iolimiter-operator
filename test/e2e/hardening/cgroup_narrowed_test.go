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

package hardening

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// agentContainerPID finds the worker node's agent container's own PID via
// crictl (run on the node, not in the container: the agent image is
// distroless, so there is no shell/crictl inside it to exec into --
// SPEC.md C7's own "via the node ... instead" fallback).
func agentContainerPID(t *testing.T) string {
	t.Helper()
	cidOut, err := exec.Command("docker", "exec", harness.WorkerNode, //nolint:gosec // fixed binary, fixed args.
		"crictl", "ps", "-q", "--name", "agent", "--state", "Running").CombinedOutput()
	require.NoErrorf(t, err, "crictl ps on %s: %s", harness.WorkerNode, cidOut)
	cid := strings.TrimSpace(strings.SplitN(string(cidOut), "\n", 2)[0])
	require.NotEmptyf(t, cid, "no running agent container found on %s", harness.WorkerNode)

	pidOut, err := exec.Command("docker", "exec", harness.WorkerNode, //nolint:gosec // fixed binary, fixed args.
		"sh", "-c", "crictl inspect "+cid+" | jq -r .info.pid").CombinedOutput()
	require.NoErrorf(t, err, "crictl inspect %s on %s: %s", cid, harness.WorkerNode, pidOut)
	pid := strings.TrimSpace(string(pidOut))
	require.NotEmptyf(t, pid, "empty pid for agent container %s", cid)
	return pid
}

// TestAgentCgroupMountNarrowed proves SPEC.md D28 directly on the live
// worker node: the agent's own mount namespace (reached via
// /proc/<pid>/root, the "via the node" fallback for a distroless agent
// image with no shell to `kubectl exec` into) can see the kubepods
// subtree it needs, but not system.slice or kubelet.service -- a
// compromised agent can no longer reach node-wide cgroups
// (memory.max/cgroup.freeze/cgroup.procs there would be a node-wide DoS).
func TestAgentCgroupMountNarrowed(t *testing.T) {
	pid := agentContainerPID(t)
	root := "/proc/" + pid + "/root/host/cgroup"

	// The agent must still see its own kubepods subtree, with io in
	// subtree_control (required for D6 discovery and D5's pod-level
	// writes to keep working under the narrowed mount).
	lsOut, err := exec.Command("docker", "exec", harness.WorkerNode, "sh", "-c", //nolint:gosec // fixed binary, fixed args.
		"find "+root+" -maxdepth 3 -name cgroup.subtree_control").CombinedOutput()
	require.NoErrorf(t, err, "find cgroup.subtree_control under %s: %s", root, lsOut)
	require.NotEmptyf(t, strings.TrimSpace(string(lsOut)), "no cgroup.subtree_control found under %s (kubepods root not mounted?)", root)

	subtreeControlPath := strings.TrimSpace(strings.SplitN(string(lsOut), "\n", 2)[0])
	catOut, err := exec.Command("docker", "exec", harness.WorkerNode, "cat", subtreeControlPath).CombinedOutput() //nolint:gosec // fixed binary, fixed args.
	require.NoErrorf(t, err, "cat %s: %s", subtreeControlPath, catOut)
	require.Containsf(t, string(catOut), "io", "io controller must be enabled in %s, got: %s", subtreeControlPath, catOut)

	// The agent must NOT be able to see node-wide cgroups anywhere under
	// its whole mounted view: neither system.slice (root-level sibling of
	// kubelet.slice) nor kubelet.service (kubelet.slice's own sibling of
	// kubelet-kubepods.slice, on kind's real host layout) exist, because
	// only the kubepods subtree was bind-mounted in -- the container
	// runtime's own mountpoint stub directories (e.g. an empty
	// "kubelet.slice") hold nothing else.
	out, err := exec.Command("docker", "exec", harness.WorkerNode, "sh", "-c", //nolint:gosec // fixed binary, fixed args.
		"find "+root+" \\( -name system.slice -o -name kubelet.service \\); true").CombinedOutput()
	require.NoError(t, err)
	require.Emptyf(t, strings.TrimSpace(string(out)),
		"agent's mount namespace can see a node-wide cgroup, D28's narrowing is not effective: %s", out)
}
