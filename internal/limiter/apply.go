package limiter

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// applyIOLimits resolves a container's cgroup and block devices for all volumes,
// then writes io.max rules. Returns nil, nil if no volumes have block devices yet.
func (l *Limiter) applyIOLimits(log *slog.Logger, containerID string, volumes map[string]volumeRule) (*appliedRule, error) {
	cgroupPath, err := l.findContainerCgroup(containerID)
	if err != nil {
		return nil, fmt.Errorf("finding cgroup: %w", err)
	}

	pid, err := findPIDInCgroup(cgroupPath)
	if err != nil {
		return nil, fmt.Errorf("finding PID: %w", err)
	}

	resolved := make(map[string]volumeRule)
	for name, vol := range volumes {
		majMin, err := l.findBlockDeviceForPath(pid, vol.volumePath)
		if err != nil {
			log.Info("no block device found for volume - skipping",
				"name", name, "volumePath", vol.volumePath, "detail", err.Error())
			continue
		}
		resolved[name] = volumeRule{
			limit:      vol.limit,
			volumePath: vol.volumePath,
			majMin:     majMin,
		}
	}

	if len(resolved) == 0 {
		return nil, nil
	}

	ioMaxPath := filepath.Join(cgroupPath, "io.max")
	var lines []string
	for name, vol := range resolved {
		rule := vol.majMin + " " + vol.limit
		lines = append(lines, rule)
		log.Info("writing io.max", "name", name, "path", ioMaxPath, "rule", rule)
	}

	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(ioMaxPath, []byte(payload), 0644); err != nil {
		applyTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("writing io.max: %w", err)
	}

	applyTotal.WithLabelValues("success").Inc()
	return &appliedRule{
		volumes:    resolved,
		cgroupPath: cgroupPath,
	}, nil
}

// resetIOLimits writes "max" values to a container's io.max to clear all limits.
func (l *Limiter) resetIOLimits(containerID string, rule appliedRule) {
	shortID := containerID[:12]
	log := l.log.With("containerID", shortID)

	cgroupPath := rule.cgroupPath
	if cgroupPath == "" {
		var err error
		cgroupPath, err = l.findContainerCgroup(containerID)
		if err != nil {
			log.Debug("container cgroup gone, nothing to reset")
			return
		}
	}

	ioMaxPath := filepath.Join(cgroupPath, "io.max")
	if _, err := os.Stat(ioMaxPath); err != nil {
		log.Debug("io.max file gone, nothing to reset")
		return
	}

	seen := make(map[string]bool)
	resetTotal.Inc()
	for _, vol := range rule.volumes {
		if vol.majMin == "" || seen[vol.majMin] {
			continue
		}
		seen[vol.majMin] = true
		resetRule := vol.majMin + " riops=max wiops=max rbps=max wbps=max"
		log.Info("resetting io.max", "path", ioMaxPath, "rule", resetRule)
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

// findBlockDeviceForPath reads /proc/<pid>/mountinfo and finds the block
// device (major:minor) for the most specific mount matching volumePath.
func (l *Limiter) findBlockDeviceForPath(pid, volumePath string) (string, error) {
	f, err := os.Open(filepath.Join(l.ProcRoot, pid, "mountinfo"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	return findBlockDeviceFromMountinfo(f, volumePath)
}

// findBlockDeviceFromMountinfo parses a mountinfo reader and finds the block
// device (major:minor) for the most specific mount matching volumePath.
func findBlockDeviceFromMountinfo(r io.Reader, volumePath string) (string, error) {
	var bestMatch, bestMountPoint string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		majMin := fields[2]
		mountPoint := fields[4]

		if strings.HasPrefix(majMin, "0:") {
			continue
		}

		isRelevant := volumePath == mountPoint ||
			strings.HasPrefix(volumePath, mountPoint+"/") ||
			mountPoint == "/"

		if !isRelevant {
			continue
		}

		if len(mountPoint) > len(bestMountPoint) {
			bestMountPoint = mountPoint
			bestMatch = majMin
		}
	}

	if bestMatch == "" {
		return "", fmt.Errorf("no block device found for %s", volumePath)
	}
	return bestMatch, nil
}

// parseIOMaxRules parses io.max content and returns a map of majMin -> rule line
// for lines that have at least one non-max value (i.e. an active limit).
func parseIOMaxRules(content string) map[string]string {
	rules := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		majMin := parts[0]
		for _, p := range parts[1:] {
			if !strings.HasSuffix(p, "=max") {
				rules[majMin] = line
				break
			}
		}
	}
	return rules
}
