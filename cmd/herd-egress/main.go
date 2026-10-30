// Command herd-egress is the per-host egress gateway (ADR 0001 A-18). On Ubuntu it serves the TCP flows and DNS
// queries nftables redirects from each VM's TAP (render a VM's TAP and table with `herd-egress nft`); it reads each
// bound actor's EgressPolicy from herd-api and applies updates to live actors within -interval. The flags are package
// deploy's (deploy.Egress).
//
//	herd-egress -api 127.0.0.1:7443 -token-file /var/lib/herdlet/egress.token -bindings /run/herd/egress-bindings.json \
//	    -ca-out /var/lib/herdlet/egress-ca.pem -secrets /etc/herd/secrets -host-addr 203.0.113.7 -host-name herd-api.example
//	herd-egress nft -id a1b2c3 -tap herd-a1b2c3 -host 10.88.0.1/30 -vm 10.88.0.2
//
// The bindings file is a JSON list of {"space","name","uid","source"} (source: the VM's TAP address); herdlet writes
// it, and herd-egress re-reads it every -interval. macOS hosts serve the same gateway behind a userspace stack per VM
// (egress.Stack), which lands with R7.
package main

import (
	"os"

	"github.com/everdict/herd/deploy"
)

func main() { os.Exit(deploy.Main("herd-egress", os.Args[1:])) }
