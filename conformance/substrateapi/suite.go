package substrateapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Config is one target: where its Control API and router are, who the suite is, and which template it drives.
type Config struct {
	// Target labels the report ("substrate-kind", "herd-all-in-one", ...).
	Target  string
	Control Endpoint
	Router  Router
	// Token is the bearer token for Control calls (Router.Token is sent to the router).
	Token    string
	Atespace string
	// Template is an existing ActorTemplate in Atespace; every actor the suite creates is of it.
	Template string
	// Probe is the request the suite sends through the router; it must match what the template serves.
	Probe Probe
	// Prefix starts every name the suite creates (default "cf-" and 6 random hex digits).
	Prefix string
	// Wait bounds every wait for a state (default 3 minutes); Poll is the interval (default 250 ms).
	Wait time.Duration
	Poll time.Duration
	// ExpectFidelity, if set, is the fidelity the target declared for this template's suspends; otherwise any
	// fidelity from VOLUMES up to the template's preferred_fidelity passes (Substrate's ceiling rule).
	ExpectFidelity ateapi.SnapshotFidelity
	// VolumePath is a file path inside one of the template's volumes. With a probe that can write files, the
	// suspend, fork and move cases check that the volume carries what was written.
	VolumePath string
	// MoveFrom and MoveTo are worker selectors of two different worker pools; with both set, the move case
	// creates an actor on MoveFrom, suspends it and resumes it on MoveTo.
	MoveFrom, MoveTo map[string]string
}

// Outcome is a case's result.
type Outcome string

const (
	Pass Outcome = "PASS"
	Fail Outcome = "FAIL"
	Skip Outcome = "SKIP"
)

// Result is one case.
type Result struct {
	Case    string
	Outcome Outcome
	// Detail is what was observed; for a skip, the server's declared reason.
	Detail string
}

// Report is a run against one target.
type Report struct {
	Target  string
	Results []Result
}

// Cases are the suite's cases, in the order they run.
var Cases = []string{
	"create-from-template", "resume", "request", "suspend", "resume-on-request", "fork-via-tag", "delete-verified",
	"refuse-stale-version", "suspend-async", "refuse-unknown-actor", "move",
}

// OK is true when no case failed and every skip carries the server's declared reason.
func (r Report) OK() bool {
	for _, c := range r.Results {
		if c.Outcome == Fail || (c.Outcome == Skip && strings.TrimSpace(c.Detail) == "") {
			return false
		}
	}
	return len(r.Results) > 0
}

// Skips lists the skipped cases with their reasons.
func (r Report) Skips() []Result {
	var out []Result
	for _, c := range r.Results {
		if c.Outcome == Skip {
			out = append(out, c)
		}
	}
	return out
}

// Get returns the result of a case.
func (r Report) Get(name string) (Result, bool) {
	for _, c := range r.Results {
		if c.Case == name {
			return c, true
		}
	}
	return Result{}, false
}

// Print writes the report: one line per case, then the target's skip list.
func (r Report) Print(w io.Writer) {
	n := map[Outcome]int{}
	for _, c := range r.Results {
		n[c.Outcome]++
		fmt.Fprintf(w, "%s %-22s %s\n", c.Outcome, c.Case, c.Detail)
	}
	skips := r.Skips()
	if len(skips) == 0 {
		fmt.Fprintf(w, "skips on %s: none\n", r.Target)
	} else {
		fmt.Fprintf(w, "skips on %s:\n", r.Target)
		for _, s := range skips {
			fmt.Fprintf(w, "  %s: %s\n", s.Case, s.Detail)
		}
	}
	verdict := "ok"
	if !r.OK() {
		verdict = "FAILED"
	}
	fmt.Fprintf(w, "%s: %d passed, %d failed, %d skipped: %s\n", r.Target, n[Pass], n[Fail], n[Skip], verdict)
}

// declared is a skip the server declared: it answered Unimplemented.
type declared struct{ reason string }

func (d declared) Error() string { return "server declared: " + d.reason }

// notConfigured is a case the operator did not configure (the move case without two worker pools). It is a skip
// whose reason is the missing configuration, printed in the skip list like a declared one.
type notConfigured struct{ what string }

