package substrateapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	ateapi "github.com/everdict/herd/api/ateapi/v1"
	ateenv "github.com/everdict/herd/api/ateenv/v1alpha"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The optional cases: an egress walk and a credential header measured from inside the guest, a long stream through
// the router, and a template with two containers sharing a volume. Each needs configuration a target may not have,
// and is a "not configured" skip without it.

// CredentialHeader is the request header the credential-header case has the target's gateway fill in.
const CredentialHeader = "X-Conformance-Credential"

// EchoRequest is one request the echo server saw.
type EchoRequest struct {
	// Host is the request's host, without a port; Nonce is its "cf" query parameter (the suite marks each request).
	Host, Nonce string
	// Credential is the CredentialHeader value, and HasCredential whether the header was there at all.
	Credential    string
	HasCredential bool
}

// Echo is the suite's echo server. It answers every request 200 "conformance-echo" and keeps what it saw for the
// suite, never in the answer, so the guest learns nothing from it (in particular not an injected credential).
type Echo struct {
	ln  net.Listener
	srv *http.Server

	mu   sync.Mutex
	seen []EchoRequest
}

// StartEcho serves an echo server on addr until Close.
func StartEcho(addr string) (*Echo, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	e := &Echo{ln: ln}
	e.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := req.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		v, has := req.Header[http.CanonicalHeaderKey(CredentialHeader)]
		rec := EchoRequest{Host: strings.ToLower(host), Nonce: req.URL.Query().Get("cf"), HasCredential: has}
		if has && len(v) > 0 {
			rec.Credential = v[0]
		}
		e.mu.Lock()
		e.seen = append(e.seen, rec)
		e.mu.Unlock()
		_, _ = w.Write([]byte("conformance-echo\n"))
	})}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// Addr is where the echo server listens.
func (e *Echo) Addr() string { return e.ln.Addr().String() }

// Close stops the echo server.
func (e *Echo) Close() error { return e.srv.Close() }

// Saw answers the request marked nonce, if the echo server saw one.
func (e *Echo) Saw(nonce string) (EchoRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.seen {
		if s.Nonce == nonce {
			return s, true
		}
	}
	return EchoRequest{}, false
}

func nonce() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// marked is raw with the query parameter cf=n, which the echo server records.
func marked(raw, n string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("cf", n)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ruleFor is the egress rule that lists raw's host and port: an http rule for an http:// URL, an https rule for an
// https:// one.
func ruleFor(raw string) (*ateapi.EgressRule, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, "", fmt.Errorf("URL %q names no host", raw)
	}
	var ports *ateapi.Ports
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, "", fmt.Errorf("URL %q: port: %w", raw, err)
		}
		ports = &ateapi.Ports{Numbers: []int32{int32(n)}}
	}
	switch u.Scheme {
	case "http":
		return &ateapi.EgressRule{Http: &ateapi.HttpRule{Hostnames: []string{host}, Ports: ports}}, host, nil
	case "https":
		return &ateapi.EgressRule{Https: &ateapi.HttpsRule{Hostnames: []string{host}, Ports: ports}}, host, nil
	}
	return nil, "", fmt.Errorf("URL %q: want http:// or https://", raw)
}

func (r *run) bound() time.Duration {
	if r.cfg.EgressBound > 0 {
		return r.cfg.EgressBound
	}
	return 15 * time.Second
}

// until runs f every half second (or Poll, if shorter) until it succeeds or bound passes, and answers how long it
// took or f's last error.
func (r *run) until(ctx context.Context, bound time.Duration, f func() error) (time.Duration, error) {
	start := time.Now()
	every := 500 * time.Millisecond
	if r.cfg.Poll < every {
		every = r.cfg.Poll
	}
	for {
		err := f()
		if err == nil {
			return time.Since(start), nil
		}
		if time.Since(start) > bound {
			return time.Since(start), err
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(every):
		}
	}
}

func (r *run) execProbe() (ExecProbe, error) {
	ep, ok := r.cfg.Probe.(ExecProbe)
	if !ok {
		return nil, notConfigured{fmt.Sprintf("needs a probe that runs commands in the guest (env-exec, sandbox-process), not %s", r.cfg.Probe.Name())}
	}
	return ep, nil
}

