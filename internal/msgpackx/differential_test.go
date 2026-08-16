// SPDX-License-Identifier: Apache-2.0

package msgpackx

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The live differential test: borge encodes, borg's msgpack decodes and re-encodes,
// and the bytes must come back identical.
//
// The fixture test (fixtures_test.go) proves borge reproduces bytes borg produced.
// This proves the other direction - that borg accepts what borge writes and agrees it
// is already in canonical form - and it can cover values too large to check in.
//
// It needs the pinned borg 2 venv, so it skips when that has not been built. The
// fixture test does not, which keeps the package testable on a bare checkout.

func pythonOracle(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg differential oracle in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the live differential test")
	}
	return py
}

// pyRoundTrip sends each value's encoding through borg's msgpack and returns what
// came back, one result per input.
func pyRoundTrip(t *testing.T, encoded [][]byte) [][]byte {
	t.Helper()
	py := pythonOracle(t)

	var in bytes.Buffer
	for _, b := range encoded {
		in.WriteString(hex.EncodeToString(b))
		in.WriteByte('\n')
	}

	cmd := exec.Command(py, "testdata/roundtrip.py")
	cmd.Stdin = &in
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("borg msgpack oracle failed: %v\nstderr:\n%s", err, stderr.String())
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(encoded) {
		t.Fatalf("oracle returned %d line(s) for %d input(s)\nstderr:\n%s",
			len(lines), len(encoded), stderr.String())
	}
	results := make([][]byte, len(lines))
	for i, line := range lines {
		if strings.HasPrefix(line, "ERROR ") {
			t.Errorf("case %d: borg's msgpack rejected borge's bytes (%x): %s",
				i, encoded[i], strings.TrimPrefix(line, "ERROR "))
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("case %d: oracle emitted bad hex %q", i, line)
		}
		results[i] = b
	}
	return results
}

func TestLiveDifferentialAgainstBorg(t *testing.T) {
	type tc struct {
		name  string
		value any
	}
	cases := []tc{
		{"nil", nil},
		{"true", true},
		{"false", false},
		{"float", 3.141592653589793},
		{"chunk_id", bytes.Repeat([]byte{0xab}, 32)},

		// Sizes past what the checked-in fixtures cover.
		{"str_65535", strings.Repeat("a", 65535)},
		{"str_65536", strings.Repeat("a", 65536)},
		{"str_100k", strings.Repeat("borge", 20000)},
		{"bin_65535", make([]byte, 65535)},
		{"bin_65536", make([]byte, 65536)},
		{"array_65535", make([]any, 65535)},
		{"array_65536", make([]any, 65536)},

		// A realistic chunk list: the largest thing in a real item.
		{"chunk_list_10k", func() any {
			out := make([]any, 10000)
			for i := range out {
				id := make([]byte, 32)
				id[0], id[1] = byte(i), byte(i>>8)
				out[i] = []any{id, int64(65536)}
			}
			return out
		}()},
	}

	// Integer boundaries, again live: this is where an off-by-one is invisible to a
	// test that only round-trips through borge itself.
	for _, n := range []int64{
		0, 1, 127, 128, 255, 256, 65535, 65536, 4294967295, 4294967296,
		-1, -32, -33, -128, -129, -32768, -32769, -2147483648, -2147483649,
		-9223372036854775808,
	} {
		cases = append(cases, tc{name: "int", value: n})
	}
	cases = append(cases, tc{"uint64_max", uint64(18446744073709551615)})

	// Timestamps across all three encodings.
	for _, ns := range []int64{0, 1, 1_000_000_000, 1755000000123456789, -1, -1000000005} {
		cases = append(cases, tc{"timestamp", TimestampFromUnixNano(ns)})
	}

	// A stable map whose key order borge must decide, not copy.
	cases = append(cases, tc{"stable_map", NewStableMap(
		MapEntry{Key: "zebra", Value: int64(1)},
		MapEntry{Key: "alpha", Value: []byte{1, 2, 3}},
		MapEntry{Key: "mike", Value: "text"},
		MapEntry{Key: "\xff\xfe", Value: nil},
	)})

	encoded := make([][]byte, len(cases))
	for i, c := range cases {
		b, err := Marshal(c.value)
		if err != nil {
			t.Fatalf("%s: Marshal: %v", c.name, err)
		}
		encoded[i] = b
	}

	results := pyRoundTrip(t, encoded)
	for i, c := range cases {
		if results[i] == nil {
			continue // already reported as a rejection
		}
		if !bytes.Equal(results[i], encoded[i]) {
			t.Errorf("%s: borg re-encoded borge's bytes differently\n  borge: %s\n  borg:  %s",
				c.name, preview(encoded[i]), preview(results[i]))
		}
	}
	t.Logf("%d values round-tripped through borg's msgpack unchanged", len(cases))
}

// TestLiveDifferentialFixturesAreCurrent catches a fixture file that has drifted from
// the interpreter, which would otherwise make the fixture test pass against a stale
// oracle.
func TestLiveDifferentialFixturesAreCurrent(t *testing.T) {
	fixtures := loadFixtures(t)
	encoded := make([][]byte, len(fixtures))
	for i, f := range fixtures {
		encoded[i] = f.bytes
	}
	results := pyRoundTrip(t, encoded)
	for i, f := range fixtures {
		if results[i] != nil && !bytes.Equal(results[i], f.bytes) {
			t.Errorf("fixture %q is not canonical for this interpreter; regenerate with 'make msgpack-fixtures'\n  stored: %s\n  borg:   %s",
				f.name, preview(f.bytes), preview(results[i]))
		}
	}
}

// preview keeps failure messages readable when a case is 100 kB of hex.
func preview(b []byte) string {
	const limit = 48
	if len(b) <= limit {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x... (%d bytes)", b[:limit], len(b))
}
