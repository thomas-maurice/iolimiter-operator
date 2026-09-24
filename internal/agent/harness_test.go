/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/cgroup"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, storagev1alpha1.AddToScheme(s))
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&storagev1alpha1.PodIOLimit{}).
		WithObjects(objs...).
		Build()
}

// testFixture wires a PodIOLimitReconciler against a real (tmp-dir) cgroup
// tree and mountinfo/sysfs, and a fake client/recorder, so Reconcile
// exercises real file I/O the way iomax/mountinfo/blockdev's own tests do
// (C1's "pure, host-path-injectable" packages), without any real kernel.
type testFixture struct {
	t          *testing.T
	root       string // tmp root: root/cgroup, root/proc, root/sys
	cgroupRoot string
	procRoot   string
	sysRoot    string
	layout     cgroup.Layout
	recorder   *events.FakeRecorder
	client     client.WithWatch
	reconciler *PodIOLimitReconciler
}

const testNode = "node-a"

func newFixture(t *testing.T, objs ...client.Object) *testFixture {
	t.Helper()
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "cgroup")
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")
	require.NoError(t, os.MkdirAll(filepath.Join(cgroupRoot, "kubepods"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(sysRoot, "dev", "block"), 0o755))
	writeFile(t, filepath.Join(procRoot, "1", "mountinfo"), "")

	layout, err := cgroup.DiscoverKubepods(cgroupRoot, "")
	require.NoError(t, err)

	rec := events.NewFakeRecorder(50)
	c := newFakeClient(t, objs...)

	f := &testFixture{
		t: t, root: root, cgroupRoot: cgroupRoot, procRoot: procRoot, sysRoot: sysRoot,
		layout: layout, recorder: rec, client: c,
	}
	kubeletRootDir := filepath.Join(root, "kubelet")
	f.reconciler = &PodIOLimitReconciler{
		Client:         c,
		Scheme:         testScheme(t),
		Recorder:       rec,
		NodeName:       testNode,
		Layout:         layout,
		CgroupRoot:     cgroupRoot,
		ProcRoot:       procRoot,
		KubeletRootDir: kubeletRootDir,
		SysRoot:        sysRoot,
		ResyncPeriod:   60 * time.Second,
		// D34's default set, mapped onto the fixture's tmp-dir "kubelet
		// root": "/" plus the kubelet root dir, mirroring D23's old
		// fixed pair for every existing test that relies on it.
		ProtectedPaths: []string{"/", kubeletRootDir},
	}
	return f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// podCgroupDir creates the pod-level cgroup directory (with a subtree_control
// enabling io, and an io.max file) that PodPath would compute for uid/qos,
// and returns it.
func (f *testFixture) podCgroupDir(t *testing.T, uid types.UID, qos corev1.PodQOSClass, ioMaxContent string) string {
	t.Helper()
	p, err := f.layout.PodPath(uid, qos)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(p, 0o755))
	writeFile(t, filepath.Join(p, "io.max"), ioMaxContent)
	return p
}

// mountEntry appends one mountinfo line for <kubeletRoot>/pods/<uid>/volumes/<plugin>/<dir>,
// device dev ("MAJ:MIN"), and rewrites procRoot/1/mountinfo.
func (f *testFixture) addMount(t *testing.T, existing string, uid types.UID, plugin, dir, dev string) string {
	t.Helper()
	mountPoint := filepath.Join(f.reconciler.KubeletRootDir, "pods", string(uid), "volumes", plugin, dir)
	line := "1 1 " + dev + " / " + mountPoint + " rw,relatime - ext4 /dev/sda1 rw\n"
	content := existing + line
	writeFile(t, filepath.Join(f.procRoot, "1", "mountinfo"), content)
	return content
}

// partitionDevice builds a real, symlinked fake sysfs partition->whole-disk
// mapping (K4), mirroring internal/blockdev's own fixture. WholeDisk
// short-circuits to "not a partition" as soon as
// <sysRoot>/dev/block/<dev>/partition doesn't exist, so a plain whole-disk
// device (major != 0) needs no fixture at all: an absent stat target is
// enough.
func (f *testFixture) partitionDevice(t *testing.T, parentDev, childDev string) {
	t.Helper()
	parentDir := filepath.Join(f.sysRoot, "devices", "virtual", "block", "disk")
	childDir := filepath.Join(parentDir, "disk1")
	require.NoError(t, os.MkdirAll(childDir, 0o755))
	writeFile(t, filepath.Join(parentDir, "dev"), parentDev+"\n")
	writeFile(t, filepath.Join(childDir, "dev"), childDev+"\n")
	writeFile(t, filepath.Join(childDir, "partition"), "1\n")
	require.NoError(t, os.Symlink(parentDir, filepath.Join(f.sysRoot, "dev", "block", parentDev)))
	require.NoError(t, os.Symlink(childDir, filepath.Join(f.sysRoot, "dev", "block", childDev)))
}

func (f *testFixture) reconcile(t *testing.T, pil *storagev1alpha1.PodIOLimit) (ctrl.Result, []string, error) {
	t.Helper()
	ctx, lines := capturingLogger(1)
	res, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pil.Namespace, Name: pil.Name}})
	return res, *lines, err
}

func capturingLogger(verbosity int) (context.Context, *[]string) {
	var lines []string
	logger := funcr.New(func(prefix, args string) {
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{Verbosity: verbosity})
	return logf.IntoContext(context.Background(), logger), &lines
}

func containsAll(lines []string, substrs ...string) bool {
	for _, l := range lines {
		ok := true
		for _, s := range substrs {
			if !strings.Contains(l, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func getPodIOLimit(t *testing.T, c client.Client, ns, name string) *storagev1alpha1.PodIOLimit {
	t.Helper()
	var pil storagev1alpha1.PodIOLimit
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &pil))
	return &pil
}

func readIOMax(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// drainEventsContain drains every currently-buffered event off rec and
// reports whether any contains substr, so a test asserting on a later
// transition's event isn't tripped up by an earlier reconcile's event
// still sitting unread in the channel.
func drainEventsContain(rec *events.FakeRecorder, substr string) bool {
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, substr) {
				return true
			}
		default:
			return false
		}
	}
}

// drainEventCount drains every currently-buffered event off rec and
// returns how many contain substr.
func drainEventCount(rec *events.FakeRecorder, substr string) int {
	n := 0
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, substr) {
				n++
			}
		default:
			return n
		}
	}
}

func basePodIOLimit(ns, name, podName string, uid types.UID, qos corev1.PodQOSClass, node string, volumes ...storagev1alpha1.PodVolumeLimit) *storagev1alpha1.PodIOLimit {
	return &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: node,
			PodName:  podName,
			PodUID:   uid,
			QOSClass: qos,
			Volumes:  volumes,
		},
	}
}
