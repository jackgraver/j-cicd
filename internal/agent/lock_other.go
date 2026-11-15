//go:build !unix

package agent

import (
	"context"
	"errors"
)

// lockFile is unsupported off unix: the agent drives docker compose on a
// Linux VPS, and deploying without the lock could overlap two rollouts.
func lockFile(ctx context.Context, path string, waiting func()) (func(), error) {
	return nil, errors.New("deploy locking is only supported on unix")
}
