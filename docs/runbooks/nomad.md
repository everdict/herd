# Runbook: herd on a Nomad cluster

Nomad supervises herd's processes; herd places actors (R-D4). herd-api and herd-router run as the service job
`dist/nomad/herd-api.nomad.hcl`; herdlet and herd-egress run on every Linux client as the system job
`dist/nomad/herdlet.nomad.hcl`. They are the same binaries as in the other modes, from the same release archive. The
commands below are checked by `docs/runbooks/runbooks_test.go`.

## Cluster prerequisites

- Nomad 1.7+ (Nomad variables, `nomadService` templates), with the `exec` and `raw_exec` drivers enabled on the
  clients (`plugin "raw_exec" { config { enabled = true } }`): herdlet needs `/dev/kvm`, TAP devices and nftables on
  the host, which only `raw_exec` gives it.
- On every Linux client: Cloud Hypervisor v54.0 at `/usr/local/bin/cloud-hypervisor` and virtiofsd, as in
  all-in-one.md step 1, and `herdlet doctor` passing. The release archive holds the guest kernel and initramfs; the
  job reads them from the task directory.
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

Each herdlet joins once with the token in its variable and keeps its grant in `/var/lib/herdlet`, so the token may
expire after the first rollout; a node added later needs a fresh token in the variable. If herd-api moves to another
client, herdlets join again at the new address (one more reason to keep a valid token in the variable).