func (n notConfigured) Error() string { return "not configured: " + n.what }

// gate turns a call's Unimplemented answer into a declared skip, and leaves every other error as it is.
func gate(what string, err error) error {
	if s, ok := status.FromError(err); ok && s.Code() == codes.Unimplemented {
		reason := strings.TrimSpace(s.Message())
		if reason == "" {
			// Unimplemented without a reason is not a declaration.
			return fmt.Errorf("%s: Unimplemented with no reason", what)
		}
		return declared{what + ": " + reason}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

type run struct {
	cfg  Config
	c    *Control
	tpl  *ateapi.ActorTemplate
	made []string // actors created, for cleanup
	tags []string
}

// Run runs every case against the target and returns the report; a target that cannot be reached at all fails
// every case.
func Run(ctx context.Context, cfg Config) Report {
	if cfg.Target == "" {
		cfg.Target = cfg.Control.Addr
	}
	rep := Report{Target: cfg.Target}
	if cfg.Prefix == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		cfg.Prefix = "cf-" + hex.EncodeToString(b)
	}
	if cfg.Wait == 0 {
		cfg.Wait = 3 * time.Minute
	}
	if cfg.Poll == 0 {
		cfg.Poll = 250 * time.Millisecond
	}
	fail := func(err error) Report {
		for _, name := range Cases {
			rep.Results = append(rep.Results, Result{Case: name, Outcome: Fail, Detail: err.Error()})
		}
		return rep
	}
	if cfg.Probe == nil || cfg.Router.Addr == "" || cfg.Atespace == "" || cfg.Template == "" {
		return fail(errors.New("the suite needs a Control endpoint, a router, an atespace, a template and a probe"))
	}
	c, err := DialControl(cfg.Control, cfg.Token)
	if err != nil {
		return fail(err)
	}
	defer c.Close()
	r := &run{cfg: cfg, c: c}
	if r.tpl, err = c.GetActorTemplate(ctx, cfg.Atespace, cfg.Template); err != nil {
		return fail(fmt.Errorf("GetActorTemplate %s/%s: %w", cfg.Atespace, cfg.Template, err))
	}
	defer r.cleanup()

	record := func(name string, err error, detail string) {
		var d declared
		var nc notConfigured
		switch {
		case errors.As(err, &d):
			rep.Results = append(rep.Results, Result{Case: name, Outcome: Skip, Detail: d.Error()})
		case errors.As(err, &nc):
			rep.Results = append(rep.Results, Result{Case: name, Outcome: Skip, Detail: nc.Error()})
		case err != nil:
			rep.Results = append(rep.Results, Result{Case: name, Outcome: Fail, Detail: err.Error()})
		default:
			rep.Results = append(rep.Results, Result{Case: name, Outcome: Pass, Detail: detail})
		}
	}
	// The lifecycle walk: each case needs the one before it, so the first failure fails the rest as not reached.
	walk := []struct {
		name string
		f    func(ctx context.Context) (string, error)
	}{
		{"create-from-template", r.create},
		{"resume", r.resume},
		{"request", r.request},
		{"suspend", r.suspend},
		{"resume-on-request", r.resumeOnRequest},
		{"fork-via-tag", r.fork},
		{"delete-verified", r.deleteVerified},
	}
	broken := ""
	for _, s := range walk {
		if broken != "" {
			record(s.name, fmt.Errorf("not reached: %s did not pass", broken), "")
			continue
		}
		detail, err := s.f(ctx)
		record(s.name, err, detail)
		var d declared
		// A declared skip of the fork does not break the walk: the delete runs on the parent all the same.
		if err != nil && !(s.name == "fork-via-tag" && errors.As(err, &d)) {
			broken = s.name
		}
	}
	for _, s := range []struct {
		name string
		f    func(ctx context.Context) (string, error)
	}{
		{"refuse-stale-version", r.staleVersion},
		{"suspend-async", r.suspendAsync},
		{"refuse-unknown-actor", r.unknownActor},
		{"move", r.move},
	} {
		detail, err := s.f(ctx)
		record(s.name, err, detail)
	}
	return rep
}

func (r *run) name(role string) string { return r.cfg.Prefix + "-" + role }

func (r *run) target(name string) string { return r.cfg.Atespace + "/" + name }

func (r *run) newActor(ctx context.Context, name string, selector map[string]string, tag string) (*ateapi.Actor, error) {
	a := &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: r.cfg.Atespace, Name: name},
		ActorTemplate: ref(r.cfg.Atespace, r.cfg.Template),
	}
	if selector != nil {
		a.WorkerSelector = &ateapi.Selector{MatchLabels: selector}
	}
	if tag != "" {
		a.SourceTag = ref(r.cfg.Atespace, tag)
	}
	got, err := r.c.CreateActor(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("CreateActor %s: %w", name, err)
	}
	r.made = append(r.made, name)
	if got.GetMetadata().GetUid() == "" || got.GetActorTemplate().GetName() != r.cfg.Template {
		return got, fmt.Errorf("CreateActor %s: answered uid %q template %v", name, got.GetMetadata().GetUid(), got.GetActorTemplate())
	}
	if tag != "" && got.GetSourceTag().GetName() != tag {
		return got, fmt.Errorf("CreateActor %s: source_tag %v, want %s", name, got.GetSourceTag(), tag)
	}
	// Substrate records a new actor SUSPENDED and a ResumeActor starts it; a runtime may also start it at once.
	switch s := got.GetStatus().GetState(); s {
	case ateapi.ActorState_ACTOR_STATE_SUSPENDED, ateapi.ActorState_ACTOR_STATE_RESUMING, ateapi.ActorState_ACTOR_STATE_RUNNING:
	default:
		return got, fmt.Errorf("CreateActor %s: state %v", name, s)
	}
	return got, nil
}

