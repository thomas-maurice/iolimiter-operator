package limiter

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// applyIOLimits resolves a container's cgroup and block devices for all volumes,
// then writes io.max rules. Returns nil, nil if no volumes have block devices yet.
func (l *Limiter) applyIOLimits(log *slog.Logger, containerID string, volumes map[string]volumeRule) (*appliedRule, error) {
	// Step 1: find the container's cgroup directory on the host
	cgroupPath, err := l.findContainerCgroup(containerID)
	if err != nil {
		return nil, fmt.Errorf("finding cgroup: %w", err)
	}
	log.Debug("found cgroup", "cgroupPath", cgroupPath)

	// Step 2: find a PID inside this cgroup
	pid, err := findPIDInCgroup(cgroupPath)
	if err != nil {
		return nil, fmt.Errorf("finding PID: %w", err)
	}
	log.Debug("found PID", "pid", pid)

	// Step 3: resolve block devices for each volume
	resolved := make(map[string]volumeRule)
	for name, vol := range volumes {
		majMin, err := l.findBlockDeviceForPath(log, pid, vol.volumePath)
		if err != nil {
			log.Info("no block device found for volume — skipping",
				"name", name, "volumePath", vol.volumePath, "detail", err.Error())
			continue
		}
		log.Debug("resolved block device", "name", name, "volumePath", vol.volumePath, "majMin", majMin)
		resolved[name] = volumeRule{
			limit:      vol.limit,
			volumePath: vol.volumePath,
			majMin:     majMin,
		}
	}

	if len(resolved) == 0 {
		return nil, nil // no volumes have block devices yet
	}

	// Step 4: write all rules to io.max in a single write
	ioMaxPath := filepath.Join(cgroupPath, "io.max")
	var lines []string
	for name, vol := range resolved {
		parts := strings.Split(vol.limit, ",")
		rule := vol.majMin + " " + strings.Join(parts, " ")
		lines = append(lines, rule)
		log.Info("writing io.max", "name", name, "path", ioMaxPath, "rule", rule)
	}

	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(ioMaxPath, []byte(payload), 0644); err != nil {
		return nil, fmt.Errorf("writing io.max: %w", err)
	}

	content, _ := os.ReadFile(ioMaxPath)
	log.Info("io.max verified", "content", strings.TrimSpace(string(content)))
	return &appliedRule{
		volumes:    resolved,
		cgroupPath: cgroupPath,
	}, nil
}

// resetIOLimits writes "max" values to a container's io.max to clear all limits.
// If the cgroup is already gone (container deleted), this is a no-op.
func (l *Limiter) resetIOLimits(containerID string, rule appliedRule) {
	shortID := containerID[:12]
	log := l.log.With("containerID", shortID)

	// The cgroup may already be gone (container deleted). That's fine.
	cgroupPath := rule.cgroupPath
	if cgroupPath == "" {
		var err error
		cgroupPath, err = l.findContainerCgroup(containerID)
		if err != nil {
			log.Debug("container cgroup gone, nothing to reset", "err", err)
			return
		}
	}

	ioMaxPath := filepath.Join(cgroupPath, "io.max")
	if _, err := os.Stat(ioMaxPath); err != nil {
		log.Debug("io.max file gone, nothing to reset", "cgroupPath", cgroupPath)
		return
	}

	// Collect all device majMins that need resetting.
	var majMins []string
	if len(rule.volumes) > 0 {
		seen := make(map[string]bool)
		for _, vol := range rule.volumes {
			if vol.majMin != "" && !seen[vol.majMin] {
				majMins = append(majMins, vol.majMin)
				seen[vol.majMin] = true
			}
		}
	}

	// Fallback: read io.max to find active rules.
	if len(majMins) == 0 {
		content, err := os.ReadFile(ioMaxPath)
		if err != nil {
			log.Debug("could not read io.max for reset", "err", err)
			return
		}
		for majMin := range parseIOMaxRules(string(content)) {
			majMins = append(majMins, majMin)
		}
	}

	for _, majMin := range majMins {
		resetRule := majMin + " riops=max wiops=max rbps=max wbps=max"
		log.Info("resetting io.max (annotation removed or pod gone)",
			"path", ioMaxPath, "rule", resetRule)
		if err := os.WriteFile(ioMaxPath, []byte(resetRule+"\n"), 0644); err != nil {
			log.Error("failed to reset io.max", "err", err)
		}
	}
}

// stripContainerIDPrefix removes the runtime prefix (e.g. "containerd://")
// from a Kubernetes container ID.
func stripContainerIDPrefix(fullID string) string {
	if idx := strings.LastIndex(fullID, "//"); idx != -1 {
		return fullID[idx+2:]
	}
	return fullID
}
