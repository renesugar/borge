// SPDX-License-Identifier: Apache-2.0

package key

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeRepo is an in-memory RepoKeyStore, so the manager can be tested without a
// repository.
type fakeRepo struct {
	blobs map[string][]byte
}

func newFakeRepo() *fakeRepo { return &fakeRepo{blobs: map[string][]byte{}} }

func (f *fakeRepo) ListKeys() ([]NamedBlob, error) {
	var names []string
	for name := range f.blobs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]NamedBlob, 0, len(names))
	for _, name := range names {
		out = append(out, NamedBlob{Name: name, Data: f.blobs[name]})
	}
	return out, nil
}

func (f *fakeRepo) StoreKey(data []byte) (string, error) {
	name := BlobName(data)
	f.blobs[name] = append([]byte(nil), data...)
	return name, nil
}

func (f *fakeRepo) DeleteKey(name string) error {
	delete(f.blobs, name)
	return nil
}

// testManager returns a manager whose keyfiles land in a temporary directory and whose
// KDF is weakened, so the tests run in milliseconds rather than minutes.
func testManager(t *testing.T) (*Manager, *fakeRepo, []byte) {
	t.Helper()
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	dir := t.TempDir()
	t.Setenv("BORGE_KEYS_DIR", dir)
	// Make sure nothing inherited from the environment reaches into the real key store.
	t.Setenv("BORG_KEYS_DIR", "")
	t.Setenv("BORG_KEY_FILE", "")
	t.Setenv("BORGE_KEY_FILE", "")

	repoID := testRepoID()
	repo := newFakeRepo()
	m, err := NewManager(repoID, repo)
	if err != nil {
		t.Fatal(err)
	}
	return m, repo, repoID
}

func TestManagerRepokeyRoundTrip(t *testing.T) {
	m, repo, repoID := testManager(t)

	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := m.Save(material, "hunter2", SaveOptions{Storage: StorageRepo, Label: AdminLabel})
	if err != nil {
		t.Fatal(err)
	}
	if blob.Storage != StorageRepo || blob.Label != AdminLabel {
		t.Errorf("blob is %+v", blob)
	}
	if len(repo.blobs) != 1 {
		t.Fatalf("the repository holds %d keys, want 1", len(repo.blobs))
	}

	u, err := m.Unlock("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.Material.CryptKey, material.CryptKey) {
		t.Error("the unlocked crypt key differs")
	}
	if u.Blob.Storage != StorageRepo {
		t.Errorf("unlocked from %q, want repokey", u.Blob.Storage)
	}

	if _, err := m.Unlock("wrong"); !errors.Is(err, ErrPassphraseWrong) {
		t.Errorf("a wrong passphrase gave %v", err)
	}
}

func TestManagerKeyfileRoundTrip(t *testing.T) {
	m, repo, repoID := testManager(t)

	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := m.Save(material, "hunter2", SaveOptions{Storage: StorageKeyfile, Label: AdminLabel})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.blobs) != 0 {
		t.Error("a keyfile key was also written into the repository")
	}
	info, err := os.Stat(blob.Path)
	if err != nil {
		t.Fatal(err)
	}
	// A key file is a secret; anything group- or world-readable is a bug.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode is %o, want 600", perm)
	}
	if filepath.Base(blob.Path) != BlobName(blob.Text) {
		t.Errorf("key file is named %q, want its content digest", filepath.Base(blob.Path))
	}

	u, err := m.Unlock("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if u.Blob.Storage != StorageKeyfile {
		t.Errorf("unlocked from %q, want keyfile", u.Blob.Storage)
	}
	if !bytes.Equal(u.Material.IDKey, material.IDKey) {
		t.Error("the unlocked id key differs")
	}
}

