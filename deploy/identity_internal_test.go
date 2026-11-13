package deploy

import (
	"testing"

	"github.com/everdict/herd/auth/localca"
)

// testEgressIdentity is a herd-egress identity of a CA of the test's: herd-egress does not start without one.
func testEgressIdentity(t *testing.T) Identity {
	t.Helper()
	ca, err := localca.New("herd.test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ca.Source("spiffe://herd.test/herd/egress/node-1", localca.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return Identity{Source: s}
}
