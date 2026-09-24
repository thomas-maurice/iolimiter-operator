// Package blockdev resolves cgroup v2 io.max device identifiers to the
// whole-disk device the kernel accepts (K4: io.max rejects partitions with
// ENODEV) and detects whether a device is the node's own root/kubelet-root
// disk (D23).
//
// This package is pure and log-free (SPEC.md §6.3): callers get plain
// values and build their own log lines.
package blockdev

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thomas-maurice/iolimiter-operator/internal/mountinfo"
)

// WholeDisk resolves maj:min to its whole-disk device (K4). A partition's
// /sys/dev/block/<maj>:<min> directory contains a "partition" file and, once
// its symlink is resolved, sits one level under its parent disk's directory,
// whose "dev" file holds the parent's "MAJ:MIN".
//
// The symlink MUST be resolved before taking the parent directory:
// filepath.Join(link, "..") resolves ".." lexically first (against the
// symlink's own path, e.g. .../block/8:1/..), which is not the same
// directory as the parent disk once .../block/8:1 is resolved to
// .../devices/.../sda/sda1. EvalSymlinks first, Dir second, is required.
//
// The partition's own maj:min (when isPartition is true) is not returned
// separately: it is exactly the (maj, min) the caller passed in.
func WholeDisk(sysRoot string, maj, min int) (wholeMaj, wholeMin int, isPartition bool, err error) {
	devDir := filepath.Join(sysRoot, "dev", "block", fmt.Sprintf("%d:%d", maj, min))

	if _, statErr := os.Stat(filepath.Join(devDir, "partition")); statErr != nil {
		if os.IsNotExist(statErr) {
			return maj, min, false, nil
		}
		return 0, 0, false, fmt.Errorf("stat %s: %w", filepath.Join(devDir, "partition"), statErr)
	}

	resolved, err := filepath.EvalSymlinks(devDir)
	if err != nil {
		return 0, 0, false, fmt.Errorf("resolving %s: %w", devDir, err)
	}

	parentDevFile := filepath.Join(filepath.Dir(resolved), "dev")
	content, err := os.ReadFile(parentDevFile)
	if err != nil {
		return 0, 0, false, fmt.Errorf("reading %s: %w", parentDevFile, err)
	}

	pMaj, pMin, err := mountinfo.ParseDevice(strings.TrimSpace(string(content)))
	if err != nil {
		return 0, 0, false, fmt.Errorf("parsing %s: %w", parentDevFile, err)
	}
	return pMaj, pMin, true, nil
}

// IsProtectedDevice reports whether maj:min (already whole-disk resolved
// via WholeDisk) backs any of paths (D34, replacing D23's fixed root +
// kubelet-dir pair): a limit on such a disk also throttles whatever else
// lives on it (rootfs/emptyDir, the container runtime's own storage), and
// other cgroups on the same disk can queue behind it under ext4 ordered
// mode (K7). Each path is resolved from the same mountinfo entries the
// caller already parsed for volume resolution (§6.1 step 4), using
// longest-mount-point-match, then whole-disk-resolved the same way as the
// volume being checked. A path with no mount found in entries is ignored,
// not an error: not every node has, say, /var/lib/containerd.
func IsProtectedDevice(sysRoot string, entries []mountinfo.Entry, paths []string, maj, min int) bool {
	for _, p := range paths {
		pMaj, pMin, err := deviceFor(sysRoot, entries, p)
		if err != nil {
			continue
		}
		if maj == pMaj && min == pMin {
			return true
		}
	}
	return false
}

// ResolveProtectedDevices resolves each of paths (D34) to its whole-disk
// "MAJ:MIN" device, for the agent's startup log line (D26). A path with no
// mount found in entries is omitted from the result, not an error.
func ResolveProtectedDevices(sysRoot string, entries []mountinfo.Entry, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		maj, min, err := deviceFor(sysRoot, entries, p)
		if err != nil {
			continue
		}
		out[p] = fmt.Sprintf("%d:%d", maj, min)
	}
	return out
}

// deviceFor finds the mountinfo entry that owns dir (the entry with the
// longest mount point that is "/" or a prefix directory of dir — standard
// "which filesystem holds this path" resolution) and whole-disk-resolves
// its device.
func deviceFor(sysRoot string, entries []mountinfo.Entry, dir string) (maj, min int, err error) {
	var best mountinfo.Entry
	bestLen := -1
	for _, e := range entries {
		if !ownsPath(e.MountPoint, dir) {
			continue
		}
		if len(e.MountPoint) > bestLen {
			bestLen = len(e.MountPoint)
			best = e
		}
	}
	if bestLen < 0 {
		return 0, 0, fmt.Errorf("no mount found for %s", dir)
	}

	dMaj, dMin, err := mountinfo.ParseDevice(best.Device)
	if err != nil {
		return 0, 0, err
	}
	wMaj, wMin, _, err := WholeDisk(sysRoot, dMaj, dMin)
	if err != nil {
		return 0, 0, err
	}
	return wMaj, wMin, nil
}

// ownsPath reports whether mountPoint is "/" or a prefix directory of dir.
func ownsPath(mountPoint, dir string) bool {
	if mountPoint == "/" {
		return true
	}
	return dir == mountPoint || strings.HasPrefix(dir, mountPoint+"/")
}
