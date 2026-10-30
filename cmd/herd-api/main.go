// Command herd-api serves the Substrate Control subset and herd's Capabilities extension, accepts herdlets'
// outbound links and answers their joins. It never dials a herdlet. The flags are package deploy's (deploy.API).
//
//	herd-api -listen 0.0.0.0:7443 -key-file /etc/herd/jwt.key -snapshots /var/lib/herd/snapshots -store bolt:/var/lib/herd/herd.db \
//	    -advertise-tunnel herd.example:7444
//	herd-api join-token -key-file /etc/herd/jwt.key -ttl 24h
//	herd-api token -key-file /etc/herd/jwt.key -sub everdict -space team-a=write
package main

import (
	"os"

	"github.com/everdict/herd/deploy"
)

func main() { os.Exit(deploy.Main("herd-api", os.Args[1:])) }
