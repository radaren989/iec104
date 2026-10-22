package conn

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

// This file enforces architecture rules whose violation is silent: a Redis call
// added to readLoop does not fail a unit test, it makes connections drop under
// load six months later, in the field, at the exact moment the operator needs them.
//
// Everything here reads this package's own source with go/ast. That is on purpose:
// a runtime test can only observe the behaviour a test happens to provoke, while
// the source says what the code is allowed to do at all.

// allowedImports is the complete list of packages the production code of
// internal/conn may import.
//
// This package is where the pure protocol layer meets the network, so unlike apci
// and asdu it is allowed a socket. It is not allowed anything past the socket:
// Redis, gRPC, a database, an HTTP client and a metrics registry all belong on the
// far side of the Handler interface, where a slow call delays the owner of the
// handler instead of the loop that has to answer t1.
//
// Adding an entry is allowed. Doing it silently is not: every entry carries the
// reason it is here.
var allowedImports = []string{
	"context", // callers' cancellation on SendASDU/SendU
	"errors",  // sentinel errors the station layer reacts to differently
	"fmt",     // wrapping errors with the context §2.1 requires
	"net",     // net.Conn: the socket itself, which is this package's whole job
	"sync",    // WaitGroup and Once for the goroutine ownership rules of §3.3
	"time",    // Duration for the write deadline

	"github.com/radaren989/iec104/apci",  // the protocol this drives
	"github.com/radaren989/iec104/clock", // §2.3: no direct time.Now
}

// forbiddenImportMarkers name the dependencies §3.2 exists to keep out, by
// substring, so that a new client library for the same thing is caught too.
//
// allowedImports already rejects every one of these — this list is here so that
// the failure says WHY rather than just "not on the whitelist", because the reason
// is the whole point: a Redis round trip inside this package's loops turns a cache
// slowdown into a lost field connection.
var forbiddenImportMarkers = []string{
	"redis",
	"grpc",
	"protobuf",
	"prometheus",
	"database/sql",
	"net/http",
	"log/slog",
}

// TestPackageImportsStayInsideTheArchitecture is the coarse form of the rule: if
// this package cannot import Redis or gRPC at all, no function in it can call
// them, readLoop included.
func TestPackageImportsStayInsideTheArchitecture(t *testing.T) {
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
		slices.Sort(files)
		where := strings.Join(slices.Compact(files), ", ")

		for _, marker := range forbiddenImportMarkers {
			if strings.Contains(path, marker) {
 			t.Errorf("conn imports %q (in %s).\n"+
 				"This package's loops must never block on anything "+
 				"outside the process. A %s call from here delays the goroutine that "+
 				"answers t1 and t2, so a slow %s becomes a dropped field connection. "+
 				"Put it behind the Handler interface instead.",
					path, where, marker, marker)
			}
		}

		if !slices.Contains(allowedImports, path) {
			t.Errorf("conn imports %q (in %s).\n"+
				"If this genuinely belongs in the connection layer, add it to "+
				"allowedImports with a reason. If it reaches outside the process, it "+
				"belongs behind Handler.",
				path, where)
		}
	}
}

// TestAllowedImportsAreAllUsed keeps the whitelist from rotting into a description
// of the past rather than a constraint on the present.
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

// readLoopCalls is everything readLoop is allowed to call.
//
// This is the whitelist form of "readLoop does nothing but move bytes".
// It is deliberately absolute rather than a ban on specific
// dangerous names: the calls that will hurt us are the ones nobody has written
// yet, and a blacklist can only name the ones somebody already thought of.
//
// Every entry is either pure parsing, a channel/buffer primitive, or failing the
// connection. Nothing in it can block on anything but the socket and the internal
// channels.
var readLoopCalls = []string{
	"append", // growing the reassembly buffer
	"len",    // bounds checks before slicing
	"make",   // the two fixed buffers, once, at the top

	"apci.Decode",       // pure: bytes -> Frame
	"apci.NextFrameLen", // pure: how much of the buffer is one frame
	"errors.Is",         // distinguishing "incomplete" from "malformed"
	"fmt.Errorf",        // wrapping the reason the connection is ending

	"c.fail",      // the only way this loop reports anything but a frame
	"c.sock.Read", // the blocking syscall this loop exists to own
	"c.wg.Done",   // goroutine ownership
}

