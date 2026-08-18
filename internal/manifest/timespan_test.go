// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTimespan(t *testing.T) {
	good := []struct {
		in   string
		want string
	}{
		{"7d", "7d"}, {"1y", "1y"}, {"12m", "12m"}, {"2w", "2w"},
		{"36H", "36H"}, {"90M", "90M"}, {"5S", "5S"}, {"0d", "0d"},
		{"365d", "365d"},
	}
	for _, c := range good {
		got, err := ParseTimespan(c.in)
		if err != nil {
			t.Errorf("ParseTimespan(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("ParseTimespan(%q).String() = %q, want %q", c.in, got.String(), c.want)
		}
		if got.IsZero() {
			t.Errorf("ParseTimespan(%q) reports itself unset", c.in)
		}
	}

	// borg's validator is anchored, so none of these are spans. A parser that accepted
	// them would turn a typo into a filter that silently selects the wrong archives.
	for _, bad := range []string{"", "d", "7", "7x", "-7d", "+7d", "7 d", "7dd", "1d12H", "7D", "1.5d", "d7"} {
		if _, err := ParseTimespan(bad); err == nil {
			t.Errorf("ParseTimespan(%q) was accepted", bad)
		}
	}
}

func TestTimespanOffset(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	cases := []struct {
		span    string
		from    string
		earlier bool
		want    string
	}{
		// Exact durations.
		{"7d", "2026-03-15T12:30:45Z", false, "2026-03-22T12:30:45Z"},
		{"7d", "2026-03-15T12:30:45Z", true, "2026-03-08T12:30:45Z"},
		{"2w", "2026-03-15T12:30:45Z", true, "2026-03-01T12:30:45Z"},
		{"36H", "2026-03-15T12:30:45Z", true, "2026-03-14T00:30:45Z"},
		{"90M", "2026-03-15T12:30:45Z", true, "2026-03-15T11:00:45Z"},
		{"45S", "2026-03-15T12:30:45Z", true, "2026-03-15T12:30:00Z"},

		// Calendar months, with the day clamped rather than spilling over. This is the
		// row that a naive AddDate gets wrong: it would give 3 March.
		{"1m", "2026-01-31T08:00:00Z", false, "2026-02-28T08:00:00Z"},
		{"1m", "2028-01-31T08:00:00Z", false, "2028-02-29T08:00:00Z"}, // leap year
		{"1m", "2026-03-31T08:00:00Z", true, "2026-02-28T08:00:00Z"},
		{"1m", "2026-01-15T08:00:00Z", false, "2026-02-15T08:00:00Z"},
		{"12m", "2026-03-15T12:30:45Z", false, "2027-03-15T12:30:45Z"},

		// Year boundaries in both directions.
		{"1m", "2026-12-15T00:00:00Z", false, "2027-01-15T00:00:00Z"},
		{"1m", "2026-01-15T00:00:00Z", true, "2025-12-15T00:00:00Z"},
		{"1y", "2028-02-29T00:00:00Z", false, "2029-02-28T00:00:00Z"}, // leap day clamped
		{"2y", "2026-06-01T00:00:00Z", true, "2024-06-01T00:00:00Z"},

		// Zero moves nothing, in either direction.
		{"0d", "2026-03-15T12:30:45Z", false, "2026-03-15T12:30:45Z"},
		{"0m", "2026-01-31T12:30:45Z", true, "2026-01-31T12:30:45Z"},
	}

	for _, c := range cases {
		span, err := ParseTimespan(c.span)
		if err != nil {
			t.Fatalf("ParseTimespan(%q): %v", c.span, err)
		}
		got := span.Offset(at(c.from), c.earlier)
		if !got.Equal(at(c.want)) {
			dir := "later"
			if c.earlier {
				dir = "earlier"
			}
			t.Errorf("%s %s from %s = %s, want %s",
				c.span, dir, c.from, got.Format(time.RFC3339), c.want)
		}
	}
}

