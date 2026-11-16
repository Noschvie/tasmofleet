package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// source is one Go file of a package directory, by file name.
type source struct {
	name string
	src  []byte
}

type parsedFile struct {
	name string
	src  []byte
	fset *token.FileSet
	file *ast.File
}

func parseSource(s source) (*parsedFile, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, s.name, s.src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return &parsedFile{name: s.name, src: s.src, fset: fset, file: f}, nil
}

func (p *parsedFile) off(pos token.Pos) int { return p.fset.Position(pos).Offset }

func (p *parsedFile) text(from, to token.Pos) string { return string(p.src[p.off(from):p.off(to)]) }

// declStart is where a declaration starts including its doc comment (a //go:embed line lives there).
func (p *parsedFile) declStart(d ast.Decl) int {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Doc != nil {
			return p.off(d.Doc.Pos())
		}
	case *ast.GenDecl:
		if d.Doc != nil {
			return p.off(d.Doc.Pos())
		}
	}
	return p.off(d.Pos())
}

func (p *parsedFile) declText(d ast.Decl) string { return string(p.src[p.declStart(d):p.off(d.End())]) }

type funcRef struct {
	p  *parsedFile
	fd *ast.FuncDecl
}

type valRef struct {
	p    *parsedFile
	spec *ast.ValueSpec
}

// decls indexes the top-level declarations of one package. Functions and methods are keyed by receiver type
// (pointer, type parameters and receiver name left out) and name, so that a method matches across the
// package's files and across a change from T to *T.
type decls struct {
	funcs map[string]funcRef
	types map[string]bool
	vals  map[string]valRef
}

func newDecls() *decls {
	return &decls{funcs: map[string]funcRef{}, types: map[string]bool{}, vals: map[string]valRef{}}
}

func (d *decls) add(p *parsedFile) {
	for _, decl := range p.file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if isInit(decl) || decl.Name.Name == "_" {
				continue
			}
			d.funcs[funcKey(decl)] = funcRef{p, decl}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					d.types[spec.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						if n.Name != "_" {
							d.vals[n.Name] = valRef{p, spec}
						}
					}
				}
			}
		}
	}
}

// declared reports whether a package-level name is taken by a type, a variable, a constant or a plain function.
func (d *decls) declared(name string) bool {
	_, fn := d.funcs[name]
	_, val := d.vals[name]
	return fn || val || d.types[name]
}

func isInit(fd *ast.FuncDecl) bool { return fd.Recv == nil && fd.Name.Name == "init" }

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return recvName(fd.Recv.List[0].Type) + "." + fd.Name.Name
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.ParenExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

func recvIsPointer(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	e := fd.Recv.List[0].Type
	for {
		switch t := e.(type) {
		case *ast.ParenExpr:
			e = t.X
		case *ast.StarExpr:
			return true
		default:
			return false
		}
	}
}

// sigShape is a function's signature with every name left out: type parameters (constraints), parameter types,
// result types and whether the receiver is a pointer. Two functions of equal shape can swap bodies and
// signatures; the base body then also finds the parameter names it uses.
func sigShape(p *parsedFile, fd *ast.FuncDecl) string {
	var b strings.Builder
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			t := strings.Join(strings.Fields(p.text(f.Type.Pos(), f.Type.End())), " ")
			for i := 0; i < max(1, len(f.Names)); i++ {
				b.WriteString(t)
				b.WriteByte(',')
			}
		}
	}
	fmt.Fprintf(&b, "ptr=%v[", recvIsPointer(fd))
	fields(fd.Type.TypeParams)
	b.WriteString("](")
	fields(fd.Type.Params)
	b.WriteString(")(")
	fields(fd.Type.Results)
	b.WriteString(")")
	return b.String()
}

// zeroBody is the body of a function the base does not have: it only returns zero values.
func zeroBody(p *parsedFile, fd *ast.FuncDecl) string {
	res := fd.Type.Results
	if res == nil || len(res.List) == 0 {
		return "{\n}"
	}
	if len(res.List[0].Names) > 0 {
		return "{\n\treturn\n}"
	}
	var vars, names []string
	for i, f := range res.List {
		v := fmt.Sprintf("__zero%d", i)
		vars = append(vars, fmt.Sprintf("\tvar %s %s\n", v, p.text(f.Type.Pos(), f.Type.End())))
		names = append(names, v)
	}
	return "{\n" + strings.Join(vars, "") + "\treturn " + strings.Join(names, ", ") + "\n}"
}