// wait polls the actor until it is in want; a CRASHED actor or a wrong final state ends the wait early.
func (r *run) wait(ctx context.Context, name string, want ateapi.ActorState) (*ateapi.Actor, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Wait)
	defer cancel()
	var last *ateapi.Actor
	var lastErr error
	for {
		a, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
		if err == nil {
			last = a
			s := a.GetStatus().GetState()
			if s == want {
				return a, nil
			}
			if s == ateapi.ActorState_ACTOR_STATE_CRASHED {
				return a, fmt.Errorf("actor %s CRASHED: %v", name, a.GetStatus().GetCrash())
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("actor %s: not %v within %v (last: %v, %v)", name, want, r.cfg.Wait, last.GetStatus().GetState(), lastErr)
		case <-time.After(r.cfg.Poll):
		}
	}
}

// waitGone polls until GetActor answers NotFound.
func (r *run) waitGone(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Wait)
	defer cancel()
	for {
		a, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
		if status.Code(err) == codes.NotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("actor %s still answered after delete: %v, %v", name, a.GetStatus().GetState(), err)
		case <-time.After(r.cfg.Poll):
		}
	}
}

func (r *run) resumeRunning(ctx context.Context, name string) (*ateapi.Actor, error) {
	if _, err := r.c.ResumeActor(ctx, r.cfg.Atespace, name); err != nil {
		return nil, fmt.Errorf("ResumeActor %s: %w", name, err)
	}
	return r.wait(ctx, name, ateapi.ActorState_ACTOR_STATE_RUNNING)
}

// suspendDurable suspends name with DURABILITY_MODE_SYNCHRONOUS, the mode whose contract is durability: the call
// returns only after the snapshot is complete in durable storage. So the actor must be SUSPENDED, with no worker,
// and its latest snapshot COMPLETED by the time the call returns (in its answer, or in a GetActor right after it;
// the suite does not wait for it).
func (r *run) suspendDurable(ctx context.Context, name string) (*ateapi.Actor, *ateapi.Snapshot, error) {
	resp, err := r.c.SuspendActor(ctx, r.cfg.Atespace, name, ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS)
	if err != nil {
		return nil, nil, fmt.Errorf("SuspendActor %s (SYNCHRONOUS): %w", name, err)
	}
	a := resp.GetActor()
	if s := latest(a); a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED ||
		s.GetDurableSnapshot().GetState() != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		if a, err = r.c.GetActor(ctx, r.cfg.Atespace, name); err != nil {
			return nil, nil, fmt.Errorf("GetActor %s: %w", name, err)
		}
	}
	s := latest(a)
	switch {
	case a.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED:
		return a, s, fmt.Errorf("SuspendActor %s (SYNCHRONOUS) returned with the actor %v, want SUSPENDED", name, a.GetStatus().GetState())
	case s.GetDurableSnapshot().GetState() != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED:
		return a, s, fmt.Errorf("SuspendActor %s (SYNCHRONOUS) returned before its snapshot was durable: %v", name, s)
	case a.GetStatus().GetWorkerAssignment() != nil:
		return a, s, fmt.Errorf("actor %s is SUSPENDED but still holds worker %v", name, a.GetStatus().GetWorkerAssignment())
	}
	return a, s, nil
}

