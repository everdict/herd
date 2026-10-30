package conformance_test

import (
	"testing"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"github.com/everdict/herd/conformance"
	"google.golang.org/grpc/codes"
)

// TestEgressPolicyAdmission: a policy Substrate would refuse at admission is refused (InvalidArgument) on create
// and on update: rules that tie, an IP address as a hostname, a rule with two protocols, a header with two
// credential sources.
func TestEgressPolicyAdmission(t *testing.T) {
	e := conformance.Start(t, conformance.HerdletSpec{Name: "h1", Slots: 2})
	ctx := ctxT(t)
	mkTemplate(t, e, "tpl", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED)
	mkActor(t, e, "a1", "tpl", nil)
	https := func(hosts ...string) *ateapi.EgressRule {
		return &ateapi.EgressRule{Https: &ateapi.HttpsRule{Hostnames: hosts}}
	}
	bad := map[string][]*ateapi.EgressRule{
		"tie":           {https("api.example.com"), https("api.example.com")},
		"ip hostname":   {https("169.254.169.254")},
		"two protocols": {{Http: &ateapi.HttpRule{Hostnames: []string{"a.com"}}, Https: &ateapi.HttpsRule{Hostnames: []string{"a.com"}}}},
		"two sources": {{Https: &ateapi.HttpsRule{Hostnames: []string{"a.com"}, Effects: &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{
			{Header: "Authorization", CredentialUri: "ate-secret://x/y/z", ActorJwt: &ateapi.ActorJwtSource{Audiences: []string{"a"}, ExpirationSeconds: 600}},
		}}}}},
	}
	for name, rules := range bad {
		_, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{Rules: rules}})
		wantCode(t, "create "+name, err, codes.InvalidArgument)
	}
	pol, err := e.Client.CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{https("api.example.com")}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, rules := range bad {
		_, err := e.Client.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: oref(e, "a1"), EgressPolicy: &ateapi.EgressPolicy{Metadata: pol.GetMetadata(), Rules: rules}})
		wantCode(t, "update "+name, err, codes.InvalidArgument)
	}
}
