package process

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// TestDescribeExitNamesExitCode pins the operator-facing wording for a process
// that chose its own exit status. The previous lifecycle error forwarded Go's
// wrapper untouched, so a reader could not tell a deliberate exit from a kill.
func TestDescribeExitNamesExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 120")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	got := DescribeExit(err)
	if !strings.Contains(got, "exited with code 120") {
		t.Fatalf("DescribeExit = %q, want it to name exit code 120", got)
	}
}

// TestDescribeExitNamesSignal is the other half of the contract: a process
// killed by a signal must be reported as such, because "exit status 137" and
// "killed by signal SIGKILL" describe very different investigations.
func TestDescribeExitNamesSignal(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
		t.Skipf("cannot signal sleep: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected the killed process to report an error")
	}
	got := DescribeExit(err)
	if !strings.Contains(got, "killed by signal") || !strings.Contains(got, "SIGKILL") {
		t.Fatalf("DescribeExit = %q, want it to name SIGKILL", got)
	}
}

// TestDescribeExitHandlesCleanAndUnknownErrors keeps the helper total: the
// death-reporting path must never itself panic or return an empty string.
func TestDescribeExitHandlesCleanAndUnknownErrors(t *testing.T) {
	if got := DescribeExit(nil); got == "" {
		t.Fatal("nil exit error must still render something")
	}
	if got := DescribeExit(exec.ErrNotFound); !strings.Contains(got, "executable file not found") {
		t.Fatalf("DescribeExit(non-exit error) = %q, want the wrapped message", got)
	}
}

// TestDescribeExitWithMemorySuppressesOomGuessWhenWeSentTheSignal pins the
// distinction that misled a real investigation: a SIGKILL that llama-swap
// itself sent (start deadline, stop, shutdown) must still be reported, but
// without the "kernel OOM killer" speculation, which sends the reader after a
// memory problem that may not exist.
func TestDescribeExitWithMemorySuppressesOomGuessWhenWeSentTheSignal(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
		t.Skipf("cannot signal sleep: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected the killed process to report an error")
	}

	selfInflicted := DescribeExitWithMemory(err, true)
	if !strings.Contains(selfInflicted, "SIGKILL") {
		t.Fatalf("self-inflicted description = %q, want the signal named", selfInflicted)
	}
	if strings.Contains(selfInflicted, "OOM killer") {
		t.Fatalf("self-inflicted description = %q, must not speculate about the OOM killer", selfInflicted)
	}

	// The unsolicited path keeps the speculation: there, a SIGKILL really is
	// most often the kernel.
	unsolicited := DescribeExitWithMemory(err, false)
	if !strings.Contains(unsolicited, "SIGKILL") {
		t.Fatalf("unsolicited description = %q, want the signal named", unsolicited)
	}
}
