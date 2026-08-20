// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// create's last five options, and the three defects implementing them found.
//
// The interesting one is --files-changed: borge had no detection of a file changing while
// it was read, so a torn copy was stored and reported as though it were whole. See
// DIVERGENCES.md #52.

// TestCreateStatusLettersMatchBorg: what "--list" calls each kind of thing.
//
// borge reported every fifo, character device and block device as "i" - which is borg's
// letter for content read from stdin - so the listing named the wrong kind and "--filter f"
// selected nothing.
func TestCreateStatusLettersMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "regular.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(src, "pipe")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo is not available: %v", err)
	}

	// Character devices are not creatable without privileges, so /dev/null is named
	// directly as a second source path.
	args := []string{"create", "--list", "-r", r.path, "PLACEHOLDER", src, "/dev/null"}

	letters := func(stream string) map[string]string {
		out := map[string]string{}
		for _, line := range strings.Split(stream, "\n") {
			if len(line) < 3 || line[1] != ' ' {
				continue
			}
			out[filepath.Base(line[2:])] = string(line[0])
		}
		return out
	}

	args[4] = "by-borg"
	_, wantErr := borgStreams(t, r, args...)
	want := letters(wantErr)
	args[4] = "by-borge"
	_, gotErr, code := r.borge(t, args...)
	if code != ExitOK {
		t.Fatalf("borge create exited %d\n%s", code, gotErr)
	}
	got := letters(gotErr)

	for _, name := range []string{"regular.txt", "link", "dir", "pipe", "null"} {
		w, ok := want[name]
		if !ok {
			t.Fatalf("borg did not list %s, so this test asserts nothing about it:\n%s", name, wantErr)
		}
		if got[name] != w {
			t.Errorf("%s: borge says %q, borg says %q", name, got[name], w)
		}
	}
	// The point of the test, stated so a future change to the letters cannot quietly
	// satisfy it: a fifo is "f" and a character device is "c".
	if want["pipe"] != "f" || want["null"] != "c" {
		t.Fatalf("borg's letters changed: pipe=%q null=%q", want["pipe"], want["null"])
	}
}

// TestCreateTagsMatchBorg: --tags, and the validator borge did not have.
//
// borg's help says "comma-separated or multiple arguments" and its validator forbids a
// comma, so the comma-separated form the help promises does not exist. borge split on
// commas and accepted what borg refuses - and its "tag" command accepted anything at all,
// including a tag with a space that no borg could have written.
func TestCreateTagsMatchBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "a,b" is not in this list: borg rejects it as one tag with a comma, and borge reads
	// it as two - the one deliberate difference, and the one that fails loudly rather than
	// quietly. See validateTags.
	for _, tag := range []string{"toolongtagname", "with space", "dollar$", "@nope"} {
		t.Run("rejects "+tag, func(t *testing.T) {
			out, err := r.runErr("create", "--tags", tag, "-r", r.path, "b"+tag, src)
			if err == nil {
				t.Fatalf("borg accepted the tag %q:\n%s", tag, out)
			}
			complaint := ""
			for _, line := range strings.Split(out, "\n") {
				if i := strings.Index(line, "Invalid tag:"); i >= 0 {
					complaint = line[i:]
				}
				if i := strings.Index(line, "Unknown special tags"); i >= 0 {
					complaint = line[i:]
				}
			}
			if complaint == "" {
				t.Fatalf("could not find borg's complaint about %q in:\n%s", tag, out)
			}
			_, stderr, code := r.borge(t, "create", "--tags", tag, "-r", r.path, "e"+tag, src)
			if code != ExitError {
				t.Fatalf("borge accepted the tag %q (exit %d)", tag, code)
			}
			if !strings.Contains(stderr, complaint) {
				t.Errorf("for %q borge said %q, want it to contain %q", tag, strings.TrimSpace(stderr), complaint)
			}
		})
	}

	// Two tags: borg's nargs="+" form, borge's comma form. The archives must end up the
	// same, which is the property that matters - the spelling differs and the result does
	// not.
	r.mustRun("create", "--tags", "keep", "@PROT", "-r", r.path, "borg-tagged", src)
	if _, stderr, code := r.borge(t, "create", "--tags", "keep,@PROT",
		"-r", r.path, "borge-tagged", src); code != ExitOK {
		t.Fatalf("borge create --tags exited %d\n%s", code, stderr)
	}
	for _, name := range []string{"borg-tagged", "borge-tagged"} {
		out, _ := borgStreams(t, r, "repo-list", "-r", r.path, "-a", name, "--format", "{tags}{NL}")
		if strings.TrimSpace(out) != "@PROT,keep" {
			t.Errorf("%s has tags %q, want %q", name, strings.TrimSpace(out), "@PROT,keep")
		}
	}

	// Repetition overwrites in both, rather than accumulating: a borge that accumulated
	// would lose the earlier tags when the same command line was run under borg.
	r.mustRun("create", "--tags", "first", "--tags", "second", "-r", r.path, "borg-twice", src)
	if _, stderr, code := r.borge(t, "create", "--tags", "first", "--tags", "second",
		"-r", r.path, "borge-twice", src); code != ExitOK {
		t.Fatalf("borge create --tags twice exited %d\n%s", code, stderr)
	}
	for _, name := range []string{"borg-twice", "borge-twice"} {
		out, _ := borgStreams(t, r, "repo-list", "-r", r.path, "-a", name, "--format", "{tags}{NL}")
		if strings.TrimSpace(out) != "second" {
			t.Errorf("%s has tags %q, want %q - the second --tags replaces the first",
				name, strings.TrimSpace(out), "second")
		}
	}

	// And the tag command validates the same way, which it did not before.
	if _, stderr, code := r.borge(t, "tag", "--add", "bad tag", "-r", r.path, "borge-tagged"); code != ExitError {
		t.Errorf("borge tag --add accepted a tag with a space (exit %d): %s", code, stderr)
	}
}

