// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// transfer, and the relatedness it depends on.
//
// The failure these tests exist for is one that looks like success: transferring into an
// unrelated repository stores every chunk again under a new id, so the command finishes,
// takes as long as a fresh backup, and deduplicates nothing. borg refuses it up front and
// so must borge - with borg's own messages, because a script may be matching them.

// The source repository has its own passphrase variable, and it does not fall back to the
// main one in either tool - see TestOtherPassphraseDoesNotFallBack. Both helpers below set
// it so that the tests exercise transfer rather than a missing passphrase.
func (r *borgRepo) borgeOther(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return r.borgeWithEnv(t, map[string]string{"BORGE_OTHER_PASSPHRASE": r.passphrase}, args...)
}

func (r *borgRepo) runErrOther(args ...string) (string, error) {
	r.t.Helper()
	return r.runErrEnv([]string{"BORG_OTHER_PASSPHRASE=" + r.passphrase}, args...)
}

// transferRepos builds a source repository with two archives of the same tree, and returns
// the source path and the tree.
func transferRepos(t *testing.T, r *borgRepo) (src string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "small.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 300000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(tree, "big.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun("create", "-r", r.path, "one", tree)
	r.mustRun("create", "-r", r.path, "two", tree)
	return tree
}

func archiveNamesIn(t *testing.T, r *borgRepo, repo string) []string {
	t.Helper()
	out, _ := borgStreams(t, r, "repo-list", "-r", repo, "--format", "{archive}{NL}")
	var names []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names
}

// TestTransferIntoARelatedRepository: the whole path, ending in borg reading the result.
func TestTransferIntoARelatedRepository(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	tree := transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")

	// borge makes the related repository, which is the step that inherits the id key and
	// the chunk seed.
	if _, stderr, code := r.borgeOther(t, "repo-create", "-r", dst, "--other-repo", r.path,
		"-e", "aes256-ocb"); code != ExitOK {
		t.Fatalf("borge repo-create --other-repo exited %d\n%s", code, stderr)
	}

	// A dry run first: it must report work to do and write nothing.
	stdout, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path, "-n")
	if code != ExitOK {
		t.Fatalf("borge transfer -n exited %d\n%s", code, stderr)
	}
	// borg puts these lines on stdout, so a pipeline sees them; borge must too.
	if !strings.Contains(stdout, "incomplete") {
		t.Errorf("a dry run of an empty destination did not report work to do:\nstdout: %s\nstderr: %s",
			stdout, stderr)
	}
	if names := archiveNamesIn(t, r, dst); len(names) != 0 {
		t.Fatalf("the dry run wrote archives: %v", names)
	}

	stdout, stderr, code = r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path)
	if code != ExitOK {
		t.Fatalf("borge transfer exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "finished") {
		t.Errorf("transfer reported no archives:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("borg's transfer writes nothing to stderr on success; borge wrote:\n%s", stderr)
	}

	// The second archive is the same tree, so it must have cost nothing: that is what a
	// related repository buys, and a transfer that re-stored everything would still say
	// "finished".
	if !strings.Contains(stdout, "transfer_size: 0 B") {
		t.Errorf("the second archive transferred data it should have deduplicated:\n%s", stdout)
	}

	want := archiveNamesIn(t, r, r.path)
	if got := archiveNamesIn(t, r, dst); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("destination holds %v, want %v", got, want)
	}

	// borg has to accept the result, and the restored tree has to be the original.
	if out, err := r.runErr("check", "--verify-data", "-r", dst); err != nil {
		t.Errorf("borg check on borge's transferred repository: %v\n%s", err, out)
	}
	into := t.TempDir()
	borgStreamsIn(t, r, into, "extract", "-r", dst, "one")
	original := filepath.Join(into, strings.TrimPrefix(tree, "/"))
	for _, name := range []string{"small.txt", "big.bin"} {
		got, err := os.ReadFile(filepath.Join(original, name))
		if err != nil {
			t.Fatalf("borg could not extract %s from the transferred archive: %v", name, err)
		}
		want, err := os.ReadFile(filepath.Join(tree, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs after transfer", name)
		}
	}
}

// TestTransferIsResumable: running it again finishes what was left rather than duplicating.
func TestTransferIsResumable(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")
	if _, stderr, code := r.borgeOther(t, "repo-create", "-r", dst, "--other-repo", r.path,
		"-e", "aes256-ocb"); code != ExitOK {
		t.Fatalf("repo-create --other-repo exited %d\n%s", code, stderr)
	}
	if _, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path); code != ExitOK {
		t.Fatalf("transfer exited %d\n%s", code, stderr)
	}
	first := archiveNamesIn(t, r, dst)

	stdout, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path)
	if code != ExitOK {
		t.Fatalf("the second transfer exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "already present in destination repo, skipping") {
		t.Errorf("the second run did not skip the archives it had already copied:\n%s", stdout)
	}
	second := archiveNamesIn(t, r, dst)
	if strings.Join(second, ",") != strings.Join(first, ",") {
		t.Errorf("re-running the transfer changed the archive list: %v -> %v", first, second)
	}
}

