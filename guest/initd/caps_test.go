package initd_test

import (
	"strings"
	"testing"

	"github.com/everdict/herd/guest/initd"
)

// A plan's container capabilities are checked as the template's were: a name Linux does not have, or ALL in add, is
// refused before any container starts; known names in either spelling are accepted.
func TestPlanRefusesAnUnknownCapability(t *testing.T) {
	const plan = `{"containers": [{"name": "box", "image": {"device": "/dev/vdb", "fsType": "erofs"}, "command": ["sleep"], %s}]}`
	for _, tc := range []struct{ fields, bad string }{
		{`"capDrop": ["NET_RAWR"]`, "NET_RAWR"},
		{`"capAdd": ["ALL"]`, "ALL"},
		{`"capAdd": ["SYS_PTRACE", "CAP_WARP"]`, "CAP_WARP"},
	} {
		_, err := initd.ParsePlan([]byte(strings.Replace(plan, "%s", tc.fields, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.bad) || !strings.Contains(err.Error(), "box") {
			t.Errorf("plan with %s: %v, want a refusal naming container box and %s", tc.fields, err, tc.bad)
		}
	}
	p, err := initd.ParsePlan([]byte(strings.Replace(plan, "%s", `"capAdd": ["CAP_SYS_PTRACE"], "capDrop": ["NET_RAW", "ALL"]`, 1)))
	if err != nil {
		t.Fatalf("plan with known capabilities: %v", err)
	}
	if c := p.Containers[0]; len(c.CapAdd) != 1 || len(c.CapDrop) != 2 {
		t.Errorf("plan container carries add %q drop %q", c.CapAdd, c.CapDrop)
	}
}
