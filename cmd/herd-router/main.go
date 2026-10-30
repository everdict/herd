// Command herd-router is resume-on-request ingress for herd actors (spec R3). Clients send requests naming
// "ate-target-actor: <atespace>/<actor>" with a bearer token; herdlets dial in and hold the tunnels requests reach
// their VMs through. The router dials only herd-api, never a node. The flags are package deploy's (deploy.Router).
//
//	herd-router -listen 0.0.0.0:8443 -tunnel-listen 0.0.0.0:7444 -api 127.0.0.1:7443 -key-file /etc/herd/jwt.key
package main

import (
	"os"

	"github.com/everdict/herd/deploy"
)

func main() { os.Exit(deploy.Main("herd-router", os.Args[1:])) }
