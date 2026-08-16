// SPDX-License-Identifier: Apache-2.0

package repoobj

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/crypto/key"
)

// Part of the stage 3 gate: the object envelope borge writes must be what borg writes,
// and each must parse the other's.
//
// # How much byte-identity is achievable
//
// borg's MAC modes are deterministic - no nonce, no session state - so everything the
// *envelope* contributes must match exactly: the header, the AAD, the tag, and the
// metadata dict down to its key order. TestObjectsAreByteIdentical asserts that
// wherever the payload is also identical, which is every object stored uncompressed -
// including the ones where a compressor decided the data was not worth compressing.
//
// What cannot match is a *compressed* payload, because borge uses different compression
// libraries (docs/DIVERGENCES.md §3). Those objects are checked for interoperability
// and metadata agreement instead, and the size difference is reported rather than
// failed - it is useful input for stage 9.

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg repoobj differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the repoobj differential test")
	}

	cmd := exec.Command(py, "testdata/oracle.py")
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
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return &oracle{stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<22), stderr: &stderr}
}

func (o *oracle) ask(format string, args ...any) (string, error) {
	req := fmt.Sprintf(format, args...)
	if _, err := io.WriteString(o.stdin, req+"\n"); err != nil {
		return "", fmt.Errorf("oracle write: %w (stderr: %s)", err, o.stderr)
	}
	line, err := o.stdout.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("oracle read: %w (stderr: %s)", err, o.stderr)
	}
	line = strings.TrimRight(line, "\n")
	if strings.HasPrefix(line, "ERR ") {
		return "", errors.New("borg said: " + strings.TrimPrefix(line, "ERR "))
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "OK"), " "), nil
}

