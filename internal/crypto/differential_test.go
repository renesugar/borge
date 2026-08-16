// SPDX-License-Identifier: Apache-2.0

package crypto

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
)

// The stage 1.3 gate: every AEAD blob borg writes decrypts under borge and vice versa,
// for each ciphersuite.
//
// borg's AEAD is OpenSSL's, so this is really borge's from-scratch OCB against
// OpenSSL's. The RFC 7253 vectors in internal/crypto/ocb prove the algorithm in
// isolation; this proves the whole envelope - the header/tag/ciphertext layout, the
// AAD construction, and the aad_offset handling - against the implementation borg
// actually ships with.
//
// The plan calls OCB the highest-risk component in the port. This test is the reason
// that risk is now bounded rather than hoped about.

type oracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startOracle(t *testing.T) *oracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg crypto differential test in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the crypto differential test")
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

func (o *oracle) ask(req string) (string, error) {
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
	return strings.TrimPrefix(line, "OK "), nil
}

func enhex(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func unhexOrDash(s string) ([]byte, error) {
	if s == "-" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

func (o *oracle) borgEncrypt(suite string, headerLen, aadOffset int, key, iv, header, aad, plain []byte) ([]byte, error) {
	resp, err := o.ask(fmt.Sprintf("E %s %d %d %s %s %s %s %s",
		suite, headerLen, aadOffset, enhex(key), enhex(iv), enhex(header), enhex(aad), enhex(plain)))
	if err != nil {
		return nil, err
	}
	return unhexOrDash(resp)
}

func (o *oracle) borgDecrypt(suite string, headerLen, aadOffset int, key, iv, aad, envelope []byte) ([]byte, error) {
	resp, err := o.ask(fmt.Sprintf("D %s %d %d %s %s %s %s",
		suite, headerLen, aadOffset, enhex(key), enhex(iv), enhex(aad), enhex(envelope)))
	if err != nil {
		return nil, err
	}
	return unhexOrDash(resp)
}

// suiteCases enumerates the parameter combinations worth covering. The header and
// aad_offset variations matter because borg's real keys use a non-zero header
// (1+1+6+24 bytes for the AEAD modes, with aad_offset 0), and an implementation that
// only ever sees headerLen=0 can have the AAD construction backwards without noticing.
var suiteCases = []struct {
	suite     string
	goSuite   Suite
	headerLen int
	aadOffset int
}{
	{"aes256-ocb", SuiteAESOCB, 0, 0},
	{"aes256-ocb", SuiteAESOCB, 1, 0},
	{"aes256-ocb", SuiteAESOCB, 32, 0},
	{"aes256-ocb", SuiteAESOCB, 32, 8},
	{"aes256-ocb", SuiteAESOCB, 32, 32}, // whole header excluded from the AAD
	{"chacha20-poly1305", SuiteChaCha20Poly1305, 0, 0},
	{"chacha20-poly1305", SuiteChaCha20Poly1305, 32, 0},
	{"chacha20-poly1305", SuiteChaCha20Poly1305, 32, 8},
}

// payloadSizes cover the block boundaries where OCB's partial-block handling lives,
// plus sizes representative of real borg objects.
var payloadSizes = []int{0, 1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 128, 129, 1000, 65536, 2 << 20}

func fixedBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// TestBorgEncryptsBorgeDecrypts is direction one of the gate.
func TestBorgEncryptsBorgeDecrypts(t *testing.T) {
	o := startOracle(t)
	key := fixedBytes(KeySize, 1)
	iv := fixedBytes(IVSize, 2)

	for _, sc := range suiteCases {
		for _, n := range payloadSizes {
			name := fmt.Sprintf("%s/hdr%d_aad%d/%d", sc.suite, sc.headerLen, sc.aadOffset, n)
			t.Run(name, func(t *testing.T) {
				plain := fixedBytes(n, int64(n)+3)
				header := fixedBytes(sc.headerLen, int64(sc.headerLen)+4)
				aad := fixedBytes(n%29, int64(n)+5)

				env, err := o.borgEncrypt(sc.suite, sc.headerLen, sc.aadOffset, key, iv, header, aad, plain)
				if err != nil {
					t.Fatalf("borg encrypt: %v", err)
				}
				if want := sc.headerLen + MACSize + n; len(env) != want {
					t.Fatalf("borg produced a %d byte envelope, expected %d", len(env), want)
				}
				if !bytes.Equal(env[:sc.headerLen], header) {
					t.Error("the header is not at the front of borg's envelope")
				}

				a, err := NewAEAD(sc.goSuite, key, sc.headerLen, sc.aadOffset)
				if err != nil {
					t.Fatal(err)
				}
				got, err := a.Decrypt(iv, env, aad)
				if err != nil {
					t.Fatalf("borge could not decrypt borg's envelope: %v", err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("decrypted %d bytes, expected %d, and they differ", len(got), len(plain))
				}
			})
		}
	}
}

// TestBorgeEncryptsBorgDecrypts is direction two of the gate.
func TestBorgeEncryptsBorgDecrypts(t *testing.T) {
	o := startOracle(t)
	key := fixedBytes(KeySize, 11)
	iv := fixedBytes(IVSize, 12)

	for _, sc := range suiteCases {
		for _, n := range payloadSizes {
			name := fmt.Sprintf("%s/hdr%d_aad%d/%d", sc.suite, sc.headerLen, sc.aadOffset, n)
			t.Run(name, func(t *testing.T) {
				plain := fixedBytes(n, int64(n)+13)
				header := fixedBytes(sc.headerLen, int64(sc.headerLen)+14)
				aad := fixedBytes(n%29, int64(n)+15)

				a, err := NewAEAD(sc.goSuite, key, sc.headerLen, sc.aadOffset)
				if err != nil {
					t.Fatal(err)
				}
				env, err := a.Encrypt(iv, plain, header, aad)
				if err != nil {
					t.Fatalf("borge encrypt: %v", err)
				}

				got, err := o.borgDecrypt(sc.suite, sc.headerLen, sc.aadOffset, key, iv, aad, env)
				if err != nil {
					t.Fatalf("borg could not decrypt borge's envelope: %v", err)
				}
				if !bytes.Equal(got, plain) {
					t.Errorf("borg decrypted %d bytes, expected %d, and they differ", len(got), len(plain))
				}
			})
		}
	}
}

// TestEnvelopesAreByteIdentical asserts more than interoperability: that borge's
// envelope is the *same bytes* borg would have written.
//
// Unlike compression, this is achievable and worth having. OCB and ChaCha20-Poly1305
// are deterministic given (key, iv, plaintext, aad), so any difference at all means a
// real disagreement about the format rather than an implementation detail.
func TestEnvelopesAreByteIdentical(t *testing.T) {
	o := startOracle(t)
	key := fixedBytes(KeySize, 21)

	for _, sc := range suiteCases {
		for _, n := range payloadSizes {
			name := fmt.Sprintf("%s/hdr%d_aad%d/%d", sc.suite, sc.headerLen, sc.aadOffset, n)
			t.Run(name, func(t *testing.T) {
				iv := fixedBytes(IVSize, int64(n)+22)
				plain := fixedBytes(n, int64(n)+23)
				header := fixedBytes(sc.headerLen, int64(sc.headerLen)+24)
				aad := fixedBytes(n%29, int64(n)+25)

				want, err := o.borgEncrypt(sc.suite, sc.headerLen, sc.aadOffset, key, iv, header, aad, plain)
				if err != nil {
					t.Fatalf("borg encrypt: %v", err)
				}
				a, err := NewAEAD(sc.goSuite, key, sc.headerLen, sc.aadOffset)
				if err != nil {
					t.Fatal(err)
				}
				got, err := a.Encrypt(iv, plain, header, aad)
				if err != nil {
					t.Fatalf("borge encrypt: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("envelopes differ\n  borge: %s\n  borg:  %s", preview(got), preview(want))
				}
			})
		}
	}
}

// TestBorgRejectsTamperedBorgeEnvelopes checks the direction that matters for
// integrity: borg must refuse an envelope borge produced and something then modified.
// An implementation whose tag does not actually cover the data would pass every
// round-trip test above and fail this one.
func TestBorgRejectsTamperedBorgeEnvelopes(t *testing.T) {
	o := startOracle(t)
	key := fixedBytes(KeySize, 31)
	iv := fixedBytes(IVSize, 32)

	for _, sc := range suiteCases {
		if sc.headerLen == 0 {
			continue // no header bytes to tamper with
		}
		a, err := NewAEAD(sc.goSuite, key, sc.headerLen, sc.aadOffset)
		if err != nil {
			t.Fatal(err)
		}
		plain := []byte("a repository object's worth of plaintext, more or less")
		header := fixedBytes(sc.headerLen, 33)
		aad := []byte("associated data")
		env, err := a.Encrypt(iv, plain, header, aad)
		if err != nil {
			t.Fatal(err)
		}

		// Tamper at a few structurally interesting offsets: inside the authenticated
		// part of the header, in the tag, and in the ciphertext.
		offsets := []int{sc.aadOffset, sc.headerLen, sc.headerLen + MACSize}
		for _, off := range offsets {
			if off >= len(env) {
				continue
			}
			bad := append([]byte(nil), env...)
			bad[off] ^= 0x01

			if _, err := o.borgDecrypt(sc.suite, sc.headerLen, sc.aadOffset, key, iv, aad, bad); err == nil {
				t.Errorf("%s hdr=%d aad=%d: borg accepted an envelope tampered at offset %d",
					sc.suite, sc.headerLen, sc.aadOffset, off)
			}
			if _, err := a.Decrypt(iv, bad, aad); !errors.Is(err, ErrAuthentication) {
				t.Errorf("%s hdr=%d aad=%d: borge accepted an envelope tampered at offset %d (err=%v)",
					sc.suite, sc.headerLen, sc.aadOffset, off, err)
			}
		}

		// A header byte *before* aad_offset is deliberately not authenticated, so
		// changing it must still decrypt - that is what aad_offset is for. Confirming it
		// keeps the test honest about what the offset actually means.
		if sc.aadOffset > 0 {
			bad := append([]byte(nil), env...)
			bad[0] ^= 0x01
			if _, err := a.Decrypt(iv, bad, aad); err != nil {
				t.Errorf("%s hdr=%d aad=%d: changing an unauthenticated header byte broke decryption: %v",
					sc.suite, sc.headerLen, sc.aadOffset, err)
			}
		}
	}
}

// TestHashesMatchBorg covers the keyed hashes borg uses for chunk ids and MACs.
func TestHashesMatchBorg(t *testing.T) {
	o := startOracle(t)

	inputs := [][]byte{
		nil,
		[]byte("a"),
		[]byte("the quick brown fox"),
		fixedBytes(32, 41),
		fixedBytes(1000, 42),
		fixedBytes(1<<20, 43),
	}
	keys := [][]byte{
		fixedBytes(32, 51),
		fixedBytes(64, 52), // HMAC accepts any key length
	}

	for _, key := range keys {
		for _, data := range inputs {
			t.Run(fmt.Sprintf("hmac_sha256/key%d/data%d", len(key), len(data)), func(t *testing.T) {
				resp, err := o.ask(fmt.Sprintf("H hmac_sha256 %s %s", enhex(key), enhex(data)))
				if err != nil {
					t.Fatal(err)
				}
				want, _ := unhexOrDash(resp)
				if got := HMACSHA256(key, data); !bytes.Equal(got, want) {
					t.Errorf("hmac_sha256\n  borge: %x\n  borg:  %x", got, want)
				}
			})
			t.Run(fmt.Sprintf("blake2b_256/key%d/data%d", len(key), len(data)), func(t *testing.T) {
				resp, err := o.ask(fmt.Sprintf("H blake2b_256 %s %s", enhex(key), enhex(data)))
				if err != nil {
					t.Fatal(err)
				}
				want, _ := unhexOrDash(resp)
				if got := Blake2b256(key, data); !bytes.Equal(got, want) {
					t.Errorf("blake2b_256\n  borge: %x\n  borg:  %x", got, want)
				}
			})
		}
	}

	// blake3 requires a 32-byte key; it is the id hash for the blake3 key types.
	for _, data := range inputs {
		t.Run(fmt.Sprintf("blake3_keyed/data%d", len(data)), func(t *testing.T) {
			key := fixedBytes(32, 61)
			resp, err := o.ask(fmt.Sprintf("H blake3_keyed %s %s", enhex(key), enhex(data)))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := unhexOrDash(resp)
			got, err := Blake3Keyed(key, data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("blake3_keyed\n  borge: %x\n  borg:  %x", got, want)
			}
		})
	}

	for _, data := range inputs {
		t.Run(fmt.Sprintf("blake2b_128/data%d", len(data)), func(t *testing.T) {
			resp, err := o.ask(fmt.Sprintf("H blake2b_128 - %s", enhex(data)))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := unhexOrDash(resp)
			if got := Blake2b128(data); !bytes.Equal(got, want) {
				t.Errorf("blake2b_128\n  borge: %x\n  borg:  %x", got, want)
			}
		})
	}
}

func preview(b []byte) string {
	const limit = 48
	if len(b) <= limit {
		return hex.EncodeToString(b)
	}
	return fmt.Sprintf("%x... (%d bytes)", b[:limit], len(b))
}
