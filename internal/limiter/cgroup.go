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
// that contains the container ID. Handles standard, Kind, and CRI-O layouts.
func (l *Limiter) findContainerCgroup(containerID string) (string, error) {
	suffixes := []string{
		"cri-containerd-" + containerID + ".scope",
		"crio-" + containerID + ".scope",
	}

	var found string
	err := filepath.WalkDir(l.CgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
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

// resetAllRules walks the cgroup tree on startup and resets any non-default
// io.max rules to max. The first reconcile (5s later) re-applies whatever
// should be active based on current pod annotations.
func (l *Limiter) resetAllRules() {
	l.log.Info("scanning cgroup tree for stale io.max rules")
	found := 0

	err := filepath.WalkDir(l.CgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()

		if name == "system.slice" || name == "user.slice" || name == "init.scope" {
			return filepath.SkipDir
		}

		if containerIDFromCgroupDir(name) == "" {
			return nil
		}

		ioMaxPath := filepath.Join(path, "io.max")
		content, err := os.ReadFile(ioMaxPath)
		if err != nil {
			return nil
		}

		rules := parseIOMaxRules(string(content))
		if len(rules) == 0 {
			return nil
		}

		for majMin := range rules {
			resetRule := majMin + " riops=max wiops=max rbps=max wbps=max"
			l.log.Info("resetting stale io.max rule", "path", ioMaxPath, "rule", resetRule)
			if err := os.WriteFile(ioMaxPath, []byte(resetRule+"\n"), 0644); err != nil {
				l.log.Error("failed to reset stale io.max", "path", ioMaxPath, "err", err)
			}
		}
		found++
		return nil
	})

	if err != nil {
		l.log.Error("error walking cgroup tree for reset", "err", err)
	}
	l.log.Info("startup reset scan complete", "containersReset", found)
}
