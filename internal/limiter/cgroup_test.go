package limiter

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerIDFromCgroupDir(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid containerd scope", "cri-containerd-abc123def456789012345678901234567890123456789012345678901234abcd.scope", "abc123def456789012345678901234567890123456789012345678901234abcd"},
		{"valid cri-o scope", "crio-abc123def456789012345678901234567890123456789012345678901234abcd.scope", "abc123def456789012345678901234567890123456789012345678901234abcd"},
		{"invalid name", "kubepods-burstable.slice", ""},
		{"short ID", "cri-containerd-abc123.scope", ""},
		{"empty string", "", ""},
		{"missing .scope suffix", "cri-containerd-abc123def456789012345678901234567890123456789012345678901234abcd", ""},
		{"uppercase hex", "cri-containerd-ABC123DEF456789012345678901234567890123456789012345678901234.scope", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerIDFromCgroupDir(tt.input)
			if got != tt.want {
				t.Errorf("containerIDFromCgroupDir(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFindContainerCgroup(t *testing.T) {
	containerID := "abc123def456789012345678901234567890123456789012345678901234abcd"

	t.Run("standard layout", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)

		l := &Limiter{CgroupRoot: root, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		got, err := l.findContainerCgroup(containerID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != cgroupDir {
			t.Errorf("got %q, want %q", got, cgroupDir)
		}
	})

	t.Run("kind-nested layout", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubelet.slice", "kubelet-kubepods.slice",
			"kubelet-kubepods-burstable.slice", "kubelet-kubepods-burstable-pod5678.slice",
			"cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)

		l := &Limiter{CgroupRoot: root, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		got, err := l.findContainerCgroup(containerID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != cgroupDir {
			t.Errorf("got %q, want %q", got, cgroupDir)
		}
	})

	t.Run("cri-o scope", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-besteffort.slice",
			"kubepods-besteffort-podabcd.slice", "crio-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)

		l := &Limiter{CgroupRoot: root, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		got, err := l.findContainerCgroup(containerID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != cgroupDir {
			t.Errorf("got %q, want %q", got, cgroupDir)
		}
	})

	t.Run("prunes system.slice", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "system.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)

		l := &Limiter{CgroupRoot: root, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		_, err := l.findContainerCgroup(containerID)
		if err == nil {
			t.Error("expected error for container under system.slice, got nil")
		}
	})

	t.Run("not found", func(t *testing.T) {
		root := t.TempDir()
		l := &Limiter{CgroupRoot: root, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		_, err := l.findContainerCgroup(containerID)
		if err == nil {
			t.Error("expected error, got nil")
		}
	})
}

func TestFindPIDInCgroup(t *testing.T) {
	t.Run("returns first PID", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("1234\n5678\n"), 0644)
		got, err := findPIDInCgroup(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "1234" {
			t.Errorf("got %q, want %q", got, "1234")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(""), 0644)
		_, err := findPIDInCgroup(dir)
		if err == nil {
			t.Error("expected error for empty cgroup.procs, got nil")
		}
	})

	t.Run("file not found", func(t *testing.T) {
		dir := t.TempDir()
		_, err := findPIDInCgroup(dir)
		if err == nil {
			t.Error("expected error for missing cgroup.procs, got nil")
		}
	})
}

func TestResetAllRules(t *testing.T) {
	containerID := "abc123def456789012345678901234567890123456789012345678901234abcd"

	t.Run("resets active rules", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100 wiops=50 rbps=max wbps=max\n"), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.resetAllRules()

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		expected := "7:0 riops=max wiops=max rbps=max wbps=max"
		if strings.TrimSpace(string(content)) != expected {
			t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
		}
	})

	t.Run("ignores all-max rules", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		original := "7:0 rbps=max wbps=max riops=max wiops=max\n"
		os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(original), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.resetAllRules()

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		if strings.TrimSpace(string(content)) != strings.TrimSpace(original) {
			t.Errorf("io.max was modified for all-max rules: %q", string(content))
		}
	})

	t.Run("ignores missing io.max", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		// Should not panic
		l.resetAllRules()
	})

	t.Run("resets multiple devices", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		os.MkdirAll(cgroupDir, 0755)
		os.WriteFile(filepath.Join(cgroupDir, "io.max"),
			[]byte("7:0 riops=100 wiops=50\n8:0 wbps=1048576\n"), 0644)

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.resetAllRules()

		content, _ := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
		got := strings.TrimSpace(string(content))
		// Both devices should have been reset (last write wins per device in our impl)
		if !strings.Contains(got, "riops=max") {
			t.Errorf("io.max should contain reset rules, got: %q", got)
		}
	})
}