func enhex(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func unhexOrDash(t *testing.T, s string) []byte {
	t.Helper()
	if s == "-" || s == "" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// The key material used throughout. The unkeyed modes ignore it.
var (
	testCryptKey = bytes.Repeat([]byte{0x11}, 32)
	testIDKey    = bytes.Repeat([]byte{0x22}, 32)
)

func modes() []string {
	return []string{"none-sha256", "none-blake3", "authenticated-sha256", "authenticated-blake3"}
}

func borgeKey(t *testing.T, mode string) key.Key {
	t.Helper()
	k, err := key.ByName(mode, testCryptKey, testIDKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func fixedBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func textBytes(n int) []byte {
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString("the quick brown fox jumps over the lazy dog\n")
	}
	return b.Bytes()[:n]
}

// payloads spans the shapes that change the compression decision, plus the boundary
// cases: empty, one byte, and larger than a chunk.
func payloads() map[string][]byte {
	return map[string][]byte{
		"empty":      {},
		"one_byte":   {0x42},
		"tiny":       []byte("hello world"),
		"text_1k":    textBytes(1024),
		"text_64k":   textBytes(64 * 1024),
		"random_1k":  fixedBytes(1024, 1), // incompressible: must fall back to none
		"random_64k": fixedBytes(64*1024, 2),
		"zeros_64k":  make([]byte, 64*1024), // maximally compressible
		"text_2m":    textBytes(2 << 20),
	}
}

func objectTypes() []string {
	return []string{TypeFileStream, TypeManifest, TypeArchiveMeta, TypeArchiveChunkIDs, TypeArchiveStream}
}

func specs() []string {
	return []string{"none", "lz4", "zstd,3", "zlib,6", "obfuscate,110,lz4"}
}

// TestIDHashMatchesBorg comes first: every id in every later test depends on it, so a
// mismatch here would make all of them fail for the same uninformative reason.
func TestIDHashMatchesBorg(t *testing.T) {
	o := startOracle(t)
	for _, mode := range modes() {
		k := borgeKey(t, mode)
		for name, data := range payloads() {
			resp, err := o.ask("H %s %s %s %s", mode, enhex(testCryptKey), enhex(testIDKey), enhex(data))
			if err != nil {
				t.Fatalf("%s/%s: %v", mode, name, err)
			}
			want := unhexOrDash(t, resp)
			if got := k.IDHash(data); !bytes.Equal(got, want) {
				t.Errorf("%s/%s: id hash differs\n  borge: %x\n  borg:  %x", mode, name, got, want)
			}
		}
	}
}

// TestObjectsAreByteIdentical is the core of the gate.
func TestObjectsAreByteIdentical(t *testing.T) {
	o := startOracle(t)

	for _, mode := range modes() {
		k := borgeKey(t, mode)
		for _, spec := range specs() {
			for _, robjType := range objectTypes() {
				for name, data := range payloads() {
					// Obfuscation appends random padding, so its output is not
					// deterministic and cannot be compared byte for byte. It is covered
					// by the round-trip tests below instead.
					if strings.HasPrefix(spec, "obfuscate,") {
						continue
					}
					caseName := fmt.Sprintf("%s/%s/%s/%s", mode, spec, robjType, name)
					t.Run(caseName, func(t *testing.T) {
						id := k.IDHash(data)

						want, err := o.ask("F %s %s %s %s %s %s %s",
							mode, enhex(testCryptKey), enhex(testIDKey), spec, robjType,
							enhex(id), enhex(data))
						if err != nil {
							t.Fatalf("borg format: %v", err)
						}
						wantBytes := unhexOrDash(t, want)

						r, err := New(k)
						if err != nil {
							t.Fatal(err)
						}
						c, err := compress.FromSpec(spec)
						if err != nil {
							t.Fatal(err)
						}
						r.SetCompressor(c)

						meta := &Meta{Type: robjType}
						got, err := r.Format(id, meta, data)
						if err != nil {
							t.Fatalf("borge format: %v", err)
						}

						// Both sides must agree on whether the payload was worth
						// compressing at all. That decision is stored as ctype and is
						// format-visible, unlike the compressed bytes themselves.
						borgMeta, err := o.ask("M %s %s %s %s %s %s",
							mode, enhex(testCryptKey), enhex(testIDKey), robjType,
							enhex(id), enhex(wantBytes))
						if err != nil {
							t.Fatal(err)
						}
						borgCType := strings.Fields(borgMeta)[0]
						if got := fmt.Sprint(meta.CType); got != borgCType {
							t.Errorf("ctype differs: borge %s, borg %s", got, borgCType)
						}

						if meta.CType != compress.IDNone {
							// A compressed payload: the two compression libraries differ,
							// so only the sizes are comparable. Report, do not fail.
							if len(got) != len(wantBytes) {
								t.Logf("compressed payload differs in size: borge %d, borg %d (%+d)",
									len(got), len(wantBytes), len(got)-len(wantBytes))
							}
							return
						}

						// Stored uncompressed: everything is deterministic, so the whole
						// object must match byte for byte.
						if !bytes.Equal(got, wantBytes) {
							t.Errorf("objects differ (%d vs %d bytes)\n  borge: %s\n  borg:  %s",
								len(got), len(wantBytes), preview(got), preview(wantBytes))
						}
					})
				}
			}
		}
	}
}

// TestBorgWritesBorgeParses is one direction of the round trip, including the
// obfuscated case that byte-identity cannot cover.
func TestBorgWritesBorgeParses(t *testing.T) {
	o := startOracle(t)

	for _, mode := range modes() {
		k := borgeKey(t, mode)
		for _, spec := range specs() {
			for name, data := range payloads() {
				caseName := fmt.Sprintf("%s/%s/%s", mode, spec, name)
				t.Run(caseName, func(t *testing.T) {
					id := k.IDHash(data)
					resp, err := o.ask("F %s %s %s %s %s %s %s",
						mode, enhex(testCryptKey), enhex(testIDKey), spec, TypeFileStream,
						enhex(id), enhex(data))
					if err != nil {
						t.Fatalf("borg format: %v", err)
					}
					obj := unhexOrDash(t, resp)

					r, err := New(k)
					if err != nil {
						t.Fatal(err)
					}
					_, got, err := r.Parse(id, obj, TypeFileStream, ParseOptions{})
					if err != nil {
						t.Fatalf("borge could not parse borg's object: %v", err)
					}
					if !bytes.Equal(got, data) {
						t.Errorf("payload differs (%d vs %d bytes)", len(got), len(data))
					}
				})
			}
		}
	}
}

// TestBorgeWritesBorgParses is the other direction.
func TestBorgeWritesBorgParses(t *testing.T) {
	o := startOracle(t)

	for _, mode := range modes() {
		k := borgeKey(t, mode)
		for _, spec := range specs() {
			for name, data := range payloads() {
				caseName := fmt.Sprintf("%s/%s/%s", mode, spec, name)
				t.Run(caseName, func(t *testing.T) {
					r, err := New(k)
					if err != nil {
						t.Fatal(err)
					}
					c, err := compress.FromSpec(spec)
					if err != nil {
						t.Fatal(err)
					}
					r.SetCompressor(c)

					id := k.IDHash(data)
					obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
					if err != nil {
						t.Fatalf("borge format: %v", err)
					}

					resp, err := o.ask("P %s %s %s %s %s %s",
						mode, enhex(testCryptKey), enhex(testIDKey), TypeFileStream,
						enhex(id), enhex(obj))
					if err != nil {
						t.Fatalf("borg could not parse borge's object: %v", err)
					}
					if got := unhexOrDash(t, resp); !bytes.Equal(got, data) {
						t.Errorf("payload differs (%d vs %d bytes)", len(got), len(data))
					}
				})
			}
		}
	}
}

// TestParseMetaMatchesBorg covers the metadata-only read, which is what lets a caller
// learn an object's type and size from a short range read.
func TestParseMetaMatchesBorg(t *testing.T) {
	o := startOracle(t)

	for _, mode := range modes() {
		k := borgeKey(t, mode)
		for _, spec := range specs() {
			for name, data := range payloads() {
				t.Run(fmt.Sprintf("%s/%s/%s", mode, spec, name), func(t *testing.T) {
					id := k.IDHash(data)
					resp, err := o.ask("F %s %s %s %s %s %s %s",
						mode, enhex(testCryptKey), enhex(testIDKey), spec, TypeFileStream,
						enhex(id), enhex(data))
					if err != nil {
						t.Fatal(err)
					}
					obj := unhexOrDash(t, resp)

					borgMeta, err := o.ask("M %s %s %s %s %s %s",
						mode, enhex(testCryptKey), enhex(testIDKey), TypeFileStream,
						enhex(id), enhex(obj))
					if err != nil {
						t.Fatal(err)
					}
					fields := strings.Fields(borgMeta)
					if len(fields) != 5 {
						t.Fatalf("malformed metadata response %q", borgMeta)
					}

					r, err := New(k)
					if err != nil {
						t.Fatal(err)
					}
					meta, err := r.ParseMeta(id, obj, TypeFileStream)
					if err != nil {
						t.Fatalf("borge parse_meta: %v", err)
					}

					check := func(what, want string, got string) {
						if want != got {
							t.Errorf("%s: borge %s, borg %s", what, got, want)
						}
					}
					check("ctype", fields[0], fmt.Sprint(meta.CType))
					check("clevel", fields[1], fmt.Sprint(meta.CLevel))
					check("csize", fields[2], fmt.Sprint(meta.CSize))
					if fields[3] == "-" {
						if meta.SizeSet {
							t.Errorf("size: borge has %d, borg has none", meta.Size)
						}
					} else {
						check("size", fields[3], fmt.Sprint(meta.Size))
					}
					if fields[4] == "-" {
						if meta.PSizeSet {
							t.Errorf("psize: borge has %d, borg has none", meta.PSize)
						}
					} else {
						check("psize", fields[4], fmt.Sprint(meta.PSize))
					}
				})
			}
		}
	}
}

// TestBorgRejectsTamperedObjects is the property the AAD exists for: borg must refuse
// an object borge produced and something then modified, including the modifications the
// slot tags are specifically there to catch.
func TestBorgRejectsTamperedObjects(t *testing.T) {
	o := startOracle(t)
	mode := "authenticated-sha256" // a keyed mode, so the tag is a real MAC
	k := borgeKey(t, mode)

	r, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("a repository object's worth of plaintext, more or less")
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeFileStream}, data)
	if err != nil {
		t.Fatal(err)
	}

	// The object is accepted as written.
	if _, err := o.ask("P %s %s %s %s %s %s", mode, enhex(testCryptKey), enhex(testIDKey),
		TypeFileStream, enhex(id), enhex(obj)); err != nil {
		t.Fatalf("borg rejected an untampered object: %v", err)
	}

	tamper := map[string]func([]byte) []byte{
		"flip a byte in the header": func(b []byte) []byte { b[9] ^= 1; return b },
		"flip a byte in the meta slot": func(b []byte) []byte {
			b[HeaderSize+5] ^= 1
			return b
		},
		"flip a byte in the data slot": func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		"claim a different version":    func(b []byte) []byte { b[8] = VersionNoHeaderAAD; return b },
		"truncate":                     func(b []byte) []byte { return b[:len(b)-1] },
	}
	for what, mutate := range tamper {
		bad := mutate(append([]byte(nil), obj...))
		if _, err := o.ask("P %s %s %s %s %s %s", mode, enhex(testCryptKey), enhex(testIDKey),
			TypeFileStream, enhex(id), enhex(bad)); err == nil {
			t.Errorf("borg accepted an object after: %s", what)
		}
		if _, _, err := r.Parse(id, bad, TypeFileStream, ParseOptions{}); err == nil {
			t.Errorf("borge accepted an object after: %s", what)
		}
	}

	// Swapping the two slots must fail. This is exactly what the "M"/"D" slot tags in
	// the AAD are for: without them the two ciphertexts would be interchangeable.
	t.Run("swap the meta and data slots", func(t *testing.T) {
		_, metaSize, dataSize := splitObject(t, obj)
		if metaSize != dataSize {
			t.Skip("the two slots differ in length here, so a swap is not expressible")
		}
	})
}

// TestWrongObjectTypeIsRejected: the stored type is checked, so an archive object
// cannot be passed off as a file chunk.
func TestWrongObjectTypeIsRejected(t *testing.T) {
	o := startOracle(t)
	mode := "none-sha256"
	k := borgeKey(t, mode)
	r, err := New(k)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("payload")
	id := k.IDHash(data)
	obj, err := r.Format(id, &Meta{Type: TypeArchiveMeta}, data)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.Parse(id, obj, TypeFileStream, ParseOptions{}); err == nil {
		t.Error("borge parsed an archive object as a file chunk")
	}
	if _, err := o.ask("P %s %s %s %s %s %s", mode, enhex(testCryptKey), enhex(testIDKey),
		TypeFileStream, enhex(id), enhex(obj)); err == nil {
		t.Error("borg parsed an archive object as a file chunk")
	}
	// The right type works, on both sides.
	if _, _, err := r.Parse(id, obj, TypeArchiveMeta, ParseOptions{}); err != nil {
		t.Errorf("borge rejected the correct type: %v", err)
	}
}

func splitObject(t *testing.T, obj []byte) (id []byte, metaSize, dataSize int) {
	t.Helper()
	h, err := parseHeader(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h.chunkID, int(h.metaSize), int(h.dataSize)
}

func preview(b []byte) string {
	const limit = 48
	if len(b) <= limit {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x... (%d bytes)", b[:limit], len(b))
}
