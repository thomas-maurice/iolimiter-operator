package limiter

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// applyIOLimit resolves a container's cgroup and block device, then writes an
// io.max rule. Returns nil, nil if the volume has no block device yet (retry later).
func (l *Limiter) applyIOLimit(log *slog.Logger, containerID, limitSpec, volumePath string) (*appliedRule, error) {
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

	// Step 3: resolve block device
	majMin, err := l.findBlockDeviceForPath(log, pid, volumePath)
	if err != nil {
		log.Info("no block device found for volume path — skipping (this is expected for overlay/tmpfs mounts, e.g. in kind without a loop device)",
			"volumePath", volumePath, "detail", err.Error())
		return nil, nil // not an error, just nothing to do yet
	}
	log.Debug("resolved block device", "majMin", majMin)

	// Step 4: write io.max
	ioMaxPath := filepath.Join(cgroupPath, "io.max")
	parts := strings.Split(limitSpec, ",")
	rule := majMin + " " + strings.Join(parts, " ")

	log.Info("writing io.max", "path", ioMaxPath, "rule", rule)
	if err := os.WriteFile(ioMaxPath, []byte(rule+"\n"), 0644); err != nil {
		return nil, fmt.Errorf("writing io.max: %w", err)
	}

	content, _ := os.ReadFile(ioMaxPath)
	log.Info("io.max verified", "content", strings.TrimSpace(string(content)))
	return &appliedRule{
		limit:      limitSpec,
		volumePath: volumePath,
		cgroupPath: cgroupPath,
		majMin:     majMin,
	}, nil
}

// resetIOLimit writes "max" values to a container's io.max to clear the limit.
// If the cgroup is already gone (container deleted), this is a no-op.
func (l *Limiter) resetIOLimit(containerID string, rule appliedRule) {
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
	if rule.majMin != "" {
		majMins = strings.Split(rule.majMin, ",")
	} else {
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
