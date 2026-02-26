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
// Returns the Limiter and the cgroup dir path.
func setupTestEnv(t *testing.T, containerID string, pods ...*corev1.Pod) (*Limiter, string) {
	t.Helper()

	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()

	// Create cgroup tree
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

	// Create mountinfo for the PID
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

	// Build fake clientset
	objs := make([]corev1.Pod, len(pods))
	for i, p := range pods {
		objs[i] = *p
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

	// Create cgroup tree
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

	// Create mountinfo with two block device mounts
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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
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

	// Verify it's in the applied cache
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

	// io.max should still be empty
	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "" {
		t.Errorf("io.max should be empty, got %q", string(content))
	}
}

func TestReconcile_SkipsOrphanedConfig(t *testing.T) {
	// Config without matching path - should warn and skip.
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationConfigPrefix + "data": "riops=100",
		// no path.data
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

func TestReconcile_SkipsOrphanedPath(t *testing.T) {
	// Path without matching config - should warn and skip.
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationPathPrefix + "data": "/data",
		// no config.data
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
		AnnotationConfigPrefix + "data": "riops=100",
		AnnotationPathPrefix + "data":   "/",
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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	// First reconcile - applies
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Overwrite io.max with something different to detect re-writes
	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("MARKER\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second reconcile - should skip (cache hit)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}
	// Should still have MARKER because reconcile skipped
	if strings.TrimSpace(string(content)) != "MARKER" {
		t.Errorf("io.max = %q, expected MARKER (cache hit should skip re-apply)", strings.TrimSpace(string(content)))
	}
}

func TestReconcile_AnnotationChanged(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	// First reconcile
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Update pod annotation
	pod.Annotations[AnnotationConfigPrefix+"data"] = "riops=200,wiops=100"
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Second reconcile - should re-apply
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnv(t, testContainerID, pod)

	// First reconcile - applies
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Remove annotations
	pod.Annotations = map[string]string{}
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Second reconcile - should reset
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

	// Should be removed from cache
	if _, ok := l.applied[testContainerID]; ok {
		t.Error("expected container removed from applied cache")
	}
}

func TestReconcile_SkipsUnreadyContainers(t *testing.T) {
	pod := makePod("test-pod", "default", "test-node", map[string]string{
		AnnotationConfigPrefix + "data": "riops=100",
		AnnotationPathPrefix + "data":   "/data",
	}, testContainerID, false) // not ready

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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
		AnnotationConfigPrefix + "logs": "wbps=1048576",
		AnnotationPathPrefix + "logs":   "/var/log/app",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cgroupDir, "io.max"))
	if err != nil {
		t.Fatal(err)
	}

	// Both rules should be present (order may vary).
	got := strings.TrimSpace(string(content))
	if !strings.Contains(got, "7:0 riops=100 wiops=50") {
		t.Errorf("io.max missing data rule, got: %q", got)
	}
	if !strings.Contains(got, "8:0 wbps=1048576") {
		t.Errorf("io.max missing logs rule, got: %q", got)
	}

	// Cache should have both volumes.
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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
		AnnotationConfigPrefix + "logs": "wbps=1048576",
		AnnotationPathPrefix + "logs":   "/var/log/app",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	// First reconcile - applies
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Overwrite io.max to detect re-writes
	if err := os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte("MARKER\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second reconcile - should skip (cache hit, both volumes unchanged)
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
		AnnotationConfigPrefix + "data": "riops=100,wiops=50",
		AnnotationPathPrefix + "data":   "/data",
		AnnotationConfigPrefix + "logs": "wbps=1048576",
		AnnotationPathPrefix + "logs":   "/var/log/app",
	}, testContainerID, true)

	l, cgroupDir := setupTestEnvMultiMount(t, testContainerID, pod)

	// First reconcile
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Change only the logs limit
	pod.Annotations[AnnotationConfigPrefix+"logs"] = "wbps=2097152"
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Second reconcile - should re-apply all (the set changed)
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
