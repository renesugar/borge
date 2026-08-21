// SPDX-License-Identifier: Apache-2.0

package repository

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/repoobj"
)

// The stage 4 gate, at repository level: borge opens repositories borg created, in every
// mode and both key locations, and borg accepts the key blobs borge writes.
//
// This drives borg's command line rather than a Python oracle, because what is under test
// is the whole thing a user would actually do - "borg repo-create", then borge - not a
// function borge happens to call.

// borgCLI runs the pinned borg 2 with an environment pointing at the test's own
// directories, so nothing touches the user's real keys or configuration.
type borgCLI struct {
	t          *testing.T
	binary     string
	keysDir    string
	configDir  string
	passphrase string
}

func newBorgCLI(t *testing.T) *borgCLI {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg key gate in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, ".venv-borg2", "bin", "borg")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the key gate")
	}

	base := t.TempDir()
	c := &borgCLI{
		t:          t,
		binary:     binary,
		keysDir:    filepath.Join(base, "keys"),
		configDir:  filepath.Join(base, "config"),
		passphrase: "gate passphrase",
	}
	for _, d := range []string{c.keysDir, c.configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// borge must look in the same places and use the same weakened KDF, or the two sides
	// simply will not find or open each other's keys.
	t.Setenv("BORGE_KEYS_DIR", c.keysDir)
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	t.Setenv("BORGE_KEY_FILE", "")
	t.Setenv("BORG_KEY_FILE", "")
	return c
}

func (c *borgCLI) env(passphrase string) []string {
	return append(os.Environ(),
		"BORG_KEYS_DIR="+c.keysDir,
		"BORG_CONFIG_DIR="+c.configDir,
		"BORG_CACHE_DIR="+filepath.Join(c.configDir, "cache"),
		"BORG_TESTONLY_WEAKEN_KDF=1",
		"BORG_PASSPHRASE="+passphrase,
		"BORG_KEY_FILE=",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	)
}

