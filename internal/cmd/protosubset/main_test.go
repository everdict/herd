package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const source = "github.com/agent-substrate/substrate api/ateapi/v1/ateapi.proto at commit 6d7b3f49f6154d52a1de46c94a1481868a5ed042 (Apache License, Version 2.0)"

func upstream(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../api/ateapi/v1/testdata/upstream-ateapi.proto")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The vendored subset is exactly what the pruner makes of the pinned upstream file: nobody edited it by hand.
func TestVendoredSubsetIsRegenerable(t *testing.T) {
	list, err := os.ReadFile("../../../api/ateapi/v1/subset.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Subset(upstream(t), ParseMethods(list), additions(t), source, "github.com/everdict/herd/api/ateapi/v1;ateapi")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../../api/ateapi/v1/ateapi.proto")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("api/ateapi/v1/ateapi.proto differs from the subset of the pinned upstream; run go generate ./api/...")
	}
}

func TestSubsetDropsWhatIsNotReached(t *testing.T) {
	got, err := Subset(upstream(t), []string{"GetTag"}, nil, source, "x;x")
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, w := range []string{"rpc GetTag(", "message Tag {", "message GetTagRequest {", "message MethodAnnotation {", "extend google.protobuf.MethodOptions {", `option go_package = "x;x";`, "Copyright 2026 Google LLC"} {
		if !strings.Contains(s, w) {
			t.Errorf("subset lacks %q", w)
		}
	}
	for _, w := range []string{"rpc CreateActor(", "message WorkerStatus {", "service WorkerService", "message AccessPolicy {", "go.ate.dev"} {
		if strings.Contains(s, w) {
			t.Errorf("subset keeps %q, which GetTag does not reach", w)
		}
	}
}

func TestSubsetRefusesUnknownMethod(t *testing.T) {
	if _, err := Subset(upstream(t), []string{"GetTag", "LaunchRocket"}, nil, source, "x;x"); err == nil || !strings.Contains(err.Error(), "LaunchRocket") {
		t.Errorf("Subset with a method upstream lacks = %v, want an error naming it", err)
	}
}

func additions(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../api/ateapi/v1/herd-additions.txt")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// herd's own fields land inside the upstream message they extend, and nowhere else; one for a message the subset
// does not reach is refused.
func TestSubsetAppendsHerdFields(t *testing.T) {
	add := []byte("// outside any block\nmessage GetTagRequest {\n  // herd addition: x.\n  string x = 1000;\n}\n")
	got, err := Subset(upstream(t), []string{"GetTag"}, add, source, "x;x")
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	i := strings.Index(s, "message GetTagRequest {")
	if i < 0 {
		t.Fatal("no GetTagRequest")
	}
	msg := s[i : i+strings.Index(s[i:], "\n}\n")]
	if !strings.Contains(msg, "\n  string x = 1000;") || strings.Contains(s, "outside any block") || !strings.Contains(s, "herd-additions.txt") {
		t.Errorf("the addition is not inside GetTagRequest, or the notice does not say so:\n%s", s)
	}
	if _, err := Subset(upstream(t), []string{"GetTag"}, []byte("message WorkerStatus {\n  string x = 1000;\n}\n"), source, "x;x"); err == nil || !strings.Contains(err.Error(), "WorkerStatus") {
		t.Errorf("an addition to a message outside the subset = %v, want an error naming it", err)
	}
}
