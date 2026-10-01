package logmon

import (
	"os/exec"
	"testing"
	"time"
)

// childRunTimeout bounds how long a test waits for a child process before
// calling it hung. It only has to stay below the test binary's own panic
// timeout: the children in these tests finish in well under a second, so the
// margin absorbs the CPU contention of a full `-race ./...` run instead of
// turning that contention into a false failure.
const childRunTimeout = 30 * time.Second

// startChild launches cmd and reaps it on a background goroutine. Waiting this
// way — rather than calling cmd.Run inside the goroutine — keeps cmd.Process
// published before the caller can touch it: os/exec assigns that field inside
// Start, so reading it from the test goroutine while Run was still starting the
// process is a data race, and the detector rightly flags it.
func startChild(t *testing.T, cmd *exec.Cmd) <-chan error {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

// awaitChild returns the child's exit error, or kills it and fails the test if
// it outlives childRunTimeout. what names the behaviour under test so the
// failure explains which contract broke.
func awaitChild(t *testing.T, cmd *exec.Cmd, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(childRunTimeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%s: still running after %v", what, childRunTimeout)
		return nil
	}
}
