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
		{
			name:  "containerd prefix",
			input: "containerd://abc123",
			want:  "abc123",
		},
		{
			name:  "cri-o prefix",
			input: "cri-o://abc123",
			want:  "abc123",
		},
		{
			name:  "docker prefix",
			input: "docker://abc123",
			want:  "abc123",
		},
		{
			name:  "no prefix",
			input: "abc123",
			want:  "abc123",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
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

		// Create cgroup tree
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}

		pidDir := filepath.Join(procRoot, "4567")
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, procRoot, "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))

		volumes := map[string]volumeRule{
			"data": {limit: "riops=100,wiops=50", volumePath: "/data"},
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
			if vol.limit != "riops=100,wiops=50" {
				t.Errorf("limit = %q, want %q", vol.limit, "riops=100,wiops=50")
			}
			if vol.majMin != "7:0" {
				t.Errorf("majMin = %q, want %q", vol.majMin, "7:0")
			}
		}

		content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		if err != nil {
			t.Fatal(err)
		}
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
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}

		multiMountinfo := `22 1 0:21 / /proc rw - proc proc rw
30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
36 30 8:0 / /logs rw - ext4 /dev/loop1 rw
`
		pidDir := filepath.Join(procRoot, "4567")
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(multiMountinfo), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, procRoot, "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))

		volumes := map[string]volumeRule{
			"data": {limit: "riops=100,wiops=50", volumePath: "/data"},
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

		content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		if err != nil {
			t.Fatal(err)
		}
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
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("4567\n"), 0644); err != nil {
			t.Fatal(err)
		}

		pseudoOnly := "22 1 0:21 / /proc rw - proc proc rw\n40 1 0:40 / / rw - overlay overlay rw\n"
		pidDir := filepath.Join(procRoot, "4567")
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(pseudoOnly), 0644); err != nil {
			t.Fatal(err)
		}

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
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100 wiops=50\n"), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: cgroupDir,
			volumes: map[string]volumeRule{
				"data": {majMin: "7:0"},
			},
		}
		l.resetIOLimits(containerID, rule)

		content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		if err != nil {
			t.Fatal(err)
		}
		expected := "7:0 riops=max wiops=max rbps=max wbps=max"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})

	t.Run("reads majMin from io.max when volumes have no majMin", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "cgroup-dir")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("8:0 riops=200 wiops=max rbps=max wbps=max\n"), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		rule := appliedRule{
			cgroupPath: cgroupDir,
			volumes:    map[string]volumeRule{},
		}
		l.resetIOLimits(containerID, rule)

		content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		if err != nil {
			t.Fatal(err)
		}
		expected := "8:0 riops=max wiops=max rbps=max wbps=max"
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
		// Should not panic or error
		l.resetIOLimits(containerID, rule)
	})
}