// writeLoopCalls is everything writeLoop is allowed to call.
//
// writeLoop is the single writer. It owns no protocol state and
// makes no decisions, so its call list is even shorter: set a deadline, write,
// report failure.
var writeLoopCalls = []string{
	"c.deadline",              // the wall-clock instant the write must finish by
	"c.fail",                  // reporting a dead socket
	"c.sock.SetWriteDeadline", // §3.1: bounded writes, so a stuck peer fails fast
	"c.sock.Write",            // the one place bytes reach the wire
	"c.wg.Done",               // goroutine ownership
	"fmt.Errorf",              // wrapping the reason
}

// TestReadLoopDoesNothingButMoveBytes is the architecture test: it
// proves, from the source, that readLoop makes no Redis or gRPC call — and more
// than that, that it makes no call at all outside the short list above.
//
// The failure mode this guards is not a crash: a Redis round
// trip inside readLoop stalls the goroutine feeding runLoop, t1 and t2 expire with
// frames unacknowledged, and the connection drops. A cache hiccup in the data
// centre becomes a lost substation. Nothing about that is visible in a unit test
// of the happy path, which is why it is checked here instead.
func TestReadLoopDoesNothingButMoveBytes(t *testing.T) {
	t.Parallel()
	assertCallsWithin(t, "readLoop", readLoopCalls,
		"readLoop reads the socket, frames the bytes, and hands them "+
			"to runLoop. Nothing else. If this work must happen somewhere, it happens "+
			"behind the Handler interface, where blocking costs a delivery instead of "+
			"the connection.")
}

// TestWriteLoopDoesNothingButWriteBytes is the same argument for the other end.
//
// A writeLoop that decided anything would need to read the sequencer, which
// runLoop owns — and then N(S) would be assigned in two goroutines, which is
// exactly the silent race to avoid.
func TestWriteLoopDoesNothingButWriteBytes(t *testing.T) {
	t.Parallel()
	assertCallsWithin(t, "writeLoop", writeLoopCalls,
		"writeLoop is the single writer and makes no decisions. "+
			"Anything it would have to decide needs the sequencer, which runLoop owns.")
}

// handlerCallers are the functions allowed to call into the Handler.
//
// The handler is the seam where this package's guarantees stop: whatever is on the
// other side may talk to Redis or gRPC, and §3.2 is about keeping that away from
// the loop that answers timers. So the interesting question is not "does readLoop
// import Redis" — it is "can readLoop reach code that might".
//
//   - handleFrame runs on runLoop, which is allowed to block on the handler
//     because the handler is contractually prompt (see the Handler doc comment).
//   - New calls OnClosed from its own fourth goroutine, after all three loops
//     have stopped, so nothing is left to stall.
var handlerCallers = []string{"handleFrame", "New"}

// TestHandlerIsReachedOnlyFromRunLoopAndClose closes the gap the import check
// leaves open. Redis and gRPC cannot be imported here, so the only way a socket
// read could end up waiting on one is through the Handler — and this is what says
// where the Handler may be called from.
func TestHandlerIsReachedOnlyFromRunLoopAndClose(t *testing.T) {
	t.Parallel()

	for name, file := range productionFiles(t, 0) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if slices.Contains(handlerCallers, fn.Name.Name) {
				continue
			}

			for _, call := range calleesOf(fn) {
				if !strings.Contains(call, ".Handler.") {
					continue
				}
				t.Errorf("%s: %s calls %s.\n"+
					"Only %s may reach the Handler. A handler call is the one place this "+
					"package can block on somebody else's Redis or gRPC, so it belongs on "+
					"runLoop — never on readLoop or writeLoop.",
					name, fn.Name.Name, call, strings.Join(handlerCallers, " and "))
			}
		}
	}
}