// TestManagerSkipsOtherRepositories: a keys directory is shared between repositories, so
// blobs naming a different one must be ignored rather than tried.
func TestManagerSkipsOtherRepositories(t *testing.T) {
	m, _, repoID := testManager(t)

	mine, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(mine, "mine", SaveOptions{Storage: StorageKeyfile, Label: AdminLabel}); err != nil {
		t.Fatal(err)
	}

	otherID := make([]byte, 32)
	for i := range otherID {
		otherID[i] = 0xee
	}
	other, err := NewMaterial(otherID)
	if err != nil {
		t.Fatal(err)
	}
	otherText, err := SealMaterial(other, "theirs", AdminLabel)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := m.keysDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirs[0], "someone-elses-key"), []byte(otherText), 0o600); err != nil {
		t.Fatal(err)
	}
	// And a file that is not a key at all.
	if err := os.WriteFile(filepath.Join(dirs[0], "notes.txt"), []byte("shopping list"), 0o600); err != nil {
		t.Fatal(err)
	}

	blobs, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("List returned %d keys, want only this repository's one", len(blobs))
	}
	if _, err := m.Unlock("theirs"); !errors.Is(err, ErrPassphraseWrong) {
		t.Error("the other repository's passphrase unlocked something")
	}
}

func TestManagerAddAndRemoveKeys(t *testing.T) {
	m, _, repoID := testManager(t)

	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(material, "admin pass", SaveOptions{Storage: StorageRepo, Label: AdminLabel}); err != nil {
		t.Fatal(err)
	}
	u, err := m.Unlock("admin pass")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.AddKey(u, "second pass", ""); err == nil {
		t.Error("a key was added with no label")
	}
	if _, err := m.AddKey(u, "second pass", AdminLabel); err == nil {
		t.Error("the reserved admin label was accepted")
	}
	if _, err := m.AddKey(u, "second pass", "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddKey(u, "third pass", "ops"); err == nil {
		t.Error("a duplicate label was accepted")
	}

	// Both passphrases now open the same material - that is the point of a second key.
	second, err := m.Unlock("second pass")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Material.CryptKey, u.Material.CryptKey) {
		t.Error("the second key protects different material")
	}
	if second.Blob.Label != "ops" {
		t.Errorf("second key's label is %q, want ops", second.Blob.Label)
	}

	if _, err := m.RemoveKey(AdminLabel); err == nil {
		t.Error("the admin key was removable")
	}
	if _, err := m.RemoveKey("ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Unlock("second pass"); !errors.Is(err, ErrPassphraseWrong) {
		t.Error("the removed key still unlocks")
	}
	if _, err := m.RemoveKey(AdminLabel); err == nil {
		t.Error("the last remaining key was removable")
	}
}

func TestManagerChangePassphrase(t *testing.T) {
	for _, storage := range []Storage{StorageRepo, StorageKeyfile} {
		t.Run(string(storage), func(t *testing.T) {
			m, _, repoID := testManager(t)
			material, err := NewMaterial(repoID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Save(material, "old", SaveOptions{Storage: storage, Label: AdminLabel}); err != nil {
				t.Fatal(err)
			}
			u, err := m.Unlock("old")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.ChangePassphrase(u, "new"); err != nil {
				t.Fatal(err)
			}

			blobs, err := m.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(blobs) != 1 {
				t.Errorf("after a passphrase change there are %d keys, want 1", len(blobs))
			}
			if blobs[0].Label != AdminLabel {
				t.Errorf("the label was lost: %q", blobs[0].Label)
			}
			if _, err := m.Unlock("old"); !errors.Is(err, ErrPassphraseWrong) {
				t.Error("the old passphrase still works")
			}
			after, err := m.Unlock("new")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after.Material.CryptKey, material.CryptKey) {
				t.Error("changing the passphrase changed the key material")
			}
		})
	}
}

