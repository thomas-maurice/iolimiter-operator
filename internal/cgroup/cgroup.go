// Package cgroup discovers the kubepods cgroup root and computes pod-level
// cgroup v2 paths under it (D6, K9), with no PID and no tree walk: the
// agent computes every path itself and never takes one from the spec.
//
// This package is pure and log-free (SPEC.md §6.3): callers get plain
// values and build their own log lines.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Driver is the cgroup driver a node's kubelet/systemd is configured with.
// It determines how the pod cgroup path under the kubepods root is named.
type Driver string

const (
	Systemd  Driver = "systemd"
	Cgroupfs Driver = "cgroupfs"
)

// candidate is one of D6's kubepods-root search locations, relative to the
// cgroup2 mount root.
type candidate struct {
	path   string
	driver Driver
	// prefix is the systemd slice prefix ("<P>" in K9: "kubepods" normally,
	// "kubelet-kubepods" under kubelet.slice/ on kind). Unused for cgroupfs.
	prefix string
}

// candidates are tried in order; the first one that exists under root wins.
var candidates = []candidate{
	{path: "kubepods.slice", driver: Systemd, prefix: "kubepods"},
	{path: filepath.Join("kubelet.slice", "kubelet-kubepods.slice"), driver: Systemd, prefix: "kubelet-kubepods"},
	{path: "kubepods", driver: Cgroupfs},
	{path: filepath.Join("kubelet", "kubepods"), driver: Cgroupfs},
}

// Layout is a discovered kubepods cgroup root and the driver/prefix needed
// to compute pod paths under it.
type Layout struct {
	// Base is the kubepods root, absolute, under the cgroup2 mount root.
	Base string
	// Driver is how the pod path segments are named under Base.
	Driver Driver
	// Prefix is the systemd slice prefix ("<P>" in K9). Unused for cgroupfs.
	Prefix string
}

// DiscoverKubepods finds the kubepods cgroup root under the cgroup2 mount
// root. If override is non-empty (agent flag --kubepods-cgroup), it is used
// directly instead of the D6 candidate list, and its driver/prefix are
// inferred from whether its last path element ends in ".slice".
func DiscoverKubepods(root, override string) (Layout, error) {
	if override != "" {
		base := filepath.Join(root, override)
		info, err := os.Stat(base)
		if err != nil {
			return Layout{}, fmt.Errorf("kubepods override %q not found under %s: %w", override, root, err)
		}
		if !info.IsDir() {
			return Layout{}, fmt.Errorf("kubepods override %q under %s is not a directory", override, root)
		}
		driver, prefix := classify(override)
		return Layout{Base: base, Driver: driver, Prefix: prefix}, nil
	}

	var tried []string
	for _, c := range candidates {
		base := filepath.Join(root, c.path)
		tried = append(tried, base)
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return Layout{Base: base, Driver: c.driver, Prefix: c.prefix}, nil
		}
	}
	return Layout{}, fmt.Errorf("no kubepods cgroup found under %s, tried %s", root, strings.Join(tried, ", "))
}

func classify(p string) (Driver, string) {
	base := filepath.Base(p)
	if prefix, ok := strings.CutSuffix(base, ".slice"); ok {
		return Systemd, prefix
	}
	return Cgroupfs, ""
}

// PodPath computes the pod-level cgroup path for (uid, qos) under l.Base
// (K9), refusing any result that would fall outside l.Base.
func (l Layout) PodPath(uid types.UID, qos corev1.PodQOSClass) (string, error) {
	var rel string
	switch l.Driver {
	case Systemd:
		safeUID := strings.ReplaceAll(string(uid), "-", "_")
		if qos == corev1.PodQOSGuaranteed {
			rel = fmt.Sprintf("%s-pod%s.slice", l.Prefix, safeUID)
		} else {
			qosSlice := strings.ToLower(string(qos))
			rel = filepath.Join(
				fmt.Sprintf("%s-%s.slice", l.Prefix, qosSlice),
				fmt.Sprintf("%s-%s-pod%s.slice", l.Prefix, qosSlice, safeUID),
			)
		}
	case Cgroupfs:
		if qos == corev1.PodQOSGuaranteed {
			rel = "pod" + string(uid)
		} else {
			rel = filepath.Join(strings.ToLower(string(qos)), "pod"+string(uid))
		}
	default:
		return "", fmt.Errorf("unknown cgroup driver %q", l.Driver)
	}

	p := filepath.Join(l.Base, rel)
	if !within(l.Base, p) {
		return "", fmt.Errorf("computed pod cgroup path %q escapes kubepods root %q", p, l.Base)
	}
	return p, nil
}

// within reports whether p is strictly below base (D39): p == base itself
// is rejected too, not just an outright escape, since a forged UID like
// "/.." (cgroupfs: rel = "pod/..") cleans straight back down to base and
// must not be accepted as a pod's own cgroup path -- writing io.max there
// would throttle the whole kubepods root, not one pod.
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// IOControllerEnabled reports whether the io controller is enabled for the
// cgroup at path: cgroup v2 only exposes an io.max file in a cgroup once
// its parent has "io" in cgroup.subtree_control, so io.max's presence is
// proof (§6.1 step 3: missing io.max in the pod cgroup -> IOControllerDisabled).
func IOControllerEnabled(path string) (bool, error) {
	_, err := os.Stat(filepath.Join(path, "io.max"))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", filepath.Join(path, "io.max"), err)
	}
}
