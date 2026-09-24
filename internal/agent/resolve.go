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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/internal/blockdev"
	"github.com/thomas-maurice/iolimiter-operator/internal/iomax"
	"github.com/thomas-maurice/iolimiter-operator/internal/mountinfo"
)

// resolution is the per-volume result of SPEC.md §6.1 step 4, assembled
// from mountinfo.Entry + blockdev.WholeDisk's return values plus the
// agent's own skip-reason logic (C1's deviation note: this struct is a C2
// type, not a C1 one). A zero State means the volume resolved to a usable
// whole-disk device and is ready to be grouped/applied; a non-zero State
// (Pending/Unsupported) is terminal for this reconcile.
type resolution struct {
	Volume storagev1alpha1.PodVolumeLimit

	MountPoint  string // host mount point matched in mountinfo
	MountDevice string // "MAJ:MIN" as mountinfo reports it (may be a partition)
	PartitionOf string // whole-disk "MAJ:MIN", set only when MountDevice was a partition
	Device      string // whole-disk "MAJ:MIN" actually throttled, set once resolved

	// State/Reason/Message mirror storagev1alpha1.VolumeStatus. State is
	// only set here for the two outcomes resolution alone can determine
	// (Pending/VolumeNotMounted, Unsupported/NoBlockDevice,
	// Unsupported/SharedRootDevice); Applied/Failed are decided later, once
	// the kernel write for the volume's device is known.
	State   string
	Reason  string
	Message string
}

// resolveVolume implements SPEC.md §6.1 step 4 for one volume: find its
// kubelet mount in entries, resolve its whole-disk device (K4), and apply
// the K5 (major 0) and D34 (protected device) skip rules. It never touches
// the kernel and never re-reads mountinfo.
func resolveVolume(entries []mountinfo.Entry, sysRoot, kubeletRootDir string, podUID types.UID, protectedPaths []string, allowRootDevice bool, v storagev1alpha1.PodVolumeLimit) resolution {
	res := resolution{Volume: v}

	entry, err := mountinfo.FindKubeletVolume(entries, kubeletRootDir, podUID, v.KubeletDirName)
	if err != nil {
		res.State, res.Reason = statePending, reasonVolumeNotMounted
		res.Message = fmt.Sprintf("kubelet mount not found under %s: %v", kubeletRootDir, err)
		return res
	}
	res.MountPoint = entry.MountPoint
	res.MountDevice = entry.Device

	maj, min, err := mountinfo.ParseDevice(entry.Device)
	if err != nil {
		res.State, res.Reason = statePending, reasonVolumeNotMounted
		res.Message = fmt.Sprintf("malformed mount device %q: %v", entry.Device, err)
		return res
	}

	wMaj, wMin, isPartition, err := blockdev.WholeDisk(sysRoot, maj, min)
	if err != nil {
		// Transient sysfs read failure (e.g. the device node is mid-teardown):
		// treated the same as "not mounted yet" so the next resync retries it
		// rather than the agent guessing at a kernel-rejected write.
		res.State, res.Reason = statePending, reasonVolumeNotMounted
		res.Message = fmt.Sprintf("resolving whole disk for %s: %v", entry.Device, err)
		return res
	}
	if isPartition {
		res.PartitionOf = fmt.Sprintf("%d:%d", wMaj, wMin)
	}

	if wMaj == 0 {
		res.State, res.Reason = stateUnsupported, "NoBlockDevice"
		res.Message = fmt.Sprintf("major 0 device %s has no request queue", entry.Device)
		return res
	}

	device := fmt.Sprintf("%d:%d", wMaj, wMin)

	if !allowRootDevice && blockdev.IsProtectedDevice(sysRoot, entries, protectedPaths, wMaj, wMin) {
		res.Device = device
		res.State, res.Reason = stateUnsupported, "SharedRootDevice"
		res.Message = fmt.Sprintf("device %s backs a protected path (%s); opt in with --allow-root-device", device, strings.Join(protectedPaths, ","))
		return res
	}

	res.Device = device
	return res
}

// deviceGroup is every resolved volume sharing one whole-disk device (D7):
// one write() per device, the per-field minimum across its volumes.
type deviceGroup struct {
	Device  string
	Rule    iomax.Rule
	Volumes []string // pod volume names, sorted
}

// groupByDevice implements SPEC.md §6.1 step 5: group resolved (State=="")
// volumes by whole-disk device and merge their limits per field minimum.
func groupByDevice(resolved []resolution) map[string]*deviceGroup {
	groups := map[string]*deviceGroup{}
	for _, r := range resolved {
		if r.State != "" {
			continue
		}
		g, ok := groups[r.Device]
		if !ok {
			g = &deviceGroup{Device: r.Device}
			groups[r.Device] = g
		}
		g.Rule = iomax.Merge(g.Rule, deviceLimitsToRule(r.Volume.Limits))
		g.Volumes = append(g.Volumes, r.Volume.Name)
	}
	for _, g := range groups {
		slices.Sort(g.Volumes)
	}
	return groups
}

func deviceLimitsToRule(dl storagev1alpha1.DeviceLimits) iomax.Rule {
	return iomax.Rule{
		ReadBPS:   int64PtrToUint64Ptr(dl.ReadBPS),
		WriteBPS:  int64PtrToUint64Ptr(dl.WriteBPS),
		ReadIOPS:  int64PtrToUint64Ptr(dl.ReadIOPS),
		WriteIOPS: int64PtrToUint64Ptr(dl.WriteIOPS),
	}
}

func int64PtrToUint64Ptr(p *int64) *uint64 {
	if p == nil {
		return nil
	}
	v := uint64(*p)
	return &v
}

// otherVolumes returns names, excluding self, sorted (already sorted in),
// for D7's SharedDevice message ("names the other volumes").
func otherVolumes(all []string, self string) []string {
	out := make([]string, 0, len(all)-1)
	for _, name := range all {
		if name != self {
			out = append(out, name)
		}
	}
	return out
}
