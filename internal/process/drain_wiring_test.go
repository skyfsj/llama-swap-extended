package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// stalledSink blocks every write until unblocked. It stands in for the log
// destinations that stall in production: a terminal whose reader has gone away,
// a container log pipe nobody is draining.
type stalledSink struct {
	release chan struct{}
	once    sync.Once
}

func newStalledSink() *stalledSink {
	return &stalledSink{release: make(chan struct{})}
}

func (s *stalledSink) unblock() {
	s.once.Do(func() { close(s.release) })
}

func (s *stalledSink) Write(p []byte) (int, error) {
	<-s.release
	return len(p), nil
}

// TestProcessCommand_StalledLogSinkDoesNotStallChild is the regression for the
// production failure. The writer bound to a child's stdout used to be the log
// monitor itself, so every byte the child printed ran the whole formatting and
// terminal-write path on os/exec's copier goroutine. A sink that stopped
// accepting output therefore stopped the copier, the child's pipe filled up,
// and the child died on its next write — which reached the operator as a model
// crash.
func TestProcessCommand_StalledLogSinkDoesNotStallChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	// The child emits far more than a pipe buffer holds, then leaves proof that
	// it reached the end of its own output.
	marker := filepath.Join(t.TempDir(), "child-finished")
	command := fmt.Sprintf("sh -c 'head -c 200000 /dev/zero; touch %s'", marker)

	sink := newStalledSink()
	defer sink.unblock()
	modelLog := logmon.NewWriter(sink)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:           command,
		Proxy:         "http://127.0.0.1:1",
		CheckEndpoint: "/health",
	}, modelLog, modelLog)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	readyDone := make(chan error, 1)
	go func() { readyDone <- p.EnsureReady(context.Background(), 15*time.Second) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never finished while the log sink was stalled: the sink's backpressure reached the child's stdout")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Let the sink accept output again so the drain can finish and the process
	// tear itself down.
	sink.unblock()
	select {
	case <-readyDone:
	case <-time.After(testStopTimeout):
	}

	// The marker alone could be reached without the output ever surviving, so
	// assert the bytes actually landed too.
	if got := len(modelLog.GetHistory()); got < 200000 {
		t.Fatalf("model log history holds %d bytes, want the child's 200000", got)
	}
}
