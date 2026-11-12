package auth

import (
	"fmt"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Kind is what a SPIFFE ID names (workload identity spec E3).
type Kind string

const (
	// KindControlPlane is Everdict's control plane: /everdict/control-plane.
	KindControlPlane Kind = "control-plane"
	// KindRunner is an Everdict runner (a herder) of a workspace: /everdict/ws/<workspace>/runner/<runner>.
	KindRunner Kind = "runner"
	// KindHerdAPI is herd-api itself: /herd/api.
	KindHerdAPI Kind = "herd-api"
	// KindNode is a herdlet's node: /herd/node/<worker>.
	KindNode Kind = "node"
	// KindActor is an actor herd booted: /herd/atespace/<atespace>/actor/<uid>.
	KindActor Kind = "actor"
	// KindOperator is a person or tool operating herd: /herd/operator/<name> (herd's addition to E3's paths).
	KindOperator Kind = "operator"
)

// Identity is a SPIFFE ID read by ParseID: its kind and the names its path carries.
type Identity struct {
	ID   spiffeid.ID
	Kind Kind
	// Workspace and Runner are a KindRunner's.
	Workspace, Runner string
	// Worker is a KindNode's.
	Worker string
	// Atespace and ActorUID are a KindActor's.
	Atespace, ActorUID string
	// Operator is a KindOperator's.
	Operator string
}

// ParseID is the one function that reads a SPIFFE ID's path (E3). A path that parses to none of the kinds is refused,
// never treated as anonymous.
func ParseID(id spiffeid.ID) (Identity, error) {
	out := Identity{ID: id}
	seg := strings.Split(strings.TrimPrefix(id.Path(), "/"), "/")
	ok := true
	switch {
	case len(seg) == 2 && seg[0] == "everdict" && seg[1] == "control-plane":
		out.Kind = KindControlPlane
	case len(seg) == 5 && seg[0] == "everdict" && seg[1] == "ws" && seg[3] == "runner":
		out.Kind, out.Workspace, out.Runner = KindRunner, seg[2], seg[4]
	case len(seg) == 2 && seg[0] == "herd" && seg[1] == "api":
		out.Kind = KindHerdAPI
	case len(seg) == 3 && seg[0] == "herd" && seg[1] == "node":
		out.Kind, out.Worker = KindNode, seg[2]
	case len(seg) == 5 && seg[0] == "herd" && seg[1] == "atespace" && seg[3] == "actor":
		out.Kind, out.Atespace, out.ActorUID = KindActor, seg[2], seg[4]
	case len(seg) == 3 && seg[0] == "herd" && seg[1] == "operator":
		out.Kind, out.Operator = KindOperator, seg[2]
	default:
		ok = false
	}
	for _, s := range seg {
		ok = ok && s != ""
	}
	if !ok {
		return Identity{}, fmt.Errorf("SPIFFE ID %s: path %q names no herd or Everdict identity", id, id.Path())
	}
	return out, nil
}
