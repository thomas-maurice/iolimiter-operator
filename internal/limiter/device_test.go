package limiter

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseIOMaxRules(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int // number of rules
		keys    []string
	}{
		{
			name:    "empty input",
			content: "",
			want:    0,
		},
		{
			name:    "all-max lines",
			content: "7:0 rbps=max wbps=max riops=max wiops=max\n8:0 rbps=max wbps=max riops=max wiops=max\n",
			want:    0,
		},
		{
			name:    "single active rule",
			content: "7:0 riops=100 wiops=50 rbps=max wbps=max\n",
			want:    1,
			keys:    []string{"7:0"},
		},
		{
			name:    "multiple devices",
			content: "7:0 riops=100 wiops=max rbps=max wbps=max\n8:0 rbps=1048576 wbps=max riops=max wiops=max\n",
			want:    2,
			keys:    []string{"7:0", "8:0"},
		},
		{
			name:    "malformed line - single field",
			content: "7:0\n",
			want:    0,
		},
		{
			name:    "mixed active and max",
			content: "7:0 riops=100 wiops=50 rbps=max wbps=max\n8:0 rbps=max wbps=max riops=max wiops=max\n",
			want:    1,
			keys:    []string{"7:0"},
		},
		{
			name:    "whitespace only",
			content: "   \n  \n",
			want:    0,
		},
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
	// Sample mountinfo content (fields: mountID parentID major:minor root mountPoint ...)
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
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
			t.Fatal(err)
		}

		l := &Limiter{ProcRoot: dir, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		got, err := l.findBlockDeviceForPath(log, "1234", "/data")
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
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
			t.Fatal(err)
		}

		l := &Limiter{ProcRoot: dir, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		got, err := l.findBlockDeviceForPath(log, "1234", "/data/subdir")
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
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
			t.Fatal(err)
		}

		l := &Limiter{ProcRoot: dir, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		got, err := l.findBlockDeviceForPath(log, "1234", "/var/lib/something")
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
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(pseudoOnly), 0644); err != nil {
			t.Fatal(err)
		}

		l := &Limiter{ProcRoot: dir, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		_, err := l.findBlockDeviceForPath(log, "1234", "/data")
		if err == nil {
			t.Error("expected error for pseudo-fs only, got nil")
		}
	})

	t.Run("most specific mount wins", func(t *testing.T) {
		// /data/deep is more specific than /data
		deepMount := `30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
36 35 8:0 / /data/deep rw - ext4 /dev/loop1 rw
`
		dir := t.TempDir()
		pidDir := filepath.Join(dir, "1234")
		if err := os.MkdirAll(pidDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(deepMount), 0644); err != nil {
			t.Fatal(err)
		}

		l := &Limiter{ProcRoot: dir, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		got, err := l.findBlockDeviceForPath(log, "1234", "/data/deep/file")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "8:0" {
			t.Errorf("got %q, want %q", got, "8:0")
		}
	})
}
