package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// #839: the product owner's transport rule (2026-11-14) says HTTP carries an operation only when MQTT is not set up or
// not usable for the device, or when the operation technically cannot be done over MQTT, and every case of the second
// kind is proven and listed in docs/security/mqtt-transport.md ("What still uses HTTP"). A read goes over MQTT first
// only when it goes through the router without an HTTP pin. These are the ways around it: the raw HTTP client
// (Router.HTTPOnly, Services.HTTP, a client of its own), an HTTP pin, the prober's own client (the raw HTTP one) and the
// health tracker's (the same). Each place that uses one is named here with the case of the list it belongs to; a new
// place fails this test until it is either moved to the router or proven and listed. A place that no longer exists
// fails it too, so the list stays the code's.

// httpWays are the patterns of a call that reaches a device over HTTP whatever the router would choose. scope limits a
// pattern to the files under a directory (the receiver names are only meaningful there).
var httpWays = []struct {
	name, scope string
	re          *regexp.Regexp
}{
	{"raw client of the router", "", regexp.MustCompile(`\.HTTPOnly\(\)`)},
	{"raw client of the services", "", regexp.MustCompile(`\bsvc\.HTTP\b`)},
	{"HTTP client of its own", "", regexp.MustCompile(`\btasmota\.NewHTTPClient\(`)},
	{"pinned to HTTP", "", regexp.MustCompile(`\btransport\.WithPath\([^;]*\btransport\.HTTP\)`)},
	{"the prober's HTTP client", "internal/app", regexp.MustCompile(`\bp\.(Client\.|cmd\(|status5\(|status11\(|readMQTT\(|mqttSwitchedOff\(|VerifyAddress\(|probe\()`)},
	{"the tracker's HTTP client", "internal/health", regexp.MustCompile(`\bs\.d\.Client\.`)},
}

// httpReadsAllowed maps every place (file, enclosing function, way) to the words of the case in "What still uses HTTP"
// that it belongs to.
var httpReadsAllowed = map[string]string{
	// The health poll and probe measure HTTP (the page's and the console's probe and the cut-off proof ask them too).
	"internal/app/app.go runtime.healthDeps: raw client of the services":  "the health poll",
	"internal/health/service.go Service.probe: the tracker's HTTP client": "the health poll",
	// The identity probe at the address (a device's first probe, and Refresh identity, the background identity refresh,
	// discovery's follow-up and the firmware recheck of a device MQTT cannot serve, #682) and its helpers. The helpers read with the client of the prober they are called
	// on: the background reads (ReadNet, ReadBroker, the checks of VerifyRead and verifyAndPin) copy the prober with the
	// router as its client.
	"internal/app/services.go openServices: raw client of the router":        "The identity probe",
	"internal/app/services.go openServices: HTTP client of its own":          "The identity probe",
	"internal/app/probe.go Prober.probeAddress: the prober's HTTP client":    "The identity probe",
	"internal/app/probe.go Prober.probeFacts: the prober's HTTP client":      "The identity probe",
	"internal/app/probe.go Prober.refresh: the prober's HTTP client":         "The identity probe",
	"internal/app/probe.go Prober.Describe: the prober's HTTP client":        "The identity probe",
	"internal/app/probe.go Prober.cmd: the prober's HTTP client":             "The identity probe",
	"internal/app/probe.go Prober.readMQTT: the prober's HTTP client":        "The identity probe",
	"internal/app/probe.go Prober.mqttSwitchedOff: the prober's HTTP client": "The identity probe",
	"internal/app/probe.go Prober.status5: the prober's HTTP client":         "The identity probe",
	"internal/app/probe.go Prober.status11: the prober's HTTP client":        "The identity probe",
	"internal/app/probe.go Prober.VerifyAddress: the prober's HTTP client":   "The identity probe",
	// VerifyRead of a prober without a router (the CLI): its reads went to the address, so its check does.
	"internal/app/probe.go Prober.VerifyRead: the prober's HTTP client": "without a router",
	// The discovery scan and the MAC check of an address: no device, no topic.
	"internal/app/discovery.go runtime.setupDiscovery: HTTP client of its own":     "The discovery scan",
	"internal/app/discovery.go runtime.setupDiscovery: raw client of the services": "The discovery scan",
	// A check vouches for the transport its read used (#810): reads pinned to where their check was answered.
	"internal/app/probe.go pinTo: pinned to HTTP": "where their check was answered",
	// The status snapshot of a backup follows the transport of its dump (T14).
	"internal/backup/service.go onDumpPath: pinned to HTTP": "follows the transport of its dump",
	// The files of a backup or restore, and the MAC check before them (T15).
	"internal/backup/files.go Manager.sameDeviceOverHTTP: pinned to HTTP": "Files of a backup or restore",
	"internal/restore/flow.go Manager.run: pinned to HTTP":                "a restore keeps the transport it chose",
	"internal/restore/flow.go Manager.filesRoute: pinned to HTTP":         "Files of a backup or restore",
	// The HTTP proof of a restore that changes the MQTT connection, and its boot check after that proof (T16).
	"internal/restore/flow.go Manager.httpReaches: pinned to HTTP": "proof that HTTP answers",
	"internal/restore/flow.go Manager.send: pinned to HTTP":        "proof that HTTP answers",
	// A power switch MQTT published nothing for goes over HTTP once (T2); a write, listed for completeness.
	"internal/devicecmd/devicecmd.go service.Power: pinned to HTTP": "the power switch",
	// An update job keeps the path it chose (T5); updates are not moved yet.
	"internal/ota/mqttpath.go pinned: pinned to HTTP": "the firmware update",
}