type edit struct {
	start, end int
	text       string
}

func applyEdits(src []byte, edits []edit) ([]byte, error) {
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), src...)
	last := len(src) + 1
	for _, e := range edits {
		if e.end > last || e.start > e.end {
			return nil, fmt.Errorf("overlapping edits at %d-%d", e.start, e.end)
		}
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
		last = e.start
	}
	return out, nil
}

// rewriteDir makes, for every changed production file of one package directory, the file the "behaviour
// reverted, API kept" variant needs. head and base are the non-test files of the directory at the head and at
// the merge base, tests the head's test files of the package (their names are never declared a second time),
// changed the names of the files the branch changed. The result holds the new content of every changed file
// that exists on either side:
//   - a function or method that exists in the base gets the base's body (and, when its signature differs only
//     in names, the base's signature); a new one gets a body that returns zero values;
//   - head's init functions are dropped and the base's of the same file come back;
//   - a variable or constant that exists in the base gets the base's value;
//   - declarations only the base has (renamed, deleted) come back, so base bodies still compile;
//   - head's types stay, so the tests keep compiling against the new struct fields and method sets;
//   - imports become the union: a missing one is taken from the base, an unused one turns into a blank import;
//   - a file the branch deleted comes back without the declarations the head declares elsewhere.
func rewriteDir(head, base, tests []source, changed map[string]bool, pkgName func(string) string) (map[string][]byte, error) {
	parse := func(srcs []source, skipExternal bool) ([]*parsedFile, error) {
		var out []*parsedFile
		for _, s := range srcs {
			p, err := parseSource(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", s.name, err)
			}
			if skipExternal && strings.HasSuffix(p.file.Name.Name, "_test") {
				continue
			}
			out = append(out, p)
		}
		return out, nil
	}
	headFiles, err := parse(head, false)
	if err != nil {
		return nil, err
	}
	baseFiles, err := parse(base, false)
	if err != nil {
		return nil, err
	}
	testFiles, err := parse(tests, true)
	if err != nil {
		return nil, err
	}
	baseIdx, headNames := newDecls(), newDecls()
	for _, p := range baseFiles {
		baseIdx.add(p)
	}
	for _, p := range append(append([]*parsedFile{}, headFiles...), testFiles...) {
		headNames.add(p)
	}
	byName := func(files []*parsedFile, name string) *parsedFile {
		for _, p := range files {
			if p.name == name {
				return p
			}
		}
		return nil
	}
	out := map[string][]byte{}
	for name := range changed {
		hp, bp := byName(headFiles, name), byName(baseFiles, name)
		if hp == nil && bp == nil {
			continue
		}
		var res []byte
		var from []*parsedFile
		if hp != nil {
			res, from, err = rewriteHead(hp, bp, baseIdx, headNames)
		} else {
			res, from, err = trimBase(bp, headNames)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		from = append(from, baseFiles...)
		res, err = fixImports(name, res, from, headNames, pkgName)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = res
	}
	return out, nil
}

// rewriteHead rewrites one head file. The second result lists the base files the new text was taken from.
func rewriteHead(hp, bp *parsedFile, baseIdx, headNames *decls) ([]byte, []*parsedFile, error) {
	var edits []edit
	var from []*parsedFile
	for _, decl := range hp.file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if isInit(decl) {
				edits = append(edits, edit{hp.declStart(decl), hp.off(decl.End()), ""})
				continue
			}
			if decl.Body == nil || decl.Name.Name == "_" {
				continue
			}
			bodyFrom, bodyTo := hp.off(decl.Body.Lbrace), hp.off(decl.Body.Rbrace)+1
			ref, ok := baseIdx.funcs[funcKey(decl)]
			switch {
			case !ok || ref.fd.Body == nil:
				edits = append(edits, edit{bodyFrom, bodyTo, zeroBody(hp, decl)})
			case sigShape(hp, decl) == sigShape(ref.p, ref.fd):
				from = append(from, ref.p)
				start, bstart := hp.off(decl.Name.Pos()), ref.p.off(ref.fd.Name.Pos())
				if decl.Recv != nil && ref.fd.Recv != nil {
					start, bstart = hp.off(decl.Recv.Pos()), ref.p.off(ref.fd.Recv.Pos())
				}
				edits = append(edits, edit{start, bodyTo, string(ref.p.src[bstart : ref.p.off(ref.fd.Body.Rbrace)+1])})
			default:
				from = append(from, ref.p)
				edits = append(edits, edit{bodyFrom, bodyTo, ref.p.text(ref.fd.Body.Lbrace, ref.fd.Body.Rbrace+1)})
			}
		case *ast.GenDecl:
			if decl.Tok != token.VAR && decl.Tok != token.CONST {
				continue
			}
			for _, spec := range decl.Specs {
				if e, bp, ok := baseValues(hp, spec.(*ast.ValueSpec), baseIdx); ok {
					from = append(from, bp)
					edits = append(edits, e)
				}
			}
		}
	}
	var add []string
	if bp != nil {
		for _, decl := range bp.file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if isInit(decl) || (decl.Name.Name != "_" && !headNames.hasFunc(decl)) {
					add = append(add, bp.declText(decl))
				}
			case *ast.GenDecl:
				add = append(add, baseOnly(bp, decl, headNames)...)
			}
		}
	}
	for _, t := range add {
		edits = append(edits, edit{len(hp.src), len(hp.src), "\n\n" + t + "\n"})
	}
	if bp != nil {
		from = append(from, bp)
	}
	res, err := applyEdits(hp.src, edits)
	return res, from, err
}

