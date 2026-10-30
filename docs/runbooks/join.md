# Runbook: several hosts, no cluster tool (`herdlet join`)

One control host runs herd-api and herd-router; every other host runs a herdlet and herd-egress and joins with one
token herd-api minted (ADR 0001 A-20). Hosts need only a network path to the control host's ports 7443 (herd-api) and
7444 (router tunnel): herdlets dial out, nothing dials a node. The commands below are checked by
`docs/runbooks/runbooks_test.go`.

Snapshots: herd-api verifies every snapshot in its store before it records it, and a VM restores from the snapshot
files its backend wrote, so herd-api and every herdlet must see one snapshot directory. Export `/var/lib/herd/snapshots`
from the control host (NFS or any shared filesystem) and mount it at the same path on every node. Without it actors
run, but suspend, resume and fork fail verification.

## On the control host

1. Install herd from its release archive (see all-in-one.md step 2; only `herd-api` and `herd-router` are needed here):

   ```sh
   ARCH=$(dpkg --print-architecture)
   curl -fsSLO https://github.com/everdict/herd/releases/latest/download/herd_linux_$ARCH.tar.gz
   mkdir herd && tar -C herd -xzf herd_linux_$ARCH.tar.gz
   sudo install -m 0755 herd/herd-api herd/herd-router /usr/local/bin/
   sudo install -d /etc/herd /var/lib/herd/snapshots
   ```

2. Create the signing key and tell herd-api the router tunnel address herdlets dial (`<control-host>` is this host's
   address as the nodes reach it):

   ```sh
   sudo herd-api keygen -out /etc/herd/jwt.key
   echo "HERD_ADVERTISE_TUNNEL=<control-host>:7444" | sudo tee /etc/herd/herd-api.env
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

1. Install Cloud Hypervisor, virtiofsd and herd as in all-in-one.md steps 1–2, mount the shared snapshot directory at
   `/var/lib/herd/snapshots`, and check the host:

   ```sh
   sudo herdlet doctor -virtiofsd /usr/libexec/virtiofsd
   ```

2. Give the node herd-api's address and the join token:

   ```sh
   sudo install -d /etc/herd
   echo "HERD_API=<control-host>:7443" | sudo tee /etc/herd/herdlet.env
   echo "<join token>" | sudo tee /etc/herd/join.token
   sudo chmod 0600 /etc/herd/join.token
   ```

3. Install and start herdlet and herd-egress (`dist/systemd/herdlet.service`, `dist/systemd/herd-egress.service`). The
   first start joins: it exchanges the token for this worker's credential and the egress credential and keeps them
   in `/var/lib/herdlet`; later starts reuse them, so the token may expire.

   ```sh
   sudo install -m 0644 herd/dist/systemd/herdlet.service herd/dist/systemd/herd-egress.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now herdlet herd-egress
   ```

   The same join by hand, in the foreground:

   ```sh
   sudo herdlet join <control-host>:7443 --token "<join token>" -virtiofsd /usr/libexec/virtiofsd
   ```

## On each Mac node (Apple Silicon)

```sh
curl -fsSLO https://github.com/everdict/herd/releases/latest/download/herd_darwin_arm64.tar.gz
mkdir herd && tar -C herd -xzf herd_darwin_arm64.tar.gz
sudo install -m 0755 herd/herdlet herd/herd-egress /usr/local/bin/
sudo install -d /usr/local/share/herd /usr/local/etc/herd /usr/local/var/herdlet
sudo install -m 0644 herd/kernel herd/initrd.img /usr/local/share/herd/
herdlet doctor
echo "<join token>" | sudo tee /usr/local/etc/herd/join.token
sed 's/@HERD_API@/<control-host>:7443/' herd/dist/launchd/com.everdict.herdlet.plist | sudo tee /Library/LaunchDaemons/com.everdict.herdlet.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.everdict.herdlet.plist
```

## Check the pool

On the control host, every joined node is listed with what it declared:

```sh
TOKEN=$(sudo herd-api token -key-file /etc/herd/jwt.key -sub everdict -space team-a=write -ttl 720h)
herd-api workers -api 127.0.0.1:7443 -token "$TOKEN"
```

A join is refused with a token herd-api did not sign, an expired one, or any credential that is not a join token; a
join cannot take the name of a worker that is connected.
