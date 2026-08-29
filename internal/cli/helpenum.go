// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/renesugar/borge/internal/compress"
	"github.com/renesugar/borge/internal/manifest"
	"github.com/renesugar/borge/internal/patterns"
	"github.com/renesugar/borge/internal/placeholders"
)

// Generated lists in the help topics.
//
// # Why these are not written out
//
// A list in a help topic is the part that goes stale first: a codec is added, a variable
// is read, a placeholder is implemented, and the topic still describes the old set. Both
// directions were checked by hand-written tests until 2026-08-27, one test per list, each
// knowing something about the topic's text.
//
// So the lists are rendered from the code that defines them. A topic marks where a list
// belongs with {{enum:name}} - or {{enum:name:argument}} where one table serves several
// places - and renderEnumerations fills it in. There is nothing to keep in step, because
// there is only one list. What still needs checking is the *table* against the behaviour,
// and that check lives beside the behaviour: patterns.Styles against the pattern parser,
// compress.SpecDocs against parseSpec, placeholders.All against the expander, and
// envVars below against every BORGE_ name the source reads.

// envVar is one documented environment variable, without its BORGE_ prefix.
type envVar struct {
	Name        string
	Section     string
	Description string
}

// envVars is the source for "borge help environment".
//
// The sections are presentation, and they are here rather than in the topic because the
// topic is generated from this: a variable added without a section would have nowhere to
// appear, which is a mistake worth making impossible rather than catching later.
//
//borge:enumerates environment-variables
//borge:claim environment/other-passphrase-no-fallback
func envVars() []envVar {
	return []envVar{
		// REPOSITORY AND KEYS
		{"REPO", "REPOSITORY AND KEYS", "the repository to act on, when -r is not given"},
		{"PASSPHRASE", "REPOSITORY AND KEYS", "the passphrase that unlocks the key"},
		{"NEW_PASSPHRASE", "REPOSITORY AND KEYS", "the passphrase to set, for \"key change-passphrase\" and \"key add\""},
		{"KEYS_DIR", "REPOSITORY AND KEYS", "where keyfiles are kept. Set, it pins the search to that one directory: a user who says where the keys are is not asking for a search."},
		{"KEY_FILE", "REPOSITORY AND KEYS", "one specific keyfile, overriding the search entirely"},
		{"CONFIG_DIR", "REPOSITORY AND KEYS", "a configuration directory; its keys/ subdirectory joins the keyfile search path"},
		{"BASE_DIR", "REPOSITORY AND KEYS", "a home-like directory; its .config/borge/keys joins the search path"},
		// CACHE
		{"CACHE_DIR", "CACHE", "where the files cache lives"},
		{"FILES_CACHE_TTL", "CACHE", "how many backups an unused cache entry survives"},
		// BEHAVIOUR
		{"FIND_FORMAT", "BEHAVIOUR", "the default --format for \"find\", when it is not given"},
		{"LIST_FORMAT", "BEHAVIOUR", "the default --format for \"list\", when it is not given"},
		{"PRUNE_FORMAT", "BEHAVIOUR", "the default --format for \"prune\", when it is not given"},
		{"REPO_LIST_FORMAT", "BEHAVIOUR", "the default --format for \"repo-list\", when it is not given"},
		{"UNITS", "BEHAVIOUR", "si (default), iec or raw, for how sizes are printed"},
		{"OTHER_REPO", "BEHAVIOUR", "the source repository for \"transfer\" and \"repo-create --other-repo\", when --other-repo is not given"},
		{"OTHER_PASSPHRASE", "BEHAVIOUR", "the passphrase for that source repository. It does NOT fall back to BORGE_PASSPHRASE, because borg's does not: two repositories are open at once and need not share one"},
		{"ZSTD_MT_WORKERS", "BEHAVIOUR", "how many threads compress a .tar.zst tarball. Unset means one per CPU, where borg's default is one thread; the bytes a decompressor sees are the same either way"},
		{"HOST_ID", "BEHAVIOUR", "this machine's lock identity, when the derived one is wrong"},
		{"DELETE_I_KNOW_WHAT_I_AM_DOING", "BEHAVIOUR", "set to YES to answer repo-delete's confirmation"},
		{"ASSERT_ID", "BEHAVIOUR", "where chunk ids are verified: a comma-separated subset of read, repair, transfer, rechunk. The default is repair,transfer,rechunk - verifying on the read path costs a full extra hash pass over everything restored."},
		// REMOTE REPOSITORIES
		{"RSH", "REMOTE REPOSITORIES", "the remote shell for a rest:// repository with a host, replacing \"ssh\" and its options entirely"},
		{"REMOTE_PATH", "REMOTE REPOSITORIES", "the borge to run on the far end, when a rest:// URL names a host. Placeholders are expanded, so one setting can serve a fleet of differently-laid-out machines"},
		{"REPO_PERMISSIONS", "REMOTE REPOSITORIES", "what a serve --rest process allows its client to do: all (default), no-delete, write-only or read-only. The option --permissions overrides it"},
		{"KNOWN_HOSTS", "REMOTE REPOSITORIES", "the known_hosts file an sftp: repository's host key must be in, instead of ~/.ssh/known_hosts. There is no option to accept an unknown key: borge refuses a host it has not seen, so first contact is made with ssh or sftp and verified there"},
		// TUNING
		{"PACK_MAX_COUNT", "TUNING", "how many objects one pack file holds"},
		{"PACK_MAX_SIZE", "TUNING", "how large one pack file may become"},
		{"PACK_ASYNC", "TUNING", "set to \"no\" to write packs on the calling goroutine"},
		// TESTING
		{"TESTONLY_WEAKEN_KDF", "TESTING", "weakens the passphrase KDF so tests are fast. Never set this for a real repository: it makes the passphrase cheap to attack."},
		{"TESTONLY_CPUPROFILE", "TESTING", "writes a Go CPU profile to this path. For the stage 9 performance work; profiling costs time, so a measured run is not a normal one."},
		{"TESTONLY_MEMPROFILE", "TESTING", "writes a Go heap profile to this path, after the command finishes and a collection."},
	}
}

