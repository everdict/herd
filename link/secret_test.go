package link

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A Work's PullAuth crosses the link whole and prints redacted (PLT-330).
func TestPullAuthPrintsRedacted(t *testing.T) {
	const secret = `{"auths": {"ghcr.io": {"auth": "c2VjcmV0"}}}`
	w := Work{Op: OpBoot, VM: "vm", PullAuth: secret}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(f, w); strings.Contains(s, "c2VjcmV0") {
			t.Errorf("%s of a Work prints its PullAuth: %s", f, s)
		}
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var back Work
	if err := json.Unmarshal(b, &back); err != nil || back.PullAuth != secret {
		t.Errorf("PullAuth over the link = %q (%v), want it whole", back.PullAuth, err)
	}
}
