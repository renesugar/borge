// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/location"
	"github.com/renesugar/borge/internal/repository"
)

// The stage 5a gate: borge reads the manifest and the archive directory of a repository
// borg created, and borg reads a manifest borge rewrote.
//
// It drives borg's command line, because "borg repo-create" and "borg create" are what
// actually produce the shapes under test.

type borgRepo struct {
	t          *testing.T
	binary     string
	path       string
	keysDir    string
	configDir  string
	passphrase string
}

func newBorgRepo(t *testing.T, encryption string) *borgRepo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the borg manifest gate in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, ".venv-borg2", "bin", "borg")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the manifest gate")
	}

	base := t.TempDir()
	r := &borgRepo{
		t:          t,
		binary:     binary,
		path:       filepath.Join(base, "repo"),
		keysDir:    filepath.Join(base, "keys"),
		configDir:  filepath.Join(base, "config"),
		passphrase: "manifest gate",
	}
	for _, d := range []string{r.keysDir, r.configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BORGE_KEYS_DIR", r.keysDir)
	t.Setenv("BORGE_TESTONLY_WEAKEN_KDF", "1")
	t.Setenv("BORGE_KEY_FILE", "")
	t.Setenv("BORG_KEY_FILE", "")

	r.mustRun("repo-create", "-r", r.path, "-e", encryption)
	return r
}

func (r *borgRepo) env() []string {
	return append(os.Environ(),
		"BORG_KEYS_DIR="+r.keysDir,
		"BORG_CONFIG_DIR="+r.configDir,
		"BORG_CACHE_DIR="+filepath.Join(r.configDir, "cache"),
		"BORG_TESTONLY_WEAKEN_KDF=1",
		"BORG_PASSPHRASE="+r.passphrase,
		"BORG_KEY_FILE=",
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	)
}

