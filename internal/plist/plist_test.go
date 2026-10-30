package plist_test

import (
	"reflect"
	"testing"

	"github.com/everdict/herd/internal/plist"
)

func TestRoundTrip(t *testing.T) {
	in := map[string]any{"Label": "a<b&c", "On": true, "Off": false, "N": 3, "Args": []string{"x", "y z"}, "Empty": map[string]any{}, "None": []any{}}
	b, err := plist.Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plist.Decode(b)
	if err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	want := map[string]any{"Label": "a<b&c", "On": true, "Off": false, "N": int64(3), "Args": []any{"x", "y z"}, "Empty": map[string]any{}, "None": []any{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip: %#v, want %#v", got, want)
	}
}

func TestDecodeTypes(t *testing.T) {
	v, err := plist.Decode([]byte(`<?xml version="1.0"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "x"><plist version="1.0"><array>
<integer>0xffffffffffffffff</integer><real>1.5</real><data>aGVy
ZA==</data><date>2026-10-30T00:00:00Z</date><string></string></array></plist>`))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(-1), 1.5, []byte("herd"), "2026-10-30T00:00:00Z", ""}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("%#v, want %#v", v, want)
	}
	for name, bad := range map[string]string{
		"not xml":     "IOConsoleUsers = ()",
		"no plist":    `<dict></dict>`,
		"empty plist": `<plist></plist>`,
		"key no val":  `<plist><dict><key>a</key></dict></plist>`,
		"bad int":     `<plist><integer>x</integer></plist>`,
		"unknown":     `<plist><set/></plist>`,
	} {
		if _, err := plist.Decode([]byte(bad)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := plist.Encode(map[string]any{"f": 1.5}); err == nil {
		t.Errorf("encoded a float the writer does not support")
	}
}
