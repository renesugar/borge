// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The archive's "size" is stored in its metadata, not computed on read, so it is the one
// number here that travels: "borg info" on a borge-made archive reports whatever borge
// wrote. That makes it interop, not formatting, and it has to be borg's number even where
// borg's number is odd - it excludes the item metadata stream because of a rebound
// counter in borg's create (see the note on Builder.AddChunk and DIVERGENCES.md #36).
//
// borge's was wrong in both directions before 2026-08-18: the stored figure included the
// item stream, and the figure "create --stats" printed was sampled before the archive was
// saved, so it reported a different rule for a large backup than a small one.

// archiveSizes reads the stored size and file count of every archive in the repository.
func archiveSizes(t *testing.T, listing string) map[string][2]int64 {
	t.Helper()
	out := map[string][2]int64{}
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		nfiles, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = [2]int64{size, nfiles}
	}
	return out
}

// TestArchiveSizeMatchesBorg: the same tree, archived by each tool, records the same size.
func TestArchiveSizeMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	base := t.TempDir()

	// The shapes are chosen to separate the three things that could be counted: content
	// bytes, item count, and the item metadata stream. "many" has a large item stream and
	// almost no content, which is the case that fails loudly if the stream is counted.
	trees := map[string]func(dir string){
		"empty": func(dir string) {
			write(t, filepath.Join(dir, "empty"), "")
		},
		"many": func(dir string) {
			for i := 0; i < 400; i++ {
				write(t, filepath.Join(dir, fmt.Sprintf("file-with-a-longish-name-%d", i)), "")
			}
		},
		"large": func(dir string) {
			write(t, filepath.Join(dir, "big.bin"), strings.Repeat("borge", 40000))
		},
		"nested": func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "a", "one.txt"), "one")
			write(t, filepath.Join(dir, "a", "b", "two.txt"), strings.Repeat("two", 500))
			if err := os.Symlink("one.txt", filepath.Join(dir, "a", "link")); err != nil {
				t.Fatal(err)
			}
		},
	}

	names := []string{"empty", "many", "large", "nested"}
	for _, name := range names {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		trees[name](dir)
		r.mustRun("create", "-r", r.path, "borg-"+name, dir)
		if _, stderr, code := r.borge(t, "create", "borge-"+name, dir); code != ExitOK {
			t.Fatalf("borge create %s exited %d\n%s", name, code, stderr)
		}
	}

	got := archiveSizes(t, r.mustRun("repo-list", "-r", r.path,
		"--format", "{archive} {size} {nfiles}{NL}"))

	var distinct = map[int64]bool{}
	for _, name := range names {
		borg, ok := got["borg-"+name]
		if !ok {
			t.Fatalf("borg's %s archive is not in the listing: %v", name, got)
		}
		borge, ok := got["borge-"+name]
		if !ok {
			t.Fatalf("borge's %s archive is not in the listing: %v", name, got)
		}
		distinct[borg[0]] = true
		if borg != borge {
			t.Errorf("%s: borg recorded size=%d nfiles=%d, borge recorded size=%d nfiles=%d",
				name, borg[0], borg[1], borge[0], borge[1])
		}
	}

	// Four shapes that all recorded the same size would compare a constant with itself,
	// and a set of sizes that never exceeds the 35-byte floor would not exercise content
	// at all. Both are ways this test could pass while measuring nothing.
	if len(distinct) < 3 {
		t.Errorf("the four trees produced %d distinct sizes; they are not separating "+
			"content from item count: %v", len(distinct), got)
	}
	var largest int64
	for size := range distinct {
		if size > largest {
			largest = size
		}
	}
	if largest < 1000 {
		t.Errorf("the largest archive recorded %d bytes; no case has real content", largest)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCreateReportedSizeFollowsBorgsRule: what create reports is the stored size plus the
// archive object, and borg's rule is the same.
//
// The two figures cannot be compared between tools directly, because the archive object
// contains the command line and borge's is spelled differently from borg's (DIVERGENCES.md
// #12) - a 42-character difference in the string is a 43-byte difference in the archive
// object and so in the number. What can be compared is the *relationship*, and that is
// what was wrong: borge read its counter before Save, so the figure it printed was taken
// before the item pointers and the archive object existed. For a tree with many items it
// came out *below* the stored size, which is not a rule at all.
func TestCreateReportedSizeFollowsBorgsRule(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	base := t.TempDir()

	// Many items, almost no content: the shape that separates "counted the content" from
	// "counted whatever had been flushed by then".
	dir := filepath.Join(base, "many")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("file-with-a-longish-name-%d", i)), "")
	}

	reported := func(what, out string) int64 {
		t.Helper()
		var doc struct {
			Archive struct {
				Stats struct {
					OriginalSize int64 `json:"original_size"`
				} `json:"stats"`
			} `json:"archive"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%s did not produce JSON: %v\n%s", what, err, out)
		}
		return doc.Archive.Stats.OriginalSize
	}

	borgReported := reported("borg create --json",
		r.mustRun("create", "-r", r.path, "borg-many", dir, "--json"))
	stdout, stderr, code := r.borge(t, "create", "borge-many", dir, "--json")
	if code != ExitOK {
		t.Fatalf("borge create --json exited %d\n%s", code, stderr)
	}
	borgeReported := reported("borge create --json", stdout)

	stored := archiveSizes(t, r.mustRun("repo-list", "-r", r.path,
		"--format", "{archive} {size} {nfiles}{NL}"))

	for _, c := range []struct {
		name     string
		reported int64
	}{
		{"borg-many", borgReported},
		{"borge-many", borgeReported},
	} {
		size := stored[c.name][0]
		if size == 0 {
			t.Fatalf("%s recorded no size; the listing is not being read: %v", c.name, stored)
		}
		gap := c.reported - size
		switch {
		case gap <= 0:
			t.Errorf("%s reported %d with %d stored: the reported figure must include the "+
				"archive object and so exceed the stored one", c.name, c.reported, size)
		case gap > 4096:
			t.Errorf("%s reported %d with %d stored: a gap of %d is far larger than an "+
				"archive object, so something else is being counted", c.name, c.reported, size, gap)
		}
	}

	// 400 empty files: if either tool were counting the item metadata stream the stored
	// figure would be in the tens of thousands rather than the tens.
	if got := stored["borg-many"][0]; got > 1000 {
		t.Errorf("borg stored %d for 400 empty files; the premise of this test is wrong", got)
	}
}

// TestImportTarFileCountIsTruthful: the nfiles borge stores for an imported tar is the
// number of regular files in it.
//
// borg stores twice that - two counters increment for one file, see DIVERGENCES.md #38 -
// and this deliberately asserts borg's doubling too. The divergence is a decision that
// rests on borg being wrong; if upstream fixes it, this should fail and be revisited
// rather than quietly outliving its reason.
func TestImportTarFileCountIsTruthful(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 3
	for i := 0; i < files; i++ {
		write(t, filepath.Join(src, fmt.Sprintf("%d.txt", i)), strings.Repeat("x", i+1))
	}

	tarPath := filepath.Join(dir, "tree.tar")
	if _, stderr, code := r.borge(t, "create", "seed", src); code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "export-tar", "seed", tarPath); code != ExitOK {
		t.Fatalf("borge export-tar exited %d\n%s", code, stderr)
	}

	r.mustRun("import-tar", "-r", r.path, "borg-import", tarPath)
	if _, stderr, code := r.borge(t, "import-tar", "borge-import", tarPath); code != ExitOK {
		t.Fatalf("borge import-tar exited %d\n%s", code, stderr)
	}

	stored := archiveSizes(t, r.mustRun("repo-list", "-r", r.path,
		"--format", "{archive} {size} {nfiles}{NL}"))

	// Counted from the archive rather than from the constant above, so the test measures
	// what was actually imported.
	stdout, stderr, code := r.borge(t, "list", "borge-import", "--json-lines")
	if code != ExitOK {
		t.Fatalf("borge list exited %d\n%s", code, stderr)
	}
	var regular int64
	for _, it := range parseJSONLines(t, stdout) {
		if typ, _ := it["type"].(string); typ == "-" {
			regular++
		}
	}
	if regular != files {
		t.Fatalf("the imported archive holds %d regular files, expected %d; the tar is "+
			"not what this test assumes", regular, files)
	}

	if got := stored["borge-import"][1]; got != regular {
		t.Errorf("borge stored nfiles=%d for an archive holding %d regular files", got, regular)
	}
	if got := stored["borg-import"][1]; got != 2*regular {
		t.Errorf("borg stored nfiles=%d for %d regular files; it has always stored double "+
			"(DIVERGENCES.md #38). If upstream fixed this, revisit the divergence rather "+
			"than the test", got, regular)
	}
	// Sizes still have to agree - the divergence is about the count alone.
	if b, e := stored["borg-import"][0], stored["borge-import"][0]; b != e {
		t.Errorf("stored size differs: borg %d, borge %d", b, e)
	}
}

// TestRecreateSizeMatchesBorg: an archive recreated by each tool records the same size.
//
// borg's recreate counts the item metadata stream where its create does not (the counter
// its create folds away is still live on the recreate path), so the recorded size depends
// on which command wrote the archive. borge reproduced the create rule everywhere when
// that was first fixed, which matched borg on two paths and broke the third - nothing
// caught it but a manual check, hence this.
//
// Both archives are created by borg and differ only in which tool recreates them, so the
// comparison is of the recreaters alone. Recreating trees the two tools created separately
// would compare their item metadata as well: borg writes bsdflags and xattrs on every item
// and borge writes neither, which is a real difference of about 18 bytes an item and not
// what this test is about.
func TestRecreateSizeMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Many items and little content, so the item stream dominates the size and the two
	// rules are far apart rather than a few bytes apart.
	for i := 0; i < 300; i++ {
		write(t, filepath.Join(src, fmt.Sprintf("file-with-a-longish-name-%d", i)), "x")
	}

	r.mustRun("create", "-r", r.path, "by-borg", src)
	r.mustRun("create", "-r", r.path, "by-borge", src)

	before := archiveSizes(t, r.mustRun("repo-list", "-r", r.path,
		"--format", "{archive} {size} {nfiles}{NL}"))

	const params = "fastcdc,18,22,20,2"
	r.mustRun("recreate", "-r", r.path, "-a", "by-borg", "--chunker-params", params)
	if _, stderr, code := r.borge(t, "recreate", "-a", "by-borge", "--chunker-params", params); code != ExitOK {
		t.Fatalf("borge recreate exited %d\n%s", code, stderr)
	}

	after := archiveSizes(t, r.mustRun("repo-list", "-r", r.path,
		"--format", "{archive} {size} {nfiles}{NL}"))

	borg, borge := after["by-borg"], after["by-borge"]
	if borg != borge {
		t.Errorf("recreated archives differ: borg size=%d nfiles=%d, borge size=%d nfiles=%d",
			borg[0], borg[1], borge[0], borge[1])
	}

	// If recreate were using create's rule the size would barely move, and this test
	// would pass while proving nothing about the distinction it exists for.
	if borg[0] <= before["by-borg"][0]*4 {
		t.Errorf("recreating changed the recorded size from %d to %d; the item stream is "+
			"not being counted, so the two rules are not being told apart",
			before["by-borg"][0], borg[0])
	}
}