// TestTransferRefusesAnUnrelatedRepository: both guards, with borg's messages.
func TestTransferRefusesAnUnrelatedRepository(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	transferRepos(t, r)

	cases := []struct {
		name       string
		encryption string
		want       string
	}{
		{
			// Same mode, unrelated key material: the chunker secret differs, so the two
			// repositories cut chunks at different boundaries.
			name: "unrelated chunker secret", encryption: "aes256-ocb",
			want: "You must use the same chunker secret",
		},
		{
			// Different id-hash family: ids mean different things, so nothing dedups.
			name: "different id hash", encryption: "none-sha256",
			want: "You must either keep the same ID hash",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			r.mustRun("repo-create", "-r", dst, "-e", c.encryption)

			// borg refuses it, and its message is what borge's is compared against.
			out, err := r.runErrOther("transfer", "-r", dst, "--other-repo", r.path)
			if err == nil {
				t.Fatalf("borg accepted an unrelated destination:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("borg refused for another reason, so this test asserts the wrong "+
					"message:\n%s", out)
			}

			_, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path)
			if code != ExitError {
				t.Fatalf("borge accepted an unrelated destination (exit %d)\n%s", code, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("borge's refusal:\n got: %s\nwant it to contain: %s", stderr, c.want)
			}
			// And nothing was written before the refusal.
			if names := archiveNamesIn(t, r, dst); len(names) != 0 {
				t.Errorf("the refused transfer wrote %v", names)
			}
		})
	}
}

// TestTransferRechunkEscapesTheGuards: --chunker-params re-hashes everything, which is
// borg's way out of both relatedness rules.
func TestTransferRechunkEscapesTheGuards(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	tree := transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")
	r.mustRun("repo-create", "-r", dst, "-e", "aes256-ocb") // deliberately unrelated

	_, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path,
		"--chunker-params", "fixed,4096")
	if code != ExitOK {
		t.Fatalf("borge transfer --chunker-params into an unrelated repo exited %d\n%s", code, stderr)
	}
	if names := archiveNamesIn(t, r, dst); len(names) != 2 {
		t.Fatalf("destination holds %v, want both archives", names)
	}
	// The content has to survive being re-cut.
	into := t.TempDir()
	borgStreamsIn(t, r, into, "extract", "-r", dst, "one")
	got, err := os.ReadFile(filepath.Join(into, strings.TrimPrefix(tree, "/"), "big.bin"))
	if err != nil {
		t.Fatalf("borg could not extract the re-chunked archive: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(tree, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("the re-chunked content differs from the original")
	}
}

// TestTransferRefusesBorg1: the two doors that lead to borg 1.x data.
func TestTransferRefusesBorg1(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")
	r.mustRun("repo-create", "-r", dst, "-e", "aes256-ocb")

	for _, args := range [][]string{
		{"transfer", "-r", dst, "--other-repo", r.path, "--from-borg1"},
		{"transfer", "-r", dst, "--other-repo", r.path, "--upgrader", "From12To20"},
		{"repo-create", "-r", filepath.Join(t.TempDir(), "x"), "-e", "aes256-ocb", "--from-borg1"},
	} {
		t.Run(strings.Join(args[len(args)-2:], " "), func(t *testing.T) {
			_, stderr, code := r.borgeOther(t, args...)
			if code != ExitError {
				t.Fatalf("borge accepted %v (exit %d)", args, code)
			}
			// Refused by name with a pointer, not as an unknown option: borg HAS these,
			// so "flag provided but not defined" would read like a typo in borge.
			if !strings.Contains(stderr, "not supported") || !strings.Contains(stderr, "borg 1.x") {
				t.Errorf("the refusal does not say what is unsupported: %s", stderr)
			}
			if !strings.Contains(stderr, "0.6") {
				t.Errorf("the refusal does not point at where the decision is written: %s", stderr)
			}
		})
	}
}

