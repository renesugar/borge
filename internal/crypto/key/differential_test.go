// SPDX-License-Identifier: Apache-2.0

package key

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
	"strconv"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/item"
)

// The stage 4 gate, at the level this package can reach on its own: borg's key classes
// read what borge writes and borge reads what borg writes - for the AEAD envelope, the
// session key derivation, the id hashes, and the passphrase-protected key blob.
//
// The repository-level half of the gate (a whole repokey and keyfile repository, opened
// from the other side) lives in internal/repository.

type keyOracle struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func startKeyOracle(t *testing.T) *keyOracle {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg key differential test in short mode")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the key differential test")
	}

	cmd := exec.Command(py, "testdata/oracle.py")
	// The real argon2 parameters cost 64 MiB and hundreds of milliseconds per attempt.
	// Both sides have to weaken them together or the blobs will not match.
	cmd.Env = append(os.Environ(), "BORG_TESTONLY_WEAKEN_KDF=1")
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
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	return &keyOracle{stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<22), stderr: &stderr}
}

func (o *keyOracle) ask(t *testing.T, format string, args ...any) string {
	t.Helper()
	resp, err := o.try(format, args...)
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
	return resp
}

func (o *keyOracle) try(format string, args ...any) (string, error) {
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

func hexOrDash(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	return hex.EncodeToString(b)
}

func textOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return hex.EncodeToString([]byte(s))
}

func mustUnhex(t *testing.T, s string) []byte {
	t.Helper()
	if s == "-" {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("oracle returned %q, which is not hex: %v", s, err)
	}
	return b
}

var allModes = []string{
	"aes256-ocb", "chacha20-poly1305", "blake3-aes256-ocb", "blake3-chacha20-poly1305",
	"authenticated-sha256", "authenticated-blake3", "none-sha256", "none-blake3",
}

func keyForMode(t *testing.T, mode string, cryptKey, idKey []byte) Key {
	t.Helper()
	k, err := ByName(mode, cryptKey, idKey)
	if err != nil {
		t.Fatalf("%s: %v", mode, err)
	}
	return k
}

// TestIDHashMatchesBorg: chunk ids must agree exactly, or the two tools would never
// deduplicate against each other and a transfer would rewrite everything.
func TestIDHashMatchesBorg(t *testing.T) {
	o := startKeyOracle(t)
	cryptKey, idKey := testMaterial()

	payloads := map[string][]byte{
		"empty": nil,
		"short": []byte("x"),
		"text":  []byte("the quick brown fox jumps over the lazy dog"),
		"large": bytes.Repeat([]byte{0xa5, 0x00, 0xff}, 100000),
	}

	for _, mode := range allModes {
		for name, data := range payloads {
			t.Run(mode+"/"+name, func(t *testing.T) {
				k := keyForMode(t, mode, cryptKey, idKey)
				// The unkeyed modes take no id key, and the oracle must be told the same.
				oracleIDKey := idKey
				if !RequiresKeyMaterial(k.Type()) {
					oracleIDKey = nil
				}
				want := o.ask(t, "H %s %s %s", mode, hexOrDash(oracleIDKey), hexOrDash(data))
				if got := hex.EncodeToString(k.IDHash(data)); got != want {
					t.Errorf("id hash differs\n  borge: %s\n  borg:  %s", got, want)
				}
			})
		}
	}
}

// TestBorgReadsBorgeEnvelope covers every mode in the direction that matters most: what
// borge writes into a repository has to be readable by borg.
func TestBorgReadsBorgeEnvelope(t *testing.T) {
	o := startKeyOracle(t)
	cryptKey, idKey := testMaterial()

	payloads := [][]byte{nil, []byte("x"), []byte("a repository object"), bytes.Repeat([]byte("borge"), 20000)}
	aads := [][]byte{nil, []byte("BORG_OBJ\x00"), bytes.Repeat([]byte{0x7f}, 64)}

	for _, mode := range allModes {
		for i, data := range payloads {
			for j, aad := range aads {
				t.Run(fmt.Sprintf("%s/data%d/aad%d", mode, i, j), func(t *testing.T) {
					k := keyForMode(t, mode, cryptKey, idKey)
					id := k.IDHash(data)
					env, err := k.Encrypt(id, data, aad)
					if err != nil {
						t.Fatal(err)
					}

					oracleCryptKey, oracleIDKey := cryptKey, idKey
					if !RequiresKeyMaterial(k.Type()) {
						oracleCryptKey, oracleIDKey = nil, nil
					}
					got := o.ask(t, "D %s %s %s %s %s %s", mode,
						hexOrDash(oracleCryptKey), hexOrDash(oracleIDKey),
						hexOrDash(id), hexOrDash(env), hexOrDash(aad))
					if !bytes.Equal(mustUnhex(t, got), data) {
						t.Errorf("borg decrypted borge's envelope to %d bytes, want %d",
							len(mustUnhex(t, got)), len(data))
					}
				})
			}
		}
	}
}

