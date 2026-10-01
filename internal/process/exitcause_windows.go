//go:build windows

package process

import (
	"os/exec"
)

// describeExitState is the windows fallback: the syscall.WaitStatus signal
// inspection in exitcause_unix.go does not compile on windows, so the
// standard Go exit description is the best available detail.
func describeExitState(exitErr *exec.ExitError) string {
	return exitErr.Error()
}
