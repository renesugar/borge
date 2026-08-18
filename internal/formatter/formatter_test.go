// SPDX-License-Identifier: Apache-2.0

package formatter

import (
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	values := map[string]any{
		"archive": "daily",
		"id":      "0123456789abcdef",
		"comment": "a comment long enough to truncate",
		"size":    int64(1234),
		"nfiles":  7,
		"unicode": "héllo",
	}

	cases := []struct{ template, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{"{archive}", "daily"},
		{"{archive}{NL}", "daily\n"},
		{"[{archive}]", "[daily]"},
		{"{archive}{SPACE}{id}", "daily 0123456789abcdef"},

		// Width, and the default alignment that goes with the type: strings left,
		// numbers right. borg's own "list" default leans on exactly this.
		{"{archive:<10}|", "daily     |"},
		{"{archive:10}|", "daily     |"},
		{"{size:10}|", "      1234|"},
		{"{archive:>10}|", "     daily|"},
		{"{archive:^11}|", "   daily   |"},
		{"{archive:.>10}|", ".....daily|"},
		{"{nfiles:<5}|", "7    |"},

		// Precision truncates.
		{"{id:.8}", "01234567"},
		{"{comment:.10}|", "a comment |"},
		{"{archive:<10.3}|", "dai       |"},
		// A width shorter than the value does not truncate; only precision does.
		{"{archive:<2}|", "daily|"},

		// Doubled braces are literal.
		{"{{}}", "{}"},
		{"{{{archive}}}", "{daily}"},

		// Padding counts characters, not bytes: "héllo" is five characters in six bytes,
		// and padding by bytes would misalign every non-ASCII name in a listing.
		{"{unicode:<7}|", "héllo  |"},
	}

	for _, c := range cases {
		got, err := Format(c.template, values)
		if err != nil {
			t.Errorf("Format(%q): %v", c.template, err)
			continue
		}
		if got != c.want {
			t.Errorf("Format(%q) = %q, want %q", c.template, got, c.want)
		}
	}
}

func TestFormatRefusesWhatItCannotDo(t *testing.T) {
	values := map[string]any{"archive": "daily", "size": int64(7)}

	// Each of these would otherwise produce a listing that looks right and is not: a
	// missing column, an unpadded field, a key silently empty.
	bad := []string{
		"{nosuchkey}",
		"{archive",
		"archive}",
		"{}",
		"{archive!r}",
		"{archive:qq}",
		"{size:.2}", // precision on a number truncates nothing
		"{size:08}", // zero padding is Python's numeric form, not implemented
		"{archive:-5}",
	}
	for _, template := range bad {
		if got, err := Format(template, values); err == nil {
			t.Errorf("Format(%q) was accepted, giving %q", template, got)
		}
	}

	// And the message names the key, so a typo in a long format string is findable.
	_, err := Format("{archive} {nosuchkey} {size}", values)
	if err == nil || !strings.Contains(err.Error(), "nosuchkey") {
		t.Errorf("the error does not name the unknown key: %v", err)
	}
}

func TestKeys(t *testing.T) {
	got, err := Keys("{id:.8}  {time}  {archive:<15}  {archive}{NL}")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "time", "archive", "NL"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
	// A malformed template is rejected here too, so a caller can validate before doing
	// any work rather than failing part-way through a listing.
	if _, err := Keys("{unclosed"); err == nil {
		t.Error("Keys accepted an unclosed field")
	}
}

// TestStaticKeysNeedNoValue: the formatting aids work with no values at all, which is what
// lets "{NL}" end a template whatever the command is listing.
func TestStaticKeysNeedNoValue(t *testing.T) {
	got, err := Format("a{TAB}b{NL}", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\tb\n" {
		t.Errorf("got %q", got)
	}
}
