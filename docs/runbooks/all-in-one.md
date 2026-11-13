# Runbook: herd all-in-one (one host)

One host runs everything in one process: herd-api on a bbolt store, herd-router, herd-egress and a herdlet that joins
that herd-api with a token the process mints (ADR 0001 A-11, A-20). The commands below are checked by
`docs/runbooks/runbooks_test.go`: every herd command and flag named here exists.

## Ubuntu (x86_64 or arm64, KVM)

1. Install Cloud Hypervisor v54.0:

   ```sh
   ARCH=$(dpkg --print-architecture)   # amd64 or arm64
   CH=cloud-hypervisor-static; [ "$ARCH" = arm64 ] && CH=cloud-hypervisor-static-aarch64
   curl -fsSLo cloud-hypervisor https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v54.0/$CH
   sudo install -m 0755 cloud-hypervisor /usr/local/bin/cloud-hypervisor
   ```

2. Install herd from its release archive (binaries, a static virtiofsd, guest kernel and initramfs, and `dist/`).
   herd needs virtiofsd >= 1.11 and Ubuntu 24.04's `virtiofsd` package is 1.10, so the archive carries virtiofsd
   1.13.2 (`scripts/virtiofsd/source.env`) and herdlet uses it from `/usr/local/libexec/herd/virtiofsd` by default.
   everdict/herd is a private repository, so its release assets answer 404 to an unauthenticated download. Download
   the archive with the GitHub CLI signed in as an account that can read the repository (`gh auth login`, or
   `GH_TOKEN` set to a token with read access to everdict/herd's contents); with no tag, `gh release download` takes
   the latest release, and a tag (`gh release download <tag> …`) pins one:

   ```sh
   gh release download --repo everdict/herd --pattern herd_linux_$ARCH.tar.gz
   mkdir herd && tar -C herd -xzf herd_linux_$ARCH.tar.gz
   sudo install -m 0755 herd/herd herd/herd-api herd/herdlet herd/herd-router herd/herd-egress /usr/local/bin/
   sudo install -d /usr/local/share/herd /usr/local/libexec/herd
   sudo install -m 0644 herd/kernel herd/initrd.img /usr/local/share/herd/
   sudo install -m 0755 herd/virtiofsd /usr/local/libexec/herd/virtiofsd
   ```

   To use a virtiofsd of your own (>= 1.11), pass its path as `-virtiofsd` to `herdlet doctor` here and in the unit's
   `ExecStart`.

3. Check that this host can run herd actors. Every line must say `ok`; a `FAIL` line says what is missing (for
   example `/dev/kvm`, a virtiofsd that does not accept the options herd starts it with, or a `vm cgroup` herd cannot
   make):

   ```sh
   sudo herdlet doctor
   ```

   herd runs each VM's cloud-hypervisor and virtiofsd in a cgroup of the VM's own, `/sys/fs/cgroup/herd/<vm>`
   (`-vm-cgroup`), outside the herd unit's, with `memory.max` = the guest memory + `-vm-memory-overhead` (512 MiB);
   that needs cgroup v2 (Ubuntu's default) and herd running as root. `cat /sys/fs/cgroup/herd/*/memory.current` shows
   what each VM holds. The VMs end with the herd process.

4. Install and start the unit (`dist/systemd/herd.service`; data in `/var/lib/herd`):

   ```sh
   sudo install -m 0644 herd/dist/systemd/herd.service /etc/systemd/system/herd.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now herd
   ```

5. Check the pool. The all-in-one is its own SPIFFE issuer, for development and tests only (trust domain `herd.local`,
   ADR 0001 A-60; herd-api logs that it runs on it, and a herd-api or herd-router that is not all-in-one refuses to
   start on `herd.local`, so the development CA never serves a real install): it keeps a local
   CA in `/var/lib/herd/identity`, issues its components their SVIDs, and writes an operator's SVID there
   (`operator.pem`, `operator-key.pem`, `spiffe://herd.local/herd/operator/local`) with the bundle (`bundle.pem`).
   List the workers as that operator — this host's name appears, with platform `PLATFORM_LINUX_CLOUD_HYPERVISOR`:

   ```sh
   sudo herd-api workers -api 127.0.0.1:7443 -tls-cert /var/lib/herd/identity/operator.pem -tls-key /var/lib/herd/identity/operator-key.pem -spiffe-bundle /var/lib/herd/identity/bundle.pem
   ```

   Clients use herd-api at `<host>:7443` (Substrate Control API) and herd-router at `<host>:8443`, both over TLS with
   SVIDs of `herd.local` (they name `localhost` and `127.0.0.1` too, for a client that checks a host name against
   `bundle.pem`): herd-api takes a client X.509-SVID or a JWT-SVID bearer for `spiffe://herd.local/herd/api`,
   herd-router the JWT-SVID. `/var/lib/herd/identity/spiffe-policy.json` grants other workloads atespace roles.

6. Optional: credentials for egress policies. An `EgressPolicy` whose `replace_headers` names
   `ate-secret://dir/<name>/<tail>` has herd-egress inject the file `<name>/<tail>` of its secrets directory (the
   actor never holds the value). The all-in-one takes that directory as `-secrets` and hands it to its herd-egress
   (on a Mac, to the herdlet's in-process herd-egress);
   add it to the unit's `ExecStart` (`sudo systemctl edit --full herd`), for example:

   ```sh
   sudo install -d -m 0700 /etc/herd/secrets/github
   sudo install -m 0600 token /etc/herd/secrets/github/token   # ate-secret://dir/github/token
   /usr/local/bin/herd -data /var/lib/herd -listen 0.0.0.0:7443 -router-listen 0.0.0.0:8443 -secrets /etc/herd/secrets
   ```

   Without `-secrets` herd-egress has no credential provider and refuses a request whose credential it must inject.

7. Optional: private images. Substrate's API carries no pull secret, so, as a Kubernetes node's container runtime
   does, the node holds its registry logins (PLT-222): `-registry-auth` names a docker config file (`auths`, per
   registry host: `auth`, base64 of `user:password`, or `username` and `password`, and optionally `identitytoken`).
   A pull answers the registry's challenge with the host's login: Basic credentials to a Bearer challenge's token realm
   (an identity token as an OAuth2 refresh token), or to a Basic-only registry itself; a login is never sent to a
   plain-HTTP realm. The file is read at every pull, so writing or changing it needs no restart; without it pulls are
   anonymous, as before, and a private image's pull fails with the realm's 403. The shipped units pass
   `-registry-auth /etc/herd/registry-auth.json`. For a private image on ghcr.io (a token with `read:packages`):

   ```sh
   AUTH=$(printf '%s:%s' <github user> <token> | base64 | tr -d '\n')
   sudo install -d /etc/herd && sudo install -m 0600 /dev/null /etc/herd/registry-auth.json
   printf '{"auths": {"ghcr.io": {"auth": "%s"}}}\n' "$AUTH" | sudo tee /etc/herd/registry-auth.json >/dev/null
   sudo herdlet doctor -registry-auth /etc/herd/registry-auth.json
   ```

   `herdlet doctor` prints `registry auth` with the hosts the file holds logins for (never the secrets), `none` when
   there is no file, and fails a file it cannot read. A `~/.docker/config.json` that `docker login` wrote with a
   `credsStore` or `credHelpers` holds no secret in `auths`: herd does not run credential helpers, so write `auths`.

   **Prefer a per-template credential** (PLT-330, ADR A-34). A template's image is pulled once, when herd-api builds
   its golden snapshot, so the caller that creates it can pass a one-shot credential for that pull instead of the node
   holding a long-lived login: `CreateActorTemplateRequest.image_pull_auth`, the same docker config JSON as the file
   above (`{"auths": {"ghcr.io": {"auth": "<base64 user:token>"}}}`). herd-api keeps it in memory until the golden
   build ends and hands it to the herdlet that builds it; it is never stored, logged or returned. For the hosts it
   names it takes precedence over `-registry-auth`; a refused one fails the golden build naming the registry. The
   node's `-registry-auth` is the fallback, and is still needed where a node pulls without that build: a template
   whose golden mode is DISABLED, and a node other than the golden builder that restores or cold-boots the template
   without the image in its cache.

8. Optional: a plain-HTTP registry (PLT-224, DEFAUL-1600). Pulls speak https to every registry except localhost and
   127.0.0.1. Preferred: the template names the plain hosts of its golden build's pull, the store and its token
   realm, in `CreateActorTemplateRequest.image_plain_http_hosts` (one-shot like `image_pull_auth`, never stored; ADR
   A-37), so no node is configured for a store. The node-level fallback, for pulls outside a golden build, is
   `-insecure-registry host[:port]` (repeatable, or comma-separated; none by default), as the docker daemon's
   `insecure-registries`: exactly as written, so `10.0.0.5:5001` makes neither `10.0.0.5` nor `10.0.0.5:5002` plain.
   A login (`image_pull_auth` or `-registry-auth`) goes to a token realm only over https or to a host named plain
   either way. `herdlet doctor` prints `insecure registries` with the node's plain-HTTP hosts.

## macOS (Apple Silicon, macOS 14+)

1. Install herd from the darwin release archive (`herd` and `herdlet` are signed with the virtualization entitlement),
   downloaded with a `gh` login that can read everdict/herd, as in the Ubuntu step 2:

   ```sh
   gh release download --repo everdict/herd --pattern herd_darwin_arm64.tar.gz
   mkdir herd && tar -C herd -xzf herd_darwin_arm64.tar.gz
   sudo install -m 0755 herd/herd herd/herd-api herd/herdlet herd/herd-router herd/herd-egress /usr/local/bin/
   sudo install -d /usr/local/share/herd /usr/local/var/herd
   sudo install -d -m 0700 /usr/local/etc/herd/secrets
   sudo install -m 0644 herd/kernel herd/initrd.img /usr/local/share/herd/
   ```

2. Check the host:

   ```sh
   herdlet doctor
   ```

3. Install and load the launchd job (`dist/launchd/com.everdict.herd.plist`; data in `/usr/local/var/herd`). On a Mac
   the VMs' egress goes through the herdlet's in-process herd-egress (ADR A-21), which resolves
   `ate-secret://dir/<name>/<tail>` credentials from the job's `-secrets` directory, `/usr/local/etc/herd/secrets`
   (the file `<name>/<tail>`, mode 0600):

   ```sh
   sudo install -m 0644 herd/dist/launchd/com.everdict.herd.plist /Library/LaunchDaemons/com.everdict.herd.plist
   sudo launchctl bootstrap system /Library/LaunchDaemons/com.everdict.herd.plist
   ```

4. Check the pool (platform `PLATFORM_DARWIN_VZ`, suspend fidelity `SNAPSHOT_FIDELITY_VOLUMES` unless this Mac
   proved it can save machine state, R-D9):

   ```sh
   sudo herd-api workers -api 127.0.0.1:7443 -tls-cert /usr/local/var/herd/identity/operator.pem -tls-key /usr/local/var/herd/identity/operator-key.pem -spiffe-bundle /usr/local/var/herd/identity/bundle.pem
   ```

5. Optional: private images. The launchd job passes `-registry-auth /usr/local/etc/herd/registry-auth.json`; write
   the docker config file there, mode 0600, as Ubuntu step 7 says (a Mac's herdlet pulls exactly as Linux's does).
   A template's `image_pull_auth` takes precedence for its golden build (Ubuntu step 7); the file is the fallback.
