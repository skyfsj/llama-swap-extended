package process

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestProcessCommand_CrashRecorderReceivesExitAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell crash fixture is POSIX-only")
	}
	logger := logmon.NewWriter(io.Discard)
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:           "sh -c 'echo inference-crash-fixture >&2; exit 7'",
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "/health",
	}, logger, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	type crashEvent struct {
		model string
		log   string
		err   error
	}
	received := make(chan crashEvent, 1)
	p.SetCrashRecorder(func(modelID string, processLog []byte, exitErr error) {
		received <- crashEvent{model: modelID, log: string(processLog), err: exitErr}
	})

	startErr := p.EnsureReady(context.Background(), testStartTimeout)
	select {
	case event := <-received:
		if event.model != t.Name() {
			t.Errorf("model=%q, want %q", event.model, t.Name())
		}
		if !strings.Contains(event.log, "inference-crash-fixture") {
			t.Errorf("process log=%q, missing crash fixture output", event.log)
		}
		if event.err == nil || !strings.Contains(event.err.Error(), "exit status 7") {
			t.Errorf("exit error=%v, want exit status 7", event.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash recorder was not called")
	}
	// The lifecycle error must name the mechanism, not forward Go's wrapper
	// verbatim: a bare "exit status 7" never told an operator whether the
	// process chose that code or was killed by a signal.
	if startErr == nil || !strings.Contains(startErr.Error(), "code 7") {
		t.Fatalf("start error=%v, want the upstream exit code", startErr)
	}
	if startErr == nil && p.State() == StateReady {
		t.Fatal("crashed process remained ready")
	}
}
