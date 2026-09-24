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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

// TestParseFlags_Defaults proves the §6.1 flag defaults.
func TestParseFlags_Defaults(t *testing.T) {
	cfg, opts, err := parseFlags([]string{"--node-name=node-a"})
	require.NoError(t, err)
	assert.Equal(t, "node-a", cfg.nodeName)
	assert.Equal(t, "/host/cgroup", cfg.cgroupRoot)
	assert.Equal(t, "/host/proc", cfg.procRoot)
	assert.Equal(t, "/var/lib/kubelet", cfg.kubeletRootDir)
	assert.Equal(t, 60*time.Second, cfg.resyncPeriod)
	assert.False(t, cfg.allowRootDevice)
	assert.True(t, opts.Level.Enabled(zapcore.InfoLevel))
	assert.False(t, opts.Level.Enabled(zapcore.DebugLevel))
}

// TestParseFlags_NodeNameFromEnv proves --node-name defaults to $NODE_NAME.
func TestParseFlags_NodeNameFromEnv(t *testing.T) {
	t.Setenv("NODE_NAME", "from-env")
	cfg, _, err := parseFlags(nil)
	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.nodeName)
}

// TestParseFlags_MissingNodeName_Errors.
func TestParseFlags_MissingNodeName_Errors(t *testing.T) {
	t.Setenv("NODE_NAME", "")
	_, _, err := parseFlags(nil)
	require.Error(t, err)
}

// TestParseFlags_ZapLogLevelAccepted proves --zap-log-level is bound
// (§6.1: "plus the scaffold's metrics/probe/zap flags").
func TestParseFlags_ZapLogLevelAccepted(t *testing.T) {
	cfg, opts, err := parseFlags([]string{"--node-name=node-a", "--zap-log-level=debug"})
	require.NoError(t, err)
	assert.Equal(t, "node-a", cfg.nodeName)
	assert.True(t, opts.Level.Enabled(zapcore.DebugLevel), "--zap-log-level=debug must lower the level")
}

// TestParseFlags_AllowRootDeviceAndResyncPeriod.
func TestParseFlags_AllowRootDeviceAndResyncPeriod(t *testing.T) {
	cfg, _, err := parseFlags([]string{"--node-name=node-a", "--allow-root-device=true", "--resync-period=5s"})
	require.NoError(t, err)
	assert.True(t, cfg.allowRootDevice)
	assert.Equal(t, 5*time.Second, cfg.resyncPeriod)
}

// TestParseFlags_ProtectedPathsDefault proves D34's default set.
func TestParseFlags_ProtectedPathsDefault(t *testing.T) {
	cfg, _, err := parseFlags([]string{"--node-name=node-a"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/", "/var/lib/kubelet", "/var/lib/containerd", "/var/lib/containers"}, cfg.protectedPaths)
}

// TestParseFlags_ProtectedPathsOverride proves D34's flag is parsed as a
// comma-separated list, trimmed and with empty entries dropped.
func TestParseFlags_ProtectedPathsOverride(t *testing.T) {
	cfg, _, err := parseFlags([]string{"--node-name=node-a", "--protected-paths= /a , /b ,,/c "})
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b", "/c"}, cfg.protectedPaths)
}

// TestSplitProtectedPaths_Empty proves an empty flag value yields no
// protected paths at all, not a spurious "".
func TestSplitProtectedPaths_Empty(t *testing.T) {
	assert.Empty(t, splitProtectedPaths(""))
	assert.Empty(t, splitProtectedPaths(","))
}

// TestIoMaxPresentInSubtree proves F13 part 2's preflight primitive: a
// kubepods root with "io" enabled in cgroup.subtree_control (checked
// separately by ioControllerInSubtree) but no io.max file at all -- the
// signature of a kernel built without CONFIG_BLK_DEV_THROTTLING -- must be
// detected, distinct from a healthy kernel where io.max is present.
func TestIoMaxPresentInSubtree(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, ioMaxPresentInSubtree(dir), "no io.max file at all must report false")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "io.max"), nil, 0o644))
	assert.True(t, ioMaxPresentInSubtree(dir))
}
