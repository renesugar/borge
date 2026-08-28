// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/renesugar/borge/internal/docs"
)

// The help topics, as templates.
//
// # Why a template rather than concatenation
//
// A topic could be assembled by collecting every fragment anchored to it and joining
// them. That makes the document's shape depend on the order the generator happened to
// read the files in: adding a file, renaming one, or splitting a package would silently
// reorder a user's help text, and no reviewer would see it in the diff.
//
// So each topic names the fragments it wants, in the order it wants them. A fragment that
// exists and is not named is reported rather than appended, and a name with no fragment
// behind it stops generation - the two directions that keep a template honest.
//
// # Where the prose lives
//
// In the doc comment of the declaration that implements it, which is the whole point:
// "borge asks at the terminal, up to three times" sits on unlockWithPrompt, so a change
// to the prompting puts the sentence in the same diff. Fragments with no single
// implementation - an introduction to a whole feature, a block of examples - are anchored
// here instead, on the declarations below, and that is a deliberate minority.

// partKind says what one piece of a topic is.
type partKind int

const (
	// partFragment interpolates an anchored doc comment.
	partFragment partKind = iota
	// partHeading is a section heading, which is presentation rather than prose.
	partHeading
	// partEnum interpolates a generated list (see helpenum.go).
	partEnum
	// partBlock interpolates a fragment and indents every line of it by two spaces.
	//
	// It exists because of gofmt: a doc comment that *starts* with an indented block has
	// the indentation taken off it, so a fragment that is nothing but example commands
	// cannot carry its own. Rather than fight the formatter with an escape nobody would
	// remember, the template says "this fragment is a block" and indents it here.
	partBlock
)

// topicPart is one piece of a topic, in the order it appears.
type topicPart struct {
	kind partKind
	// name is the //borge:help anchor, the heading text, or the list name with an
	// optional ":argument".
	name string
}

func fragment(name string) topicPart { return topicPart{partFragment, name} }
func heading(text string) topicPart  { return topicPart{partHeading, text} }
func enum(name string) topicPart     { return topicPart{partEnum, name} }
func block(name string) topicPart    { return topicPart{partBlock, name} }

// topicTemplate is one topic: its name, its one-line summary for the index, and its parts.
type topicTemplate struct {
	name    string
	summary string
	parts   []topicPart
}

// helpTemplates is the shape of every topic. It is data rather than text: the text is in
// the code that implements it.
func helpTemplates() []topicTemplate {
	return []topicTemplate{
		{
			name:    "patterns",
			summary: "selecting paths to include or exclude",
			parts: []topicPart{
				fragment("patterns/intro"),
				heading("STYLES"),
				fragment("patterns/styles"),
				enum("pattern-styles"),
				fragment("patterns/prefix-on-positionals"),
				heading("PATHS INSIDE AN ARCHIVE"),
				fragment("patterns/stored-paths"),
				heading("OPTIONS AND PATHS"),
				fragment("patterns/option-order"),
				heading("ORDER"),
				fragment("patterns/match-order"),
				heading("EXAMPLES"),
				block("patterns/examples"),
			},
		},
		{
			name:    "match-archives",
			summary: "selecting archives",
			parts: []topicPart{
				fragment("match-archives/intro"),
				enum("archive-selectors"),
				heading("FILTERS"),
				fragment("match-archives/filters"),
				heading("THE PROTECTED TAG"),
				fragment("match-archives/protected-tag"),
				heading("EXAMPLES"),
				block("match-archives/examples"),
			},
		},
		{
			name:    "placeholders",
			summary: "substitutions in archive names",
			parts: []topicPart{
				fragment("placeholders/intro"),
				enum("placeholders"),
				fragment("placeholders/one-instant"),
				heading("FORMATS"),
				fragment("placeholders/formats"),
				heading("LITERAL BRACES"),
				fragment("placeholders/braces"),
				heading("EXAMPLES"),
				block("placeholders/examples"),
			},
		},
		{
			name:    "compression",
			summary: "compression specifications",
			parts: []topicPart{
				fragment("compression/intro"),
				heading("SPECIFICATIONS"),
				enum("compression-specs"),
				fragment("compression/incompressible"),
				heading("LEVELS ARE COARSER THAN BORG'S"),
				fragment("compression/levels"),
				heading("EXAMPLES"),
				block("compression/examples"),
			},
		},
		{
			name:    "environment",
			summary: "environment variables borge reads",
			parts: []topicPart{
				fragment("environment/intro"),
				heading("REPOSITORY AND KEYS"),
				enum("environment-variables:repository and keys"),
				fragment("environment/keyfile-search"),
				fragment("environment/passphrases"),
				heading("CACHE"),
				enum("environment-variables:cache"),
				heading("BEHAVIOUR"),
				enum("environment-variables:behaviour"),
				heading("REMOTE REPOSITORIES"),
				enum("environment-variables:remote repositories"),
				fragment("environment/remote-not-ours"),
				heading("TUNING"),
				enum("environment-variables:tuning"),
				heading("TESTING"),
				enum("environment-variables:testing"),
				heading("NOT BORGE'S"),
				fragment("environment/not-borges"),
				heading("EXAMPLES"),
				block("environment/examples"),
			},
		},
	}
}