// TestOtherPassphraseDoesNotFallBack is a unit test, and it exists because the first
// version of otherPassphrase() DID fall back to BORGE_PASSPHRASE.
//
// borg's Passphrase.env_passphrase(other=True) reads BORG_OTHER_PASSPHRASE alone and then
// prompts; running "borg repo-create --other-repo" with only BORG_PASSPHRASE set asks
// interactively and fails in a script. Falling back would be friendlier and would mean a
// command line that works under borge hangs under borg.
func TestOtherPassphraseDoesNotFallBack(t *testing.T) {
	env := func(vars map[string]string) *Env {
		return &Env{Getenv: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		}}
	}

	e := env(map[string]string{"BORGE_PASSPHRASE": "main"})
	if got := e.otherPassphrase(); got != "" {
		t.Errorf("with only BORGE_PASSPHRASE set, otherPassphrase() = %q, want empty", got)
	}
	if got := e.passphrase(); got != "main" {
		t.Errorf("passphrase() = %q, want the main one", got)
	}

	e = env(map[string]string{"BORGE_PASSPHRASE": "main", "BORGE_OTHER_PASSPHRASE": "other"})
	if got := e.otherPassphrase(); got != "other" {
		t.Errorf("otherPassphrase() = %q, want %q", got, "other")
	}

	// The BORG_ spelling works too, as it does for every other variable.
	e = env(map[string]string{"BORG_OTHER_PASSPHRASE": "borg-spelling"})
	if got := e.otherPassphrase(); got != "borg-spelling" {
		t.Errorf("otherPassphrase() = %q, want the BORG_ spelling to be honoured", got)
	}
}

