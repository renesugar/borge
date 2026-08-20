// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/msgpackx"
)

// borge debug convert-profile, compared against borg's.
//
// The comparison is of the *loaded objects*, not of the two files' bytes, and that is a
// deliberate weakening with a reason: CPython's marshal writer emits back-references for
// any object whose refcount is above one, so borg's output records which strings and small
// integers the interpreter happened to be sharing at the time. borge writes the plain
// forms. Both load to the same profile; only one of them could ever be reproduced without
// emulating CPython's refcounting. See pymarshal.go.
//
// So the assertion is the one a profile reader actually makes: marshal.load of borge's file
// == marshal.load of borg's, and pstats can open it.

// python is the interpreter borg runs under, which is the only one guaranteed to be here
// and the one whose marshal reader has to accept the output.
func (r *borgRepo) python(t *testing.T) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(r.binary), "python")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("no python next to the borg binary: %v", err)
	}
	return p
}

// runPython runs a script with the given arguments and fails the test on a non-zero exit,
// which is how the script reports a mismatch.
func runPython(t *testing.T, python, script string, args ...string) string {
	t.Helper()
	out, err := exec.Command(python, append([]string{"-c", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("python check failed: %v\n%s", err, out)
	}
	return string(out)
}

// compareProfiles asserts both files load to equal objects, and that the object is a real
// profile rather than an empty dict two converters would agree about trivially.
const compareProfiles = `
import marshal, pstats, sys
want, got = sys.argv[1], sys.argv[2]
a = marshal.load(open(want, "rb"))
b = marshal.load(open(got, "rb"))
assert isinstance(a, dict), "borg's own conversion is not a dict: %r" % type(a)
assert len(a) >= 5, "borg's profile has only %d entries; the comparison would be vacuous" % len(a)
keys = [k for k in a if isinstance(k, tuple) and len(k) == 3]
assert len(keys) == len(a), "borg's profile keys are not (file, line, func) triples"
vals = [v for v in a.values() if isinstance(v, tuple) and len(v) == 5]
assert len(vals) == len(a), "borg's profile values are not cProfile 5-tuples"
assert a == b, "the two conversions differ:\n  borg:  %r\n  borge: %r" % (
    sorted(a.items())[:2], sorted(b.items())[:2])
st = pstats.Stats(got)
assert st.total_calls > 0, "pstats read no calls out of borge's file"
print("entries=%d total_calls=%d" % (len(b), st.total_calls))
`

func TestConvertProfileMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	python := r.python(t)
	dir := t.TempDir()
	profile := filepath.Join(dir, "borg.prof")

	// A real profile, written by borg the only way borg writes one. Anything smaller
	// would be testing a fixture rather than the format.
	cmd := exec.Command(r.binary, "repo-list", "-r", r.path)
	cmd.Env = append(r.env(), "BORG_DEBUG_PROFILE="+profile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("borg repo-list under BORG_DEBUG_PROFILE: %v\n%s", err, out)
	}
	if st, err := os.Stat(profile); err != nil || st.Size() == 0 {
		t.Fatalf("borg wrote no profile to %s (%v)", profile, err)
	}

	borgOut := filepath.Join(dir, "borg.pyprof")
	borgeOut := filepath.Join(dir, "borge.pyprof")
	r.mustRun("debug", "convert-profile", profile, borgOut)
	if _, stderr, code := r.borge(t, "debug", "convert-profile", profile, borgeOut); code != ExitOK {
		t.Fatalf("borge debug convert-profile exited %d\n%s", code, stderr)
	}

	t.Log(strings.TrimSpace(runPython(t, python, compareProfiles, borgOut, borgeOut)))
}

// TestConvertProfileTypesMatchBorg covers what a real profile does not contain.
//
// A profile holds strings, ints, floats, tuples and dicts, so the conversion above never
// reaches the arbitrary-precision integers, the bytes, or the non-UTF-8 string that msgpack
// can carry - and those are exactly where a hand-written marshal encoder goes wrong. The
// input is built with borge's own msgpack encoder and converted by *both* tools, so the
// expected answer still comes from borg rather than from what I believed Python would do.
func TestConvertProfileTypesMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	python := r.python(t)
	dir := t.TempDir()

	// The non-UTF-8 str is the interesting one. Python's unpacker decodes it with
	// surrogateescape and marshal re-encodes it with surrogatepass, so the byte 0xff
	// becomes the three-byte encoding of U+DCFF - which is not a legal UTF-8 sequence and
	// which Go's utf8.EncodeRune refuses to produce.
	value := msgpackx.NewMap(
		msgpackx.MapEntry{Key: []any{"file.py", int64(12), "func"},
			Value: []any{int64(1), int64(2), 0.5, -2.25, msgpackx.NewMap()}},
		msgpackx.MapEntry{Key: "int32 edges",
			Value: []any{int64(-2147483648), int64(2147483647), int64(-2147483649), int64(2147483648)}},
		msgpackx.MapEntry{Key: "int64 edges",
			Value: []any{int64(-9223372036854775808), int64(9223372036854775807), uint64(18446744073709551615)}},
		msgpackx.MapEntry{Key: "floats", Value: []any{0.0, 1e300, -1e-300}},
		msgpackx.MapEntry{Key: "bytes", Value: []byte{0, 1, 0xff, 0xfe}},
		msgpackx.MapEntry{Key: "b\xffad", Value: "also \xff not utf-8"},
		msgpackx.MapEntry{Key: "empties", Value: []any{[]any{}, msgpackx.NewMap(), "", []byte{}}},
		msgpackx.MapEntry{Key: int64(7), Value: nil},
		msgpackx.MapEntry{Key: []byte("bytes key"), Value: []any{true, false, nil}},
		msgpackx.MapEntry{Key: "nested",
			Value: msgpackx.NewMap(msgpackx.MapEntry{Key: "deep", Value: []any{msgpackx.NewMap(
				msgpackx.MapEntry{Key: "deeper", Value: []any{int64(1)}})}})},
	)
	packed, err := msgpackx.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "types.prof")
	if err := os.WriteFile(in, packed, 0o600); err != nil {
		t.Fatal(err)
	}

	borgOut := filepath.Join(dir, "borg.pyprof")
	borgeOut := filepath.Join(dir, "borge.pyprof")
	r.mustRun("debug", "convert-profile", in, borgOut)
	if _, stderr, code := r.borge(t, "debug", "convert-profile", in, borgeOut); code != ExitOK {
		t.Fatalf("borge debug convert-profile exited %d\n%s", code, stderr)
	}

	// The guards check that borg's own conversion really does contain each awkward type,
	// so that "equal" cannot be the equality of two things that lost the same field.
	const script = `
import marshal, sys
a = marshal.load(open(sys.argv[1], "rb"))
b = marshal.load(open(sys.argv[2], "rb"))
assert a["int64 edges"] == (-9223372036854775808, 9223372036854775807, 18446744073709551615), a["int64 edges"]
assert a["bytes"] == b"\x00\x01\xff\xfe", a["bytes"]
assert "b\udcffad" in a, sorted(map(repr, a))
assert a["b\udcffad"] == "also \udcff not utf-8", repr(a["b\udcffad"])
assert a[b"bytes key"] == (True, False, None), a[b"bytes key"]
assert a["empties"] == ((), {}, "", b""), a["empties"]
assert isinstance(a["nested"], dict) and a["nested"]["deep"][0]["deeper"] == (1,), a["nested"]
assert isinstance(next(iter(a["floats"])), float)
assert a == b, "\n  borg:  %r\n  borge: %r" % (a, b)
print("types ok: %d entries" % len(b))
`
	t.Log(strings.TrimSpace(runPython(t, python, script, borgOut, borgeOut)))
}

