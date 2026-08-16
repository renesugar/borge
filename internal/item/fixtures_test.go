// SPDX-License-Identifier: Apache-2.0

package item

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The stage 1.5 gate: every item fixture round-trips byte-identically.
//
// The fixtures come from borg's own Item/ArchiveItem/ManifestItem/Key/EncryptedKey
// classes via its own msgpack wrapper (testdata/gen_fixtures.py). Decoding one through
// borge and re-encoding it must give back the same bytes, which checks both directions
// at once: borge has to understand every shape borg writes, and has to independently
// reproduce borg's key ordering and value encodings rather than copying them through.

type fixture struct {
	kind, name string
	bytes      []byte
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	f, err := os.Open("testdata/fixtures.txt")
	if err != nil {
		t.Fatalf("cannot read fixtures (regenerate with 'make item-fixtures'): %v", err)
	}
	defer f.Close()

	var out []fixture
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(text, "\t")
		if len(parts) != 3 {
			t.Fatalf("testdata/fixtures.txt:%d: expected '<kind>\\t<name>\\t<hex>'", line)
		}
		raw, err := hex.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("testdata/fixtures.txt:%d (%s/%s): bad hex: %v", line, parts[0], parts[1], err)
		}
		out = append(out, fixture{kind: parts[0], name: parts[1], bytes: raw})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("testdata/fixtures.txt contains no fixtures")
	}
	return out
}

// roundTrip decodes and re-encodes a fixture through the type its kind names.
func roundTrip(kind string, b []byte) ([]byte, error) {
	switch kind {
	case "item":
		v, err := UnmarshalItem(b)
		if err != nil {
			return nil, err
		}
		return v.Marshal()
	case "archive":
		v, err := UnmarshalArchiveItem(b)
		if err != nil {
			return nil, err
		}
		return v.Marshal()
	case "manifest":
		v, err := UnmarshalManifestItem(b)
		if err != nil {
			return nil, err
		}
		return v.Marshal()
	case "key":
		v, err := UnmarshalKey(b)
		if err != nil {
			return nil, err
		}
		return v.Marshal()
	case "enckey":
		v, err := UnmarshalEncryptedKey(b)
		if err != nil {
			return nil, err
		}
		return v.Marshal()
	default:
		return nil, errUnknownKind(kind)
	}
}

type errUnknownKind string

func (e errUnknownKind) Error() string { return "unknown fixture kind " + string(e) }

func TestFixtureRoundTrip(t *testing.T) {
	fixtures := loadFixtures(t)
	t.Logf("checking %d fixtures produced by borg's own item classes", len(fixtures))

	for _, f := range fixtures {
		t.Run(f.kind+"/"+f.name, func(t *testing.T) {
			got, err := roundTrip(f.kind, f.bytes)
			if err != nil {
				t.Fatalf("round trip failed: %v\n  borg bytes: %s", err, preview(f.bytes))
			}
			if string(got) != string(f.bytes) {
				t.Errorf("re-encode differs from borg\n  borg:  %s\n  borge: %s",
					preview(f.bytes), preview(got))
			}
		})
	}
}

// TestFixtureTruncatedIsRejected: every prefix of every fixture must be refused. A
// truncated item is what a torn write or a corrupt pack looks like, and decoding one
// into a partially-populated struct would be worse than failing.
func TestFixtureTruncatedIsRejected(t *testing.T) {
	for _, f := range loadFixtures(t) {
		if len(f.bytes) < 2 {
			continue
		}
		// Every prefix for a small fixture; a sampled stride for a large one, where
		// checking all 40k prefixes costs a minute and adds no distinct cases.
		stride := 1
		if len(f.bytes) > 4096 {
			stride = len(f.bytes) / 512
		}
		for n := 1; n < len(f.bytes); n += stride {
			if _, err := roundTrip(f.kind, f.bytes[:n]); err == nil {
				t.Errorf("%s/%s: a %d-byte prefix of %d decoded without error",
					f.kind, f.name, n, len(f.bytes))
			}
		}
	}
}

// TestFixtureCorruptedDoesNotPanic: arbitrary bit flips must produce an error, never a
// crash. borge reads bytes that may be corrupt or hostile as a matter of course.
func TestFixtureCorruptedDoesNotPanic(t *testing.T) {
	for _, f := range loadFixtures(t) {
		// The large fixtures would make this quadratic for no extra coverage.
		if len(f.bytes) > 4096 {
			continue
		}
		for i := range f.bytes {
			for _, mask := range []byte{0xff, 0x01, 0x80} {
				corrupt := append([]byte(nil), f.bytes...)
				corrupt[i] ^= mask
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("%s/%s: panic on corrupted input (byte %d ^= 0x%02x): %v",
								f.kind, f.name, i, mask, r)
						}
					}()
					_, _ = roundTrip(f.kind, corrupt)
				}()
			}
		}
	}
}

func preview(b []byte) string {
	const limit = 64
	if len(b) <= limit {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:limit]) + "..."
}
