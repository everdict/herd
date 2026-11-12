# herdlet and herd-egress as a Nomad system job: one of each on every Linux client (docs/runbooks/nomad.md). Each
# herdlet is its node's SPIFFE identity, spiffe://<td>/herd/node/<worker>, from the node's SPIRE agent (ADR 0001
# A-60; herd-egress is spiffe://<td>/herd/egress/<worker>): no join token, its node's settings asked of herd-api by that
# SVID at its first start and reused after; without the agent neither starts. It keeps snapshots in the S3 store the join hands it, so it takes no -snapshots. The same variable's registry_auth, a
# docker config file, is the node's registry logins for private template images (-registry-auth, rendered 0600).
# insecure_registries are registry hosts it pulls from over plain HTTP (-insecure-registry, comma-separated; ""
# names none), a node-level fallback that defaults to none: a template's golden pull names its own plain-HTTP hosts
# (image_plain_http_hosts), the store and its token realm, so no job holds a cluster's address.
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
  default = ""
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
          "run", "-api", "${HERD_API}",
          "-state", "/var/lib/herdlet",
          "-egress-ca", "/var/lib/herd-egress/egress-ca.pem",
          "-kernel", "${NOMAD_TASK_DIR}/kernel",
          "-initrd", "${NOMAD_TASK_DIR}/initrd.img",
          "-virtiofsd", "${NOMAD_TASK_DIR}/virtiofsd",
          "-registry-auth", "secrets/registry-auth.json",
          "-insecure-registry", "${var.insecure_registries}",
          "-egress-allow-prefix", "${var.egress_allow_prefix}",
        ]
      }
    }

    # herd-egress needs no root and no capability (its ports are above 1024, SO_ORIGINAL_DST is a getsockopt on its own
    # socket): it runs as herd-egress, the uid its SPIRE entry selects, and keeps its CA in /var/lib/herd-egress and its
    # bindings acknowledgements in /run/herd, both its own (the runbook's tmpfiles.d line). The herdlet stays root.
    task "herd-egress" {
      driver = "raw_exec"
      user   = "herd-egress"

      artifact {
        source = "${var.release_url}/herd_linux_${attr.cpu.arch}.tar.gz"
      }

      template {
        data        = "HERD_API={{ range nomadService \"herd-api\" }}{{ .Address }}:{{ .Port }}{{ end }}\nHERD_ROUTER={{ range $i, $s := nomadService \"herd-router\" }}{{ if $i }},{{ end }}{{ .Address }}:{{ .Port }}{{ end }}"
        destination = "local/herd-api.env"
        env         = true
      }

      # The declared host services (-host-services-file, DEFAUL-1599): every Nomad service tagged herd-host-service is
      # <service>.herd.internal on its port, dialed at the address Nomad registered for it (its first instance), so an
      # actor whose policy allows that name reaches a service on a node's private address. Empty when none is
      # registered. herd-egress re-reads the file every -interval, so a change does not restart it.
      template {
        data        = "{{ range nomadServices }}{{ if .Tags | contains \"herd-host-service\" }}{{ $name := .Name }}{{ range $i, $s := nomadService .Name }}{{ if eq $i 0 }}{{ $name }}.herd.internal:{{ .Port }} {{ .Address }}:{{ .Port }}\n{{ end }}{{ end }}{{ end }}{{ end }}"
        destination = "local/host-services"
        change_mode = "noop"
      }

      config {
        command = "local/herd-egress"
        args = [
          "-api", "${HERD_API}",
          "-bindings", "/run/herd/egress-bindings.json",
          "-ca-out", "/var/lib/herd-egress/egress-ca.pem",
          "-host-services-file", "local/host-services",
          "-router", "${HERD_ROUTER}",
          "-egress-allow-prefix", "${var.egress_allow_prefix}",
        ]
      }
    }
  }
}