func TestEveryWayAroundTheRoutersReadRuleIsOnTheProvenList(t *testing.T) {
	root := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "docs", "security", "mqtt-transport.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, list, ok := strings.Cut(string(doc), "## What still uses HTTP")
	if !ok {
		t.Fatal(`docs/security/mqtt-transport.md has no "What still uses HTTP" section`)
	}
	list, _, _ = strings.Cut(list, "\n## ")

	found := map[string]bool{}
	err = filepath.WalkDir(filepath.Join(root, "internal"), func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		switch {
		case e.IsDir() && (rel == "internal/simulator" || rel == "internal/testutil" || rel == "internal/transport"):
			return filepath.SkipDir // the device simulator, test helpers, and the router itself (the rule, tested there)
		case e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		for _, k := range httpPlaces(t, path, rel) {
			found[k] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		words, ok := httpReadsAllowed[k]
		switch {
		case !ok:
			t.Errorf("%s reaches the device over HTTP past the router's rule: move it to the router (MQTT first, #839) "+
				"or prove that MQTT cannot do it and list it in docs/security/mqtt-transport.md and here", k)
		case !strings.Contains(list, words):
			t.Errorf("%s: %q is not in the \"What still uses HTTP\" section", k, words)
		}
	}
	for k := range httpReadsAllowed {
		if !found[k] {
			t.Errorf("%s is listed but no longer exists: remove it from the list", k)
		}
	}
}

// httpPlaces lists the places in one file that match httpWays, as "file func: way".
func httpPlaces(t *testing.T, path, rel string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code, _, _ := strings.Cut(line, " // ")
		for _, w := range httpWays {
			if w.scope != "" && !strings.HasPrefix(rel, w.scope+"/") || !w.re.MatchString(code) {
				continue
			}
			out = append(out, rel+" "+enclosing(fset, f, i+1)+": "+w.name)
		}
	}
	return out
}

// enclosing names the function declared around line ("Recv.Name" for a method, "" outside every function).
func enclosing(fset *token.FileSet, f *ast.File, line int) string {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fset.Position(fn.Pos()).Line > line || fset.Position(fn.End()).Line < line {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return fn.Name.Name
		}
		typ := fn.Recv.List[0].Type
		if s, ok := typ.(*ast.StarExpr); ok {
			typ = s.X
		}
		if id, ok := typ.(*ast.Ident); ok {
			return id.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
	return ""
}