// TestArchiveNameAndCommentValidation pins the two validators against borg's, including
// the wording, because the message is what tells a user which archive to rename.
//
// borge originally validated neither: create accepted a name borg's parser rejects, so a
// borge-written repository could hold an archive that borg's own transfer would then
// refuse to move. The rules are borg's, measured from borg's validators:
//
//	name:    1..200 characters, no control characters, none of /\"<|>?*,
//	         no leading or trailing blank, valid unicode
//	comment: at most 10000 characters, no NUL, valid unicode
//
// Lengths are counted in characters and not bytes, as Python counts them.
func TestArchiveNameAndCommentValidation(t *testing.T) {
	for _, c := range []struct {
		name, want string
	}{
		{"", `Invalid archive name: "" [length < 1]`},
		{strings.Repeat("x", 201), `[length > 200]`},
		{"a/b", `Invalid archive name: "a/b" [invalid chars detected matching "/\"<|>?*"]`},
		{"a\tb", "[invalid control chars detected]"},
		{" lead", `Invalid archive name: " lead" [leading or trailing blanks detected]`},
		{"trail ", `[leading or trailing blanks detected]`},
	} {
		err := validateArchiveName(c.name)
		if err == nil {
			t.Errorf("validateArchiveName(%q) accepted it", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("validateArchiveName(%q) = %q, want it to contain %q", c.name, err, c.want)
		}
	}
	// 200 characters, not 200 bytes: an accented name of the same length must pass.
	for _, ok := range []string{"x", strings.Repeat("x", 200), strings.Repeat("é", 200),
		"2026-08-20T12:00:00", "a:b", "with space inside"} {
		if err := validateArchiveName(ok); err != nil {
			t.Errorf("validateArchiveName(%q) refused it: %v", ok, err)
		}
	}

	if err := validateComment(strings.Repeat("c", 10000)); err != nil {
		t.Errorf("a 10000 character comment was refused: %v", err)
	}
	if err := validateComment(strings.Repeat("c", 10001)); err == nil {
		t.Error("a 10001 character comment was accepted")
	}
	// A comment may hold newlines and tabs - only NUL is out.
	if err := validateComment("two\nlines\tand a tab"); err != nil {
		t.Errorf("a multi-line comment was refused: %v", err)
	}
	if err := validateComment("a\x00b"); err == nil {
		t.Error("a comment containing NUL was accepted")
	}
}

// TestCreateRefusesNamesBorgWould checks the validators are actually wired to the commands
// that write names, not just present in the package.
func TestCreateRefusesNamesBorgWould(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what string
		args []string
	}{
		{"create with a slash in the name", []string{"create", "a/b", src}},
		{"create with a trailing blank", []string{"create", "trailing ", src}},
		// A NUL would be the sharper case, but neither tool can be handed one: an argv
		// string ends at the first NUL, so the length rule is what is testable here.
		{"create with an over-long comment",
			[]string{"create", "--comment", strings.Repeat("c", 10001), "ok", src}},
		{"recreate with an empty target", []string{"recreate", "--target", ""}},
		{"import-tar with a slash in the name", []string{"import-tar", "a/b", "/dev/null"}},
	} {
		t.Run(c.what, func(t *testing.T) {
			// borg refuses it at parse time, before it touches the repository.
			if out, err := r.runErr(c.args...); err == nil {
				t.Fatalf("borg accepted it, so this test asserts a rule borg does not have:\n%s", out)
			} else if !strings.Contains(out, "Invalid archive name") &&
				!strings.Contains(out, "Invalid comment") {
				t.Fatalf("borg refused it for another reason:\n%s", out)
			}
			_, stderr, code := r.borge(t, c.args...)
			if code != ExitError {
				t.Fatalf("borge accepted it (exit %d)\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "Invalid archive name") &&
				!strings.Contains(stderr, "Invalid comment") {
				t.Errorf("borge refused it with a different message: %s", stderr)
			}
		})
	}
}

// TestIDHashFamilies is borg's uses_same_id_hash, one pair at a time.
//
// It is a table because the interesting cases are the ones nobody runs by hand: the
// blake3 modes reached a `f[:7]` slice on a six-character family name, so
// `borge transfer` between two blake3 repositories panicked - on the guard that exists to
// make transfer safe. The mode names are borge's canonical ones, which carry the id hash
// because borg's `-e aes256-ocb` names two different key classes depending on `--id-hash`.
func TestIDHashFamilies(t *testing.T) {
	related := [][2]string{
		// Same family, different encryption: allowed, and this is the case the feature is
		// for - re-keying a repository while keeping its deduplication.
		{"aes256-ocb", "chacha20-poly1305"},
		{"aes256-ocb", "authenticated-sha256"},
		{"sha256-aes256-ocb", "aes256-ocb"},
		{"blake3-aes256-ocb", "blake3-chacha20-poly1305"},
		{"blake3-chacha20-poly1305", "authenticated-blake3"},
		{"none-sha256", "none-sha256"},
		{"none-blake3", "none-blake3"},
	}
	for _, p := range related {
		if !usesSameIDHash(p[0], p[1]) {
			t.Errorf("%s -> %s: refused, but both hash ids the same way", p[0], p[1])
		}
	}

	unrelated := [][2]string{
		{"aes256-ocb", "blake3-aes256-ocb"},  // keyed sha256 vs keyed blake3
		{"aes256-ocb", "none-sha256"},        // keyed vs unkeyed, both sha256
		{"none-sha256", "none-blake3"},       // unkeyed, different hash
		{"blake3-aes256-ocb", "none-blake3"}, // keyed vs unkeyed blake3
		{"aes256-ocb", "made-up"},            // an unknown mode is never "the same"
		{"made-up", "made-up"},               //
	}
	for _, p := range unrelated {
		if usesSameIDHash(p[0], p[1]) {
			t.Errorf("%s -> %s: allowed, but the ids would not line up", p[0], p[1])
		}
	}
}

// TestTransferBetweenBlake3Repositories walks the whole path in a keyed-BLAKE3 mode.
//
// Every other transfer test here uses an HMAC-SHA256 mode, which is what made the family
// table's blake3 row - the one whose name is six characters long - reachable only by a
// user. This is that user.
func TestTransferBetweenBlake3Repositories(t *testing.T) {
	r := newBorgRepo(t, "authenticated-blake3")
	transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")

	if _, stderr, code := r.borgeOther(t, "repo-create", "-r", dst, "--other-repo", r.path,
		"-e", "authenticated-blake3"); code != ExitOK {
		t.Fatalf("repo-create --other-repo in a blake3 mode exited %d\n%s", code, stderr)
	}
	stdout, stderr, code := r.borgeOther(t, "transfer", "-r", dst, "--other-repo", r.path)
	if code != ExitOK {
		t.Fatalf("transfer between blake3 repositories exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "finished") {
		t.Errorf("transfer moved no archives:\n%s", stdout)
	}
	if out, err := r.runErr("check", "--verify-data", "-r", dst); err != nil {
		t.Errorf("borg check on the transferred blake3 repository: %v\n%s", err, out)
	}
	if got := archiveNamesIn(t, r, dst); len(got) != 2 {
		t.Errorf("destination holds %v, want both archives", got)
	}
}

// TestTransferWithDifferentPassphrases is the case every other test here hides.
//
// The two repositories are allowed to have different passphrases - that is most of the
// point of re-keying while transferring - but the test harness gives both the same one, so
// unlocking the source with the *destination's* passphrase passes every test above and
// fails for the user who set them apart. Both commands that open a source repository are
// checked: repo-create --other-repo and transfer.
func TestTransferWithDifferentPassphrases(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")

	// The destination gets a passphrase of its own, and the source's goes in
	// BORGE_OTHER_PASSPHRASE where it belongs.
	env := map[string]string{
		"BORGE_PASSPHRASE":       "the destination's own passphrase",
		"BORGE_OTHER_PASSPHRASE": r.passphrase,
	}
	if _, stderr, code := r.borgeWithEnv(t, env, "repo-create", "-r", dst,
		"--other-repo", r.path, "-e", "aes256-ocb"); code != ExitOK {
		t.Fatalf("repo-create --other-repo with two passphrases exited %d\n%s", code, stderr)
	}
	stdout, stderr, code := r.borgeWithEnv(t, env, "transfer", "-r", dst, "--other-repo", r.path)
	if code != ExitOK {
		t.Fatalf("transfer with two passphrases exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "finished") {
		t.Errorf("transfer moved no archives:\n%s", stdout)
	}
	// Listed by borg, under the destination's passphrase - the harness's default is the
	// source's, which the destination must no longer accept.
	out, err := r.runErrEnv([]string{"BORG_PASSPHRASE=the destination's own passphrase"},
		"repo-list", "-r", dst, "--format", "{archive}{NL}")
	if err != nil {
		t.Fatalf("borg could not list the transferred repository: %v\n%s", err, out)
	}
	if n := len(strings.Fields(out)); n != 2 {
		t.Fatalf("destination holds %d archive(s), want both:\n%s", n, out)
	}

	// And the destination really is locked with the new passphrase, not the old one: a
	// repository that silently kept the source's would be the same bug reported as a pass.
	if _, _, code := r.borgeWithEnv(t, map[string]string{"BORGE_PASSPHRASE": r.passphrase},
		"repo-list", "-r", dst); code == ExitOK {
		t.Error("the destination opened with the source's passphrase")
	}
}

// TestRepoCreateInheritsFromTheEnvironment: BORGE_OTHER_REPO alone makes a related
// repository, with no --other-repo on the command line.
//
// borg spells this as an argparse default - --other-repo defaults to Location(other=True),
// which reads BORG_OTHER_REPO - so the option being absent does not mean there is no source.
// Measured on borg before it was ported: with only the variable set, repo-create asks for
// the *source* repository's passphrase, and the repository it makes is one transfer accepts.
// borge read the flag and nothing else, so the same command made an unrelated repository,
// and the surprise would have arrived one transfer later. DIVERGENCES.md #57.
func TestRepoCreateInheritsFromTheEnvironment(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	transferRepos(t, r)
	dst := filepath.Join(t.TempDir(), "dst")
	plain := filepath.Join(t.TempDir(), "plain")

	if _, stderr, code := r.borgeWithEnv(t, map[string]string{
		"BORGE_OTHER_REPO":       r.path,
		"BORGE_OTHER_PASSPHRASE": r.passphrase,
	}, "repo-create", "-r", dst, "-e", "aes256-ocb"); code != ExitOK {
		t.Fatalf("borge repo-create with BORGE_OTHER_REPO exited %d\n%s", code, stderr)
	}

	// borg is the judge of whether the key material really was inherited: its own transfer
	// refuses a repository that is not related, so a dry run that finds work to do is proof
	// that the id key and the chunk seed came across.
	out, err := r.runErrOther("transfer", "-r", dst, "--other-repo", r.path, "--dry-run")
	if err != nil {
		t.Fatalf("borg refuses the repository borge made from BORGE_OTHER_REPO: %v\n%s", err, out)
	}
	if !strings.Contains(out, "incomplete") {
		t.Errorf("borg's dry run reported no archives to transfer:\n%s", out)
	}

	// The control, without which the assertion above would pass for a repository that is
	// related to everything: the same command with no source in the environment.
	if _, stderr, code := r.borge(t, "repo-create", "-r", plain, "-e", "aes256-ocb"); code != ExitOK {
		t.Fatalf("borge repo-create exited %d\n%s", code, stderr)
	}
	out, err = r.runErrOther("transfer", "-r", plain, "--other-repo", r.path, "--dry-run")
	if err == nil {
		t.Fatalf("borg transferred into an unrelated repository:\n%s", out)
	}
	if !strings.Contains(out, "chunker secret") {
		t.Errorf("borg refused for some other reason than relatedness:\n%s", out)
	}
}
