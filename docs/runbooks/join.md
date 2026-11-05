# Runbook: several hosts, no cluster tool (`herdlet join`)

One control host runs herd-api and herd-router; every other host runs a herdlet and herd-egress and joins with one
token herd-api minted (ADR 0001 A-20). Hosts need only a network path to the control host's ports 7443 (herd-api) and
7444 (router tunnel): herdlets dial out, nothing dials a node. The commands below are checked by
`docs/runbooks/runbooks_test.go`.

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

2. Create the signing key and tell herd-api the router tunnel address herdlets dial (`<control-host>` is this host's
   address as the nodes reach it) and the snapshot bucket (`<s3-endpoint>` is the S3 endpoint as herd-api and every
   node reach it, e.g. `http://minio.example:9000`):

   ```sh
   sudo herd-api keygen -out /etc/herd/jwt.key
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

4. Mint the join token (valid 24 h; any number of hosts may join with it while it is valid):

   ```sh
   sudo herd-api join-token -key-file /etc/herd/jwt.key -ttl 24h
   ```

## On each Ubuntu node

1. Install Cloud Hypervisor and herd (with its virtiofsd) as in all-in-one.md steps 1–2 (no snapshot directory to
   mount: the join hands the node herd-api's bucket), and check the host (the `vm cgroup` line included: each VM runs in
   `/sys/fs/cgroup/herd/<vm>`, outside the herdlet unit's cgroup, as all-in-one.md step 3 says):

   ```sh
   sudo herdlet doctor
   ```

2. Give the node herd-api's address and the join token:

   ```sh
   sudo install -d /etc/herd
   echo "HERD_API=<control-host>:7443" | sudo tee /etc/herd/herdlet.env
   echo "<join token>" | sudo tee /etc/herd/join.token
   sudo chmod 0600 /etc/herd/join.token
   ```

3. Install and start herdlet and herd-egress (`dist/systemd/herdlet.service`, `dist/systemd/herd-egress.service`). The
   first start joins: it exchanges the token for this worker's credential, the egress credential and herd-api's
   snapshot store, and keeps them in `/var/lib/herdlet`; later starts reuse them, so the token may expire.

   ```sh
   sudo install -m 0644 herd/dist/systemd/herdlet.service herd/dist/systemd/herd-egress.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now herdlet herd-egress
   ```

   The same join by hand, in the foreground:

   ```sh
   sudo herdlet join <control-host>:7443 --token "<join token>"
   ```

4. Optional: private images. The unit passes `-registry-auth /etc/herd/registry-auth.json`: write the node's registry
   logins there, a docker config file, mode 0600, as all-in-one.md step 7 shows; `sudo herdlet doctor -registry-auth
   /etc/herd/registry-auth.json` lists the hosts it holds logins for. Every node that may run a private image needs
   the file; it is read at every pull, so no restart.

## On each Mac node (Apple Silicon)

```sh
gh release download --repo everdict/herd --pattern herd_darwin_arm64.tar.gz
mkdir herd && tar -C herd -xzf herd_darwin_arm64.tar.gz
sudo install -m 0755 herd/herdlet herd/herd-egress /usr/local/bin/
sudo install -d /usr/local/share/herd /usr/local/etc/herd /usr/local/var/herdlet
sudo install -d -m 0700 /usr/local/etc/herd/secrets
sudo install -m 0644 herd/kernel herd/initrd.img /usr/local/share/herd/
herdlet doctor
echo "<join token>" | sudo tee /usr/local/etc/herd/join.token
sed 's/@HERD_API@/<control-host>:7443/' herd/dist/launchd/com.everdict.herdlet.plist | sudo tee /Library/LaunchDaemons/com.everdict.herdlet.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.everdict.herdlet.plist
```

On a Mac the herdlet runs herd-egress itself, in-process (each VM's network device is a file handle into its own
userspace stack, ADR A-21), with the egress credential the join granted. It resolves an egress policy's
`ate-secret://dir/<name>/<tail>` from its `-secrets` directory, which the launchd daemon sets to
`/usr/local/etc/herd/secrets` (put the file `<name>/<tail>` there, mode 0600); without `-secrets` it refuses every
request whose credential it must inject. The daemon also passes `-registry-auth
/usr/local/etc/herd/registry-auth.json`, the node's registry logins for private images (a docker config file, mode
0600, as all-in-one.md step 7 shows; without it pulls are anonymous). `herdlet doctor` fails a herdlet that is not
signed with `com.apple.security.virtualization` (the release's is; `scripts/release.sh` signs it on a Mac) and a
kernel of the other architecture. The Mac declares MEMORY only while its save/restore probe passed and a user is at
the console with the screen unlocked, VOLUMES otherwise; a lock, an unlock or a wake reaches herd-api's worker record
within 30 s (ADR A-22).

## Check the pool

On the control host, every joined node is listed with what it declared:

```sh
TOKEN=$(sudo herd-api token -key-file /etc/herd/jwt.key -sub everdict -space team-a=write -ttl 720h)
herd-api workers -api 127.0.0.1:7443 -token "$TOKEN"
```

A Substrate-API client (Everdict's herder) reads the same records with `Control.ListWorkers`: each node is a
`Worker` of sandbox class `microvm`, ACTIVE while connected, with its slots as `capacity.actors` and its declared
fidelity and restore family as its runtime's compat attributes (ADR 0001 A-4).

A join is refused with a token herd-api did not sign, an expired one, or any credential that is not a join token; a
join cannot take the name of a worker that is connected.
