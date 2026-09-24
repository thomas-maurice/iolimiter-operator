package blockdev

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thomas-maurice/k8s-blkio-limiter/internal/mountinfo"
)

// buildFakeSysfs lays out a minimal real /sys/dev/block-shaped tree with an
// actual symlink, mirroring the kernel's own layout: /sys/dev/block/<M:m> is
// a symlink into /sys/devices/.../block/<disk>[/<partition>], and a
// partition's directory is nested one level under its whole-disk's
// directory.
//
//	sysRoot/dev/block/8:0            (real dir: the whole disk, not a symlink)
//	sysRoot/dev/block/8:1            -> ../../devices/.../block/sda/sda1
//	sysRoot/devices/.../block/sda/dev            = "8:0"
//	sysRoot/devices/.../block/sda/sda1/dev       = "8:1"
//	sysRoot/devices/.../block/sda/sda1/partition = "1"
//
// It also plants a decoy sysRoot/dev/block/dev file containing a device
// that is neither 8:0 nor 8:1: filepath.Join(devDir, "..") on
// sysRoot/dev/block/8:1 lexically resolves to sysRoot/dev/block (without
// ever following the symlink), so a lexical-join implementation reads this
// decoy file instead of the real parent and returns the wrong device.
func buildFakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	sdaDir := filepath.Join(root, "devices", "pci0000:00", "ata1", "host0", "target0:0:0", "0:0:0:0", "block", "sda")
	sda1Dir := filepath.Join(sdaDir, "sda1")
	require.NoError(t, os.MkdirAll(sda1Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdaDir, "dev"), []byte("8:0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sda1Dir, "dev"), []byte("8:1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sda1Dir, "partition"), []byte("1\n"), 0o644))

	devBlock := filepath.Join(root, "dev", "block")
	require.NoError(t, os.MkdirAll(devBlock, 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join("..", "..", "devices", "pci0000:00", "ata1", "host0", "target0:0:0", "0:0:0:0", "block", "sda", "sda1"),
		filepath.Join(devBlock, "8:1"),
	))
	require.NoError(t, os.Symlink(sdaDir, filepath.Join(devBlock, "8:0")))

	// Decoy: what a lexical filepath.Join(devDir, "..") would land on.
	require.NoError(t, os.WriteFile(filepath.Join(devBlock, "dev"), []byte("99:99\n"), 0o644))

	return root
}

// addWholeDiskDevice adds a /sys/dev/block/<maj>:<min> entry for a device
// with no "partition" file (dm, loop, and whole disks in general): it must
// resolve to itself.
func addWholeDiskDevice(t *testing.T, sysRoot, name string, maj, min int) {
	t.Helper()
	dir := filepath.Join(sysRoot, "devices", "virtual", "block", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev"), []byte(deviceStr(maj, min)+"\n"), 0o644))
	require.NoError(t, os.Symlink(dir, filepath.Join(sysRoot, "dev", "block", deviceStr(maj, min))))
}

// addPartitionDevice adds a parent/child pair shaped like a real
// partitioned disk (K4): the child directory sits under the parent's and
// has a "partition" file; /sys/dev/block/<child> symlinks into it.
func addPartitionDevice(t *testing.T, sysRoot, parentName string, parentMaj, parentMin int, childName string, childMaj, childMin int) {
	t.Helper()
	parentDir := filepath.Join(sysRoot, "devices", "virtual", "block", parentName)
	childDir := filepath.Join(parentDir, childName)
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "dev"), []byte(deviceStr(parentMaj, parentMin)+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "dev"), []byte(deviceStr(childMaj, childMin)+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "partition"), []byte("1\n"), 0o644))
	require.NoError(t, os.Symlink(childDir, filepath.Join(sysRoot, "dev", "block", deviceStr(childMaj, childMin))))
}

func deviceStr(maj, min int) string {
	return strconv.Itoa(maj) + ":" + strconv.Itoa(min)
}

