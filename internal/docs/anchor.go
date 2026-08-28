// SPDX-License-Identifier: Apache-2.0

// Package docs reads borge's documentation anchors: the directives that tie a piece of
// user-facing prose to the declaration that implements it.
//
// # Why anchors
//
// Four documentation claims went false during stage 8 while the code around them stayed
// correct. The two with tests behind them failed loudly; the two that were prose needed a
// human to notice. The cause is structural rather than careless - the sentence lives in
// internal/cli/help.go and the behaviour lives in internal/cli/passphrase.go, so a change
// to one does not put the other in the diff, and a reviewer cannot catch a claim they
// never see.
//
// So the prose moves onto the declaration and carries a directive saying where it
// belongs. Go's own tooling makes this work: a comment line matching ^//[a-z0-9]+:[a-z0-9]
// is a directive, so it is stripped from "go doc" output and from CommentGroup.Text()
// while remaining readable from CommentGroup.List.
//
// The only subset is "user". An "api" subset was specified and declined - see
// PLAN.md, R2 T4: borge has no exported API, every package is under internal/, and
// "go doc ./internal/..." already renders all 794 exported declarations from the same
// comments a generator would read.
//
// # The vocabulary
//
//	//borge:doc user              this comment is user-facing documentation
//	//borge:help topic[/section]  this comment is the source of that topic or section
//	//borge:enumerates expr       the list here is generated from the set the code defines
//	//borge:claim id              this prose makes a behavioural claim checked by id
//	//borge:checks id             this function is the registered check for that claim
//	//borge:about Decl            this prose describes that function, in the same package
//
// # Why //borge:about exists
//
// gofmt relocates directives to the end of a doc comment, so a fragment cannot mix
// maintainer rationale with user prose. borge's user-facing fragments therefore live on
// "var _ = helpText" carriers beside the code rather than on the functions themselves,
// and a carrier records the file but not the function. //borge:about restores the link
// the arrangement lost. It was added in R2 T5, when doccheck was found to be checking
// nothing: every user fragment was on a carrier, so a checker that looked only at
// functions had an empty list and reported a clean tree.
//
// docs/PORTING_PLAN.md 2.1 has the design and the findings that produced it.
package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// The directive vocabulary. An unrecognised //borge: directive is a finding rather than
// something to ignore: "//borge:claims foo" is a typo that would otherwise register
// nothing at all, silently, which is the failure mode the anchors exist to remove.
const (
	DirectiveDoc        = "doc"
	DirectiveHelp       = "help"
	DirectiveEnumerates = "enumerates"
	DirectiveClaim      = "claim"
	DirectiveChecks     = "checks"
	// DirectiveAbout names the declaration a fragment describes.
	//
	// It exists because of the carriers. A doc comment on the function that implements a
	// sentence needs no such directive - the attachment is the link. But gofmt moves
	// //borge: directives to the end of a comment, which made "rationale above, user
	// prose below" impossible, so user-facing fragments live on "var _ = helpText"
	// declarations beside the code instead (see internal/cli/helptemplate.go). That put
	// the prose in the right file and lost which function it is about, and doccheck has
	// nothing to read without it.
	DirectiveAbout = "about"
)

var knownDirectives = map[string]bool{
	DirectiveDoc:        true,
	DirectiveHelp:       true,
	DirectiveEnumerates: true,
	DirectiveClaim:      true,
	DirectiveChecks:     true,
	DirectiveAbout:      true,
}

// directiveRE matches a borge directive line. It follows Go's own rule for what counts as
// a directive - no space after the slashes - so that a line this package treats as an
// anchor is exactly a line Go strips from the rendered comment. A "// borge:help x" with
// a space is prose to Go and would be prose in the generated documentation too, so it is
// reported rather than honoured.
var directiveRE = regexp.MustCompile(`^//borge:([a-z0-9]+)(?:[ \t]+(.*))?$`)

// looseDirectiveRE catches the near-misses: a space after the slashes, or a capitalised
// name. Both read as anchors to a human and are invisible to the tooling.
var looseDirectiveRE = regexp.MustCompile(`^//[ \t]*[Bb]orge:([A-Za-z0-9]+)(?:[ \t]+(.*))?$`)

// Directive is one anchor line.
type Directive struct {
	Name string
	Arg  string
	File string
	Line int
}

// Block is one anchored doc comment, with the declaration it is attached to.
type Block struct {
	File       string
	Line       int
	Decl       string   // the declaration this comment documents
	Audience   string   // "user", "api", or "" when the comment carries no //borge:doc
	Topics     []string // //borge:help values, e.g. "environment/passphrases"
	Enumerates []string // //borge:enumerates values
	Claims     []string // //borge:claim ids
	About      []string // //borge:about declarations, e.g. "Env.unlockWithPrompt"
	Prose      string   // the comment with the directives removed, as Go renders it
	IsFunc     bool
	IsTest     bool // the block is in a _test.go file
}

// Check is a registered //borge:checks: a function that verifies a claim.
type Check struct {
	Claim string
	File  string
	Line  int
	Func  string
	IsFun bool
}

// Set is everything the parser found.
type Set struct {
	Blocks []Block
	Checks []Check
	// Malformed holds directives that are not part of the vocabulary, and near-misses
	// that Go would not treat as directives at all.
	Malformed []Directive
	// Decls maps a directory to the function declarations it defines, so a
	// //borge:about naming something that does not exist is a finding rather than a
	// pointer nobody follows.
	Decls map[string]map[string]bool
	// Files counts the Go files parsed, so a caller can refuse to trust a run that
	// scanned nothing. A parser that silently matched no files reports a clean audit.
	Files int
}

