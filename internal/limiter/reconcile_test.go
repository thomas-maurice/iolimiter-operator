package limiter

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testContainerID = "abc123def456789012345678901234567890123456789012345678901234abcd"

// setupTestEnv creates a temp cgroup tree + proc tree for a single container.
func setupTestEnv(t *testing.T, containerID string, pods ...*corev1.Pod) (*Limiter, string) {
	t.Helper()

	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()

	cgroupDir := filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("9999\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	mountinfo := `22 1 0:21 / /proc rw - proc proc rw
30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
`
	pidDir := filepath.Join(procRoot, "9999")
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
		t.Fatal(err)
	}

	clientset := fake.NewSimpleClientset()
	for _, p := range pods {
		if _, err := clientset.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	l := New(cgroupRoot, procRoot, "test-node", clientset, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return l, cgroupDir
}

// setupTestEnvMultiMount is like setupTestEnv but creates mountinfo with
// multiple block device mounts.
func setupTestEnvMultiMount(t *testing.T, containerID string, pods ...*corev1.Pod) (*Limiter, string) {
	t.Helper()

	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()

	cgroupDir := filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("9999\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	mountinfo := `22 1 0:21 / /proc rw - proc proc rw
30 1 259:1 / / rw - ext4 /dev/sda1 rw
35 30 7:0 / /data rw - ext4 /dev/loop0 rw
36 30 8:0 / /var/log/app rw - ext4 /dev/loop1 rw
`
	pidDir := filepath.Join(procRoot, "9999")
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte(mountinfo), 0644); err != nil {
		t.Fatal(err)
	}

	clientset := fake.NewSimpleClientset()
	for _, p := range pods {
		if _, err := clientset.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	l := New(cgroupRoot, procRoot, "test-node", clientset, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return l, cgroupDir
}

func makePod(name, ns, nodeName string, annotations map[string]string, containerID string, ready bool) *corev1.Pod {
	cs := corev1.ContainerStatus{
		Name:        "test-container",
		ContainerID: "containerd://" + containerID,
		Ready:       ready,
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

func TestReconcile_AppliesLimits(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	expected := "7:0 riops=100 wiops=50"
	if strings.TrimSpace(string(content)) != expected {
		t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
	}

	if _, ok := l.applied[testContainerID]; !ok {
		t.Error("expected container in applied cache")
	}
}

func TestReconcile_SkipsMissingAnnotation(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "" {
		t.Errorf("io.max should be empty, got %q", string(content))
	}
}

func TestReconcile_SkipsInvalidAnnotation(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "riops=100", // missing path=
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "" {
		t.Errorf("io.max should be empty, got %q", string(content))
	}
}

func TestReconcile_SkipsRootVolumePath(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/ riops=100",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "" {
		t.Errorf("io.max should be empty, got %q", string(content))
	}
}

func TestReconcile_CacheHit(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("MARKER\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "MARKER" {
		t.Errorf("io.max = %q, expected MARKER (cache hit should skip re-apply)", strings.TrimSpace(string(content)))
	}
}

func TestReconcile_AnnotationChanged(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, _ := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	pod.Annotations[AnnotationPrefix+"data"] = "path=/data riops=200 wiops=100"
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Find the cgroup dir to check io.max
	cgroupPath := l.applied[testContainerID].cgroupPath
	content, err := os.ReadFile(filepath.Join(cgroupPath, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	expected := "7:0 riops=200 wiops=100"
	if strings.TrimSpace(string(content)) != expected {
		t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
	}
}

func TestReconcile_ResetsRemovedAnnotation(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	pod.Annotations = map[string]string{}
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	expected := "7:0 riops=max wiops=max rbps=max wbps=max"
	if strings.TrimSpace(string(content)) != expected {
		t.Errorf("io.max = %q, want %q", strings.TrimSpace(string(content)), expected)
	}

	if _, ok := l.applied[testContainerID]; ok {
		t.Error("expected container removed from applied cache")
	}
}

func TestReconcile_SkipsUnreadyContainers(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100",
	}, testContainerID, false)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "" {
		t.Errorf("io.max should be empty for unready container, got %q", string(content))
	}
}

func TestReconcile_MultipleVolumes(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
		AnnotationPrefix + "logs": "path=/var/log/app wbps=1048576",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
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

	rule, ok := l.applied[testContainerID]
	if !ok {
		t.Fatal("expected container in applied cache")
	}
	if len(rule.volumes) != 2 {
		t.Errorf("expected 2 volumes in cache, got %d", len(rule.volumes))
	}
}

func TestReconcile_MultipleVolumesCacheHit(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
		AnnotationPrefix + "logs": "path=/var/log/app wbps=1048576",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("MARKER\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "MARKER" {
		t.Errorf("io.max = %q, expected MARKER (cache hit)", strings.TrimSpace(string(content)))
	}
}

func TestReconcile_ChangeOneVolumeReapplies(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
		AnnotationPrefix + "logs": "path=/var/log/app wbps=1048576",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	pod.Annotations[AnnotationPrefix+"logs"] = "path=/var/log/app wbps=2097152"
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(content))
	if !strings.Contains(got, "8:0 wbps=2097152") {
		t.Errorf("io.max missing updated logs rule, got: %q", got)
	}
}

func TestReconcile_HandlesApplyError(t *testing.T) {
	// Container has no cgroup tree — applyIOLimits will fail at findContainerCgroup.
	containerID := "bbb123def456789012345678901234567890123456789012345678901234abcd"
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100",
	}, containerID, true)

	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()
	// No cgroup tree created — findContainerCgroup will fail

	clientset := fake.NewSimpleClientset()
	clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	l := New(cgroupRoot, procRoot, "test-node", clientset, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Should not return error — apply errors are logged, not propagated
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile should not propagate apply errors: %v", err)
	}

	// Container should NOT be in applied cache
	if _, ok := l.applied[containerID]; ok {
		t.Error("expected container NOT in applied cache after apply error")
	}
}

func TestVolumesEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]volumeRule
		want bool
	}{
		{
			name: "both empty",
			a:    map[string]volumeRule{},
			b:    map[string]volumeRule{},
			want: true,
		},
		{
			name: "equal",
			a:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			b:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			want: true,
		},
		{
			name: "different length",
			a:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			b:    map[string]volumeRule{},
			want: false,
		},
		{
			name: "different name",
			a:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			b:    map[string]volumeRule{"logs": {limit: "riops=100", volumePath: "/data"}},
			want: false,
		},
		{
			name: "different limit",
			a:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			b:    map[string]volumeRule{"data": {limit: "riops=200", volumePath: "/data"}},
			want: false,
		},
		{
			name: "different path",
			a:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/data"}},
			b:    map[string]volumeRule{"data": {limit: "riops=100", volumePath: "/logs"}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := volumesEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("volumesEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseAnnotation(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantPath   string
		wantLimit  string
		wantErr    bool
	}{
		{
			name:      "standard",
			input:     "path=/data riops=100 wiops=50",
			wantPath:  "/data",
			wantLimit: "riops=100 wiops=50",
		},
		{
			name:      "path at end",
			input:     "riops=100 path=/data",
			wantPath:  "/data",
			wantLimit: "riops=100",
		},
		{
			name:      "all params",
			input:     "path=/data riops=100 wiops=50 rbps=10485760 wbps=5242880",
			wantPath:  "/data",
			wantLimit: "riops=100 wiops=50 rbps=10485760 wbps=5242880",
		},
		{
			name:    "missing path",
			input:   "riops=100 wiops=50",
			wantErr: true,
		},
		{
			name:    "no limits",
			input:   "path=/data",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, limit, err := parseAnnotation(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
			if limit != tt.wantLimit {
				t.Errorf("limit = %q, want %q", limit, tt.wantLimit)
			}
		})
	}
}
