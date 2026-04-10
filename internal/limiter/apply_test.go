package limiter

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripContainerIDPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"containerd prefix", "containerd://abc123", "abc123"},
		{"cri-o prefix", "cri-o://abc123", "abc123"},
		{"docker prefix", "docker://abc123", "abc123"},
		{"no prefix", "abc123", "abc123"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripContainerIDPrefix(tt.input)
			if got != tt.want {
				t.Errorf("stripContainerIDPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestApplyIOLimits(t *testing.T) {
	containerID := "abc123def456789012345678901234567890123456789012345678901234abcd"

	mountinfo := `22 1 0:21 / /proc rw - proc proc rw
30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
`

	t.Run("applies single volume limit", func(t *testing.T) {
		root := t.TempDir()
		procRoot := t.TempDir()

		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644)

		pidDir := filepath.Join(procRoot, "4567")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644)

		l := New(root, procRoot, "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))

		volumes := map[string]volumeRule{
			"data": {limit: "riops=100 wiops=50", volumePath: "/data"},
		}
		result, err := l.applyIOLimits(log, containerID, volumes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if vol, ok := result.volumes["data"]; !ok {
			t.Error("expected 'data' in result volumes")
		} else {
			if vol.limit != "riops=100 wiops=50" {
				t.Errorf("limit = %q, want %q", vol.limit, "riops=100 wiops=50")
			}
			if vol.majMin != "7:0" {
				t.Errorf("majMin = %q, want %q", vol.majMin, "7:0")
			}
		}

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		expected := "7:0 riops=100 wiops=50"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})

	t.Run("applies multiple volume limits", func(t *testing.T) {
		root := t.TempDir()
		procRoot := t.TempDir()

		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644)

		multiMountinfo := `22 1 0:21 / /proc rw - proc proc rw
30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
36 30 8:0 / /logs rw - ext4 /dev/loop1 rw
`
		pidDir := filepath.Join(procRoot, "4567")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(multiMountinfo), 0644)

		l := New(root, procRoot, "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))

		volumes := map[string]volumeRule{
			"data": {limit: "riops=100 wiops=50", volumePath: "/data"},
			"logs": {limit: "wbps=1048576", volumePath: "/logs"},
		}
		result, err := l.applyIOLimits(log, containerID, volumes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("expected non-nil result")
		}
		if len(result.volumes) != 2 {
			t.Errorf("expected 2 volumes, got %d", len(result.volumes))
		}

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		got := strings.TrimSpace(string(content))
		if !strings.Contains(got, "7:0 riops=100 wiops=50") {
			t.Errorf("io.max missing data rule, got: %q", got)
		}
		if !strings.Contains(got, "8:0 wbps=1048576") {
			t.Errorf("io.max missing logs rule, got: %q", got)
		}
	})

	t.Run("returns nil when no block device", func(t *testing.T) {
		root := t.TempDir()
		procRoot := t.TempDir()

		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644)

		pseudoOnly := "22 1 0:21 / /proc rw - proc proc rw\n40 1 0:40 / / rw - overlay overlay rw\n"
		pidDir := filepath.Join(procRoot, "4567")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(pseudoOnly), 0644)

		l := New(root, procRoot, "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))

		volumes := map[string]volumeRule{
			"data": {limit: "riops=100", volumePath: "/data"},
		}
		result, err := l.applyIOLimits(log, containerID, volumes)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil result for no block device, got %+v", result)
		}
	})
}

func TestResetIOLimits(t *testing.T) {
	containerID := "abc123def456789012345678901234567890123456789012345678901234abcd"

	t.Run("resets with stored volumes", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "cgroup-dir")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100 wiops=50\n"), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: cgroupDir,
			volumes: map[string]volumeRule{
				"data": {majMin: "7:0"},
			},
		}
		l.resetIOLimits(containerID, rule)

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		expected := "7:0 riops=max wiops=max rbps=max wbps=max"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})

	t.Run("no error when cgroup is gone", func(t *testing.T) {
		root := t.TempDir()
		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: filepath.Join(root, "nonexistent"),
			volumes: map[string]volumeRule{
				"data": {majMin: "7:0"},
			},
		}
		l.resetIOLimits(containerID, rule)
	})

	t.Run("falls back to findContainerCgroup when cgroupPath empty", func(t *testing.T) {
		root := t.TempDir()
		// Create cgroup tree so findContainerCgroup can find it
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100\n"), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: "", // empty — forces fallback
			volumes: map[string]volumeRule{
				"data": {majMin: "7:0"},
			},
		}
		l.resetIOLimits(containerID, rule)

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		expected := "7:0 riops=max wiops=max rbps=max wbps=max"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})

	t.Run("fallback returns when cgroup not found", func(t *testing.T) {
		root := t.TempDir()
		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: "", // empty — forces fallback, but cgroup doesn't exist
			volumes: map[string]volumeRule{
				"data": {majMin: "7:0"},
			},
		}
		// Should not panic
		l.resetIOLimits(containerID, rule)
	})

	t.Run("skips empty majMin and deduplicates", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "cgroup-dir")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100\n"), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: cgroupDir,
			volumes: map[string]volumeRule{
				"data":  {majMin: "7:0"},
				"data2": {majMin: "7:0"}, // duplicate — should be deduped
				"empty": {majMin: ""},     // empty — should be skipped
			},
		}
		l.resetIOLimits(containerID, rule)

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		expected := "7:0 riops=max wiops=max rbps=max wbps=max"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})
}

