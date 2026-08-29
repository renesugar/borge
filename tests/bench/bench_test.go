// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Package bench is the Stage 9 performance baseline (docs/PORTING_PLAN.md §12).
//
// It runs borge, and borg where it is available, over the same corpus on the same machine
// and emits JSON: wall time, CPU time, peak RSS, repository size and chunk count.
//
// # What this is, and what it deliberately is not yet
//
// §12 asks for nine scenarios, cold and warm cache, syscall counts and time-to-first-byte
// on restore. This is the minimum that makes *one* scenario repeatable - create and extract
// the pathological directory - because a profile taken against a scenario nobody can re-run
// produces numbers nobody can check. The rest is added when there is something to compare.
//
// # Why it is not part of the suite
//
// It takes minutes and it needs a corpus that only exists on the development machine. Set
// BORGE_BENCH=1 to run it. Absent that it skips, saying so.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/renesugar/borge/tests/harness"
)

// pathologicalDir is the corpus the project brief singles out: 118,866 files in one
// directory, 479 MB. It is the case where per-file costs dominate, which is why the
// baseline starts here rather than on the 1.6-million-file whole.
const pathologicalDir = "/home/renes/projects/recipedb/recipe_vault/www-wedesoft-de/downloads/deutsche-rezepte"

// defaultMode is unencrypted on purpose.
//
// AES-OCB is 17x behind borg because of a ceiling in crypto/cipher that is not borge's
// (§12.2, golang/go#81029), and measuring it here would swamp everything the write path
// does. "What does borge's write path cost" and "what does OCB cost" are different
// questions with different fixes, so they get different runs. BORGE_BENCH_MODE overrides.
const defaultMode = "none-sha256"

// run is one measured invocation.
type run struct {
	Tool   string  `json:"tool"`
	Op     string  `json:"op"`
	Wall   float64 `json:"wall_s"`
	User   float64 `json:"user_s"`
	Sys    float64 `json:"sys_s"`
	MaxRSS int64   `json:"max_rss_kib"`
	ExitOK bool    `json:"exit_ok"`
	Stderr string  `json:"stderr,omitempty"`
	// stdout is kept for the caller to parse and is not serialised: create --json prints
	// a document, and a whole archive listing in the record would bury the numbers.
	stdout string
}

