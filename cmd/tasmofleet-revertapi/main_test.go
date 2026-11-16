package main

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rewrite is what turns "the tests do not build without the change" into a real verdict (#681). Every case
// here type-checks the rewritten package together with the head's tests: a variant that does not build is
// worthless (the check would call it inconclusive), so "compiles" is part of what each case pins.

func stdName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// build type-checks the package made of the rewritten files, the head files that were not changed and the tests.
func typeCheck(t *testing.T, out map[string][]byte, head, tests []source) {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	add := func(name string, b []byte) {
		f, err := parser.ParseFile(fset, name, b, 0)
		if err != nil {
			t.Fatalf("%s does not parse: %v\n%s", name, err, b)
		}
		files = append(files, f)
	}
	for name, b := range out {
		add(name, b)
	}
	for _, s := range head {
		if _, ok := out[s.name]; !ok {
			add(s.name, s.src)
		}
	}
	for _, s := range tests {
		add(s.name, s.src)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := conf.Check("p", fset, files, nil); err != nil {
		for name, b := range out {
			t.Logf("--- %s\n%s", name, b)
		}
		t.Fatalf("the rewritten package does not compile: %v", err)
	}
}

func rewrite(t *testing.T, head, base, tests []source, changed ...string) map[string][]byte {
	t.Helper()
	ch := map[string]bool{}
	for _, c := range changed {
		ch[c] = true
	}
	out, err := rewriteDir(head, base, tests, ch, stdName)
	if err != nil {
		t.Fatal(err)
	}
	typeCheck(t, out, head, tests)
	return out
}

func contains(t *testing.T, got []byte, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(string(got), w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func absent(t *testing.T, got []byte, not ...string) {
	t.Helper()
	for _, w := range not {
		if strings.Contains(string(got), w) {
			t.Errorf("unexpected %q in:\n%s", w, got)
		}
	}
}

func TestExistingFunctionGetsTheBaseBodyAndNewOnesReturnZeroValues(t *testing.T) {
	// A test that uses new API but asserts old behaviour (Clamp(9,5)==5) has to pass here, and one that
	// asserts the new behaviour has to fail: existing bodies come from the base, never from the head.
	base := []source{{"a.go", []byte("package p\n\nfunc Clamp(n, max int) int {\n\tif n > max {\n\t\treturn max\n\t}\n\treturn n\n}\n")}}
	head := []source{{"a.go", []byte(`package p

func Clamp(x, limit int) int {
	if x < 0 {
		return 0
	}
	if x > limit {
		return limit
	}
	return x
}

func Double(n int) int { return 2 * n }
func Pair(a string) (int, error) { return len(a), nil }
func Named(a string) (n int, err error) { return 1, nil }
func Nothing() { panic("new") }
func Gen[K comparable, V any](m map[K]V) (K, V, bool) { for k, v := range m { return k, v, true }; var k K; var v V; return k, v, false }

type Box[T any] struct{ v T }

func (b *Box[T]) Get() (T, bool) { return b.v, true }
`)}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, "if n > max {", "return n", "var __zero0 int", "var __zero1 error", "return __zero0, __zero1",
		"return __zero0, __zero1, __zero2")
	absent(t, out, "x < 0", "return 2 * n", `panic("new")`, "len(a)", "return 1, nil", "return b.v, true")
	contains(t, out, "func Named(a string) (n int, err error) {\n\treturn\n}")
	contains(t, out, "func Nothing() {\n}")
}

func TestSignatureThatOnlyRenamesParametersTakesTheBaseSignatureToo(t *testing.T) {
	// The base body uses the base's parameter and receiver names; without them it would not compile. The head's
	// new field stays, because types are the head's.
	base := []source{{"a.go", []byte("package p\n\ntype S struct{ n int }\n\nfunc (s *S) Add(by int) int { s.n += by; return s.n }\n")}}
	head := []source{{"a.go", []byte("package p\n\ntype S struct{ n, extra int }\n\nfunc (srv *S) Add(delta int) int { return delta }\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, "type S struct{ n, extra int }", "func (s *S) Add(by int) int { s.n += by; return s.n }")
}

func TestBodyOfAChangedResultTypeIsKeptAndTheVariantStaysBroken(t *testing.T) {
	// A changed result type: the base body cannot satisfy it. The rewrite does not hide that by guessing; the
	// variant fails to build and the check reports "inconclusive" from the build output.
	base := []source{{"a.go", []byte("package p\n\nfunc F() int { return 1 }\n")}}
	head := []source{{"a.go", []byte("package p\n\nfunc F() string { return \"x\" }\n")}}
	out, err := rewriteDir(head, base, nil, map[string]bool{"a.go": true}, stdName)
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out["a.go"], "func F() string { return 1 }")
}

func TestImportsAreTheUnionAndAnUnusedOneBecomesBlank(t *testing.T) {
	// The base body needs strings, which the head dropped; the head's fmt use was in a body that went back, so
	// the import is unused now. Both must still compile, or the variant is lost over a bookkeeping detail.
	base := []source{{"a.go", []byte("package p\n\nimport \"strings\"\n\nfunc Name(s string) string { return strings.ToUpper(s) }\n")}}
	head := []source{{"a.go", []byte("package p\n\nimport \"fmt\"\n\nfunc Name(s string) string { return fmt.Sprint(s) }\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, `"strings"`, `_ "fmt"`)
	absent(t, out, "fmt.Sprint")
}

func TestAliasedImportKeepsItsAlias(t *testing.T) {
	base := []source{{"a.go", []byte("package p\n\nimport str \"strings\"\n\nfunc Name(s string) string { return str.ToUpper(s) }\n")}}
	head := []source{{"a.go", []byte("package p\n\nfunc Name(s string) string { return s }\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, `str "strings"`, "str.ToUpper")
}

func TestInitFunctionsComeFromTheBase(t *testing.T) {
	// A new init is new behaviour (registration, a default) and has to go; the base's init stays.
	base := []source{{"a.go", []byte("package p\n\nvar Mode = \"base\"\n\nfunc init() { Mode = \"base-init\" }\n")}}
	head := []source{{"a.go", []byte("package p\n\nvar Mode = \"head\"\n\nfunc init() { Mode = \"head-init\" }\n\nfunc init() { Mode += \"2\" }\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, `Mode = "base-init"`, `Mode = "base"`)
	absent(t, out, "head-init", `"head"`, `Mode += "2"`)
}

func TestVariablesAndConstantsGetTheirBaseValueAndNewOnesStay(t *testing.T) {
	// A changed table value or constant is behaviour. A new constant has no base value and keeps the head's, so
	// the tests that name it compile.
	base := []source{{"a.go", []byte("package p\n\nconst Limit = 5\n\nvar Names = map[string]int{\"a\": 1}\n")}}
	head := []source{{"a.go", []byte("package p\n\nconst (\n\tLimit = 9\n\tExtra = 3\n)\n\nvar Names = map[string]int{\"a\": 2, \"b\": 3}\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, "Limit = 5", "Extra = 3", `map[string]int{"a": 1}`)
	absent(t, out, "Limit = 9", `"b": 3`)
}

func TestDeclarationsOnlyTheBaseHasComeBackUnlessATestDeclaresThem(t *testing.T) {
	// Renamed or deleted declarations return, because base bodies call them. A helper the branch moved into a
	// test file is the test's now: declaring it twice would not compile.
	base := []source{{"a.go", []byte("package p\n\ntype Old struct{}\n\nfunc oldHelper() int { return 1 }\n\nfunc moved() int { return 2 }\n\nfunc Run() int { return oldHelper() + moved() }\n")}}
	head := []source{{"a.go", []byte("package p\n\nfunc Run() int { return NewThing() }\n\nfunc NewThing() int { return 7 }\n")}}
	tests := []source{{"a_test.go", []byte("package p\n\nfunc moved() int { return 20 }\n")}}
	out := rewrite(t, head, base, tests, "a.go")["a.go"]
	contains(t, out, "type Old struct{}", "func oldHelper() int", "return oldHelper() + moved()")
	absent(t, out, "func moved() int", "return NewThing()")
}

func TestFunctionMovedToAnotherFileGetsItsBaseBodyAndIsNotDeclaredTwice(t *testing.T) {
	// The branch moves F from a.go to b.go. Methods and functions are matched across the package's files; the
	// restored a.go loses its F, or the package would declare it twice.
	base := []source{
		{"a.go", []byte("package p\n\nfunc F() int { return 1 }\n\nfunc Keep() int { return 3 }\n")},
		{"b.go", []byte("package p\n\nfunc G() int { return 2 }\n")},
	}
	head := []source{
		{"a.go", []byte("package p\n\nfunc Keep() int { return 4 }\n")},
		{"b.go", []byte("package p\n\nfunc G() int { return 2 }\n\nfunc F() int { return 10 }\n")},
	}
	out := rewrite(t, head, base, nil, "a.go", "b.go")
	contains(t, out["b.go"], "func F() int { return 1 }")
	contains(t, out["a.go"], "return 3")
	absent(t, out["a.go"], "func F()")
}

func TestDeletedFileComesBackWithoutWhatTheHeadDeclaresElsewhere(t *testing.T) {
	base := []source{
		{"old.go", []byte("package p\n\ntype T struct{}\n\nfunc F() int { return 1 }\n\nfunc Gone() int { return 5 }\n\nconst (\n\tA = 1\n\tB = 2\n)\n")},
		{"x.go", []byte("package p\n\nfunc X() int { return F() + Gone() + A + B }\n")},
	}
	head := []source{
		{"new.go", []byte("package p\n\ntype T struct{ v int }\n\nfunc F() int { return 10 }\n\nconst A = 100\n")},
		{"x.go", []byte("package p\n\nfunc X() int { return F() }\n")},
	}
	out := rewrite(t, head, base, nil, "old.go", "new.go")
	contains(t, out["old.go"], "func Gone() int", "B = 2")
	absent(t, out["old.go"], "type T", "func F()", "A = 1")
	absent(t, out["new.go"], "A = 100")
	contains(t, out["new.go"], "type T struct{ v int }", "return 1 }", "const A = 1")
}

func TestNewFileOfTheBranchIsKeptWithZeroBodies(t *testing.T) {
	head := []source{{"feature.go", []byte("package p\n\nfunc Double(n int) int { return 2 * n }\n")}}
	tests := []source{{"feature_test.go", []byte("package p\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) { _ = Double(2) }\n")}}
	out := rewrite(t, head, nil, tests, "feature.go")["feature.go"]
	contains(t, out, "var __zero0 int")
	absent(t, out, "2 * n")
}

func TestEmbedDirectiveOfABaseOnlyVariableComesBack(t *testing.T) {
	base := []source{{"a.go", []byte("package p\n\nimport \"embed\"\n\n//go:embed a.go\nvar old embed.FS\n\nfunc Read() int { _, _ = old.ReadFile(\"a.go\"); return 1 }\n")}}
	head := []source{{"a.go", []byte("package p\n\nfunc Read() int { return 2 }\n")}}
	out := rewrite(t, head, base, nil, "a.go")["a.go"]
	contains(t, out, "//go:embed a.go", "var old embed.FS", `"embed"`)
}

func TestGuessNameKnowsVersionSuffixes(t *testing.T) {
	for in, want := range map[string]string{
		"gopkg.in/yaml.v3":           "yaml",
		"math/rand/v2":               "rand",
		"github.com/x/go-sqlite":     "sqlite",
		"github.com/hellwand/t/util": "util",
	} {
		if got := guessName(in); got != want {
			t.Errorf("guessName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunRewritesTheScratchCopyAndNeverTheCheckout(t *testing.T) {
	// The tool is run against the real directories: the checkout has the head, the copy the base. Only the copy may
	// change, and a file the build tags exclude is left alone.
	head, tree := t.TempDir(), t.TempDir()
	write := func(root, name, content string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(head, "go.mod", "module ex.test/m\n\ngo 1.21\n")
	write(head, "pkg/a.go", "package pkg\n\nfunc F() int { return 2 }\n\nfunc G() int { return 3 }\n")
	write(head, "pkg/tagged.go", "//go:build never\n\npackage pkg\n\nfunc F() int { return 99 }\n")
	write(tree, "pkg/a.go", "package pkg\n\nfunc F() int { return 1 }\n")
	write(tree, "pkg/tagged.go", "//go:build never\n\npackage pkg\n\nfunc F() int { return 98 }\n")
	paths := filepath.Join(t.TempDir(), "prod")
	if err := os.WriteFile(paths, []byte("modified\tpkg/a.go\nmodified\tpkg/tagged.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := run([]string{"-head", head, "-tree", tree, "-paths", paths, "-tags", "integration"}, &sb); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(tree, "pkg/a.go"))
	contains(t, got, "return 1 }", "var __zero0 int")
	tagged, _ := os.ReadFile(filepath.Join(tree, "pkg/tagged.go"))
	if !strings.Contains(string(tagged), "98") {
		t.Errorf("a file excluded by its build tag was rewritten: %s", tagged)
	}
	checkout, _ := os.ReadFile(filepath.Join(head, "pkg/a.go"))
	if !strings.Contains(string(checkout), "return 2") {
		t.Errorf("the checkout was written to: %s", checkout)
	}
	if !strings.Contains(sb.String(), "rewrote 1 file(s) in 1 package(s)") {
		t.Errorf("summary line: %q", sb.String())
	}
}
