// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reserveFiles lists the reservation objects on disk, which is where both tools look.
func reserveFiles(t *testing.T, repo string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(repo, "config", reservePrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// TestRepoSpaceReservesAndFrees walks the whole cycle, because the halves are only useful
// together: a reserve that cannot be freed is just wasted disk.
func TestRepoSpaceReservesAndFrees(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	stdout, stderr, code := r.borge(t, "repo-space")
	if code != ExitOK {
		t.Fatalf("repo-space exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "0 B") {
		t.Errorf("a fresh repository does not report zero reserved space:\n%s", stdout)
	}

	// One block. 64 MiB is borg's granularity and is not negotiable, so the smallest
	// meaningful reservation is one block.
	if _, stderr, code := r.borge(t, "repo-space", "--reserve", "1M"); code != ExitOK {
		t.Fatalf("repo-space --reserve exited %d\n%s", code, stderr)
	}
	files := reserveFiles(t, r.path)
	if len(files) != 1 {
		t.Fatalf("--reserve 1M created %d object(s), want 1 (rounded up to one 64 MiB block)", len(files))
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != reserveBlockSize {
		t.Errorf("the reservation object is %d bytes, want %d", info.Size(), reserveBlockSize)
	}

	stdout, _, _ = r.borge(t, "repo-space")
	if !strings.Contains(stdout, "67.11 MB") {
		t.Errorf("repo-space does not report the 64 MiB block it just wrote:\n%s", stdout)
	}

	stdout, stderr, code = r.borge(t, "repo-space", "--free")
	if code != ExitOK {
		t.Fatalf("repo-space --free exited %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "freed") {
		t.Errorf("--free did not say what it freed:\n%s", stdout)
	}
	if files := reserveFiles(t, r.path); len(files) != 0 {
		t.Errorf("%d reservation object(s) survived --free: %v", len(files), files)
	}
}

// TestRepoSpaceReserveReplacesRatherThanAccumulates.
//
// "--reserve 1G" means "let there be 1G". If running it twice reserved two gigabytes, a
// cron job that reserves after every backup would fill the disk it is meant to protect.
func TestRepoSpaceReserveReplacesRatherThanAccumulates(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	for i := 0; i < 3; i++ {
		if _, stderr, code := r.borge(t, "repo-space", "--reserve", "1M"); code != ExitOK {
			t.Fatalf("repo-space --reserve exited %d\n%s", code, stderr)
		}
	}
	if files := reserveFiles(t, r.path); len(files) != 1 {
		t.Errorf("three reservations of the same size left %d object(s), want 1: %v",
			len(files), files)
	}
}

// TestRepoSpaceInteroperatesWithBorg: both tools manage the same objects, so each has to
// see and be able to free what the other reserved.
//
// The failure this guards against is silent. If the two disagreed on the object naming,
// "borge repo-space --free" during a disk-full emergency would report freeing nothing
// while borg's reservation sat there untouched.
func TestRepoSpaceInteroperatesWithBorg(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	// borg reserves, borge reports and frees.
	r.mustRun("repo-space", "-r", r.path, "--reserve", "1M")
	stdout, stderr, code := r.borge(t, "repo-space")
	if code != ExitOK {
		t.Fatalf("repo-space exited %d\n%s", code, stderr)
	}
	if strings.Contains(stdout, "there is 0 B") {
		t.Fatalf("borge does not see the space borg reserved:\n%s", stdout)
	}
	if _, stderr, code := r.borge(t, "repo-space", "--free"); code != ExitOK {
		t.Fatalf("repo-space --free exited %d\n%s", code, stderr)
	}
	if files := reserveFiles(t, r.path); len(files) != 0 {
		t.Errorf("borge did not free borg's reservation: %v", files)
	}

	// borge reserves, borg reports and frees.
	if _, stderr, code := r.borge(t, "repo-space", "--reserve", "1M"); code != ExitOK {
		t.Fatalf("repo-space --reserve exited %d\n%s", code, stderr)
	}
	out := r.mustRun("repo-space", "-r", r.path)
	if strings.Contains(out, "There is 0 B") {
		t.Fatalf("borg does not see the space borge reserved:\n%s", out)
	}
	r.mustRun("repo-space", "-r", r.path, "--free")
	if files := reserveFiles(t, r.path); len(files) != 0 {
		t.Errorf("borg did not free borge's reservation: %v", files)
	}
}

// TestRepoSpaceRejectsContradictoryFlags rather than silently preferring one.
func TestRepoSpaceRejectsContradictoryFlags(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	_, stderr, code := r.borge(t, "repo-space", "--reserve", "1M", "--free")
	if code != ExitError {
		t.Fatalf("--reserve with --free exited %d, want ExitError (%d)", code, ExitError)
	}
	if !strings.Contains(stderr, "opposites") {
		t.Errorf("the error does not explain the conflict: %q", stderr)
	}
}

// TestParseFileSizeIsDecimal pins the unit convention.
//
// borg parses "1G" as a decimal gigabyte, and the two tools manage the same reservation
// objects, so reading it as 2^30 would make one tool's "1G" 7% larger than the other's.
func TestParseFileSizeIsDecimal(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1234", 1234},
		{"1k", 1_000},
		{"1K", 1_000},
		{"1M", 1_000_000},
		{"1G", 1_000_000_000},
		{"1.5G", 1_500_000_000},
		{"2T", 2_000_000_000_000},
	} {
		got, err := parseFileSize(tc.in)
		if err != nil {
			t.Errorf("parseFileSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseFileSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// A trailing letter that is not a known suffix is an error, not a silently ignored
	// one: "--reserve 1GB" meaning one byte would be an expensive misreading.
	for _, bad := range []string{"", "GB", "1GB", "-5M", "one", "1 000"} {
		if got, err := parseFileSize(bad); err == nil {
			t.Errorf("parseFileSize(%q) = %d, want an error", bad, got)
		}
	}
}

// TestFormatBytesMatchesBorgsUnits pins the output shape, including BORG_UNITS.
func TestFormatBytesMatchesBorgsUnits(t *testing.T) {
	for _, tc := range []struct {
		n     int64
		units string
		want  string
	}{
		{0, "si", "0 B"},
		{999, "si", "999 B"},
		{1000, "si", "1.00 kB"},
		{1_500_000, "si", "1.50 MB"},
		{67_108_864, "si", "67.11 MB"},
		{1024, "iec", "1.00 KiB"},
		{67_108_864, "iec", "64.00 MiB"},
		{67_108_864, "raw", "67108864 B"},
	} {
		if got := formatBytesIn(tc.n, tc.units); got != tc.want {
			t.Errorf("formatBytesIn(%d, %q) = %q, want %q", tc.n, tc.units, got, tc.want)
		}
	}
}
