// Package scheduler contains the request-scheduling strategies used by the
// router's baseRouter. A Scheduler owns the queue, in-flight tracking, and the
// decision tree for when to start a swap versus queue a request. The baseRouter
// owns the channels, run loop, and process machinery, and exposes the
// side-effects a scheduler needs through the Effects interface.
//
// Splitting these apart lets the scheduling strategy be swapped out
// independently of both the process machinery (baseRouter) and the eviction
// policy (Swapper). FIFO is the first and currently only implementation.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ErrModelNotFound is granted to callers whose model is not handled by this
// router. It is an alias for swaputil.ErrNoLocalModelFound.
var ErrModelNotFound = swaputil.ErrNoLocalModelFound

var (
	// ErrRestartNotPending is returned when an operator asks to restart a
	// model whose effective process configuration is already applied.
	ErrRestartNotPending = errors.New("model has no pending configuration restart")
	// ErrRestartInProgress is kept separate for callers that want to render a
	// precise control-plane status. The restart endpoint treats it as an
	// idempotent success and returns the current lifecycle state.
	ErrRestartInProgress = errors.New("model restart is already in progress")
)

const invalidRestartGeneration = ^uint64(0)

const (
	ConfigStatusApplied     = "applied"
	ConfigStatusModified    = "modified"
	ConfigStatusDraining    = "draining"
	ConfigStatusRestarting  = "restarting"
	ConfigStatusRollingBack = "rolling_back"
	ConfigStatusApplyFailed = "apply_failed"
	ConfigStatusRemoving    = "removing"
	ConfigStatusUnloading   = "unloading"
)

// ModelLifecycleStatus is the read-only status projection used by the server
// API and SSE modelStatus event. It is produced by the scheduler's single
// event loop and copied before being returned to concurrent readers.
type ModelLifecycleStatus struct {
	ConfigStatus    string
	AppliedRevision uint64
	DesiredRevision uint64
	OldRequests     int
	WaitingRequests int
	Error           string
	// Maintenance reports the model is out of service after a failed
	// configuration start, because auto-rollback is disabled. The reason is
	// the start error; the model leaves this state once a later start
	// succeeds and serves a request.
	Maintenance bool
}

// RestartDone is emitted by the router process seam after a requested model
// restart. RolledBack means the desired process failed but the last known-good
// process was recreated and can continue serving waiting requests. Maintenance
// means the operator disabled auto-rollback, so the desired process failed and
// the model was left out of service instead of being restored.
type RestartDone struct {
	ModelID     string
	Generation  uint64
	Err         error
	RolledBack  bool
	Maintenance bool
}

// RestartProgress is emitted while a confirmed restart is draining, creating
// the desired generation, or restoring the last known-good generation.
type RestartProgress struct {
	ModelID    string
	Generation uint64
	Status     string
	Err        error
}

// RemovalDone is emitted after a deleted model has drained and its process has
// been stopped and removed from the local router registry.
type RemovalDone struct {
	ModelID    string
	Generation uint64
	Err        error
}

// Swapper is the eviction policy: it decides which running models must be
// stopped before a target can serve. It is orthogonal to the scheduling
// strategy — any Scheduler works with any Swapper.
type Swapper interface {
	// EvictionFor returns running model IDs that must be stopped before
	// target can serve. running is the complete set the scheduler considers
	// live: every process that is not stopped, unioned with the targets of
	// in-flight swaps the scheduler has already committed to (which are not yet
	// visible in process state). The planner does not inspect process state
	// itself. Pure decision; must not log.
	EvictionFor(target string, running []string) []string

	// OnSwapStart runs once at the start of every swap, with the same running
	// set EvictionFor was given for this decision. Planners may log their
	// decision here at whatever verbosity they choose.
	OnSwapStart(target string, running []string)
}

