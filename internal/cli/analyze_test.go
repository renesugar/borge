// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// analyzeJSON runs borge analyze and parses the numbers out of its JSON.
//
// The numbers are nested: borg puts them under "dedup_size", or under "by_name" for
// --by-name, beside the encryption and repository blocks every JSON command carries, and
// borge has matched that since 2026-08-19 (DIVERGENCES.md #42). Unwrapping here rather
// than in each caller means the borge side is read exactly as the borg side already was -
// the borg side has always had to unwrap, which is what made the difference visible.
func analyzeJSON(t *testing.T, r *borgRepo, out any, args ...string) {
	t.Helper()
	stdout, stderr, code := r.borge(t, append([]string{"analyze", "-json"}, args...)...)
	if code != ExitOK {
		t.Fatalf("borge analyze exited %d\n%s", code, stderr)
	}
	key := "dedup_size"
	for _, a := range args {
		if a == "--by-name" || a == "-by-name" {
			key = "by_name"
		}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("analyze JSON does not parse: %v\n%s", err, stdout)
	}
	inner, ok := doc[key]
	if !ok {
		t.Fatalf("analyze JSON has no %q block: %s", key, stdout)
	}
	if err := json.Unmarshal(inner, out); err != nil {
		t.Fatalf("analyze %s does not parse: %v\n%s", key, err, inner)
	}
	// Then the document itself, for the keys that sit beside the block rather than inside
	// it: "hotspots" is one, because borg reports it next to dedup_size and not within.
	// Unmarshalling twice fills each field from the level it actually lives at, and leaves
	// alone the fields absent from this one.
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		t.Fatalf("analyze JSON does not parse: %v\n%s", err, stdout)
	}
}

type analyzeSetResult struct {
	ConsideredArchives int  `json:"considered_archives"`
	TotalArchives      int  `json:"total_archives"`
	WholeRepository    bool `json:"whole_repository"`
	Deduplicated       struct {
		SourceSize int64 `json:"source_size"`
		StoredSize int64 `json:"stored_size"`
	} `json:"deduplicated"`
	Exclusive *struct {
		SourceSize int64 `json:"source_size"`
		StoredSize int64 `json:"stored_size"`
	} `json:"exclusive"`
	Unreferenced struct {
		StoredSize int64 `json:"stored_size"`
		Chunks     int   `json:"chunks"`
	} `json:"unreferenced"`
	TotalChunks   int `json:"total_chunks"`
	MissingChunks int `json:"missing_chunks"`
	Hotspots      []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	} `json:"hotspots"`
}

type analyzeNameResult struct {
	Archives int `json:"archives"`
	Names    []struct {
		Name       string `json:"name"`
		Archives   int    `json:"archives"`
		SourceSize int64  `json:"source_size"`
		StoredSize int64  `json:"stored_size"`
	} `json:"names"`
	Shared struct {
		SourceSize int64 `json:"source_size"`
		StoredSize int64 `json:"stored_size"`
	} `json:"shared"`
	Unreferenced struct {
		StoredSize int64 `json:"stored_size"`
		Chunks     int   `json:"chunks"`
	} `json:"unreferenced"`
	Total struct {
		SourceSize int64 `json:"source_size"`
		StoredSize int64 `json:"stored_size"`
	} `json:"total"`
	TotalChunks int `json:"total_chunks"`
}

