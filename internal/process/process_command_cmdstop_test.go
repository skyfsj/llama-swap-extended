//go:build !windows

package process

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// waitForLog polls buf until it contains every string in want or the deadline
// passes. CmdStop runs in its own goroutine and its pipe copy can land just
// after Stop returns.
func waitForLog(buf *syncBuffer, want ...string) bool {
	hasAll := func() bool {
		out := buf.String()
		for _, w := range want {
			if !strings.Contains(out, w) {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(testReturnTimeout)
	for !hasAll() && time.Now().Before(deadline) {
		time.Sleep(testLogPollInterval)
	}
	return hasAll()
}

// TestProcessCommand_CmdStopOutputLogged verifies that stdout and stderr from
// CmdStop are written to the process logger, the same as the start command.
// It lives in this file for the !windows build tag since it needs sh.
func TestProcessCommand_CmdStopOutputLogged(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	// printf builds the markers at runtime so they never appear verbatim in
	// the command text, which is itself logged in some paths.
	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" out; printf "cmdstop-%s\n" err >&2; kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-out", "cmdstop-err") {
		t.Errorf("expected process log to contain CmdStop stdout and stderr; got:\n%s", logBuf.String())
	}
}

// TestProcessCommand_CmdStopBackgroundChildNotAnError verifies that a CmdStop
// which exits successfully but leaves a background child holding its output
// open is bounded by waitDelay and not reported as a failed stop.
func TestProcessCommand_CmdStopBackgroundChildNotAnError(t *testing.T) {
	skipIfNoSimpleResponder(t)

	port := getFreePort(t)
	logBuf := &syncBuffer{}
	procLogger := logmon.NewWriter(logBuf)
	proxyLogger := logmon.NewWriter(io.Discard)

	p, err := New(context.Background(), t.Name(), config.ModelConfig{
		Cmd:                fmt.Sprintf("%s -port %d -silent", simpleResponderPath, port),
		CmdStop:            `sh -c 'printf "cmdstop-%s\n" bg; sleep 2 & kill -TERM ${PID}'`,
		Proxy:              fmt.Sprintf("http://127.0.0.1:%d", port),
		CheckEndpoint:      "/health",
		HealthCheckTimeout: 10,
	}, procLogger, proxyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.waitDelay = 200 * time.Millisecond

	_ = runAsync(t, p)
	if err := p.Stop(testStopTimeout); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !waitForLog(logBuf, "cmdstop-bg", "child held its output open") {
		t.Fatalf("expected CmdStop output and WaitDelay warning in process log; got:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "stop command failed") {
		t.Errorf("CmdStop that exited cleanly was logged as a failure:\n%s", logBuf.String())
	}
}