// TestConvertProfileRejectsBadInput: the two tools have to fail on the same files, or a
// script that checks the exit code learns something untrue.
func TestConvertProfileRejectsBadInput(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	dir := t.TempDir()

	notMsgpack := filepath.Join(dir, "not-a-profile")
	if err := os.WriteFile(notMsgpack, []byte("this is not msgpack at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A msgpack timestamp: valid msgpack, but Python's unpacker turns it into a Timestamp
	// object and marshal has no code for one, so borg fails too.
	ext, err := msgpackx.Marshal(msgpackx.Timestamp{Seconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	unmarshallable := filepath.Join(dir, "timestamp.prof")
	if err := os.WriteFile(unmarshallable, ext, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, in := range []string{notMsgpack, unmarshallable, filepath.Join(dir, "missing")} {
		name := filepath.Base(in)
		t.Run(name, func(t *testing.T) {
			if out, err := r.runErr("debug", "convert-profile", in, filepath.Join(dir, name+".borg")); err == nil {
				t.Fatalf("borg accepted %s:\n%s", name, out)
			}
			_, stderr, code := r.borge(t, "debug", "convert-profile", in, filepath.Join(dir, name+".borge"))
			if code == ExitOK {
				t.Fatalf("borge accepted %s", name)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatalf("borge rejected %s without saying why", name)
			}
		})
	}

	// Wrong number of arguments: borg's parser refuses, and so must borge's.
	if out, err := r.runErr("debug", "convert-profile", notMsgpack); err == nil {
		t.Fatalf("borg accepted convert-profile with one argument:\n%s", out)
	}
	if _, _, code := r.borge(t, "debug", "convert-profile", notMsgpack); code == ExitOK {
		t.Fatal("borge accepted convert-profile with one argument")
	}
}
