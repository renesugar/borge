// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"
)

// The spec parser, tested apart from the commands because two of its rules are invisible
// in output: an empty field between commas is skipped rather than being an error, and a
// descending pass must not reverse ties.

func TestParseSortSpec(t *testing.T) {
	cases := []struct {
		spec string
		want []sortField
	}{
		{"path", []sortField{{name: "path"}}},
		{">size", []sortField{{name: "size", descending: true}}},
		{"<size", []sortField{{name: "size"}}},
		{" path , >size ", []sortField{{name: "path"}, {name: "size", descending: true}}},
		// borg skips blank parts rather than rejecting them: "a,,b" is two fields.
		{"path,,size", []sortField{{name: "path"}, {name: "size"}}},
		{"", nil},
		{"  ", nil},
		{",", nil},
	}
	for _, c := range cases {
		if got := parseSortSpec(c.spec); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseSortSpec(%q) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestValidateSortSpec(t *testing.T) {
	allowed := []string{"path", "size"}
	canonical, err := validateSortSpec("<path,>size", allowed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The canonical form drops "<" and keeps ">", as borg's validator returns it.
	if canonical != "path,>size" {
		t.Errorf("canonical form = %q, want %q", canonical, "path,>size")
	}
	if _, err := validateSortSpec("", allowed); err == nil {
		t.Error("an empty spec was accepted")
	}
	if _, err := validateSortSpec("path,nope", allowed); err == nil {
		t.Error("an unknown field was accepted")
	}
}

// TestSortBySpecIsStable: equal elements keep their input order, in both directions.
//
// Descending is a reversed comparison, not a reversed slice. Reversing the slice would
// give the same ordering of distinct keys and the opposite ordering of ties, and a test
// that only sorted distinct values could not tell the two implementations apart - so this
// one is all ties but one.
func TestSortBySpecIsStable(t *testing.T) {
	type row struct {
		group int64
		id    string
	}
	keyFor := func(field string, r row) sortKey {
		if field == "group" {
			return numSortKey(r.group)
		}
		return textSortKey(r.id)
	}
	ids := func(rows []row) string {
		out := ""
		for _, r := range rows {
			out += r.id
		}
		return out
	}

	rows := []row{{1, "a"}, {1, "b"}, {0, "c"}, {1, "d"}, {0, "e"}}
	sortBySpec(rows, "group", keyFor)
	if got := ids(rows); got != "ceabd" {
		t.Errorf("ascending = %q, want %q", got, "ceabd")
	}

	rows = []row{{1, "a"}, {1, "b"}, {0, "c"}, {1, "d"}, {0, "e"}}
	sortBySpec(rows, ">group", keyFor)
	if got := ids(rows); got != "abdce" {
		t.Errorf("descending = %q, want %q; ties must keep their order", got, "abdce")
	}

	// Last field first: "group,id" sorts by group, breaking ties by id.
	rows = []row{{1, "d"}, {1, "a"}, {0, "e"}, {0, "c"}}
	sortBySpec(rows, "group,id", keyFor)
	if got := ids(rows); got != "cead" {
		t.Errorf("two fields = %q, want %q", got, "cead")
	}
}
