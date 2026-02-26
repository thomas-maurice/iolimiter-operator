package limiter

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestContainerIDFromCgroupDir(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "valid containerd scope",
			input: "cri-containerd-abc123def456789012345678901234567890123456789012345678901234abcd.scope",
			want:  "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:  "valid cri-o scope",
			input: "crio-abc123def456789012345678901234567890123456789012345678901234abcd.scope",
			want:  "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:  "invalid name - no match",
			input: "kubepods-burstable.slice",
			want:  "",
		},
		{
			name:  "short ID - no match",
			input: "cri-containerd-abc123.scope",
			want:  "",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "missing .scope suffix",
			input: "cri-containerd-abc123def456789012345678901234567890123456789012345678901234abcd",
			want:  "",
		},
		{
			name:  "uppercase hex - no match",
			input: "cri-containerd-ABC123DEF456789012345678901234567890123456789012345678901234.scope",
			want:  "",
		},
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
		// Create standard cgroup tree
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}

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
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}

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
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}

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
		// Put container under system.slice — should NOT be found
		cgroupDir := filepath.Join(root, "system.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}

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
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("1234\n5678\n"), 0644); err != nil {
			t.Fatal(err)
		}
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
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(""), 0644); err != nil {
			t.Fatal(err)
		}
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

func TestRecoverOrphanedRules(t *testing.T) {
	containerID := "abc123def456789012345678901234567890123456789012345678901234abcd"

	t.Run("recovers active rules", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 riops=100 wiops=50 rbps=max wbps=max\n"), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.recoverOrphanedRules()

		rule, ok := l.applied[containerID]
		if !ok {
			t.Fatal("expected container to be in applied map")
		}
		if rule.cgroupPath != cgroupDir {
			t.Errorf("expected cgroupPath %q, got %q", cgroupDir, rule.cgroupPath)
		}
		// Should have a synthetic volume entry for the recovered device.
		if len(rule.volumes) != 1 {
			t.Fatalf("expected 1 recovered volume, got %d", len(rule.volumes))
		}
		for _, vol := range rule.volumes {
			if vol.majMin != "7:0" {
				t.Errorf("expected majMin %q, got %q", "7:0", vol.majMin)
			}
			if vol.limit != "__recovered__" {
				t.Errorf("expected sentinel limit, got %q", vol.limit)
			}
		}
	})

	t.Run("ignores all-max rules", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("7:0 rbps=max wbps=max riops=max wiops=max\n"), 0644); err != nil {
			t.Fatal(err)
		}

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.recoverOrphanedRules()

		if _, ok := l.applied[containerID]; ok {
			t.Error("expected container NOT to be in applied map for all-max rules")
		}
	})

	t.Run("ignores missing io.max", func(t *testing.T) {
		root := t.TempDir()
		cgroupDir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
			"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
		if err := os.MkdirAll(cgroupDir, 0755); err != nil {
			t.Fatal(err)
		}

		l := New(root, "", "test-node", nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		l.recoverOrphanedRules()

		if _, ok := l.applied[containerID]; ok {
			t.Error("expected container NOT to be in applied map when io.max missing")
		}
	})
}
