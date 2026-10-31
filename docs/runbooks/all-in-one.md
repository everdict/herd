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
   1.13.2 (`scripts/virtiofsd/source.env`) and herdlet uses it from `/usr/local/libexec/herd/virtiofsd` by default:

   ```sh
   curl -fsSLO https://github.com/everdict/herd/releases/latest/download/herd_linux_$ARCH.tar.gz
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

5. Check the pool. The process wrote its signing key to `/var/lib/herd/jwt.key`; mint a client token with it and list
   the workers — this host's name appears, with platform `PLATFORM_LINUX_CLOUD_HYPERVISOR`:

   ```sh
   TOKEN=$(sudo herd-api token -key-file /var/lib/herd/jwt.key -sub everdict -space team-a=write -ttl 720h)
   herd-api workers -api 127.0.0.1:7443 -token "$TOKEN"
   ```

   Clients use herd-api at `<host>:7443` (Substrate Control API) and herd-router at `<host>:8443` with that token.
   Both serve cleartext HTTP/2: put a TLS terminator in front before exposing them beyond the host (ADR A-12).

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

## macOS (Apple Silicon, macOS 14+)

1. Install herd from the darwin release archive (`herd` and `herdlet` are signed with the virtualization entitlement):

   ```sh
   curl -fsSLO https://github.com/everdict/herd/releases/latest/download/herd_darwin_arm64.tar.gz
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
   TOKEN=$(sudo herd-api token -key-file /usr/local/var/herd/jwt.key -sub everdict -space team-a=write -ttl 720h)
   herd-api workers -api 127.0.0.1:7443 -token "$TOKEN"
   ```