// TestAnalyzeCountsEveryChunkExactlyOnce is the invariant the whole report rests on.
//
// Every chunk in the index is in exactly one bucket, so the buckets have to add up to the
// index. If they did not, some chunk would be either double-counted (making a deletion
// look more profitable than it is) or missed entirely.
func TestAnalyzeCountsEveryChunkExactlyOnce(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("alpha")
	// A second name, so the shared and exclusive rows are both non-trivial.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "shared.txt"),
		[]byte(strings.Repeat("content ", 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "unique.txt"),
		[]byte(strings.Repeat("only here ", 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "beta", other)

	var byName analyzeNameResult
	analyzeJSON(t, r, &byName, "--by-name")

	if byName.TotalChunks == 0 {
		t.Fatal("the repository index reports no chunks at all")
	}
	if len(byName.Names) != 2 {
		t.Fatalf("want 2 distinct names, got %d: %+v", len(byName.Names), byName.Names)
	}

	// The stored sizes of the rows must add up to the total row.
	var sum int64
	for _, n := range byName.Names {
		sum += n.StoredSize
	}
	sum += byName.Shared.StoredSize
	if sum != byName.Total.StoredSize {
		t.Errorf("the name rows sum to %d stored bytes but the total row says %d",
			sum, byName.Total.StoredSize)
	}
	if byName.Total.StoredSize == 0 {
		t.Error("the total stored size is zero, so the sum above proves nothing")
	}
}

// TestAnalyzeExclusiveIsWhatDeletingWouldFree.
//
// The exclusive figure is the one someone acts on, so it is checked against reality:
// delete the set, compact, and the repository should shrink by about that much.
func TestAnalyzeExclusiveIsWhatDeletingWouldFree(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	r.makeArchives("keep")
	// A second archive with content nothing else has, so it has a real exclusive size.
	other := t.TempDir()
	big := strings.Repeat("this line is not in the other archive at all\n", 20000)
	if err := os.WriteFile(filepath.Join(other, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "doomed", other)

	var before analyzeSetResult
	analyzeJSON(t, r, &before, "-a", "doomed")
	if before.Exclusive == nil {
		t.Fatal("no exclusive figure: the set was treated as the whole repository")
	}
	if before.Exclusive.StoredSize == 0 {
		t.Fatal("the doomed archive has no exclusive content, so this test proves nothing")
	}

	sizeBefore := repoBytes(t, r.path)
	if _, stderr, code := r.borge(t, "delete", "-a", "doomed"); code != ExitOK {
		t.Fatalf("delete exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borge(t, "compact"); code != ExitOK {
		t.Fatalf("compact exited %d\n%s", code, stderr)
	}
	sizeAfter := repoBytes(t, r.path)

	freed := sizeBefore - sizeAfter
	want := before.Exclusive.StoredSize
	// Pack framing and index rewriting mean this is never exact. Within a factor of two
	// either way still distinguishes a correct figure from a wrong one, and a wrong one
	// here is usually wrong by an order of magnitude (the whole repo, or nothing).
	if freed < want/2 || freed > want*2+(1<<20) {
		t.Errorf("analyze said deleting the set would free about %d bytes, but the "+
			"repository shrank by %d", want, freed)
	}
}

// TestAnalyzeWholeRepositoryHasNoExclusiveRow: with no archives left over, every
// referenced chunk is trivially exclusive to the set, so repeating the deduplicated figure
// under a different name would be noise dressed as information.
func TestAnalyzeWholeRepositoryHasNoExclusiveRow(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one", "two")

	var all analyzeSetResult
	analyzeJSON(t, r, &all)
	if !all.WholeRepository {
		t.Errorf("analysing with no filter did not consider the whole repository: %+v", all)
	}
	if all.Exclusive != nil {
		t.Errorf("the whole-repository report has an exclusive row: %+v", all.Exclusive)
	}

	var subset analyzeSetResult
	analyzeJSON(t, r, &subset, "--last", "1")
	if subset.WholeRepository {
		t.Error("--last 1 was treated as the whole repository")
	}
	if subset.Exclusive == nil {
		t.Error("a proper subset has no exclusive row")
	}
}

// TestAnalyzeByNameRefusesArchiveFilters: "shared" and "unreferenced" are only meaningful
// across the whole repository, so a filter would quietly change what the words mean.
func TestAnalyzeByNameRefusesArchiveFilters(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("one", "two")

	_, stderr, code := r.borge(t, "analyze", "--by-name", "--last", "1")
	if code != ExitError {
		t.Fatalf("--by-name with a filter exited %d, want ExitError (%d)", code, ExitError)
	}
	if !strings.Contains(stderr, "whole repository") {
		t.Errorf("the error does not explain why: %q", stderr)
	}
}

// TestAnalyzeHotspotsFindTheChurningDirectory.
//
// The hot-spot report answers "what keeps changing", which a size report cannot: a small
// directory rewritten every run costs more over time than a large one that never changes,
// and looks like nothing in every other view.
func TestAnalyzeHotspotsFindTheChurningDirectory(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	src := t.TempDir()
	for _, d := range []string{"stable", "churning"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The stable directory is much larger, so a report that simply ranked by size would
	// name it and be wrong.
	if err := os.WriteFile(filepath.Join(src, "stable", "big.bin"),
		[]byte(strings.Repeat("unchanging data here\n", 40000)), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		body := strings.Repeat("rewritten every single time round "+string(rune('a'+i))+"\n", 8000)
		if err := os.WriteFile(filepath.Join(src, "churning", "log.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		r.mustRun("create", "-r", r.path, "series", src)
	}

	var res analyzeSetResult
	analyzeJSON(t, r, &res)
	if len(res.Hotspots) == 0 {
		t.Fatal("no hot spots reported for three archives with a rewritten file")
	}
	top := res.Hotspots[0].Path
	if !strings.HasSuffix(top, "/churning") {
		var all []string
		for _, h := range res.Hotspots {
			all = append(all, h.Path)
		}
		t.Errorf("the busiest directory is %q, want the churning one; got %v", top, all)
	}
}

// TestAnalyzeNeedsTwoArchivesForHotspots: one archive has nothing to compare against, and
// saying so beats an empty list that looks like "nothing ever changes".
func TestAnalyzeNeedsTwoArchivesForHotspots(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())
	r.makeArchives("only")

	stdout, stderr, code := r.borge(t, "analyze")
	if code != ExitOK {
		t.Fatalf("analyze exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "at least two") {
		t.Errorf("analyze did not explain why there is no hot-spot section:\n%s", stdout)
	}
}

// borgByName is borg's --by-name JSON, which nests the decomposition under "by_name".
type borgByName struct {
	ByName analyzeNameResult `json:"by_name"`
}

// TestAnalyzeByNameMatchesBorg is the gate: borge's decomposition of a repository has to
// be borg's, number for number.
//
// Both tools read the same chunk index, so the stored sizes come from the same source and
// any difference is a difference in *how the chunks were attributed* - which is the entire
// substance of the command. The corpus is built so that every bucket is non-empty: a
// comparison where "shared" and "unreferenced" are both zero would not test them.
func TestAnalyzeByNameMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	t.Setenv("BORGE_CACHE_DIR", t.TempDir())

	shared := strings.Repeat("this content appears under both names\n", 5000)
	aOnly := strings.Repeat("this content is only ever under alpha\n", 5000)
	bOnly := strings.Repeat("this content is only ever under beta\n", 5000)

	// alpha and beta each hold the shared file, so its chunks belong to neither name.
	alphaDir := t.TempDir()
	mustWrite(t, filepath.Join(alphaDir, "shared.txt"), shared)
	mustWrite(t, filepath.Join(alphaDir, "alpha.txt"), aOnly)
	r.mustRun("create", "-r", r.path, "alpha", alphaDir)

	betaDir := t.TempDir()
	mustWrite(t, filepath.Join(betaDir, "shared.txt"), shared)
	mustWrite(t, filepath.Join(betaDir, "beta.txt"), bOnly)
	r.mustRun("create", "-r", r.path, "beta", betaDir)

	// A third archive, deleted but not compacted, so its exclusive chunks are
	// unreferenced and land in that bucket.
	goneDir := t.TempDir()
	mustWrite(t, filepath.Join(goneDir, "gone.txt"),
		strings.Repeat("this content belongs to a deleted archive\n", 5000))
	r.mustRun("create", "-r", r.path, "gone", goneDir)
	r.mustRun("delete", "-r", r.path, "-a", "gone")

	var fromBorg borgByName
	out, err := r.runErr("analyze", "-r", r.path, "--by-name", "--json")
	if err != nil {
		t.Fatalf("borg analyze failed: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &fromBorg); err != nil {
		t.Fatalf("borg analyze JSON does not parse: %v\n%s", err, out)
	}
	want := fromBorg.ByName

	var got analyzeNameResult
	analyzeJSON(t, r, &got, "--by-name")

	// Guard the comparison: every bucket has to be exercised, or agreeing on zeroes would
	// pass for agreement.
	if want.Shared.StoredSize == 0 {
		t.Fatal("the shared bucket is empty, so this comparison does not test it")
	}
	if want.Unreferenced.StoredSize == 0 {
		t.Fatal("the unreferenced bucket is empty, so this comparison does not test it")
	}
	if len(want.Names) != 2 {
		t.Fatalf("borg found %d name(s), want 2: %+v", len(want.Names), want.Names)
	}

	if got.Archives != want.Archives {
		t.Errorf("archives: borg %d, borge %d", want.Archives, got.Archives)
	}
	if got.TotalChunks != want.TotalChunks {
		t.Errorf("total_chunks: borg %d, borge %d", want.TotalChunks, got.TotalChunks)
	}
	if got.Shared != want.Shared {
		t.Errorf("shared: borg %+v, borge %+v", want.Shared, got.Shared)
	}
	if got.Unreferenced != want.Unreferenced {
		t.Errorf("unreferenced: borg %+v, borge %+v", want.Unreferenced, got.Unreferenced)
	}
	if got.Total != want.Total {
		t.Errorf("total: borg %+v, borge %+v", want.Total, got.Total)
	}

	byName := map[string]int64{}
	for _, n := range got.Names {
		byName[n.Name] = n.StoredSize
	}
	for _, n := range want.Names {
		if byName[n.Name] != n.StoredSize {
			t.Errorf("name %q stored size: borg %d, borge %d", n.Name, n.StoredSize, byName[n.Name])
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
