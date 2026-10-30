# Conformance: one Substrate-API client suite for Substrate and herd

`herd conformance` runs the suite in `conformance/substrateapi` (spec R8, R-D2) against one target. It is a plain
client of Substrate's `Control` API (`substrate.ateapi.v1`, the subset herd pins in ADR 0001 A-4) and of the target's
actor ingress (the `ate-target-actor: <atespace>/<actor>` header atenet-router and herd-router both read). It imports
no herd package besides the two API definitions and names no herd-only method or header
(`TestSuiteAssumesNothingHerdSpecific`), so a green run means the same thing on Substrate and on herd. Everdict's
herder (PLT-107) runs the same command.

## What it checks

| case | what passes |
|---|---|
| `create-from-template` | `CreateActor{actor_template}` answers the actor with a uid, in SUSPENDED (Substrate) or already RESUMING/RUNNING |
| `resume` | `ResumeActor` brings it to RUNNING with a worker assignment |
| `request` | the probe's request through the router is answered (and, with `--volume-path`, a file is written in the volume) |
| `suspend` | `SuspendActor{options.mode: DURABILITY_MODE_SYNCHRONOUS}` returns with the actor SUSPENDED, no worker, and its latest snapshot already COMPLETED (in the answer or a GetActor right after; no waiting), owned by the actor, at a fidelity from VOLUMES up to the template's `preferred_fidelity` (exactly `--expect-fidelity` when given). Every suspend the lifecycle and fork cases make is SYNCHRONOUS. |
| `resume-on-request` | a request to the suspended actor through the router resumes it: answered, and the actor is RUNNING (the volume file is still there) |
| `fork-via-tag` | `CreateTag{source_actor, TAG_SCOPE_ATESPACE}` of the suspended parent becomes COMPLETED under the template's uid; two actors from `CreateActor{actor_template, source_tag}` resume and answer, read the parent's volume, write their own; suspending and deleting one leaves the other RUNNING and answering with its own write; the parent keeps its uid and snapshot generation, and resumes and answers with its own volume |
| `delete-verified` | after `DeleteActor`, `GetActor` answers NotFound (polled, bounded by `--wait`) and `ListActors` omits it |
| `refuse-stale-version` | `UpdateActor` at the current uid+version is accepted and moves the version; at the stale version, or another uid, it is refused `Aborted` |
| `suspend-async` | `SuspendActor{options.mode: DURABILITY_MODE_ASYNCHRONOUS}` of a running actor answers SUSPENDING or SUSPENDED; the actor becomes SUSPENDED with no worker and a snapshot of its own at an allowed fidelity. Durability is not asserted: the mode does not promise it at return. |
| `refuse-unknown-actor` | `GetActor`, `SuspendActor`, `ResumeActor`, `DeleteActor` of an actor that does not exist answer `NotFound` |
| `move` | with `--move-from` and `--move-to` (worker selectors of two pools): an actor suspended on the first resumes on the second after an `UpdateActor` of its `worker_selector`, answers, and keeps its volume file |
| `egress-walk` | optional, below: from inside the guest a listed host answers, an unlisted one and host-side targets are refused, and an `UpdateActorEgressPolicy` flips them on the live actor within a bound |
| `credential-header` | optional, below: a `replace_headers` credential reaches the suite's echo server in place of the guest's placeholder; the guest never holds it |
| `long-stream` | optional, below: a streamed exec stays open through the router for `--stream-for` across idle gaps; a suspend cuts it with a status; after the resume a re-attach by exec id reads the rest |
| `two-containers` | optional, below: two containers of one template sharing a DurableDir volume read each other's writes, before and after suspend/resume and a move |

The four optional cases each run only when their flags are given, and are otherwise a `not configured` skip (like
`move`), so a run without them stays green. They assume only the Control API, Substrate's `EgressPolicy` and env's
guest API, so they run against Substrate too wherever it has the feature.

A case is skipped only when the server declared why: it answered the call `Unimplemented` with a reason, which the
report prints. `Unimplemented` with no reason, or any other refusal, fails the case. The one other skip is `move`
without `--move-from/--move-to` (a target with one worker pool), printed as `not configured`. The report ends with the
target's skip list and `N passed, M failed, K skipped`; the command exits 1 unless the run is green. Every name the
suite creates starts with `--prefix` (default `cf-<random>`) and is deleted at the end.