// TestCreateSparseChangesNothingStored: --sparse is a read optimisation, and an archive
// made with it has to be the archive made without it.
func TestCreateSparseChangesNothingStored(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	holey := filepath.Join(src, "holey.bin")
	f, err := os.Create(holey)
	if err != nil {
		t.Fatal(err)
	}
	// Data, a large hole, data - and a hole at the end, which is the case a naive
	// implementation reads through.
	if _, err := f.Write([]byte(strings.Repeat("A", 100000))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(50<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(strings.Repeat("B", 100000))); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// A file the filesystem did not actually make sparse would test nothing.
	info, err := os.Stat(holey)
	if err != nil {
		t.Fatal(err)
	}
	if used := blocksOf(t, holey) * 512; used > info.Size()/2 {
		t.Skipf("the filesystem stored %d bytes for a %d byte sparse file; it has no holes to skip",
			used, info.Size())
	}

	r.mustRun("create", "-r", r.path, "by-borg", src)
	for _, name := range []string{"plain", "sparse"} {
		args := []string{"create", "-r", r.path, name, src}
		if name == "sparse" {
			args = append(args, "--sparse")
		}
		if _, stderr, code := r.borge(t, args...); code != ExitOK {
			t.Fatalf("borge create %s exited %d\n%s", name, code, stderr)
		}
	}
	// Identical to each other and to borg's: --sparse must not change one stored byte.
	for _, pair := range [][2]string{{"plain", "sparse"}, {"by-borg", "sparse"}} {
		out, _ := borgStreams(t, r, "diff", "-r", r.path, pair[0], pair[1])
		if strings.TrimSpace(out) != "" {
			t.Errorf("%s and %s differ:\n%s", pair[0], pair[1], out)
		}
	}
}

// blocksOf reports the 512-byte blocks the filesystem actually allocated.
func blocksOf(t *testing.T, path string) int64 {
	t.Helper()
	out, err := exec.Command("stat", "-c", "%b", path).Output()
	if err != nil {
		t.Skipf("stat is not available: %v", err)
	}
	var blocks int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &blocks); err != nil {
		t.Skipf("could not read the block count: %v", err)
	}
	return blocks
}

