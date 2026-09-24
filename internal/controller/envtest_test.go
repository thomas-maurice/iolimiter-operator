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

package controller

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// repoRootFromPackageDir returns the module root, given `go test` always
// runs with the package directory (internal/controller) as its cwd.
func repoRootFromPackageDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// startEnvtestConfig regenerates the CRDs from the current API source into a
// temp dir (so these tests exercise in-flight marker changes, not whatever
// happens to be checked into config/crd/bases) and starts envtest against
// it, returning the rest.Config. Skips the test if KUBEBUILDER_ASSETS is
// unset, so `go test` works without `make test` having installed the
// envtest binaries.
func startEnvtestConfig(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test` (or set it to bin/setup-envtest's output) for the envtest suite")
	}

	root := repoRootFromPackageDir(t)
	crdDir := t.TempDir()
	cmd := exec.Command(filepath.Join(root, "bin", "controller-gen"), "crd",
		"paths=./api/...", "output:crd:artifacts:config="+crdDir)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "controller-gen failed: %s", out)

	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr)))
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, testEnv.Stop())
	})
	return cfg
}

// startEnvtestClient is startEnvtestConfig plus a plain (uncached) client
// against it -- enough for the CEL/immutability/field-selector suites in
// this file, which never use a field index.
func startEnvtestClient(t *testing.T) client.Client {
	t.Helper()
	cfg := startEnvtestConfig(t)
	k8sClient, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	return k8sClient
}

func newTestNamespace(t *testing.T, k8sClient client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, k8sClient.Create(context.Background(), ns))
}

func baseIOLimiter(ns, name string) *storagev1alpha1.IOLimiter {
	return &storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: storagev1alpha1.IOLimiterSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
			Volumes: []storagev1alpha1.VolumeLimit{
				{Name: "data", Limits: storagev1alpha1.IOLimits{}},
			},
		},
	}
}

// TestIOLimiter_CEL_QuantityLimits_Envtest proves what the schema/CEL on
// IOLimits actually enforces against a real apiserver (the fake client
// never runs CEL). Both the Quantity string form ("10Mi") and the
// plain-integer form (1048576, the same value in bytes) must be accepted,
// and a volume with no limit set at all must be rejected (has(...) CEL,
// cheap: no quantity() call).
//
// Deviation from SPEC.md §4 (recorded there under C0): the 1Ki..1Pi
// range/whole-number check on readBytesPerSecond/writeBytesPerSecond is
// NOT enforced by CEL here. Every quantity()/isQuantity() CEL call on
// these fields -- even a single, bound-free call -- exceeds the
// apiserver's CEL cost budget once volumes[] carries its spec-mandated
// MaxItems=16 (measured: ~1.006x over budget for one call; ~2x for two).
// There is no controller-gen marker to bound the field's maxLength (it
// rejects MaxLength on a non-string Go type), so the cost estimator always
// assumes an unbounded string. "100m" and "512" (both below 1Ki) are
// therefore accepted by CEL here; the range/integer check must move to a
// controller-side check (desired.Compute in C4, or a C7 VAP) instead.
func TestIOLimiter_CEL_QuantityLimits_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "iol-quantity")

	tests := []struct {
		name    string
		limits  storagev1alpha1.IOLimits
		wantErr bool
	}{
		{
			name:    "accept quantity string form 10Mi",
			limits:  storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")},
			wantErr: false,
		},
		{
			name:    "accept quantity integer form 1048576",
			limits:  storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("1048576")},
			wantErr: false,
		},
		{
			// SPEC.md wants this rejected; CEL cost budget forces this to
			// be a controller-side check instead (see the deviation note
			// above). Documents actual behaviour so a future CEL fix is a
			// visible test change, not a silent regression.
			name:    "accepted by CEL despite being non-integer (100m): controller-side check deferred",
			limits:  storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("100m")},
			wantErr: false,
		},
		{
			name:    "accepted by CEL despite being below 1Ki (512): controller-side check deferred",
			limits:  storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("512")},
			wantErr: false,
		},
		{
			name:    "reject no limits set at all",
			limits:  storagev1alpha1.IOLimits{},
			wantErr: true,
		},
		{
			name:    "reject readIOPS of 1 (kernel ignores <=1)",
			limits:  storagev1alpha1.IOLimits{ReadIOPS: ptr.To[int64](1)},
			wantErr: true,
		},
		{
			name:    "reject readIOPS above uint32 max",
			limits:  storagev1alpha1.IOLimits{ReadIOPS: ptr.To[int64](4294967296)},
			wantErr: true,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr := baseIOLimiter("iol-quantity", fmt.Sprintf("case-%d", i))
			cr.Spec.Volumes[0].Limits = tc.limits
			err := k8sClient.Create(ctx, cr)
			if tc.wantErr {
				assert.Error(t, err, "expected the apiserver to reject this IOLimiter")
			} else {
				assert.NoError(t, err, "expected the apiserver to accept this IOLimiter")
			}
		})
	}
}

// TestIOLimiter_CEL_DuplicateVolumeNames_Envtest: spec.volumes is a
// listType=map keyed by name, so the apiserver must reject two entries with
// the same volume name (silently keeping the last one would misrepresent
// what the user asked for).
func TestIOLimiter_CEL_DuplicateVolumeNames_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "iol-dupvol")

	cr := baseIOLimiter("iol-dupvol", "dup")
	cr.Spec.Volumes = []storagev1alpha1.VolumeLimit{
		{Name: "data", Limits: storagev1alpha1.IOLimits{ReadBytesPerSecond: quantityPtr("10Mi")}},
		{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}},
	}
	err := k8sClient.Create(ctx, cr)
	assert.Error(t, err, "duplicate volume names must be rejected (listMapKey=name)")
}

// TestPodIOLimitSpec_Identity_Immutable_Envtest proves the CEL
// self==oldSelf rule on PodIOLimitSpec: nodeName/podName/podUID/qosClass
// must never change after create (D18) against a real apiserver.
func TestPodIOLimitSpec_Identity_Immutable_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "pil-immutable")

	cr := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: "pil-immutable"},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: "node-a",
			PodName:  "web-0",
			PodUID:   types.UID("abcd1234-0000-0000-0000-000000000000"),
			QOSClass: corev1.PodQOSGuaranteed,
			Volumes: []storagev1alpha1.PodVolumeLimit{
				{
					Name:           "data",
					KubeletDirName: "pvc-abc",
					Limits:         storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](1048576)},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cr))

	// Mutable: status is a subresource, unaffected by the spec CEL rule.
	// Immutable: any of the four identity fields.
	badNode := cr.DeepCopy()
	badNode.Spec.NodeName = "node-b"
	assert.Error(t, k8sClient.Update(ctx, badNode), "nodeName must be immutable")

	badPod := cr.DeepCopy()
	badPod.Spec.PodName = "web-1"
	assert.Error(t, k8sClient.Update(ctx, badPod), "podName must be immutable")

	badUID := cr.DeepCopy()
	badUID.Spec.PodUID = types.UID("different-uid")
	assert.Error(t, k8sClient.Update(ctx, badUID), "podUID must be immutable")

	badQOS := cr.DeepCopy()
	badQOS.Spec.QOSClass = corev1.PodQOSBurstable
	assert.Error(t, k8sClient.Update(ctx, badQOS), "qosClass must be immutable")

	// A non-identity field (volumes) may change.
	okVolumes := cr.DeepCopy()
	okVolumes.Spec.Volumes[0].Limits.ReadBPS = ptr.To[int64](2097152)
	assert.NoError(t, k8sClient.Update(ctx, okVolumes), "volumes must remain mutable")
}

// TestPodIOLimitSpec_D39_PodUIDMustBeUUID_Envtest proves the D39 CEL rule
// against a real apiserver: a podUID that isn't a UUID must be rejected at
// admission, before the agent ever sees it and trusts it into a
// cgroup/filesystem path. (Field-level +kubebuilder:validation:Pattern
// doesn't compile against types.UID's schema -- see the doc comment on
// PodUID -- so this is a spec-level CEL rule, proven here since the fake
// client used elsewhere never runs CEL.)
func TestPodIOLimitSpec_D39_PodUIDMustBeUUID_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "pil-uuid")

	mk := func(name string, uid types.UID) *storagev1alpha1.PodIOLimit {
		return &storagev1alpha1.PodIOLimit{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pil-uuid"},
			Spec: storagev1alpha1.PodIOLimitSpec{
				NodeName: "node-a",
				PodName:  name,
				PodUID:   uid,
				QOSClass: corev1.PodQOSBestEffort,
				Volumes: []storagev1alpha1.PodVolumeLimit{
					{Name: "data", KubeletDirName: "pvc-x", Limits: storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](1048576)}},
				},
			},
		}
	}

	assert.Error(t, k8sClient.Create(ctx, mk("bad-uid", types.UID("not-a-uuid"))), "a non-UUID podUID must be rejected")
	assert.Error(t, k8sClient.Create(ctx, mk("path-escape", types.UID("../../etc/passwd"))), "a path-shaped podUID must be rejected")
	assert.NoError(t, k8sClient.Create(ctx, mk("good-uid", types.UID("abcd1234-0000-0000-0000-000000000000"))), "a real UUID must be accepted")
}

// TestPodIOLimitSpec_D39_KubeletDirNameMustBeDNSSubdomain_Envtest proves the
// CRD pattern on kubeletDirName: it must be a DNS-1123 subdomain (PV names
// are DNS subdomains; the inline-CSI case reuses the already
// label-shaped pod volume name), so a forged value can't escape the
// kubelet volumes directory the agent builds a path from.
func TestPodIOLimitSpec_D39_KubeletDirNameMustBeDNSSubdomain_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "pil-dirname")

	mk := func(name, dirName string) *storagev1alpha1.PodIOLimit {
		return &storagev1alpha1.PodIOLimit{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pil-dirname"},
			Spec: storagev1alpha1.PodIOLimitSpec{
				NodeName: "node-a",
				PodName:  name,
				PodUID:   types.UID("abcd1234-0000-0000-0000-000000000000"),
				QOSClass: corev1.PodQOSBestEffort,
				Volumes: []storagev1alpha1.PodVolumeLimit{
					{Name: "data", KubeletDirName: dirName, Limits: storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](1048576)}},
				},
			},
		}
	}

	assert.Error(t, k8sClient.Create(ctx, mk("path-escape", "../../etc")), "a path-shaped kubeletDirName must be rejected")
	assert.Error(t, k8sClient.Create(ctx, mk("upper", "Not-Valid")), "an uppercase kubeletDirName must be rejected")
	assert.NoError(t, k8sClient.Create(ctx, mk("good", "pvc-1234")), "a plain DNS label must be accepted")
}

// TestPodIOLimit_FieldSelector_NodeName_Envtest proves the
// +kubebuilder:selectablefield on spec.nodeName actually filters server-side
// (RBAC and the fake client both let a List through unfiltered; the agent's
// node-scoped cache (D16) depends on the apiserver doing the filtering).
func TestPodIOLimit_FieldSelector_NodeName_Envtest(t *testing.T) {
	k8sClient := startEnvtestClient(t)
	ctx := context.Background()
	newTestNamespace(t, k8sClient, "pil-fieldsel")

	mk := func(name, node string) *storagev1alpha1.PodIOLimit {
		return &storagev1alpha1.PodIOLimit{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pil-fieldsel"},
			Spec: storagev1alpha1.PodIOLimitSpec{
				NodeName: node,
				PodName:  name,
				// D39: podUID must be a UUID.
				PodUID:   types.UID(fmt.Sprintf("11111111-1111-1111-1111-%012x", crc32.ChecksumIEEE([]byte(name)))),
				QOSClass: corev1.PodQOSBestEffort,
				Volumes: []storagev1alpha1.PodVolumeLimit{
					{Name: "data", KubeletDirName: "pvc-x", Limits: storagev1alpha1.DeviceLimits{ReadBPS: ptr.To[int64](1048576)}},
				},
			},
		}
	}
	require.NoError(t, k8sClient.Create(ctx, mk("pod-a", "node-a")))
	require.NoError(t, k8sClient.Create(ctx, mk("pod-b", "node-b")))
	require.NoError(t, k8sClient.Create(ctx, mk("pod-c", "node-a")))

	var list storagev1alpha1.PodIOLimitList
	require.NoError(t, k8sClient.List(ctx, &list,
		client.InNamespace("pil-fieldsel"),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.nodeName", "node-a")},
	))

	got := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		got = append(got, item.Name)
	}
	assert.ElementsMatch(t, []string{"pod-a", "pod-c"}, got, "field-selected List must return only node-a's PodIOLimits")
}

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}
