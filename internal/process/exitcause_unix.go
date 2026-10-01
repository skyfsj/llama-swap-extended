//go:build !windows

package process

import (
	"fmt"
	"os/exec"
	"syscall"
)

// describeExitState names the signal for a signalled child. Go's own
// ProcessState.String already says "signal: killed", but the raw signal name
// and number are what an operator needs to tell an OOM kill (SIGKILL) from a
// crash (SIGSEGV/SIGABRT) from a deliberate stop (SIGTERM).
func describeExitState(exitErr *exec.ExitError) string {
	state := exitErr.ProcessState
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			signal := ws.Signal()
			// syscall.Signal.String() renders the *action* ("killed",
			// "terminated"), not the constant an operator needs; the numeric
			// value plus a stable SIG* name is what maps onto a kill -9, an OOM
			// kill, or a crash.
			return fmt.Sprintf("killed by signal %s (%d)%s", signalName(signal), int(signal), signalHint(signal))
		}
		return fmt.Sprintf("exited with code %d%s", ws.ExitStatus(), exitCodeHint(ws.ExitStatus()))
	}
	return exitErr.Error()
}

// exitCodeHint annotates the exit codes that carry a well-known meaning, so a
// bare number does not read as an unexplained failure.
func exitCodeHint(code int) string {
	switch code {
	case 1:
		return " (generic failure)"
	case 2:
		return " (usage / argument error)"
	case 137:
		return " (128+SIGKILL — killed by the kernel, typically out of memory)"
	case 139:
		return " (128+SIGSEGV — segmentation fault)"
	case 143:
		return " (128+SIGTERM — terminated)"
	default:
		return ""
	}
}

// signalName reports the conventional SIG* constant for a signal, falling
// back to the platform rendering for the long tail this package does not
// specialise.
func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGTRAP:
		return "SIGTRAP"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGALRM:
		return "SIGALRM"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return signal.String()
	}
}

// signalHint interprets the signals a supervised model server actually dies
// from.
func signalHint(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGKILL:
		return " — forced kill (nothing the process can catch)"
	case syscall.SIGSEGV:
		return " — segmentation fault in the upstream process"
	case syscall.SIGABRT:
		return " — abort (assertion or allocation failure inside the upstream process)"
	case syscall.SIGTERM:
		return " — terminated (graceful stop request)"
	case syscall.SIGBUS:
		return " — bus error, often a bad memory-mapped file or device access"
	default:
		return ""
	}
}
