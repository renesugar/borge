// SPDX-License-Identifier: Apache-2.0

//go:build linux

package archive

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/renesugar/borge/internal/crypto/key"
	"github.com/renesugar/borge/internal/item"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/repository"
)

// The stage 6 gate: `borge create` produces an archive that borg accepts and can restore.
//
// Two assertions, and the second is the one that matters:
//
//   - `borg check --verify-data` passes, which reads every chunk of every archive and
//     re-verifies it against its id. That is borg's own statement that the repository is
//     internally consistent.
//   - `borg extract` of a borge-written archive reproduces the source tree under the
//     strict comparator, so the restore is not merely valid but *right*.

// createBuilder opens a repository and starts an archive builder on it.
func createBuilder(t *testing.T, r *borgRepo) (*repository.Repository, *Builder) {
	t.Helper()
	repo, err := repository.Open(r.path, repository.Options{Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	k, unlocked, err := repo.Unlock(r.passphrase)
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	m, err := manifest.Load(repo, k, manifest.OpWrite)
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	var seed uint32
	if unlocked != nil {
		seed = uint32(key.ChunkSeed(unlocked.Material))
	}
	b, err := NewBuilder(m, BuilderOptions{ChunkSeed: seed})
	if err != nil {
		repo.Close()
		t.Fatal(err)
	}
	return repo, b
}

// TestBorgReadsBorgeCreatedArchive is the gate.
func TestBorgReadsBorgeCreatedArchive(t *testing.T) {
	for _, encryption := range []string{"aes256-ocb", "none-sha256"} {
		t.Run(encryption, func(t *testing.T) {
			r := newBorgRepo(t, encryption)
			src := extractTree(t)

			repo, b := createBuilder(t, r)
			stats, err := b.Create(CreateOptions{
				Paths: []string{src},
				OnError: func(p string, err error) error {
					t.Errorf("borge could not archive %s: %v", p, err)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.Save(SaveOptions{Name: "borge-made", Comment: "written by borge"}); err != nil {
				t.Fatal(err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("archived %d files, %d bytes, %d chunks (%d new)",
				stats.NFiles, stats.OriginalSize, stats.Chunks, stats.NewChunks)
			if stats.NFiles == 0 {
				t.Fatal("nothing was archived")
			}

			// borg's own full verification.
			if out, err := r.run("check", "--verify-data", "-r", r.path); err != nil {
				t.Fatalf("borg check --verify-data failed on a borge-written repository: %v\n%s", err, out)
			}

			// borg lists it with the metadata borge wrote.
			listed := r.mustRun("repo-list", "-r", r.path)
			if !strings.Contains(listed, "borge-made") {
				t.Fatalf("borg does not list borge's archive:\n%s", listed)
			}

			// And borg restores it to something equal to the source.
			borgDir := t.TempDir()
			cmd := exec.Command(r.binary, "extract", "-r", r.path, "borge-made")
			cmd.Env = r.env()
			cmd.Dir = borgDir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("borg extract of a borge-written archive: %v\n%s", err, out)
			}

			rel := strings.TrimPrefix(filepath.ToSlash(src), "/")
			restored := filepath.Join(borgDir, filepath.FromSlash(rel))

			want, err := scanTree(src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := scanTree(restored)
			if err != nil {
				t.Fatalf("scanning borg's restore of borge's archive: %v", err)
			}
			if len(want) == 0 {
				t.Fatal("the source tree is empty; the comparison would be vacuous")
			}

			diffs := compareTrees(want, got, compareOptions{CheckOwner: true, CheckACLs: true})
			for _, d := range diffs {
				t.Error(d)
			}
			if len(diffs) > 0 {
				t.Errorf("%d difference(s) between the source and borg's restore of borge's archive", len(diffs))
			}
			t.Logf("compared %d entries", len(want))
		})
	}
}

// TestBorgeCreateDeduplicates: the second archive of an unchanged tree must store almost
// nothing new. Deduplication is the whole point of the program, and a chunker or id-hash
// mistake shows up here as an archive that stores everything again.
func TestBorgeCreateDeduplicates(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")
	src := extractTree(t)

	var firstNew, secondNew int64
	for i, name := range []string{"first", "second"} {
		repo, b := createBuilder(t, r)
		if _, err := b.Create(CreateOptions{Paths: []string{src}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.Save(SaveOptions{Name: name}); err != nil {
			t.Fatal(err)
		}
		s := b.Stats()
		if i == 0 {
			firstNew = s.NewChunks
		} else {
			secondNew = s.NewChunks
		}
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
	}

	t.Logf("first archive stored %d new chunks, second stored %d", firstNew, secondNew)
	if firstNew == 0 {
		t.Fatal("the first archive stored nothing")
	}
	// The second archive re-stores only its own metadata objects: the item stream is
	// identical, so its chunks dedup too, leaving the archive object and its pointer
	// block. Anything close to the first archive's count means deduplication is broken.
	if secondNew > 4 {
		t.Errorf("the second archive of an unchanged tree stored %d new chunks; deduplication is not working",
			secondNew)
	}
}

// TestBorgeAndBorgProduceTheSameChunks is the strongest statement about the chunker: for
// the same tree and the same key, both tools must produce the *same chunk ids*, or the
// two would never deduplicate against each other.
func TestBorgeAndBorgProduceTheSameChunks(t *testing.T) {
	r := newBorgRepo(t, "aes256-ocb")

	src := t.TempDir()
	// Big enough and irregular enough that the content-defined chunker actually splits it
	// in several places, so the comparison is about boundaries and not just about hashing.
	var content strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&content, "line %d of a file that should be chunked in several places\n", i)
	}
	if err := os.WriteFile(filepath.Join(src, "big.txt"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// borg archives it first.
	r.mustRun("create", "-r", r.path, "by-borg", src)
	borgChunks := chunkIDsOf(t, r, "by-borg")

	// Then borge, into the same repository.
	repo, b := createBuilder(t, r)
	if _, err := b.Create(CreateOptions{Paths: []string{src}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Save(SaveOptions{Name: "by-borge"}); err != nil {
		t.Fatal(err)
	}
	newChunks := b.Stats().NewChunks
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	borgeChunks := chunkIDsOf(t, r, "by-borge")
	if len(borgChunks) < 2 {
		t.Fatalf("borg split the file into %d chunk(s); this test needs several", len(borgChunks))
	}
	if strings.Join(borgeChunks, ",") != strings.Join(borgChunks, ",") {
		t.Errorf("the two tools chunked the same file differently\n  borge (%d): %v\n  borg  (%d): %v",
			len(borgeChunks), borgeChunks, len(borgChunks), borgChunks)
	}
	t.Logf("both tools produced %d identical content chunks; borge stored %d new objects "+
		"(metadata only)", len(borgChunks), newChunks)
}

// chunkIDsOf returns the chunk ids of the one content file in an archive, via borge's own
// reader - which is the reader both cases go through, so the comparison is about what was
// written rather than about how it is read.
func chunkIDsOf(t *testing.T, r *borgRepo, archiveName string) []string {
	t.Helper()
	m := r.open(t)
	a, err := OpenByName(m, archiveName)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	err = a.Items(func(it *item.Item) error {
		if it.IsRegular() {
			for _, c := range it.Chunks {
				ids = append(ids, fmt.Sprintf("%x", c.ID))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
