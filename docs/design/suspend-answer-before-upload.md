# Answering a suspend before its upload (PLT-591 S5)

Status: **proposed, not built.** This is the written decision PLT-591's spec asks for (decision S5): what it would take
for herd to answer a MEMORY suspend once its bundle is durable on the node, and upload it behind the answer. It changes
what `ACTOR_STATE_SUSPENDED` promises, so it is for the owner to decide; no code in this round depends on it.

## What a suspend promises today

A suspend (`SuspendActor`, synchronous, or asynchronous answering `SUSPENDING` at once) ends `SUSPENDED` only after:

1. herdlet flushed the guest, the VMM wrote its snapshot files, and herdlet wrote the bundle (A-60: compressed, hashed
   as written);
2. herdlet `Put` the bundle in the snapshot store and answered its key and digest;
3. herd-api proved the object in the store against that digest (`Prove`), recorded the snapshot on the actor, and only
   then deleted the VM and released the worker (R-D6: durable before release).

So `SUSPENDED` means: the actor's snapshot is in the store, any node of its restore family can resume it, and the node
that ran it holds nothing the actor needs. Everdict reads exactly that: a row whose herd actor is `SUSPENDED` may be
resumed anywhere, its node's room is free, and losing that node loses nothing.

What PLT-591 measured is the cost of step 2: on v0.1.23 it was most of a 5 to 7 minute suspend. A-60 reduces the bytes
and the passes; the upload still runs over the node's link to the store, and the VM's memory stays allocated on the
node until step 3.

## The proposal

Substrate's API already names this state, and herd does not use it: a `Snapshot`'s `durable_snapshot.state` is
`PENDING` "while the snapshot is uploaded, including an asynchronous suspend's snapshot that so far exists only on the
node named by locality", and `COMPLETED` "once the upload is recorded". herd answers every snapshot `COMPLETED` today
(`apiserver.snapshotProto`) and fills `locality` with the restore family, not a node. The proposal uses those fields
as Substrate defines them, so the actor state stays `SUSPENDED` and a client that reads only the state sees no new
value:

| `durable_snapshot.state` | `locality` | where the bundle is | who may restore it |
| --- | --- | --- | --- |
| `PENDING` (new in herd) | the worker's name | the node's bundle cache only, fsynced; the upload runs | that node only |
| `COMPLETED` (today's only value) | the restore family, as today (or cleared, per Substrate: a decision of its own) | the snapshot store, proved by herd-api | any node of its restore family |

The suspend would answer `SUSPENDED` with its snapshot `PENDING` on the worker once herdlet reports the bundle durable on its disk
(written, fsynced, renamed into the cache, its digest known); herd-api would then release the worker's allocation
(the VM itself stops right after the VMM's snapshot, before the bundle, in both designs; what comes earlier is the
answer and the release of the room herd-api charges for it). herdlet uploads behind the answer
and reports the object; herd-api proves it and records the snapshot `COMPLETED`.

### Who reads the new state

- **herd-api's scheduler.** A resume of an actor whose snapshot is `PENDING` must be placed on that worker, or
  wait for `COMPLETED`. Placement already takes a selector; this adds a hard constraint the actor did not ask for.
- **herd-api's sweeps (A-58).** The object does not exist yet: the sweep must not read its absence as garbage, and an
  upload that ends after the actor was deleted must delete its object.
- **The bundle cache (A-57, A-58).** A pending bundle is the only copy: the cache must never evict it, its sweep must not
  drop it, and a full disk must fail the next suspend, not this bundle.
- **Everdict.** A row `suspended` today means "resumable anywhere, node free". With this change Everdict must read the snapshot's
  `durable_snapshot.state` and `locality`: admission may count the node free (herd-api released the room) but a resume of that row is bound to the node
  until `COMPLETED`; the reconciler must not treat a node loss as harmless while any of its rows are `PENDING`.
  Everdict needs a field on its row (or reads herd's snapshot locality on each resume) and a herd version check, since
  an older herd never answers it.

## What each failure does

| failure | today | with the proposal |
| --- | --- | --- |
| node lost (power, disk) before the upload ends | the suspend never answered `SUSPENDED`; the actor stays `RUNNING`/owed and the VM is gone: the actor is lost to its last good snapshot, if any, and herd-api says so | the actor reads `SUSPENDED` and its only snapshot is gone: a resume fails. herd-api must turn the actor `FAILED` (or back to its previous snapshot) once the worker is judged lost, and Everdict must settle a row that it already read as safely suspended. **Data loss after a success answer** — the case that makes this a promise change. |
| herdlet restarts mid-upload | the call ends unknown; herd-api retries the snapshot (owed) | the bundle is in the cache, the upload must resume from the cache on herdlet's start (a new duty: herdlet must list pending bundles and their keys, which today only herd-api knows) |
| upload fails (store refuses, digest mismatch) | the suspend fails, the VM boots again from its share (`Restart`, a cold boot), the actor stays `RUNNING` | the caller was told `SUSPENDED`: herd-api must retry the upload from the cached bundle, and an upload that fails for good leaves a `SUSPENDED` actor restorable only on that node (or, if the room was given away, nowhere until it frees) |
| restore requested elsewhere before the upload ends | not possible: `SUSPENDED` implies `COMPLETED` | must wait for `COMPLETED` (the resume blocks for the rest of the upload, which is the time saved, moved to the resume) or be placed on the node (which may not have room) |
| resume on the same node before the upload ends | — | restores from the cached bundle at once (the case that gains most: an idle delegate woken minutes later) |
| actor deleted before the upload ends | — | the upload must be cancelled or its object deleted after it lands; herd-api's record of the deletion must outlive the upload |
| herd-api restarts | owed steps are replayed | the `PENDING` state and its worker must be in the record store, and a pending upload's end must be reconciled against it |
| fork of a running box (PLT-590, two snapshots) | each snapshot is in the store before the fork restores | a fork elsewhere waits for `COMPLETED` exactly as a restore elsewhere does |

## Recommendation

Do not ship it as a silent change of `SUSPENDED`. A-60 brings the measured 2 GiB case's upload from the bundle's raw
size at one stream to its compressed size at four; re-measure on in-heo first (the phase line names the upload's
seconds). If the upload is still the dominant phase, build the proposal behind an explicit option that the caller asks
for per suspend (`SuspendOptions` gains a field saying the caller accepts a `PENDING` snapshot; default: it does not), so Everdict opts in only where
it reads them (idle delegates resumed on their own node), and the node-loss row above is a risk the caller
chose. Its first counterexamples: a node lost after a `PENDING` answer turns the actor `FAILED` with a reason, never a
`SUSPENDED` actor with no snapshot; a pending bundle survives a cache sweep and a herdlet restart; a resume elsewhere
waits for `COMPLETED`.
