package ateenvv1alpha_test

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
)

const protoSHA256 = "e42206e1ce07ce515caae4754de58a29211cbf632cb2316440299f3856f7240d"

func TestGuestProtoIsUpstreamByteForByte(t *testing.T) {
	b, err := os.ReadFile("guest.proto")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != protoSHA256 {
		t.Fatalf("guest.proto sha256 = %s, want %s (agent-substrate/env @281e8608); see UPSTREAM.md", got, protoSHA256)
	}
}

// embeddedDescriptor extracts the serialized FileDescriptorProto a protoc-gen-go output file embeds.
func embeddedDescriptor(t *testing.T, file string) *descriptorpb.FileDescriptorProto {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || !strings.HasSuffix(vs.Names[0].Name, "_rawDesc") {
			return true
		}
		// The literal is a concatenation of quoted strings.
		var b strings.Builder
		ast.Inspect(vs.Values[0], func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				b.WriteString(s)
			}
			return true
		})
		raw = b.String()
		return false
	})
	if raw == "" {
		t.Fatalf("no rawDesc in %s", file)
	}
	fd := new(descriptorpb.FileDescriptorProto)
	if err := proto.Unmarshal([]byte(raw), fd); err != nil {
		t.Fatal(err)
	}
	return fd
}

// normalize drops what may differ without changing the wire: the file's name and Go package.
func normalize(fd *descriptorpb.FileDescriptorProto) *descriptorpb.FileDescriptorProto {
	fd = proto.Clone(fd).(*descriptorpb.FileDescriptorProto)
	fd.Name = nil
	fd.SourceCodeInfo = nil
	if fd.Options != nil {
		fd.Options.GoPackage = nil
	}
	return fd
}

// herd's generated guest.pb.go against env's own generated guest.pb.go. A renamed field, a renumbered field, a changed type or a missing method makes this fail.
func TestGeneratedDescriptorMatchesUpstream(t *testing.T) {
	want := normalize(embeddedDescriptor(t, "testdata/upstream-guest.pb.go.txt"))
	got := normalize(embeddedDescriptor(t, "guest.pb.go"))
	if !proto.Equal(want, got) {
		t.Fatalf("herd's ateenv.v1alpha descriptor differs from agent-substrate/env's:\nupstream: %s\nherd:     %s",
			prototext.Format(want), prototext.Format(got))
	}
	if got.GetPackage() != "ateenv.v1alpha" {
		t.Fatalf("package = %q, want ateenv.v1alpha", got.GetPackage())
	}
}

// The descriptor herd's generated package registers at run time is the one compared above.
func TestRegisteredDescriptorIsTheGeneratedOne(t *testing.T) {
	for _, svc := range []string{"ProcessService", "FileSystemService"} {
		if ateenv.File_ateenv_v1alpha_guest_proto.Services().ByName(protoreflect.Name(svc)) == nil {
			t.Errorf("service ateenv.v1alpha.%s is not registered", svc)
		}
	}
}
