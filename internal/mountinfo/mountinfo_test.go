package mountinfo

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestParse_ExtractsDeviceAndMountPoint(t *testing.T) {
	content := "36 35 8:1 / /mnt/foo rw,relatime shared:1 - ext4 /dev/sda1 rw\n"
	entries, err := Parse(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "8:1", entries[0].Device)
	assert.Equal(t, "/mnt/foo", entries[0].MountPoint)
}

func TestParse_UnescapesOctalSequences(t *testing.T) {
	// mountinfo escapes space (\040), tab (\011), newline (\012) and
	// backslash (\134) in path fields so whitespace-splitting a line
	// stays unambiguous. A parser that doesn't unescape would return a
	// mount point that never matches the real filesystem path.
	content := `36 35 8:1 / /mnt/my\040volume\040name rw - ext4 /dev/sda1 rw` + "\n"
	entries, err := Parse(strings.NewReader(content))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "/mnt/my volume name", entries[0].MountPoint)
}

func TestParse_SkipsShortLines(t *testing.T) {
	entries, err := Parse(strings.NewReader("garbage\n\n36 35 8:1 / /mnt rw - ext4 /dev/sda1 rw\n"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "/mnt", entries[0].MountPoint)
}

func csiEntry(podUID, dirName, device string) Entry {
	return Entry{
		Device:     device,
		MountPoint: "/var/lib/kubelet/pods/" + podUID + "/volumes/kubernetes.io~csi/" + dirName + "/mount",
	}
}

func localVolumeEntry(podUID, dirName, device string) Entry {
	return Entry{
		Device:     device,
		MountPoint: "/var/lib/kubelet/pods/" + podUID + "/volumes/kubernetes.io~local-volume/" + dirName,
	}
}

func TestFindKubeletVolume_CSIWithMountSuffix(t *testing.T) {
	uid := types.UID("abc-123")
	entries := []Entry{csiEntry("abc-123", "pvc-data", "8:16")}

	e, err := FindKubeletVolume(entries, "/var/lib/kubelet", uid, "pvc-data")
	require.NoError(t, err)
	assert.Equal(t, "8:16", e.Device)
	assert.Equal(t, "/var/lib/kubelet/pods/abc-123/volumes/kubernetes.io~csi/pvc-data/mount", e.MountPoint,
		"the caller needs the mount point too, to log it without re-reading mountinfo")
}

func TestFindKubeletVolume_LocalVolumeNoMountSuffix(t *testing.T) {
	uid := types.UID("abc-123")
	entries := []Entry{localVolumeEntry("abc-123", "pv-data", "7:0")}

	e, err := FindKubeletVolume(entries, "/var/lib/kubelet", uid, "pv-data")
	require.NoError(t, err)
	assert.Equal(t, "7:0", e.Device)
}

func TestFindKubeletVolume_ShadowedMountLastWins(t *testing.T) {
	// D6: a later mount at the same path shadows an earlier one (e.g. a
	// remount after volume expansion). mountinfo lists mounts in the
	// order they happened, so the resolver must take the last match.
	uid := types.UID("abc-123")
	entries := []Entry{
		csiEntry("abc-123", "pvc-data", "8:16"),
		csiEntry("abc-123", "pvc-data", "8:32"),
	}

	e, err := FindKubeletVolume(entries, "/var/lib/kubelet", uid, "pvc-data")
	require.NoError(t, err)
	assert.Equal(t, "8:32", e.Device, "the later mount must win, not the first")
}

func TestFindKubeletVolume_MajorZero(t *testing.T) {
	// K5: major 0 (overlay/tmpfs/NFS/...) has no request queue and can't
	// be throttled, but resolution itself must still succeed: the
	// unsupported decision belongs to the agent (Unsupported/NoBlockDevice),
	// not to mountinfo.
	uid := types.UID("abc-123")
	entries := []Entry{localVolumeEntry("abc-123", "pv-data", "0:42")}

	e, err := FindKubeletVolume(entries, "/var/lib/kubelet", uid, "pv-data")
	require.NoError(t, err)
	assert.Equal(t, "0:42", e.Device)
}

func TestFindKubeletVolume_NotFound(t *testing.T) {
	uid := types.UID("abc-123")
	_, err := FindKubeletVolume(nil, "/var/lib/kubelet", uid, "pv-data")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestFindKubeletVolume_DoesNotMatchOtherPodOrVolume(t *testing.T) {
	entries := []Entry{
		localVolumeEntry("other-uid", "pv-data", "8:0"),
		csiEntry("abc-123", "other-vol", "8:16"),
	}
	_, err := FindKubeletVolume(entries, "/var/lib/kubelet", types.UID("abc-123"), "pv-data")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestParseDevice(t *testing.T) {
	maj, min, err := ParseDevice("259:1")
	require.NoError(t, err)
	assert.Equal(t, 259, maj)
	assert.Equal(t, 1, min)

	_, _, err = ParseDevice("not-a-device")
	require.Error(t, err)
}