// suspendAsync: DURABILITY_MODE_ASYNCHRONOUS returns once the snapshot is captured on the node and uploads it in
// the background. The case asserts the suspend only — the actor becomes SUSPENDED with a snapshot of its own at an
// allowed fidelity and gives up its worker — and not durability, which this mode does not promise at return.
func (r *run) suspendAsync(ctx context.Context) (string, error) {
	name := r.name("async")
	if _, err := r.newActor(ctx, name, nil, ""); err != nil {
		return "", err
	}
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	resp, err := r.c.SuspendActor(ctx, r.cfg.Atespace, name, ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS)
	if err != nil {
		return "", fmt.Errorf("SuspendActor %s (ASYNCHRONOUS): %w", name, err)
	}
	answered := resp.GetActor().GetStatus().GetState()
	if answered != ateapi.ActorState_ACTOR_STATE_SUSPENDED && answered != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
		return "", fmt.Errorf("SuspendActor %s (ASYNCHRONOUS) answered %v", name, answered)
	}
	a, err := r.wait(ctx, name, ateapi.ActorState_ACTOR_STATE_SUSPENDED)
	if err != nil {
		return "", err
	}
	s := latest(a)
	if err := r.fidelityOK(s); err != nil {
		return "", err
	}
	if a.GetStatus().GetWorkerAssignment() != nil {
		return "", fmt.Errorf("actor %s is SUSPENDED but still holds worker %v", name, a.GetStatus().GetWorkerAssignment())
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); err != nil {
		return "", fmt.Errorf("DeleteActor: %w", err)
	}
	if err := r.waitGone(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("answered %v, then SUSPENDED at %v (storage %v at that point)", answered, s.GetFidelity(), s.GetDurableSnapshot().GetState()), nil
}

func latest(a *ateapi.Actor) *ateapi.Snapshot {
	var s *ateapi.Snapshot
	for _, x := range a.GetStatus().GetSnapshots() {
		if s == nil || x.GetGeneration() > s.GetGeneration() {
			s = x
		}
	}
	return s
}

func (r *run) files() (FileProbe, bool) {
	fp, ok := r.cfg.Probe.(FileProbe)
	return fp, ok && r.cfg.VolumePath != ""
}

func (r *run) create(ctx context.Context) (string, error) {
	a, err := r.newActor(ctx, r.name("a"), nil, "")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s uid %s created %v", a.GetMetadata().GetName(), a.GetMetadata().GetUid(), a.GetStatus().GetState()), nil
}

func (r *run) resume(ctx context.Context) (string, error) {
	a, err := r.resumeRunning(ctx, r.name("a"))
	if err != nil {
		return "", err
	}
	if a.GetStatus().GetWorkerAssignment() == nil {
		return "", fmt.Errorf("actor %s is RUNNING with no worker assignment", a.GetMetadata().GetName())
	}
	return "RUNNING on " + a.GetStatus().GetAssignedNode(), nil
}

func (r *run) request(ctx context.Context) (string, error) {
	if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(r.name("a"))); err != nil {
		return "", fmt.Errorf("%s through the router: %w", r.cfg.Probe.Name(), err)
	}
	if fp, ok := r.files(); ok {
		if err := fp.WriteFile(ctx, r.cfg.Router, r.target(r.name("a")), r.cfg.VolumePath, "before-suspend"); err != nil {
			return "", fmt.Errorf("writing %s: %w", r.cfg.VolumePath, err)
		}
	}
	if rp, ok := r.cfg.Probe.(RefusalProbe); ok {
		if err := rp.Refusal(ctx, r.cfg.Router, r.target(r.name("a"))); err != nil {
			return "", err
		}
		return r.cfg.Probe.Name() + " answered; the actor's own refusal came through typed", nil
	}
	return r.cfg.Probe.Name() + " answered", nil
}

