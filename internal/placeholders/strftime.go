// SPDX-License-Identifier: Apache-2.0

package placeholders

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// {now} and {utcnow} take strftime directives: %Y year, %m month, %d day, %H hour,
// %M minute, %S second, %f microseconds, %j day of year, %U and %W week number, %V and %G
// the ISO week and its year, %a %A %b %B the day and month names, %p AM/PM, %z and %Z the
// zone, %s the epoch seconds. %F, %T, %D and %R are the usual composites. A literal percent
// is %%.
//
// %c, %x and %X are refused. They format a date the way the machine's locale prefers, and an
// archive name is an identifier: it is matched by scripts and sorted by retention rules, so
// it must not change with LC_TIME. For the same reason the day and month names are always
// English here, where borg follows the locale.
//
// An unknown directive is an error rather than being copied through, so a typo in a crontab
// is caught the first time rather than baked into a year of archive names.
//
//borge:doc user
//borge:help placeholders/formats
//borge:claim placeholders/locale-independent-formats
var _ = helpText

// strftime formats a time with C/Python strftime directives.
//
// # Why not time.Format
//
// Go formats times with a reference layout ("2006-01-02"), which is a different notation
// entirely. borg's placeholders take strftime directives because Python's datetime does,
// and a user's "{now:%Y-%m-%d}" comes from a borg crontab. Translating it to a Go layout
// and calling time.Format would work for the handful of directives that map cleanly and
// silently mangle the rest, so the directives are implemented directly.
//
// # Locale
//
// Everything here is the C locale: English month and day names, and a 24-hour clock
// unless %I/%p is asked for. Python's strftime follows the process locale, so a machine
// with LC_TIME=de_DE would get "Mo" from borg's %a and "Mon" from borge's.
//
// That is deliberate. An archive name is an identifier: it is matched by scripts, sorted
// by retention rules and typed back in at a shell. A name that changes with the ambient
// locale is a name that is not stable across the machines a repository is shared between,
// which is the opposite of what it is for. The locale-dependent directives Python offers
// for whole dates and times - %c, %x, %X - are refused outright rather than approximated.
func strftime(t time.Time, format string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) {
			return "", fmt.Errorf("placeholders: %q ends with a lone %%", format)
		}
		out, err := directive(t, format[i])
		if err != nil {
			return "", err
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

var (
	shortDays   = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	longDays    = [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	shortMonths = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	longMonths  = [...]string{"January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December"}
)

func directive(t time.Time, c byte) (string, error) {
	switch c {
	case '%':
		return "%", nil

	case 'a':
		return shortDays[int(t.Weekday())], nil
	case 'A':
		return longDays[int(t.Weekday())], nil
	case 'b', 'h':
		return shortMonths[int(t.Month())-1], nil
	case 'B':
		return longMonths[int(t.Month())-1], nil

	case 'd':
		return pad2(t.Day()), nil
	case 'e':
		return fmt.Sprintf("%2d", t.Day()), nil
	case 'm':
		return pad2(int(t.Month())), nil
	case 'y':
		return pad2(t.Year() % 100), nil
	case 'Y':
		return strconv.Itoa(t.Year()), nil
	case 'C':
		return pad2(t.Year() / 100), nil

	case 'H':
		return pad2(t.Hour()), nil
	case 'I':
		h := t.Hour() % 12
		if h == 0 {
			h = 12
		}
		return pad2(h), nil
	case 'M':
		return pad2(t.Minute()), nil
	case 'S':
		return pad2(t.Second()), nil
	case 'f':
		// Python's %f is microseconds, always six digits. C's strftime has no %f at all.
		return fmt.Sprintf("%06d", t.Nanosecond()/1000), nil
	case 'p':
		if t.Hour() < 12 {
			return "AM", nil
		}
		return "PM", nil

	case 'j':
		return fmt.Sprintf("%03d", t.YearDay()), nil
	case 'w':
		// 0 is Sunday, which is what C and Python both say.
		return strconv.Itoa(int(t.Weekday())), nil
	case 'u':
		// ISO weekday: 1 is Monday, 7 is Sunday.
		if wd := int(t.Weekday()); wd == 0 {
			return "7", nil
		} else {
			return strconv.Itoa(wd), nil
		}
	case 'U':
		// Week of the year, Sunday as the first day. Days before the first Sunday are
		// week 00.
		return pad2((t.YearDay() + 6 - int(t.Weekday())) / 7), nil
	case 'W':
		// The same with Monday as the first day.
		monday := (int(t.Weekday()) + 6) % 7
		return pad2((t.YearDay() + 6 - monday) / 7), nil
	case 'V':
		_, week := t.ISOWeek()
		return pad2(week), nil
	case 'G':
		year, _ := t.ISOWeek()
		return strconv.Itoa(year), nil

	case 'z':
		return t.Format("-0700"), nil
	case 'Z':
		return t.Format("MST"), nil
	case 's':
		// A glibc extension Python passes through on Linux. Useful enough in a name to be
		// worth having, and unambiguous.
		return strconv.FormatInt(t.Unix(), 10), nil

	case 'n':
		return "\n", nil
	case 't':
		return "\t", nil

	// Composites, spelled out rather than delegated, so they cannot drift from the parts.
	case 'D':
		return pad2(int(t.Month())) + "/" + pad2(t.Day()) + "/" + pad2(t.Year()%100), nil
	case 'F':
		return strconv.Itoa(t.Year()) + "-" + pad2(int(t.Month())) + "-" + pad2(t.Day()), nil
	case 'T':
		return pad2(t.Hour()) + ":" + pad2(t.Minute()) + ":" + pad2(t.Second()), nil
	case 'R':
		return pad2(t.Hour()) + ":" + pad2(t.Minute()), nil

	case 'c', 'x', 'X':
		return "", fmt.Errorf("placeholders: %%%c formats a date the way the machine's "+
			"locale prefers, so the same command would produce different archive names on "+
			"different machines; spell the format out instead", c)

	default:
		// Python hands an unknown directive to the C library, which on glibc copies it
		// through. Refusing is more useful: a typo in a crontab that silently produces
		// "%Q" inside every archive name is not something anybody wants discovered later.
		return "", fmt.Errorf("placeholders: %%%c is not a format directive borge knows", c)
	}
}

func pad2(n int) string {
	if n < 0 {
		return strconv.Itoa(n)
	}
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
