// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// diffPaths returns the paths borge's diff reports as changed, from its JSON output.
func diffPaths(t *testing.T, r *borgRepo, a, b string, extra ...string) map[string][]string {
	t.Helper()
	args := append([]string{"diff", "-json-lines"}, extra...)
	args = append(args, a, b)
	stdout, stderr, code := r.borge(t, args...)
	if code != ExitOK {
		t.Fatalf("borge diff exited %d\n%s", code, stderr)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d struct {
			Path    string `json:"path"`
			Changes []struct {
				// borg's key is "type", and its values are borg's phrases; borge
				// matched neither until 2026-08-19 (DIVERGENCES.md #43).
				Kind string `json:"type"`
			} `json:"changes"`
		}
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("borge diff JSON does not parse: %v\n%s", err, line)
		}
		var kinds []string
		for _, c := range d.Changes {
			kinds = append(kinds, c.Kind)
		}
		sort.Strings(kinds)
		out[filepath.Base(d.Path)] = kinds
	}
	return out
}

// TestDiffMatchesBorg: the set of paths borge reports as changed has to be the set borg
// reports. A diff that misses a change is a user believing two archives agree when they
// do not.
func TestDiffMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}

	write("unchanged.txt", "same in both", 0o644)
	write("edited.txt", "the first version", 0o644)
	write("removed.txt", "only in the first", 0o644)
	write("mode-change.txt", "same content", 0o644)
	if err := os.Symlink("unchanged.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, code := r.borge(t, "create", "first", src); code != ExitOK {
		t.Fatal("create first failed")
	}

	write("edited.txt", "the second version, which is longer", 0o644)
	if err := os.Remove(filepath.Join(src, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	write("added.txt", "only in the second", 0o644)
	if err := os.Chmod(filepath.Join(src, "mode-change.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("added.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, code := r.borge(t, "create", "second", src); code != ExitOK {
		t.Fatal("create second failed")
	}

	got := diffPaths(t, r, "first", "second")

	// borg's own answer, as the set of paths it names.
	out := r.mustRun("diff", "-r", r.path, "first", "second")
	want := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		want[filepath.Base(fields[len(fields)-1])] = true
	}

	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("borg reports %s as changed, borge does not", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("borge reports %s as changed, borg does not", name)
		}
	}
	if _, ok := got["unchanged.txt"]; ok {
		t.Error("an unchanged file was reported as changed")
	}

	// And the kinds are right, not just the paths. These are borg's names for them: a
	// content change is the bare word, everything else is the phrase.
	for name, wantKind := range map[string]string{
		"edited.txt":      "modified",
		"removed.txt":     "removed",
		"added.txt":       "added",
		"mode-change.txt": "changed mode",
		"link":            "changed link",
	} {
		kinds, ok := got[name]
		if !ok {
			t.Errorf("%s was not reported at all", name)
			continue
		}
		found := false
		for _, k := range kinds {
			if k == wantKind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: borge reports %v, expected a %q change", name, kinds, wantKind)
		}
	}
	t.Logf("borge reported %d changed path(s): %v", len(got), got)
}

// TestDiffOfIdenticalArchives: two archives of an unchanged tree must produce no output at
// all. A diff that reports spurious changes is as useless as one that misses them.
func TestDiffOfIdenticalArchives(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for i := 0; i < 5; i++ {
		p := filepath.Join(src, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 1000+i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a", "b"} {
		if _, _, code := r.borge(t, "create", name, src); code != ExitOK {
			t.Fatalf("create %s failed", name)
		}
	}

	stdout, stderr, code := r.borge(t, "diff", "a", "b")
	if code != ExitOK {
		t.Fatalf("diff exited %d\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("two archives of an unchanged tree differ:\n%s", stdout)
	}
}

// TestExportTarReadableByTar: the whole point of an export is that something other than
// borge can read it, so the check is GNU tar's own listing plus a full extraction.
func TestExportTarReadableByTar(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dir/nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p := filepath.Join(src, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte(strings.Repeat("content ", 100+i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "dir/nested/deep.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f0.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "f0.txt"), filepath.Join(src, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, _, code := r.borge(t, "create", "tree", src); code != ExitOK {
		t.Fatal("create failed")
	}

	tarPath := filepath.Join(t.TempDir(), "out.tar")
	if _, stderr, code := r.borge(t, "export-tar", "tree", tarPath); code != ExitOK {
		t.Fatalf("export-tar exited %d\n%s", code, stderr)
	}

	// GNU tar reads it.
	listing, err := exec.Command("tar", "-tf", tarPath).CombinedOutput()
	if err != nil {
		t.Fatalf("tar could not read borge's export: %v\n%s", err, listing)
	}
	if !strings.Contains(string(listing), "f0.txt") {
		t.Errorf("the export does not list the files:\n%s", listing)
	}

	// And extracting it reproduces the contents.
	dest := t.TempDir()
	if out, err := exec.Command("tar", "-xf", tarPath, "-C", dest).CombinedOutput(); err != nil {
		t.Fatalf("tar could not extract borge's export: %v\n%s", err, out)
	}
	restored := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(src), "/")))
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("f%d.txt", i)
		want, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s missing from the tar extraction: %v", name, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s differs after a tar round trip", name)
		}
	}

	// The hard link must be a tar link entry, not a second copy: that is the difference
	// between an export that preserves the relationship and one that doubles the size.
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var links, symlinks, dirs int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Typeflag {
		case tar.TypeLink:
			links++
		case tar.TypeSymlink:
			symlinks++
		case tar.TypeDir:
			dirs++
		}
	}
	if links != 1 {
		t.Errorf("the export has %d tar hard link entries, want 1", links)
	}
	if symlinks != 1 {
		t.Errorf("the export has %d symlink entries, want 1", symlinks)
	}
	if dirs == 0 {
		t.Error("the export has no directory entries")
	}
}

// TestExportTarCarriesXAttrsAndACLs checks the PAX records, which are the reason PAX is
// the default format.
func TestExportTarCarriesXAttrsAndACLs(t *testing.T) {
	r := newBorgRepo(t, "none-sha256")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	target := filepath.Join(src, "attrs.txt")
	if err := os.WriteFile(target, []byte("has attributes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setTestXattr(target, "user.borge", "an attribute"); err != nil {
		t.Skipf("extended attributes are not available here: %v", err)
	}
	if _, _, code := r.borge(t, "create", "attrs", src); code != ExitOK {
		t.Fatal("create failed")
	}

	tarPath := filepath.Join(t.TempDir(), "out.tar")
	if _, stderr, code := r.borge(t, "export-tar", "attrs", tarPath); code != ExitOK {
		t.Fatalf("export-tar exited %d\n%s", code, stderr)
	}

	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(hdr.Name, "attrs.txt") {
			if v, ok := hdr.PAXRecords["SCHILY.xattr.user.borge"]; !ok || v != "an attribute" {
				t.Errorf("the xattr is not in the PAX records: %v", hdr.PAXRecords)
			}
			found = true
		}
	}
	if !found {
		t.Error("attrs.txt is not in the export")
	}

	// The GNU format cannot carry them, and has to say so rather than dropping them
	// silently.
	gnuPath := filepath.Join(t.TempDir(), "out-gnu.tar")
	_, stderr, code := r.borge(t, "export-tar", "-tar-format", "GNU", "attrs", gnuPath)
	if code != ExitWarning {
		t.Errorf("a GNU export of a file with xattrs exited %d, want a warning (%d)", code, ExitWarning)
	}
	if !strings.Contains(stderr, "extended attributes") {
		t.Errorf("the GNU export did not warn about the lost attributes: %q", stderr)
	}
}
