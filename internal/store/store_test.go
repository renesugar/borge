// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Unit tests that run without the borg venv: name validation, nesting, permissions, the
// pack cache, and the failure modes.

func newTestStore(t *testing.T, cache Backend) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repo")
	backend, err := NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(backend, map[string]NamespaceConfig{
		"archives/": {Levels: []int{0}},
		"config/":   {Levels: []int{0}},
		"index/":    {Levels: []int{0}},
		"packs/":    {Levels: []int{1}, Cache: cacheModeFor(cache)},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(); err != nil {
		t.Fatal(err)
	}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func cacheModeFor(cache Backend) CacheMode {
	if cache == nil {
		return CacheOff
	}
	return CacheWritethrough
}

func TestValidateName(t *testing.T) {
	valid := []string{
		"a", "config/readme", "packs/01/0123abcd", strings.Repeat("a", MaxNameLength),
		"archives/" + strings.Repeat("0", 64), "a.del", "a.hid",
	}
	for _, name := range valid {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q) rejected a valid name: %v", name, err)
		}
	}

	invalid := map[string]string{
		"too long":       strings.Repeat("a", MaxNameLength+1),
		"non-ascii":      "café",
		"absolute":       "/a",
		"trailing slash": "a/",
		"parent":         "../a",
		"parent inside":  "a/../b",
		"parent bare":    "..",
		"backslash":      `a\b`,
		"space":          "a b",
		"uppercase":      "Config/readme",
		"uppercase hex":  "packs/ABCDEF",
		"temp suffix":    "a.tmp",
	}
	for what, name := range invalid {
		if err := validateName(name); err == nil {
			t.Errorf("%s: validateName(%q) accepted an invalid name", what, name)
		}
	}
}

func TestNest(t *testing.T) {
	tests := []struct {
		name   string
		levels int
		suffix string
		want   string
	}{
		{"packs/0123456789abcdef", 0, "", "packs/0123456789abcdef"},
		{"packs/0123456789abcdef", 1, "", "packs/01/0123456789abcdef"},
		{"packs/0123456789abcdef", 2, "", "packs/01/23/0123456789abcdef"},
		{"packs/0123456789abcdef", 3, "", "packs/01/23/45/0123456789abcdef"},
		{"packs/0123456789abcdef", 1, DelSuffix, "packs/01/0123456789abcdef.del"},
		{"0123456789abcdef", 1, "", "01/0123456789abcdef"}, // no namespace
		{"", 2, "", ""},
	}
	for _, tc := range tests {
		if got := Nest(tc.name, tc.levels, tc.suffix); got != tc.want {
			t.Errorf("Nest(%q, %d, %q) = %q, want %q", tc.name, tc.levels, tc.suffix, got, tc.want)
		}
	}
}

func TestUnnest(t *testing.T) {
	tests := []struct {
		name, namespace, suffix, want string
	}{
		{"packs/01/23/0123456789abcdef", "packs", "", "packs/0123456789abcdef"},
		{"packs/01/23/0123456789abcdef", "packs/", "", "packs/0123456789abcdef"},
		{"packs/01/0123456789abcdef.del", "packs", DelSuffix, "packs/0123456789abcdef"},
		{"packs/0123456789abcdef", "packs", "", "packs/0123456789abcdef"},
	}
	for _, tc := range tests {
		got, err := Unnest(tc.name, tc.namespace, tc.suffix)
		if err != nil {
			t.Errorf("Unnest(%q, %q): %v", tc.name, tc.namespace, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Unnest(%q, %q, %q) = %q, want %q", tc.name, tc.namespace, tc.suffix, got, tc.want)
		}
	}

	// Nest and Unnest must be inverses.
	for _, levels := range []int{0, 1, 2, 3} {
		name := "packs/0123456789abcdef"
		nested := Nest(name, levels, "")
		back, err := Unnest(nested, "packs", "")
		if err != nil {
			t.Fatal(err)
		}
		if back != name {
			t.Errorf("levels=%d: %q -> %q -> %q", levels, name, nested, back)
		}
	}

	if _, err := Unnest("other/abc", "packs", ""); err == nil {
		t.Error("Unnest accepted a name outside the namespace")
	}
}

func TestStoreLoadRoundTrip(t *testing.T) {
	s, _ := newTestStore(t, nil)

	cases := map[string][]byte{
		"config/readme":                     []byte("hello"),
		"config/empty":                      nil,
		"packs/" + strings.Repeat("ab", 32): bytes.Repeat([]byte{0x7f}, 5000),
	}
	for name, value := range cases {
		if err := s.Store(name, value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := s.Load(name, 0, -1, false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, value) {
			t.Errorf("%s: round trip changed the value", name)
		}
	}

	if _, err := s.Load("config/missing", 0, -1, false); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("loading a missing object gave %v, want ErrObjectNotFound", err)
	}
}

func TestRangeReads(t *testing.T) {
	s, _ := newTestStore(t, nil)
	name := "config/data"
	value := []byte("0123456789abcdefghij")
	if err := s.Store(name, value); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		offset, size int64
		want         string
	}{
		{0, -1, "0123456789abcdefghij"},
		{0, 5, "01234"},
		{10, 5, "abcde"},
		{10, -1, "abcdefghij"},
		{19, 5, "j"},      // short read at the end, not an error
		{20, 5, ""},       // exactly at the end
		{-5, -1, "fghij"}, // negative offset counts from the end
		{-5, 2, "fg"},
	} {
		got, err := s.Load(name, tc.offset, tc.size, false)
		if err != nil {
			t.Fatalf("(%d, %d): %v", tc.offset, tc.size, err)
		}
		if string(got) != tc.want {
			t.Errorf("(%d, %d) = %q, want %q", tc.offset, tc.size, got, tc.want)
		}
	}
}

