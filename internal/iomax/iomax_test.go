package iomax

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func u64(v uint64) *uint64 { return new(v) }

func TestRule_Format_AllFourKeysAlwaysPresent(t *testing.T) {
	// K2: a partial write keeps the other keys at their previous kernel
	// value, so Format must never omit a key even when only one is set.
	r := Rule{ReadBPS: u64(1048576)}
	assert.Equal(t, "8:0 rbps=1048576 wbps=max riops=max wiops=max", r.Format("8:0"))
}

func TestRule_Format_AllNilEqualsResetLine(t *testing.T) {
	// K3: writing all-max removes the device's line. Reset must write
	// exactly this line, so the two must be identical by construction.
	assert.Equal(t, "8:0 rbps=max wbps=max riops=max wiops=max", Rule{}.Format("8:0"))
}

func TestParse_RoundTripsFormat(t *testing.T) {
	content := "8:0 rbps=1048576 wbps=max riops=200 wiops=max\n7:1 rbps=max wbps=max riops=max wiops=max\n"
	rules := Parse(content)
	require.Len(t, rules, 2)

	got := rules["8:0"]
	require.NotNil(t, got.ReadBPS)
	assert.Equal(t, uint64(1048576), *got.ReadBPS)
	assert.Nil(t, got.WriteBPS)
	require.NotNil(t, got.ReadIOPS)
	assert.Equal(t, uint64(200), *got.ReadIOPS)
	assert.Nil(t, got.WriteIOPS)

	other := rules["7:1"]
	assert.Nil(t, other.ReadBPS)
	assert.Nil(t, other.WriteBPS)
	assert.Nil(t, other.ReadIOPS)
	assert.Nil(t, other.WriteIOPS)
}

func TestParse_IgnoresBlankAndMalformedLines(t *testing.T) {
	rules := Parse("\n   \n8:0 rbps=100\nnotadevice\n")
	require.Contains(t, rules, "8:0")
	assert.Equal(t, uint64(100), *rules["8:0"].ReadBPS)
}

func TestMerge_PerFieldMinimum(t *testing.T) {
	// D7: overlapping IOLimiters combine to the most restrictive value per
	// field, independent of argument order, and a limiter can only
	// tighten never loosen (nil/"max" never wins over a set value).
	a := Rule{ReadBPS: u64(200), WriteBPS: u64(100)}
	b := Rule{ReadBPS: u64(100), ReadIOPS: u64(50)}

	got := Merge(a, b)
	require.NotNil(t, got.ReadBPS)
	assert.Equal(t, uint64(100), *got.ReadBPS, "min(200,100)=100")
	require.NotNil(t, got.WriteBPS)
	assert.Equal(t, uint64(100), *got.WriteBPS, "unset in b keeps a's value, not max")
	require.NotNil(t, got.ReadIOPS)
	assert.Equal(t, uint64(50), *got.ReadIOPS, "unset in a keeps b's value, not max")
	assert.Nil(t, got.WriteIOPS, "unset in both stays max")

	reversed := Merge(b, a)
	assert.Equal(t, got, reversed, "merge must be order-independent")
}

func TestMerge_BothNilStaysNil(t *testing.T) {
	got := Merge(Rule{}, Rule{})
	assert.Nil(t, got.ReadBPS)
	assert.Nil(t, got.WriteBPS)
	assert.Nil(t, got.ReadIOPS)
	assert.Nil(t, got.WriteIOPS)
}

// recordingWriteCloser wraps a real file and counts Write calls, proving
// how many write(2) calls a given operation issues. A plain os.File on
// disk can't show this after the fact: only observing the calls as they
// happen can.
type recordingWriteCloser struct {
	f      *os.File
	writes int
}

func (r *recordingWriteCloser) Write(p []byte) (int, error) {
	r.writes++
	return r.f.Write(p)
}

func (r *recordingWriteCloser) Close() error { return r.f.Close() }

