package scheduler

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// defaultConcurrencyLimit caps simultaneous in-flight requests per model when
// the model config leaves concurrencyLimit unset.
const defaultConcurrencyLimit = 10

// activeSwap tracks one in-flight swap and the callers waiting on it.
type activeSwap struct {
	modelID string
	evict   []string
	waiters []HandlerReq

	// pendingUnload marks a swap whose target was unloaded by the operator
	// while the swap goroutine was still running. The active entry is kept as
	// a drain barrier: StopProcesses must not race the goroutine's
	// EnsureReady, or the just-unloaded model is silently started again. The
	// deferred stop runs from OnSwapDone.
	pendingUnload bool
	unloadTimeout time.Duration
}

type lifecycleState struct {
	status               string
	draining             bool
	restarting           bool
	force                bool
	removing             bool
	unloading            bool
	pendingAgain         bool
	waiters              []HandlerReq
	err                  string
	oldAppliedRev        uint64
	desiredRevision      uint64
	revisionsInitialized bool
	restartGeneration    uint64
	staleRestarts        map[uint64]struct{}
	unloadTimeout        time.Duration
	unloadStarted        bool
	keepModified         bool
	// maintenance marks the model as out of service after a failed
	// configuration start with auto-rollback disabled. The router owns the
	// authoritative copy (it refuses requests there); this flag keeps the
	// published status and the waiting room in agreement with it while the
	// record lives.
	maintenance bool
}

// FIFO is the default scheduler. Requests are handled in a first-in, first-out order.
// To reduce swapping requests for a model that is already running will be handled
// immediately by the running process.
//
// Requests into this schedule are handled like this:
//
// A B C A B C --> A A B B C C
//
// The strategy is simple and reduces the number of swaps required.
type FIFO struct {
	name    string
	logger  *logmon.Monitor
	planner Swapper
	cfg     config.FifoConfig
	effects Effects

	limits   map[string]int
	active   map[string]*activeSwap
	reserved map[string]int
	inFlight map[string]int
	// serveGeneration fences completion events from requests that were already
	// handed to an old process when a force restart discarded that generation.
	// forcedOld keeps only the counters needed to retire those late events.
	serveGeneration map[string]uint64
	forcedOld       map[string]map[uint64]forcedServeGeneration
	queued          []HandlerReq
	lifecycle       map[string]*lifecycleState
}

type forcedServeGeneration struct {
	reserved int
	inFlight int
}

// NewFIFO builds a FIFO scheduler. Per-model concurrency limits are derived
// from models: each model's ConcurrencyLimit overrides defaultConcurrencyLimit
// when set to a value greater than zero.
func NewFIFO(name string, logger *logmon.Monitor, planner Swapper, cfg config.FifoConfig, models map[string]config.ModelConfig, eff Effects) *FIFO {
	limits := make(map[string]int, len(models))
	for id, mc := range models {
		limit := defaultConcurrencyLimit
		if mc.ConcurrencyLimit > 0 {
			limit = mc.ConcurrencyLimit
		}
		limits[id] = limit
	}

	return &FIFO{
		name:            name,
		logger:          logger,
		planner:         planner,
		cfg:             cfg,
		effects:         eff,
		limits:          limits,
		active:          make(map[string]*activeSwap),
		reserved:        make(map[string]int),
		inFlight:        make(map[string]int),
		serveGeneration: make(map[string]uint64),
		forcedOld:       make(map[string]map[uint64]forcedServeGeneration),
		lifecycle:       make(map[string]*lifecycleState),
	}
}