// result is what one scenario produced.
type result struct {
	Scenario  string `json:"scenario"`
	Corpus    string `json:"corpus"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	Mode      string `json:"encryption"`
	Cache     string `json:"cache"`
	BorgeVer  string `json:"borge_version"`
	BorgVer   string `json:"borg_version,omitempty"`
	Host      string `json:"host"`
	StartedAt string `json:"started_at"`
	Runs      []run  `json:"runs"`
	// RepoBytes is borge's repository after its create, and Archive what borge recorded
	// about the archive it wrote.
	RepoBytes int64       `json:"repo_bytes"`
	Archive   archiveInfo `json:"borge_archive"`
}

// measure runs a command and reports what it cost.
//
// Both tools are measured the same way, as subprocesses through SysUsage, so neither gets
// an in-process advantage that would make the comparison meaningless.
func measure(t *testing.T, tool, op, dir string, env []string, name string, args ...string) run {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr, stdout strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout

	started := time.Now()
	err := cmd.Run()
	wall := time.Since(started)

	r := run{Tool: tool, Op: op, Wall: wall.Seconds(), ExitOK: err == nil}
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		r.User = time.Duration(ru.Utime.Nano()).Seconds()
		r.Sys = time.Duration(ru.Stime.Nano()).Seconds()
		r.MaxRSS = ru.Maxrss
	}
	if err != nil {
		r.Stderr = lastLines(stderr.String(), 5)
	}
	r.stdout = stdout.String()
	return r
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// countTree returns the number of regular files and their total size.
func countTree(t *testing.T, root string) (int, int64) {
	t.Helper()
	var files int
	var bytes int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return files, bytes
}

// dirBytes is the on-disk size of a directory tree.
func dirBytes(t *testing.T, root string) int64 {
	_, b := countTree(t, root)
	return b
}

// borgeBinary returns the path to bin/borge, refusing one older than the tree.
//
// A stale binary in a correctness gate reports a false pass; here it would report a false
// number, and numbers get quoted. See harness.StaleBinary for the run that made this
// necessary.
func borgeBinary(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "bin", "borge")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("borge is not built at %s; run 'make build'", bin)
	}
	if why := harness.StaleBinary(root, info.ModTime()); why != "" {
		t.Fatal(why)
	}
	return bin
}

// toolVersion asks a tool what it is, for the record. A measurement whose JSON does not
// say which build produced it cannot be compared with anything later.
func toolVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// createStats reads the timings out of what "create --json" printed.
//
// Not out of "info --json", which was this harness's first mistake. borge reports
// chunking_time and hashing_time there as 0.0 *by design* - nothing in an archive records
// them, borg's Statistics object is equally fresh for info, and borge matches it
// deliberately (internal/cli/jsonapi.go). Reading them there produced "chunking 0.0s,
// hashing 0.0s" in the first baseline, which looked like borge measuring zero and was
// actually the harness asking a command that never answers.
//
// create is where they are collected, so create is where they are read.
func createStats(t *testing.T, out string) archiveInfo {
	t.Helper()
	var doc struct {
		Archive struct {
			Stats archiveInfo `json:"stats"`
		} `json:"archive"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Logf("create --json did not parse, timings unavailable: %v", err)
		return archiveInfo{}
	}
	return doc.Archive.Stats
}

// archiveInfo is what the tool recorded about the archive it wrote.
//
// StoreStats is kept whole rather than picked apart: it is the backend's own call counts,
// volumes and times - the I/O half of the picture §12 wants - and which of its keys matter
// is a question for whoever reads the JSON, not for this harness to decide in advance.
type archiveInfo struct {
	NFiles       int             `json:"nfiles"`
	OriginalSize int64           `json:"original_size"`
	ChunkingTime float64         `json:"chunking_time"`
	HashingTime  float64         `json:"hashing_time"`
	StoreStats   json.RawMessage `json:"store_stats,omitempty"`
}

