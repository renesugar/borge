// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The debug dumps are compared against borg's byte for byte rather than field by field.
//
// That is the whole point of them: they exist to be diffed when the two tools disagree
// about an object, and a dump that needs normalising before it can be compared is a dump
// that cannot do that job. A byte comparison is also a much stronger test - "both parse to
// equal objects" would still pass if borge rendered an integer where borg rendered a
// string, or lost a field's byte-vs-text distinction.

// makeDumpSource builds a tree that exercises every rendering the dumps have to get right.
//
// Each entry is here for a specific reason, listed at the entry. A dump comparison over a
// tree of plain ASCII files would pass without testing any of the hard parts.
func makeDumpSource(t *testing.T) string {
	t.Helper()
	src := t.TempDir()

	write := func(name, content string) string {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A file with content: its item carries a chunk list, whose ids are byte strings and
	// so exercise the U+007F hex marker.
	plain := write("plain.txt", strings.Repeat("some content ", 40))
	// An empty file: no chunk list at all, which is a different item shape.
	write("empty.txt", "")
	// A non-ASCII but valid UTF-8 name: every character above 0x7e has to come out as a
	// \uXXXX escape, and the CJK ones sit above the BMP boundary logic.
	write("café-日本.txt", "unicode name")
	// A name that is not UTF-8 at all, which Linux allows. borg's msgpack decodes it with
	// surrogateescape and json then writes lone surrogates; if borge replaced or dropped
	// the bad bytes instead, this is the only entry that would notice.
	write("bad\xff\xfename.txt", "invalid utf-8 name")
	// A symlink: the target is a separate string field.
	if err := os.Symlink("plain.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	// A hard link: both items carry an hlid, which is a byte string and always hex.
	if err := os.Link(plain, filepath.Join(src, "hardlink.txt")); err != nil {
		t.Fatal(err)
	}
	// A directory, so the dump has an item with no size and no chunks.
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("sub/nested.txt", "nested")
	// An xattr whose value is not text: the xattrs map is the one place where a *value*
	// takes the hex branch, and a text-only xattr would not test it.
	if err := setTestXattr(plain, "user.binary", "\x00\x01\x02\xff\xfe"); err != nil {
		t.Fatalf("cannot set an xattr on the test tree: %v", err)
	}
	return src
}

// dumpRepo builds a repository holding one archive of makeDumpSource's tree.
func dumpRepo(t *testing.T) (*borgRepo, string) {
	t.Helper()
	r := newBorgRepo(t, "aes256-ocb")
	src := makeDumpSource(t)
	// Created from inside the tree so the archived paths are relative, which is what makes
	// the odd names appear as leading path components in the dump.
	t.Chdir(src)
	r.mustRun("create", "-r", r.path, "dumped", ".")
	return r, src
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDebugDumpArchiveMatchesBorg is the central test of pydump.go.
func TestDebugDumpArchiveMatchesBorg(t *testing.T) {
	r, _ := dumpRepo(t)
	out := t.TempDir()
	wantPath := filepath.Join(out, "borg.json")
	gotPath := filepath.Join(out, "borge.json")

	r.mustRun("debug", "dump-archive", "-r", r.path, "dumped", wantPath)
	if _, stderr, code := r.borge(t, "debug", "dump-archive", "dumped", gotPath); code != ExitOK {
		t.Fatalf("borge debug dump-archive exited %d\n%s", code, stderr)
	}
	want := readFileString(t, wantPath)

	// Without this the comparison could pass on a dump that exercises none of the
	// interesting cases - which is exactly what a tree of ASCII files would produce.
	for _, probe := range []struct{ what, needle string }{
		{"a hex-marked byte string (chunk id or hlid)", `\u007f`},
		{"a surrogate escape from the non-UTF-8 name", `\udcff`},
		{"a \\uXXXX escape from the non-ASCII name", `\u00e9`},
		{"a hard link's hlid", `"hlid"`},
		{"a symlink target", `"target"`},
		{"a binary xattr value", `"user.binary"`},
		{"the archive metadata block", `"_meta"`},
		{"the manifest entry block", `"_manifest_entry"`},
	} {
		if !strings.Contains(want, probe.needle) {
			t.Fatalf("borg's own dump has no %s (%s), so comparing against it proves nothing:\n%s",
				probe.what, probe.needle, want)
		}
	}

	got := readFileString(t, gotPath)
	if got != want {
		t.Errorf("borge's dump differs from borg's:\n%s", firstDifference(want, got))
	}
}

// TestDebugDumpManifestMatchesBorg.
func TestDebugDumpManifestMatchesBorg(t *testing.T) {
	r, _ := dumpRepo(t)
	out := t.TempDir()
	wantPath := filepath.Join(out, "borg.json")
	gotPath := filepath.Join(out, "borge.json")

	r.mustRun("debug", "dump-manifest", "-r", r.path, wantPath)
	if _, stderr, code := r.borge(t, "debug", "dump-manifest", gotPath); code != ExitOK {
		t.Fatalf("borge debug dump-manifest exited %d\n%s", code, stderr)
	}

	want := readFileString(t, wantPath)
	if !strings.Contains(want, `"item_keys"`) || !strings.Contains(want, `"timestamp"`) {
		t.Fatalf("borg's manifest dump is missing the fields this compares:\n%s", want)
	}
	// json.dump writes no trailing newline, and a dump that gained one would not diff
	// cleanly against borg's.
	if strings.HasSuffix(want, "\n") {
		t.Fatalf("borg's manifest dump now ends with a newline; borge's does not")
	}

	if got := readFileString(t, gotPath); got != want {
		t.Errorf("borge's manifest dump differs from borg's:\n%s", firstDifference(want, got))
	}
}

// TestDebugDumpArchiveItemsMatchesBorg: the files are the item stream as stored, so they
// have to be identical, name and content alike.
func TestDebugDumpArchiveItemsMatchesBorg(t *testing.T) {
	r, _ := dumpRepo(t)

	byBorg := dumpIntoDir(t, func(dir string) {
		t.Chdir(dir)
		r.mustRun("debug", "dump-archive-items", "-r", r.path, "dumped")
	})
	byBorge := dumpIntoDir(t, func(dir string) {
		t.Chdir(dir)
		if _, stderr, code := r.borge(t, "debug", "dump-archive-items", "dumped"); code != ExitOK {
			t.Fatalf("borge debug dump-archive-items exited %d\n%s", code, stderr)
		}
	})

	if len(byBorg) == 0 {
		t.Fatal("borg dumped no item stream chunks, so there is nothing to compare")
	}
	compareDumpDirs(t, byBorg, byBorge)
}

// TestDebugDumpRepoObjsMatchesBorg.
func TestDebugDumpRepoObjsMatchesBorg(t *testing.T) {
	r, _ := dumpRepo(t)

	byBorg := dumpIntoDir(t, func(dir string) {
		t.Chdir(dir)
		r.mustRun("debug", "dump-repo-objs", "-r", r.path)
	})
	byBorge := dumpIntoDir(t, func(dir string) {
		t.Chdir(dir)
		if _, stderr, code := r.borge(t, "debug", "dump-repo-objs"); code != ExitOK {
			t.Fatalf("borge debug dump-repo-objs exited %d\n%s", code, stderr)
		}
	})

	// The archive holds several files, a metadata stream and an archive object, so a
	// handful of objects at least. One or two would mean something went wrong earlier.
	if len(byBorg) < 4 {
		t.Fatalf("borg dumped only %d object(s); the comparison would be trivial", len(byBorg))
	}
	compareDumpDirs(t, byBorg, byBorge)
}

// dumpIntoDir runs a dumping command in a fresh directory and returns what it wrote.
func dumpIntoDir(t *testing.T, run func(dir string)) map[string]string {
	t.Helper()
	dir := t.TempDir()
	run(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		out[e.Name()] = readFileString(t, filepath.Join(dir, e.Name()))
	}
	return out
}

func compareDumpDirs(t *testing.T, want, got map[string]string) {
	t.Helper()
	var missing, extra []string
	for name := range want {
		if _, ok := got[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("borge did not write %d file(s) borg did: %v", len(missing), missing)
	}
	if len(extra) > 0 {
		t.Errorf("borge wrote %d file(s) borg did not: %v", len(extra), extra)
	}
	for name, w := range want {
		if g, ok := got[name]; ok && g != w {
			t.Errorf("%s: borge wrote %d bytes, borg %d, and they differ", name, len(g), len(w))
		}
	}
}

// findingRe pulls the object id and hit count out of a search finding line, which is the
// part of it that is a fact about the repository rather than about iteration order.
var findingRe = regexp.MustCompile(`^\d+ ([0-9a-f]{64}) #(\d+):`)

// TestDebugSearchRepoObjsMatchesBorg.
//
// The full lines cannot be compared: they start with the position in a scan whose order is
// the chunk index's, and the two implementations are free to differ there. What has to
// agree is which objects contain the term and how often.
func TestDebugSearchRepoObjsMatchesBorg(t *testing.T) {
	r, _ := dumpRepo(t)

	hits := func(out string) []string {
		var found []string
		for _, line := range strings.Split(out, "\n") {
			if m := findingRe.FindStringSubmatch(line); m != nil {
				found = append(found, m[1]+" #"+m[2])
			}
		}
		sort.Strings(found)
		return found
	}

	want := hits(r.mustRun("debug", "search-repo-objs", "-r", r.path, "str:some content"))
	stdout, stderr, code := r.borge(t, "debug", "search-repo-objs", "str:some content")
	if code != ExitOK {
		t.Fatalf("borge debug search-repo-objs exited %d\n%s", code, stderr)
	}
	got := hits(stdout)

	if len(want) == 0 {
		t.Fatal("borg found the search term in no object at all, so this compares nothing")
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("borge found %v, borg found %v", got, want)
	}
	if !strings.Contains(stdout, "Done.") {
		t.Errorf("borge did not report that the scan finished:\n%s", stdout)
	}

	// A term in neither tool's repository has to be reported as nothing found, not as an
	// error - a search that exits non-zero when it finds nothing is unusable in a script.
	stdout, _, code = r.borge(t, "debug", "search-repo-objs", "str:certainly-not-in-this-repository")
	if code != ExitOK {
		t.Errorf("a search with no hits exited %d, want ExitOK", code)
	}
	if len(hits(stdout)) != 0 {
		t.Errorf("a search for an absent term found something:\n%s", stdout)
	}
}

// TestDebugSearchRepoObjsRejectsUnprefixedTerms: "borg debug search-repo-objs foo" is an
// error in both tools, because a bare term is ambiguous between text and hex.
func TestDebugSearchRepoObjsRejectsUnprefixedTerms(t *testing.T) {
	r, _ := dumpRepo(t)
	for _, term := range []string{"content", "hex:zz", "str:", "hex:"} {
		_, stderr, code := r.borge(t, "debug", "search-repo-objs", term)
		if code != ExitError {
			t.Errorf("search term %q exited %d, want ExitError (%d)", term, code, ExitError)
		}
		if !strings.Contains(stderr, "hex:") {
			t.Errorf("search term %q: the error does not say what a term looks like: %q", term, stderr)
		}
	}
}

// TestDebugObjectsRoundTripBetweenTools walks the whole object toolchain across the two
// implementations: id-hash, get-obj, parse-obj, format-obj, put-obj, delete-obj.
//
// Each half is checked against the other tool rather than against itself, because a pair
// of borge commands that agree only with each other would pass every self-consistency test
// while writing objects borg cannot read.
func TestDebugObjectsRoundTripBetweenTools(t *testing.T) {
	r, _ := dumpRepo(t)
	work := t.TempDir()

	// A plaintext, and the id both tools give it.
	content := filepath.Join(work, "content")
	if err := os.WriteFile(content, []byte("a plaintext for the object round trip"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantID := strings.TrimSpace(r.mustRun("debug", "id-hash", "-r", r.path, content))
	stdout, stderr, code := r.borge(t, "debug", "id-hash", content)
	if code != ExitOK {
		t.Fatalf("borge debug id-hash exited %d\n%s", code, stderr)
	}
	gotID := strings.TrimSpace(stdout)
	if len(wantID) != 64 {
		t.Fatalf("borg's id-hash is %q, which is not a chunk id", wantID)
	}
	if gotID != wantID {
		t.Fatalf("borge hashed the file to %s, borg to %s", gotID, wantID)
	}

	// An object that is already in the repository: both tools have to fetch the same bytes
	// and split it into the same plaintext and metadata.
	existingID := anyChunkID(t, r)
	borgObj := filepath.Join(work, "borg.obj")
	borgeObj := filepath.Join(work, "borge.obj")
	r.mustRun("debug", "get-obj", "-r", r.path, existingID, borgObj)
	if _, stderr, code := r.borge(t, "debug", "get-obj", existingID, borgeObj); code != ExitOK {
		t.Fatalf("borge debug get-obj exited %d\n%s", code, stderr)
	}
	if readFileString(t, borgObj) != readFileString(t, borgeObj) {
		t.Fatal("the two tools fetched different bytes for the same object")
	}

	r.mustRun("debug", "parse-obj", "-r", r.path, existingID, borgObj,
		filepath.Join(work, "borg.data"), filepath.Join(work, "borg.meta.json"))
	if _, stderr, code := r.borge(t, "debug", "parse-obj", existingID, borgObj,
		filepath.Join(work, "borge.data"), filepath.Join(work, "borge.meta.json")); code != ExitOK {
		t.Fatalf("borge debug parse-obj exited %d\n%s", code, stderr)
	}
	wantMeta := readFileString(t, filepath.Join(work, "borg.meta.json"))
	if !strings.Contains(wantMeta, `"ctype"`) {
		t.Fatalf("borg's object metadata does not look like metadata: %q", wantMeta)
	}
	if got := readFileString(t, filepath.Join(work, "borge.meta.json")); got != wantMeta {
		t.Errorf("object metadata differs\n  borge: %s\n  borg:  %s", got, wantMeta)
	}
	if readFileString(t, filepath.Join(work, "borge.data")) != readFileString(t, filepath.Join(work, "borg.data")) {
		t.Error("the two tools decoded the object to different plaintexts")
	}

	// borge formats an object; borg has to be able to parse it back. The object bytes
	// themselves cannot be compared - an AEAD mode picks a fresh nonce every time - so the
	// check is that the plaintext survives the trip through the other tool.
	metaJSON := filepath.Join(work, "new.meta.json")
	if err := os.WriteFile(metaJSON, []byte(`{"type": "F"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	newObj := filepath.Join(work, "new.obj")
	if _, stderr, code := r.borge(t, "debug", "format-obj", wantID, content, metaJSON, newObj); code != ExitOK {
		t.Fatalf("borge debug format-obj exited %d\n%s", code, stderr)
	}
	r.mustRun("debug", "parse-obj", "-r", r.path, wantID, newObj,
		filepath.Join(work, "back.data"), filepath.Join(work, "back.meta.json"))
	if readFileString(t, filepath.Join(work, "back.data")) != readFileString(t, content) {
		t.Error("borg parsed a borge-formatted object into the wrong plaintext")
	}

	// borge stores it; borg fetches it back and the repository still checks out.
	if _, stderr, code := r.borge(t, "debug", "put-obj", wantID, newObj); code != ExitOK {
		t.Fatalf("borge debug put-obj exited %d\n%s", code, stderr)
	}
	fetched := filepath.Join(work, "fetched.obj")
	r.mustRun("debug", "get-obj", "-r", r.path, wantID, fetched)
	if readFileString(t, fetched) != readFileString(t, newObj) {
		t.Error("borg fetched back something other than what borge put")
	}
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check fails after borge put an object: %v\n%s", err, out)
	}

	// And borge removes it again. The object was never referenced by an archive, so the
	// repository has to be intact afterwards - which is what distinguishes a delete that
	// rewrote the pack correctly from one that dropped its neighbours too.
	if _, stderr, code := r.borge(t, "debug", "delete-obj", wantID); code != ExitOK {
		t.Fatalf("borge debug delete-obj exited %d\n%s", code, stderr)
	}
	if out, err := r.runErr("debug", "get-obj", "-r", r.path, wantID, filepath.Join(work, "gone.obj")); err == nil {
		t.Errorf("borg still finds the object borge deleted:\n%s", out)
	}
	if out, err := r.runErr("check", "--verify-data", "-r", r.path); err != nil {
		t.Fatalf("borg check fails after borge deleted an unreferenced object: %v\n%s", err, out)
	}
	// The archive that shares the pack must still extract, which is the failure a
	// pack-rewriting delete would cause and a check might not.
	if out, err := r.runErr("list", "-r", r.path, "dumped"); err != nil {
		t.Fatalf("the archive is unreadable after a delete-obj: %v\n%s", err, out)
	}
}

// anyChunkID returns the id of some object in the repository, via borg, so the test does
// not depend on borge to pick its own subject.
func anyChunkID(t *testing.T, r *borgRepo) string {
	t.Helper()
	dumped := dumpIntoDir(t, func(dir string) {
		t.Chdir(dir)
		r.mustRun("debug", "dump-repo-objs", "-r", r.path)
	})
	var names []string
	for name := range dumped {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("the repository holds no objects")
	}
	return strings.TrimSuffix(names[0], ".obj")
}

// TestDebugDeleteObjKeepsThePackNeighbours.
//
// A repository object lives inside a pack holding many others, so deleting one means
// rewriting its pack without it. Everything else in that pack has to come out the other
// side byte-identical - and the round-trip test above cannot show that, because the object
// it deletes was put there by itself and so has no neighbours to lose.
//
// The failure this guards against is not subtle in its effect and is completely silent at
// the time: a delete that dropped the rest of the pack would take an archive's files with
// it, and nothing would say so until a restore.
func TestDebugDeleteObjKeepsThePackNeighbours(t *testing.T) {
	r, _ := dumpRepo(t)

	dump := func() map[string]string {
		return dumpIntoDir(t, func(dir string) {
			t.Chdir(dir)
			r.mustRun("debug", "dump-repo-objs", "-r", r.path)
		})
	}
	before := dump()

	// The point of the test is the neighbours, so the victim has to have some. Which pack
	// an object is in is worked out from the bytes on disk rather than from borge's chunk
	// index, so the test does not take the implementation's word for the very thing it is
	// checking.
	packs := packMembership(t, r, before)
	var victim string
	var neighbours int
	for _, members := range packs {
		if len(members) > neighbours {
			neighbours = len(members)
			victim = members[0]
		}
	}
	if neighbours < 3 {
		t.Fatalf("the fullest pack holds %d object(s); deleting one would leave too few "+
			"neighbours to prove anything", neighbours)
	}

	if _, stderr, code := r.borge(t, "debug", "delete-obj", victim); code != ExitOK {
		t.Fatalf("borge debug delete-obj exited %d\n%s", code, stderr)
	}

	after := dump()
	if _, ok := after[victim+".obj"]; ok {
		t.Errorf("the object %s survived its own deletion", victim)
	}
	for name, content := range before {
		if name == victim+".obj" {
			continue
		}
		got, ok := after[name]
		if !ok {
			t.Errorf("%s was lost when its pack was rewritten", name)
			continue
		}
		if got != content {
			t.Errorf("%s changed when its pack was rewritten: %d bytes, was %d",
				name, len(got), len(content))
		}
	}
}

// packMembership works out which pack each object is stored in, from the files on disk.
//
// An object is a contiguous run of bytes inside its pack and its header carries its own
// chunk id, so the stored form is unique and searching for it is unambiguous. The stored
// form comes from borg's get-obj, so nothing here depends on borge.
func packMembership(t *testing.T, r *borgRepo, objects map[string]string) map[string][]string {
	t.Helper()
	work := t.TempDir()

	var packFiles []string
	err := filepath.WalkDir(filepath.Join(r.path, "packs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			packFiles = append(packFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packFiles) == 0 {
		t.Fatal("the repository has no pack files")
	}

	contents := make(map[string][]byte, len(packFiles))
	for _, p := range packFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		contents[p] = b
	}

	var ids []string
	for name := range objects {
		ids = append(ids, strings.TrimSuffix(name, ".obj"))
	}
	sort.Strings(ids)

	membership := map[string][]string{}
	for _, id := range ids {
		out := filepath.Join(work, id)
		r.mustRun("debug", "get-obj", "-r", r.path, id, out)
		stored, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for p, b := range contents {
			if bytes.Contains(b, stored) {
				membership[p] = append(membership[p], id)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("object %s is not in any pack file", id)
		}
	}
	return membership
}

// TestDebugDeleteObjReportsWhatItCouldNotDelete.
//
// borg prints "not found" and exits 0. borge prints the same and exits with the warning
// code, because a script that deletes a list of ids has no other way to learn that some of
// them were not there. See docs/DIVERGENCES.md.
func TestDebugDeleteObjReportsWhatItCouldNotDelete(t *testing.T) {
	r, _ := dumpRepo(t)
	absent := strings.Repeat("ab", 32)

	stdout, _, code := r.borge(t, "debug", "delete-obj", absent)
	if code != ExitWarning {
		t.Errorf("deleting an absent object exited %d, want ExitWarning (%d)", code, ExitWarning)
	}
	if !strings.Contains(stdout, "not found") {
		t.Errorf("borge did not say the object was absent:\n%s", stdout)
	}

	stdout, _, code = r.borge(t, "debug", "delete-obj", "not-hex")
	if code != ExitWarning {
		t.Errorf("deleting an unreadable id exited %d, want ExitWarning (%d)", code, ExitWarning)
	}
	if !strings.Contains(stdout, "invalid") {
		t.Errorf("borge did not say the id was unreadable:\n%s", stdout)
	}

	// The rest of the list is still processed: one bad id must not abandon the others.
	if !strings.Contains(stdout, "Done.") {
		t.Errorf("borge stopped at the bad id instead of finishing:\n%s", stdout)
	}
}

// TestDebugRejectsShortObjectIDs: a prefix is not accepted where a full id is meant.
//
// These commands write. Resolving "4a9c" to whichever object happens to start with it
// would make a typo destructive.
func TestDebugRejectsShortObjectIDs(t *testing.T) {
	r, _ := dumpRepo(t)
	work := t.TempDir()
	short := "4a9cd8a3"

	_, stderr, code := r.borge(t, "debug", "get-obj", short, filepath.Join(work, "out"))
	if code != ExitError {
		t.Errorf("get-obj with a short id exited %d, want ExitError (%d)", code, ExitError)
	}
	if !strings.Contains(stderr, "invalid") {
		t.Errorf("the error does not say the id is invalid: %q", stderr)
	}
}

// TestDebugUsage: an unknown subcommand is an error that lists the real ones, rather than
// a silent success.
func TestDebugUsage(t *testing.T) {
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}

	if code := Run(e, []string{"debug"}); code != ExitOK {
		t.Errorf("bare 'debug' exited %d, want ExitOK", code)
	}
	for _, name := range []string{"dump-archive", "put-obj", "id-hash"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("the debug usage does not list %q:\n%s", name, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := Run(e, []string{"debug", "no-such-thing"}); code != ExitError {
		t.Errorf("an unknown debug command exited %d, want ExitError", code)
	}
	if !strings.Contains(errOut.String(), "no-such-thing") {
		t.Errorf("the error does not name the unknown command:\n%s", errOut.String())
	}
}

// TestDebugInfoReportsTheBuild.
func TestDebugInfoReportsTheBuild(t *testing.T) {
	var out, errOut strings.Builder
	e := &Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) (string, bool) { return "", false }}
	if code := Run(e, []string{"debug", "info"}); code != ExitOK {
		t.Fatalf("debug info exited %d\n%s", code, errOut.String())
	}
	// The upstream commit is the fact that decides whether a given repository is readable,
	// so a bug report without it is not much use.
	for _, needle := range []string{"Platform:", "Borge:", "PID:", "Process ID:", "114bd1e9"} {
		if !strings.Contains(out.String(), needle) {
			t.Errorf("debug info does not report %q:\n%s", needle, out.String())
		}
	}
}

// firstDifference reports where two dumps diverge, since printing two megabyte documents
// side by side helps nobody.
func firstDifference(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] != gotLines[i] {
			return fmt.Sprintf("line %d:\n  borg:  %s\n  borge: %s", i+1, wantLines[i], gotLines[i])
		}
	}
	if len(wantLines) != len(gotLines) {
		return fmt.Sprintf("borg wrote %d lines, borge %d", len(wantLines), len(gotLines))
	}
	return "the documents differ but no line does; check the trailing bytes"
}
