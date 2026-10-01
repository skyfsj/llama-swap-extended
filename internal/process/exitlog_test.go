package process

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// TestProcessCommand_ExitDiagnosticReachesModelLog pins the wiring an operator
// depends on: the model log panel reads the process monitor, so a lifecycle
// diagnostic written only to the proxy logger leaves the panel ending mid-line
// with no explanation. The two monitors are deliberately distinct here — the
// shared test helper passes the same monitor for both roles, which hides this
// class of bug entirely.
func TestProcessCommand_ExitDiagnosticReachesModelLog(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	modelLog := logmon.NewWriter(io.Discard)
	proxyLog := logmon.NewWriter(io.Discard)

	// Exits non-zero immediately, before any readiness check can pass.
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:           "sh -c 'exit 7'",
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "/health",
	}, modelLog, proxyLog)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	_ = p.EnsureReady(context.Background(), 3*time.Second)

	history := string(modelLog.GetHistory())
	if !strings.Contains(history, "process exited before becoming ready") {
		t.Fatalf("model log = %q, want the pre-ready exit diagnostic", history)
	}
	if !strings.Contains(history, "code 7") {
		t.Fatalf("model log = %q, want the exit code named", history)
	}
}

// TestProcessCommand_ExitDiagnosticIsNotDuplicated guards the other side: the
// prefix writer forwards the process log into the shared log, so reporting a
// death twice would show the operator the same sentence in two places.
func TestProcessCommand_ExitDiagnosticIsNotDuplicated(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	modelLog := logmon.NewWriter(io.Discard)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:           "sh -c 'exit 9'",
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "/health",
	}, modelLog, modelLog)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	_ = p.EnsureReady(context.Background(), 3*time.Second)

	history := string(modelLog.GetHistory())
	if got := strings.Count(history, "process exited before becoming ready"); got != 1 {
		t.Fatalf("pre-ready diagnostic appears %d times, want exactly 1:\n%s", got, history)
	}
	// The Wait goroutine used to report the same death a second time as an
	// "unexpected" exit, so one death produced two ERROR lines.
	if got := strings.Count(history, "process exited unexpectedly"); got != 0 {
		t.Fatalf("death reported twice (unexpected line present %d times):\n%s", got, history)
	}
	if got := strings.Count(history, "exited with code 9"); got != 1 {
		t.Fatalf("exit code reported %d times, want exactly 1:\n%s", got, history)
	}
}