// TestBaselinePathologicalDir is the scenario: create the 118,866-file directory into a
// fresh repository, then extract it, for each tool available.
//
// It asserts almost nothing. A baseline's job is to produce numbers that can be compared
// with later numbers; the one thing it does insist on is that every command *succeeded*,
// because a fast failure is not a fast create.
func TestBaselinePathologicalDir(t *testing.T) {
	if os.Getenv("BORGE_BENCH") == "" {
		t.Skip("set BORGE_BENCH=1 to run the Stage 9 baseline; it takes minutes and needs the corpus")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	corpus := pathologicalDir
	if v := os.Getenv("BORGE_BENCH_CORPUS"); v != "" {
		corpus = v
	}
	if info, err := os.Stat(corpus); err != nil || !info.IsDir() {
		t.Skipf("corpus not available at %s", corpus)
	}
	mode := defaultMode
	if v := os.Getenv("BORGE_BENCH_MODE"); v != "" {
		mode = v
	}

	borge := borgeBinary(t, root)
	borg := filepath.Join(root, ".venv-borg2", "bin", "borg")
	haveBorg := false
	if _, err := os.Stat(borg); err == nil {
		haveBorg = true
	} else {
		t.Logf("borg 2 not built at %s; measuring borge alone (run 'make borg2' for the comparison)", borg)
	}

	files, bytes := countTree(t, corpus)
	host, _ := os.Hostname()
	res := result{
		Scenario: "pathological-dir create+extract", Corpus: corpus,
		Files: files, Bytes: bytes, Mode: mode,
		// Warm, and said so. Cold needs drop_caches and root; a harness that ran warm
		// and called it cold would be worse than one that runs warm and admits it.
		Cache:     "warm",
		BorgeVer:  toolVersion(borge),
		Host:      host,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if haveBorg {
		res.BorgVer = toolVersion(borg)
	}

	base := t.TempDir()
	type tool struct {
		name, bin string
		env       []string
	}
	tools := []tool{{"borge", borge, nil}}
	if haveBorg {
		tools = append(tools, tool{"borg", borg, nil})
	}

	for _, tl := range tools {
		repo := filepath.Join(base, tl.name+"-repo")
		dest := filepath.Join(base, tl.name+"-out")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		env := append([]string{
			"BORG_CACHE_DIR=" + filepath.Join(base, tl.name+"-cache"),
			"BORG_KEYS_DIR=" + filepath.Join(base, tl.name+"-keys"),
			"BORGE_CACHE_DIR=" + filepath.Join(base, tl.name+"-cache"),
			"BORGE_KEYS_DIR=" + filepath.Join(base, tl.name+"-keys"),
		}, tl.env...)

		init := measure(t, tl.name, "repo-create", base, env, tl.bin,
			"repo-create", "-r", repo, "-e", mode)
		if !init.ExitOK {
			t.Fatalf("%s repo-create failed:\n%s", tl.name, init.Stderr)
		}

		// --json on both, so the two are still measured doing the same work; it is also
		// the only place the chunking and hashing timings exist.
		create := measure(t, tl.name, "create", base, env, tl.bin,
			"create", "--json", "-r", repo, "bench", corpus)
		res.Runs = append(res.Runs, create)
		if !create.ExitOK {
			t.Errorf("%s create failed, so its timing means nothing:\n%s", tl.name, create.Stderr)
			continue
		}

		extract := measure(t, tl.name, "extract", dest, env, tl.bin,
			"extract", "-r", repo, "bench")
		res.Runs = append(res.Runs, extract)
		if !extract.ExitOK {
			t.Errorf("%s extract failed, so its timing means nothing:\n%s", tl.name, extract.Stderr)
		}

		if tl.name == "borge" {
			res.RepoBytes = dirBytes(t, repo)
			res.Archive = createStats(t, create.stdout)
		}
	}

	report(t, res)
}

// report writes the JSON and prints the table a person reads.
func report(t *testing.T, res result) {
	t.Helper()
	blob, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("BORGE_BENCH_OUT")
	if out == "" {
		out = filepath.Join("out", fmt.Sprintf("baseline-%s.json",
			time.Now().UTC().Format("20060102T150405Z")))
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Logf("%s: %d files, %s, %s, %s cache",
		res.Scenario, res.Files, humanBytes(res.Bytes), res.Mode, res.Cache)
	t.Logf("%-6s %-11s %9s %9s %9s %10s", "tool", "op", "wall", "user", "sys", "maxrss")
	for _, r := range res.Runs {
		t.Logf("%-6s %-11s %8.1fs %8.1fs %8.1fs %9s",
			r.Tool, r.Op, r.Wall, r.User, r.Sys, humanBytes(r.MaxRSS*1024))
	}
	t.Logf("borge repository %s; borge recorded %d files, %s, chunking %.3fs, hashing %.3fs",
		humanBytes(res.RepoBytes), res.Archive.NFiles, humanBytes(res.Archive.OriginalSize),
		res.Archive.ChunkingTime, res.Archive.HashingTime)
	if len(res.Archive.StoreStats) > 0 {
		var st map[string]any
		if json.Unmarshal(res.Archive.StoreStats, &st) == nil {
			t.Logf("borge store: %.0f load call(s) in %.3fs, %.0f store call(s) in %.3fs",
				num(st["load_calls"]), num(st["load_time"]),
				num(st["store_calls"]), num(st["store_time"]))
		}
	}
	t.Logf("written to %s", out)
}

// num reads a JSON number that may be absent.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