func TestWholeDisk_NVMePartitionResolvesToParent(t *testing.T) {
	// NVMe naming: nvme0n1p1 is a partition of nvme0n1 (major 259 in this
	// fixture), same nested-directory shape as sda/sda1 but a different
	// naming scheme, which the code must not special-case.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dev", "block"), 0o755))
	addPartitionDevice(t, root, "nvme0n1", 259, 0, "nvme0n1p1", 259, 1)

	maj, min, isPartition, err := WholeDisk(root, 259, 1)
	require.NoError(t, err)
	assert.True(t, isPartition)
	assert.Equal(t, 259, maj)
	assert.Equal(t, 0, min)
}

func TestWholeDisk_DeviceMapperHasNoPartitionFile(t *testing.T) {
	// dm devices (LVM, e2e loop-backed PVs on some CSI drivers) are never
	// partitions themselves: no "partition" file, so WholeDisk must return
	// the device unchanged rather than erroring or misresolving.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dev", "block"), 0o755))
	addWholeDiskDevice(t, root, "dm-0", 253, 0)

	maj, min, isPartition, err := WholeDisk(root, 253, 0)
	require.NoError(t, err)
	assert.False(t, isPartition)
	assert.Equal(t, 253, maj)
	assert.Equal(t, 0, min)
}

func TestWholeDisk_LoopDeviceHasNoPartitionFile(t *testing.T) {
	// loop devices back the e2e fixtures (SPEC.md §10): no "partition"
	// file, resolves to itself.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dev", "block"), 0o755))
	addWholeDiskDevice(t, root, "loop0", 7, 0)

	maj, min, isPartition, err := WholeDisk(root, 7, 0)
	require.NoError(t, err)
	assert.False(t, isPartition)
	assert.Equal(t, 7, maj)
	assert.Equal(t, 0, min)
}

func TestWholeDisk_PartitionResolvesToParentViaSymlink(t *testing.T) {
	root := buildFakeSysfs(t)

	maj, min, isPartition, err := WholeDisk(root, 8, 1)
	require.NoError(t, err)
	assert.True(t, isPartition)
	// If this ever reads 99, 99 the implementation regressed to a lexical
	// filepath.Join(devDir, "..") instead of EvalSymlinks-then-Dir (K4):
	// it would have read the decoy file next to the symlink rather than
	// sda's real "dev" file.
	assert.Equal(t, 8, maj)
	assert.Equal(t, 0, min)
}

func TestWholeDisk_WholeDiskIsNotAPartition(t *testing.T) {
	root := buildFakeSysfs(t)

	maj, min, isPartition, err := WholeDisk(root, 8, 0)
	require.NoError(t, err)
	assert.False(t, isPartition)
	assert.Equal(t, 8, maj)
	assert.Equal(t, 0, min)
}

func TestWholeDisk_UnknownDeviceIsNotAPartition(t *testing.T) {
	// No /sys/dev/block/<M:m> entry at all: os.Stat on the partition file
	// fails with ENOENT, same as a whole disk. Absence must not be an
	// error: callers see plenty of these (any device not on this node).
	root := t.TempDir()

	maj, min, isPartition, err := WholeDisk(root, 253, 0)
	require.NoError(t, err)
	assert.False(t, isPartition)
	assert.Equal(t, 253, maj)
	assert.Equal(t, 0, min)
}

func TestIsProtectedDevice_MatchesRoot(t *testing.T) {
	root := buildFakeSysfs(t)
	entries := []mountinfo.Entry{
		{Device: "8:0", MountPoint: "/"},
		{Device: "8:1", MountPoint: "/var/lib/kubelet"},
	}

	is := IsProtectedDevice(root, entries, []string{"/", "/var/lib/kubelet"}, 8, 0)
	assert.True(t, is, "8:0 backs / and must be refused per D34")
}

