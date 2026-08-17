// SPDX-License-Identifier: Apache-2.0

package placeholders

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// strftime is compared against CPython's, because CPython's is what borg uses and the
// question this package has to answer is "would borg have spelled this archive name the
// same way".
//
// A table of expected strings written by hand would encode my belief about what %U does,
// which is exactly the belief worth checking - week numbering and the ISO week rules are
// where a hand-rolled implementation goes wrong.

// pythonBin is the interpreter of the pinned borg 2 virtualenv, which is the same CPython
// borg itself runs under.
func pythonBin(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"python3", "python"} {
		p := filepath.Join(root, ".venv-borg2", "bin", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("borg 2 venv not built; run 'make borg2' to enable the CPython comparison")
	return ""
}

// strftimeDirectives are the ones borge implements and CPython agrees on.
//
// %Z is excluded: it prints a zone abbreviation that depends on the tz database the
// interpreter loaded, and Go and CPython can legitimately disagree for a zone with
// several names. %s is excluded because it is a glibc extension CPython passes through
// rather than implements, so comparing it tests the C library.
const strftimeDirectives = "aAbBhdemyYCHIMSfpjwuUWVGzDFTRnt%"

// TestStrftimeMatchesCPython over a spread of instants chosen for the edges.
func TestStrftimeMatchesCPython(t *testing.T) {
	python := pythonBin(t)

	// Each of these exists to break a plausible implementation.
	times := []time.Time{
		// A Sunday that is 1 January: %U is 01 and %W is 00, and they differ.
		time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
		// A Monday that is 1 January: the reverse.
		time.Date(2024, 1, 1, 12, 30, 45, 123456000, time.UTC),
		// 31 December in a year whose ISO week belongs to the next year.
		time.Date(2019, 12, 31, 23, 59, 59, 999999000, time.UTC),
		// 1 January in a year whose ISO week belongs to the previous one.
		time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
		// A leap day, and midday exactly, for %I and %p either side of noon.
		time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 23, 0, 0, 0, time.UTC),
		// A century boundary for %C and %y.
		time.Date(2000, 12, 31, 6, 7, 8, 90000, time.UTC),
		// An offset zone, for %z.
		time.Date(2026, 8, 17, 14, 15, 16, 0, time.FixedZone("", 5*3600+30*60)),
		time.Date(2026, 8, 17, 14, 15, 16, 0, time.FixedZone("", -8*3600)),
		// The last day of a year, for %j.
		time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
	}

	for _, when := range times {
		for _, d := range strftimeDirectives {
			format := "%" + string(d)
			got, err := strftime(when, format)
			if err != nil {
				t.Errorf("%s on %s: %v", format, when, err)
				continue
			}
			want := cpythonStrftime(t, python, when, format)
			if got != want {
				t.Errorf("%s on %s: borge %q, CPython %q", format, when.Format(time.RFC3339Nano), got, want)
			}
		}
	}
}

// TestStrftimeMatchesCPythonOnRealFormats: the combinations people actually write.
func TestStrftimeMatchesCPythonOnRealFormats(t *testing.T) {
	python := pythonBin(t)
	when := time.Date(2026, 8, 17, 14, 5, 6, 7000, time.FixedZone("", 2*3600))

	for _, format := range []string{
		"%Y-%m-%d",
		"%Y-%m-%dT%H:%M:%S",
		"%Y%m%dT%H%M%S",
		"%Y-%m-%d %H:%M:%S%z",
		"backup-%Y-%W",
		"%Y/%j",
		"%A %d %B %Y",
		"100%% done at %H:%M",
		"%F %T",
	} {
		got, err := strftime(when, format)
		if err != nil {
			t.Errorf("%q: %v", format, err)
			continue
		}
		if want := cpythonStrftime(t, python, when, format); got != want {
			t.Errorf("%q: borge %q, CPython %q", format, got, want)
		}
	}
}

// cpythonStrftime asks CPython to format the same instant.
func cpythonStrftime(t *testing.T, python string, when time.Time, format string) string {
	t.Helper()
	// The time is passed as its components plus a fixed offset, so nothing depends on the
	// interpreter's local zone.
	_, offset := when.Zone()
	script := `
import sys, datetime
y, mo, d, h, mi, s, us, off = (int(a) for a in sys.argv[1:9])
tz = datetime.timezone(datetime.timedelta(seconds=off))
dt = datetime.datetime(y, mo, d, h, mi, s, us, tzinfo=tz)
sys.stdout.write(dt.strftime(sys.argv[9]))
`
	args := []string{"-c", script,
		strconv.Itoa(when.Year()), strconv.Itoa(int(when.Month())), strconv.Itoa(when.Day()),
		strconv.Itoa(when.Hour()), strconv.Itoa(when.Minute()), strconv.Itoa(when.Second()),
		strconv.Itoa(when.Nanosecond() / 1000), strconv.Itoa(offset), format,
	}
	cmd := exec.Command(python, args...)
	// LC_ALL/LANG so CPython's %a and %B are the English names borge produces; borge does
	// not follow the locale at all, see the note in strftime.go. PYTHONDONTWRITEBYTECODE
	// and PYTHONUNBUFFERED per AGENTS.md.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C",
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("CPython failed on %q: %v", format, err)
	}
	return string(out)
}

// TestStrftimeRefusesWhatItCannotDoRight.
func TestStrftimeRefusesWhatItCannotDoRight(t *testing.T) {
	when := time.Now()

	// The locale composites: borge refuses rather than approximating, because an archive
	// name that changes with LC_TIME is not an identifier.
	for _, format := range []string{"%c", "%x", "%X"} {
		if _, err := strftime(when, format); err == nil {
			t.Errorf("%q was accepted; it is locale-dependent and should be refused", format)
		} else if !strings.Contains(err.Error(), "locale") {
			t.Errorf("%q: the error does not explain why: %v", format, err)
		}
	}

	// An unknown directive is an error rather than being passed through, which is what
	// glibc does and what would bake a typo into every archive name.
	if _, err := strftime(when, "%Q"); err == nil {
		t.Error("%Q was accepted")
	}
	if _, err := strftime(when, "ends with %"); err == nil {
		t.Error("a trailing %% was accepted")
	}
}
