package netaddr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// redactAllowed lists the redaction functions outside this package that read a
// URL on purpose, with the reason. Everything else that hides parts of a URL
// goes through RedactURL or URLHost.
var redactAllowed = map[string]string{
	"internal/config.RedactWebhookURL":       "hides the path and the query, because they can be the secret itself (#249); RedactURL keeps the path",
	"internal/settings.RedactURLCredentials": "reads a URL from a device and keeps its query, so the policy comparison still works; RedactURL drops the query",
}

// Why: a URL redaction written twice drifts. One copy kept the query, another
// dropped it, two disagreed about what to show for a value that does not parse,
// and a fix in one never reached the other (#1055). A function that hides parts
// of a URL must live here, so that a password in a broker URL is hidden by one
// rule; the two rules that differ on purpose are named above with their reason.
//
// The test looks at every function whose name contains "redact" outside this
// package: it must not call net/url Parse, directly or through a function of
// its own package that does.
func TestNoURLRedactionOutsideNetaddr(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]string{} // "dir.Func" -> file
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if rel, _ := filepath.Rel(root, path); rel == filepath.Join("internal", "netaddr") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			for name, file := range urlRedactors(t, root, path) {
				found[name] = file
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var unexpected []string
	for name, file := range found {
		if _, ok := redactAllowed[name]; !ok {
			unexpected = append(unexpected, name+" ("+file+")")
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("URL redaction outside internal/netaddr: %s\nUse netaddr.RedactURL or netaddr.URLHost, or add the function to redactAllowed with the reason its rule differs",
			strings.Join(unexpected, ", "))
	}
	for name, why := range redactAllowed {
		if _, ok := found[name]; !ok {
			t.Errorf("redactAllowed lists %s (%s) but no such function reads a URL any more: remove the entry", name, why)
		}
	}
}

// urlRedactors returns the functions of the package in dir whose name contains
// "redact" and that call net/url Parse (or ParseRequestURI), directly or through
// a plain function call to a function of the same package that does.
func urlRedactors(t *testing.T, root, dir string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	pkgs := map[string]map[string]*ast.File{} // package name -> path -> file
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if pkgs[f.Name.Name] == nil {
			pkgs[f.Name.Name] = map[string]*ast.File{}
		}
		pkgs[f.Name.Name][path] = f
	}
	out := map[string]string{}
	rel, _ := filepath.Rel(root, dir)
	for _, files := range pkgs {
		type fn struct {
			file   string
			parses bool
			calls  []string
		}
		funcs := map[string]*fn{}
		for path, f := range files {
			alias := urlImportName(f)
			if alias == "" {
				alias = "\x00" // no net/url in this file: nothing can match
			}
			for _, decl := range f.Decls {
				d, ok := decl.(*ast.FuncDecl)
				if !ok || d.Body == nil {
					continue
				}
				e := &fn{file: filepath.ToSlash(filepath.Join(rel, filepath.Base(path)))}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch c := call.Fun.(type) {
					case *ast.SelectorExpr:
						if id, ok := c.X.(*ast.Ident); ok && id.Name == alias && (c.Sel.Name == "Parse" || c.Sel.Name == "ParseRequestURI") {
							e.parses = true
						}
					case *ast.Ident:
						e.calls = append(e.calls, c.Name)
					}
					return true
				})
				funcs[d.Name.Name] = e
			}
		}
		for name, e := range funcs {
			if !strings.Contains(strings.ToLower(name), "redact") {
				continue
			}
			reads := e.parses
			for _, c := range e.calls {
				if callee := funcs[c]; callee != nil && callee.parses {
					reads = true
				}
			}
			if reads {
				out[filepath.ToSlash(rel)+"."+name] = e.file
			}
		}
	}
	return out
}

// urlImportName is the name under which f imports net/url ("" if it does not).
func urlImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path.Value != `"net/url"` {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "url"
	}
	return ""
}