func TestWrite_ExactlyOneWriteSyscallPerDevice(t *testing.T) {
	// K1: io.max parses one rule per write(2) call; a second line in the
	// same write is rejected and drops the whole write. The legacy bug
	// (internal/limiter/apply.go) joined every device's rule into one
	// multi-line write. Write must never do that, and this test proves it
	// by observing the call count, not just the resulting file content.
	dir := t.TempDir()
	path := filepath.Join(dir, "io.max")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	var rec *recordingWriteCloser
	orig := openForWrite
	openForWrite = func(p string) (io.WriteCloser, error) {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		rec = &recordingWriteCloser{f: f}
		return rec, nil
	}
	t.Cleanup(func() { openForWrite = orig })

	_, err := Write(path, "8:0", Rule{ReadBPS: u64(1048576)})
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 1, rec.writes, "Write must issue exactly one write() syscall for the device")
}

func TestWrite_ReadsBackBeforeAndAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "io.max")
	require.NoError(t, os.WriteFile(path, []byte("8:0 rbps=500 wbps=max riops=max wiops=max\n"), 0o644))

	res, err := Write(path, "8:0", Rule{ReadBPS: u64(1048576)})
	require.NoError(t, err)
	assert.Equal(t, "8:0 rbps=500 wbps=max riops=max wiops=max", res.Before)
	assert.Equal(t, "8:0 rbps=1048576 wbps=max riops=max wiops=max", res.After)
}

func TestWrite_BeforeIsNoneWhenDeviceAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "io.max")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	res, err := Write(path, "8:0", Rule{WriteIOPS: u64(10)})
	require.NoError(t, err)
	assert.Equal(t, noLine, res.Before)
	assert.Equal(t, "8:0 rbps=max wbps=max riops=max wiops=10", res.After)
}

func TestReset_WritesAllMaxLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "io.max")
	require.NoError(t, os.WriteFile(path, []byte("8:0 rbps=500 wbps=max riops=max wiops=max\n"), 0o644))

	res, err := Reset(path, "8:0")
	require.NoError(t, err)
	assert.Equal(t, "8:0 rbps=500 wbps=max riops=max wiops=max", res.Before)
	assert.Equal(t, Rule{}.Format("8:0"), res.After, "reset writes exactly the all-nil Format line")
}

func TestReset_MissingFileCountsAsSuccess(t *testing.T) {
	// D8/D9: release must not fail just because the cgroup (and therefore
	// its io.max) is already gone.
	path := filepath.Join(t.TempDir(), "does-not-exist", "io.max")

	res, err := Reset(path, "8:0")
	require.NoError(t, err)
	assert.Equal(t, noLine, res.Before)
	assert.Equal(t, noLine, res.After)
}

func TestWrite_OpenErrorReturnsBeforeAndError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "io.max")

	_, err := Write(path, "8:0", Rule{})
	require.Error(t, err)
}

// enodevWriteCloser fails Write with ENODEV, as the kernel does when a
// device named in an io.max write no longer exists.
type enodevWriteCloser struct{ path string }

func (e enodevWriteCloser) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: e.path, Err: syscall.ENODEV}
}

func (e enodevWriteCloser) Close() error { return nil }

func TestReset_ENODEVCountsAsSuccess(t *testing.T) {
	// D8/D9: release must not fail when the device itself is gone (e.g. a
	// disk unplugged since the rule was written), not just when the
	// cgroup directory is gone (TestReset_MissingFileCountsAsSuccess).
	dir := t.TempDir()
	path := filepath.Join(dir, "io.max")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	orig := openForWrite
	openForWrite = func(p string) (io.WriteCloser, error) {
		return enodevWriteCloser{path: p}, nil
	}
	t.Cleanup(func() { openForWrite = orig })

	res, err := Reset(path, "8:0")
	require.NoError(t, err)
	assert.Equal(t, noLine, res.Before)
	assert.Equal(t, noLine, res.After)
}
