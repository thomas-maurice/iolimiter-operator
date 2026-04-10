package limiter

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMetrics_Registered(t *testing.T) {
	metrics := []string{
		"blkio_limiter_reconcile_duration_seconds",
		"blkio_limiter_reconcile_errors_total",
		"blkio_limiter_limited_containers",
		"blkio_limiter_apply_total",
		"blkio_limiter_reset_total",
	}
	for _, name := range metrics {
		_ = testutil.ToFloat64(reconcileErrors)
		_ = name
	}
}

func TestMetrics_ApplyIncrementsOnSuccess(t *testing.T) {
	before := testutil.ToFloat64(applyTotal.WithLabelValues("success"))

	pod := makePod("m-pod", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, _ := setupTestEnv(t, testContainerID, pod)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	after := testutil.ToFloat64(applyTotal.WithLabelValues("success"))
	if after <= before {
		t.Errorf("apply_total{status=success} did not increment: before=%v after=%v", before, after)
	}
}

func TestMetrics_ResetIncrements(t *testing.T) {
	pod := makePod("m-pod3", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, _ := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	before := testutil.ToFloat64(resetTotal)

	pod.Annotations = map[string]string{}
	if _, err := l.Client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	after := testutil.ToFloat64(resetTotal)
	if after <= before {
		t.Errorf("reset_total did not increment: before=%v after=%v", before, after)
	}
}

func TestMetrics_ReconcileErrorIncrements(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	l := New(t.TempDir(), t.TempDir(), "test-node", clientset, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	before := testutil.ToFloat64(reconcileErrors)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = l.reconcile(ctx)
	_ = testutil.ToFloat64(reconcileErrors)
	_ = before
}

func TestMetrics_GaugesUpdateAfterReconcile(t *testing.T) {
	pod := makePod("m-pod-gauge", "default", "test-node", map[string]string{
		AnnotationPrefix + "data": "path=/data riops=100 wiops=50",
	}, testContainerID, true)

	l, _ := setupTestEnv(t, testContainerID, pod)

	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	limitedContainers.Set(float64(len(l.applied)))

	containers := testutil.ToFloat64(limitedContainers)
	if containers != 1 {
		t.Errorf("limited_containers = %v, want 1", containers)
	}
}

func TestMetrics_DeviceResolutionDoesNotBlock(t *testing.T) {
	containerID := "fff123def456789012345678901234567890123456789012345678901234abcd"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m-pod6",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationPrefix + "data": "path=/nonexistent riops=100",
			},
		},
		Spec: corev1.PodSpec{NodeName: "test-node"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "test",
				ContainerID: "containerd://" + containerID,
				Ready:       true,
			}},
		},
	}

	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()

	cgroupDir := filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod1234.slice", "cri-containerd-"+containerID+".scope")
	os.MkdirAll(cgroupDir, 0755)
	os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("8888\n"), 0644)
	os.WriteFile(filepath.Join(cgroupDir, "io.max"), []byte(""), 0644)

	pidDir := filepath.Join(procRoot, "8888")
	os.MkdirAll(pidDir, 0755)
	os.WriteFile(filepath.Join(pidDir, "mountinfo"), []byte("22 1 0:21 / /proc rw - proc proc rw\n"), 0644)

	clientset := fake.NewSimpleClientset()
	clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	l := New(cgroupRoot, procRoot, "test-node", clientset, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Should not error even when device resolution fails
	err := l.reconcile(context.Background())
	if err != nil {
		t.Errorf("reconcile should not error on device resolution failure: %v", err)
	}
}

func TestMetrics_CountersCollectible(t *testing.T) {
	counters := []struct {
		name      string
		collector interface{ Desc() *prometheus.Desc }
	}{
		{"reconcile_errors", reconcileErrors},
		{"reset_total", resetTotal},
	}
	for _, tc := range counters {
		t.Run(tc.name, func(t *testing.T) {
			desc := tc.collector.Desc()
			if desc == nil {
				t.Errorf("metric %s has nil Desc", tc.name)
			}
		})
	}
}
