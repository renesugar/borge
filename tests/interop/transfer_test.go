// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"path/filepath"
	"strings"
	"testing"
)

// transfer, across the tools.
//
// A transfer is the one operation that reads a whole repository and writes a second one,
// and it is the operation a user runs when they are migrating - so a difference here is
// found at the worst possible moment. Three things have to hold across the pair:
//
//   - the destination one tool prepares with --other-repo is one the OTHER tool will
//     transfer into: that means the inherited id key and chunk seed are stored where the
//     other tool looks for them, and its relatedness guards accept them;
//   - --recompress never moves the compressed payload as-is, so the destination ends up
//     holding blobs the transferring tool never decompressed - and the other tool has to
//     read them;
//   - --recompress always re-compresses, so the destination holds the same content under
//     a different compressor, and ids still have to match.
//
// The archives being moved were written by both tools, so neither is only ever moving its
// own output.

// extractFrom restores an archive from an arbitrary repository, which extractWith cannot
// do: it always uses tl.repo.
func (tl *tools) extractFrom(bin, repo, archive, sourcePath string) string {
	tl.t.Helper()
	dest := tl.t.TempDir()
	var err error
	if bin == tl.borg {
		_, err = tl.run(bin, dest, "extract", "-r", repo, archive)
	} else {
		_, err = tl.run(bin, "", "extract", "-r", repo, "-C", dest, archive)
	}
	if err != nil {
		tl.t.Fatal(err)
	}
	return filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(sourcePath), "/")))
}

func TestTransferBetweenTools(t *testing.T) {
	cases := []struct {
		name string
		// which tool prepares the related destination, and which one moves the archives:
		// the two are deliberately not always the same tool.
		creates, transfers string
		recompress         []string
	}{
		{"borge-creates-borge-transfers-keeping-compression", "borge", "borge",
			[]string{"--recompress", "never"}},
		{"borge-creates-borg-transfers-recompressing", "borge", "borg",
			[]string{"--recompress", "always", "-C", "zstd,3"}},
		{"borg-creates-borge-transfers-keeping-compression", "borg", "borge",
			[]string{"--recompress", "never"}},
		{"borg-creates-borge-transfers-recompressing", "borg", "borge",
			[]string{"--recompress", "always", "-C", "zstd,3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := newTools(t, "aes256-ocb")
			src := syntheticTree(t)
			// lz4 by default here, so --recompress always has something to change.
			tl.mustBorg("create", "-r", tl.repo, "-C", "lz4", "by-borg", src)
			tl.mustBorge("create", "-C", "lz4", "by-borge", src)

			bin := func(name string) string {
				if name == "borg" {
					return tl.borg
				}
				return tl.borge
			}
			other := func(name string) string {
				if name == "borg" {
					return tl.borge
				}
				return tl.borg
			}

			dst := filepath.Join(t.TempDir(), "dst")
			if out, err := tl.run(bin(c.creates), "", "repo-create", "-r", dst,
				"--other-repo", tl.repo, "-e", "aes256-ocb"); err != nil {
				t.Fatalf("%s repo-create --other-repo: %v\n%s", c.creates, err, out)
			}

			args := append([]string{"transfer", "-r", dst, "--other-repo", tl.repo}, c.recompress...)
			out, err := tl.run(bin(c.transfers), "", args...)
			if err != nil {
				t.Fatalf("%s transfer: %v\n%s", c.transfers, err, out)
			}

			// Both tools have to accept the result, chunk by chunk.
			for _, name := range []string{"borg", "borge"} {
				if out, err := tl.run(bin(name), "", "check", "--verify-data", "-r", dst); err != nil {
					t.Errorf("%s check --verify-data on the transferred repository: %v\n%s",
						name, err, out)
				}
			}

			// And the archives have to restore to the original tree - read by the tool
			// that did NOT do the transfer, which is the reader that never saw the
			// decisions the transfer made.
			reader := other(c.transfers)
			for _, archive := range []string{"by-borg", "by-borge"} {
				t.Run("restore-"+archive, func(t *testing.T) {
					checkTrees(t, src, tl.extractFrom(reader, dst, archive, src), false)
				})
			}
		})
	}
}

// TestTransferRefusesAnUnrelatedRepositoryLikeBorg pins the refusal against borg itself
// rather than against a string in borge's source.
//
// The reason it matters that this is a refusal and not a warning: transferring into an
// unrelated repository succeeds. It just stores every chunk again under a new id, so the
// user pays for a full copy and gets no deduplication, and nothing in the output says so.
func TestTransferRefusesAnUnrelatedRepositoryLikeBorg(t *testing.T) {
	tl := newTools(t, "aes256-ocb")
	src := syntheticTree(t)
	tl.mustBorge("create", "by-borge", src)

	dst := filepath.Join(t.TempDir(), "dst")
	tl.mustBorg("repo-create", "-r", dst, "-e", "aes256-ocb") // unrelated on purpose

	borgOut, borgErr := tl.run(tl.borg, "", "transfer", "-r", dst, "--other-repo", tl.repo)
	if borgErr == nil {
		t.Fatalf("borg accepted an unrelated destination:\n%s", borgOut)
	}
	borgeOut, borgeErr := tl.run(tl.borge, "", "transfer", "-r", dst, "--other-repo", tl.repo)
	if borgeErr == nil {
		t.Fatalf("borge accepted an unrelated destination:\n%s", borgeOut)
	}

	const want = "You must use the same chunker secret or deduplication will break. " +
		"Use a related repository!"
	if !strings.Contains(borgOut, want) {
		t.Fatalf("borg's refusal is not the one this test pins:\n%s", borgOut)
	}
	if !strings.Contains(borgeOut, want) {
		t.Errorf("borge's refusal differs from borg's:\n got: %s\nwant it to contain: %s",
			borgeOut, want)
	}
}
