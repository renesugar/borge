// SPDX-License-Identifier: Apache-2.0

package item

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Path sanitisation is the one part of this package that is a security boundary rather
// than a format question, so it is tested twice: against a table of cases whose
// intent is stated, and against borg itself over a much wider set of inputs.
//
// Both directions of a disagreement are bugs. Accepting a path borg rejects is a
// path-traversal vulnerability. Rejecting one borg accepts makes borge unable to read
// valid archives.

func TestMakePathSafe(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		reject bool
	}{
		// Already safe: unchanged.
		{in: "a", want: "a"},
		{in: "home/renes/notes.md", want: "home/renes/notes.md"},

		// Made relative.
		{in: "/etc/passwd", want: "etc/passwd"},
		{in: "///etc/passwd", want: "etc/passwd"},

		// Normalised: repeated slashes, "." elements, trailing slash.
		{in: "a//b", want: "a/b"},
		{in: "a/./b", want: "a/b"},
		{in: "a/b/", want: "a/b"},
		{in: "a/b//", want: "a/b"},
		{in: "./a", want: "a"},
		{in: ".", want: "."},
		{in: "", want: "."},
		{in: "/", want: "."},

		// Traversal: every spelling must be refused.
		{in: "..", reject: true},
		{in: "../etc", reject: true},
		{in: "/../etc", reject: true},
		{in: "a/../../etc", reject: true},
		{in: "a/..", reject: true},
		{in: "a/../b", reject: true},

		// Not traversal, despite appearances: these are ordinary names.
		{in: "..a", want: "..a"},
		{in: "a..", want: "a.."},
		{in: "a/..b", want: "a/..b"},
		{in: "...", want: "..."},

		// Backslash forms. borg refuses these even on POSIX, where a backslash is a
		// legal filename character, so borge does too - a path borg will not store
		// must not become storable just because borge wrote it.
		{in: `a\..`, reject: true},
		{in: `..\a`, reject: true},
		// A lone backslash is not a traversal and stays a normal character on POSIX.
		{in: `a\b`, want: `a\b`},

		// Arbitrary bytes are ordinary path characters.
		{in: "caf\xe9.txt", want: "caf\xe9.txt"},
		{in: "/\xff\xfe/x", want: "\xff\xfe/x"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, err := MakePathSafe(tc.in)
			if tc.reject {
				if err == nil {
					t.Errorf("accepted %q, giving %q; it must be rejected", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("MakePathSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAssertSanitizedPath(t *testing.T) {
	// The encode-side check accepts only paths that are already in normal form.
	for _, ok := range []string{"a", "a/b", ".", "..a", "caf\xe9"} {
		if _, err := AssertSanitizedPath(ok); err != nil {
			t.Errorf("AssertSanitizedPath(%q) failed: %v", ok, err)
		}
	}
	for _, bad := range []string{"/a", "a//b", "a/./b", "a/", "..", "../a", ""} {
		if _, err := AssertSanitizedPath(bad); err == nil {
			t.Errorf("AssertSanitizedPath(%q) accepted an unsanitised path", bad)
		}
	}
}

// TestItemRejectsUnsafePathOnEncode: the check has to be on the write path too, or
// borge could produce an archive borg would refuse to extract.
func TestItemRejectsUnsafePathOnEncode(t *testing.T) {
	for _, bad := range []string{"../escape", "/absolute", "a/../b"} {
		it := &Item{Path: bad, MTime: OptInt(0)}
		if _, err := it.Marshal(); err == nil {
			t.Errorf("encoding an item with path %q succeeded; it must be refused", bad)
		} else {
			var unsafe *ErrUnsafePath
			if !errors.As(err, &unsafe) {
				t.Errorf("path %q: got %v, want an ErrUnsafePath", bad, err)
			}
		}
	}
}

// TestItemSanitisesPathOnDecode: an archive is attacker-controlled input on the read
// side, so a hostile path must be neutralised or refused, never passed through.
func TestItemSanitisesPathOnDecode(t *testing.T) {
	build := func(path string) []byte {
		m := newRawMap()
		m.Set("path", path)
		m.Set("mtime", timestampFor(0))
		b, err := marshalRaw(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("absolute path is made relative", func(t *testing.T) {
		it, err := UnmarshalItem(build("/etc/passwd"))
		if err != nil {
			t.Fatal(err)
		}
		if it.Path != "etc/passwd" {
			t.Errorf("decoded path = %q, want %q", it.Path, "etc/passwd")
		}
	})

	t.Run("traversal is refused", func(t *testing.T) {
		if _, err := UnmarshalItem(build("../../etc/passwd")); err == nil {
			t.Error("decoded an item whose path escapes the extraction directory")
		}
	})
}

// ------------------------------------------------------- differential against borg

func TestPathSanitisationMatchesBorg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the borg path differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the path differential test")
	}

	// A wide set of shapes, including the ones that are easy to get subtly wrong:
	// interior "..", names that merely start with dots, repeated and trailing slashes,
	// backslashes, and non-UTF-8 bytes.
	var inputs []string
	base := []string{
		"", ".", "..", "...", "....", "/", "//", "///",
		"a", "a/b", "a/b/c", "/a", "//a", "/a/", "a/", "a//", "a//b", "a/./b", "./a", "a/.",
		"../a", "a/../b", "a/..", "/../a", "a/b/../c", "..a", "a..", "a/..b", "a/b..",
		".hidden", "a/.hidden", "a/./.", "././.", "/./", "/.",
		`a\b`, `a\..`, `..\a`, `a\.\b`, `\a`, `a\`, `\`, `\\`,
		"caf\xe9", "/caf\xe9/", "\xff", "a/\xff/b", "\x00", "a\x00b",
		" ", " /a", "a/ ", "a b/c d",
		strings.Repeat("a/", 50) + "b",
		strings.Repeat("../", 3) + "etc",
		"a/" + strings.Repeat(".", 10) + "/b",
	}
	inputs = append(inputs, base...)
	// Combinations of a prefix and a suffix, to reach shapes the flat list misses.
	for _, p := range []string{"", "/", "a/", "../", "./", "//"} {
		for _, s := range []string{"", "..", "b", "b/", "/..", ".", "\xff"} {
			inputs = append(inputs, p+s)
		}
	}

	cmd := exec.Command(py, "testdata/path_oracle.py")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	r := bufio.NewReader(stdout)

	agreed, rejected := 0, 0
	for _, in := range inputs {
		encoded := hex.EncodeToString([]byte(in))
		if encoded == "" {
			encoded = "-"
		}
		if _, err := io.WriteString(stdin, encoded+"\n"); err != nil {
			t.Fatalf("oracle write: %v (stderr: %s)", err, stderr.String())
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("oracle read: %v (stderr: %s)", err, stderr.String())
		}
		line = strings.TrimRight(line, "\n")

		got, gotErr := MakePathSafe(in)

		switch {
		case line == "REJECT":
			rejected++
			if gotErr == nil {
				t.Errorf("borg rejects %q but borge accepted it as %q "+
					"- this is a path-traversal hole", in, got)
			}
		case strings.HasPrefix(line, "OK "):
			payload := strings.TrimPrefix(line, "OK ")
			var want string
			if payload != "-" {
				b, err := hex.DecodeString(payload)
				if err != nil {
					t.Fatalf("oracle emitted bad hex %q for input %q", payload, in)
				}
				want = string(b)
			}
			if gotErr != nil {
				t.Errorf("borg accepts %q as %q but borge rejected it: %v", in, want, gotErr)
				continue
			}
			if got != want {
				t.Errorf("MakePathSafe(%q):\n  borge: %q\n  borg:  %q", in, got, want)
				continue
			}
			agreed++
		default:
			t.Fatalf("oracle error for %q: %s", in, line)
		}
	}
	t.Logf("%d inputs: %d sanitised identically, %d rejected by both", len(inputs), agreed, rejected)
}

func FuzzMakePathSafeIsIdempotent(f *testing.F) {
	for _, s := range []string{"a/b", "/a", "../a", "a/./b", "caf\xe9", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		safe, err := MakePathSafe(path)
		if err != nil {
			return // rejected, nothing more to check
		}
		// The result must be a fixed point, or AssertSanitizedPath would refuse paths
		// that MakePathSafe itself produced.
		again, err := MakePathSafe(safe)
		if err != nil {
			t.Fatalf("MakePathSafe(%q) = %q, which is then rejected: %v", path, safe, err)
		}
		if again != safe {
			t.Errorf("not idempotent: %q -> %q -> %q", path, safe, again)
		}
		if _, err := AssertSanitizedPath(safe); err != nil {
			t.Errorf("AssertSanitizedPath rejects the output of MakePathSafe(%q) = %q: %v",
				path, safe, err)
		}

		// The safety properties themselves.
		if strings.HasPrefix(safe, "/") {
			t.Errorf("MakePathSafe(%q) = %q is absolute", path, safe)
		}
		for _, part := range strings.Split(safe, "/") {
			if part == ".." {
				t.Errorf("MakePathSafe(%q) = %q contains a '..' element", path, safe)
			}
		}
	})
}
