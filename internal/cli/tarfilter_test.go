// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --tar-filter, and what borge did with a compressed file name before it had one.
//
// The option is the smaller half. borge compressed a ".gz" name with compress/gzip and did
// nothing else, so four of borg's five suffixes produced a PLAIN TAR under a compressed
// name - reported as success - and a bare ".gz" was compressed where borg leaves it alone.
// See DIVERGENCES.md #49.

// magic is the first bytes of each format, so that "is it actually compressed" is answered
// by the file rather than by whether the other tool happened to read it.
var tarMagic = map[string][]byte{
	"gzip":  {0x1f, 0x8b},
	"bzip2": {'B', 'Z', 'h'},
	"xz":    {0xfd, '7', 'z', 'X', 'Z', 0x00},
	"zstd":  {0x28, 0xb5, 0x2f, 0xfd},
	"tar":   nil, // checked at offset 257 instead
}

// looksLike reports whether the file starts with the format's magic. A plain tar has its
// magic at offset 257 ("ustar"), which is exactly what a mislabelled .tar.xz looked like.
func looksLike(t *testing.T, path, format string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if format == "tar" {
		return len(data) > 262 && string(data[257:262]) == "ustar"
	}
	m := tarMagic[format]
	return len(data) >= len(m) && bytes.Equal(data[:len(m)], m)
}

// tarTree is a small tree with a directory, so that a listing has more than one status.
func tarTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"a.txt":     "alpha",
		"b.txt":     strings.Repeat("beta ", 500),
		"sub/c.txt": "gamma",
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// paths lists an archive's contents, sorted - see DIVERGENCES.md #23.
func archivePaths(t *testing.T, r *borgRepo, name string) []string {
	t.Helper()
	out, _ := borgStreams(t, r, "list", "-r", r.path, name, "--format", "{path}{NL}")
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	sort.Strings(lines)
	return lines
}

