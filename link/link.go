// Package link is the herdlet ↔ herd-api protocol. A herdlet dials out to herd-api and opens one bidirectional
// stream (LinkMethod); herd-api never dials a herdlet (spec R-D3). On the stream the herdlet registers its
// capabilities and capacity, herd-api sends it work, and the herdlet answers each piece of work with a three-valued
// outcome: done, failed (definitely not done) or unknown (it could not find out). A call that gets no answer in time
// is unknown, never done.
//
// Messages are JSON in gRPC framing (application/grpc+json) until the protocol gets a .proto alongside the gRPC
// dependency.
package link

import (
	"strconv"

	"github.com/everdict/herd/vmm"
)

// LinkMethod is the stream's full gRPC method name.
const LinkMethod = "/herd.ext.v1.Herdlet/Link"

// Up is a herdlet → herd-api message. Exactly one field is set.
type Up struct {
	Register *Register `json:"register,omitempty"`
	Capacity *Capacity `json:"capacity,omitempty"`
	// Capabilities replaces what the herdlet declared when it registered: a host whose fidelity changed (a Mac whose
	// console locked, or whose save/restore probe stopped passing) says so at once.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Outcome      *Outcome      `json:"outcome,omitempty"`
	// Exited says that a VM the herdlet ran for an actor stopped running without being asked to.
	Exited *Exited `json:"exited,omitempty"`
	// Progress reports the bytes a piece of work in flight has moved so far.
	Progress *Progress `json:"progress,omitempty"`
}

// Down is a herd-api → herdlet message. Exactly one field is set.
type Down struct {
	Registered *Registered `json:"registered,omitempty"`
	Work       *Work       `json:"work,omitempty"`
	// Extend moves the deadline of a piece of work in flight.
	Extend *Extend `json:"extend,omitempty"`
}

// Progress reports how many bytes the work with ID moves, as far as the herdlet knows yet: for OpSnapshot, the size
// its snapshot object has reached, which it then puts in the snapshot store. herd-api may lengthen the work's
// deadline by them (Hub.CallSized) and then sends the new deadline (Extend). A report never shortens a deadline.
type Progress struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
}

// Extend moves the deadline of the work with ID (Work.DeadlineUnixMilli) later.
type Extend struct {
	ID                string `json:"id"`
	DeadlineUnixMilli int64  `json:"deadlineUnixMilli"`
}

// Capabilities is what the host declares (ADR 0001, A-5; vmm.Capabilities).
type Capabilities struct {
	Platform              vmm.Platform `json:"platform"`
	Arch                  string       `json:"arch"`
	SuspendMaxFidelity    vmm.Fidelity `json:"suspendMaxFidelity"`
	RestoreFamily         string       `json:"restoreFamily,omitempty"`
	PortableRestoreFamily string       `json:"portableRestoreFamily"`
	HypervisorVersion     string       `json:"hypervisorVersion,omitempty"`
	Reason                string       `json:"reason,omitempty"`
	// EgressAllowPrefixes are the operator's lab allowances on the host's herd-egress (-egress-allow-prefix).
	EgressAllowPrefixes []string `json:"egressAllowPrefixes,omitempty"`
	// Guest names the guest build the herdlet boots its VMs with (the digest of its kernel and initramfs,
	// deploy.GuestIdentity); a golden snapshot records the guest that built it (DEFAUL-1606). Empty: not known.
	Guest string `json:"guest,omitempty"`
}

// Register is a herdlet's first message.
type Register struct {
	Worker string `json:"worker"`
	// Class is the sandbox class the worker runs ("microvm").
	Class        string            `json:"class"`
	Labels       map[string]string `json:"labels,omitempty"`
	Capabilities Capabilities      `json:"capabilities"`
	Capacity     Capacity          `json:"capacity"`
}

// Capacity is how many actors the herdlet can hold and which templates it keeps warm VMs for.
type Capacity struct {
	Slots int `json:"slots"`
	// Warm maps an actor template uid to the warm VMs the herdlet holds for it.
	Warm map[string]int `json:"warm,omitempty"`
}

// Registered acknowledges Register.
type Registered struct {
	Worker string `json:"worker"`
}

// Op is a piece of work.
type Op string

const (
	OpBoot     Op = "boot"
	OpSnapshot Op = "snapshot"
	OpRestore  Op = "restore"
	OpDelete   Op = "delete"
	OpInspect  Op = "inspect"
)

// SnapshotRef names a snapshot object in the snapshot store and what it holds.
type SnapshotRef struct {
	Key           string       `json:"key"`
	Digest        string       `json:"digest"`
	Fidelity      vmm.Fidelity `json:"fidelity"`
	RestoreFamily string       `json:"restoreFamily"`
}