// TestManagerExportImport: a key exported from a repokey repository must import into a
// keyfile one and the other way round, because the blob does not record where it lives.
func TestManagerExportImport(t *testing.T) {
	m, repo, repoID := testManager(t)
	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(material, "pass", SaveOptions{Storage: StorageRepo, Label: AdminLabel}); err != nil {
		t.Fatal(err)
	}

	exported, err := m.Export("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(exported.Text), KeyfileID+" "+hex.EncodeToString(repoID)) {
		t.Fatal("the export is not a key blob")
	}

	// Import it as a keyfile into a fresh manager for the same repository.
	m2, _, _ := testManager(t)
	m2.Repo = repo
	imported, err := m2.Import(exported.Text, StorageKeyfile)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Storage != StorageKeyfile {
		t.Errorf("imported as %q", imported.Storage)
	}
	u, err := m2.Unlock("pass")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(u.Material.CryptKey, material.CryptKey) {
		t.Error("the imported key protects different material")
	}

	// A key for another repository must be refused rather than stored.
	otherID := bytes.Repeat([]byte{0x99}, 32)
	otherMaterial, err := NewMaterial(otherID)
	if err != nil {
		t.Fatal(err)
	}
	otherText, err := SealMaterial(otherMaterial, "pass", AdminLabel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Import([]byte(otherText), StorageRepo); !errors.Is(err, ErrRepositoryMismatch) {
		t.Errorf("importing another repository's key gave %v", err)
	}
	if _, err := m2.Import([]byte("not a key at all"), StorageRepo); !errors.Is(err, ErrNotAKeyfile) {
		t.Errorf("importing junk gave %v", err)
	}
}

// TestManagerKeyfileWinsOverRepokey pins the search order: a keyfile is tried first, so a
// user who moved their key out of the repository is not silently served the copy that
// stayed behind.
func TestManagerKeyfileWinsOverRepokey(t *testing.T) {
	m, _, repoID := testManager(t)
	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(material, "pass", SaveOptions{Storage: StorageRepo, Label: "in-repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(material, "pass", SaveOptions{Storage: StorageKeyfile, Label: "on-disk"}); err != nil {
		t.Fatal(err)
	}
	u, err := m.Unlock("pass")
	if err != nil {
		t.Fatal(err)
	}
	if u.Blob.Storage != StorageKeyfile {
		t.Errorf("unlocked the %s key, want the keyfile", u.Blob.Storage)
	}
}

// TestManagerCreateRefusesToOverwrite covers borg #6036: a careless BORG_KEY_FILE must
// not destroy another repository's key.
func TestManagerCreateRefusesToOverwrite(t *testing.T) {
	m, _, repoID := testManager(t)
	path := filepath.Join(t.TempDir(), "the-only-copy")
	if err := os.WriteFile(path, []byte("someone else's key"), 0o600); err != nil {
		t.Fatal(err)
	}
	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Save(material, "pass", SaveOptions{Storage: StorageKeyfile, Path: path, Create: true})
	if err == nil {
		t.Fatal("an existing key file was overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "someone else's key" {
		t.Error("the existing file was modified anyway")
	}
}

func TestCorruptKeyIsListedNotHidden(t *testing.T) {
	m, _, repoID := testManager(t)
	material, err := NewMaterial(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(material, "pass", SaveOptions{Storage: StorageRepo, Label: AdminLabel}); err != nil {
		t.Fatal(err)
	}

	dirs, err := m.keysDirs()
	if err != nil {
		t.Fatal(err)
	}
	broken := KeyfileID + " " + hex.EncodeToString(repoID) + "\nnot base64 at all!!!\n"
	if err := os.WriteFile(filepath.Join(dirs[0], "broken"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	blobs, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var corrupt int
	for _, b := range blobs {
		if b.Corrupt {
			corrupt++
		}
	}
	if corrupt != 1 {
		t.Errorf("%d corrupt keys listed, want 1", corrupt)
	}
	// The good key still opens: a corrupt one must not stop the search.
	if _, err := m.Unlock("pass"); err != nil {
		t.Fatalf("a corrupt key blocked unlocking: %v", err)
	}
}

func TestKeysDirsPrefersExplicitSetting(t *testing.T) {
	t.Setenv("BORGE_KEYS_DIR", "/explicit/borge")
	t.Setenv("BORG_KEYS_DIR", "/explicit/borg")
	dirs, err := KeysDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "/explicit/borge" {
		t.Errorf("KeysDirs is %v, want only the explicit borge setting", dirs)
	}
}

// TestKeysDirsReadsBorgsDirectory is the divergence recorded in docs/DIVERGENCES.md:
// borge writes into its own configuration directory but still reads borg's, so a
// repository borg just created can be opened.
func TestKeysDirsReadsBorgsDirectory(t *testing.T) {
	os.Unsetenv("BORGE_KEYS_DIR")
	os.Unsetenv("BORG_KEYS_DIR")
	t.Setenv("BORGE_CONFIG_DIR", "/cfg/borge")
	t.Setenv("BORG_CONFIG_DIR", "/cfg/borg")
	dirs, err := KeysDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("/cfg/borge", "keys"), filepath.Join("/cfg/borg", "keys")}
	if len(dirs) != 2 || dirs[0] != want[0] || dirs[1] != want[1] {
		t.Errorf("KeysDirs is %v, want %v", dirs, want)
	}
}