func (d *decls) hasFunc(fd *ast.FuncDecl) bool {
	if fd.Recv == nil {
		return d.declared(fd.Name.Name)
	}
	_, ok := d.funcs[funcKey(fd)]
	return ok
}

// baseValues is the edit that puts the base's value back into a head variable or constant spec of the same
// names, in the same order, when both have values.
func baseValues(hp *parsedFile, spec *ast.ValueSpec, baseIdx *decls) (edit, *parsedFile, bool) {
	if len(spec.Values) == 0 || len(spec.Names) == 0 {
		return edit{}, nil, false
	}
	first, ok := baseIdx.vals[spec.Names[0].Name]
	if !ok || len(first.spec.Values) == 0 || len(first.spec.Names) != len(spec.Names) {
		return edit{}, nil, false
	}
	for i, n := range spec.Names {
		if first.spec.Names[i].Name != n.Name {
			return edit{}, nil, false
		}
	}
	vals := first.spec.Values
	return edit{
		hp.off(spec.Values[0].Pos()), hp.off(spec.Values[len(spec.Values)-1].End()),
		first.p.text(vals[0].Pos(), vals[len(vals)-1].End()),
	}, first.p, true
}

// baseOnly lists the texts of the specs of a base declaration that no head file or test declares.
func baseOnly(bp *parsedFile, decl *ast.GenDecl, headNames *decls) []string {
	if decl.Tok == token.IMPORT {
		return nil
	}
	var keep []ast.Spec
	for _, spec := range decl.Specs {
		if !specClashes(spec, headNames) && !blankOnly(spec) {
			keep = append(keep, spec)
		}
	}
	switch {
	case len(keep) == 0:
		return nil
	case len(keep) == len(decl.Specs):
		return []string{bp.declText(decl)}
	}
	var out []string
	for _, spec := range keep {
		out = append(out, decl.Tok.String()+" "+bp.text(spec.Pos(), spec.End()))
	}
	return out
}

func blankOnly(spec ast.Spec) bool {
	vs, ok := spec.(*ast.ValueSpec)
	if !ok {
		return false
	}
	for _, n := range vs.Names {
		if n.Name != "_" {
			return false
		}
	}
	return true
}

func specClashes(spec ast.Spec, headNames *decls) bool {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		return headNames.declared(spec.Name.Name)
	case *ast.ValueSpec:
		for _, n := range spec.Names {
			if n.Name != "_" && headNames.declared(n.Name) {
				return true
			}
		}
	}
	return false
}

