//go:build windows

package process

import (
	"fmt"
	"os/exec"
)

// describeExitState names the chosen exit code on Windows. There are no
// signals to report (a terminated process is just an exit code), so the code
// plus the shared hint table is the full story an operator gets.
func describeExitState(exitErr *exec.ExitError) string {
	if state := exitErr.ProcessState; state != nil {
		return fmt.Sprintf("exited with code %d%s", state.ExitCode(), exitCodeHint(state.ExitCode()))
	}
	return exitErr.Error()
}