// churnFile keeps a file's timestamps moving until the returned stop is called.
//
// A tight loop writing ONE BYTE, with no sleep and on a handle that stays open. The first
// version of this rewrote the whole 8 MB file and then slept a millisecond, and it was
// flaky: an 8 MB read from the page cache takes a few milliseconds, so a read could fit
// entirely between two writes and see no change at all. That is exactly what happened in
// the suite - three retries, then a clean read, and the file was reported "A".
//
// One byte per iteration updates ctime and mtime thousands of times a second, so a read
// taking longer than a microsecond cannot escape it. The detection needs *every* one of
// borg's ten attempts to collide before it reports "C", so "usually collides" is not
// enough.
func churnFile(t *testing.T, path string) (stop func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer f.Close()
		var n byte
		for {
			select {
			case <-done:
				return
			default:
			}
			n++
			if _, err := f.WriteAt([]byte{n}, 0); err != nil {
				return
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// TestCreateFilesChangedDetectsATornFile is the reason this cluster matters.
//
// A file written while it is being read is stored as a mix of before and after: not the old
// contents, not the new ones, and nothing that ever existed. borge stored that and reported
// "A". It now retries, and when the file will not settle it reports "C" and - just as
// important - does not memorize the file in the files cache, so the next run reads it again.
//
// Driven in-process with a writer goroutine, because that is the only way to make the race
// happen on purpose: an external writer either finishes too early or has to be timed. It
// takes about fifteen seconds, which is borg's retry schedule run to its end.
func TestCreateFilesChangedDetectsATornFile(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	churn := filepath.Join(src, "churn.bin")

	// Big enough that reading it takes long enough for the writer to land inside.
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = byte(i)
	}
	if err := os.WriteFile(churn, body, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := churnFile(t, churn)
	stdout, stderr, code := r.borge(t, "create", "--list", "-r", r.path, "churned", src)
	stop()
	if code != ExitOK && code != ExitWarning {
		t.Fatalf("borge create exited %d\n%s%s", code, stdout, stderr)
	}

	if !strings.Contains(stderr, "file changed while we read it") {
		t.Fatalf("borge did not notice a file being rewritten under it:\n%s", stderr)
	}
	if !strings.Contains(stderr, "C "+churn) {
		t.Fatalf("the churning file was not reported as C, so some attempt read it cleanly - "+
			"the writer is not keeping up with the reader:\n%s", stderr)
	}

	// The C file must not be in the files cache: the next run has to read it again,
	// because the copy that was stored is the one known to be wrong.
	_, stderr2, code := r.borge(t, "create", "--list", "-r", r.path, "after", src)
	if code != ExitOK && code != ExitWarning {
		t.Fatalf("the second borge create exited %d\n%s", code, stderr2)
	}
	if strings.Contains(stderr2, "U "+churn) {
		t.Error("the second run took the torn file from the files cache; a file stored as " +
			"C must be read again")
	}
}

// TestCreateFilesChangedDisabled: the option turns the detection off, as borg's does.
func TestCreateFilesChangedDisabled(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	churn := filepath.Join(src, "churn.bin")
	body := make([]byte, 8<<20)
	if err := os.WriteFile(churn, body, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := churnFile(t, churn)
	_, stderr, code := r.borge(t, "create", "--list", "--files-changed", "disabled",
		"-r", r.path, "unchecked", src)
	stop()

	if code != ExitOK && code != ExitWarning {
		t.Fatalf("borge create exited %d\n%s", code, stderr)
	}
	if strings.Contains(stderr, "file changed while we read it") {
		t.Errorf("--files-changed disabled still checked:\n%s", stderr)
	}
	if strings.Contains(stderr, "C "+churn) {
		t.Errorf("--files-changed disabled still reported C:\n%s", stderr)
	}

	// And the value is validated the way borg validates it.
	if out, err := r.runErr("create", "--files-changed", "nonsense", "-r", r.path, "x", src); err == nil {
		t.Fatalf("borg accepted --files-changed nonsense:\n%s", out)
	}
	if _, stderr, code := r.borge(t, "create", "--files-changed", "nonsense",
		"-r", r.path, "x", src); code != ExitError {
		t.Errorf("borge accepted --files-changed nonsense (exit %d): %s", code, stderr)
	}
}
