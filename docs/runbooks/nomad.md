# Runbook: herd on a Nomad cluster

Nomad supervises herd's processes; herd places actors (R-D4). herd-api and herd-router run as the service job
`dist/nomad/herd-api.nomad.hcl`; herdlet and herd-egress run on every Linux client as the system job
`dist/nomad/herdlet.nomad.hcl`. They are the same binaries as in the other modes, from the same release archive. The
commands below are checked by `docs/runbooks/runbooks_test.go`.

## Cluster prerequisites

- Nomad 1.7+ (Nomad variables, `nomadService` templates), with the `exec` and `raw_exec` drivers enabled on the
  clients (`plugin "raw_exec" { config { enabled = true } }`): herdlet needs `/dev/kvm`, TAP devices and nftables on
  the host, which only `raw_exec` gives it.
- On every Linux client: Cloud Hypervisor v54.0 at `/usr/local/bin/cloud-hypervisor`, as in all-in-one.md step 1,
  and `herdlet doctor` passing (from the unpacked archive, with the job's kernel, initramfs and virtiofsd:
  `sudo ./herd/herdlet doctor -kernel ./herd/kernel -initrd ./herd/initrd.img -virtiofsd ./herd/virtiofsd`).
  The release archive holds the guest kernel and initramfs and herd's virtiofsd; the job reads them from the task
  directory.
- cgroup v2 on every Linux client (Ubuntu 24.04's default), which doctor's `vm cgroup` line checks: the herdlet runs each
  VM's cloud-hypervisor and virtiofsd in a cgroup of the VM's own, `/sys/fs/cgroup/herd/<vm>`, outside its Nomad task's
  (`/nomad.slice/...<alloc>.herdlet.scope`). The herdlet task's `resources` (1000 MHz, 1024 MB) are the herdlet's
  own; a VM's memory is charged to its own cgroup (`memory.max` = the guest memory + 512 MiB, `-vm-memory-overhead`),
  so a MEMORY restore or a bigger guest is never the task's memory limit's to kill. Nomad does not see the VMs'
  memory: on a client that also runs other jobs, set it aside in the client's `reserved { memory = ... }` block. The
  VMs end with the herdlet process, as they did inside its task.
- One client holds the host volume `herd-data` (the bbolt store allows one herd-api).
- An S3-compatible bucket (e.g. `herd-snapshots`) and an access key for it, on an endpoint every client reaches:
  herd-api keeps snapshots there and each herdlet's join hands it the store, so no client mounts a shared directory
  (see join.md, Snapshots).

## Steps

1. Get the release archive and create the signing key and join token on an operator machine:

   ```sh
   curl -fsSLO https://github.com/everdict/herd/releases/latest/download/herd_linux_amd64.tar.gz
   mkdir herd && tar -C herd -xzf herd_linux_amd64.tar.gz
   ./herd/herd-api keygen -out jwt.key
   ./herd/herd-api join-token -key-file jwt.key -ttl 24h > join.token
   ```

2. Put them, and herd-api's snapshot bucket (`<s3-endpoint>` as every client reaches it, e.g.
   `http://minio.example:9000`), into the jobs' Nomad variables:

   ```sh
   nomad var put nomad/jobs/herd-api jwt_key="$(cat jwt.key)" s3=s3://herd-snapshots s3_endpoint=<s3-endpoint> \
     aws_access_key_id=<access key> aws_secret_access_key=<secret key>
   nomad var put nomad/jobs/herdlet join_token="$(cat join.token)"
   ```

   For private template images, the herdlet variable also holds the nodes' registry logins, `registry_auth`: a docker
   config file (`auths`, as all-in-one.md step 7 shows). The job renders it to `secrets/registry-auth.json`, mode
   0600, and runs the herdlet with `-registry-auth` on it; without the key the file is empty and pulls are anonymous.
   `nomad var put` replaces the whole variable, so give both keys:

   ```sh
   nomad var put nomad/jobs/herdlet join_token="$(cat join.token)" registry_auth=@registry-auth.json
   ```

   The herdlets pull over plain HTTP from the hosts in the job variable `insecure_registries` (`-insecure-registry`,
   comma-separated; PLT-224), by default the cluster's managed image store `100.69.164.81:5001` (PLT-42), and over
   https from every other host. Another store: `nomad job run -var insecure_registries=<host:port> ...`; none:
   `-var insecure_registries=`.

3. Run the job pair:

   ```sh
   nomad job run herd/dist/nomad/herd-api.nomad.hcl
   nomad job run herd/dist/nomad/herdlet.nomad.hcl
   ```

4. Check the pool: every Linux client is a worker.

   ```sh
   TOKEN=$(./herd/herd-api token -key-file jwt.key -sub everdict -space team-a=write -ttl 720h)
   ./herd/herd-api workers -api <herd-api client address>:7443 -token "$TOKEN"
   ```

   Once actors run, each client's VMs are in cgroups of their own, not in the herdlet task's: every
   cloud-hypervisor process is in `/herd/<vm>`.

   ```sh
   for p in $(pgrep -x cloud-hyperviso); do cat /proc/$p/cgroup; done
   ```

Each herdlet joins once with the token in its variable and keeps its grant in `/var/lib/herdlet`, so the token may
expire after the first rollout; a node added later needs a fresh token in the variable. If herd-api moves to another
client, herdlets join again at the new address (one more reason to keep a valid token in the variable).