// Work asks a herdlet to do one thing to one VM.
type Work struct {
	ID string `json:"id"`
	Op Op     `json:"op"`
	// VM is the VM's id on the herdlet (the actor's uid, label-safe).
	VM string `json:"vm"`
	// Template is the actor template's uid (a boot may take a warm VM of it).
	Template string `json:"template,omitempty"`
	// Spec is the template itself, for a boot.
	Spec *Template `json:"spec,omitempty"`
	// Fidelity is what OpSnapshot must keep.
	Fidelity vmm.Fidelity `json:"fidelity,omitempty"`
	// SnapshotKey is where OpSnapshot writes its object.
	SnapshotKey string `json:"snapshotKey,omitempty"`
	// Snapshot is what OpRestore restores.
	Snapshot *SnapshotRef `json:"snapshot,omitempty"`
	// Actor names the actor whose VM this is, on boot and restore (macOS hosts bind the VM's network to its egress
	// policy by it).
	Actor *ActorRef `json:"actor,omitempty"`
	// Cause says why herd-api asks for a snapshot or a delete (CauseSuspend, CauseDelete). The herdlet cuts the VM's
	// tunnel streams with it, so clients learn why their stream ended.
	Cause Cause `json:"cause,omitempty"`
	// DeadlineUnixMilli bounds the work; past it the herdlet answers unknown.
	DeadlineUnixMilli int64 `json:"deadlineUnixMilli,omitempty"`
	// PullAuth is a boot's one-shot registry logins for the template's images (CreateActorTemplateRequest's
	// image_pull_auth, a docker config's auths as JSON), sent only for the boot of a template's golden actor. The
	// herdlet's pulls for that boot log in with it ahead of the node's -registry-auth; it is never written down.
	PullAuth Secret `json:"pullAuth,omitempty"`
	// PlainHosts are a boot's one-shot plain-HTTP registry hosts (CreateActorTemplateRequest's
	// image_plain_http_hosts, DEFAUL-1600), sent only with the boot of a template's golden actor: the herdlet's pulls
	// for that boot speak plain HTTP to them, registries and token realms alike, besides its -insecure-registry.
	PlainHosts []string `json:"plainHosts,omitempty"`
}

// ActorRef names an actor.
type ActorRef struct {
	Space string `json:"space"`
	Name  string `json:"name"`
	UID   string `json:"uid,omitempty"`
}

// Cause is why a piece of work is asked for.
type Cause string

const (
	// CauseSuspend: the snapshot or delete is part of suspending the actor.
	CauseSuspend Cause = "suspend"
	// CauseDelete: the delete is the actor's deletion.
	CauseDelete Cause = "delete"
)

// Result is the three-valued outcome of a piece of work.
type Result string

const (
	// ResultDone: the work was carried out (for delete: and the VM verified gone).
	ResultDone Result = "done"
	// ResultFailed: the work was definitely not carried out.
	ResultFailed Result = "failed"
	// ResultUnknown: the herdlet could not find out. The zero value reads as unknown.
	ResultUnknown Result = ""
)

// Outcome answers a Work.
type Outcome struct {
	ID     string `json:"id"`
	Result Result `json:"result"`
	// Presence is the VM's presence after OpDelete or OpInspect: "gone", "present" or "" (unknown).
	Presence string `json:"presence,omitempty"`
	// Snapshot is what OpSnapshot wrote.
	Snapshot *SnapshotRef `json:"snapshot,omitempty"`
	Reason   string       `json:"reason,omitempty"`
	// Code, if set, is what kind of failure a failed outcome is (Code* constants), for herd-api to answer by.
	Code string `json:"code,omitempty"`
}

// CodeUnavailable: the work failed on something outside the host that can serve again later (an image registry
// that refused with 429 or 503 past the herdlet's backoff); herd-api answers it as Unavailable, not as a failure
// of the actor.
const CodeUnavailable = "unavailable"

// Presence names for Outcome.Presence.
const (
	PresenceGone    = "gone"
	PresencePresent = "present"
)

// Template is an actor template as a herdlet needs it to turn it into a VM (ADR A-19): herd-api resolves it from the
// ActorTemplate and sends it with a boot; a herdlet keeps Warm VMs of the templates it was given ahead of time.
type Template struct {
	// UID is the actor template's uid.
	UID        string      `json:"uid"`
	Containers []Container `json:"containers"`
	// Volumes are the template's DurableDir volume names.
	Volumes   []string `json:"volumes,omitempty"`
	VCPUs     int      `json:"vcpus,omitempty"`
	MemoryMiB int      `json:"memoryMiB,omitempty"`
	// Warm is how many booted VMs of this template the herdlet keeps ready for CreateActor.
	Warm int `json:"warm,omitempty"`
}

// Container is one container of a Template. Image is an OCI reference; Command and Args follow Substrate's rules
// (command replaces the image's ENTRYPOINT and CMD; args alone replace CMD).
type Container struct {
	Name         string        `json:"name"`
	Image        string        `json:"image"`
	Command      []string      `json:"command,omitempty"`
	Args         []string      `json:"args,omitempty"`
	Env          []EnvVar      `json:"env,omitempty"`
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

// ExitReason is why a herdlet reports a VM exited.
type ExitReason string

// ExitHypervisorExited: the VM's hypervisor process exited (or the VM is gone) while the actor was running, and
// nothing herd-api asked for stopped it.
const ExitHypervisorExited ExitReason = "hypervisor-exited"

// ExitContainerExited: a container's main process ended while its VM runs (herd-guest recorded its status and last
// output); the actor serves nothing more, and a delete ends the VM.
const ExitContainerExited ExitReason = "container-exited"

// Exited reports a VM that stopped running on its own. The herdlet observed it; a herdlet that could not observe its
// VM reports nothing, since unknown is not crashed.
type Exited struct {
	// VM is the VM's id as herd-api named it in Work (the actor's uid).
	VM     string     `json:"vm"`
	Reason ExitReason `json:"reason"`
	// Detail is what the herdlet knows of why (the process's exit status, the end of the hypervisor's log).
	Detail string `json:"detail,omitempty"`
}

// Secret is a credential carried on the link: it marshals as itself, and prints redacted, so a %v of the Work that
// holds it logs no secret.
type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "(redacted)"
}

// GoString keeps %#v redacted too.
func (s Secret) GoString() string { return strconv.Quote(s.String()) }