// renderTopic builds one topic's text from the anchored fragments.
//
// The result is what "borge help TOPIC" prints, and it is generated into
// help_generated.go rather than assembled at run time: a topic that stopped rendering
// correctly would then be a build failure and a diff, not a surprise for a user.
func renderTopic(t topicTemplate, set *docs.Set) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "borge help %s\n", t.name)
	for _, part := range t.parts {
		switch part.kind {
		case partHeading:
			fmt.Fprintf(&b, "\n%s\n", part.name)
		case partEnum:
			name, arg, _ := strings.Cut(part.name, ":")
			marker := "{{enum:" + name + "}}"
			if arg != "" {
				marker = "{{enum:" + name + ":" + arg + "}}"
			}
			rendered, err := renderEnumerations(marker)
			if err != nil {
				return "", fmt.Errorf("%s topic: %w", t.name, err)
			}
			fmt.Fprintf(&b, "\n%s\n", rendered)
		case partBlock, partFragment:
			fragment, ok := set.Fragment(part.name)
			if !ok {
				return "", fmt.Errorf("%s topic: no //borge:help %s anywhere in the source",
					t.name, part.name)
			}
			if fragment.Audience != "user" {
				return "", fmt.Errorf("%s topic: the %s fragment is //borge:doc %q, not user",
					t.name, part.name, fragment.Audience)
			}
			prose, err := renderProse(fragment.Prose)
			if err != nil {
				return "", fmt.Errorf("%s topic, %s fragment: %w", t.name, part.name, err)
			}
			if part.kind == partBlock {
				prose = indentLines(prose, "  ")
			}
			fmt.Fprintf(&b, "\n%s\n", prose)
		}
	}
	return b.String(), nil
}

// renderProse turns a doc comment into help text.
//
// One transformation, and it is forced: gofmt rewrites an indented block in a doc comment
// to a tab, so an example written with two spaces becomes "\tborge create ..." the first
// time anyone formats the file. The topics indent examples by two spaces, and
// TestHelpExamplesRun finds them that way, so a leading tab becomes two spaces here. Doing
// it the other way round - accepting whatever indentation the source has - would make the
// output depend on whether gofmt had run.
func renderProse(prose string) (string, error) {
	lines := strings.Split(strings.TrimRight(prose, "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "\t") {
			lines[i] = "  " + strings.TrimPrefix(line, "\t")
		}
		if strings.Contains(lines[i], "\t") {
			return "", fmt.Errorf("line %d contains a tab, which would not align in help "+
				"output: %q", i+1, line)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// generateHelpTopics renders every topic, and reports any fragment nothing names.
//
// The second return value is the leftovers: a //borge:help anchor in the source that no
// template asks for. That is prose written for a topic and never shown, which is worth a
// louder complaint than a missing one - the author believes it is documented.
func generateHelpTopics(set *docs.Set) (map[string]string, []string, error) {
	out := map[string]string{}
	named := map[string]bool{}
	for _, t := range helpTemplates() {
		body, err := renderTopic(t, set)
		if err != nil {
			return nil, nil, err
		}
		out[t.name] = body
		for _, part := range t.parts {
			if part.kind == partFragment || part.kind == partBlock {
				named[part.name] = true
			}
		}
	}
	var orphans []string
	for _, anchor := range set.HelpAnchors() {
		if !named[anchor] {
			orphans = append(orphans, anchor)
		}
	}
	return out, orphans, nil
}

// indentLines puts prefix in front of every non-empty line.
func indentLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// GenerateHelpFile renders every topic and returns the source of help_generated.go.
//
// Exported for cmd/docgen and used by TestDocsAreCurrent, which calls it and compares
// rather than shelling out: a freshness test that runs the generator is a test of the
// generator too.
//
// The second return value lists //borge:help anchors no template asks for.
func GenerateHelpFile(root string) (string, []string, error) {
	set, err := docs.Parse(root)
	if err != nil {
		return "", nil, err
	}
	if set.Files == 0 {
		return "", nil, fmt.Errorf("no Go files under %s; nothing to generate from", root)
	}
	topics, orphans, err := generateHelpTopics(set)
	if err != nil {
		return "", nil, err
	}

	names := make([]string, 0, len(topics))
	for _, t := range helpTemplates() {
		names = append(names, t.name)
	}

	var b strings.Builder
	b.WriteString(`// SPDX-License-Identifier: Apache-2.0

// Code generated by "make docgen"; DO NOT EDIT.
//
// The text below is assembled from the //borge:help fragments in the source and the
// generated lists in helpenum.go, in the order helptemplate.go asks for. Edit the doc
// comment beside the code that implements the sentence, not this file.

package cli

// helpGeneratedTopics is the rendered text of every topic, keyed by topic name.
var helpGeneratedTopics = map[string]string{
`)
	for _, name := range names {
		fmt.Fprintf(&b, "\t%q: %s,\n", name, quoteTopic(topics[name]))
	}
	b.WriteString("}\n")
	return b.String(), orphans, nil
}

// quoteTopic renders a topic as a Go raw string where it can be, and a quoted one where a
// backquote in the text makes that impossible.
func quoteTopic(body string) string {
	if !strings.Contains(body, "`") {
		return "`" + body + "`"
	}
	return fmt.Sprintf("%q", body)
}