// wget runs wget in actor's guest: -q -O - with a 10 s timeout, each header as --header.
func (r *run) wget(ctx context.Context, ep ExecProbe, actor, u string, headers ...string) (ExecResult, error) {
	w := r.cfg.Wget
	if w == "" {
		w = "wget"
	}
	argv := []string{w, "-q", "-O", "-", "-T", "10"}
	for _, h := range headers {
		argv = append(argv, "--header", h)
	}
	return ep.Exec(ctx, r.cfg.Router, r.target(actor), append(argv, u)...)
}

// reach: wget of u from inside actor's guest answers (exit 0).
func (r *run) reach(ctx context.Context, ep ExecProbe, actor, u string, headers ...string) (ExecResult, error) {
	res, err := r.wget(ctx, ep, actor, u, headers...)
	if err != nil {
		return res, fmt.Errorf("wget %s: %w", u, err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("wget %s from the guest: exit %d: %s", u, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// refused: wget of u from inside actor's guest fails (a refused connection, an HTTP error, a timeout).
func (r *run) refused(ctx context.Context, ep ExecProbe, actor, u string) error {
	res, err := r.wget(ctx, ep, actor, u)
	if err != nil {
		return fmt.Errorf("wget %s: %w", u, err)
	}
	if res.ExitCode == 0 {
		return fmt.Errorf("wget %s from the guest answered: %q", u, trim(res.Stdout))
	}
	return nil
}

func trim(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// hostSide are the URLs the egress walk wants refused: the target's own endpoints, the link-local metadata address
// and a metadata name, and any the configuration adds.
func (r *run) hostSide() []string {
	out := []string{"http://" + r.cfg.Control.Addr + "/", "http://" + r.cfg.Router.Addr + "/",
		"http://169.254.169.254/latest/meta-data/", "http://metadata.google.internal/computeMetadata/v1/"}
	return append(out, r.cfg.HostSide...)
}

// egressWalk: an actor whose EgressPolicy lists one host reaches it from inside its guest and is refused an
// unlisted host (the suite's echo server, which must not see the request) and the host-side targets; an
// UpdateActorEgressPolicy that lists the echo host instead flips both on the live actor within the bound.
func (r *run) egressWalk(ctx context.Context) (string, error) {
	if r.cfg.EgressListed == "" || r.cfg.EchoURL == "" || r.cfg.Echo == nil {
		return "", notConfigured{"needs --egress-walk <listed URL>, --echo-listen and --echo-url"}
	}
	ep, err := r.execProbe()
	if err != nil {
		return "", err
	}
	listed, _, err := ruleFor(r.cfg.EgressListed)
	if err != nil {
		return "", err
	}
	echoRule, _, err := ruleFor(r.cfg.EchoURL)
	if err != nil {
		return "", err
	}
	name := r.name("egress")
	if _, err := r.newActor(ctx, name, nil, ""); err != nil {
		return "", err
	}
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	pol, err := r.c.CreateActorEgressPolicy(ctx, r.cfg.Atespace, name, &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{listed}})
	if err != nil {
		return "", gate("CreateActorEgressPolicy", err)
	}
	bound := r.bound()
	applied, err := r.until(ctx, bound, func() error { _, err := r.reach(ctx, ep, name, r.cfg.EgressListed); return err })
	if err != nil {
		return "", fmt.Errorf("the listed host, within %v of CreateActorEgressPolicy: %w", bound, err)
	}
	n := nonce()
	echoURL, err := marked(r.cfg.EchoURL, n)
	if err != nil {
		return "", err
	}
	if err := r.refused(ctx, ep, name, echoURL); err != nil {
		return "", fmt.Errorf("unlisted host: %w", err)
	}
	if _, saw := r.cfg.Echo.Saw(n); saw {
		return "", errors.New("the echo server saw a request from the guest while its host was unlisted")
	}
	hs := r.hostSide()
	for _, u := range hs {
		if err := r.refused(ctx, ep, name, u); err != nil {
			return "", fmt.Errorf("host-side target: %w", err)
		}
	}

	// The update lists the echo host and drops the listed one, on the running actor.
	upd := proto.CloneOf(pol)
	upd.Rules = []*ateapi.EgressRule{echoRule}
	if _, err := r.c.UpdateActorEgressPolicy(ctx, r.cfg.Atespace, name, upd); err != nil {
		return "", fmt.Errorf("UpdateActorEgressPolicy: %w", err)
	}
	flipped, err := r.until(ctx, bound, func() error {
		n := nonce()
		u, err := marked(r.cfg.EchoURL, n)
		if err != nil {
			return err
		}
		if _, err := r.reach(ctx, ep, name, u); err != nil {
			return fmt.Errorf("newly listed echo host: %w", err)
		}
		if _, saw := r.cfg.Echo.Saw(n); !saw {
			return errors.New("the guest's wget of the echo URL answered, but the echo server saw no such request")
		}
		if err := r.refused(ctx, ep, name, r.cfg.EgressListed); err != nil {
			return fmt.Errorf("host the update unlisted: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("within %v of UpdateActorEgressPolicy: %w", bound, err)
	}
	if err := r.deleteActor(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("listed %s answered (policy applied in %v); unlisted echo host and %d host-side targets refused; the update flipped both in %v (bound %v)",
		r.cfg.EgressListed, applied.Round(time.Millisecond), len(hs), flipped.Round(time.Millisecond), bound), nil
}

// credentialHeader: the echo host is listed with a replace_headers effect; the guest's request carrying a
// placeholder reaches the echo server with the credential in its place, and the guest sees the credential neither
// in the answer nor in its environment. A request without the header gets none.
func (r *run) credentialHeader(ctx context.Context) (string, error) {
	if r.cfg.CredentialURI == "" || r.cfg.Credential == "" || r.cfg.EchoURL == "" || r.cfg.Echo == nil {
		return "", notConfigured{"needs --credential-uri, --credential-file, --echo-listen and --echo-url"}
	}
	ep, err := r.execProbe()
	if err != nil {
		return "", err
	}
	rule, _, err := ruleFor(r.cfg.EchoURL)
	if err != nil {
		return "", err
	}
	fx := &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{{Header: CredentialHeader, CredentialUri: r.cfg.CredentialURI}}}
	if rule.Http != nil {
		rule.Http.Effects = fx
	} else {
		rule.Https.Effects = fx
	}
	name := r.name("cred")
	if _, err := r.newActor(ctx, name, nil, ""); err != nil {
		return "", err
	}
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	if _, err := r.c.CreateActorEgressPolicy(ctx, r.cfg.Atespace, name, &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{rule}}); err != nil {
		return "", gate("CreateActorEgressPolicy", err)
	}
	const placeholder = "conformance-placeholder"
	var seen EchoRequest
	var answer ExecResult
	bound := r.bound()
	if _, err := r.until(ctx, bound, func() error {
		n := nonce()
		u, err := marked(r.cfg.EchoURL, n)
		if err != nil {
			return err
		}
		if answer, err = r.reach(ctx, ep, name, u, CredentialHeader+": "+placeholder); err != nil {
			return err
		}
		var saw bool
		if seen, saw = r.cfg.Echo.Saw(n); !saw {
			return errors.New("the guest's wget answered, but the echo server saw no such request")
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("the listed echo host, within %v of CreateActorEgressPolicy: %w", bound, err)
	}
	switch {
	case !seen.HasCredential:
		return "", fmt.Errorf("the echo server saw no %s header", CredentialHeader)
	case seen.Credential == placeholder:
		return "", fmt.Errorf("the echo server saw the guest's placeholder in %s: nothing was injected", CredentialHeader)
	case seen.Credential != r.cfg.Credential:
		// The value is not printed: it may be a near miss of the secret.
		return "", fmt.Errorf("the echo server saw %d bytes in %s that are not the credential", len(seen.Credential), CredentialHeader)
	}
	if strings.Contains(answer.Stdout+answer.Stderr, r.cfg.Credential) {
		return "", errors.New("the guest read the credential in the answer to its request")
	}
	env, err := ep.Exec(ctx, r.cfg.Router, r.target(name), "env")
	if err != nil {
		return "", fmt.Errorf("env in the guest: %w", err)
	}
	if strings.Contains(env.Stdout, r.cfg.Credential) {
		return "", errors.New("the guest holds the credential in its environment")
	}
	n := nonce()
	u, err := marked(r.cfg.EchoURL, n)
	if err != nil {
		return "", err
	}
	if _, err := r.reach(ctx, ep, name, u); err != nil {
		return "", err
	}
	if s, _ := r.cfg.Echo.Saw(n); s.HasCredential {
		return "", fmt.Errorf("a request without %s reached the echo server with one", CredentialHeader)
	}
	if err := r.deleteActor(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("the echo server saw the credential in %s in place of the guest's placeholder; the guest's answer and environment hold none", CredentialHeader), nil
}

func (r *run) deleteActor(ctx context.Context, name string) error {
	if _, err := r.c.DeleteActor(ctx, r.cfg.Atespace, name); err != nil {
		return fmt.Errorf("DeleteActor %s: %w", name, err)
	}
	return r.waitGone(ctx, name)
}

// StreamProbe is a probe that can start a process and follow its output by the process id, from an offset:
// env's StartProcess and StreamProcessOutput.
type StreamProbe interface {
	Probe
	Start(ctx context.Context, r Router, actor string, argv ...string) (string, error)
	// Follow streams process id's stdout from offset to each until the stream ends, and answers the exit code if the
	// stream carried the exit. A stream that ends with an error answers it.
	Follow(ctx context.Context, r Router, actor, id string, offset int64, each func(stdout []byte)) (*int, error)
}

// Start implements StreamProbe.
func (e EnvExec) Start(ctx context.Context, r Router, actor string, argv ...string) (string, error) {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return "", err
	}
	defer cc.Close()
	p := &ateenv.Process{}
	if err := cc.Invoke(ctx, envStartProcess, &ateenv.StartProcessRequest{Command: argv}, p); err != nil {
		return "", fmt.Errorf("StartProcess: %w", err)
	}
	return p.GetProcessId(), nil
}

// Follow implements StreamProbe.
func (e EnvExec) Follow(ctx context.Context, r Router, actor, id string, offset int64, each func([]byte)) (*int, error) {
	cc, ctx, err := e.conn(ctx, r, actor)
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	exit, err := e.follow(ctx, cc, id, offset, func(m *ateenv.ProcessOutput) {
		if len(m.GetStdout()) > 0 {
			each(m.GetStdout())
		}
	})
	if exit == nil {
		return nil, err
	}
	code := int(exit.GetExitCode())
	return &code, err
}

func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }

// longStream: a process whose output comes StreamGap apart is followed through the router for StreamFor; the
// stream stays open across every idle gap. A suspend then cuts it with a status, and after the resume a re-attach by
// the process id from the offset read so far reads the rest to the exit, with nothing lost or repeated.
func (r *run) longStream(ctx context.Context) (string, error) {
	if !r.cfg.LongStream {
		return "", notConfigured{"needs --long-stream"}
	}
	sp, ok := r.cfg.Probe.(StreamProbe)
	if !ok {
		return "", notConfigured{fmt.Sprintf("needs a probe that follows a process's output by id (env-exec), not %s", r.cfg.Probe.Name())}
	}
	hold, gap := r.cfg.StreamFor, r.cfg.StreamGap
	if hold <= 0 {
		hold = 30 * time.Minute
	}
	if gap <= 0 {
		gap = time.Minute
	}
	ticks := max(int(hold/gap), 2)
	// The tail outlasts the suspend: the stream is still open when the snapshot cuts it.
	tailGap := min(gap, time.Second)
	const tails = 20
	script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo tick $i; i=$((i+1)); sleep %s; done; `+
		`i=0; while [ $i -lt %d ]; do echo tail $i; i=$((i+1)); sleep %s; done; echo end`, ticks, seconds(gap), tails, seconds(tailGap))
	var want strings.Builder
	for i := range ticks {
		fmt.Fprintf(&want, "tick %d\n", i)
	}
	lastTick := fmt.Sprintf("tick %d\n", ticks-1)
	for i := range tails {
		fmt.Fprintf(&want, "tail %d\n", i)
	}
	want.WriteString("end\n")

	name := r.name("stream")
	if _, err := r.newActor(ctx, name, nil, ""); err != nil {
		return "", err
	}
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	id, err := sp.Start(ctx, r.cfg.Router, r.target(name), "/bin/sh", "-c", script)
	if err != nil {
		return "", err
	}
	var mu sync.Mutex
	var got bytes.Buffer
	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return got.String()
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type end struct {
		exit *int
		err  error
	}
	ended := make(chan end, 1)
	start := time.Now()
	go func() {
		exit, err := sp.Follow(sctx, r.cfg.Router, r.target(name), id, 0, func(b []byte) {
			mu.Lock()
			got.Write(b)
			mu.Unlock()
		})
		ended <- end{exit, err}
	}()
	// Hold the stream for the whole duration, then until the last tick is in.
	early := func(e end) error {
		return fmt.Errorf("the stream ended after %v of %v (exit %v): %v; output so far %q", time.Since(start).Round(time.Millisecond), hold, e.exit, e.err, trim(output()))
	}
	select {
	case e := <-ended:
		return "", early(e)
	case <-time.After(hold):
	}
	deadline := time.After(2 * gap)
	for !strings.Contains(output(), lastTick) {
		select {
		case e := <-ended:
			return "", early(e)
		case <-deadline:
			return "", fmt.Errorf("after %v the stream had %q, want through %q", hold, trim(output()), strings.TrimSpace(lastTick))
		case <-time.After(r.cfg.Poll):
		}
	}
	if out := output(); !strings.HasPrefix(want.String(), out) {
		return "", fmt.Errorf("the stream delivered %q, which is not how the output starts", trim(out))
	}
	// A suspend cuts the open stream, and says why.
	_, snap, err := r.suspendDurable(ctx, name)
	if err != nil {
		return "", err
	}
	var e end
	select {
	case e = <-ended:
	case <-time.After(r.cfg.Wait):
		return "", errors.New("the stream stayed open across the suspend")
	}
	if e.err == nil {
		return "", fmt.Errorf("at the suspend the stream ended cleanly (exit %v); want a cut with a status", e.exit)
	}
	st, ok := status.FromError(e.err)
	if !ok || st.Code() == codes.OK || st.Message() == "" {
		return "", fmt.Errorf("at the suspend the stream ended with %v; want a gRPC status with a reason", e.err)
	}
	cut := fmt.Sprintf("%v %q", st.Code(), st.Message())
	offset := len(output())
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	if snap.GetFidelity() < ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		if err := r.deleteActor(ctx, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("held %v (%d outputs %v apart) through the router; the suspend cut it: %s; re-attach not checked: a %v suspend keeps no processes",
			hold, ticks, gap, cut, snap.GetFidelity()), nil
	}
	var rest bytes.Buffer
	exit, err := sp.Follow(ctx, r.cfg.Router, r.target(name), id, int64(offset), func(b []byte) { rest.Write(b) })
	if err != nil {
		return "", fmt.Errorf("re-attaching to process %s at offset %d after the resume: %w", id, offset, err)
	}
	if exit == nil || *exit != 0 {
		return "", fmt.Errorf("re-attached to process %s: exit %v, want 0", id, exit)
	}
	if all := output() + rest.String(); all != want.String() {
		return "", fmt.Errorf("before the cut and after the re-attach the process wrote %q, want %q", trim(all), trim(want.String()))
	}
	if err := r.deleteActor(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("held %v (%d outputs %v apart) through the router; the suspend cut it: %s; re-attached by id at offset %d after the resume and read the rest to exit 0",
		hold, ticks, gap, cut, offset), nil
}

// shared finds two containers of t that mount one DurableDir volume, and where each mounts it.
func shared(t *ateapi.ActorTemplate) (c1, c2, p1, p2 string, err error) {
	durable := map[string]bool{}
	for _, v := range t.GetVolumes() {
		if v.GetDurableDir() != nil {
			durable[v.GetName()] = true
		}
	}
	for i, a := range t.GetContainers() {
		for _, b := range t.GetContainers()[i+1:] {
			for _, ma := range a.GetVolumeMounts() {
				for _, mb := range b.GetVolumeMounts() {
					if ma.GetName() == mb.GetName() && durable[ma.GetName()] {
						return a.GetName(), b.GetName(), ma.GetMountPath(), mb.GetMountPath(), nil
					}
				}
			}
		}
	}
	return "", "", "", "", fmt.Errorf("template %s has no two containers mounting one DurableDir volume", t.GetMetadata().GetName())
}

// twoContainers: in an actor of a template whose two containers mount one DurableDir volume, each container reads
// what the other wrote there — at once, after a suspend and resume, and (with --move-from/--move-to) after a move to
// another worker pool.
func (r *run) twoContainers(ctx context.Context) (string, error) {
	if r.cfg.Template2 == "" {
		return "", notConfigured{"needs --template2 (two containers sharing a DurableDir volume)"}
	}
	if r.cfg.ContainerHeader == "" {
		return "", notConfigured{"needs --container-header: env's guest API names no container, so the case needs the key the target's guest picks one by"}
	}
	cp, ok := r.cfg.Probe.(ContainerProbe)
	if !ok {
		return "", notConfigured{fmt.Sprintf("needs a file probe that can address a container (env-exec), not %s", r.cfg.Probe.Name())}
	}
	tpl, err := r.c.GetActorTemplate(ctx, r.cfg.Atespace, r.cfg.Template2)
	if err != nil {
		return "", fmt.Errorf("GetActorTemplate %s: %w", r.cfg.Template2, err)
	}
	c1, c2, p1, p2, err := shared(tpl)
	if err != nil {
		return "", err
	}
	in := map[string]FileProbe{c1: cp.InContainer(r.cfg.ContainerHeader, c1), c2: cp.InContainer(r.cfg.ContainerHeader, c2)}
	mount := map[string]string{c1: p1, c2: p2}
	name := r.name("two")
	move := len(r.cfg.MoveFrom) > 0 && len(r.cfg.MoveTo) > 0
	var sel map[string]string
	if move {
		sel = r.cfg.MoveFrom
	}
	if _, err := r.newActorOf(ctx, r.cfg.Template2, name, sel, ""); err != nil {
		return "", err
	}
	a, err := r.resumeRunning(ctx, name)
	if err != nil {
		return "", err
	}
	file := func(writer, reader string) string { return path.Join(mount[reader], "cf-from-"+writer+".txt") }
	// write: each container writes its file in the volume, and the other reads it.
	write := func(round string) error {
		for _, w := range [][2]string{{c1, c2}, {c2, c1}} {
			body := round + " by " + w[0]
			if err := in[w[0]].WriteFile(ctx, r.cfg.Router, r.target(name), file(w[0], w[0]), body); err != nil {
				return fmt.Errorf("%s: container %s writing %s: %w", round, w[0], file(w[0], w[0]), err)
			}
			if got, err := in[w[1]].ReadFile(ctx, r.cfg.Router, r.target(name), file(w[0], w[1])); err != nil || got != body {
				return fmt.Errorf("%s: container %s reads %q, %v at %s; want what %s wrote, %q", round, w[1], got, err, file(w[0], w[1]), w[0], body)
			}
		}
		return nil
	}
	// kept: both containers still read both files of round.
	kept := func(round, after string) error {
		for _, writer := range []string{c1, c2} {
			for _, reader := range []string{c1, c2} {
				want := round + " by " + writer
				if got, err := in[reader].ReadFile(ctx, r.cfg.Router, r.target(name), file(writer, reader)); err != nil || got != want {
					return fmt.Errorf("after %s container %s reads %q, %v at %s; want %q", after, reader, got, err, file(writer, reader), want)
				}
			}
		}
		return nil
	}
	if err := write("first"); err != nil {
		return "", err
	}
	sa, snap, err := r.suspendDurable(ctx, name)
	if err != nil {
		return "", err
	}
	first := snap.GetFidelity()
	if _, err := r.resumeRunning(ctx, name); err != nil {
		return "", err
	}
	if err := kept("first", "the "+snap.GetFidelity().String()+" suspend and resume"); err != nil {
		return "", err
	}
	if err := write("resumed"); err != nil {
		return "", err
	}
	moved := ""
	if move {
		from := a.GetStatus().GetAssignedNode()
		if sa, snap, err = r.suspendDurable(ctx, name); err != nil {
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
		if err := kept("resumed", "the move"); err != nil {
			return "", err
		}
		if err := write("moved"); err != nil {
			return "", err
		}
		moved = fmt.Sprintf(", and after a %v move %s → %s", snap.GetFidelity(), from, to)
	}
	if err := r.deleteActor(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%s) and %s (%s) read each other's writes, before and after a %v suspend/resume%s", c1, p1, c2, p2, first, moved), nil
}
