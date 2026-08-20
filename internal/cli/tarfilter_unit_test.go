// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"
)

// The suffix table and the command splitter, tested apart from the commands.
//
// Both have cases that cost an external program to exercise through export-tar, and one -
// the unterminated quote - that has no output at all to compare.

func TestTarFilterFor(t *testing.T) {
	cases := []struct {
		name              string
		compress, extract string
	}{
		{"backup.tar.gz", "gzip", "gzip -d"},
		{"backup.tgz", "gzip", "gzip -d"},
		{"backup.tar.bz2", "bzip2", "bzip2 -d"},
		{"backup.tbz", "bzip2", "bzip2 -d"},
		{"backup.tar.xz", "xz", "xz -d"},
		{"backup.txz", "xz", "xz -d"},
		{"backup.tar.lz4", "lz4", "lz4 -d"},
		// zstd is (de)compressed in-process in both tools, so the same sentinel either way.
		{"backup.tar.zst", inProcessZstd, inProcessZstd},
		{"backup.tar.zstd", inProcessZstd, inProcessZstd},
		{"backup.tzst", inProcessZstd, inProcessZstd},
		// Not borg's suffixes. The bare ".gz" is the one borge used to compress.
		{"backup.tar", "", ""},
		{"backup.gz", "", ""},
		{"backup.xz", "", ""},
		{"backup", "", ""},
		{"-", "", ""},
		{"", "", ""},
		// The suffix is matched, not the extension: a name ending in one counts.
		{"weird.name.tar.gz", "gzip", "gzip -d"},
	}
	for _, c := range cases {
		if got := tarFilterFor(c.name, false); got != c.compress {
			t.Errorf("tarFilterFor(%q, compress) = %q, want %q", c.name, got, c.compress)
		}
		if got := tarFilterFor(c.name, true); got != c.extract {
			t.Errorf("tarFilterFor(%q, decompress) = %q, want %q", c.name, got, c.extract)
		}
	}
}

// TestSplitCommandLine covers Python's shlex.split in POSIX mode, which is what borg uses
// and therefore what "--tar-filter" means.
func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"gzip", []string{"gzip"}},
		{"gzip -9", []string{"gzip", "-9"}},
		{"  gzip   -9  ", []string{"gzip", "-9"}},
		{"", nil},
		{"   ", nil},
		{`zstd -T0 --long=27`, []string{"zstd", "-T0", "--long=27"}},
		// Quotes group, and are removed.
		{`sh -c 'gzip -9'`, []string{"sh", "-c", "gzip -9"}},
		{`prog "two words"`, []string{"prog", "two words"}},
		{`prog "a b" c`, []string{"prog", "a b", "c"}},
		// An empty quoted string is an argument, which is why "started" is tracked apart
		// from the buffer being non-empty.
		{`prog ""`, []string{"prog", ""}},
		{`prog ''`, []string{"prog", ""}},
		// A backslash escapes outside quotes and before these four inside double quotes.
		{`prog a\ b`, []string{"prog", "a b"}},
		{`prog "a\"b"`, []string{"prog", `a"b`}},
		{`prog "a\\b"`, []string{"prog", `a\b`}},
		// ...and stays a backslash before anything else, as POSIX shells do.
		{`prog "a\nb"`, []string{"prog", `a\nb`}},
		// Single quotes are literal: a backslash inside them is a backslash.
		{`prog 'a\b'`, []string{"prog", `a\b`}},
		// Adjacent quoting joins rather than splitting.
		{`prog a"b c"d`, []string{"prog", "ab cd"}},
	}
	for _, c := range cases {
		got, err := splitCommandLine(c.in)
		if err != nil {
			t.Errorf("splitCommandLine(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitCommandLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{`prog "unterminated`, `prog 'unterminated`, `prog trailing\`} {
		if got, err := splitCommandLine(bad); err == nil {
			t.Errorf("splitCommandLine(%q) = %q, want an error", bad, got)
		}
	}
}
