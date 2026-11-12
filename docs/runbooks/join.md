# Runbook: several hosts, no cluster tool (`herdlet join`)

One control host runs herd-api and herd-router; every other host runs a herdlet and herd-egress and joins as its
SPIFFE identity (ADR 0001 A-20, A-60). Hosts need only a network path to the control host's ports 7443 (herd-api) and
7444 (router tunnel): herdlets dial out, nothing dials a node. The commands below are checked by
`docs/runbooks/runbooks_test.go`.

Identity: every herd process is a SPIFFE identity it reads from the host's SPIRE agent (the Workload API at
`$SPIFFE_ENDPOINT_SOCKET`, else `unix:///run/spire/agent/sockets/api.sock`), and none starts without one. SPIRE comes
first: a SPIRE server for the install's trust domain `<td>`, an agent on every host, and these registration entries
(by the process's unix user or its unit, as the install attests workloads):

| process | SPIFFE ID |
|---|---|
| herd-api | `spiffe://<td>/herd/api` |
| herd-router | `spiffe://<td>/herd/router` |
| herdlet on worker `<w>` | `spiffe://<td>/herd/node/<w>` (the worker name is the ID's) |
| herd-egress on worker `<w>` | `spiffe://<td>/herd/egress/<w>` |
| an operator's tools | `spiffe://<td>/herd/operator/<name>` |

Nodes, herd-egress and operators need no grant: theirs are built in. The workloads that call herd (Everdict's
herders, `spiffe://<td>/everdict/ws/<ws>/runner/<runner>`) get atespace roles from `/etc/herd/spiffe-policy.json` on
the control host. On a cluster without a Workload API, `-tls-cert`, `-tls-key` and `-spiffe-bundle` give each process
its SVID and the bundle as files instead.

Snapshots: herd-api keeps them in an S3-compatible bucket (MinIO, AWS S3, ...) and hands that store — endpoint, bucket
and credentials — to every herdlet in its join grant, so herdlets write and read snapshots there and no host mounts
anything. herd-api verifies every snapshot in the bucket before it records it. Create a bucket (e.g. `herd-snapshots`)
and an access key for it on the S3 endpoint first; every node must reach that endpoint. Without `HERD_S3`, herd-api
keeps snapshots in `/var/lib/herd/snapshots`, which only hosts that share that directory could use.

## On the control host

1. Install herd from its release archive (see all-in-one.md step 2: everdict/herd is private, so the download needs a
   `gh` login that can read it; only `herd-api` and `herd-router` are needed here):

   ```sh
   ARCH=$(dpkg --print-architecture)
   gh release download --repo everdict/herd --pattern herd_linux_$ARCH.tar.gz
   mkdir herd && tar -C herd -xzf herd_linux_$ARCH.tar.gz
   sudo install -m 0755 herd/herd-api herd/herd-router /usr/local/bin/
   sudo install -d /etc/herd /var/lib/herd/snapshots
   ```

2. Write the workloads' grants, and tell herd-api the router tunnel address herdlets dial (`<control-host>` is this
   host's address as the nodes reach it) and the snapshot bucket (`<s3-endpoint>` is the S3 endpoint as herd-api and
   every node reach it, e.g. `http://minio.example:9000`). herd-api and herd-router refuse to start without the policy
   file; `{"rules": []}` admits only herd's own nodes, egress and operators:

   ```sh
   echo '{"rules": [{"prefix": "spiffe://<td>/everdict/ws/team-a/", "spaces": {"team-a": "write"}}]}' | sudo tee /etc/herd/spiffe-policy.json
   echo "HERD_ADVERTISE_TUNNEL=<control-host>:7444" | sudo tee /etc/herd/herd-api.env
   echo "HERD_S3=s3://herd-snapshots" | sudo tee -a /etc/herd/herd-api.env
   echo "HERD_S3_ENDPOINT=<s3-endpoint>" | sudo tee -a /etc/herd/herd-api.env
   echo "AWS_ACCESS_KEY_ID=<access key>" | sudo tee -a /etc/herd/herd-api.env
   echo "AWS_SECRET_ACCESS_KEY=<secret key>" | sudo tee -a /etc/herd/herd-api.env
   sudo chmod 0600 /etc/herd/herd-api.env
   ```

3. Install and start herd-api and herd-router (`dist/systemd/herd-api.service`, `dist/systemd/herd-router.service`):

   ```sh
   sudo install -m 0644 herd/dist/systemd/herd-api.service herd/dist/systemd/herd-router.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now herd-api herd-router
   ```

## On each Ubuntu node

1. Install Cloud Hypervisor and herd (with its virtiofsd) as in all-in-one.md steps 1–2 (no snapshot directory to
   mount: the join hands the node herd-api's bucket), and check the host (the `vm cgroup` line included: each VM runs in
   `/sys/fs/cgroup/herd/<vm>`, outside the herdlet unit's cgroup, as all-in-one.md step 3 says):

   ```sh
   sudo herdlet doctor
   ```

2. Give the node herd-api's address (the SPIRE agent on the node gives it its identity):

   ```sh
   sudo install -d /etc/herd
   echo "HERD_API=<control-host>:7443" | sudo tee /etc/herd/herdlet.env
   ```

3. Install and start herdlet and herd-egress (`dist/systemd/herdlet.service`, `dist/systemd/herd-egress.service`). The
   first start joins as the node's SVID: herd-api grants it its snapshot store and the router tunnel address, which
   it keeps in `/var/lib/herdlet`; later starts reuse them.

   ```sh
   sudo install -m 0644 herd/dist/systemd/herdlet.service herd/dist/systemd/herd-egress.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now herdlet herd-egress
   ```

   The same join by hand, in the foreground:

   ```sh
   sudo herdlet join <control-host>:7443
   ```

4. Optional: private images. The unit passes `-registry-auth /etc/herd/registry-auth.json`: write the node's registry
   logins there, a docker config file, mode 0600, as all-in-one.md step 7 shows; `sudo herdlet doctor -registry-auth
   /etc/herd/registry-auth.json` lists the hosts it holds logins for. Every node that may run a private image needs
   the file; it is read at every pull, so no restart. Prefer a per-template `image_pull_auth` (all-in-one.md step 7,
   PLT-330): the golden build pulls with it, and the node file is the fallback.

## On each Mac node (Apple Silicon)

```sh
gh release download --repo everdict/herd --pattern herd_darwin_arm64.tar.gz
mkdir herd && tar -C herd -xzf herd_darwin_arm64.tar.gz
sudo install -m 0755 herd/herdlet herd/herd-egress /usr/local/bin/
sudo install -d /usr/local/share/herd /usr/local/etc/herd /usr/local/var/herdlet
sudo install -d -m 0700 /usr/local/etc/herd/secrets
sudo install -m 0644 herd/kernel herd/initrd.img /usr/local/share/herd/
herdlet doctor
sed 's/@HERD_API@/<control-host>:7443/' herd/dist/launchd/com.everdict.herdlet.plist | sudo tee /Library/LaunchDaemons/com.everdict.herdlet.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.everdict.herdlet.plist
```

On a Mac the herdlet runs herd-egress itself, in-process (each VM's network device is a file handle into its own
userspace stack, ADR A-21), as the node's SVID. It resolves an egress policy's
`ate-secret://dir/<name>/<tail>` from its `-secrets` directory, which the launchd daemon sets to
`/usr/local/etc/herd/secrets` (put the file `<name>/<tail>` there, mode 0600); without `-secrets` it refuses every
request whose credential it must inject. The daemon also passes `-registry-auth
/usr/local/etc/herd/registry-auth.json`, the node's registry logins for private images (a docker config file, mode
0600, as all-in-one.md step 7 shows; without it pulls are anonymous; a template's `image_pull_auth` takes precedence
for its golden build). `herdlet doctor` fails a herdlet that is not
signed with `com.apple.security.virtualization` (the release's is; `scripts/release.sh` signs it on a Mac) and a
kernel of the other architecture. The Mac declares MEMORY only while its save/restore probe passed and a user is at
the console with the screen unlocked, VOLUMES otherwise; a lock, an unlock or a wake reaches herd-api's worker record
within 30 s (ADR A-22).

## Check the pool

On the control host, as an operator's SVID (the agent's entry for the operator's unix user), every joined node is
listed with what it declared:

```sh
herd-api workers -api 127.0.0.1:7443
```

A Substrate-API client (Everdict's herder) reads the same records with `Control.ListWorkers`: each node is a
`Worker` of sandbox class `microvm`, ACTIVE while connected, with its cpu and memory (and slot ceiling, `actors`) as `capacity`, what herd-api's actors on it take as
`allocated`, and its declared
fidelity and restore family as its runtime's compat attributes (ADR 0001 A-4).

A join is refused to any caller that is not the node SVID of the worker it names (another node's, an operator's); a
join cannot take the name of a worker that is connected.

Break-glass: until every caller has an SVID, herd-api and herd-router accept HS256 bearers signed with `-key-file` only
with `-break-glass-hs256-until <YYYY-MM-DD>` (each call counted on `herd_auth_breakglass_calls_total` at
`http://<host>:7449/metrics`), and refuse to start once that day has passed. Under it a host may still join with a
join token (`herd-api join-token -key-file ...`, `herdlet join --token`), on top of its node SVID.