// TestBorgeReadsBorgEnvelope is the other direction. For the AEAD modes it is the real
// test of the session key derivation: borg picks a random session id, borge has to
// rederive the same key from it.
func TestBorgeReadsBorgEnvelope(t *testing.T) {
	o := startKeyOracle(t)
	cryptKey, idKey := testMaterial()

	payloads := [][]byte{nil, []byte("y"), []byte("written by borg"), bytes.Repeat([]byte{1, 2, 3}, 30000)}
	aads := [][]byte{nil, []byte("BORG_OBJ\x00"), bytes.Repeat([]byte{0x7f}, 64)}

	for _, mode := range allModes {
		for i, data := range payloads {
			for j, aad := range aads {
				t.Run(fmt.Sprintf("%s/data%d/aad%d", mode, i, j), func(t *testing.T) {
					k := keyForMode(t, mode, cryptKey, idKey)
					id := k.IDHash(data)

					oracleCryptKey, oracleIDKey := cryptKey, idKey
					if !RequiresKeyMaterial(k.Type()) {
						oracleCryptKey, oracleIDKey = nil, nil
					}
					envHex := o.ask(t, "E %s %s %s %s %s %s", mode,
						hexOrDash(oracleCryptKey), hexOrDash(oracleIDKey),
						hexOrDash(id), hexOrDash(data), hexOrDash(aad))
					env := mustUnhex(t, envHex)

					if env[0] != k.Type() {
						t.Errorf("borg's type byte is 0x%02x, borge expects 0x%02x", env[0], k.Type())
					}
					got, err := k.Decrypt(id, env, aad)
					if err != nil {
						t.Fatalf("borge could not read borg's envelope: %v", err)
					}
					if !bytes.Equal(got, data) {
						t.Errorf("borge decrypted borg's envelope to %d bytes, want %d", len(got), len(data))
					}
				})
			}
		}
	}
}

