//go:build !unix

package pipeline

// inspectDockerHost has no portable way to probe the daemon here, so the
// error falls back to the generic "can't reach a Docker daemon".
func inspectDockerHost() dockerDiagnosis {
	return dockerDiagnosis{problem: problemUnknown}
}
