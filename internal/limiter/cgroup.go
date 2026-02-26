package limiter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// cgroupScopeRe matches cgroup scope directory names like
// "cri-containerd-<ID>.scope" or "crio-<ID>.scope".
var cgroupScopeRe = regexp.MustCompile(`^(?:cri-containerd|crio)-([0-9a-f]{64})\.scope$`)

// containerIDFromCgroupDir extracts the container ID from a cgroup scope
// directory name like "cri-containerd-<ID>.scope" or "crio-<ID>.scope".
func containerIDFromCgroupDir(name string) string {
	m := cgroupScopeRe.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// findContainerCgroup walks the host cgroup tree looking for a scope directory
// that contains the container ID. This handles all known layouts:
//   - Standard:  /sys/fs/cgroup/kubepods.slice/<qos>/<pod>/cri-containerd-<ID>.scope
//   - Kind:      /sys/fs/cgroup/kubelet.slice/kubelet-kubepods.slice/<qos>/<pod>/cri-containerd-<ID>.scope
//   - CRI-O:     same patterns but with crio-<ID>.scope
func (l *Limiter) findContainerCgroup(containerID string) (string, error) {
	suffixes := []string{
		"cri-containerd-" + containerID + ".scope",
		"crio-" + containerID + ".scope",
	}

	var found string
	err := filepath.WalkDir(l.CgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		for _, suffix := range suffixes {
			if name == suffix {
				found = path
				return filepath.SkipAll
			}
		}
		// Prune directories we know won't contain kubepods
		if name == "system.slice" || name == "user.slice" || name == "init.scope" {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", fmt.Errorf("walking cgroup tree: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("no cgroup found for container %s", containerID[:12])
	}
	return found, nil
}

// findPIDInCgroup reads the first PID from a cgroup's cgroup.procs file.
func findPIDInCgroup(cgroupPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs"))
	if err != nil {
		return "", err
	}
	lines := strings.Fields(strings.TrimSpace(string(data)))
	if len(lines) == 0 {
		return "", fmt.Errorf("no PIDs in cgroup")
	}
	return lines[0], nil
}

// recoverOrphanedRules walks the cgroup tree on startup and finds containers
// that have non-default io.max rules. It populates the applied cache with a
// sentinel value so that the first reconcile loop can detect containers whose
// pods no longer have annotations and reset their io.max.
func (l *Limiter) recoverOrphanedRules() {
	l.log.Info("scanning cgroup tree for orphaned io.max rules")
	found := 0

	err := filepath.WalkDir(l.CgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()

		// Prune irrelevant subtrees
		if name == "system.slice" || name == "user.slice" || name == "init.scope" {
			return filepath.SkipDir
		}

		containerID := containerIDFromCgroupDir(name)
		if containerID == "" {
			return nil
		}

		// Check if this cgroup has a non-default io.max
		ioMaxPath := filepath.Join(path, "io.max")
		content, err := os.ReadFile(ioMaxPath)
		if err != nil {
			return nil
		}

		rules := parseIOMaxRules(string(content))
		if len(rules) == 0 {
			return nil
		}

		// Found a cgroup with active io.max rules. Collect all device majMins.
		var majMins []string
		for majMin := range rules {
			majMins = append(majMins, majMin)
		}

		l.log.Info("recovered orphaned io.max rule",
			"containerID", containerID[:12],
			"cgroupPath", path,
			"devices", majMins,
			"rules", strings.TrimSpace(string(content)))

		// Create a synthetic volume entry per recovered device so resetIOLimits
		// can clear each one.
		recovered := make(map[string]volumeRule)
		for i, mm := range majMins {
			recovered[fmt.Sprintf("__recovered_%d__", i)] = volumeRule{
				limit:  "__recovered__",
				majMin: mm,
			}
		}
		l.applied[containerID] = appliedRule{
			volumes:    recovered,
			cgroupPath: path,
		}
		recoveredRules.Inc()
		found++
		return nil
	})

	if err != nil {
		l.log.Error("error walking cgroup tree for recovery", "err", err)
	}
	l.log.Info("cgroup recovery scan complete", "rulesFound", found)
}
