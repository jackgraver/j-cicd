package cli

import "runtime/debug"

// Version returns stamped if the build set one (-ldflags "-X
// main.version=..."), else the git revision Go recorded when it built
// from a checkout (suffixed -dirty for uncommitted changes), else "dev".
// That makes a plain `go build` or `go install ./cmd/...` traceable to a
// commit with no build tooling beyond Go itself.
func Version(stamped string) string {
	if stamped != "" {
		return stamped
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
