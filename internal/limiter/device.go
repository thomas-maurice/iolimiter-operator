package limiter

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// findBlockDeviceForPath reads /proc/<pid>/mountinfo and finds the block
// device (major:minor) for the most specific mount matching volumePath.
//
// mountinfo fields: mountID parentID major:minor root mountPoint options ...
func (l *Limiter) findBlockDeviceForPath(log *slog.Logger, pid, volumePath string) (string, error) {
	f, err := os.Open(filepath.Join(l.ProcRoot, pid, "mountinfo"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	return findBlockDeviceFromMountinfo(log, f, volumePath)
}

// findBlockDeviceFromMountinfo parses a mountinfo reader and finds the block
// device (major:minor) for the most specific mount matching volumePath.
func findBlockDeviceFromMountinfo(log *slog.Logger, r *os.File, volumePath string) (string, error) {
	var bestMatch, bestMountPoint string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		majMin := fields[2]
		mountPoint := fields[4]

		// Skip pseudo-filesystems (major 0: proc, sysfs, cgroup, tmpfs, overlay, etc.)
		if strings.HasPrefix(majMin, "0:") {
			log.Debug("skipping non-block mount",
				"mountPoint", mountPoint, "majMin", majMin, "reason", "pseudo-filesystem (major 0)")
			continue
		}

		// Check if this mount is relevant to the requested volume path
		isRelevant := volumePath == mountPoint ||
			strings.HasPrefix(volumePath, mountPoint+"/") ||
			mountPoint == "/"

		if !isRelevant {
			log.Debug("skipping mount not matching volume path",
				"mountPoint", mountPoint, "majMin", majMin, "volumePath", volumePath)
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
// Lines like "7:0 rbps=max wbps=max riops=max wiops=max" are considered default/cleared.
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
		hasActiveLimit := false
		for _, p := range parts[1:] {
			if !strings.HasSuffix(p, "=max") {
				hasActiveLimit = true
				break
			}
		}
		if hasActiveLimit {
			rules[majMin] = line
		}
	}
	return rules
}
