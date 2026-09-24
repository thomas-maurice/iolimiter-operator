package cgroup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
}

func TestDiscoverKubepods_AllFourLayouts(t *testing.T) {
	cases := []struct {
		name       string
		layoutDir  string
		wantDriver Driver
		wantPrefix string
	}{
		{"systemd default", "kubepods.slice", Systemd, "kubepods"},
		{"systemd kind (kubelet.slice wrapper)", filepath.Join("kubelet.slice", "kubelet-kubepods.slice"), Systemd, "kubelet-kubepods"},
		{"cgroupfs default", "kubepods", Cgroupfs, ""},
		{"cgroupfs kubelet wrapper", filepath.Join("kubelet", "kubepods"), Cgroupfs, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkdirs(t, root, tc.layoutDir)

			l, err := DiscoverKubepods(root, "")
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, tc.layoutDir), l.Base)
			assert.Equal(t, tc.wantDriver, l.Driver)
			assert.Equal(t, tc.wantPrefix, l.Prefix)
		})
	}
}

func TestDiscoverKubepods_PrefersFirstCandidate(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "kubepods.slice", "kubepods")

	l, err := DiscoverKubepods(root, "")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "kubepods.slice"), l.Base)
}

func TestDiscoverKubepods_NoneFound(t *testing.T) {
	root := t.TempDir()
	_, err := DiscoverKubepods(root, "")
	require.Error(t, err)
}

func TestDiscoverKubepods_Override(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, filepath.Join("kubelet.slice", "kubelet-kubepods.slice"))

	l, err := DiscoverKubepods(root, filepath.Join("kubelet.slice", "kubelet-kubepods.slice"))
	require.NoError(t, err)
	assert.Equal(t, Systemd, l.Driver)
	assert.Equal(t, "kubelet-kubepods", l.Prefix)
}

func TestDiscoverKubepods_OverrideMissing(t *testing.T) {
	root := t.TempDir()
	_, err := DiscoverKubepods(root, "does-not-exist.slice")
	require.Error(t, err)
}

// wantPath renders the expected pod cgroup path for a layout, mirroring K9
// directly (kept separate from PodPath's own construction so the test can
// actually catch a K9 regression instead of restating the implementation).
func wantSystemdPath(base, prefix, uidUnderscored string, qos corev1.PodQOSClass) string {
	if qos == corev1.PodQOSGuaranteed {
		return filepath.Join(base, prefix+"-pod"+uidUnderscored+".slice")
	}
	q := map[corev1.PodQOSClass]string{corev1.PodQOSBurstable: "burstable", corev1.PodQOSBestEffort: "besteffort"}[qos]
	return filepath.Join(base, prefix+"-"+q+".slice", prefix+"-"+q+"-pod"+uidUnderscored+".slice")
}

func wantCgroupfsPath(base, uid string, qos corev1.PodQOSClass) string {
	if qos == corev1.PodQOSGuaranteed {
		return filepath.Join(base, "pod"+uid)
	}
	q := map[corev1.PodQOSClass]string{corev1.PodQOSBurstable: "burstable", corev1.PodQOSBestEffort: "besteffort"}[qos]
	return filepath.Join(base, q, "pod"+uid)
}

func TestPodPath_SystemdAndCgroupfsAcrossQoS(t *testing.T) {
	uid := types.UID("1234abcd-5678-90ef-aaaa-bbbbccccdddd")
	uidUnderscored := "1234abcd_5678_90ef_aaaa_bbbbccccdddd"
	qosClasses := []corev1.PodQOSClass{corev1.PodQOSGuaranteed, corev1.PodQOSBurstable, corev1.PodQOSBestEffort}

	t.Run("systemd", func(t *testing.T) {
		root := t.TempDir()
		mkdirs(t, root, "kubepods.slice")
		l, err := DiscoverKubepods(root, "")
		require.NoError(t, err)

		for _, qos := range qosClasses {
			p, err := l.PodPath(uid, qos)
			require.NoError(t, err)
			assert.Equal(t, wantSystemdPath(l.Base, "kubepods", uidUnderscored, qos), p, "qos=%s", qos)
			assert.Contains(t, p, uidUnderscored, "systemd slice names replace dashes with underscores (K9)")
			assert.NotContains(t, p, string(uid), "the raw dashed UID must not appear in a systemd slice name")
		}
	})

	t.Run("cgroupfs", func(t *testing.T) {
		root := t.TempDir()
		mkdirs(t, root, "kubepods")
		l, err := DiscoverKubepods(root, "")
		require.NoError(t, err)

		for _, qos := range qosClasses {
			p, err := l.PodPath(uid, qos)
			require.NoError(t, err)
			assert.Equal(t, wantCgroupfsPath(l.Base, string(uid), qos), p, "qos=%s", qos)
			assert.Contains(t, p, string(uid), "cgroupfs keeps the raw (dashed) UID, unlike systemd (K9)")
		}
	})

	t.Run("kind layout (kubelet.slice wrapper)", func(t *testing.T) {
		root := t.TempDir()
		mkdirs(t, root, filepath.Join("kubelet.slice", "kubelet-kubepods.slice"))
		l, err := DiscoverKubepods(root, "")
		require.NoError(t, err)

		for _, qos := range qosClasses {
			p, err := l.PodPath(uid, qos)
			require.NoError(t, err)
			assert.Equal(t, wantSystemdPath(l.Base, "kubelet-kubepods", uidUnderscored, qos), p, "qos=%s", qos)
		}
	})
}

func TestPodPath_RefusesEscapeOutsideKubepodsRoot(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "kubepods")
	l, err := DiscoverKubepods(root, "")
	require.NoError(t, err)

	// A forged UID must never make the computed path climb out of the
	// kubepods root (D6: "refuses anything outside the discovered
	// kubepods root" is defense in depth against a forged PodIOLimit).
	maliciousUID := types.UID("../../../../etc")
	_, err = l.PodPath(maliciousUID, corev1.PodQOSGuaranteed)
	require.Error(t, err)
}

func TestPodPath_RefusesEqualToKubepodsRoot(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "kubepods")
	l, err := DiscoverKubepods(root, "")
	require.NoError(t, err)

	// D39 / security review: a forged UID of "/.." cleans straight back
	// down to the kubepods root itself (cgroupfs: rel = "pod/.." ->
	// base). Accepting that would let the agent write io.max on the
	// kubepods root cgroup, throttling every pod on the node instead of
	// one -- within() must reject "equal to base", not just "outside
	// base".
	maliciousUID := types.UID("/..")
	_, err = l.PodPath(maliciousUID, corev1.PodQOSGuaranteed)
	require.Error(t, err)
}

func TestIOControllerEnabled(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "io.max"), nil, 0o644))

	enabled, err := IOControllerEnabled(root)
	require.NoError(t, err)
	assert.True(t, enabled)

	other := t.TempDir()
	enabled, err = IOControllerEnabled(other)
	require.NoError(t, err)
	assert.False(t, enabled, "no io.max file means the io controller isn't enabled on this cgroup")
}