// TestTimespanOffsetMatchesBorg checks the arithmetic against borg's own
// calculate_relative_offset rather than against this port's reading of it.
//
// The month cases are the reason: "one month before 31 March" is a decision, not a
// derivation, and borg's answer is the one that has to be reproduced.
func TestTimespanOffsetMatchesBorg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the borg cross-check in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv-borg2", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		t.Skip("borg 2 venv not built; run 'make borg2' to enable the cross-check")
	}

	instants := []string{
		"2026-01-31T08:00:00+00:00",
		"2028-01-31T08:00:00+00:00",
		"2026-03-31T23:59:59+00:00",
		"2026-12-15T00:00:00+00:00",
		"2026-01-01T00:00:00+00:00",
		"2028-02-29T12:00:00+00:00",
		"2026-06-15T12:30:45+00:00",
	}
	spans := []string{"1d", "7d", "2w", "36H", "90M", "45S", "1m", "2m", "12m", "1y", "3y"}

	script := `
import sys
from datetime import datetime
from borg.helpers.time import calculate_relative_offset
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    span, ts, earlier = line.split(" ")
    out = calculate_relative_offset(span, datetime.fromisoformat(ts), earlier=(earlier == "1"))
    print(out.isoformat())
`
	var input strings.Builder
	type probe struct {
		span    string
		from    string
		earlier bool
	}
	var probes []probe
	for _, ts := range instants {
		for _, span := range spans {
			for _, earlier := range []bool{false, true} {
				e := "0"
				if earlier {
					e = "1"
				}
				input.WriteString(span + " " + ts + " " + e + "\n")
				probes = append(probes, probe{span, ts, earlier})
			}
		}
	}

	cmd := exec.Command(python, "-c", script)
	cmd.Stdin = strings.NewReader(input.String())
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running borg's calculate_relative_offset: %v\n%s", err, out)
	}
	want := strings.Fields(strings.TrimSpace(string(out)))
	if len(want) != len(probes) {
		t.Fatalf("borg returned %d answers for %d probes; the cross-check is not comparing "+
			"what it thinks:\n%s", len(want), len(probes), out)
	}

	for i, p := range probes {
		span, err := ParseTimespan(p.span)
		if err != nil {
			t.Fatalf("ParseTimespan(%q): %v", p.span, err)
		}
		from, err := time.Parse(time.RFC3339Nano, p.from)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := time.Parse(time.RFC3339Nano, want[i])
		if err != nil {
			t.Fatalf("parsing borg's answer %q: %v", want[i], err)
		}
		if got := span.Offset(from, p.earlier); !got.Equal(expected) {
			dir := "later"
			if p.earlier {
				dir = "earlier"
			}
			t.Errorf("%s %s from %s: borge %s, borg %s",
				p.span, dir, p.from, got.Format(time.RFC3339), expected.Format(time.RFC3339))
		}
	}
	if len(probes) < 100 {
		t.Errorf("only %d probes; the cross-check is too small to mean much", len(probes))
	}
}

// TestFilterByRelativeExtremes covers --oldest and --newest at the level where the
// reference point is visible: they measure from the extremes of the list they are given,
// not from now.
func TestFilterByRelativeExtremes(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mk := func(name string, daysAgo int) Info {
		return Info{Name: name, Time: base.AddDate(0, 0, -daysAgo)}
	}
	infos := []Info{mk("d100", 100), mk("d040", 40), mk("d010", 10), mk("d001", 1)}

	names := func(list []Info) string {
		var out []string
		for _, i := range list {
			out = append(out, i.Name)
		}
		return strings.Join(out, ",")
	}

	oldest, err := ParseTimespan("70d")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(filterByRelativeExtremes(infos, oldest, Timespan{})); got != "d100,d040" {
		t.Errorf("--oldest 70d selected %q, want %q", got, "d100,d040")
	}

	newest, err := ParseTimespan("30d")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(filterByRelativeExtremes(infos, Timespan{}, newest)); got != "d010,d001" {
		t.Errorf("--newest 30d selected %q, want %q", got, "d010,d001")
	}

	// Neither given leaves the list alone, which is what every other caller depends on.
	if got := names(filterByRelativeExtremes(infos, Timespan{}, Timespan{})); got != "d100,d040,d010,d001" {
		t.Errorf("no filter changed the list: %q", got)
	}
	// An empty list stays empty rather than dividing by an extreme that does not exist.
	if got := filterByRelativeExtremes(nil, oldest, newest); len(got) != 0 {
		t.Errorf("filtering an empty list produced %v", got)
	}
}
