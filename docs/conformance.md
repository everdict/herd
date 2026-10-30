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

A case is skipped only when the server declared why: it answered the call `Unimplemented` with a reason, which the
report prints. `Unimplemented` with no reason, or any other refusal, fails the case. The one other skip is `move`
without `--move-from/--move-to` (a target with one worker pool), printed as `not configured`. The report ends with the
target's skip list and `N passed, M failed, K skipped`; the command exits 1 unless the run is green. Every name the
suite creates starts with `--prefix` (default `cf-<random>`) and is deleted at the end.

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

`go test ./conformance/substrateapi/` runs exactly this against herd all-in-one and against the join mode with two
herdlets of different families (a VZ host declaring VOLUMES and a Cloud Hypervisor host declaring MEMORY), on the fake
hypervisor with the real herd-guest in every VM, and once more through `herd conformance` itself. The Nomad mode runs
the same binaries as the join mode (dist/nomad), so its run is the join invocation against the Nomad service
addresses; it needs the supervisor's cluster.