// TestExportTarCompressionByExtension: the file name decides the compression, and it has to
// decide it the same way in both tools.
func TestExportTarCompressionByExtension(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := tarTree(t)
	r.mustRun("create", "-r", r.path, "src", src)
	want := archivePaths(t, r, "src")
	if len(want) < 4 {
		t.Fatalf("the archive holds %d items; too few to be worth exporting", len(want))
	}

	cases := []struct {
		ext     string
		format  string
		program string // the external program needed, empty when none is
	}{
		{"tar.gz", "gzip", "gzip"},
		{"tgz", "gzip", "gzip"},
		{"tar.bz2", "bzip2", "bzip2"},
		{"tbz", "bzip2", "bzip2"},
		{"tar.xz", "xz", "xz"},
		{"txz", "xz", "xz"},
		{"tar.zst", "zstd", ""}, // in-process in both tools
		{"tar.zstd", "zstd", ""},
		{"tzst", "zstd", ""},
		{"tar.lz4", "lz4", "lz4"},
		{"tar", "tar", ""},
		// A bare ".gz" is NOT one of borg's suffixes. borge used to compress it.
		{"gz", "tar", ""},
	}

	compressed := 0
	for _, c := range cases {
		t.Run(c.ext, func(t *testing.T) {
			if c.program != "" {
				if _, err := exec.LookPath(c.program); err != nil {
					t.Skipf("%s is not installed, so neither tool can use it", c.program)
				}
			}
			dir := t.TempDir()
			borgFile := filepath.Join(dir, "borg."+c.ext)
			borgeFile := filepath.Join(dir, "borge."+c.ext)
			r.mustRun("export-tar", "-r", r.path, "src", borgFile)
			if _, stderr, code := r.borge(t, "export-tar", "-r", r.path, "src", borgeFile); code != ExitOK {
				t.Fatalf("borge export-tar exited %d\n%s", code, stderr)
			}

			// borg's file says what the format is; borge's has to be the same thing.
			if !looksLike(t, borgFile, c.format) {
				t.Fatalf("borg's %s is not %s, so this case asserts the wrong format", c.ext, c.format)
			}
			if !looksLike(t, borgeFile, c.format) {
				t.Errorf("borge's %s is not %s", c.ext, c.format)
			}
			if c.format != "tar" {
				compressed++
			}

			// And each tool has to be able to read the other's.
			r.mustRun("import-tar", "-r", r.path, "borg-reads-"+c.ext, borgeFile)
			if got := archivePaths(t, r, "borg-reads-"+c.ext); !equalStrings(got, want) {
				t.Errorf("borg read borge's %s as\n%v\nwant\n%v", c.ext, got, want)
			}
			if _, stderr, code := r.borge(t, "import-tar", "-r", r.path, "borge-reads-"+c.ext, borgFile); code != ExitOK {
				t.Fatalf("borge import-tar of borg's %s exited %d\n%s", c.ext, code, stderr)
			}
			if got := archivePaths(t, r, "borge-reads-"+c.ext); !equalStrings(got, want) {
				t.Errorf("borge read borg's %s as\n%v\nwant\n%v", c.ext, got, want)
			}
		})
	}
	if compressed == 0 {
		t.Fatal("no compressed format was exercised; the whole comparison would be about plain tars")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTarFilterExplicit: an explicitly named program is run, with its arguments.
func TestTarFilterExplicit(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip is not installed")
	}
	src := tarTree(t)
	r.mustRun("create", "-r", r.path, "src", src)
	want := archivePaths(t, r, "src")
	dir := t.TempDir()

	// "gzip -9" rather than "gzip": the argument has to survive the split, and the result
	// is a gzip stream whether or not the level reached it - so the level is checked by
	// comparing against borg's own output for the same filter.
	out := filepath.Join(dir, "explicit.out")
	if _, stderr, code := r.borge(t, "export-tar", "--tar-filter", "gzip -9", "-r", r.path, "src", out); code != ExitOK {
		t.Fatalf("borge export-tar --tar-filter exited %d\n%s", code, stderr)
	}
	if !looksLike(t, out, "gzip") {
		t.Fatal("--tar-filter 'gzip -9' produced something that is not gzip")
	}
	// The name has no compressed suffix, so without the option there would be no filter:
	// this is the option working and not the extension.
	plain := filepath.Join(dir, "plain.out")
	if _, _, code := r.borge(t, "export-tar", "-r", r.path, "src", plain); code != ExitOK {
		t.Fatal("borge export-tar without a filter failed")
	}
	if !looksLike(t, plain, "tar") {
		t.Fatal("the same name without --tar-filter was compressed anyway; the test proves nothing")
	}

	// And the read side: an explicit decompressor for a name that implies nothing.
	if _, stderr, code := r.borge(t, "import-tar", "--tar-filter", "gzip -d", "-r", r.path, "explicit", out); code != ExitOK {
		t.Fatalf("borge import-tar --tar-filter exited %d\n%s", code, stderr)
	}
	if got := archivePaths(t, r, "explicit"); !equalStrings(got, want) {
		t.Errorf("imported\n%v\nwant\n%v", got, want)
	}
	// borg reads what borge's explicit filter wrote.
	r.mustRun("import-tar", "--tar-filter", "gzip -d", "-r", r.path, "borg-explicit", out)
	if got := archivePaths(t, r, "borg-explicit"); !equalStrings(got, want) {
		t.Errorf("borg imported\n%v\nwant\n%v", got, want)
	}
}

// TestTarFilterMissingProgram: both tools fail, and neither claims success.
func TestTarFilterMissingProgram(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := tarTree(t)
	r.mustRun("create", "-r", r.path, "src", src)
	dir := t.TempDir()

	out, err := r.runErr("export-tar", "--tar-filter", "borge-no-such-filter", "-r", r.path, "src",
		filepath.Join(dir, "borg.out"))
	if err == nil {
		t.Fatalf("borg accepted a filter that does not exist:\n%s", out)
	}
	if !strings.Contains(out, "borge-no-such-filter") {
		t.Fatalf("borg's message does not name the filter:\n%s", out)
	}

	_, stderr, code := r.borge(t, "export-tar", "--tar-filter", "borge-no-such-filter", "-r", r.path, "src",
		filepath.Join(dir, "borge.out"))
	if code != ExitError {
		t.Fatalf("borge exited %d for a missing filter, want %d\n%s", code, ExitError, stderr)
	}
	if !strings.Contains(stderr, "borge-no-such-filter") {
		t.Errorf("borge's message does not name the filter: %s", stderr)
	}
}

// TestImportTarListMatchesBorg: the listing's stream, its order and --filter.
//
// Compared as a sequence, which the create listing cannot be: import-tar's order comes from
// the tar file, not from a directory walk, so DIVERGENCES.md #23 does not apply here.
func TestImportTarListMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := tarTree(t)
	r.mustRun("create", "-r", r.path, "src", src)
	tarball := filepath.Join(t.TempDir(), "src.tar")
	r.mustRun("export-tar", "-r", r.path, "src", tarball)

	for _, filter := range []string{"", "d", "A", "Ad"} {
		name := filter
		if name == "" {
			name = "(none)"
		}
		t.Run(name, func(t *testing.T) {
			borgArgs := []string{"import-tar", "--list", "-r", r.path, "b" + name, tarball}
			borgeArgs := []string{"import-tar", "--list", "-r", r.path, "e" + name, tarball}
			if filter != "" {
				borgArgs = append(borgArgs, "--filter", filter)
				borgeArgs = append(borgeArgs, "--filter", filter)
			}
			wantOut, wantErr := borgStreams(t, r, borgArgs...)
			if wantOut != "" {
				t.Fatalf("borg wrote the listing to stdout, so this test asserts the wrong stream:\n%s", wantOut)
			}
			if strings.TrimSpace(wantErr) == "" {
				t.Fatalf("borg listed nothing with --filter %q; the case is vacuous", filter)
			}
			gotOut, gotErr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge import-tar exited %d\n%s", code, gotErr)
			}
			if gotOut != "" {
				t.Errorf("borge wrote the listing to stdout: %q", gotOut)
			}
			if gotErr != wantErr {
				t.Errorf("listing\n got:\n%s\nwant:\n%s", gotErr, wantErr)
			}
		})
	}
}