// skipDir names directories that are somebody else's code or not code at all.
func skipDir(name string) bool {
	switch name {
	case "vendor", ".git", "testdata", "bin", ".venv-borg2", "node_modules":
		return true
	}
	return false
}

// Parse reads every Go file under root, including tests, and returns its anchors.
func Parse(root string) (*Set, error) {
	set := &Set{Decls: map[string]map[string]bool{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		set.Files++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		collectFile(set, fset, file, rel)
		collectDecls(set, file, filepath.Dir(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return set, nil
}

// collectFile walks one file's declarations. Anchors live on declarations by design:
// colocation is the whole mechanism, and a free-floating anchored comment would document
// nothing in particular.
func collectFile(set *Set, fset *token.FileSet, file *ast.File, name string) {
	add := func(doc *ast.CommentGroup, decl string, isFunc bool) {
		if doc == nil {
			return
		}
		collectBlock(set, fset, doc, name, decl, isFunc)
	}
	// The file's own package comment can carry anchors: a package-level topic fragment is
	// a reasonable thing to write.
	add(file.Doc, "package "+file.Name.Name, false)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			add(d.Doc, funcName(d), true)
		case *ast.GenDecl:
			add(d.Doc, genDeclName(d), false)
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					add(s.Doc, specNames(s.Names), false)
				case *ast.TypeSpec:
					add(s.Doc, s.Name.Name, false)
				}
			}
		}
	}
}

// collectDecls records the functions a directory's files declare.
//
// Test files are included: a fragment may name a helper, and refusing to resolve it would
// turn a working pointer into a finding.
func collectDecls(set *Set, file *ast.File, dir string) {
	names := set.Decls[dir]
	if names == nil {
		names = map[string]bool{}
		set.Decls[dir] = names
	}
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			names[funcName(fd)] = true
		}
	}
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv != nil && len(d.Recv.List) > 0 {
		return receiverName(d.Recv.List[0].Type) + "." + d.Name.Name
	}
	return d.Name.Name
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // a generic receiver
		return receiverName(t.X)
	}
	return "?"
}

func genDeclName(d *ast.GenDecl) string {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			return specNames(s.Names)
		case *ast.TypeSpec:
			return s.Name.Name
		}
	}
	return d.Tok.String()
}

func specNames(names []*ast.Ident) string {
	var out []string
	for _, n := range names {
		out = append(out, n.Name)
	}
	return strings.Join(out, ", ")
}

// collectBlock turns one comment group into a Block, a set of Checks, or neither.
func collectBlock(set *Set, fset *token.FileSet, doc *ast.CommentGroup, file, decl string, isFunc bool) {
	isTest := strings.HasSuffix(file, "_test.go")
	block := Block{
		File:   file,
		Line:   fset.Position(doc.Pos()).Line,
		Decl:   decl,
		Prose:  strings.TrimSpace(doc.Text()),
		IsFunc: isFunc,
		IsTest: isTest,
	}
	anchored := false
	for _, comment := range doc.List {
		line := fset.Position(comment.Pos()).Line
		text := comment.Text
		match := directiveRE.FindStringSubmatch(text)
		if match == nil {
			// Not a directive to Go. It may still be a near-miss that reads as one.
			if loose := looseDirectiveRE.FindStringSubmatch(text); loose != nil {
				set.Malformed = append(set.Malformed, Directive{
					Name: loose[1], Arg: strings.TrimSpace(loose[2]), File: file, Line: line,
				})
			}
			continue
		}
		name, arg := match[1], strings.TrimSpace(match[2])
		if !knownDirectives[name] {
			set.Malformed = append(set.Malformed, Directive{
				Name: name, Arg: arg, File: file, Line: line,
			})
			continue
		}
		anchored = true
		switch name {
		case DirectiveDoc:
			block.Audience = arg
		case DirectiveHelp:
			block.Topics = append(block.Topics, arg)
		case DirectiveEnumerates:
			block.Enumerates = append(block.Enumerates, arg)
		case DirectiveClaim:
			block.Claims = append(block.Claims, arg)
		case DirectiveAbout:
			block.About = append(block.About, arg)
		case DirectiveChecks:
			set.Checks = append(set.Checks, Check{
				Claim: arg, File: file, Line: line, Func: decl, IsFun: isFunc,
			})
		}
	}
	if !anchored {
		return
	}
	// A comment that only registers checks is not a documentation block.
	if block.Audience == "" && len(block.Topics) == 0 &&
		len(block.Enumerates) == 0 && len(block.Claims) == 0 && len(block.About) == 0 {
		return
	}
	set.Blocks = append(set.Blocks, block)
}

// Fragment returns the block anchored to one //borge:help value, which is a topic name or
// a "topic/section".
//
// The generator asks for fragments by name rather than walking the set in source order:
// assembling a document from whatever order the files happened to be read in makes its
// shape depend on the filesystem, which is neither reviewable nor stable.
func (s *Set) Fragment(name string) (Block, bool) {
	for _, b := range s.Blocks {
		for _, anchor := range b.Topics {
			if anchor == name {
				return b, true
			}
		}
	}
	return Block{}, false
}

// HelpAnchors lists every //borge:help value in the set, in source order.
func (s *Set) HelpAnchors() []string {
	var out []string
	for _, b := range s.Blocks {
		out = append(out, b.Topics...)
	}
	return out
}