func (r *run) suspend(ctx context.Context) (string, error) {
	a, s, err := r.suspendDurable(ctx, r.name("a"))
	if err != nil {
		return "", err
	}
	if err := r.fidelityOK(s); err != nil {
		return "", err
	}
	return fmt.Sprintf("SYNCHRONOUS: SUSPENDED and durable at return, %v (generation %d, preferred %v)", s.GetFidelity(), s.GetGeneration(), r.preferred()) + nodeOf(a), nil
}

func (r *run) preferred() ateapi.SnapshotFidelity {
	if p := r.tpl.GetSnapshotConfig().GetPreferredFidelity(); p != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_UNSPECIFIED {
		return p
	}
	return ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
}

// fidelityOK: a suspend's snapshot is the actor's own, at a fidelity from VOLUMES up to the template's
// preferred_fidelity (Substrate's ceiling rule), and exactly the one the target declared when one was given.
func (r *run) fidelityOK(s *ateapi.Snapshot) error {
	f, pref := s.GetFidelity(), r.preferred()
	switch {
	case s == nil:
		return errors.New("the suspended actor holds no snapshot")
	case f < ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES || f > pref:
		return fmt.Errorf("snapshot at %v; the template's preferred_fidelity %v is the ceiling", f, pref)
	case r.cfg.ExpectFidelity != 0 && f != r.cfg.ExpectFidelity:
		return fmt.Errorf("snapshot at %v, the target declared %v", f, r.cfg.ExpectFidelity)
	case s.GetOwner() != ateapi.SnapshotOwner_SNAPSHOT_OWNER_ACTOR:
		return fmt.Errorf("snapshot owner %v, want ACTOR", s.GetOwner())
	}
	return nil
}

func nodeOf(a *ateapi.Actor) string {
	if n := a.GetStatus().GetAssignedNode(); n != "" {
		return ", node " + n
	}
	return ""
}

func (r *run) resumeOnRequest(ctx context.Context) (string, error) {
	name := r.name("a")
	if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(name)); err != nil {
		return "", fmt.Errorf("%s to the suspended actor: %w", r.cfg.Probe.Name(), err)
	}
	a, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
	if err != nil {
		return "", fmt.Errorf("GetActor: %w", err)
	}
	if s := a.GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_RUNNING {
		return "", fmt.Errorf("after the request the actor is %v, want RUNNING", s)
	}
	if fp, ok := r.files(); ok {
		got, err := fp.ReadFile(ctx, r.cfg.Router, r.target(name), r.cfg.VolumePath)
		if err != nil || got != "before-suspend" {
			return "", fmt.Errorf("%s after the resume: %q, %v; want what was written before the suspend", r.cfg.VolumePath, got, err)
		}
	}
	return "the request resumed it: RUNNING" + nodeOf(a), nil
}

