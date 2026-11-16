//go:build unix

package pipeline

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultDockerSocket = "/var/run/docker.sock"

// inspectDockerHost works out why a local Docker daemon can't be reached
// by trying its socket directly, which tells "not running" apart from "not
// allowed" without parsing the docker CLI's error text. It only looks; it
// changes nothing.
func inspectDockerHost() dockerDiagnosis {
	sock, ok := localDockerSocket()
	if !ok {
		// A remote or non-default context: nothing local to inspect.
		return dockerDiagnosis{problem: problemUnknown}
	}
	diag := dockerDiagnosis{socket: sock}

	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	switch {
	case err == nil:
		conn.Close()
		diag.problem = problemUnknown
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		diag.problem = problemNotRunning
	case errors.Is(err, fs.ErrPermission):
		diag.problem, diag.group = accessProblem(sock)
	default:
		diag.problem = problemUnknown
	}
	return diag
}

// accessProblem decides whether a permission-denied socket is fixed by
// joining its group or just by starting a new session.
func accessProblem(sock string) (dockerProblem, string) {
	info, err := os.Stat(sock)
	if err != nil {
		return problemNoAccess, "docker"
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return problemNoAccess, "docker"
	}
	gid := strconv.FormatUint(uint64(st.Gid), 10)
	group := gid
	if g, err := user.LookupGroupId(gid); err == nil {
		group = g.Name
	}

	// The group database says who's a member now; this process's groups
	// are whatever its session had when it started.
	if u, err := user.Current(); err == nil {
		if ids, err := u.GroupIds(); err == nil && contains(ids, gid) && !processHasGroup(int(st.Gid)) {
			return problemStaleSession, group
		}
	}
	return problemNoAccess, group
}

func processHasGroup(gid int) bool {
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == gid {
			return true
		}
	}
	return os.Getegid() == gid
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// localDockerSocket returns the unix socket the docker CLI will use, if
// that can be known: DOCKER_HOST when it's unix://, else the default
// socket unless a non-default docker context is selected.
func localDockerSocket() (string, bool) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		path, ok := strings.CutPrefix(host, "unix://")
		return path, ok
	}
	if ctx := os.Getenv("DOCKER_CONTEXT"); ctx != "" && ctx != "default" {
		return "", false
	}
	if ctx := currentDockerContext(); ctx != "" && ctx != "default" {
		return "", false
	}
	return defaultDockerSocket, true
}

// currentDockerContext reads the context `docker context use` selected.
func currentDockerContext() string {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return cfg.CurrentContext
}
