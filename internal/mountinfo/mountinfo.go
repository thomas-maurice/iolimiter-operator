// Package mountinfo parses /proc/<pid>/mountinfo and resolves the block
// device backing a kubelet-managed volume mount (D6, K8): kubelet mounts
// every PVC/CSI volume on the host at
// <kubeletRoot>/pods/<podUID>/volumes/<plugin>/<dirName>[/mount] before any
// container starts, so the agent needs no PID and no container tree walk.
//
// This package is pure and log-free (SPEC.md §6.3): callers get plain
// values and build their own log lines.
package mountinfo

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Entry is the subset of one mountinfo line this package needs: field 3
// (MAJ:MIN) and field 5 (mount point), per the mountinfo(5) format.
type Entry struct {
	// Device is "MAJ:MIN" as it appears in mountinfo (may be a partition;
	// see internal/blockdev.WholeDisk).
	Device string
	// MountPoint is the mount point, octal-unescaped.
	MountPoint string
}

// ErrNotFound is returned by FindKubeletVolume when no entry matches.
var ErrNotFound = fmt.Errorf("kubelet volume mount not found")

// Parse parses mountinfo content from r into Entries. Malformed or short
// lines are skipped rather than failing the whole parse: mountinfo is
// kernel-owned content.
func Parse(r io.Reader) ([]Entry, error) {
	var entries []Entry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// mountinfo(5): (1) mount ID (2) parent ID (3) MAJ:MIN (4) root
		// (5) mount point (6) options ... at least 5 fields precede the
		// optional-fields separator "-".
		if len(fields) < 5 {
			continue
		}
		entries = append(entries, Entry{
			Device:     fields[2],
			MountPoint: unescape(fields[4]),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning mountinfo: %w", err)
	}
	return entries, nil
}

// unescape reverses mountinfo's octal escaping of space, tab, newline and
// backslash (`\040` etc.) in path fields.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// FindKubeletVolume finds the mountinfo Entry backing the kubelet mount for
// (podUID, dirName) under kubeletRoot:
// <kubeletRoot>/pods/<podUID>/volumes/<plugin>/<dirName>[/mount]. The
// plugin directory name (kubernetes.io~csi, kubernetes.io~local-volume,
// ...) is matched structurally, not by name, since callers only know
// dirName. When several entries match (a volume remounted in place),
// mountinfo lists the newer mount later, so the last match wins. The full
// Entry is returned (not just the parsed device) so the caller can log the
// mount point and mount device without re-reading mountinfo (SPEC.md §6.3:
// the agent assembles its own Resolution from this plus
// internal/blockdev.WholeDisk's result).
func FindKubeletVolume(entries []Entry, kubeletRoot string, podUID types.UID, dirName string) (Entry, error) {
	base := filepath.Join(kubeletRoot, "pods", string(podUID), "volumes") + "/"

	var match Entry
	var found bool
	for _, e := range entries {
		rel, ok := strings.CutPrefix(e.MountPoint, base)
		if !ok {
			continue
		}
		parts := strings.Split(rel, "/")
		switch {
		case len(parts) == 2 && parts[1] == dirName: // <plugin>/<dirName>
			match, found = e, true
		case len(parts) == 3 && parts[1] == dirName && parts[2] == "mount": // <plugin>/<dirName>/mount (CSI)
			match, found = e, true
		}
	}
	if !found {
		return Entry{}, fmt.Errorf("%w: pod %s dir %s under %s", ErrNotFound, podUID, dirName, kubeletRoot)
	}
	return match, nil
}

// ParseDevice parses a mountinfo/sysfs "MAJ:MIN" device string into its
// major and minor numbers. Exported so callers (internal/blockdev) don't
// duplicate this parser.
func ParseDevice(s string) (maj, min int, err error) {
	majS, minS, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("malformed device %q", s)
	}
	maj, err = strconv.Atoi(majS)
	if err != nil {
		return 0, 0, fmt.Errorf("malformed device %q: %w", s, err)
	}
	min, err = strconv.Atoi(minS)
	if err != nil {
		return 0, 0, fmt.Errorf("malformed device %q: %w", s, err)
	}
	return maj, min, nil
}