// fork: CreateTag of the suspended parent, two actors from the tag (CreateActor{template, source_tag}); the forks
// run independently of each other and the parent is not touched by either.
func (r *run) fork(ctx context.Context) (string, error) {
	parent, tagName := r.name("a"), r.name("tag")
	fp, files := r.files()
	if files {
		if err := fp.WriteFile(ctx, r.cfg.Router, r.target(parent), r.cfg.VolumePath, "parent"); err != nil {
			return "", fmt.Errorf("writing the parent's %s: %w", r.cfg.VolumePath, err)
		}
	}
	// A tag captures a suspended actor's snapshot.
	pa, ps, err := r.suspendDurable(ctx, parent)
	if err != nil {
		return "", err
	}
	if _, err := r.c.CreateTag(ctx, &ateapi.Tag{
		Metadata: &ateapi.ResourceMetadata{Atespace: r.cfg.Atespace, Name: tagName},
		Scope:    ateapi.TagScope_TAG_SCOPE_ATESPACE, SourceActor: ref(r.cfg.Atespace, parent),
	}); err != nil {
		return "", gate("CreateTag", err)
	}
	r.tags = append(r.tags, tagName)
	if err := r.waitTag(ctx, tagName); err != nil {
		return "", err
	}

	forks := []string{r.name("fork1"), r.name("fork2")}
	uids := map[string]string{pa.GetMetadata().GetUid(): parent}
	for _, f := range forks {
		a, err := r.newActor(ctx, f, nil, tagName)
		if err != nil {
			return "", err
		}
		if other, dup := uids[a.GetMetadata().GetUid()]; dup {
			return "", fmt.Errorf("fork %s has the uid of %s", f, other)
		}
		uids[a.GetMetadata().GetUid()] = f
		if _, err := r.resumeRunning(ctx, f); err != nil {
			return "", err
		}
		if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(f)); err != nil {
			return "", fmt.Errorf("%s to fork %s: %w", r.cfg.Probe.Name(), f, err)
		}
		if files {
			got, err := fp.ReadFile(ctx, r.cfg.Router, r.target(f), r.cfg.VolumePath)
			if err != nil || got != "parent" {
				return "", fmt.Errorf("fork %s reads %q, %v from %s; want the parent's %q", f, got, err, r.cfg.VolumePath, "parent")
			}
			if err := fp.WriteFile(ctx, r.cfg.Router, r.target(f), r.cfg.VolumePath, f); err != nil {
				return "", fmt.Errorf("fork %s writing %s: %w", f, r.cfg.VolumePath, err)
			}
		}
	}
	// Siblings are independent: suspending and deleting one leaves the other running and reachable.
	if _, _, err := r.suspendDurable(ctx, forks[0]); err != nil {
		return "", err
	}
	if err := r.running(ctx, forks[1], "after its sibling's suspend"); err != nil {
		return "", err
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, forks[0]); err != nil {
		return "", fmt.Errorf("DeleteActor %s: %w", forks[0], err)
	}
	if err := r.waitGone(ctx, forks[0]); err != nil {
		return "", err
	}
	if err := r.running(ctx, forks[1], "after its sibling's delete"); err != nil {
		return "", err
	}
	if files {
		if got, err := fp.ReadFile(ctx, r.cfg.Router, r.target(forks[1]), r.cfg.VolumePath); err != nil || got != forks[1] {
			return "", fmt.Errorf("fork %s reads %q, %v; want its own write", forks[1], got, err)
		}
	}
	// The parent is unaffected: same actor, still SUSPENDED on the snapshot it had, and it resumes as itself.
	now, err := r.c.GetActor(ctx, r.cfg.Atespace, parent)
	if err != nil {
		return "", fmt.Errorf("GetActor %s: %w", parent, err)
	}
	if now.GetMetadata().GetUid() != pa.GetMetadata().GetUid() || now.GetStatus().GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED ||
		latest(now).GetGeneration() != ps.GetGeneration() {
		return "", fmt.Errorf("the parent changed under its forks: uid %s→%s, %v, snapshot generation %d→%d",
			pa.GetMetadata().GetUid(), now.GetMetadata().GetUid(), now.GetStatus().GetState(), ps.GetGeneration(), latest(now).GetGeneration())
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, forks[1]); err != nil {
		return "", fmt.Errorf("DeleteActor %s: %w", forks[1], err)
	}
	if err := r.waitGone(ctx, forks[1]); err != nil {
		return "", err
	}
	if _, err := r.c.DeleteTag(ctx, r.cfg.Atespace, tagName); err != nil {
		return "", fmt.Errorf("DeleteTag: %w", err)
	}
	r.tags = nil
	back, err := r.resumeRunning(ctx, parent)
	if err != nil {
		return "", err
	}
	if back.GetMetadata().GetUid() != pa.GetMetadata().GetUid() {
		return "", fmt.Errorf("the parent resumed as uid %s, was %s", back.GetMetadata().GetUid(), pa.GetMetadata().GetUid())
	}
	if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(parent)); err != nil {
		return "", fmt.Errorf("%s to the parent after the forks: %w", r.cfg.Probe.Name(), err)
	}
	if files {
		if got, err := fp.ReadFile(ctx, r.cfg.Router, r.target(parent), r.cfg.VolumePath); err != nil || got != "parent" {
			return "", fmt.Errorf("the parent reads %q, %v after its forks wrote; want %q", got, err, "parent")
		}
	}
	return fmt.Sprintf("tag of a %v snapshot; 2 forks ran independently; parent unaffected", ps.GetFidelity()), nil
}

