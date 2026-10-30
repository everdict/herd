# herd-api and herd-router as a Nomad service job (docs/runbooks/nomad.md). Nomad supervises the processes; herd
# places the actors. The bbolt store allows one herd-api, so count = 1 on the node that holds the herd-data host
# volume. Snapshots go to the S3 bucket named in the job's Nomad variable, which joins hand to every herdlet.
# Rendered by deploy/artifacts; edit the template, not this file.
variable "release_url" {
  type    = string
  default = "https://github.com/everdict/herd/releases/latest/download"
}

job "herd-api" {
  type = "service"

  group "herd-api" {
    count = 1

    network {
      port "api" {
        static = 7443
      }
      port "router" {
        static = 8443
      }
      port "tunnel" {
        static = 7444
      }
    }

    volume "herd-data" {
      type   = "host"
      source = "herd-data"
    }

    service {
      name     = "herd-api"
      port     = "api"
      provider = "nomad"
    }

    service {
      name     = "herd-router"
      port     = "router"
      provider = "nomad"
    }

    task "herd-api" {
      driver = "exec"

      artifact {
        source = "${var.release_url}/herd_linux_${attr.cpu.arch}.tar.gz"
      }

      volume_mount {
        volume      = "herd-data"
        destination = "/var/lib/herd"
      }

      template {
        data        = "{{ with nomadVar \"nomad/jobs/herd-api\" }}{{ .jwt_key }}{{ end }}"
        destination = "secrets/jwt.key"
        perms       = "0600"
      }

      template {
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/herd-api" }}HERD_S3={{ .s3 }}
          HERD_S3_ENDPOINT={{ .s3_endpoint }}
          AWS_ACCESS_KEY_ID={{ .aws_access_key_id }}
          AWS_SECRET_ACCESS_KEY={{ .aws_secret_access_key }}{{ end }}
        EOT
        destination = "secrets/s3.env"
        perms       = "0600"
        env         = true
      }

      config {
        command = "local/herd-api"
        args = [
          "-listen", "0.0.0.0:${NOMAD_PORT_api}",
          "-key-file", "secrets/jwt.key",
          "-store", "bolt:/var/lib/herd/herd.db",
          "-snapshots", "/var/lib/herd/snapshots",
          "-advertise-tunnel", "${NOMAD_ADDR_tunnel}",
          "-s3", "${HERD_S3}",
          "-s3-endpoint", "${HERD_S3_ENDPOINT}",
        ]
      }
    }

    task "herd-router" {
      driver = "exec"

      artifact {
        source = "${var.release_url}/herd_linux_${attr.cpu.arch}.tar.gz"
      }

      template {
        data        = "{{ with nomadVar \"nomad/jobs/herd-api\" }}{{ .jwt_key }}{{ end }}"
        destination = "secrets/jwt.key"
        perms       = "0600"
      }

      config {
        command = "local/herd-router"
        args = [
          "-listen", "0.0.0.0:${NOMAD_PORT_router}",
          "-tunnel-listen", "0.0.0.0:${NOMAD_PORT_tunnel}",
          "-api", "${NOMAD_ADDR_api}",
          "-key-file", "secrets/jwt.key",
        ]
      }
    }
  }
}