// OnRequest decides what to do with one incoming ServeHTTP request. It never
// blocks indefinitely: any work that has to wait (starting a process, stopping
// siblings, waiting for ready) is deferred to a swap goroutine and reported back
// via OnSwapDone.
//
// The decision tree, in order:
//
//  1. Unknown model — respond with ErrModelNotFound and move on.
//  2. A swap to the same model is already in flight — attach this waiter so
//     one swap serves all callers that asked for the same model.
//  3. Fast path — the target process is already ready, the planner sees
//     nothing to evict, and no in-flight swap is evicting it. Hand back its
//     ServeHTTP immediately.
//  4. Would collide with an in-flight swap (we'd stop their target, or they're
//     stopping us) — park in the queue for OnSwapDone to drain.
//  5. Would evict a process that is still handling requests — park in the
//     queue. OnServeDone will retry when the busy process drains.
//  6. Otherwise — start a new swap. This may run in parallel with other active
//     swaps when their evict sets don't intersect.
func (s *FIFO) OnRequest(req HandlerReq) {
	// (1) Unknown model.
	state, ok := s.effects.ModelState(req.Model)
	if !ok {
		s.logger.Debugf("%s: model %s not handled by this router", s.name, req.Model)
		s.rejectAdmission(req, ErrModelNotFound)
		return
	}

	// A confirmed restart fences new requests away from the old generation.
	// Requests already present in queued/active swap state are deliberately not
	// here; they continue through the normal decision tree and are drained
	// before the replacement process is created.
	if lifecycle := s.lifecycle[req.Model]; lifecycle != nil {
		switch {
		case lifecycle.removing || lifecycle.status == ConfigStatusUnloading:
			s.rejectAdmission(req, ErrModelNotFound)
			return
		case lifecycle.draining || lifecycle.restarting:
			s.enqueueRestartWaiter(req, lifecycle)
			return
		}
	}

	if !s.admit(req) {
		return
	}

	// (2) Join an in-flight swap for the same model.
	if sw, ok := s.active[req.Model]; ok {
		if sw.pendingUnload {
			// The operator unloaded this model while its swap was still
			// running; do not hand the dying swap new callers.
			s.grantError(req, s.unloadErr())
			return
		}
		s.logger.Debugf("%s: joining in-flight swap for model %s (%d waiters)", s.name, req.Model, len(sw.waiters)+1)
		sw.waiters = append(sw.waiters, req)
		return
	}

	running := s.runningSet(req.Model)
	evict := s.planner.EvictionFor(req.Model, running)

	// (3) Fast path: ready, nothing to evict, and nobody is evicting us.
	if state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active) {
		s.logger.Debugf("%s: fast-path serving model %s (already ready)", s.name, req.Model)
		s.grantHandler(req, req.Model)
		return
	}

	// (4) Collision with an in-flight swap — queue.
	if collidesWith(req.Model, evict, s.active) {
		s.logger.Debugf("%s: queuing request for model %s (collides with in-flight swap)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (5) Would evict a busy process — queue until it drains.
	if conflictsWithInFlight(evict, s.inFlight) {
		s.logger.Debugf("%s: queuing request for model %s (would evict in-flight process)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (5b) Would evict a model whose restart is committed — the replacement
	// process will reoccupy the model's planner slot, so starting this swap
	// now would leave the card/set shared by the replacement and this swap.
	if s.conflictsWithCommittedRestart(evict) {
		s.logger.Debugf("%s: queuing request for model %s (would evict restarting model)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// Compatibility websocket requests are normally outside scheduler
	// accounting, but a confirmed restart/removal must still wait for them.
	// Keep a conflicting swap from stopping that old generation underneath the
	// websocket while it is draining.
	if s.conflictsWithIgnoredLifecycle(evict) {
		s.logger.Debugf("%s: queuing request for model %s (would evict draining websocket)", s.name, req.Model)
		s.enqueue(req)
		return
	}

	// (6) Start a new (possibly parallel) swap.
	s.logger.Debugf("%s: starting swap for model %s, evicting %v", s.name, req.Model, evict)
	s.startSwap(req, evict, running)
}

// Reconfigure publishes the online scheduler settings without disturbing
// queues, active swaps, or in-flight counters. The router calls this from its
// single run loop, so replacing the planner and limits is atomic with respect
// to request events.
func (s *FIFO) Reconfigure(conf config.Config, planner Swapper) {
	s.cfg = conf.Routing.Scheduler.Settings.Fifo
	if planner != nil {
		s.planner = planner
	}
	limits := make(map[string]int, len(conf.Models))
	for id, model := range conf.Models {
		limit := defaultConcurrencyLimit
		if model.ConcurrencyLimit > 0 {
			limit = model.ConcurrencyLimit
		}
		limits[id] = limit
	}
	s.limits = limits
	s.drainQueue()
	s.maybeStartAllMaintenance()
}

// ModelRemovalPending reports whether a topology removal is still owned by the
// scheduler. The base router uses this distinction when a model is re-added
// while the old process's asynchronous Stop is still in flight.
func (s *FIFO) ModelRemovalPending(modelID string) bool {
	lifecycle := s.lifecycle[modelID]
	return lifecycle != nil && lifecycle.removing
}

// SwapInFlight reports whether a start (swap) goroutine is currently
// registered for modelID. Swaps are registered synchronously in OnRequest on
// the router's run loop, so a caller that also runs on that loop — the
// reconfigure path — can use this to detect a manual start whose goroutine
// has not yet flipped the process out of StateStopped.
func (s *FIFO) SwapInFlight(modelID string) bool {
	_, ok := s.active[modelID]
	return ok
}

// MarkConfigModified records that the desired model process differs from the
// process currently serving. It does not interrupt traffic. If a restart has
// already begun, the change is retained as the next pending revision and is
// applied by a later explicit confirmation.
func (s *FIFO) MarkConfigModified(modelID string) {
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil {
		lifecycle = &lifecycleState{}
		s.lifecycle[modelID] = lifecycle
	}
	if lifecycle.draining || lifecycle.restarting {
		lifecycle.pendingAgain = true
		return
	}
	// Re-adding a model while its removal is still pending cancels the
	// removal fence. If Stop has already started, OnRemoveDone will preserve
	// the re-added registry entry and the base router can refresh it.
	lifecycle.removing = false
	lifecycle.unloading = false
	lifecycle.status = ConfigStatusModified
	lifecycle.err = ""
}

// MarkConfigApplied clears a stale model-level pending state. It is used for
// stopped models whose new process configuration is already the next start
// configuration, and for models that were unchanged by a reconciliation.
func (s *FIFO) MarkConfigApplied(modelID string) {
	if lifecycle := s.lifecycle[modelID]; lifecycle != nil && !lifecycle.draining && !lifecycle.restarting {
		// A re-add cancels a removal that has not completed. The removal event
		// is ignored by OnRemoveDone once this flag is cleared.
		lifecycle.removing = false
		lifecycle.unloading = false
		delete(s.lifecycle, modelID)
	}
}

// RequestRestart confirms the latest pending model configuration. Repeated
// confirmations are idempotent while the model is draining or restarting.
func (s *FIFO) RequestRestart(modelID string) error {
	if _, ok := s.effects.ModelState(modelID); !ok {
		return ErrModelNotFound
	}
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil {
		return ErrRestartNotPending
	}
	if lifecycle.removing || lifecycle.unloading {
		return ErrRestartNotPending
	}
	if lifecycle.draining || lifecycle.restarting {
		return nil
	}
	if lifecycle.status != ConfigStatusModified && lifecycle.status != ConfigStatusApplyFailed {
		return ErrRestartNotPending
	}
	lifecycle.draining = true
	lifecycle.force = false
	lifecycle.unloading = false
	lifecycle.status = ConfigStatusDraining
	lifecycle.err = ""
	s.maybeStartRestart(modelID)
	return nil
}

// RequestForceRestart confirms a pending model configuration and immediately
// fences the current serving generation. Requests already waiting in the normal
// queue or in a model-start swap are failed, while requests arriving after this
// point wait for the replacement generation. The old process is still stopped
// through the router's normal lifecycle; only the scheduler's drain barrier is
// bypassed.
func (s *FIFO) RequestForceRestart(modelID string) error {
	if _, ok := s.effects.ModelState(modelID); !ok {
		return ErrModelNotFound
	}
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil {
		return ErrRestartNotPending
	}
	if lifecycle.removing || lifecycle.unloading {
		return ErrRestartNotPending
	}
	if lifecycle.restarting {
		// Upgrade an already-started normal restart in place. The base router can
		// wake a restart that is waiting on an ignored websocket without launching
		// a second replacement generation.
		if !lifecycle.force {
			lifecycle.force = true
			s.cancelPendingForceRequests(modelID)
			s.advanceServeGeneration(modelID)
		}
		if effects, ok := s.effects.(interface{ ForceCurrentRestart(string) }); ok {
			effects.ForceCurrentRestart(modelID)
		}
		return nil
	}
	if !lifecycle.draining && lifecycle.status != ConfigStatusModified && lifecycle.status != ConfigStatusApplyFailed {
		return ErrRestartNotPending
	}
	if !lifecycle.draining {
		lifecycle.draining = true
		lifecycle.unloading = false
		lifecycle.status = ConfigStatusDraining
	}
	lifecycle.force = true
	lifecycle.err = ""
	s.cancelPendingForceRequests(modelID)
	s.advanceServeGeneration(modelID)
	s.maybeStartRestart(modelID)
	return nil
}

// MarkRemoving fences new requests for a model deleted from the desired
// topology. Requests admitted before this event continue through the old
// queue/generation and the process is stopped only after that work drains.
func (s *FIFO) MarkRemoving(modelID string) {
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil {
		lifecycle = &lifecycleState{}
		s.lifecycle[modelID] = lifecycle
	}
	if lifecycle.removing {
		return
	}
	if lifecycle.unloading {
		// A topology deletion supersedes an operator unload. The process is
		// still removed, rather than being left stopped with a stale modified
		// lifecycle record.
		lifecycle.keepModified = false
		lifecycle.unloading = false
	}
	for _, waiter := range lifecycle.waiters {
		s.effects.GrantError(waiter, swaputil.StatusError{Status: 503, Message: "model is being removed", Code: "model_removing"})
	}
	lifecycle.waiters = nil
	if lifecycle.restarting || lifecycle.draining {
		if canceler, ok := s.effects.(interface{ CancelRestart(string) }); ok {
			canceler.CancelRestart(modelID)
		}
	}
	s.invalidateRestart(lifecycle)
	lifecycle.draining = false
	lifecycle.restarting = false
	lifecycle.removing = true
	lifecycle.unloading = false
	lifecycle.status = ConfigStatusRemoving
	lifecycle.err = ""
	s.maybeStartRemoval(modelID)
}

// ModelStatuses returns a copy suitable for concurrent API/SSE readers.
func (s *FIFO) ModelStatuses() map[string]ModelLifecycleStatus {
	result := make(map[string]ModelLifecycleStatus, len(s.lifecycle))
	for id, lifecycle := range s.lifecycle {
		oldRequests := s.reserved[id]
		if counter, ok := s.effects.(interface{ IgnoredRequests(string) int }); ok {
			oldRequests += counter.IgnoredRequests(id)
		}
		result[id] = ModelLifecycleStatus{
			ConfigStatus:    lifecycle.status,
			AppliedRevision: lifecycle.oldAppliedRev,
			DesiredRevision: lifecycle.desiredRevision,
			OldRequests:     oldRequests,
			WaitingRequests: len(lifecycle.waiters),
			Error:           lifecycle.err,
			Maintenance:     lifecycle.maintenance,
		}
	}
	return result
}

// SetConfigRevisions associates the current reconciler revisions with model
// lifecycle records. A model that is waiting for a restart still serves the
// previous applied generation, so its applied revision is the revision before
// the snapshot that caused the pending change. Keep that value stable while a
// restart is in progress; later desired revisions must still be visible.
func (s *FIFO) SetConfigRevisions(applied, desired uint64) {
	for _, lifecycle := range s.lifecycle {
		if !lifecycle.revisionsInitialized {
			if applied > 0 {
				lifecycle.oldAppliedRev = applied - 1
			}
			lifecycle.revisionsInitialized = true
		}
		lifecycle.desiredRevision = desired
	}
}

func (s *FIFO) invalidateRestart(lifecycle *lifecycleState) {
	if lifecycle.restartGeneration == 0 {
		lifecycle.restartGeneration = invalidRestartGeneration
		return
	}
	if lifecycle.staleRestarts == nil {
		lifecycle.staleRestarts = make(map[uint64]struct{})
	}
	lifecycle.staleRestarts[lifecycle.restartGeneration] = struct{}{}
	lifecycle.restartGeneration = invalidRestartGeneration
}

func (s *FIFO) isStaleRestart(lifecycle *lifecycleState, generation uint64) bool {
	if generation == 0 {
		// Direct scheduler embedders predating generation-aware effects emit
		// zero. Keep those events compatible; the concrete base router always
		// supplies a non-zero generation.
		return false
	}
	if lifecycle.restartGeneration == invalidRestartGeneration {
		return true
	}
	if lifecycle.restartGeneration != 0 && lifecycle.restartGeneration != generation {
		return true
	}
	_, stale := lifecycle.staleRestarts[generation]
	return stale
}

func (s *FIFO) enqueueRestartWaiter(req HandlerReq, lifecycle *lifecycleState) {
	if len(lifecycle.waiters) >= s.limit(req.Model) {
		s.rejectAdmission(req, swaputil.ConcurrencyLimitError{})
		return
	}
	if !sendAdmission(req, nil) {
		return
	}
	lifecycle.waiters = append(lifecycle.waiters, req)
	broadcastQueuePositions(lifecycle.waiters)
}

func (s *FIFO) maybeStartAllMaintenance() {
	for modelID := range s.lifecycle {
		s.maybeStartRestart(modelID)
		s.maybeStartRemoval(modelID)
		s.maybeStartUnload(modelID)
	}
}

func (s *FIFO) maybeStartRestart(modelID string) {
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil || !lifecycle.draining || lifecycle.restarting || lifecycle.removing {
		return
	}
	if lifecycle.force {
		if s.hasRestartSwapWork(modelID) {
			return
		}
	} else if s.hasOldWork(modelID) {
		return
	}
	lifecycle.restarting = true
	lifecycle.status = ConfigStatusRestarting
	lifecycle.restartGeneration = 0
	if lifecycle.force {
		if effects, ok := s.effects.(interface{ ForceRestartProcessWithGeneration(string) uint64 }); ok {
			lifecycle.restartGeneration = effects.ForceRestartProcessWithGeneration(modelID)
			return
		}
	}
	if effects, ok := s.effects.(interface{ RestartProcessWithGeneration(string) uint64 }); ok {
		lifecycle.restartGeneration = effects.RestartProcessWithGeneration(modelID)
		return
	}
	if effects, ok := s.effects.(interface{ RestartProcess(string) }); ok {
		effects.RestartProcess(modelID)
		return
	}
	s.OnRestartDone(RestartDone{ModelID: modelID, Err: fmt.Errorf("router cannot restart model %s", modelID)})
}

// OnRestartProgress keeps the externally visible lifecycle state aligned with
// the process replacement seam. It is intentionally a small optional event:
// the run loop still owns every mutation and OnRestartDone remains the only
// event that releases the waiting room.
func (s *FIFO) OnRestartProgress(ev RestartProgress) {
	lifecycle := s.lifecycle[ev.ModelID]
	if lifecycle == nil || lifecycle.unloading || lifecycle.removing || s.isStaleRestart(lifecycle, ev.Generation) {
		return
	}
	if ev.Status != "" {
		lifecycle.status = ev.Status
	}
	if ev.Err != nil {
		lifecycle.err = ev.Err.Error()
	}
}

func (s *FIFO) maybeStartRemoval(modelID string) {
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil || !lifecycle.removing || lifecycle.status == ConfigStatusUnloading || s.hasOldWork(modelID) {
		return
	}
	lifecycle.status = ConfigStatusUnloading
	if effects, ok := s.effects.(interface{ RemoveProcess(string) }); ok {
		effects.RemoveProcess(modelID)
		return
	}
	s.OnRemoveDone(RemovalDone{ModelID: modelID, Err: fmt.Errorf("router cannot remove model %s", modelID)})
}

// maybeStartUnload completes the unload half of a restart superseded by an
// explicit operator unload. Unlike a normal unload, this path waits for the
// old generation's reservations and any active swap to drain before calling
// StopProcesses, so an in-flight request is never torn down underneath the
// generation fence.
func (s *FIFO) maybeStartUnload(modelID string) {
	lifecycle := s.lifecycle[modelID]
	if lifecycle == nil || !lifecycle.unloading || lifecycle.unloadStarted || s.hasOldWork(modelID) {
		return
	}
	lifecycle.unloadStarted = true
	timeout := lifecycle.unloadTimeout
	s.effects.StopProcesses(timeout, []string{modelID})
	if lifecycle.keepModified {
		// A stopped generation has nothing left to disturb, so let the router
		// adopt the configuration the operator already saved: the next start then
		// uses it, and there is nothing left for a pending-restart flag to
		// describe. A model whose process survived the stop, or whose start is
		// already in flight, keeps the flag.
		if adopter, ok := s.effects.(interface{ AdoptDesiredProcessConfig(string) bool }); ok && adopter.AdoptDesiredProcessConfig(modelID) {
			delete(s.lifecycle, modelID)
			return
		}
		lifecycle.unloading = false
		lifecycle.unloadStarted = false
		lifecycle.status = ConfigStatusModified
		lifecycle.err = ""
		return
	}
	delete(s.lifecycle, modelID)
}

func (s *FIFO) hasOldWork(modelID string) bool {
	if s.reserved[modelID] > 0 {
		return true
	}
	if lifecycle := s.lifecycle[modelID]; lifecycle != nil && lifecycleNeedsDrain(lifecycle) {
		if counter, ok := s.effects.(interface{ IgnoredRequests(string) int }); ok && counter.IgnoredRequests(modelID) > 0 {
			return true
		}
	}
	for target, swap := range s.active {
		if target == modelID || containsString(swap.evict, modelID) {
			return true
		}
	}
	return false
}

func (s *FIFO) hasRestartSwapWork(modelID string) bool {
	for target, swap := range s.active {
		if target == modelID || containsString(swap.evict, modelID) {
			return true
		}
	}
	return false
}

func (s *FIFO) cancelPendingForceRequests(modelID string) {
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, req := range s.queued {
			if req.Model == modelID {
				s.grantError(req, swaputil.StatusError{
					Status:  503,
					Message: "request discarded by force restart",
					Code:    "model_force_restart",
				})
				continue
			}
			kept = append(kept, req)
		}
		s.queued = kept
	}
	for _, swap := range s.active {
		kept := swap.waiters[:0]
		for _, req := range swap.waiters {
			if req.Model == modelID {
				s.grantError(req, swaputil.StatusError{
					Status:  503,
					Message: "request discarded by force restart",
					Code:    "model_force_restart",
				})
				continue
			}
			kept = append(kept, req)
		}
		swap.waiters = kept
	}
	broadcastQueuePositions(s.queued)
}

func (s *FIFO) advanceServeGeneration(modelID string) {
	oldGeneration := s.serveGeneration[modelID]
	old := forcedServeGeneration{
		reserved: s.reserved[modelID],
		inFlight: s.inFlight[modelID],
	}
	if old.reserved > 0 || old.inFlight > 0 {
		byGeneration := s.forcedOld[modelID]
		if byGeneration == nil {
			byGeneration = make(map[uint64]forcedServeGeneration)
			s.forcedOld[modelID] = byGeneration
		}
		byGeneration[oldGeneration] = old
	}
	delete(s.reserved, modelID)
	delete(s.inFlight, modelID)
	s.serveGeneration[modelID] = oldGeneration + 1
}

func (s *FIFO) conflictsWithIgnoredLifecycle(models []string) bool {
	counter, ok := s.effects.(interface{ IgnoredRequests(string) int })
	if !ok {
		return false
	}
	for _, modelID := range models {
		lifecycle := s.lifecycle[modelID]
		if lifecycle != nil && lifecycleNeedsDrain(lifecycle) && counter.IgnoredRequests(modelID) > 0 {
			return true
		}
	}
	return false
}

func lifecycleNeedsDrain(lifecycle *lifecycleState) bool {
	return lifecycle.draining || lifecycle.restarting || lifecycle.removing || lifecycle.unloading
}

// OnCancel removes a request whose client has disconnected from the queue and
// from every in-flight swap's waiters. If the request was the sole waiter of an
// active swap, the swap goroutine is left to complete on its own — OnSwapDone
// will find no waiters and simply clean up. This prevents drainQueue from ever
// starting a model load for a caller that is no longer there.
func (s *FIFO) OnCancel(req HandlerReq) {
	removed := false

	// Prune from the queue.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, q := range s.queued {
			if q.Respond == req.Respond {
				removed = true
				s.release(q.Model)
				continue
			}
			kept = append(kept, q)
		}
		s.queued = kept
	}

	// Prune from any active swap's waiters.
	for _, sw := range s.active {
		filtered := sw.waiters[:0]
		for _, w := range sw.waiters {
			if w.Respond == req.Respond {
				removed = true
				s.release(w.Model)
				continue
			}
			filtered = append(filtered, w)
		}
		sw.waiters = filtered
	}

	// Restart waiters are admitted into a separate capacity pool and therefore
	// must not release a normal scheduler reservation when they disconnect.
	for _, lifecycle := range s.lifecycle {
		kept := lifecycle.waiters[:0]
		for _, waiter := range lifecycle.waiters {
			if waiter.Respond == req.Respond {
				removed = true
				continue
			}
			kept = append(kept, waiter)
		}
		lifecycle.waiters = kept
	}

	if removed {
		s.logger.Debugf("%s: cancelled request for model %s pruned from scheduler", s.name, req.Model)
		broadcastQueuePositions(s.queued)
		for _, lifecycle := range s.lifecycle {
			broadcastQueuePositions(lifecycle.waiters)
		}
		s.maybeStartAllMaintenance()
	}
}

// OnSwapDone fans the result out to every waiter that joined this swap, removes
// the swap from the active map, then walks the queue once, promoting any items
// that no longer collide with the remaining active set. FIFO order is preserved:
// items still blocked stay in place.
func (s *FIFO) OnSwapDone(ev SwapDone) {
	sw, ok := s.active[ev.ModelID]
	if !ok {
		return
	}
	delete(s.active, ev.ModelID)
	if sw.pendingUnload {
		// The operator unloaded this target while its swap was in flight. The
		// swap goroutine has now finished, so the deferred stop can no longer
		// race its EnsureReady and resurrect the model.
		s.logger.Debugf("%s: stopping %s after swap raced operator unload", s.name, ev.ModelID)
		s.effects.StopProcesses(sw.unloadTimeout, []string{ev.ModelID})
		s.maybeStartAllMaintenance()
		s.drainQueue()
		return
	}
	if lifecycle := s.lifecycle[ev.ModelID]; lifecycle != nil && lifecycle.unloading {
		for _, waiter := range sw.waiters {
			if ev.Err != nil {
				s.grantError(waiter, ev.Err)
			} else {
				// The swap began before the explicit unload and is therefore
				// part of the old generation. Let its callers complete before
				// the lifecycle stop is allowed to run.
				s.grantHandler(waiter, ev.ModelID)
			}
		}
		s.maybeStartAllMaintenance()
		s.drainQueue()
		return
	}
	if lifecycle := s.lifecycle[ev.ModelID]; lifecycle != nil && lifecycle.force {
		// Every waiter attached before the force confirmation belongs to the
		// discarded generation. Normally RequestForceRestart removes them before
		// this event arrives; keep this guard for an event racing that transition.
		for _, waiter := range sw.waiters {
			s.grantError(waiter, swaputil.StatusError{
				Status:  503,
				Message: "request discarded by force restart",
				Code:    "model_force_restart",
			})
		}
		s.drainQueue()
		s.maybeStartAllMaintenance()
		return
	}

	for _, w := range sw.waiters {
		if ev.Err != nil {
			s.grantError(w, ev.Err)
		} else {
			s.grantHandler(w, ev.ModelID)
		}
	}

	s.drainQueue()
	s.maybeStartAllMaintenance()
}

// OnServeDone decrements the per-model in-flight count and, when that drops to
// zero, retries the queue: requests whose swap was deferred because they would
// have evicted this (now-idle) process can now proceed.
func (s *FIFO) OnServeDone(ev ServeDoneEvent) {
	if ev.Ignored {
		// Compatibility websocket requests do not have a reservation or an
		// in-flight slot to release. Their completion can nevertheless unblock a
		// confirmed restart/unload and queued conflicting swaps.
		s.drainQueue()
		s.maybeStartAllMaintenance()
		return
	}
	currentGeneration := s.serveGeneration[ev.ModelID]
	if ev.Generation != currentGeneration {
		byGeneration := s.forcedOld[ev.ModelID]
		old, ok := byGeneration[ev.Generation]
		if !ok || old.inFlight <= 0 {
			// A duplicate or an event from an already-retired generation must not
			// decrement the replacement generation's counters.
			s.maybeStartAllMaintenance()
			return
		}
		old.inFlight--
		if old.reserved > 0 {
			old.reserved--
		}
		if old.inFlight == 0 && old.reserved == 0 {
			delete(byGeneration, ev.Generation)
			if len(byGeneration) == 0 {
				delete(s.forcedOld, ev.ModelID)
			}
		} else {
			byGeneration[ev.Generation] = old
		}
		s.maybeStartAllMaintenance()
		return
	}
	if s.inFlight[ev.ModelID] <= 0 || s.reserved[ev.ModelID] <= 0 {
		// Keep a duplicate completion from panicking the scheduler or corrupting
		// the counters used by later evictions.
		s.maybeStartAllMaintenance()
		return
	}
	s.inFlight[ev.ModelID]--
	s.release(ev.ModelID)
	if s.inFlight[ev.ModelID] <= 0 {
		delete(s.inFlight, ev.ModelID)
		s.drainQueue()
	}
	s.maybeStartAllMaintenance()
}

// OnRestartDone publishes the result of a process replacement and releases
// the waiting-room requests in arrival order. A rollback is serviceable: its
// waiters are released to the restored old process while the model remains in
// apply_failed until the operator confirms another attempt.
func (s *FIFO) OnRestartDone(ev RestartDone) {
	lifecycle := s.lifecycle[ev.ModelID]
	if lifecycle == nil {
		return
	}
	if lifecycle.unloading || lifecycle.removing || s.isStaleRestart(lifecycle, ev.Generation) {
		// A canceled generation may finish after an unload, topology deletion,
		// or a newer restart has already been accepted by the run loop. It must
		// never clear that newer lifecycle record or resurrect waiting traffic.
		s.maybeStartAllMaintenance()
		return
	}
	// A restart completion is a one-shot event. Mark this generation stale
	// before releasing waiters or starting the next maintenance pass so a
	// duplicate/late completion cannot clear a newer pending lifecycle record.
	if ev.Generation != 0 {
		if lifecycle.staleRestarts == nil {
			lifecycle.staleRestarts = make(map[uint64]struct{})
		}
		lifecycle.staleRestarts[ev.Generation] = struct{}{}
		lifecycle.restartGeneration = invalidRestartGeneration
	}
	lifecycle.restarting = false
	lifecycle.draining = false
	// The force marker belongs to the generation that just finished. Clearing it
	// only on the success path left the record in apply_failed with force still
	// set, and the next cold-start swap then discarded its waiters with
	// "request discarded by force restart" long after the restart had ended —
	// even though the process it had just brought up was ready.
	lifecycle.force = false
	if ev.Err == nil {
		if lifecycle.pendingAgain {
			lifecycle.pendingAgain = false
			lifecycle.status = ConfigStatusModified
		} else {
			delete(s.lifecycle, ev.ModelID)
		}
	} else if ev.Maintenance {
		// The operator disabled auto-rollback, so the desired configuration was
		// kept and the model was taken out of service. This is not a stuck
		// failure: it is the modified state waiting for a start that works, so
		// keep the record in modified rather than apply_failed. The reason is
		// preserved so the UI can explain why the model is not serving.
		lifecycle.status = ConfigStatusModified
		lifecycle.err = ev.Err.Error()
		lifecycle.maintenance = true
	} else {
		lifecycle.status = ConfigStatusApplyFailed
		lifecycle.err = ev.Err.Error()
	}

	waiters := lifecycle.waiters
	lifecycle.waiters = nil
	for _, waiter := range waiters {
		if ev.Err != nil && !ev.RolledBack {
			s.effects.GrantError(waiter, swaputil.StatusError{Status: 503, Message: "model restart failed", Code: "model_restart_failed"})
			continue
		}
		s.grantWaitingHandler(waiter, ev.ModelID)
	}
	if len(waiters) > 0 {
		broadcastQueuePositions(lifecycle.waiters)
	}
	s.drainQueue()
	s.maybeStartAllMaintenance()
}

func (s *FIFO) grantServe(req HandlerReq, modelID string) bool {
	if effects, ok := s.effects.(interface {
		GrantServeWithGeneration(HandlerReq, string, uint64) bool
	}); ok {
		return effects.GrantServeWithGeneration(req, modelID, s.serveGeneration[modelID])
	}
	return s.effects.GrantServe(req, modelID)
}

// grantWaitingHandler moves a request from the restart waiting room into the
// normal in-flight accounting. Waiting-room admission intentionally does not
// reserve a scheduler slot while the old process is draining; reserve it only
// once the new (or rolled-back) process can actually receive the request.
func (s *FIFO) grantWaitingHandler(req HandlerReq, modelID string) {
	if err := swaputil.SetReqData(req.Ctx, "fifo_priority", strconv.Itoa(s.cfg.Priority[req.Model])); err != nil {
		s.logger.Debugf("failed to set fifo_priority metadata: %v", err)
	}
	if s.grantServe(req, modelID) {
		s.reserved[modelID]++
		s.inFlight[modelID]++
	}
}

// OnRemoveDone clears a completed removal. A failed stop remains visible as
// unloading so a later configuration reconciliation can retry it without
// exposing a half-removed model as ready.
func (s *FIFO) OnRemoveDone(ev RemovalDone) bool {
	lifecycle := s.lifecycle[ev.ModelID]
	if lifecycle == nil {
		return false
	}
	if !lifecycle.removing {
		return false
	}
	if ev.Err != nil {
		lifecycle.status = ConfigStatusUnloading
		lifecycle.err = ev.Err.Error()
		return false
	}
	delete(s.lifecycle, ev.ModelID)
	s.drainQueue()
	return true
}

// OnUnload reconciles router-owned state with the impending Stop, then drains
// the queue. Lifecycle targets and targets with an in-flight swap are stopped
// only after their old work drains (via maybeStartUnload and the
// pendingUnload barrier in OnSwapDone respectively); every other target is
// stopped synchronously via Effects, so callers of Unload still block until
// those processes have exited.
func (s *FIFO) OnUnload(targets []string, timeout time.Duration) {
	unloadErr := fmt.Errorf("%s: model unloaded", s.name)

	targetSet := make(map[string]bool, len(targets))
	for _, id := range targets {
		targetSet[id] = true
	}

	// An explicit unload supersedes a confirmed restart. Fence its waiting
	// room with 503 and cancel the asynchronous replacement before stopping the
	// old process; otherwise a late restart completion could resurrect a model
	// that the operator just asked us to unload.
	lifecycleTargets := make(map[string]struct{})
	for id := range targetSet {
		lifecycle := s.lifecycle[id]
		if lifecycle == nil {
			continue
		}
		wasRemoving := lifecycle.removing
		lifecycleTargets[id] = struct{}{}
		for _, waiter := range lifecycle.waiters {
			s.effects.GrantError(waiter, unloadErr)
		}
		lifecycle.waiters = nil
		if lifecycle.restarting || lifecycle.draining {
			s.invalidateRestart(lifecycle)
			if canceler, ok := s.effects.(interface{ CancelRestart(string) }); ok {
				canceler.CancelRestart(id)
			}
		}
		lifecycle.draining = false
		lifecycle.restarting = false
		lifecycle.removing = false
		lifecycle.unloading = true
		lifecycle.unloadTimeout = timeout
		lifecycle.keepModified = !wasRemoving
		lifecycle.status = ConfigStatusUnloading
	}

	// Release waiters of any in-flight swap whose target is being unloaded.
	// For a restart lifecycle keep the active entry as a drain barrier: its
	// swap goroutine may still be starting the old generation, and
	// StopProcesses must not race that goroutine. A normal unload with an
	// in-flight swap keeps the same barrier as a safety net, but the target is
	// also stopped immediately below: the process run loop aborts a start
	// cleanly when Stop arrives mid-start, which is what makes "stop a model
	// that is still starting" responsive. The deferred stop from OnSwapDone
	// remains for the narrow ordering where the stop lands before the run loop
	// has consumed the start request (there it is a no-op and the start would
	// otherwise complete and linger until the barrier stop runs).
	for id := range targetSet {
		sw, ok := s.active[id]
		if !ok {
			continue
		}
		if _, lifecycle := lifecycleTargets[id]; lifecycle {
			// This swap was admitted before the lifecycle unload event. Keep
			// its old-generation callers attached so OnSwapDone can grant them
			// before maybeStartUnload waits for their handlers to finish.
			continue
		}
		for _, w := range sw.waiters {
			s.grantError(w, unloadErr)
		}
		sw.waiters = nil
		sw.pendingUnload = true
		sw.unloadTimeout = timeout
	}

	// Drop queued requests addressed to unloaded models. Requests for other
	// models stay queued and may benefit from drainQueue at the end.
	if len(s.queued) > 0 {
		kept := s.queued[:0]
		for _, w := range s.queued {
			if targetSet[w.Model] {
				if _, lifecycle := lifecycleTargets[w.Model]; lifecycle {
					// Queued before unload: it remains an old-generation
					// request and will be reconsidered by drainQueue.
					kept = append(kept, w)
					continue
				}
				s.grantError(w, unloadErr)
				continue
			}
			kept = append(kept, w)
		}
		s.queued = kept
	}

	// A lifecycle target is stopped only after its old reservations and active
	// swaps drain. Every other target — including one deferred behind a
	// pendingUnload swap barrier — is stopped synchronously: for a model still
	// starting this is what aborts the start instead of letting it run to
	// completion before the barrier stop.
	normalTargets := make([]string, 0, len(targets))
	for _, id := range targets {
		if _, lifecycle := lifecycleTargets[id]; lifecycle {
			continue
		}
		normalTargets = append(normalTargets, id)
	}
	if len(normalTargets) > 0 {
		// inFlight is intentionally NOT cleared here: each dying handler will
		// fire its tracked serve and reach OnServeDone in the normal way.
		s.effects.StopProcesses(timeout, normalTargets)
	}
	s.maybeStartAllMaintenance()

	// Removing entries from normal active swaps may have unblocked queued
	// requests that previously collided with them.
	s.drainQueue()
}

// OnShutdown grants err to every waiter still held by the scheduler.
func (s *FIFO) OnShutdown(err error) {
	for _, sw := range s.active {
		for _, w := range sw.waiters {
			s.grantError(w, err)
		}
	}
	for _, w := range s.queued {
		s.grantError(w, err)
	}
	for _, lifecycle := range s.lifecycle {
		for _, w := range lifecycle.waiters {
			s.effects.GrantError(w, err)
		}
		lifecycle.waiters = nil
	}
}

// grantHandler hands the caller a tracked handler for modelID and, only if the
// caller was still there to receive it, bumps the in-flight count. Incrementing
// when the grant failed would strand the counter and block future evictions.
// Concurrency-limit rejection happens earlier in admit, before a request can
// start the loading stream.
func (s *FIFO) grantHandler(req HandlerReq, modelID string) {
	if err := swaputil.SetReqData(req.Ctx, "fifo_priority", strconv.Itoa(s.cfg.Priority[req.Model])); err != nil {
		s.logger.Debugf("failed to set fifo_priority metadata: %v", err)
	}

	if s.grantServe(req, modelID) {
		s.inFlight[modelID]++
	} else {
		s.release(modelID)
	}
}

// grantError reports a post-admission error to the caller and releases the
// request's reserved concurrency slot.
func (s *FIFO) grantError(req HandlerReq, err error) {
	s.release(req.Model)
	s.effects.GrantError(req, err)
}

// admit performs the pre-stream admission handshake. Accepted requests reserve
// one future serving slot until they serve, cancel while waiting, or receive a
// post-admission error.
func (s *FIFO) admit(req HandlerReq) bool {
	if s.reserved[req.Model] >= s.limit(req.Model) {
		s.rejectAdmission(req, swaputil.ConcurrencyLimitError{})
		return false
	}
	if !sendAdmission(req, nil) {
		return false
	}
	s.reserved[req.Model]++
	return true
}

func (s *FIFO) rejectAdmission(req HandlerReq, err error) {
	sendAdmission(req, err)
}

func sendAdmission(req HandlerReq, err error) bool {
	if req.Admit == nil {
		return true
	}
	done := reqDone(req)
	select {
	case <-done:
		return false
	default:
	}
	select {
	case req.Admit <- err:
		return true
	case <-done:
		return false
	}
}

func reqDone(req HandlerReq) <-chan struct{} {
	if req.Ctx == nil {
		return nil
	}
	return req.Ctx.Done()
}

func (s *FIFO) release(modelID string) {
	if s.reserved[modelID] <= 0 {
		panic(fmt.Sprintf("%s: release without reservation for model %s", s.name, modelID))
	}
	s.reserved[modelID]--
	if s.reserved[modelID] == 0 {
		delete(s.reserved, modelID)
	}
}

// limit returns the per-model concurrency cap, defaulting to
// defaultConcurrencyLimit when the model has no explicit entry.
func (s *FIFO) limit(modelID string) int {
	if l, ok := s.limits[modelID]; ok {
		return l
	}
	return defaultConcurrencyLimit
}

// startSwap records the swap as active and launches it via Effects. running is
// the set EvictionFor saw, forwarded to OnSwapStart so the planner logs against
// the same picture it decided on.
func (s *FIFO) startSwap(initial HandlerReq, evict, running []string) {
	s.active[initial.Model] = &activeSwap{
		modelID: initial.Model,
		evict:   evict,
		waiters: []HandlerReq{initial},
	}
	s.planner.OnSwapStart(initial.Model, running)
	s.effects.StartSwap(initial.Model, evict)
}

// enqueue inserts req into the queue in priority order: it goes just before the
// first queued item whose priority is strictly lower, so higher-priority models
// are serviced first while equal-priority requests keep their arrival (FIFO)
// order. Priorities come from the FifoConfig; unlisted models default to 0.
func (s *FIFO) enqueue(req HandlerReq) {
	p := s.cfg.Priority[req.Model]
	i := len(s.queued)
	for j, q := range s.queued {
		if s.cfg.Priority[q.Model] < p {
			i = j
			break
		}
	}
	s.queued = append(s.queued, HandlerReq{})
	copy(s.queued[i+1:], s.queued[i:])
	s.queued[i] = req
	broadcastQueuePositions(s.queued)
}

// drainQueue walks the queued requests in order, re-running the OnRequest
// decision tree against the (now smaller) active set. Items that can now start
// or join become satisfied; items still blocked remain queued in original order
// so they get another chance on the next swap completion.
func (s *FIFO) drainQueue() {
	if len(s.queued) == 0 {
		return
	}
	pending := s.queued
	var remaining []HandlerReq
	for _, req := range pending {
		state, ok := s.effects.ModelState(req.Model)
		if !ok {
			s.grantError(req, ErrModelNotFound)
			continue
		}
		if sw, ok := s.active[req.Model]; ok {
			s.logger.Debugf("%s: queued request for model %s now joining in-flight swap", s.name, req.Model)
			sw.waiters = append(sw.waiters, req)
			continue
		}
		running := s.runningSet(req.Model)
		evict := s.planner.EvictionFor(req.Model, running)
		if state == process.StateReady && len(evict) == 0 && !collidesWith(req.Model, evict, s.active) {
			s.logger.Debugf("%s: queued request for model %s now served fast-path", s.name, req.Model)
			s.grantHandler(req, req.Model)
			continue
		}
		if collidesWith(req.Model, evict, s.active) {
			remaining = append(remaining, req)
			continue
		}
		if conflictsWithInFlight(evict, s.inFlight) {
			remaining = append(remaining, req)
			continue
		}
		if s.conflictsWithCommittedRestart(evict) {
			remaining = append(remaining, req)
			continue
		}
		if s.conflictsWithIgnoredLifecycle(evict) {
			remaining = append(remaining, req)
			continue
		}
		s.logger.Debugf("%s: queued request for model %s now starting swap, evicting %v", s.name, req.Model, evict)
		s.startSwap(req, evict, running)
	}
	s.queued = remaining
	broadcastQueuePositions(s.queued)
}

// runningSet is the live model set handed to the Swapper: every process the
// baseRouter reports as running, unioned with the targets of in-flight swaps
// (excluding excludeActive, the model whose own swap is being decided — its
// in-flight entry must not count as "already running") and with models whose
// restart is committed (draining/restarting): the replacement process will
// reoccupy the model's slot even though the stopped old process is invisible
// to RunningModels. The result is sorted so eviction decisions derived from
// it are deterministic.
func (s *FIFO) runningSet(excludeActive string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for id := range s.effects.RunningModels() {
		add(id)
	}
	for _, id := range activeTargets(s.active, excludeActive) {
		add(id)
	}
	for id, lifecycle := range s.lifecycle {
		if lifecycle.draining || lifecycle.restarting {
			add(id)
		}
	}
	sort.Strings(out)
	return out
}

// conflictsWithCommittedRestart reports whether any model in models has a
// restart in progress (draining/restarting). Evicting such a model cannot
// work — its process is already stopped and a replacement is on the way —
// so the conflicting swap must wait for the restart to finish.
func (s *FIFO) conflictsWithCommittedRestart(models []string) bool {
	for _, modelID := range models {
		lifecycle := s.lifecycle[modelID]
		if lifecycle != nil && (lifecycle.draining || lifecycle.restarting) {
			return true
		}
	}
	return false
}

// unloadErr is the error granted to callers of a model the operator unloaded.
func (s *FIFO) unloadErr() error {
	return fmt.Errorf("%s: model unloaded", s.name)
}

// activeTargets returns the IDs of every in-flight swap target except exclude.
// The planner uses this to account for models committed to but not yet reflected
// in process state.
func activeTargets(active map[string]*activeSwap, exclude string) []string {
	if len(active) == 0 {
		return nil
	}
	out := make([]string, 0, len(active))
	for id := range active {
		if id == exclude {
			continue
		}
		out = append(out, id)
	}
	return out
}

// collidesWith reports whether a new swap with this target and evict set can
// safely run alongside the currently active swaps. Same-target callers should
// JOIN (handled before this) — they do not collide with themselves.
func collidesWith(target string, evict []string, active map[string]*activeSwap) bool {
	for id, sw := range active {
		if id == target {
			continue
		}
		if containsString(evict, id) {
			return true
		}
		if containsString(sw.evict, target) {
			return true
		}
		if slicesOverlap(evict, sw.evict) {
			return true
		}
	}
	return false
}

// slicesOverlap reports whether xs and ys share any common element.
func slicesOverlap(xs, ys []string) bool {
	for _, x := range xs {
		if containsString(ys, x) {
			return true
		}
	}
	return false
}

// conflictsWithInFlight reports whether any model in evict is still handling
// requests. Stopping a busy process would cancel its callers' connections, so
// the scheduler defers the swap until those callers finish.
func conflictsWithInFlight(evict []string, inFlight map[string]int) bool {
	for _, m := range evict {
		if inFlight[m] > 0 {
			return true
		}
	}
	return false
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// broadcastQueuePositions sends each queued request its current 1-indexed
// position. Sends are non-blocking: if the channel is full, the old value is
// drained first so the consumer always sees the latest position.
func broadcastQueuePositions(queued []HandlerReq) {
	for i, req := range queued {
		pos := i + 1
		select {
		case req.PositionCh <- pos:
		default:
			select {
			case <-req.PositionCh:
			default:
			}
			select {
			case req.PositionCh <- pos:
			default:
			}
		}
	}
}
