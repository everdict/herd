// Command renderdist writes herd's install artifacts (package deploy/artifacts) under -out, normally dist/.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/everdict/herd/deploy/artifacts"
)

func main() {
	out := flag.String("out", "dist", "directory to write the artifacts under")
	flag.Parse()
	files, err := artifacts.Render(artifacts.Defaults)
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(*out, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
