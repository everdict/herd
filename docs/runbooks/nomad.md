# Runbook: herd on a Nomad cluster

Nomad supervises herd's processes; herd places actors (R-D4). herd-api and herd-router run as the service job
`dist/nomad/herd-api.nomad.hcl`; herdlet and herd-egress run on every Linux client as the system job
`dist/nomad/herdlet.nomad.hcl`. They are the same binaries as in the other modes, from the same release archive. The
commands below are checked by `docs/runbooks/runbooks_test.go`.

A service a node runs for its actors (a herder's door relay) is reached by registering it as a Nomad service tagged
`herd-host-service`: each client's herd-egress renders it into its `-host-services-file` as
`<service>.herd.internal:<port>`, dialed at the address Nomad registered (ADR A-18), so actors on any private network
reach it with no lab allowance and no address in a job file. An actor still needs a policy rule for that name.

## Cluster prerequisites

- Nomad 1.7+ (Nomad variables, `nomadService` templates), with the `exec` and `raw_exec` drivers enabled on the
  clients (`plugin "raw_exec" { config { enabled = true } }`): herdlet needs `/dev/kvm`, TAP devices and nftables on
  the host, which only `raw_exec` gives it, and `conntrack` (Ubuntu package `conntrack`) to clear a VM address's
  stale connection tracking before its guest runs (ADR A-59; without it herdlet logs `conntrack entries … not
  cleared` and goes on).
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
  (see join.md, Snapshots). A snapshot past 64 MiB goes up as a multipart upload, so the key needs
  `s3:AbortMultipartUpload` besides put, get and delete (an upload that fails is aborted). The job sets no
  `-call-timeout`: herd-api waits for a snapshot by the size its herdlet reports, at `-restore-bandwidth` (8 MiB/s by
  default), as it waits for a restore by its object's size (ADR A-35).

## Steps

1. Get the release archive (with a `gh` login that can read the private everdict/herd, as in all-in-one.md step 2)
   and create the signing key and join token on an operator machine:

   ```sh
   gh release download --repo everdict/herd --pattern herd_linux_amd64.tar.gz
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

   Prefer a per-template credential to `registry_auth` (PLT-330): whoever creates the template passes
   `image_pull_auth` with CreateActorTemplate, and the golden build's pull uses it (all-in-one.md step 7). Keep
   `registry_auth` as the fallback for pulls outside a golden build (ADR A-34, "Not here"): a node other than the
   golden builder that restores or cold-boots a private template without the image in its cache logs in with it.

   The herdlets pull over https from every registry but loopback. A store run without TLS is named per template:
   whoever creates the template passes its host and its token realm's in `image_plain_http_hosts` with
   CreateActorTemplate, beside `image_pull_auth`, and the golden build's pull speaks plain HTTP to exactly those
   (ADR A-37). The job variable `insecure_registries` (`-insecure-registry`, comma-separated; PLT-224) is a node-level
   fallback for pulls outside a golden build and names none by default; to set one:
   `nomad job run -var insecure_registries=<host:port>,<realm host:port> ...`.

3. Serve the archive to the clients and run the job pair. A Nomad artifact cannot sign in to GitHub, and a private
   repository's release URL answers 404 without it, so the jobs fetch `herd_linux_<arch>.tar.gz` from a mirror every
   client reaches: any HTTP(S) URL or bucket that serves the archive you downloaded in step 1 (for example a bucket
   readable without credentials on the S3 endpoint of the snapshots). `release_url` is that mirror's base URL, with
   no default:

   ```sh
   nomad job run -var release_url=<mirror base URL> herd/dist/nomad/herd-api.nomad.hcl
   nomad job run -var release_url=<mirror base URL> herd/dist/nomad/herdlet.nomad.hcl
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