func (c *borgCLI) run(passphrase string, args ...string) (string, error) {
	c.t.Helper()
	cmd := exec.Command(c.binary, args...)
	cmd.Env = c.env(passphrase)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("borg %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (c *borgCLI) mustRun(passphrase string, args ...string) string {
	c.t.Helper()
	out, err := c.run(passphrase, args...)
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

// createRepo makes a repository with borg and returns its path.
func (c *borgCLI) createRepo(name, encryption, idHash, location string) string {
	c.t.Helper()
	path := filepath.Join(c.t.TempDir(), name)
	args := []string{"repo-create", "-r", path, "-e", encryption}
	if idHash != "" {
		args = append(args, "-i", idHash)
	}
	if location != "" {
		args = append(args, "--key-location", location)
	}
	c.mustRun(c.passphrase, args...)
	return path
}

// gateModes is every mode borg can create, in both key locations where that is
// meaningful.
var gateModes = []struct {
	name       string
	encryption string
	idHash     string
	location   string
	wantType   byte
	wantMode   string
}{
	{"aes256-ocb/repokey", "aes256-ocb", "sha256", "repokey", key.TypeAESOCB, "aes256-ocb"},
	{"aes256-ocb/keyfile", "aes256-ocb", "sha256", "keyfile", key.TypeAESOCB, "aes256-ocb"},
	{"chacha20-poly1305/repokey", "chacha20-poly1305", "sha256", "repokey", key.TypeCHPO, "chacha20-poly1305"},
	{"chacha20-poly1305/keyfile", "chacha20-poly1305", "sha256", "keyfile", key.TypeCHPO, "chacha20-poly1305"},
	{"blake3-aes256-ocb/repokey", "aes256-ocb", "blake3", "repokey", key.TypeBlake3AESOCB, "blake3-aes256-ocb"},
	{"blake3-chacha20-poly1305/keyfile", "chacha20-poly1305", "blake3", "keyfile", key.TypeBlake3CHPO, "blake3-chacha20-poly1305"},
	{"authenticated-sha256/repokey", "authenticated-sha256", "", "repokey", key.TypeSHA256Authenticated, "authenticated-sha256"},
	{"authenticated-blake3/keyfile", "authenticated-blake3", "", "keyfile", key.TypeBlake3Authenticated, "authenticated-blake3"},
	{"none-sha256", "none-sha256", "", "", key.TypeSHA256None, "none-sha256"},
	{"none-blake3", "none-blake3", "", "", key.TypeBlake3None, "none-blake3"},
}

// TestBorgeUnlocksBorgRepositories is the gate: every mode borg can create, borge opens -
// finding the key wherever borg put it, and decrypting the manifest object to prove the
// key really is the right one.
func TestBorgeUnlocksBorgRepositories(t *testing.T) {
	c := newBorgCLI(t)

	for _, tc := range gateModes {
		t.Run(tc.name, func(t *testing.T) {
			path := c.createRepo("repo", tc.encryption, tc.idHash, tc.location)

			r, err := Open(location.MustLocal(path), Options{})
			if err != nil {
				t.Fatalf("borge could not open borg's repository: %v", err)
			}
			defer r.Close()

			gotType, err := r.DetectKeyType()
			if err != nil {
				t.Fatal(err)
			}
			if gotType != tc.wantType {
				t.Fatalf("borge read key type 0x%02x, want 0x%02x (%s)",
					gotType, tc.wantType, key.TypeName(tc.wantType))
			}

			k, unlocked, err := r.Unlock(c.passphrase)
			if err != nil {
				t.Fatalf("borge could not unlock borg's repository: %v", err)
			}
			if k.Name() != tc.wantMode {
				t.Errorf("borge built a %s key, want %s", k.Name(), tc.wantMode)
			}
			if key.RequiresKeyMaterial(gotType) {
				if unlocked == nil {
					t.Fatal("a keyed mode unlocked without a key blob")
				}
				wantStorage := key.Storage(tc.location)
				if unlocked.Blob.Storage != wantStorage {
					t.Errorf("the key was found as %q, borg stored it as %q",
						unlocked.Blob.Storage, wantStorage)
				}
				if unlocked.Blob.Label != key.AdminLabel {
					t.Errorf("the key's label is %q, want %q", unlocked.Blob.Label, key.AdminLabel)
				}
				if !bytes.Equal(unlocked.Material.RepositoryID, r.ID()) {
					t.Error("the key names a different repository")
				}
			}

			// The real proof: read the manifest object through the key borge built. A
			// wrong key, a wrong session derivation or a wrong AAD all fail here.
			ro, err := repoobj.New(k)
			if err != nil {
				t.Fatal(err)
			}
			obj, err := r.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			// The manifest's chunk id is 32 zero bytes, and it goes into the AAD, so it
			// has to be passed even though it hashes nothing.
			_, data, err := ro.Parse(key.ManifestID, obj, repoobj.TypeManifest, repoobj.ParseOptions{})
			if err != nil {
				t.Fatalf("borge could not read borg's manifest: %v", err)
			}
			if len(data) == 0 {
				t.Error("the manifest decrypted to nothing")
			}
			// A manifest is a msgpack map; 0x8x or 0xde/0xdf starts one.
			if b := data[0]; b>>4 != 0x8 && b != 0xde && b != 0xdf {
				t.Errorf("the decrypted manifest does not start like a msgpack map (0x%02x)", b)
			}
		})
	}
}

// TestBorgAcceptsBorgeWrittenKeys is the other direction. borge writes key blobs into a
// borg-created repository - a second key, then a changed passphrase - and borg has to
// keep working with them.
func TestBorgAcceptsBorgeWrittenKeys(t *testing.T) {
	c := newBorgCLI(t)

	for _, keyLoc := range []string{"repokey", "keyfile"} {
		t.Run(keyLoc, func(t *testing.T) {
			path := c.createRepo("repo", "aes256-ocb", "sha256", keyLoc)

			r, err := Open(location.MustLocal(path), Options{Exclusive: true})
			if err != nil {
				t.Fatal(err)
			}
			_, unlocked, err := r.Unlock(c.passphrase)
			if err != nil {
				t.Fatal(err)
			}
			m, err := r.KeyManager()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.AddKey(unlocked, "second passphrase", "ops"); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			// borg now has two keys, and the one borge added opens the repository.
			out := c.mustRun("second passphrase", "repo-info", "-r", path)
			if !strings.Contains(out, "Repository ID:") {
				t.Errorf("borg's repo-info did not report a repository:\n%s", out)
			}
			listed := c.mustRun(c.passphrase, "key", "list", "-r", path)
			if !strings.Contains(listed, "ops") {
				t.Errorf("borg does not list the key borge added:\n%s", listed)
			}
			if !strings.Contains(listed, key.AdminLabel) {
				t.Errorf("borge's write disturbed the admin key:\n%s", listed)
			}

			// And a passphrase change made by borge takes effect for borg.
			r, err = Open(location.MustLocal(path), Options{Exclusive: true})
			if err != nil {
				t.Fatal(err)
			}
			_, unlocked, err = r.Unlock("second passphrase")
			if err != nil {
				t.Fatal(err)
			}
			// A fresh manager: the previous one holds the repository that was closed above.
			m, err = r.KeyManager()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.ChangePassphrase(unlocked, "third passphrase"); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := c.run("second passphrase", "repo-info", "-r", path); err == nil {
				t.Error("borg still accepts the passphrase borge replaced")
			}
			if _, err := c.run("third passphrase", "repo-info", "-r", path); err != nil {
				t.Errorf("borg does not accept the passphrase borge set: %v", err)
			}
		})
	}
}

// TestKeyExportImportCrossCheck: an exported key is a portable artefact, so it has to
// cross the tools in both directions.
func TestKeyExportImportCrossCheck(t *testing.T) {
	c := newBorgCLI(t)
	path := c.createRepo("repo", "aes256-ocb", "sha256", "repokey")

	r, err := Open(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m, err := r.KeyManager()
	if err != nil {
		t.Fatal(err)
	}

	// borg exports, borge reads.
	exported := filepath.Join(t.TempDir(), "borg-export.key")
	c.mustRun(c.passphrase, "key", "export", "-r", path, exported)
	data, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	repoIDHex := hex.EncodeToString(r.ID())
	if !key.IsKeyfile(data, repoIDHex) {
		t.Fatalf("borg's export does not name this repository:\n%s", data)
	}
	fromBorg, env, err := key.OpenMaterial(data, repoIDHex, c.passphrase)
	if err != nil {
		t.Fatalf("borge could not open borg's exported key: %v", err)
	}
	if env.Label != key.AdminLabel {
		t.Errorf("the exported key's label is %q, want %q", env.Label, key.AdminLabel)
	}

	// borge exports, borg imports. The blob has to survive a round trip through borg's
	// own importer, which is stricter than just reading the file.
	ours, err := m.Export("")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ours.Text, normaliseBlob(data)) {
		// Not a failure: borg's export may re-wrap the base64. Compare the material.
		theirs, _, err := key.OpenMaterial(ours.Text, repoIDHex, c.passphrase)
		if err != nil {
			t.Fatalf("borge's own export does not open: %v", err)
		}
		if !bytes.Equal(theirs.CryptKey, fromBorg.CryptKey) || !bytes.Equal(theirs.IDKey, fromBorg.IDKey) {
			t.Fatal("borge exported different key material than borg did")
		}
	}

	ourExport := filepath.Join(t.TempDir(), "borge-export.key")
	if err := os.WriteFile(ourExport, ours.Text, 0o600); err != nil {
		t.Fatal(err)
	}
	c.mustRun(c.passphrase, "key", "import", "-r", path, "--key-location", "keyfile", ourExport)

	// borg now has the key as a keyfile too, and still opens the repository.
	if _, err := c.run(c.passphrase, "repo-info", "-r", path); err != nil {
		t.Errorf("borg cannot open the repository after importing borge's export: %v", err)
	}
	// And borge finds that keyfile: it landed in the shared keys directory.
	found, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var keyfiles int
	for _, b := range found {
		if b.Storage == key.StorageKeyfile {
			keyfiles++
		}
	}
	if keyfiles == 0 {
		t.Error("borge does not see the keyfile borg imported")
	}
}

// normaliseBlob strips trailing whitespace so two spellings of the same blob compare
// equal when they happen to match.
func normaliseBlob(b []byte) []byte {
	return append(bytes.TrimRight(b, "\n \t"), '\n')
}

// TestBorgReadsARepositoryBorgeHasLocked covers the bug the stage 4 gate found: borge's
// lock timestamps have to be timezone-aware, or borg raises TypeError comparing them
// against its own "now" and cannot open the repository at all.
func TestBorgReadsARepositoryBorgeHasLocked(t *testing.T) {
	c := newBorgCLI(t)
	path := c.createRepo("repo", "none-sha256", "", "")

	r, err := Open(location.MustLocal(path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// borge is holding a shared lock right now.
	locks, err := os.ReadDir(filepath.Join(path, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Fatalf("borge holds %d locks, want 1", len(locks))
	}
	content, err := os.ReadFile(filepath.Join(path, "locks", locks[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "+00:00") {
		t.Errorf("the lock timestamp is not timezone-aware: %s", content)
	}

	if out, err := c.run(c.passphrase, "repo-info", "-r", path); err != nil {
		t.Fatalf("borg could not read a repository borge has locked: %v\n%s", err, out)
	}
}

// TestBorgeSeesBorgsLocks is the same problem in the other direction: borge has to parse
// borg's timestamp, because a lock it cannot read is a lock it ignores - and ignoring a
// lock is how two writers end up in one repository.
func TestBorgeSeesBorgsLocks(t *testing.T) {
	c := newBorgCLI(t)
	path := c.createRepo("repo", "none-sha256", "", "")

	// Write a lock in borg's exact spelling, as if borg were holding it.
	record := fmt.Sprintf(
		`{"exclusive": true, "hostid": "somewhere", "processid": 12345, "threadid": 0, "time": "%s"}`,
		time.Now().UTC().Format(lockTimeLayout))
	lockDir := filepath.Join(path, "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := key.BlobName([]byte(record))
	if err := os.WriteFile(filepath.Join(lockDir, name), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Open(location.MustLocal(path), Options{})
	if err == nil {
		r.Close()
		t.Fatal("borge opened a repository borg holds an exclusive lock on")
	}
	if !strings.Contains(err.Error(), "lock") {
		t.Errorf("the failure does not mention locking: %v", err)
	}
}
