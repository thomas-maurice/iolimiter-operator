// Package iomax parses and writes cgroup v2 io.max rules.
//
// io.max parses one "MAJ:MIN key=val ..." rule per write(2) call; a second
// line in the same write fails with EINVAL and the whole write is dropped
// (K1), and a partial rule (fewer than four keys) keeps the other keys at
// their previous kernel value (K2). Callers must therefore always supply
// all four keys via Rule (nil = "max") and issue one Write per device.
//
// This package is pure and log-free (SPEC.md §6.3): Write and Reset return
// WriteResult with the device's io.max line before and after the call so
// the agent can log without re-reading anything.
package iomax

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// noLine is what WriteResult.Before/After hold when a device has no line in
// io.max (unlimited / "max").
const noLine = "<none>"

// Rule is one device's cgroup v2 io.max limits. A nil field means "max"
// (unlimited).
type Rule struct {
	ReadBPS   *uint64
	WriteBPS  *uint64
	ReadIOPS  *uint64
	WriteIOPS *uint64
}

// Format renders rule as the full io.max line for dev ("MAJ:MIN"), always
// all four keys in rbps/wbps/riops/wiops order (K2). An all-nil Rule
// produces the reset line (K3).
func (r Rule) Format(dev string) string {
	return fmt.Sprintf("%s rbps=%s wbps=%s riops=%s wiops=%s",
		dev, formatVal(r.ReadBPS), formatVal(r.WriteBPS), formatVal(r.ReadIOPS), formatVal(r.WriteIOPS))
}

func formatVal(v *uint64) string {
	if v == nil {
		return "max"
	}
	return strconv.FormatUint(*v, 10)
}

// Parse parses io.max content, one "MAJ:MIN key=val ..." line per device,
// into a map keyed by device ("MAJ:MIN"). Unknown keys and malformed values
// are ignored rather than failing the whole parse: io.max is kernel-owned
// content, not user input to validate.
func Parse(content string) map[string]Rule {
	out := make(map[string]Rule)
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		var r Rule
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			var val *uint64
			if v != "max" {
				n, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					continue
				}
				val = &n
			}
			switch k {
			case "rbps":
				r.ReadBPS = val
			case "wbps":
				r.WriteBPS = val
			case "riops":
				r.ReadIOPS = val
			case "wiops":
				r.WriteIOPS = val
			}
		}
		out[dev] = r
	}
	return out
}

// Merge returns the per-field minimum of a and b (D7: overlapping limits
// combine to the most restrictive value per key). nil ("max"/unlimited)
// never wins over a set value: a limiter can only tighten, never loosen.
func Merge(a, b Rule) Rule {
	return Rule{
		ReadBPS:   minPtr(a.ReadBPS, b.ReadBPS),
		WriteBPS:  minPtr(a.WriteBPS, b.WriteBPS),
		ReadIOPS:  minPtr(a.ReadIOPS, b.ReadIOPS),
		WriteIOPS: minPtr(a.WriteIOPS, b.WriteIOPS),
	}
}

func minPtr(a, b *uint64) *uint64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a < *b:
		v := *a
		return &v
	default:
		v := *b
		return &v
	}
}

// WriteResult is a device's io.max line before and after a Write or Reset
// call. "<none>" means no line for the device (unlimited).
type WriteResult struct {
	Before string
	After  string
}

// openForWrite opens path for a single write(2) call. It is a variable, not
// a direct os.OpenFile call, so tests can substitute a recording writer:
// a plain file on disk can't observe how many write() calls produced its
// contents, only a wrapper around the write itself can.
var openForWrite = func(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}

// Write writes rule for dev to the io.max file at path in exactly one
// write(2) call (K1), then reads the device's line back into
// WriteResult.After. It does not compare Before/After for correctness: K6
// (silently ignored values) is the caller's concern.
func Write(path, dev string, rule Rule) (WriteResult, error) {
	return writeLine(path, dev, rule.Format(dev))
}

// Reset writes the all-"max" reset line for dev (K3), removing its io.max
// entry. A missing cgroup or device (ENOENT/ENODEV) counts as already
// reset, not an error (D8/D9: release must not fail just because the
// kernel state is already gone).
func Reset(path, dev string) (WriteResult, error) {
	res, err := writeLine(path, dev, Rule{}.Format(dev))
	if err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODEV)) {
		return WriteResult{Before: noLine, After: noLine}, nil
	}
	return res, err
}

func writeLine(path, dev, line string) (WriteResult, error) {
	before := readDeviceLine(path, dev)

	f, err := openForWrite(path)
	if err != nil {
		return WriteResult{Before: before}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	payload := []byte(line + "\n")
	n, err := f.Write(payload)
	if err != nil {
		return WriteResult{Before: before}, fmt.Errorf("writing %s: %w", path, err)
	}
	if n != len(payload) {
		return WriteResult{Before: before}, fmt.Errorf("short write to %s: wrote %d of %d bytes", path, n, len(payload))
	}

	return WriteResult{Before: before, After: readDeviceLine(path, dev)}, nil
}

// readDeviceLine returns dev's trimmed io.max line, or noLine if the file
// or the device's line is absent.
func readDeviceLine(path, dev string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return noLine
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == dev {
			return line
		}
	}
	return noLine
}
