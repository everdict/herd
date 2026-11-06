# herdlet and herd-egress as a Nomad system job: one of each on every Linux client (docs/runbooks/nomad.md). Each
# herdlet joins herd-api with the join token in the job's Nomad variable once and reuses its grant after that; it
# keeps snapshots in the S3 store the join hands it, so it takes no -snapshots. The same variable's registry_auth, a
# docker config file, is the node's registry logins for private template images (-registry-auth, rendered 0600).
# insecure_registries are the registry hosts it pulls from over plain HTTP (-insecure-registry, comma-separated; ""
# names none): by default the cluster's managed image store, plain HTTP on the tailnet.
# The resources are the herdlet's own: each VM's cloud-hypervisor and virtiofsd run in a cgroup of the VM's own under
# /sys/fs/cgroup/herd (-vm-cgroup), outside this task's, so the VMs' memory is herd's capacity (-slots), not the task's.
# Rendered by deploy/artifacts; edit the template, not this file.
# The base URL every client fetches herd_linux_<arch>.tar.gz from: a mirror of the release (docs/runbooks/nomad.md
# step 3). No default: everdict/herd is private, and its GitHub release URL answers 404 to an artifact, which
# cannot authenticate.
variable "release_url" {
  type = string
}

variable "insecure_registries" {
  type    = string
  default = "100.69.164.81:5001,100.69.164.81:8787"
}

# LAB ONLY: a host-side prefix herd-egress stops refusing, e.g. a node's tailnet address as a /32. Empty, none.
variable "egress_allow_prefix" {
  type    = string
  default = ""
}

job "herdlet" {
  type = "system"

  group "herdlet" {
    constraint {
      attribute = "${attr.kernel.name}"
      value     = "linux"
    }

    task "herdlet" {
      driver = "raw_exec"

      # The herdlet process and what it runs itself (image unpacking, mke2fs); its VMs are not in this task's cgroup.
      resources {
        cpu    = 1000
        memory = 1024
      }

      artifact {
        source = "${var.release_url}/herd_linux_${attr.cpu.arch}.tar.gz"
      }

      template {
        data        = "HERD_API={{ range nomadService \"herd-api\" }}{{ .Address }}:{{ .Port }}{{ end }}"
        destination = "local/herd-api.env"
        env         = true
      }

      template {
        data        = "{{ with nomadVar \"nomad/jobs/herdlet\" }}{{ .join_token }}{{ end }}"
        destination = "secrets/join.token"
        perms       = "0600"
      }

      # The node's registry logins for private template images (-registry-auth): the variable's registry_auth, a docker
      # config file; without it the file is empty and pulls are anonymous.
      template {
        data        = "{{ with nomadVar \"nomad/jobs/herdlet\" }}{{ range $k, $v := . }}{{ if eq $k \"registry_auth\" }}{{ $v }}{{ end }}{{ end }}{{ end }}"
        destination = "secrets/registry-auth.json"
        perms       = "0600"
      }

      config {
        command = "local/herdlet"
        args = [
          "join", "${HERD_API}",
          "-token-file", "secrets/join.token",
          "-state", "/var/lib/herdlet",
          "-kernel", "${NOMAD_TASK_DIR}/kernel",
          "-initrd", "${NOMAD_TASK_DIR}/initrd.img",
          "-virtiofsd", "${NOMAD_TASK_DIR}/virtiofsd",
          "-registry-auth", "secrets/registry-auth.json",
          "-insecure-registry", "${var.insecure_registries}",
          "-egress-allow-prefix", "${var.egress_allow_prefix}",
        ]
      }
    }

    task "herd-egress" {
      driver = "raw_exec"

      artifact {
        source = "${var.release_url}/herd_linux_${attr.cpu.arch}.tar.gz"
      }

      template {
        data        = "HERD_API={{ range nomadService \"herd-api\" }}{{ .Address }}:{{ .Port }}{{ end }}"
        destination = "local/herd-api.env"
        env         = true
      }

      config {
        command = "local/herd-egress"
        args = [
          "-api", "${HERD_API}",
          "-token-file", "/var/lib/herdlet/egress.token",
          "-bindings", "/run/herd/egress-bindings.json",
          "-ca-out", "/var/lib/herdlet/egress-ca.pem",
          "-egress-allow-prefix", "${var.egress_allow_prefix}",
        ]
      }
    }
  }
}
