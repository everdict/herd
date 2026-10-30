// Command herdlet is herd's node agent (ADR A-19, A-20). It joins herd-api with one token (or dials with a herdlet
// credential), then dials out to herd-api (its link) and herd-router (its tunnel) and turns actor templates into
// microVMs on Cloud Hypervisor (Linux) or Virtualization.framework (macOS, cgo): rootfs images from the templates'
// OCI images, DurableDir volumes in each VM's virtio-fs share, warm VMs per template. The flags are package
// deploy's (deploy.Herdlet).
//
//	herdlet doctor
//	herdlet join herd-api.example:7443 --token <join token>
//	herdlet run
//	herdlet -api 10.0.0.1:7443 -token-file /etc/herd/node.jwt -worker node-1 -snapshots /srv/herd/snapshots -warm <template-uid>=2
package main

import (
	"os"

	"github.com/everdict/herd/deploy"
)

func main() { os.Exit(deploy.Main("herdlet", os.Args[1:])) }