func TestIsProtectedDevice_MatchesKubeletRootDisk(t *testing.T) {
	root := buildFakeSysfs(t)
	// / is on 8:0 (a separate disk from 8:1's parent), kubelet root is on
	// the 8:1 partition, which whole-disk-resolves to 8:0 too via the fake
	// sysfs above — use a distinct parent to prove kubelet-root-dir is
	// checked independently of "/".
	entries := []mountinfo.Entry{
		{Device: "253:0", MountPoint: "/"},
		{Device: "8:1", MountPoint: "/var/lib/kubelet"},
	}

	is := IsProtectedDevice(root, entries, []string{"/", "/var/lib/kubelet"}, 8, 0)
	assert.True(t, is, "kubelet root dir's whole disk (8:1 -> 8:0) must also be refused per D34")
}

func TestIsProtectedDevice_UnrelatedDeviceIsFalse(t *testing.T) {
	root := buildFakeSysfs(t)
	entries := []mountinfo.Entry{
		{Device: "8:0", MountPoint: "/"},
		{Device: "8:0", MountPoint: "/var/lib/kubelet"},
	}

	is := IsProtectedDevice(root, entries, []string{"/", "/var/lib/kubelet"}, 253, 0)
	assert.False(t, is)
}

func TestIsProtectedDevice_UsesLongestPrefixMatch(t *testing.T) {
	// A volume mounted under /var/lib/kubelet/pods/... must resolve
	// against the kubelet root's own mount entry, not an unrelated "/"
	// entry that happens to also be a prefix.
	root := buildFakeSysfs(t)
	entries := []mountinfo.Entry{
		{Device: "253:0", MountPoint: "/"},
		{Device: "8:0", MountPoint: "/var/lib/kubelet"},
	}

	is := IsProtectedDevice(root, entries, []string{"/var/lib/kubelet/pods/x/volumes/y"}, 8, 0)
	assert.True(t, is)
}

// TestIsProtectedDevice_MissingPathIsIgnored proves D34: a protected path
// with no owning mount found in entries at all (e.g. malformed/incomplete
// mountinfo with no "/" entry) is skipped, not an error that would block
// every volume on the node. (A node that DOES have "/" in its mountinfo
// always resolves every path through it -- "/" owns everything with no
// more specific mount -- so "missing" only arises for entries this
// broken.)
func TestIsProtectedDevice_MissingPathIsIgnored(t *testing.T) {
	root := buildFakeSysfs(t)
	var entries []mountinfo.Entry // no "/" entry at all: nothing resolves.

	is := IsProtectedDevice(root, entries, []string{"/", "/var/lib/containerd"}, 253, 0)
	assert.False(t, is, "no path resolved, so nothing is refused")
}

// TestIsProtectedDevice_MatchesAnyOfMultiplePaths proves D34's "any of"
// semantics: a device backing a later path in the list (not just the
// first) must still be refused.
func TestIsProtectedDevice_MatchesAnyOfMultiplePaths(t *testing.T) {
	root := buildFakeSysfs(t)
	entries := []mountinfo.Entry{
		{Device: "253:0", MountPoint: "/"},
		{Device: "8:0", MountPoint: "/var/lib/containerd"},
	}

	is := IsProtectedDevice(root, entries, []string{"/var/lib/kubelet", "/var/lib/containerd"}, 8, 0)
	assert.True(t, is, "8:0 backs /var/lib/containerd, the second path in the list")
}

// TestResolveProtectedDevices_OmitsMissingPaths proves D34's startup log
// line only names paths that actually resolved (see
// TestIsProtectedDevice_MissingPathIsIgnored on why that requires no "/"
// entry at all).
func TestResolveProtectedDevices_OmitsMissingPaths(t *testing.T) {
	root := buildFakeSysfs(t)
	var entries []mountinfo.Entry

	got := ResolveProtectedDevices(root, entries, []string{"/", "/var/lib/kubelet", "/var/lib/containerd"})
	assert.Empty(t, got)
}
