package substrateapi

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// allowed is everything the suite may import besides the standard library: gRPC, protobuf, Substrate's Control API
// (herd's byte-for-byte pinned subset) and agent-substrate/env's guest API (the env-exec probe).
func allowed(path string) bool {
	if !strings.Contains(strings.Split(path, "/")[0], ".") {
		return true // standard library
	}
	return path == "google.golang.org/grpc" || strings.HasPrefix(path, "google.golang.org/grpc/") ||
		strings.HasPrefix(path, "google.golang.org/protobuf/") ||
		path == "github.com/everdict/herd/api/ateapi/v1" || path == "github.com/everdict/herd/api/ateenv/v1alpha"
}

// herdOnly are names only herd answers to: its extension service, its owed-operation trailer, its router reasons.
var herdOnly = []string{"herd.ext", "herd-owed", "herd-reason", "/herd."}

// TestSuiteAssumesNothingHerdSpecific (client-suite): the suite's code imports no herd package beyond the two
// pinned API definitions (no apiserver, store, link, router, herdext ...) and names no herd-only method or header,
// so a case cannot pass by reading herd's extension or store.
func TestSuiteAssumesNothingHerdSpecific(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !allowed(p) {
				t.Errorf("%s imports %s: the suite may use only the pinned Control and env APIs", fset.Position(imp.Pos()), p)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, h := range herdOnly {
				if strings.Contains(lit.Value, h) {
					t.Errorf("%s: %s names %q, which only herd answers", fset.Position(lit.Pos()), lit.Value, h)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}

// TestSkipOnlyWithTheServersReason: an Unimplemented answer with a reason is a declared skip and keeps the report
// green; Unimplemented with no reason, any other code, or a skip with an empty reason fail it.
func TestSkipOnlyWithTheServersReason(t *testing.T) {
	var d declared
	if err := gate("CreateTag", status.Error(codes.Unimplemented, "tags are not served by this target")); !errors.As(err, &d) ||
		!strings.Contains(d.Error(), "tags are not served") {
		t.Errorf("Unimplemented with a reason: %v, want a declared skip carrying it", err)
	}
	for _, err := range []error{status.Error(codes.Unimplemented, ""), status.Error(codes.FailedPrecondition, "no"), errors.New("io")} {
		if g := gate("CreateTag", err); g == nil || errors.As(g, &d) {
			t.Errorf("gate(%v) = %v, want a failure", err, g)
		}
	}
	ok := Report{Results: []Result{{Case: "a", Outcome: Pass}, {Case: "b", Outcome: Skip, Detail: "server declared: x"}}}
	if !ok.OK() {
		t.Error("a declared skip failed the report")
	}
	bad := Report{Results: []Result{{Case: "a", Outcome: Pass}, {Case: "b", Outcome: Skip, Detail: " "}}}
	if bad.OK() {
		t.Error("a skip without a reason kept the report green")
	}
}