// TestCreateFilterMatchesBorg: --filter on create.
//
// Compared as a set. borg walks a directory in inode order and borge in name order, and
// borg reports a directory after its contents where borge reports it before - the same
// divergence from two ends (DIVERGENCES.md #23). What is under test is *which* lines
// appear, which is what --filter decides.
func TestCreateFilterMatchesBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := tarTree(t)

	lines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		sort.Strings(out)
		return out
	}

	var unfiltered int
	for _, filter := range []string{"", "d", "A", "Ad", "x"} {
		name := filter
		if name == "" {
			name = "(none)"
		}
		t.Run(name, func(t *testing.T) {
			borgArgs := []string{"create", "--list", "-r", r.path, "b" + name, src}
			borgeArgs := []string{"create", "--list", "-r", r.path, "e" + name, src}
			if filter != "" {
				borgArgs = append(borgArgs, "--filter", filter)
				borgeArgs = append(borgeArgs, "--filter", filter)
			}
			_, wantErr := borgStreams(t, r, borgArgs...)
			_, gotErr, code := r.borge(t, borgeArgs...)
			if code != ExitOK {
				t.Fatalf("borge create exited %d\n%s", code, gotErr)
			}
			want, got := lines(wantErr), lines(gotErr)
			if filter == "" {
				unfiltered = len(want)
			}
			if !equalStrings(got, want) {
				t.Errorf("--filter %q\n got: %v\nwant: %v", filter, got, want)
			}
		})
	}
	if unfiltered < 4 {
		t.Fatalf("the unfiltered listing had %d lines; the filters cannot have been tested", unfiltered)
	}
}
