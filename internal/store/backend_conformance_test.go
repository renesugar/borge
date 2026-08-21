// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
)

// planter puts a file into the store that the backend would refuse to write, at a name
// relative to the store's root.
type planter func(t *testing.T, name string)

// One suite, every backend.
//
// The Store above these backends does not ask which one it has, so the only thing that
// makes that safe is the two of them agreeing on what the methods mean - not roughly, but
// on the cases the repository depends on: a missing object is distinguishable from an
// empty one, a range that runs past the end is a short read rather than an error, a listing
// skips names that are not ours, and a move is a rename that takes the old name away.
//
// This suite is what "is store.Backend the right shape?" is answered with (PORTING_PLAN
// §11.5): the interface needed no change for the second backend, and this suite is the
// evidence rather than the claim.
//
// open returns an opened backend and a way to plant a file in it that the backend itself
// would refuse to write - a name that is not one of borge's. One case below needs that, and
// how it is done differs: for a local store it is a file, and for a store on an SFTP server
// it goes over the same connection the backend uses.
func runBackendConformance(t *testing.T, open func(t *testing.T) (Backend, planter)) {
	t.Run("store and load", func(t *testing.T) {
		b, _ := open(t)
		if err := b.Store("config/id", []byte("0123456789abcdef")); err != nil {
			t.Fatalf("Store: %v", err)
		}
		got, err := b.Load("config/id", 0, -1)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if string(got) != "0123456789abcdef" {
			t.Errorf("Load returned %q", got)
		}
	})

	t.Run("overwriting replaces", func(t *testing.T) {
		b, _ := open(t)
		if err := b.Store("config/version", []byte("3")); err != nil {
			t.Fatal(err)
		}
		if err := b.Store("config/version", []byte("4")); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		got, err := b.Load("config/version", 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "4" {
			t.Errorf("after overwriting, Load returned %q", got)
		}
	})

	t.Run("ranges", func(t *testing.T) {
		b, _ := open(t)
		const body = "0123456789abcdefghij"
		if err := b.Store("config/ranges", []byte(body)); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			offset, size int64
			want         string
		}{
			{0, -1, body},
			{0, 4, "0123"},
			{10, 4, "abcd"},
			{10, -1, "abcdefghij"},
			{-4, -1, "ghij"},
			{-10, 4, "abcd"},
			// Past the end is a short read, not a failure: the pack reader asks for a
			// header-sized slice at the end of a pack and uses the short result as its
			// signal for a clean end of file.
			{16, 100, "ghij"},
		} {
			got, err := b.Load("config/ranges", c.offset, c.size)
			if err != nil {
				t.Errorf("Load(offset=%d size=%d): %v", c.offset, c.size, err)
				continue
			}
			if string(got) != c.want {
				t.Errorf("Load(offset=%d size=%d) = %q, want %q", c.offset, c.size, got, c.want)
			}
		}
	})

	t.Run("a missing object is not an empty one", func(t *testing.T) {
		b, _ := open(t)
		_, err := b.Load("config/absent", 0, -1)
		if !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Load of a missing object returned %v, want ErrObjectNotFound", err)
		}
		if err := b.Delete("config/absent"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Delete of a missing object returned %v, want ErrObjectNotFound", err)
		}
		info, err := b.Info("config/absent")
		if err != nil {
			t.Errorf("Info of a missing object failed: %v", err)
		} else if info.Exists {
			t.Error("Info says a missing object exists")
		}
	})

	t.Run("info reports size and kind", func(t *testing.T) {
		b, _ := open(t)
		if err := b.Store("config/sized", []byte("12345")); err != nil {
			t.Fatal(err)
		}
		info, err := b.Info("config/sized")
		if err != nil {
			t.Fatal(err)
		}
		if !info.Exists || info.Directory || info.Size != 5 {
			t.Errorf("Info = %+v, want an existing 5-byte object", info)
		}
		if info.Name != "sized" {
			t.Errorf("Info reported name %q, want the entry's own name", info.Name)
		}
		dir, err := b.Info("config")
		if err != nil {
			t.Fatal(err)
		}
		if !dir.Exists || !dir.Directory {
			t.Errorf("Info of a namespace = %+v, want an existing directory", dir)
		}
	})

	t.Run("list is sorted and skips what is not ours", func(t *testing.T) {
		b, plant := open(t)
		for _, name := range []string{"archives/c", "archives/a", "archives/b"} {
			if err := b.Store(name, []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		// Names no borge would write, put there behind the backend's back because the
		// backend itself refuses to write them. A listing has to skip them rather than
		// report them or fail: the storage may hold a leftover temp file, or something
		// that never came from borge at all, and neither is a reason to stop a backup.
		for _, foreign := range []string{"NOTES.txt", "half-written" + TmpSuffix, "a b"} {
			plant(t, "archives/"+foreign)
		}
		entries, err := b.List("archives")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name)
		}
		if strings.Join(names, ",") != "a,b,c" {
			t.Errorf("List gave %v, want a,b,c in order", names)
		}
	})

	t.Run("list of a missing directory", func(t *testing.T) {
		b, _ := open(t)
		if _, err := b.List("archives"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("List of a missing directory returned %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("move takes the old name away", func(t *testing.T) {
		b, _ := open(t)
		if err := b.Store("archives/one", []byte("body")); err != nil {
			t.Fatal(err)
		}
		if err := b.Move("archives/one", "archives/one.del"); err != nil {
			t.Fatalf("Move: %v", err)
		}
		if _, err := b.Load("archives/one", 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("after a move the old name returned %v, want ErrObjectNotFound", err)
		}
		got, err := b.Load("archives/one.del", 0, -1)
		if err != nil {
			t.Fatalf("Load of the new name: %v", err)
		}
		if string(got) != "body" {
			t.Errorf("the moved object holds %q", got)
		}
		if err := b.Move("archives/absent", "archives/whatever"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Move of a missing object returned %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("mkdir and rmdir", func(t *testing.T) {
		b, _ := open(t)
		if err := b.Mkdir("packs/ab"); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		info, err := b.Info("packs/ab")
		if err != nil {
			t.Fatal(err)
		}
		if !info.Exists || !info.Directory {
			t.Errorf("after Mkdir, Info = %+v", info)
		}
		if err := b.Rmdir("packs/ab"); err != nil {
			t.Fatalf("Rmdir: %v", err)
		}
		if info, err := b.Info("packs/ab"); err == nil && info.Exists {
			t.Error("the directory is still there after Rmdir")
		}
	})

	t.Run("names that would escape the store are refused", func(t *testing.T) {
		b, _ := open(t)
		for _, name := range []string{"../outside", "/etc/passwd", "config/../../x"} {
			if err := b.Store(name, []byte("x")); err == nil {
				t.Errorf("Store accepted %q", name)
			}
			if _, err := b.Load(name, 0, -1); err == nil {
				t.Errorf("Load accepted %q", name)
			}
		}
	})
}
