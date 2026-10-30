// Command herd is herd all-in-one: herd-api (bbolt store), herd-router, herd-egress and a herdlet in one process, the
// same components the separate binaries run; the herdlet joins the herd-api with a token the process mints. The flags
// are package deploy's (deploy.AllInOne).
//
//	herd -data /var/lib/herd
package main

import (
	"os"

	"github.com/everdict/herd/deploy"
)

func main() { os.Exit(deploy.Main("herd", os.Args[1:])) }