// TestAtomicStore: an object is written to a temp file and renamed, so a reader never
// sees a partial write and a leftover temp file is never mistaken for an object.
func TestAtomicStore(t *testing.T) {
	s, path := newTestStore(t, nil)
	if err := s.Store("config/a", []byte("value")); err != nil {
		t.Fatal(err)
	}

	// No temp file may survive a successful write.
	var leftovers []string
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, TmpSuffix) {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}

	// And one planted by hand must be invisible to a listing, because validateName
	// rejects the suffix.
	tmp := filepath.Join(path, "config", "leftover"+TmpSuffix)
	if err := os.WriteFile(tmp, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := s.ListNames("config", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.Contains(n, TmpSuffix) {
			t.Errorf("a temp file appeared in the listing: %s", n)
		}
	}
	if len(names) != 1 || names[0] != "a" {
		t.Errorf("listing = %v, want [a]", names)
	}
}

func TestSoftDeleteAndUndelete(t *testing.T) {
	s, path := newTestStore(t, nil)
	name := "archives/" + strings.Repeat("cd", 32)
	if err := s.Store(name, []byte("archive")); err != nil {
		t.Fatal(err)
	}

	if err := s.SoftDelete(name); err != nil {
		t.Fatal(err)
	}
	// The file is renamed, not removed: that is what makes undelete possible.
	if _, err := os.Stat(filepath.Join(path, name+DelSuffix)); err != nil {
		t.Errorf("soft delete did not leave a %s file: %v", DelSuffix, err)
	}

	live, err := s.ListNames("archives", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("a soft-deleted object is still listed as live: %v", live)
	}
	deleted, err := s.ListNames("archives", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted listing = %v, want one entry", deleted)
	}

	// Its content is still readable while soft-deleted.
	got, err := s.Load(name, 0, -1, true)
	if err != nil {
		t.Fatalf("loading a soft-deleted object: %v", err)
	}
	if string(got) != "archive" {
		t.Errorf("content changed: %q", got)
	}

	if err := s.Undelete(name); err != nil {
		t.Fatal(err)
	}
	live, _ = s.ListNames("archives", false)
	if len(live) != 1 {
		t.Errorf("undelete did not restore the object: %v", live)
	}
}

