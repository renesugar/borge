// SPDX-License-Identifier: Apache-2.0

package doccheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"sort"
	"strings"
)

// The code a claim is judged against.
//
// # Why a declaration alone is not enough
//
// unlockWithPrompt reads as "prompts in a loop"; that the echo is disabled lives one call
// down in promptPassphrase. A reading built from the declaration on its own is confident
// and incomplete, which is the worst combination for a checker whose output a human has to
// triage. So the unit is the declaration plus the functions it calls directly, within its
// own package.
//
// # Why only one level, and only within the package
//
// Two levels reaches the standard library and the whole repository, and the budget is
// spent on code the claim is not about. A claim that genuinely needs more than one hop is a
// claim anchored in the wrong place - the sentence belongs beside the function that
// implements it, which is the rule the anchors exist to enforce.

// Unit is a declaration and its direct callees, as source text.
type Unit struct {
	// Name is the declaration the claim is anchored to, e.g. "Env.unlockWithPrompt".
	Name string
	// Source is the declaration followed by each callee, in call order.
	Source string
	// Callees names what was pulled in, so a reading that missed something has a visible
	// reason rather than an invisible one.
	Callees []string
	// Truncated reports that the budget stopped the unit short. A reading built from a
	// truncated unit is worth less, and the report says so rather than hiding it.
	Truncated bool
}

// UnitBudget is how many bytes of source a unit may carry.
//
// The model this was built against has an 8192-token context, and the reading prompt has
// to fit the code, the instructions and the answer. 6000 bytes of Go is roughly 1800
// tokens, which leaves room for all three with the margin a long declaration needs.
const UnitBudget = 6000

// ExtractUnit reads the package in dir and returns the unit for one declaration.
//
// decl is "Func" or "Type.Method"; test files are excluded, because a claim about
// behaviour is a claim about the program rather than about its tests.
func ExtractUnit(dir, decl string) (Unit, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return Unit{}, fmt.Errorf("parsing %s: %w", dir, err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				funcs[declName(fd)] = fd
			}
		}
	}

	root, ok := funcs[decl]
	if !ok {
		return Unit{}, fmt.Errorf("no function %q in %s", decl, dir)
	}

	var b strings.Builder
	u := Unit{Name: decl}
	if err := writeDecl(&b, fset, root); err != nil {
		return Unit{}, err
	}
	for _, name := range directCallees(root, funcs, methodsByName(funcs)) {
		var one strings.Builder
		if err := writeDecl(&one, fset, funcs[name]); err != nil {
			return Unit{}, err
		}
		if b.Len()+one.Len() > UnitBudget {
			u.Truncated = true
			break
		}
		b.WriteString("\n")
		b.WriteString(one.String())
		u.Callees = append(u.Callees, name)
	}
	u.Source = b.String()
	return u, nil
}

// writeDecl prints one declaration with its doc comment removed.
//
// The comment is exactly what is being judged, so leaving it in the code would hand the
// reading step the answer - and a reading anchored on the claim is the thing the two-step
// design exists to avoid.
func writeDecl(b *strings.Builder, fset *token.FileSet, fd *ast.FuncDecl) error {
	stripped := *fd
	stripped.Doc = nil
	if err := printer.Fprint(b, fset, &stripped); err != nil {
		return err
	}
	b.WriteString("\n")
	return nil
}

// directCallees names the functions a declaration calls, in the order it calls them.
//
// Only calls that resolve to a function in the same package are kept: a call to fmt or to
// another package is behaviour the reader is expected to know, and pulling it in would
// spend the budget on the standard library.
func directCallees(fd *ast.FuncDecl, funcs map[string]*ast.FuncDecl, methods map[string][]string) []string {
	seen := map[string]bool{declName(fd): true}
	var out []string
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, name := range calleeNames(call.Fun, methods) {
			if seen[name] || funcs[name] == nil {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
		return true
	})
	return out
}

// methodsByName indexes the package's methods by their bare name.
func methodsByName(funcs map[string]*ast.FuncDecl) map[string][]string {
	out := map[string][]string{}
	for full := range funcs {
		if _, method, isMethod := strings.Cut(full, "."); isMethod {
			out[method] = append(out[method], full)
		}
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

// calleeNames gives the names a call expression resolves to.
//
// A method call is written e.method(), and which type e has is not known without type
// checking. So a method is followed only when the package declares exactly one with that
// name. Two methods of the same name on different types is an ambiguity this resolves by
// following neither: a unit that quietly showed the wrong function would produce a reading
// that is confident and about other code, which is worse than a reading that is short.
func calleeNames(fun ast.Expr, methods map[string][]string) []string {
	switch f := fun.(type) {
	case *ast.Ident:
		return []string{f.Name}
	case *ast.SelectorExpr:
		// pkg.Func or receiver.Method. The plain name covers a function called through
		// a package qualifier, which the funcs lookup then rejects unless this package
		// declares it too.
		names := []string{f.Sel.Name}
		if candidates := methods[f.Sel.Name]; len(candidates) == 1 {
			names = append(names, candidates[0])
		}
		return names
	}
	return nil
}

// declName is "Func" for a function and "Type.Method" for a method.
func declName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return receiverType(fd.Recv.List[0].Type) + "." + fd.Name.Name
}

// receiverType is the type name in a receiver, with any pointer star removed.
func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // a generic receiver, Type[T]
		return receiverType(t.X)
	}
	return ""
}