// trimBase is the base's version of a file the branch deleted, without what the head declares elsewhere.
func trimBase(bp *parsedFile, headNames *decls) ([]byte, []*parsedFile, error) {
	var edits []edit
	for _, decl := range bp.file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if !isInit(decl) && decl.Name.Name != "_" && headNames.hasFunc(decl) {
				edits = append(edits, edit{bp.declStart(decl), bp.off(decl.End()), ""})
			}
		case *ast.GenDecl:
			if decl.Tok == token.IMPORT {
				continue
			}
			var clash []ast.Spec
			for _, spec := range decl.Specs {
				if specClashes(spec, headNames) {
					clash = append(clash, spec)
				}
			}
			if len(clash) == len(decl.Specs) && len(clash) > 0 {
				edits = append(edits, edit{bp.declStart(decl), bp.off(decl.End()), ""})
				continue
			}
			for _, spec := range clash {
				edits = append(edits, edit{bp.off(spec.Pos()), bp.off(spec.End()), ""})
			}
		}
	}
	res, err := applyEdits(bp.src, edits)
	return res, nil, err
}

// fixImports makes the imports of a rewritten file the ones its text needs: a package it uses and does not
// import is taken from the base files (candidates), an import it no longer uses stays as a blank import (its
// init side effects stay as the head has them; the file still compiles).
func fixImports(name string, src []byte, candidates []*parsedFile, headNames *decls, pkgName func(string) string) ([]byte, error) {
	p, err := parseSource(source{name, src})
	if err != nil {
		return nil, fmt.Errorf("the rewrite produced invalid Go: %w", err)
	}
	used := map[string]bool{}
	ast.Inspect(p.file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil {
				used[id.Name] = true
			}
		}
		return true
	})
	type imp struct{ alias, path string }
	var keep []imp
	provided := map[string]bool{}
	blank := map[string]bool{}
	for _, is := range p.file.Imports {
		path, err := strconv.Unquote(is.Path.Value)
		if err != nil {
			return nil, err
		}
		alias := ""
		if is.Name != nil {
			alias = is.Name.Name
		}
		switch {
		case path == "C":
			return src, nil // cgo: leave the file as it is
		case alias == "_" || alias == ".":
			keep = append(keep, imp{alias, path})
			blank[path] = alias == "_"
		default:
			eff := alias
			if eff == "" {
				eff = pkgName(path)
			}
			if used[eff] {
				keep = append(keep, imp{alias, path})
				provided[eff] = true
			} else if !blank[path] {
				keep = append(keep, imp{"_", path})
				blank[path] = true
			}
		}
	}
	var missing []string
	for n := range used {
		if !provided[n] && !headNames.declared(n) {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	for _, n := range missing {
	find:
		for _, c := range candidates {
			for _, is := range c.file.Imports {
				path, err := strconv.Unquote(is.Path.Value)
				if err != nil || path == "C" {
					continue
				}
				alias := ""
				if is.Name != nil {
					alias = is.Name.Name
				}
				eff := alias
				if eff == "" {
					eff = pkgName(path)
				}
				if eff == n && alias != "_" && alias != "." {
					keep = append(keep, imp{alias, path})
					break find
				}
			}
		}
	}
	var edits []edit
	for _, d := range p.file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			edits = append(edits, edit{p.declStart(d), p.off(d.End()), ""})
		}
	}
	if len(keep) > 0 {
		var b strings.Builder
		b.WriteString("\n\nimport (\n")
		for _, k := range keep {
			if k.alias != "" {
				b.WriteString("\t" + k.alias + " " + strconv.Quote(k.path) + "\n")
			} else {
				b.WriteString("\t" + strconv.Quote(k.path) + "\n")
			}
		}
		b.WriteString(")\n")
		at := p.off(p.file.Name.End())
		edits = append(edits, edit{at, at, b.String()})
	}
	res, err := applyEdits(src, edits)
	if err != nil {
		return nil, err
	}
	formatted, err := format.Source(res)
	if err != nil {
		return nil, fmt.Errorf("the rewrite produced invalid Go: %w", err)
	}
	return formatted, nil
}
