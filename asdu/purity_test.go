package asdu

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is the complete list of packages the production code of this
// package may import.
//
// The asdu package is pure: no network, no Redis, no gRPC, no logging,
// no metrics, no context. golangci-lint's depguard enforces that as a blacklist,
// which catches the packages somebody thought to name. This test is the
// whitelist, which catches the ones nobody thought of — and it fails on the line
// that added the import rather than on a code review that might not happen.
//
// Adding an entry here is allowed. Doing it silently is not: an import that
// belongs in this list comes with a reason written next to it, and anything that
// reaches outside the process does not belong here at all.
var allowedImports = []string{
	"encoding/binary", // little-endian multi-octet fields, which every one of them is
	"errors",          // sentinel errors: malformed closes the link, unsupported does not
	"fmt",             // error messages that name the offending octet
	"math",            // Float32frombits for IEEE 754 short float values
	"strconv",         // printing an unknown type or cause by number
	"strings",         // joining quality flag names for String()
	"time",            // CP56Time2a <-> time.Time; no clock, no Now
}

// TestPackageStaysPure reads this package's own source and fails if it imports
// anything outside allowedImports.
//
// Purity is not a style preference here. It is what makes byte-level golden
// tests possible, and byte-level golden tests are the only trustworthy proof
// that a protocol implementation is right: a layer that reaches for a socket
// cannot be replayed against a capture.
func TestPackageStaysPure(t *testing.T) {
	t.Parallel()

	seen := make(map[string][]string) // import path -> files that use it

	for name, file := range productionFiles(t, parser.ImportsOnly) {
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unparsable import %s", name, spec.Path.Value)
			}
			seen[path] = append(seen[path], name)
		}
	}

	for path, files := range seen {
		if !slices.Contains(allowedImports, path) {
			slices.Sort(files)
			t.Errorf("%s imports %q (in %s).\n"+
				"asdu is a pure package: it turns bytes into values and back, "+
				"and nothing else. If this import genuinely belongs, add it to allowedImports "+
				"with a reason; if it reaches outside the process, it belongs in conn.",
				"asdu", path, strings.Join(slices.Compact(files), ", "))
		}
	}
}

// The whitelist must not rot in the other direction either: an entry left behind
// after the code that needed it was removed makes the list a description of the
// past rather than a constraint on the present.
func TestAllowedImportsAreAllUsed(t *testing.T) {
	t.Parallel()

	used := make(map[string]bool)
	for _, file := range productionFiles(t, parser.ImportsOnly) {
		for _, spec := range file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil {
				used[path] = true
			}
		}
	}

	for _, allowed := range allowedImports {
		if !used[allowed] {
			t.Errorf("allowedImports lists %q but nothing imports it any more; remove the entry", allowed)
		}
	}
}

// productionFiles parses this package's non-test sources.
//
// It lists the directory itself instead of calling parser.ParseDir. Two reasons:
// ParseDir is deprecated as of Go 1.25, and its filter takes an os.FileInfo —
// which would mean importing os, and this package may not. filepath.Glob plus
// parser.ParseFile needs neither.
//
// Unlike ParseDir this ignores build tags, which here is stricter rather than
// looser: an import hidden behind a tag is still an import, and still has to be
// on the list.
func productionFiles(t *testing.T, mode parser.Mode) map[string]*ast.File {
	t.Helper()

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package: %v", err)
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, path := range paths {
		// Production code only. Tests may import whatever they need — this file
		// itself imports go/parser.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, mode)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files[filepath.Base(path)] = f
	}
	if len(files) == 0 {
		t.Fatal("no production sources found; the test is not looking where it thinks it is")
	}
	return files
}