func TestParseIOMaxRules(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
		keys    []string
	}{
		{"empty input", "", 0, nil},
		{"all-max lines", "7:0 rbps=max wbps=max riops=max wiops=max\n8:0 rbps=max wbps=max riops=max wiops=max\n", 0, nil},
		{"single active rule", "7:0 riops=100 wiops=50 rbps=max wbps=max\n", 1, []string{"7:0"}},
		{"multiple devices", "7:0 riops=100 wiops=max rbps=max wbps=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n", 2, []string{"7:0", "8:0"}},
		{"malformed line - single field", "7:0\n", 0, nil},
		{"mixed active and max", "7:0 riops=100 wiops=50 rbps=max wbps=max\n8:0 rbps=max wbps=max riops=max wiops=max\n", 1, []string{"7:0"}},
		{"whitespace only", "   \n  \n", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIOMaxRules(tt.content)
			if len(got) != tt.want {
				t.Errorf("parseIOMaxRules() returned %d rules, want %d", len(got), tt.want)
			}
			for _, key := range tt.keys {
				if _, ok := got[key]; !ok {
					t.Errorf("expected key %q in rules map", key)
				}
			}
		})
	}
}

func TestFindBlockDeviceForPath(t *testing.T) {
	mountinfo := `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
23 1 0:22 / /sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw
24 1 0:23 / /dev rw,nosuid - devtmpfs devtmpfs rw
30 1 259:1 / / rw,relatime - ext4 /dev/nvme0n1p1 rw
35 30 7:0 / /data rw,relatime - ext4 /dev/loop0 rw
40 30 0:40 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw
`
	t.Run("exact match", func(t *testing.T) {
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644)

		l := &Limiter{ProcRoot: dir}
		got, err := l.findBlockDeviceForPath("1234", "/data")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "7:0" {
			t.Errorf("got %q, want %q", got, "7:0")
		}
	})

	t.Run("prefix match - subdir of /data", func(t *testing.T) {
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644)

		l := &Limiter{ProcRoot: dir}
		got, err := l.findBlockDeviceForPath("1234", "/data/subdir")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "7:0" {
			t.Errorf("got %q, want %q", got, "7:0")
		}
	})

	t.Run("falls back to root mount", func(t *testing.T) {
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644)

		l := &Limiter{ProcRoot: dir}
		got, err := l.findBlockDeviceForPath("1234", "/var/lib/something")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "259:1" {
			t.Errorf("got %q, want %q", got, "259:1")
		}
	})

	t.Run("no block device - only pseudo-fs", func(t *testing.T) {
		pseudoOnly := `22 1 0:21 / /proc rw - proc proc rw
23 1 0:22 / /sys rw - sysfs sysfs rw
40 1 0:40 / / rw - overlay overlay rw
`
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(pseudoOnly), 0644)

		l := &Limiter{ProcRoot: dir}
		_, err := l.findBlockDeviceForPath("1234", "/data")
		if err == nil {
			t.Error("expected error for pseudo-fs only, got nil")
		}
	})

	t.Run("error when mountinfo missing", func(t *testing.T) {
		dir := t.TempDir()
		// No mountinfo file created
		l := &Limiter{ProcRoot: dir}
		_, err := l.findBlockDeviceForPath("9999", "/data")
		if err == nil {
			t.Error("expected error for missing mountinfo, got nil")
		}
	})

	t.Run("most specific mount wins", func(t *testing.T) {
		deepMount := `30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
36 35 8:0 / /data/deep rw - ext4 /dev/loop1 rw
`
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		os.MkdirAll(pidDir, 0755)
		os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(deepMount), 0644)

		l := &Limiter{ProcRoot: dir}
		got, err := l.findBlockDeviceForPath("1234", "/data/deep/file")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "8:0" {
			t.Errorf("got %q, want %q", got, "8:0")
		}
	})
}
