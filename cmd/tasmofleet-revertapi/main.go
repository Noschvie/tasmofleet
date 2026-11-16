// Command tasmofleet-revertapi prepares the second variant of the revert check (#681, tools/revert-check.sh):
// "revert behaviour, keep the API". The first variant puts the production files back to the merge base; tests
// that use API the branch added then do not build, and a build failure proves nothing about behaviour. This
// tool rewrites that scratch copy in place: every changed production file keeps the head's declarations (types,
// fields, signatures, constants) so the tests compile, with the base's bodies and values back and zero-value
// bodies for what the branch added. The rules are in rewrite.go (rewriteDir).
//
// It reads the checkout (-head) and writes only into the scratch copy (-tree), never into the checkout. It
// is built from the checkout, like the scripts of the revert check themselves.
//
//	tasmofleet-revertapi -head <checkout> -tree <scratch copy> -paths <"status<TAB>path" lines> [-tags integration]
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "tasmofleet-revertapi:", err)
		os.Exit(1)
	}
}

type dirWork struct {
	dir     string
	changed map[string]bool
	head    []source
	base    []source
	tests   []source
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("tasmofleet-revertapi", flag.ContinueOnError)
	headDir := fs.String("head", "", "the checkout (the PR's head)")
	treeDir := fs.String("tree", "", "the scratch copy, already put back to the merge base; rewritten in place")
	pathsFile := fs.String("paths", "", "file with one \"status<TAB>path\" line per changed production Go file")
	tags := fs.String("tags", "", "comma separated build tags, as the test run uses them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *headDir == "" || *treeDir == "" || *pathsFile == "" {
		return errors.New("-head, -tree and -paths are required")
	}
	ctx := build.Default
	ctx.BuildTags = nil
	for _, t := range strings.Split(*tags, ",") {
		if t != "" {
			ctx.BuildTags = append(ctx.BuildTags, t)
		}
	}
	paths, err := readPaths(*pathsFile)
	if err != nil {
		return err
	}
	byDir := map[string]*dirWork{}
	for _, p := range paths {
		dir := filepath.Dir(p)
		w := byDir[dir]
		if w == nil {
			w = &dirWork{dir: dir, changed: map[string]bool{}}
			byDir[dir] = w
		}
		w.changed[filepath.Base(p)] = true
	}
	var dirs []string
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	imports := map[string]bool{}
	for _, d := range dirs {
		w := byDir[d]
		if w.head, w.tests, err = readGoFiles(ctx, filepath.Join(*headDir, d)); err != nil {
			return err
		}
		if w.base, _, err = readGoFiles(ctx, filepath.Join(*treeDir, d)); err != nil {
			return err
		}
		for _, s := range append(append([]source{}, w.head...), w.base...) {
			collectImports(s, imports)
		}
	}
	names := packageNames(*headDir, *tags, imports)
	pkgName := func(path string) string {
		if n := names[path]; n != "" {
			return n
		}
		return guessName(path)
	}
	files := 0
	for _, d := range dirs {
		w := byDir[d]
		out, err := rewriteDir(w.head, w.base, w.tests, w.changed, pkgName)
		if err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
		for name, content := range out {
			target := filepath.Join(*treeDir, d, name)
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(target, content, 0o600); err != nil {
				return err
			}
			files++
		}
	}
	_, err = fmt.Fprintf(stdout, "revert check: rewrote %d file(s) in %d package(s): behaviour reverted, API kept\n", files, len(dirs))
	return err
}

func readPaths(file string) ([]string, error) {
	f, err := os.Open(filepath.Clean(file))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: nothing to lose on a failed close
	var paths []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if _, p, ok := strings.Cut(sc.Text(), "\t"); ok && strings.HasSuffix(p, ".go") {
			paths = append(paths, p)
		}
	}
	return paths, sc.Err()
}

// readGoFiles returns the non-test files of a directory that the build with these tags includes, and the
// test files of the package itself. A missing directory has none.
func readGoFiles(ctx build.Context, dir string) (prod, tests []source, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if ok, err := ctx.MatchFile(dir, name); err != nil || !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file of the checkout or of the scratch copy
		if err != nil {
			return nil, nil, err
		}
		if strings.HasSuffix(name, "_test.go") {
			tests = append(tests, source{name, b})
		} else {
			prod = append(prod, source{name, b})
		}
	}
	return prod, tests, nil
}

func collectImports(s source, into map[string]bool) {
	f, err := parser.ParseFile(token.NewFileSet(), s.name, s.src, parser.ImportsOnly)
	if err != nil {
		return // rewriteDir reports the parse error with the file name
	}
	for _, is := range f.Imports {
		if p, err := strconv.Unquote(is.Path.Value); err == nil && p != "C" {
			into[p] = true
		}
	}
}

// packageNames asks the go tool, once, for the package names of every import path (a path's last element is
// not always the name: gopkg.in/yaml.v3, math/rand/v2). A path it cannot resolve is left out; guessName
// covers it.
func packageNames(dir, tags string, imports map[string]bool) map[string]string {
	names := map[string]string{}
	if len(imports) == 0 {
		return names
	}
	var paths []string
	for p := range imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"list", "-e", "-f", "{{.ImportPath}}\t{{.Name}}"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	cmd := exec.CommandContext(ctx, "go", append(args, paths...)...) //nolint:gosec // fixed command, import paths of the project's own files
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run() // -e lists what it can; an unresolved path falls back to guessName
	for _, line := range strings.Split(out.String(), "\n") {
		if p, n, ok := strings.Cut(line, "\t"); ok && n != "" {
			names[p] = n
		}
	}
	return names
}

// guessName is the package name an import path most likely has.
func guessName(path string) string {
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && len(last) > 1 && last[0] == 'v' {
		if _, err := strconv.Atoi(last[1:]); err == nil {
			last = parts[len(parts)-2]
		}
	}
	if i := strings.Index(last, "."); i > 0 {
		last = last[:i]
	}
	last = strings.TrimPrefix(last, "go-")
	return strings.ReplaceAll(last, "-", "_")
}