// envSections lists the sections in the order the topic presents them, lowercased: a
// marker is written {{enum:environment-variables:cache}}, and the heading above it in the
// topic is the prose's business rather than this table's.
func envSections() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range envVars() {
		key := strings.ToLower(v.Section)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// enumEntry is one line of a generated list: a term and what it means.
type enumEntry struct {
	Term        string
	Description string
}

// enumeration is a list the code defines and the documentation renders.
type enumeration struct {
	// name is what a topic writes in {{enum:name}} and what //borge:enumerates names.
	name string
	// source names the code this list comes from, for the audit's report.
	source string
	// args are the accepted {{enum:name:argument}} values. Empty means the list takes no
	// argument.
	args []string
	// sharedWidth lays every part of a multi-part list out in one column. Without it each
	// part is measured on its own and the environment topic's six sections indent their
	// descriptions differently, which reads as six unrelated tables rather than one.
	sharedWidth bool
	// entries renders the list, given one of args (or "" when there are none).
	entries func(arg string) []enumEntry
}

// enumerations is the registry. docaudit is given these names, so //borge:enumerates
// naming a list that does not exist is a finding rather than a comment nobody reads.
func enumerations() []enumeration {
	return []enumeration{
		{
			name:   "pattern-styles",
			source: "patterns.Styles",
			entries: func(string) []enumEntry {
				var out []enumEntry
				for _, s := range patterns.Styles() {
					// The term is written as a user writes it, with the argument the
					// style takes: "sh:PATTERN", "pp:PATH".
					argument := "PATTERN"
					if s.Prefix == patterns.StylePathPrefix || s.Prefix == patterns.StylePathFull {
						argument = "PATH"
					}
					out = append(out, enumEntry{s.Prefix + ":" + argument, s.Description})
				}
				return out
			},
		},
		{
			name:   "archive-selectors",
			source: "manifest.Selectors",
			entries: func(string) []enumEntry {
				var out []enumEntry
				for _, sel := range manifest.Selectors() {
					out = append(out, enumEntry{sel.Syntax, sel.Description})
				}
				return out
			},
		},
		{
			name:   "compression-specs",
			source: "compress.SpecDocs",
			entries: func(string) []enumEntry {
				var out []enumEntry
				for _, s := range compress.SpecDocs() {
					out = append(out, enumEntry{s.Syntax, s.Description})
				}
				return out
			},
		},
		{
			name:   "placeholders",
			source: "placeholders.All",
			entries: func(string) []enumEntry {
				var out []enumEntry
				for _, p := range placeholders.All() {
					out = append(out, enumEntry{p.Syntax, p.Description})
				}
				return out
			},
		},
		{
			name:        "environment-variables",
			source:      "cli.envVars",
			args:        envSections(),
			sharedWidth: true,
			entries: func(section string) []enumEntry {
				var out []enumEntry
				for _, v := range envVars() {
					if !strings.EqualFold(v.Section, section) {
						continue
					}
					out = append(out, enumEntry{"BORGE_" + v.Name, v.Description})
				}
				return out
			},
		},
	}
}

// EnumerationNames is the registry's keys.
//
// Exported for the documentation tooling, for the same reason as HelpTopicNames: the
// audit must ask the code what exists rather than keep a list of its own.
func EnumerationNames() []string { return enumerationNames() }

// enumerationNames is the registry's keys, for the documentation audit.
func enumerationNames() []string {
	var out []string
	for _, e := range enumerations() {
		out = append(out, e.name)
	}
	sort.Strings(out)
	return out
}

// enumMarker matches {{enum:name}} and {{enum:name:argument}}.
//
// Doubled braces are deliberate: the placeholders topic is full of single-braced {now}
// and even writes "{{" for a literal brace, and neither is a marker.
var enumMarker = regexp.MustCompile(`\{\{enum:([a-z-]+)(?::([a-z0-9- ]+))?\}\}`)

// renderEnumerations replaces every marker in a topic with its generated list.
//
// It returns an error rather than leaving the marker in place: a topic printed with
// "{{enum:placeholders}}" in it would be a visible defect, and one silently printed
// without the list would be an invisible one. The caller turns the error into a panic at
// startup, because a topic that cannot be rendered is a programming mistake and there is
// no useful way for the command to continue.
func renderEnumerations(body string) (string, error) {
	byName := map[string]enumeration{}
	for _, e := range enumerations() {
		byName[e.name] = e
	}
	var failure error
	out := enumMarker.ReplaceAllStringFunc(body, func(marker string) string {
		m := enumMarker.FindStringSubmatch(marker)
		name, arg := m[1], m[2]
		e, ok := byName[name]
		if !ok {
			failure = fmt.Errorf("help: %s names no generated list", marker)
			return marker
		}
		if len(e.args) == 0 && arg != "" {
			failure = fmt.Errorf("help: the %s list takes no argument, but %s gives %q", name, marker, arg)
			return marker
		}
		if len(e.args) > 0 {
			found := false
			for _, allowed := range e.args {
				if allowed == arg {
					found = true
					break
				}
			}
			if !found {
				failure = fmt.Errorf("help: %s is not one of the %s list's parts (%s)",
					marker, name, strings.Join(e.args, ", "))
				return marker
			}
		}
		entries := e.entries(arg)
		if len(entries) == 0 {
			failure = fmt.Errorf("help: %s rendered nothing", marker)
			return marker
		}
		measured := entries
		if e.sharedWidth {
			measured = nil
			for _, other := range e.args {
				measured = append(measured, e.entries(other)...)
			}
		}
		return renderEnumEntries(entries, enumColumn(measured))
	})
	if failure != nil {
		return "", failure
	}
	return out, nil
}

// The shape of a generated list: two spaces, the term, then the description in a column.
// The column is as narrow as the list's own terms allow, between these bounds, so a list
// of short terms does not get the wide column a list of long ones needs. A term wider than
// the column keeps its description on the next line rather than pushing the column out for
// everything else - which is what the environment topic did by hand for
// BORGE_DELETE_I_KNOW_WHAT_I_AM_DOING.
const (
	enumMinWidth = 17
	enumMaxWidth = 29
	enumWrap     = 88
)

// enumColumn is the column the descriptions start in for a set of entries.
func enumColumn(entries []enumEntry) int {
	width := enumMinWidth
	for _, e := range entries {
		if n := len("  " + e.Term + " "); n > width {
			width = n
		}
	}
	if width > enumMaxWidth {
		width = enumMaxWidth
	}
	return width
}

// renderEnumEntries lays out one list in the given column.
func renderEnumEntries(entries []enumEntry, width int) string {
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		prefix := "  " + e.Term
		b.WriteString(prefix)
		if e.Description == "" {
			continue
		}
		if len(prefix) >= width {
			b.WriteString("\n" + strings.Repeat(" ", width))
		} else {
			b.WriteString(strings.Repeat(" ", width-len(prefix)))
		}
		for j, line := range wrapWords(e.Description, enumWrap-width) {
			if j > 0 {
				b.WriteString("\n" + strings.Repeat(" ", width))
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

// wrapWords breaks text into lines of at most width characters, never mid-word.
func wrapWords(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(word) <= width {
			lines[last] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}