// TestPermissions checks the model borg uses to serve a repository to a client that
// should not be able to damage it.
func TestPermissions(t *testing.T) {
	// borg's "no-delete" permission set (src/borg/repository.py: borg_permissions).
	perms := Permissions{
		"":         "lr",
		"archives": "lrw",
		"cache":    "lrwWD",
		"config":   "lrW",
		"index":    "lrwWD",
		"keys":     "lr",
		"locks":    "lrwD",
		"packs":    "lrw",
	}

	tests := []struct {
		name     string
		required string
		allowed  bool
		why      string
	}{
		{"archives/abc", "lr", true, "archives grants l and r"},
		{"archives/abc", "D", false, "archives grants no delete"},
		{"packs/01/abc", "w", true, "permissions apply below the granted prefix"},
		{"packs/01/abc", "D", false, "packs grants no delete"},
		{"cache/checked-packs", "D", true, "cache grants delete"},
		{"config/manifest", "W", true, "config grants overwrite, for the manifest"},
		{"config/manifest", "D", false, "config grants no delete"},
		{"keys/abc", "r", true, "keys is readable"},
		{"keys/abc", "w", false, "keys is not writable"},
		{"unknown/abc", "r", true, "falls back to the root grant"},
		{"unknown/abc", "w", false, "the root grant has no write"},
	}
	for _, tc := range tests {
		err := perms.check(tc.name, tc.required)
		if allowed := err == nil; allowed != tc.allowed {
			t.Errorf("check(%q, %q) allowed=%v, want %v (%s)", tc.name, tc.required, allowed, tc.allowed, tc.why)
		}
	}

	// The most specific entry wins outright: a narrower grant must not be widened by a
	// broader one further up.
	specific := Permissions{"": "lrwWD", "keys": "lr"}
	if err := specific.check("keys/abc", "D"); err == nil {
		t.Error("a specific read-only grant on keys was overridden by the root grant")
	}
	if err := specific.check("other/abc", "D"); err != nil {
		t.Errorf("the root grant should apply where nothing more specific matches: %v", err)
	}

	// An empty map means unrestricted.
	if err := Permissions(nil).check("anything", "D"); err != nil {
		t.Errorf("a nil permission map should allow everything: %v", err)
	}
}

func TestPermissionsAreEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	backend, err := NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Create(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := NewPosixFS(path, Permissions{"": "lr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Open(); err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	if err := readOnly.Store("config/a", []byte("x")); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("a read-only backend allowed a store: %v", err)
	}
	if err := readOnly.Delete("config/a"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("a read-only backend allowed a delete: %v", err)
	}
	if _, err := readOnly.List(""); err != nil {
		t.Errorf("a read-only backend refused a listing: %v", err)
	}
}

// TestWritethroughCache covers the behaviour the plan calls load-bearing for restore
// performance: a range read that misses fetches the whole object once, so subsequent
// reads of other ranges are local.
func TestWritethroughCache(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cacheBackend, err := NewPosixFS(cacheDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cacheBackend.Create(); err != nil {
		t.Fatal(err)
	}

	counting := &countingBackend{Backend: cacheBackend}
	s, _ := newTestStore(t, counting)

	name := "packs/" + strings.Repeat("ef", 32)
	value := bytes.Repeat([]byte{0x11}, 10000)
	if err := s.Store(name, value); err != nil {
		t.Fatal(err)
	}
	// A writethrough namespace caches on write, so the object is already local.
	if counting.stores == 0 {
		t.Error("storing into a writethrough namespace did not populate the cache")
	}

	// Reads must be served correctly whether they come from the cache or not.
	for _, r := range []struct{ offset, size int64 }{{0, 49}, {49, 49}, {5000, 100}, {0, -1}} {
		got, err := s.Load(name, r.offset, r.size, false)
		if err != nil {
			t.Fatalf("(%d, %d): %v", r.offset, r.size, err)
		}
		if want := sliceRange(value, r.offset, r.size); !bytes.Equal(got, want) {
			t.Errorf("(%d, %d): %d bytes, want %d", r.offset, r.size, len(got), len(want))
		}
	}

	// With the cache emptied, a range read must fall back to the primary backend, fetch
	// the whole object, and repopulate - the header-walk pattern the pack reader uses.
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	counting.stores = 0
	got, err := s.Load(name, 100, 49, false)
	if err != nil {
		t.Fatalf("range read after a cache flush: %v", err)
	}
	if !bytes.Equal(got, value[100:149]) {
		t.Error("range read after a cache flush returned the wrong bytes")
	}
	if counting.stores == 0 {
		t.Error("a cache miss did not repopulate the cache")
	}
}

// TestCacheFailuresAreNotFatal: the cache is an optimisation, and a broken one must
// slow borge down rather than break it.
func TestCacheFailuresAreNotFatal(t *testing.T) {
	s, _ := newTestStore(t, brokenBackend{})
	name := "packs/" + strings.Repeat("12", 32)
	value := []byte("payload")

	if err := s.Store(name, value); err != nil {
		t.Fatalf("a failing cache broke a store: %v", err)
	}
	got, err := s.Load(name, 0, -1, false)
	if err != nil {
		t.Fatalf("a failing cache broke a load: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Error("value changed")
	}
}

func TestListMissingNamespaceIsEmpty(t *testing.T) {
	// Namespaces are created lazily (docs/FORMAT.md §1), so listing one that has never
	// been written to must give an empty result, not an error.
	s, _ := newTestStore(t, nil)
	names, err := s.ListNames("archives", false)
	if err != nil {
		t.Fatalf("listing an unwritten namespace failed: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("listing an unwritten namespace gave %v", names)
	}
}

func TestUnconfiguredNamespaceIsRejected(t *testing.T) {
	s, _ := newTestStore(t, nil)
	if err := s.Store("nosuchns/abc", []byte("x")); err == nil {
		t.Error("storing into an unconfigured namespace succeeded")
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	// The name validation is what keeps a caller inside the store; filepath.Join alone
	// would resolve "..".
	s, path := newTestStore(t, nil)
	for _, name := range []string{"config/../../escape", "../escape", "/etc/passwd", "config/../../../tmp/x"} {
		if err := s.Store(name, []byte("x")); err == nil {
			t.Errorf("storing to %q succeeded", name)
		}
		if _, err := s.Load(name, 0, -1, false); err == nil {
			t.Errorf("loading %q succeeded", name)
		}
	}
	// Nothing may have been created outside the store.
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "escape")); err == nil {
		t.Error("a file was created outside the store")
	}
}

func TestCreateRefusesNonEmptyDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "unrelated"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	backend, err := NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Create(); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("creating a store over a non-empty directory gave %v, want ErrAlreadyExists", err)
	}
}

func TestBackendLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo")
	backend, err := NewPosixFS(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Operations before Open must be refused rather than half-working.
	if _, err := backend.Load("config/a", 0, -1); !errors.Is(err, ErrMustBeOpen) {
		t.Errorf("Load before Open gave %v", err)
	}
	if err := backend.Open(); !errors.Is(err, ErrDoesNotExist) {
		t.Errorf("opening a missing store gave %v, want ErrDoesNotExist", err)
	}
	if err := backend.Create(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Open(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Open(); !errors.Is(err, ErrMustNotBeOpen) {
		t.Errorf("opening twice gave %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); !errors.Is(err, ErrMustBeOpen) {
		t.Errorf("closing twice gave %v", err)
	}
}

func TestRelativePathIsRejected(t *testing.T) {
	if _, err := NewPosixFS("relative/path", nil); err == nil {
		t.Error("a relative base path was accepted")
	}
}

func TestParseFileURL(t *testing.T) {
	for url, want := range map[string]string{
		"file:///home/renes/repo": "/home/renes/repo",
		"file:///":                "/",
	} {
		got, err := ParseFileURL(url)
		if err != nil {
			t.Errorf("ParseFileURL(%q): %v", url, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFileURL(%q) = %q, want %q", url, got, want)
		}
	}
	for _, url := range []string{"file://relative", "ssh://host/path", "/plain/path", "file://host/path"} {
		if _, err := ParseFileURL(url); err == nil {
			t.Errorf("ParseFileURL(%q) accepted a URL it should not", url)
		}
	}
}

// countingBackend records how many stores went through it.
type countingBackend struct {
	Backend
	stores int
}

func (b *countingBackend) Store(name string, value []byte) error {
	b.stores++
	return b.Backend.Store(name, value)
}

// brokenBackend fails every operation, standing in for an unusable cache.
type brokenBackend struct{}

func (brokenBackend) Create() error  { return errors.New("broken") }
func (brokenBackend) Destroy() error { return errors.New("broken") }
func (brokenBackend) Open() error    { return errors.New("broken") }
func (brokenBackend) Close() error   { return errors.New("broken") }
func (brokenBackend) Load(string, int64, int64) ([]byte, error) {
	return nil, errors.New("broken")
}
func (brokenBackend) Store(string, []byte) error      { return errors.New("broken") }
func (brokenBackend) Delete(string) error             { return errors.New("broken") }
func (brokenBackend) Move(string, string) error       { return errors.New("broken") }
func (brokenBackend) Info(string) (ItemInfo, error)   { return ItemInfo{}, errors.New("broken") }
func (brokenBackend) List(string) ([]ItemInfo, error) { return nil, errors.New("broken") }
func (brokenBackend) Mkdir(string) error              { return errors.New("broken") }
func (brokenBackend) Rmdir(string) error              { return errors.New("broken") }
