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

	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestAgentProcMountNarrowed proves the F1 fix (Fable review): the agent's
// /proc hostPath is a single File (host /proc/1/mountinfo), not a
// Directory bind-mount of the whole host /proc. Before the fix, a uid-0
// process in the agent container could reach the host's real rootfs via
// /host/proc/1/root (a magic symlink into host PID 1's own root, gated
// only by a same-uid ptrace check -- no capability, no privileged, no
// hostPID needed) and read/write anything uid 0 owns on the node
// (/etc/shadow, kubelet's client certificate, every Secret volume mounted
// on the node). A File-type hostPath has no /1 directory and no other PID
// to jump through at all, so /host/proc/1/root must not exist, and nothing
// else under /host/proc (a sibling PID, a /host/proc/sys, etc.) can exist
// either -- the mount is exactly one file.
//
// Verified via the node's own view of the agent's mount namespace
// (/proc/<pid>/root, the same "via the node" fallback cgroup_narrowed_test
// uses: the agent image is distroless, no shell to `kubectl exec` into).
func TestAgentProcMountNarrowed(t *testing.T) {
	pid := agentContainerPID(t)
	procRoot := "/proc/" + pid + "/root/host/proc"

	// The agent must still be able to read the one file it needs (D6):
	// host PID 1's own mountinfo, via the same path it always used
	// (--proc-root's default composes to <root>/1/mountinfo, unchanged by
	// this fix -- the mount target now IS that exact file).
	catOut, err := exec.Command("docker", "exec", harness.WorkerNode, //nolint:gosec // fixed binary, fixed args.
		"cat", procRoot+"/1/mountinfo").CombinedOutput()
	require.NoErrorf(t, err, "cat %s/1/mountinfo: %s (D6 discovery would be broken by the F1 fix if this fails)", procRoot, catOut)
	require.NotEmptyf(t, strings.TrimSpace(string(catOut)), "%s/1/mountinfo is empty", procRoot)

	// The security-relevant assertion: /host/proc/1/root (the magic
	// symlink that jumped into host PID 1's real rootfs pre-fix) must not
	// be traversable. A File-type hostPath means /host/proc/1 is not even
	// a directory, so "test -e" on the symlink path itself must fail.
	out, err := exec.Command("docker", "exec", harness.WorkerNode, "sh", "-c", //nolint:gosec // fixed binary, fixed args.
		"test -e "+procRoot+"/1/root && echo PRESENT || echo ABSENT").CombinedOutput()
	require.NoError(t, err, "checking %s/1/root: %s", procRoot, out)
	require.Containsf(t, string(out), "ABSENT",
		"agent's mount namespace can still reach %s/1/root (host rootfs escape via the magic /proc/<pid>/root symlink, F1 is not fixed): %s",
		procRoot, out)

	// A File-type hostPath mounted at a nested path still needs its parent
	// directories to exist in the container (kubelet creates them empty),
	// so /host/proc and /host/proc/1 are themselves ordinary directories --
	// the security property is that /host/proc/1 holds exactly the one
	// mounted file and nothing else (no "root", "cwd", "environ", "maps",
	// or any other PID's own directory a Directory-type mount would have
	// exposed).
	lsOut, err := exec.Command("docker", "exec", harness.WorkerNode, //nolint:gosec // fixed binary, fixed args.
		"ls", "-a", procRoot+"/1").CombinedOutput()
	require.NoErrorf(t, err, "ls %s/1: %s", procRoot, lsOut)
	for e := range strings.FieldsSeq(string(lsOut)) {
		if e == "." || e == ".." {
			continue
		}
		require.Equalf(t, "mountinfo", e,
			"%s/1 must contain only mountinfo, found %q too (F1 is not fixed: something beyond the one file the agent reads is reachable)",
			procRoot, e)
	}

	lsRootOut, err := exec.Command("docker", "exec", harness.WorkerNode, //nolint:gosec // fixed binary, fixed args.
		"ls", "-a", procRoot).CombinedOutput()
	require.NoErrorf(t, err, "ls %s: %s", procRoot, lsRootOut)
	for e := range strings.FieldsSeq(string(lsRootOut)) {
		if e == "." || e == ".." {
			continue
		}
		require.Equalf(t, "1", e,
			"%s must contain only \"1\" (host PID 1's mountinfo), found %q too -- not a narrowed single-file mount (F1)",
			procRoot, e)
	}
}