func (r *borgRepo) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command(r.binary, args...)
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("borg %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func (r *borgRepo) mustRun(args ...string) string {
	r.t.Helper()
	out, err := r.run(args...)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// createArchive backs up a small tree, so the repository has real archives to list.
func (r *borgRepo) createArchive(name string, extra ...string) string {
	r.t.Helper()
	src := r.t.TempDir()
	for i := 0; i < 3; i++ {
		p := filepath.Join(src, fmt.Sprintf("file%d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat(name+" ", 100+i)), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "nested.txt"), []byte("nested "+name), 0o644); err != nil {
		r.t.Fatal(err)
	}

	args := append([]string{"create", "-r", r.path}, extra...)
	args = append(args, name, src)
	r.mustRun(args...)
	return src
}

// open opens the repository with borge and unlocks it.
func (r *borgRepo) open(t *testing.T) (*repository.Repository, *Manifest) {
	t.Helper()
	repo, err := repository.Open(location.MustLocal(r.path), repository.Options{})
	if err != nil {
		t.Fatalf("borge could not open borg's repository: %v", err)
	}
	t.Cleanup(func() { repo.Close() })

	k, _, err := repo.Unlock(r.passphrase)
	if err != nil {
		t.Fatalf("borge could not unlock: %v", err)
	}
	m, err := Load(repo, k, OpRead)
	if err != nil {
		t.Fatalf("borge could not load the manifest: %v", err)
	}
	return repo, m
}

// borgArchive is one row of "borg repo-list --json".
type borgArchive struct {
	Archive  string `json:"archive"`
	Name     string `json:"name"`
	ID       string `json:"id"`
	Time     string `json:"time"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Comment  string `json:"comment"`
	// borg renders tags as one comma-joined string here, not as a list.
	Tags string `json:"tags"`
}

func (r *borgRepo) repoList(extra ...string) []borgArchive {
	r.t.Helper()
	args := append([]string{"repo-list", "-r", r.path, "--json"}, extra...)
	out := r.mustRun(args...)
	var parsed struct {
		Archives []borgArchive `json:"archives"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		r.t.Fatalf("could not parse borg's repo-list JSON: %v\n%s", err, out)
	}
	return parsed.Archives
}

// TestBorgeListsBorgArchives is the gate: the archive directory borge reads is the one
// borg reports, down to ids, names, timestamps and hosts.
func TestBorgeListsBorgArchives(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	for _, name := range []string{"first", "second", "third"} {
		r.createArchive(name)
	}
	want := r.repoList()
	if len(want) != 3 {
		t.Fatalf("borg reports %d archives, want 3", len(want))
	}

	_, m := r.open(t)
	got, err := m.Archives.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("borge lists %d archives, borg %d", len(got), len(want))
	}

	// borg's default repo-list order is by timestamp, and so is borge's.
	for i := range want {
		w, g := want[i], got[i]
		if g.Name != w.Name {
			t.Errorf("archive %d: borge says name %q, borg %q", i, g.Name, w.Name)
		}
		if hex.EncodeToString(g.ID) != w.ID {
			t.Errorf("archive %q: borge says id %s, borg %s", w.Name, hex.EncodeToString(g.ID), w.ID)
		}
		if !g.Exists {
			t.Errorf("archive %q: borge thinks it does not exist (%s)", w.Name, g.Problem)
		}
		if g.Host != w.Hostname {
			t.Errorf("archive %q: borge says host %q, borg %q", w.Name, g.Host, w.Hostname)
		}
		if g.User != w.Username {
			t.Errorf("archive %q: borge says user %q, borg %q", w.Name, g.User, w.Username)
		}
		// borg's JSON renders the archive time in *local* time with an offset, while the
		// repository stores UTC. Compare the instants, not the spellings.
		wantTime, err := ParseTimestamp(w.Time)
		if err != nil {
			t.Fatalf("could not parse borg's timestamp %q: %v", w.Time, err)
		}
		if !g.Time.Equal(wantTime) {
			t.Errorf("archive %q: borge says time %s, borg %s", w.Name, g.Time, wantTime)
		}
	}
}

// TestBorgeReadsManifestFields checks the manifest itself rather than the directory.
func TestBorgeReadsManifestFields(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	r.createArchive("only")

	_, m := r.open(t)

	if m.Version() != 2 {
		t.Errorf("manifest version is %d, want 2", m.Version())
	}
	if _, err := m.LastTimestamp(); err != nil {
		t.Errorf("the manifest timestamp does not parse: %v", err)
	}
	// item_keys is the one config entry that matters, and it must be a superset of the
	// required keys or an item stream could not be decoded.
	have := map[string]bool{}
	for _, k := range m.ItemKeys {
		have[k] = true
	}
	for _, required := range []string{"path", "mtime", "chunks", "mode", "user", "group"} {
		if !have[required] {
			t.Errorf("item_keys is missing %q", required)
		}
	}
	if len(m.MandatoryFeatures()) != 0 {
		t.Errorf("borg declared mandatory features borge did not expect: %v", m.MandatoryFeatures())
	}
}

// TestBorgReadsBorgeRewrittenManifest is the write direction: borge rewrites the manifest
// and borg must still open the repository and see the same archives.
func TestBorgReadsBorgeRewrittenManifest(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	for _, name := range []string{"alpha", "beta"} {
		r.createArchive(name)
	}
	before := r.repoList()

	repo, err := repository.Open(location.MustLocal(r.path), repository.Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := repo.Unlock(r.passphrase)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(repo, k, OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	oldTimestamp := m.Timestamp
	if err := m.Write(); err != nil {
		t.Fatalf("borge could not write the manifest: %v", err)
	}
	if m.Timestamp <= oldTimestamp {
		t.Errorf("the manifest timestamp did not advance: %q -> %q", oldTimestamp, m.Timestamp)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	after := r.repoList()
	if len(after) != len(before) {
		t.Fatalf("after borge rewrote the manifest borg sees %d archives, was %d", len(after), len(before))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Name != after[i].Name {
			t.Errorf("archive %d changed: %+v -> %+v", i, before[i], after[i])
		}
	}
	if _, err := r.run("check", "-r", r.path); err != nil {
		t.Errorf("borg check failed after borge rewrote the manifest: %v", err)
	}
}

// TestArchiveMatchingAgreesWithBorg: the selectors borge implements have to pick the same
// archives borg's do, because the same syntax is used to delete them.
func TestArchiveMatchingAgreesWithBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	for _, name := range []string{"daily-1", "daily-2", "weekly-1"} {
		r.createArchive(name)
	}

	_, m := r.open(t)

	cases := []struct{ match, borgArg string }{
		{"daily-1", "daily-1"},
		{"sh:daily-*", "sh:daily-*"},
		{"re:^weekly", "re:^weekly"},
		{"sh:*-1", "sh:*-1"},
	}
	for _, tc := range cases {
		t.Run(tc.match, func(t *testing.T) {
			got, err := m.Archives.List(ListOptions{Match: []string{tc.match}})
			if err != nil {
				t.Fatal(err)
			}
			var gotNames []string
			for _, info := range got {
				gotNames = append(gotNames, info.Name)
			}
			sort.Strings(gotNames)

			var wantNames []string
			for _, a := range r.repoList("-a", tc.borgArg) {
				wantNames = append(wantNames, a.Name)
			}
			sort.Strings(wantNames)

			if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
				t.Errorf("borge selected %v, borg %v", gotNames, wantNames)
			}
		})
	}
}

// TestSoftDeleteMatchesBorg: a deleted archive disappears from the listing but is still
// findable as deleted, and borg agrees on both.
func TestSoftDeleteMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	for _, name := range []string{"keep", "drop"} {
		r.createArchive(name)
	}

	repo, err := repository.Open(location.MustLocal(r.path), repository.Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := repo.Unlock(r.passphrase)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Load(repo, k, OpDelete)
	if err != nil {
		t.Fatal(err)
	}
	victim, err := m.Archives.ByName("drop")
	if err != nil || victim == nil {
		t.Fatalf("borge could not find the archive to delete: %v", err)
	}
	if err := m.Archives.Delete(victim.ID); err != nil {
		t.Fatal(err)
	}

	live, err := m.Archives.Names()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(live, ",") != "keep" {
		t.Errorf("after a soft delete borge lists %v, want [keep]", live)
	}
	deleted, err := m.Archives.IDs(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || hex.EncodeToString(deleted[0]) != hex.EncodeToString(victim.ID) {
		t.Errorf("the deleted archive is not listed as deleted: %v", deleted)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	// borg sees the same thing.
	names := []string{}
	for _, a := range r.repoList() {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "keep" {
		t.Errorf("borg lists %v after borge's soft delete, want [keep]", names)
	}

	// And borge can put it back.
	repo, err = repository.Open(location.MustLocal(r.path), repository.Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	k, _, err = repo.Unlock(r.passphrase)
	if err != nil {
		t.Fatal(err)
	}
	m, err = Load(repo, k, OpDelete)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Archives.Undelete(victim.ID); err != nil {
		t.Fatal(err)
	}
	live, err = m.Archives.Names()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(live)
	if strings.Join(live, ",") != "drop,keep" {
		t.Errorf("after an undelete borge lists %v, want [drop keep]", live)
	}
}
