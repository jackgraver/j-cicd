// j-cicd is a self-hosted CI/CD pipeline: it builds, tests, and packages a
// project, then pushes the result to an image registry for deployment.
package main

import "os"

func main() {
	os.Exit(appStart(os.Args[1:]))
}