func (r *run) running(ctx context.Context, name, when string) error {
	a, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
	if err != nil {
		return fmt.Errorf("GetActor %s %s: %w", name, when, err)
	}
	if s := a.GetStatus().GetState(); s != ateapi.ActorState_ACTOR_STATE_RUNNING {
		return fmt.Errorf("%s is %v %s", name, s, when)
	}
	if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(name)); err != nil {
		return fmt.Errorf("%s to %s %s: %w", r.cfg.Probe.Name(), name, when, err)
	}
	return nil
}

// waitTag polls until the tag's snapshot is COMPLETED (a tag is usable only then) under the suite's template.
func (r *run) waitTag(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Wait)
	defer cancel()
	for {
		t, err := r.c.GetTag(ctx, r.cfg.Atespace, name)
		if err == nil && t.GetStatus().GetSnapshot().GetDurableSnapshot().GetState() == ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
			if uid := t.GetStatus().GetActorTemplateUid(); uid != r.tpl.GetMetadata().GetUid() {
				return fmt.Errorf("tag %s was taken under template uid %q, want %q", name, uid, r.tpl.GetMetadata().GetUid())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tag %s not COMPLETED within %v: %v, %v", name, r.cfg.Wait, t.GetStatus(), err)
		case <-time.After(r.cfg.Poll):
		}
	}
}

func (r *run) deleteVerified(ctx context.Context) (string, error) {
	name := r.name("a")
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); err != nil {
		return "", fmt.Errorf("DeleteActor: %w", err)
	}
	if err := r.waitGone(ctx, name); err != nil {
		return "", err
	}
	list, err := r.c.ListActors(ctx, r.cfg.Atespace)
	if err != nil {
		return "", fmt.Errorf("ListActors: %w", err)
	}
	for _, a := range list.GetActors() {
		if a.GetMetadata().GetName() == name {
			return "", fmt.Errorf("ListActors still lists %s (%v)", name, a.GetStatus().GetState())
		}
	}
	return "GetActor answers NotFound; ListActors omits it", nil
}

// staleVersion: an UpdateActor carrying a version the server no longer holds is refused Aborted, as is one with
// another uid; the current version is accepted.
func (r *run) staleVersion(ctx context.Context) (string, error) {
	name := r.name("upd")
	if _, err := r.newActor(ctx, name, nil, ""); err != nil {
		return "", err
	}
	cur, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
	if err != nil {
		return "", fmt.Errorf("GetActor: %w", err)
	}
	upd := func(uid string, version int64) error {
		a := proto.CloneOf(cur)
		a.Status = nil
		a.Metadata.Uid, a.Metadata.Version = uid, version
		_, err := r.c.UpdateActor(ctx, a)
		return err
	}
	uid, old := cur.GetMetadata().GetUid(), cur.GetMetadata().GetVersion()
	if err := upd(uid, old); err != nil {
		return "", gate("UpdateActor at the current version", err)
	}
	now, err := r.c.GetActor(ctx, r.cfg.Atespace, name)
	if err != nil {
		return "", fmt.Errorf("GetActor: %w", err)
	}
	if now.GetMetadata().GetVersion() == old {
		return "", fmt.Errorf("an accepted UpdateActor left version %d", old)
	}
	if err := upd(uid, old); status.Code(err) != codes.Aborted {
		return "", fmt.Errorf("UpdateActor at stale version %d (current %d): %v, want Aborted", old, now.GetMetadata().GetVersion(), err)
	}
	if err := upd("00000000-0000-4000-8000-000000000000", now.GetMetadata().GetVersion()); status.Code(err) != codes.Aborted {
		return "", fmt.Errorf("UpdateActor with another uid: %v, want Aborted", err)
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); err != nil {
		return "", fmt.Errorf("DeleteActor: %w", err)
	}
	if err := r.waitGone(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("version %d→%d accepted; stale version and other uid Aborted", old, now.GetMetadata().GetVersion()), nil
}

// unknownActor: every call on an actor that does not exist answers NotFound.
func (r *run) unknownActor(ctx context.Context) (string, error) {
	name := r.name("nobody")
	calls := []struct {
		what string
		f    func() error
	}{
		{"GetActor", func() error { _, err := r.c.GetActor(ctx, r.cfg.Atespace, name); return err }},
		{"SuspendActor", func() error {
			_, err := r.c.SuspendActor(ctx, r.cfg.Atespace, name, ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS)
			return err
		}},
		{"ResumeActor", func() error { _, err := r.c.ResumeActor(ctx, r.cfg.Atespace, name); return err }},
		{"DeleteActor", func() error { _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); return err }},
	}
	var bad []string
	for _, c := range calls {
		if err := c.f(); status.Code(err) != codes.NotFound {
			bad = append(bad, fmt.Sprintf("%s: %v", c.what, err))
		}
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("want NotFound: %s", strings.Join(bad, "; "))
	}
	return "Get/Suspend/Resume/Delete answer NotFound", nil
}

