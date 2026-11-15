// j-cicd-agent runs on the deploy host (the VPS). `j-cicd run` on the
// build host invokes it over ssh once images are pushed, and it rolls the
// new digests out through docker compose, health-checks them, and rolls
// back if they fail.
package main

import "os"

func main() {
	os.Exit(appStart(os.Args[1:]))
}