// TestConnectionStartsExactlyTheDocumentedGoroutines pins the goroutine count.
//
// A design that adds a goroutine must be justified in writing before
// it is written. That is a rule about review, and review is exactly what gets
// skipped at 18:00 on a Friday — so the count is checked here as well.
//
// Four `go` statements, and the doc comment on New explains the fourth: the three
// loops cannot report their own ending, because whichever one called OnClosed
// would still be running while the handler was told everything had stopped.
func TestConnectionStartsExactlyTheDocumentedGoroutines(t *testing.T) {
	t.Parallel()

	const want = 4

	got := 0
	for name, file := range productionFiles(t, 0) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			n := 0
			ast.Inspect(fn, func(node ast.Node) bool {
				if _, ok := node.(*ast.GoStmt); ok {
					n++
				}
				return true
			})
			if n > 0 && fn.Name.Name != "New" {
				t.Errorf("%s: %s starts %d goroutine(s); only New may",
					name, fn.Name.Name, n)
			}
			got += n
		}
	}

	if got != want {
		t.Errorf("the package starts %d goroutines, want %d.\n"+
			"A connection is capped at three loops, plus the one New owns to "+
			"report the ending. Raising the count needs a written justification "+
			"and in the package doc comment, in the same commit as the goroutine.",
			got, want)
	}
}

// --- helpers ----------------------------------------------------------------

// productionFiles parses this package's non-test sources.
//
// It lists the directory itself instead of calling parser.ParseDir, which is
// deprecated as of Go 1.25 and whose filter takes an os.FileInfo. filepath.Glob
// plus parser.ParseFile needs neither, and unlike ParseDir it ignores build
// tags — which here is stricter rather than looser: a call hidden behind a tag
// is still a call, and still has to be on the list.
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
		// itself imports go/ast.
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

// assertCallsWithin fails if the named function calls anything outside allowed,
// and also if allowed lists something the function no longer calls.
func assertCallsWithin(t *testing.T, fnName string, allowed []string, why string) {
	t.Helper()

	var (
		found bool
		used  = make(map[string]bool)
	)

	for name, file := range productionFiles(t, 0) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != fnName || fn.Body == nil {
				continue
			}
			found = true

			for _, call := range calleesOf(fn) {
				used[call] = true
				if !slices.Contains(allowed, call) {
					t.Errorf("%s: %s calls %s, which is not on its allowed list.\n%s\n"+
						"If the call is genuinely harmless, add it to the list with a "+
						"reason — but read the reason out loud first.",
						name, fnName, call, why)
				}
			}
		}
	}

	if !found {
		t.Fatalf("no function named %s found; this test is guarding something that no longer exists", fnName)
	}

	for _, call := range allowed {
		if !used[call] {
			t.Errorf("%s's allowed list has %q but %s no longer calls it; remove the entry",
				fnName, call, fnName)
		}
	}
}

// calleesOf returns the dotted name of every call made inside fn, including the
// ones inside function literals it contains.
func calleesOf(fn *ast.FuncDecl) []string {
	var calls []string

	ast.Inspect(fn, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := calleeName(call.Fun); name != "" {
			calls = append(calls, name)
		}
		return true
	})

	slices.Sort(calls)
	return slices.Compact(calls)
}

// calleeName renders a call's target the way it is written in the source:
// "apci.Decode", "c.sock.Read", "len". It returns "" for anything it cannot name
// that way — a call through a function value, say — which is itself worth knowing,
// and is why the loops above are written without any.
func calleeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.ParenExpr:
		return calleeName(v.X)
	case *ast.SelectorExpr:
		if base := calleeName(v.X); base != "" {
			return base + "." + v.Sel.Name
		}
		return v.Sel.Name
	default:
		return ""
	}
}