// TestMACEnvelopesAreByteIdentical: the MAC modes are deterministic, so borge's envelope
// must equal borg's exactly. The AEAD modes cannot be compared this way - they carry a
// random session id - which is why the tests above compare by round trip instead.
func TestMACEnvelopesAreByteIdentical(t *testing.T) {
	o := startKeyOracle(t)
	cryptKey, idKey := testMaterial()

	for _, mode := range []string{"none-sha256", "none-blake3", "authenticated-sha256", "authenticated-blake3"} {
		t.Run(mode, func(t *testing.T) {
			k := keyForMode(t, mode, cryptKey, idKey)
			data := []byte("deterministic by construction")
			id := k.IDHash(data)
			aad := []byte("BORG_OBJ\x00")

			env, err := k.Encrypt(id, data, aad)
			if err != nil {
				t.Fatal(err)
			}
			oracleCryptKey, oracleIDKey := cryptKey, idKey
			if !RequiresKeyMaterial(k.Type()) {
				oracleCryptKey, oracleIDKey = nil, nil
			}
			want := o.ask(t, "E %s %s %s %s %s %s", mode,
				hexOrDash(oracleCryptKey), hexOrDash(oracleIDKey),
				hexOrDash(id), hexOrDash(data), hexOrDash(aad))
			if got := hex.EncodeToString(env); got != want {
				t.Errorf("envelopes differ\n  borge: %s\n  borg:  %s", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------- the key blob

func testRepoID() []byte {
	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(0x10 + i)
	}
	return id
}

func newTestMaterial(repoID, cryptKey, idKey []byte, seed int64) *item.Key {
	s := seed
	return &item.Key{
		Version:      2,
		RepositoryID: repoID,
		CryptKey:     cryptKey,
		IDKey:        idKey,
		ChunkSeed:    &s,
	}
}

// TestKeyBlobRoundTripsThroughBorg exercises both directions of the passphrase-protected
// blob: borg opens what borge sealed, and borge opens what borg sealed.
func TestKeyBlobRoundTripsThroughBorg(t *testing.T) {
	o := startKeyOracle(t)
	repoID := testRepoID()
	repoIDHex := hex.EncodeToString(repoID)
	cryptKey, idKey := testMaterial()

	cases := []struct {
		name       string
		passphrase string
		label      string
		seed       int64
	}{
		{"admin", "correct horse battery staple", AdminLabel, -123456},
		{"empty passphrase", "", AdminLabel, 0},
		{"utf-8 passphrase", "ünïcodé ✓", "backup", 2147483647},
		{"no label", "plain", "", -2147483648},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/borge seals", func(t *testing.T) {
			material := newTestMaterial(repoID, cryptKey, idKey, tc.seed)
			text, err := SealMaterial(material, tc.passphrase, tc.label)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(text, KeyfileID+" "+repoIDHex+"\n") {
				t.Fatalf("blob does not start with borg's header line: %q", text[:40])
			}

			resp := o.ask(t, "O %s %s %s", textOrDash(tc.passphrase), hexOrDash(repoID),
				hexOrDash([]byte(text)))
			fields := strings.Fields(resp)
			if len(fields) != 5 {
				t.Fatalf("malformed response %q", resp)
			}
			if got := mustUnhex(t, fields[0]); !bytes.Equal(got, cryptKey) {
				t.Error("borg read a different crypt key")
			}
			if got := mustUnhex(t, fields[1]); !bytes.Equal(got, idKey) {
				t.Error("borg read a different id key")
			}
			if got, _ := strconv.ParseInt(fields[2], 10, 64); got != tc.seed {
				t.Errorf("borg read chunk seed %d, want %d", got, tc.seed)
			}
			if fields[3] != "2" {
				t.Errorf("borg read key version %s, want 2", fields[3])
			}
			gotLabel := string(mustUnhex(t, fields[4]))
			if gotLabel != tc.label {
				t.Errorf("borg read label %q, want %q", gotLabel, tc.label)
			}
		})

		t.Run(tc.name+"/borg seals", func(t *testing.T) {
			textHex := o.ask(t, "S %s %s %s %s %d %s",
				textOrDash(tc.passphrase), hexOrDash(repoID),
				hexOrDash(cryptKey), hexOrDash(idKey), tc.seed, textOrDash(tc.label))
			text := mustUnhex(t, textHex)

			material, env, err := OpenMaterial(text, repoIDHex, tc.passphrase)
			if err != nil {
				t.Fatalf("borge could not open borg's key blob: %v", err)
			}
			if !bytes.Equal(material.CryptKey, cryptKey) {
				t.Error("borge read a different crypt key")
			}
			if !bytes.Equal(material.IDKey, idKey) {
				t.Error("borge read a different id key")
			}
			if got := int64(ChunkSeed(material)); got != tc.seed {
				t.Errorf("borge read chunk seed %d, want %d", got, tc.seed)
			}
			if !bytes.Equal(material.RepositoryID, repoID) {
				t.Error("borge read a different repository id")
			}
			if env.Label != tc.label {
				t.Errorf("borge read label %q, want %q", env.Label, tc.label)
			}
			if env.Algorithm != AlgorithmArgon2 {
				t.Errorf("borge read algorithm %q, want %q", env.Algorithm, AlgorithmArgon2)
			}

			// And the wrong passphrase must fail, rather than producing garbage material.
			if _, _, err := OpenMaterial(text, repoIDHex, tc.passphrase+"x"); !errors.Is(err, ErrPassphraseWrong) {
				t.Errorf("a wrong passphrase gave %v, want ErrPassphraseWrong", err)
			}
		})
	}
}

// TestPaperKeyMatchesBorg: the numbered lines and their checksums must be borg's
// exactly, or a key printed by one tool could not be typed into the other - which is the
// one situation a paper key exists for.
func TestPaperKeyMatchesBorg(t *testing.T) {
	o := startKeyOracle(t)
	repoID := testRepoID()
	repoIDHex := hex.EncodeToString(repoID)
	cryptKey, idKey := testMaterial()

	material := newTestMaterial(repoID, cryptKey, idKey, -42)
	text, err := SealMaterial(material, "pass", AdminLabel)
	if err != nil {
		t.Fatal(err)
	}
	blob := Blob{ID: BlobName([]byte(text)), Text: []byte(text), Label: AdminLabel}

	ours, err := ExportPaperKey(blob, repoIDHex)
	if err != nil {
		t.Fatal(err)
	}
	theirs := string(mustUnhex(t, o.ask(t, "P %s %s", hexOrDash(repoID), hexOrDash([]byte(text)))))

	// The leading instruction line names the tool, so compare from the magic line on -
	// that is the part a person types back in.
	cut := func(s string) []string {
		i := strings.Index(s, paperKeyMagic)
		if i < 0 {
			t.Fatalf("no %q in:\n%s", paperKeyMagic, s)
		}
		return strings.Split(strings.TrimRight(s[i:], "\n"), "\n")
	}
	gotLines, wantLines := cut(ours), cut(theirs)
	if len(gotLines) != len(wantLines) {
		t.Fatalf("borge printed %d lines, borg %d", len(gotLines), len(wantLines))
	}
	for i := range gotLines {
		if gotLines[i] != wantLines[i] {
			t.Errorf("line %d differs\n  borge: %q\n  borg:  %q", i, gotLines[i], wantLines[i])
		}
	}

	// And borge reads borg's printout.
	back, err := ImportPaperKey(theirs, repoIDHex)
	if err != nil {
		t.Fatalf("borge could not read borg's paper key: %v", err)
	}
	if _, _, err := OpenMaterial(back, repoIDHex, "pass"); err != nil {
		t.Fatalf("the key reconstructed from borg's paper key does not unlock: %v", err)
	}
}