// move: an actor suspended on one worker pool resumes on another (Substrate: worker_selector changes take effect
// on the next ResumeActor) and keeps its volume.
func (r *run) move(ctx context.Context) (string, error) {
	if len(r.cfg.MoveFrom) == 0 || len(r.cfg.MoveTo) == 0 {
		return "", notConfigured{"needs two worker pools (--move-from, --move-to)"}
	}
	name := r.name("mv")
	if _, err := r.newActor(ctx, name, r.cfg.MoveFrom, ""); err != nil {
		return "", err
	}
	a, err := r.resumeRunning(ctx, name)
	if err != nil {
		return "", err
	}
	from := a.GetStatus().GetAssignedNode()
	fp, files := r.files()
	if files {
		if err := fp.WriteFile(ctx, r.cfg.Router, r.target(name), r.cfg.VolumePath, "moved"); err != nil {
			return "", fmt.Errorf("writing %s: %w", r.cfg.VolumePath, err)
		}
	}
	sa, s, err := r.suspendDurable(ctx, name)
	if err != nil {
		return "", err
	}
	u := proto.CloneOf(sa)
	u.Status = nil
	u.WorkerSelector = &ateapi.Selector{MatchLabels: r.cfg.MoveTo}
	if _, err := r.c.UpdateActor(ctx, u); err != nil {
		return "", fmt.Errorf("UpdateActor worker_selector: %w", err)
	}
	b, err := r.resumeRunning(ctx, name)
	if err != nil {
		return "", err
	}
	to := b.GetStatus().GetAssignedNode()
	if to == from {
		return "", fmt.Errorf("resumed on %q again, want another worker pool", to)
	}
	if err := r.cfg.Probe.Request(ctx, r.cfg.Router, r.target(name)); err != nil {
		return "", fmt.Errorf("%s after the move: %w", r.cfg.Probe.Name(), err)
	}
	if files {
		if got, err := fp.ReadFile(ctx, r.cfg.Router, r.target(name), r.cfg.VolumePath); err != nil || got != "moved" {
			return "", fmt.Errorf("%s after the move: %q, %v; want %q", r.cfg.VolumePath, got, err, "moved")
		}
	}
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); err != nil {
		return "", fmt.Errorf("DeleteActor: %w", err)
	}
	if err := r.waitGone(ctx, name); err != nil {
		return "", err
	}
	kept := ""
	if files {
		kept = ", volume kept"
	}
	return fmt.Sprintf("%s → %s at %v%s", from, to, s.GetFidelity(), kept), nil
}

// cleanup deletes what the suite created and is still there; it reports nothing.
func (r *run) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range r.made {
		_, _ = r.c.DeleteActor(ctx, r.cfg.Atespace, n)
	}
	for _, t := range r.tags {
		_, _ = r.c.DeleteTag(ctx, r.cfg.Atespace, t)
	}
}
