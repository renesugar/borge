// SPDX-License-Identifier: Apache-2.0

package item

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestFormatModeMatchesPython compares against stat.filemode, which is what borg's
// listing uses. It runs against the system python3 rather than the borg venv: filemode is
// standard library, so there is nothing borg-specific to pin.
func TestFormatModeMatchesPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	var modes []int64
	// Every file type, with a spread of permission and special bits - including the
	// combinations that distinguish "s" from "S" and "t" from "T", which is the part of
	// the table that is easy to get wrong.
	for _, typ := range []int64{SIFREG, SIFDIR, SIFLNK, SIFSOCK, SIFIFO, SIFBLK, SIFCHR, 0} {
		for _, perm := range []int64{
			0, 0o777, 0o644, 0o755, 0o600, 0o400, 0o111, 0o007, 0o070, 0o700,
			0o4755, 0o4644, // setuid with and without execute
			0o2755, 0o2644, // setgid with and without execute
			0o1777, 0o1666, // sticky with and without execute
			0o7777, 0o6644,
		} {
			modes = append(modes, typ|perm)
		}
	}

	var args []string
	for _, m := range modes {
		args = append(args, strconv.FormatInt(m, 10))
	}
	script := `
import stat, sys
for a in sys.argv[1:]:
    print(stat.filemode(int(a)))
`
	cmd := exec.Command("python3", append([]string{"-c", script}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python3: %v\n%s", err, out)
	}
	want := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(want) != len(modes) {
		t.Fatalf("python printed %d lines for %d modes", len(want), len(modes))
	}

	for i, m := range modes {
		if got := FormatMode(m); got != want[i] {
			t.Errorf("mode 0o%o: borge says %q, python says %q", m, got, want[i])
		}
	}
	t.Logf("compared %d modes", len(modes))
}

func TestModeClassification(t *testing.T) {
	cases := []struct {
		mode                             int64
		dir, link, regular, device, fifo bool
		typeChar                         string
	}{
		{SIFDIR | 0o755, true, false, false, false, false, "d"},
		{SIFREG | 0o644, false, false, true, false, false, "-"},
		{SIFLNK | 0o777, false, true, false, false, false, "l"},
		{SIFBLK | 0o660, false, false, false, true, false, "b"},
		{SIFCHR | 0o660, false, false, false, true, false, "c"},
		{SIFIFO | 0o644, false, false, false, false, true, "p"},
	}
	for _, tc := range cases {
		if IsDir(tc.mode) != tc.dir || IsSymlink(tc.mode) != tc.link ||
			IsRegular(tc.mode) != tc.regular || IsDevice(tc.mode) != tc.device ||
			IsFIFO(tc.mode) != tc.fifo {
			t.Errorf("mode 0o%o classified wrongly", tc.mode)
		}
		if got := TypeChar(tc.mode); got != tc.typeChar {
			t.Errorf("mode 0o%o: type char %q, want %q", tc.mode, got, tc.typeChar)
		}
	}
}

func TestContentSize(t *testing.T) {
	it := &Item{Chunks: []ChunkListEntry{{Size: 100}, {Size: 23}}, ChunksSet: true}
	if got := it.ContentSize(); got != 123 {
		t.Errorf("ContentSize is %d, want 123", got)
	}
	empty := &Item{ChunksSet: true}
	if got := empty.ContentSize(); got != 0 {
		t.Errorf("an empty chunk list gives %d, want 0", got)
	}
}
