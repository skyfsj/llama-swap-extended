package process

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type ProcessState string

const (
	StateStopped  ProcessState = ProcessState("stopped")
	StateStarting ProcessState = ProcessState("starting")
	StateReady    ProcessState = ProcessState("ready")
	StateStopping ProcessState = ProcessState("stopping")
	// StateSleeping is the public lifecycle projection used by status events
	// while a vLLM process remains alive and can be woken for the next request.
	// Process.State itself intentionally remains StateReady so the router does
	// not start a second process for a sleeping-but-reusable listener.
	StateSleeping ProcessState = ProcessState("sleeping")

	// process is shutdown and will not be restarted
	StateShutdown ProcessState = ProcessState("shutdown")
)

var (
	// ErrSleepUnsupported means the process does not expose the vLLM sleep
	// control surface.
	ErrSleepUnsupported = errors.New("process sleep control is not supported")
	// ErrSleepUnavailable means the process exists but is not currently ready
	// for an explicit sleep/wake operation.
	ErrSleepUnavailable = errors.New("process is not ready for sleep control")
)

// SleepController is an optional process capability. It is deliberately kept
// outside Process so embedders with custom Process implementations do not need
// to add lifecycle methods they cannot support.
type SleepController interface {
	Sleep(context.Context, int) error
	Wake(context.Context) error
	Sleeping() bool
}

type Process interface {
	// Run starts the process blocks until the process is terminated.
	// The timeout parameter controls how long to wait for the process to get
	// to a ready state to process traffic
	Run(timeout time.Duration) error

	// WaitReady blocks until the process is ready to serve requests
	// or the context is cancelled. It returns nil when the process is ready
	//
	// WaitReady only subscribes, it never starts anything, and it cannot tell
	// "stopped, but a start is coming" apart from "stopped, and nothing is
	// coming" — a subscription that arrives after a start has already failed or
	// been aborted waits for a process nobody is going to start. Pass a context
	// with a deadline if that matters. Callers that want the process serving
	// should use EnsureReady, which has neither problem.
	WaitReady(context.Context) error

	// EnsureReady brings the process to a ready state and blocks until it is
	// serving, the start fails, or ctx is cancelled. The timeout parameter
	// controls how long to wait for the process to become ready.
	//
	// Unlike Run, the decision of whether a start is needed is made inside the
	// process's own state machine, so callers never inspect State() first and
	// cannot race a concurrent transition:
	//
	//	ready    -> returns nil immediately
	//	stopped  -> starts the process and waits for it to become ready
	//	stopping -> waits for the stop to finish, then starts
	//	shutdown -> returns an error
	EnsureReady(ctx context.Context, timeout time.Duration) error

	// Stop blocks until the process has terminated. It returns nil when
	// the process terminated as expected (exit 0)
	Stop(timeout time.Duration) error

	// State returns the current state of the process
	// Note: this is a snapshot of the state at the time of the call
	// and may change at any time after the call returns.
	State() ProcessState

	// ServeHTTP forwards requests to the underlying process
	// Calling it when the process is not ready will result in a
	// 503 response with an error body identifying llama-swap as the source
	ServeHTTP(http.ResponseWriter, *http.Request)

	// Logger returns the monitor that captures this process's stdout/stderr.
	Logger() *logmon.Monitor
}
