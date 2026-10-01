package process

import (
	"os/exec"
	"syscall"
	"testing"
)

// If Setpgid did not take effect the child would NOT be a group leader, and
// kill(-pid) would target a group id that may not exist (ESRCH) — the fallback
// then signals the child alone. This test proves the child really is its own
// group leader, which is the precondition for the negative-PID signal to be
// safe and correctly scoped.
func TestSetProcAttributes_ChildBecomesGroupLeader(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	cmd := exec.Command("sh", "-c", "sleep 5")
	setProcAttributes(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Fatalf("child pgid=%d, want pid=%d (not a group leader: -pid signal would be misdirected)",
			pgid, cmd.Process.Pid)
	}
	// And it must not share llama-swap's own group.
	self, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("Getpgid(self): %v", err)
	}
	if pgid == self {
		t.Fatalf("child shares the parent's group %d: kill(-pid) could signal llama-swap itself", self)
	}
}
