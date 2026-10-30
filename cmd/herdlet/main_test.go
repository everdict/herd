package main

import "testing"

func TestParseFlags(t *testing.T) {
	l, err := parseLabels("host=mac,os=darwin")
	if err != nil || l["host"] != "mac" || l["os"] != "darwin" {
		t.Fatalf("parseLabels = %v, %v", l, err)
	}
	if _, err := parseLabels("nokey"); err == nil {
		t.Error("parseLabels accepted a label without =")
	}
	w, err := parseWarm("tpl-a=2,tpl-b=0")
	if err != nil || len(w) != 2 || w[0].UID != "tpl-a" || w[0].Warm != 2 || len(w[0].Containers) != 0 {
		t.Fatalf("parseWarm = %+v, %v", w, err)
	}
	for _, bad := range []string{"tpl", "tpl=-1", "=2", "tpl=x"} {
		if _, err := parseWarm(bad); err == nil {
			t.Errorf("parseWarm(%q) accepted", bad)
		}
	}
}