// Scheduler decides what happens to each event the router's run loop receives.
// All methods run on that single run-loop goroutine, so implementations need no
// internal locking for their own state.
type Scheduler interface {
	// OnRequest handles one incoming ServeHTTP request.
	OnRequest(req HandlerReq)
	// OnCancel handles a request whose client has disconnected before it was
	// granted. The scheduler must remove the request from its queue and from
	// any in-flight swap's waiters so it never triggers a model load or grant
	// for a caller that is no longer there.
	OnCancel(req HandlerReq)
	// OnSwapDone handles a swap goroutine reporting completion.
	OnSwapDone(ev SwapDone)
	// OnServeDone handles a tracked ServeHTTP finishing (in-flight decrement).
	OnServeDone(ev ServeDoneEvent)
	// OnUnload reconciles scheduler state for an unload and drains the queue.
	// Most targets are stopped synchronously via Effects, so Unload blocks
	// until they exit; a target whose swap goroutine is still starting it is
	// kept as a drain barrier and stopped from OnSwapDone once the goroutine
	// can no longer resurrect it.
	OnUnload(targets []string, timeout time.Duration)
	// OnShutdown grants err to every waiter the scheduler still holds (active
	// swap waiters and queued requests). Process teardown is the baseRouter's
	// responsibility.
	OnShutdown(err error)
}

// Effects is implemented by the baseRouter. The scheduler calls back through it
// for every side-effect: inspecting process state, launching swaps, responding
// to callers, and stopping processes.
type Effects interface {
	// ModelState returns the current state of a model's process. ok is false
	// when the model is not handled by this router.
	ModelState(modelID string) (process.ProcessState, bool)
	// RunningModels returns the state of every process that is not stopped or
	// shut down, keyed by model ID. The scheduler uses it to build the running
	// set it hands the Swapper.
	RunningModels() map[string]process.ProcessState
	// StartSwap launches the swap goroutine for modelID, stopping evict first.
	StartSwap(modelID string, evict []string)
	// GrantError responds to a caller with an error.
	GrantError(req HandlerReq, err error)
	// GrantServe hands a caller the wrapped handler for modelID and reports
	// whether the caller was still there to receive it. The scheduler bumps
	// its in-flight count only when this returns true.
	GrantServe(req HandlerReq, modelID string) bool
	// StopProcesses stops the named processes in parallel and blocks until all
	// have stopped. Unknown IDs are skipped.
	StopProcesses(timeout time.Duration, ids []string)
}

// New returns a Scheduler selected by conf.Routing.Scheduler.Use, configured
// from conf and bound to the given planner and effects. Currently only "fifo"
// (the default) is supported.
func New(conf config.Config, name string, logger *logmon.Monitor, planner Swapper, eff Effects) (Scheduler, error) {
	use := conf.Routing.Scheduler.Use
	if use == "" {
		use = "fifo"
	}
	switch use {
	case "fifo":
		return NewFIFO(name, logger, planner, conf.Routing.Scheduler.Settings.Fifo, conf.Models, eff), nil
	default:
		return nil, fmt.Errorf("unsupported scheduler type: %q", use)
	}
}

// HandlerReq is one in-flight ServeHTTP request waiting for a routing decision.
type HandlerReq struct {
	Model      string
	Ctx        context.Context
	Admit      chan error
	Respond    chan HandlerResp
	PositionCh chan int
}

// HandlerResp is the routing decision returned to a HandlerReq's caller: either
// a handler to serve with, or an error.
type HandlerResp struct {
	HandleFunc http.HandlerFunc
	Err        error
}

// SwapDone is reported by a swap goroutine when its target is ready (or failed).
type SwapDone struct {
	ModelID string
	Err     error
}

// ServeDoneEvent is reported when a tracked ServeHTTP handler returns.
type ServeDoneEvent struct {
	ModelID string
	// Generation is the process generation that served the request. Zero is the
	// initial generation and remains the compatible value for older embedders.
	Generation uint64
	// Ignored marks a websocket that intentionally bypassed normal request
	// admission. It still participates in a confirmed config restart drain, but
	// it never consumed a scheduler reservation or in-flight slot.
	Ignored bool
}