## Optional cases

They need a probe that runs commands in the guest (`env-exec`, or `sandbox-process` for the egress cases) and, for the
egress cases, `wget` in the guest image (busybox's; `--wget` names another). Every wget is `wget -q -O - -T 10
[--header h] URL`; it "answers" when it exits 0 and is "refused" when it exits non-zero, which is how a refusal looks from
inside the guest whatever the gateway did (an HTTP 403, a closed connection, a timeout).

**The echo server.** `--echo-listen <addr>` makes the suite serve a tiny HTTP server on the machine it runs on. It
answers every request `200 conformance-echo` and keeps, for the suite only, each request's host, its `cf` query mark
and its `X-Conformance-Credential` header; it never echoes a header back, so the guest learns nothing from it.
`--echo-url` is the `http://` URL a guest reaches it at **through the target's egress**: a DNS name the target's
gateway resolves to the suite's machine, at an address the gateway does not count host-side (herd-egress refuses
loopback, private, link-local, CGNAT and metadata addresses whatever the policy says, so on a LAN the suite must run
where the gateway sees a public address, or behind a port forward that has one, or the operator opens exactly the echo
host's address with herd's lab allowance: [A lab echo host without a public address](#a-lab-echo-host-without-a-public-address)).

**`egress-walk`** (`--egress-walk <URL>`, with `--echo-listen`, `--echo-url`; `--egress-bound`, default 15 s;
`--host-side u1,u2` adds targets). The suite creates an actor, resumes it, and creates its `EgressPolicy` listing only
the `--egress-walk` URL's host (an `http` rule for `http://`, an `https` rule for `https://`, with the URL's port).
Then, from inside the guest: the listed URL answers within the bound (that is how long the policy took to apply); the
echo URL is refused and the echo server saw no such request; and the host-side targets are refused: the Control and
router endpoints (`http://<--endpoint>/`, `http://<--router>/`), `http://169.254.169.254/latest/meta-data/` and
`http://metadata.google.internal/computeMetadata/v1/`. Give `--endpoint` and `--router` as addresses the guest would
route to the host (not 127.0.0.1, which is the guest itself), or add the host's own address with `--host-side`. An
`UpdateActorEgressPolicy` on the running actor then lists the echo host instead: within the bound the echo URL answers
(and the echo server saw the request) and the formerly listed URL is refused. The report gives both times. With
herd-egress bypassed the case is red (`TestEgressCasesRedWhenEgressBypassed`: the unlisted echo host answers).

**`credential-header`** (`--credential-uri ate-secret://...`, `--credential-file <file holding the value it resolves
to>`, `--echo-listen`, `--echo-url`). The policy lists the echo host with `effects.replace_headers:
[{header: X-Conformance-Credential, credential_uri}]`. The guest sends `X-Conformance-Credential:
conformance-placeholder`; the echo server must see exactly the credential in its place (not the placeholder, and the
value is never printed). The guest's answer and its `env` must not contain the credential, and a request without the
header reaches the echo server without one. herd-egress resolves `ate-secret://dir/<name>/<tail>` from its `-secrets`
directory (`herd-egress -secrets` on a joined host, `herd -secrets` for an all-in-one, which hands it to its
herd-egress). Without injection the case is red (the echo server sees the placeholder).

**`long-stream`** (`--long-stream`; `--stream-for`, default 30m; `--stream-gap`, default 60s). The suite starts
`/bin/sh -c` printing `tick i` every `--stream-gap` for `--stream-for`, then 20 `tail i` lines up to 1 s apart and
`end`, and follows its output (env's `StreamProcessOutput`, `follow: true`) through the router. The stream must stay
open for the whole duration, across every idle gap, and deliver every tick in order. A `SuspendActor`
(SYNCHRONOUS) then must end the open stream with a gRPC status that carries a reason (a clean end fails). After
`ResumeActor`, a new `StreamProcessOutput` for the same process id from the byte offset already read must read the
rest to exit 0, so that the two halves are exactly the output. A suspend below MEMORY keeps no process, so there the
re-attach is not checked and the report says so. Under a router path that cuts idle connections the case is red
(`TestLongStreamRedUnderRouterIdleTimeout`). `go test` runs it for 1.5 s with 300 ms gaps; a real run uses the
defaults.

**`two-containers`** (`--template2 <template>`, `--container-header <key>`). The template must have two containers
that mount one DurableDir volume (at any mount paths); the suite finds them. env's guest API has no way to name a
container, so the case needs the request metadata key by which the target's guest picks one (herd-guest:
`x-herd-container`); without it the case is a `not configured` skip. Each container writes a file in the volume
(env's `WriteFile`) and the other must read it (`ReadFile`) at its own mount path; after a SYNCHRONOUS suspend and a
resume both still read both files and write again; with `--move-from`/`--move-to` the actor starts in the first pool
and, after a suspend and an `UpdateActor` of its `worker_selector`, must resume in the other with both files and write
again. If each container got its own copy of the volume, the case is red.

## Probes

The router request depends on what the template's containers serve:

- `http-get:<path>`: `GET <path>`, any 2xx passes — Substrate's counter demo (`http-get:/`).
- `sandbox-process`: Substrate's sandbox demo, `POST /process {"command": ["sh", "-c", "echo conformance"]}`; it can
  also write and read files, so `--volume-path` works with a template that has a volume.
- `env-exec`: agent-substrate/env's guest API over gRPC (`ateenv.v1alpha.ProcessService`, `FileSystemService`),
  which herd-guest serves on herd-router's default port.

## Against Substrate on kind (5e224278)

Verified by the supervisor's run against Substrate 5e224278 on kind (2026-10-30): the commands below reached the
Control API and atenet-router, the router client certificate minted as shown was accepted, and atenet-router's HTTPS
listener served the `http-get` probe. That run also showed why the suite names its durability mode: SuspendActor with
no `options.mode` is `DURABILITY_MODE_ASYNCHRONOUS` on Substrate, and the snapshot stayed `PENDING` for minutes; the
`suspend` case now sends `DURABILITY_MODE_SYNCHRONOUS` (and asserts the snapshot COMPLETED at return), `suspend-async`
sends `ASYNCHRONOUS` and asserts the suspend only.

Substrate's Control API (Service `api`, namespace `ate-system`) serves TLS with a certificate for
`api.ate-system.svc` from the `servicedns.podcert.ate.dev/identity` signer, and takes a bearer token minted for the
`ate-client` ServiceAccount with that audience. atenet-router (Service `atenet-router`, port 443 → 8443) serves TLS for
`atenet-router.ate-system.svc` and requires a client certificate whose SPIFFE ID it allows: the one kubectl-ate gets
with a PodCertificateRequest for the `ate-system/atenet-client` ServiceAccount. The counter demo (`deploy demo
counter`) gives atespace `ate-demo-counter` and template `counter` (preferred MEMORY, volume `data` at
`/home/counter`, HTTP on port 80); its pool must have two free workers, since an actor occupies a whole worker and the
fork case runs two forks at once.

The router client certificate comes from Substrate's own `internal/podidentity`, so the program that mints it lives
in the Substrate checkout (an `internal` package is importable only from inside that module), e.g. as
`cmd/mint-router-cert/main.go`:

```go
package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log"
	"os"
	"time"

	"go.ate.dev/substrate/internal/podidentity"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := podidentity.RequestCertificate(context.Background(), cs, "kubectl-ate", "ate-system", "atenet-client", 2*time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	var crt []byte
	for _, der := range cert.Certificate {
		crt = append(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[1], crt, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[2], pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Printf("router client cert until %v", cert.Leaf.NotAfter)
}
```

```sh
(cd substrate && go run ./cmd/mint-router-cert /tmp/atenet-client.crt /tmp/atenet-client.key)
kubectl -n ate-system port-forward svc/api 7443:443 &
kubectl -n ate-system port-forward svc/atenet-router 8443:443 &
kubectl -n ate-system create token ate-client --audience api.ate-system.svc > /tmp/ate.token
kubectl get clustertrustbundles -l podcert.ate.dev/canarying=live -o jsonpath='{range .items[?(@.spec.signerName=="servicedns.podcert.ate.dev/identity")]}{.spec.trustBundle}{end}' > /tmp/ate-ca.pem
herd conformance --target substrate-kind --endpoint 127.0.0.1:7443 --tls --tls-server-name api.ate-system.svc --ca-file /tmp/ate-ca.pem --token-file /tmp/ate.token --atespace ate-demo-counter --template counter --router 127.0.0.1:8443 --router-tls --router-tls-server-name atenet-router.ate-system.svc --router-cert-file /tmp/atenet-client.crt --router-key-file /tmp/atenet-client.key --probe http-get:/
```

The certificate is short-lived (the run above got one valid for a day); mint it again before a later run. The
sandbox demo is the same invocation with `--atespace ate-demo-sandbox --template sandbox-template --probe
sandbox-process`. Known Substrate facts the suite follows: CreateActor records the actor SUSPENDED and needs
`actor_template` even with a `source_tag`; SuspendActor's mode defaults to ASYNCHRONOUS; a suspend cuts in-flight
requests (the suite sends none across one); admission is per worker slot.

## Against herd

herd-api speaks cleartext gRPC and herd-router cleartext HTTP/1.1 and h2c; one bearer token (from `herd-api token`)
serves both. Create the template first (any Control client; `--template` must exist), then:

```sh
herd-api token -key-file /var/lib/herd/jwt.key -sub conformance -space conformance=write > /tmp/herd.token
herd conformance --target herd-all-in-one --endpoint 127.0.0.1:7443 --token-file /tmp/herd.token --atespace conformance --template agent --router 127.0.0.1:8443 --probe env-exec --volume-path /work/conformance.txt --expect-fidelity MEMORY
herd conformance --target herd-join --endpoint herd-api.internal:7443 --token-file /tmp/herd.token --atespace conformance --template agent --router herd-api.internal:8443 --probe env-exec --volume-path /work/conformance.txt --expect-fidelity VOLUMES --move-from pool=mac --move-to pool=ubuntu
```

The optional cases on a real host (here herd-api and herd-router on the host's LAN address; the template `pair` has two containers mounting one DurableDir volume; the
secret file sits in herd-egress's `-secrets` directory as `conformance/token`, and `/tmp/cred` holds the same value).
The all-in-one takes that directory as its own `-secrets` and hands it to its herd-egress, so it runs as:

```sh
sudo install -d -m 0700 /etc/herd/secrets/conformance
sudo install -m 0600 /tmp/cred /etc/herd/secrets/conformance/token
sudo herd -data /var/lib/herd -listen 192.168.1.10:7443 -router-listen 192.168.1.10:8443 -secrets /etc/herd/secrets
herd conformance --target herd-all-in-one --endpoint 192.168.1.10:7443 --token-file /tmp/herd.token --atespace conformance --template agent --router 192.168.1.10:8443 --probe env-exec \
  --egress-walk https://example.com/ --echo-listen :18080 --echo-url http://echo.example.net:18080/ \
  --credential-uri ate-secret://dir/conformance/token --credential-file /tmp/cred \
  --long-stream --stream-for 30m --stream-gap 60s --template2 pair --container-header x-herd-container
```

The egress cases need the VM's network to go through herd-egress: on a Mac that is the in-process gateway (ADR A-21);
on Ubuntu it is the VM's TAP and nftables table (A-18), which the Cloud Hypervisor backend must have attached for the
listed host to answer at all.

### A lab echo host without a public address

On a lab host the machine running the suite often has no public address, only a private or tailnet one (here
`100.113.143.93`, in the shared space `100.64.0.0/10`), which herd-egress refuses as host-side whatever the policy
lists. Two things make it reachable, and neither weakens the rule that a policy names hosts:

1. **A DNS name for the echo host.** An `EgressPolicy` rule takes host names, never addresses (`--echo-url
   http://100.113.143.93:18080/` fails at `CreateActorEgressPolicy` with `an IP address is not a hostname`, and a
   guest's request to an address is refused by herd-egress all the same). The name must resolve **where the gateway
   runs** (herd-egress on Linux, the herdlet on a Mac): the guest's DNS is herd-egress's synthetic one, and the gateway
   resolves and dials the listed name itself. Either use a wildcard DNS that encodes the address,
   `100.113.143.93.sslip.io` (the gateway host needs public DNS), or add a hosts entry on the gateway host:
   `echo 100.113.143.93 echo.lab.example | sudo tee -a /etc/hosts`.
2. **The lab allowance.** `-egress-allow-prefix 100.113.143.93/32` (repeatable) on `herd` (all-in-one: it hands it
   to its herd-egress and its herdlet), on `herd-egress` and on `herdlet` (a joined Linux host runs both: give both the
   flag, herd-egress to enforce it, the herdlet to report it; a Mac's herdlet enforces it in its in-process gateway).
   It opens exactly the written prefix inside a default denied prefix and nothing else: `100.113.143.94` and the rest
   of `100.64.0.0/10` stay refused, and so do the host's own addresses (`-host-addr`), the Control and router
   endpoints and the metadata endpoints. A prefix that would open more than written is refused at start: one with
   host bits set (`100.113.143.93/10`), one containing a whole default prefix (`100.64.0.0/10`, `0.0.0.0/0`), one
   containing a metadata endpoint (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`), or one in no default
   prefix (nothing to open). Each component logs a set allowance at start (`WARNING: LAB EGRESS ALLOWANCE
   -egress-allow-prefix ...`), the herdlet reports it in its worker capabilities (`herd.ext.v1.WorkerCapabilities
   .egress_allow_prefixes`, shown by `herd-api workers`), and `herd conformance` opens its report with it:

```text
egress allowances: LAB -egress-allow-prefix 100.113.143.93/32 on worker aio (host-side addresses herd-egress does not refuse)
```

A run without one says `egress allowances: none on N workers`, and against Substrate `egress allowances: not
reported by the target (Unimplemented)`. A report whose egress cases passed under an allowance is a lab run, not a
production configuration. The run then is:

```sh
echo 100.113.143.93 echo.lab.example | sudo tee -a /etc/hosts   # or use 100.113.143.93.sslip.io below
sudo herd -data /var/lib/herd -listen 192.168.1.10:7443 -router-listen 192.168.1.10:8443 -secrets /etc/herd/secrets \
  -egress-allow-prefix 100.113.143.93/32
herd conformance --target herd-all-in-one-lab --endpoint 192.168.1.10:7443 --token-file /tmp/herd.token --atespace conformance --template agent --router 192.168.1.10:8443 --probe env-exec \
  --egress-walk https://example.com/ --echo-listen 100.113.143.93:18080 --echo-url http://echo.lab.example:18080/ \
  --credential-uri ate-secret://dir/conformance/token --credential-file /tmp/cred
```

`TestEgressCasesThroughLabAllowPrefix` runs the egress walk and the credential header this way through the
all-in-one's own herd-egress: the test's resolver maps `echo.lab.test` to `100.113.143.93` (its dialer reaches that
address on loopback) and the all-in-one starts with `-egress-allow-prefix 100.113.143.93/32`; both pass and the
report opens with the allowance. `TestEgressCasesRedWithoutLabAllowPrefix` is the same run without the flag: both
cases fail with the echo host refused (403) although listed, and the header says no allowance was set.

`go test ./conformance/substrateapi/` runs exactly this against herd all-in-one and against the join mode with two
herdlets of different families (a VZ host declaring VOLUMES and a Cloud Hypervisor host declaring MEMORY), on the fake
hypervisor with the real herd-guest in every VM, and once more through `herd conformance` itself. In the all-in-one run every optional case is configured: the fake VMs' processes reach out only through
herd-egress's gateway (each VM gets an explicit-proxy listener whose connections the gateway serves as the VM's actor,
and the test binary is the guest's `wget`), the gateway's policies come from herd-api over the Control API as
herd-egress reads them, and its resolver maps `listed.test`/`echo.test` to TEST-NET-3 addresses that its dialer
reaches on loopback, so its Guard is the real host's. `TestCredentialHeaderThroughAllInOneEgress` runs the egress walk and the
credential header through the all-in-one's own herd-egress instead, started with `-secrets` (only its resolver
and dialer are the test's; the herdlet leases each VM a TAP link through a fake ip(8)/nft(8) and binds the VM's address to its actor in herd-egress's bindings file itself, as on a Linux host); without the directory
handed to herd-egress the credential header is red (the gateway refuses the request whose credential it cannot
resolve). `TestRealHostTAPEgress` (HERD_TAP_REAL=1, root, KVM) runs the same two cases from real Cloud Hypervisor guests on TAPs with real nftables tables; the command is in its doc comment. The join run adds `two-containers` with the VOLUMES move. The Nomad mode runs
the same binaries as the join mode (dist/nomad), so its run is the join invocation against the Nomad service
addresses; it needs the supervisor's cluster.
