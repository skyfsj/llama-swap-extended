package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type shutdownReq struct {
	timeout time.Duration
	respond chan error
}

type unloadReq struct {
	targets []string
	timeout time.Duration
	respond chan struct{}
}

type reconfigureReq struct {
	conf     config.Config
	planner  scheduler.Swapper
	modelIDs map[string]struct{}
	respond  chan error
}

type restartReq struct {
	modelID string
	force   bool
	respond chan error
}

type lifecycleStatusReq struct {
	respond chan map[string]scheduler.ModelLifecycleStatus
}

type configRevisionReq struct {
	applied uint64
	desired uint64
}

type processFactory func(context.Context, string, config.ModelConfig) (process.Process, error)

// processRuntimeSnapshot records the runtime definition used to construct a
// process generation. The model config alone is not enough: a model can keep
// the same Backend.Runtime reference while the referenced runtime definition
// changes in a later reconciliation.
type processRuntimeSnapshot struct {
	name  string
	value config.RuntimeConfig
	known bool
}

type restartControl struct {
	cancel     context.CancelFunc
	ctx        context.Context
	generation uint64
	done       chan struct{}
	suppressCh chan struct{}
	forceCh    chan struct{}
	suppressed atomic.Bool
	forced     atomic.Bool
}

// restartProcessState is the process generation that is being prepared for a
// restart. It is kept separate from processes until readiness succeeds, but it
// must still be visible to status and log readers while the new generation is
// starting.
type restartProcessState struct {
	generation uint64
	process    process.Process
}

// removalStopWait lets a re-added model defer its next EnsureReady until the
// process from the preceding removal has fully stopped. Without this gate a
// replacement can race the old command for the same configured listener port.
type removalStopWait struct {
	generation uint64
	done       chan struct{}
}

// baseRouter owns the channels, run-loop, and process machinery shared by every
// concrete router. Concrete routers embed *baseRouter and supply a
// scheduler.Swapper describing how eviction sets are decided. baseRouter
// implements scheduler.Effects so the scheduler can call back for side-effects.
type baseRouter struct {
	name           string
	config         config.Config
	processes      map[string]process.Process
	processMu      sync.RWMutex
	processConfigs map[string]config.ModelConfig
	processRuntime map[string]processRuntimeSnapshot
	desiredConfigs map[string]config.ModelConfig

	// maintenance maps a model ID to the reason it is in maintenance mode.
	// Maintenance is entered when a configuration change fails to start the
	// model and the operator disabled auto-rollback, so the model keeps the
	// new configuration instead of being restored to the old one. A model in
	// maintenance refuses to be started by incoming requests; the operator
	// must start it explicitly. It leaves maintenance on its own once that
	// start succeeds and a request is actually served.
	maintenanceMu sync.RWMutex
	maintenance   map[string]string

	makeProcess processFactory
	// processDecorator, when set, is applied to every managed
	// *ProcessCommand: the processes that exist when it is installed and each
	// one created afterwards. Used by the server to attach per-model
	// dependency hooks without the router knowing what they do.
	processDecorator func(modelID string, p *process.ProcessCommand)
	logger           *logmon.Monitor
	schedule         scheduler.Scheduler

	// shutdownCtx governs the request machinery: cancelling it tells grant()
	// and ServeHTTP to stop granting and reject callers. It is deliberately
	// separate from procCtx — see procCtx below.
	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool

	// procCtx is the parent context for every managed process and governs
	// process lifetime only. handleShutdown stops processes gracefully via
	// Stop() and cancels procCtx afterwards, so teardown is never a context
	// cancel racing the graceful path (which collapsed the grace to 100ms and
	// let the caller return before children were reaped — see process run loop).
	procCtx    context.Context
	procCancel context.CancelFunc

	handlerCh         chan scheduler.HandlerReq
	cancelCh          chan scheduler.HandlerReq
	shutdownCh        chan shutdownReq
	unloadCh          chan unloadReq
	reconfigureCh     chan reconfigureReq
	restartCh         chan restartReq
	statusCh          chan lifecycleStatusReq
	configRevisionCh  chan configRevisionReq
	swapDoneCh        chan scheduler.SwapDone
	serveDoneCh       chan scheduler.ServeDoneEvent
	restartProgressCh chan scheduler.RestartProgress
	restartDoneCh     chan scheduler.RestartDone
	removeDoneCh      chan scheduler.RemovalDone

	runDone chan struct{}

	// testProcessed, when non-nil, receives one event after each handlerReq
	// or swapDone has been fully processed by run(). Tests use it to wait
	// for run() to reach a deterministic state without sleeping. serveDone
	// events are intentionally NOT signalled here so test event counts
	// remain stable.
	testProcessed        chan struct{}
	testRemovalProcessed chan struct{}

	statusMu         sync.RWMutex
	statuses         map[string]scheduler.ModelLifecycleStatus
	restartMu        sync.Mutex
	restartCancels   map[string]*restartControl
	restartNextGen   uint64
	restartProcesses map[string]restartProcessState

	// removalGenerations fences asynchronous topology-removal completions. A
	// model can be re-added while the old process is still stopping; a late
	// completion from that old removal must not delete the replacement entry.
	removalNextGen       uint64
	removalGenerations   map[string]uint64
	removalStopWaits     map[string]removalStopWait
	ignoredWebsocketMu   sync.Mutex
	ignoredWebsockets    map[string]int
	ignoredWebsocketDone chan struct{}
}

func newBaseRouter(
	name string,
	conf config.Config,
	processes map[string]process.Process,
	logger *logmon.Monitor,
	planner scheduler.Swapper,
) (*baseRouter, error) {
	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	procCtx, procCancel := context.WithCancel(context.Background())
	b := &baseRouter{
		name:                 name,
		config:               conf,
		processes:            processes,
		processConfigs:       make(map[string]config.ModelConfig, len(processes)),
		processRuntime:       make(map[string]processRuntimeSnapshot, len(processes)),
		desiredConfigs:       make(map[string]config.ModelConfig, len(conf.Models)),
		logger:               logger,
		shutdownCtx:          shutdownCtx,
		shutdownFn:           shutdownFn,
		procCtx:              procCtx,
		procCancel:           procCancel,
		handlerCh:            make(chan scheduler.HandlerReq),
		cancelCh:             make(chan scheduler.HandlerReq),
		shutdownCh:           make(chan shutdownReq),
		unloadCh:             make(chan unloadReq),
		reconfigureCh:        make(chan reconfigureReq),
		restartCh:            make(chan restartReq),
		statusCh:             make(chan lifecycleStatusReq),
		configRevisionCh:     make(chan configRevisionReq),
		swapDoneCh:           make(chan scheduler.SwapDone),
		serveDoneCh:          make(chan scheduler.ServeDoneEvent),
		restartProgressCh:    make(chan scheduler.RestartProgress),
		restartDoneCh:        make(chan scheduler.RestartDone),
		removeDoneCh:         make(chan scheduler.RemovalDone),
		runDone:              make(chan struct{}),
		statuses:             make(map[string]scheduler.ModelLifecycleStatus),
		restartCancels:       make(map[string]*restartControl),
		restartProcesses:     make(map[string]restartProcessState),
		removalGenerations:   make(map[string]uint64),
		removalStopWaits:     make(map[string]removalStopWait),
		maintenance:          make(map[string]string),
		ignoredWebsockets:    make(map[string]int),
		ignoredWebsocketDone: make(chan struct{}),
	}
	for id, modelConfig := range conf.Models {
		b.desiredConfigs[id] = modelConfig
	}
	for id := range processes {
		if modelConfig, ok := conf.Models[id]; ok {
			b.processConfigs[id] = modelConfig
			b.processRuntime[id] = runtimeSnapshot(conf, modelConfig)
		}
	}
	sched, err := scheduler.New(conf, name, logger, planner, b)
	if err != nil {
		return nil, err
	}
	b.schedule = sched
	return b, nil
}

func (b *baseRouter) notifyProcessed() {
	if b.testProcessed != nil {
		b.testProcessed <- struct{}{}
	}
}

func (b *baseRouter) notifyRemovalProcessed() {
	if b.testRemovalProcessed == nil {
		return
	}
	select {
	case b.testRemovalProcessed <- struct{}{}:
	default:
	}
}

func (b *baseRouter) run() {
	defer close(b.runDone)

	for {
		select {
		case req := <-b.shutdownCh:
			b.handleShutdown(req)
			return

		case req := <-b.handlerCh:
			b.schedule.OnRequest(req)
			b.publishLifecycleStatuses()
			b.notifyProcessed()

		case req := <-b.cancelCh:
			b.schedule.OnCancel(req)
			b.publishLifecycleStatuses()
			b.notifyProcessed()

		case req := <-b.unloadCh:
			b.schedule.OnUnload(req.targets, req.timeout)
			b.publishLifecycleStatuses()
			close(req.respond)
			b.notifyProcessed()

		case req := <-b.reconfigureCh:
			err := b.handleReconfigure(req.conf, req.planner, req.modelIDs)
			b.publishLifecycleStatuses()
			req.respond <- err

		case req := <-b.restartCh:
			var err error
			if req.force {
				if lifecycle, ok := b.schedule.(interface{ RequestForceRestart(string) error }); ok {
					err = lifecycle.RequestForceRestart(req.modelID)
				} else {
					err = fmt.Errorf("%s scheduler does not support force restart", b.name)
				}
			} else if lifecycle, ok := b.schedule.(interface{ RequestRestart(string) error }); ok {
				err = lifecycle.RequestRestart(req.modelID)
			} else {
				err = fmt.Errorf("%s scheduler does not support model restart", b.name)
			}
			b.publishLifecycleStatuses()
			req.respond <- err

		case ev := <-b.swapDoneCh:
			b.schedule.OnSwapDone(ev)
			b.publishLifecycleStatuses()
			b.notifyProcessed()

		case ev := <-b.serveDoneCh:
			b.schedule.OnServeDone(ev)
			b.publishLifecycleStatuses()

		case ev := <-b.restartDoneCh:
			if lifecycle, ok := b.schedule.(interface{ OnRestartDone(scheduler.RestartDone) }); ok {
				lifecycle.OnRestartDone(ev)
			}
			b.publishLifecycleStatuses()

		case ev := <-b.restartProgressCh:
			if lifecycle, ok := b.schedule.(interface {
				OnRestartProgress(scheduler.RestartProgress)
			}); ok {
				lifecycle.OnRestartProgress(ev)
			}
			b.publishLifecycleStatuses()

		case ev := <-b.removeDoneCh:
			// A completion from an older removal may arrive after the model has
			// been re-added (or after a newer removal has started). Ignore it before
			// handing it to the scheduler so it cannot clear the newer lifecycle.
			if ev.Generation != 0 && !b.removalIsCurrent(ev.ModelID, ev.Generation) {
				b.publishLifecycleStatuses()
				b.notifyRemovalProcessed()
				continue
			}
			removalCompleted := false
			if lifecycle, ok := b.schedule.(interface {
				OnRemoveDone(scheduler.RemovalDone) bool
			}); ok {
				removalCompleted = lifecycle.OnRemoveDone(ev)
			} else if lifecycle, ok := b.schedule.(interface {
				OnRemoveDone(scheduler.RemovalDone)
			}); ok {
				lifecycle.OnRemoveDone(ev)
				removalCompleted = ev.Err == nil
			}
			if ev.Err == nil && removalCompleted && (ev.Generation == 0 || b.removalIsCurrent(ev.ModelID, ev.Generation)) {
				b.processMu.Lock()
				if removed, existed := b.processes[ev.ModelID]; existed {
					discardProcessLogMonitor(removed)
				}
				delete(b.processes, ev.ModelID)
				delete(b.processConfigs, ev.ModelID)
				delete(b.processRuntime, ev.ModelID)
				delete(b.desiredConfigs, ev.ModelID)
				if ev.Generation != 0 && b.removalGenerations[ev.ModelID] == ev.Generation {
					delete(b.removalGenerations, ev.ModelID)
				}
				b.processMu.Unlock()
			}
			b.publishLifecycleStatuses()
			b.notifyRemovalProcessed()

		case req := <-b.statusCh:
			req.respond <- b.statusSnapshotFromRunLoop()

		case req := <-b.configRevisionCh:
			if revisioner, ok := b.schedule.(interface {
				SetConfigRevisions(uint64, uint64)
			}); ok {
				revisioner.SetConfigRevisions(req.applied, req.desired)
			}
			b.publishLifecycleStatuses()
		}
	}
}

// setProcessFactory supplies the concrete process constructor used when a
// stopped model is refreshed or a running model is restarted. It must be set
// before the concrete router starts its run loop.
func (b *baseRouter) setProcessFactory(factory processFactory) {
	b.processMu.Lock()
	b.makeProcess = factory
	b.processMu.Unlock()
}

// SetProcessDecorator installs a callback applied to every managed
// *ProcessCommand: the processes created so far (in the caller's goroutine)
// and each one created afterwards (in the router's run loop). The callback
// must be cheap and non-blocking. A nil decorator clears it; already-decorated
// processes are not undone.
func (b *baseRouter) SetProcessDecorator(decorator func(modelID string, p *process.ProcessCommand)) {
	b.processMu.Lock()
	b.processDecorator = decorator
	existing := make(map[string]*process.ProcessCommand, len(b.processes))
	for id, p := range b.processes {
		if cmd, ok := p.(*process.ProcessCommand); ok {
			existing[id] = cmd
		}
	}
	b.processMu.Unlock()
	for id, cmd := range existing {
		if decorator != nil {
			decorator(id, cmd)
		}
	}
}

// Reconfigure applies a new topology/config snapshot through the router's
// event loop. Existing process objects, queues and in-flight accounting remain
// in place; only stopped processes are replaced immediately. Running process
// changes are fenced by the scheduler until an explicit RestartModel call.
func (b *baseRouter) Reconfigure(conf config.Config, planner scheduler.Swapper, modelIDs map[string]struct{}) error {
	req := reconfigureReq{conf: conf, planner: planner, modelIDs: modelIDs, respond: make(chan error, 1)}
	select {
	case b.reconfigureCh <- req:
	case <-b.runDone:
		return fmt.Errorf("%s router is stopped", b.name)
	}
	return <-req.respond
}

// RestartModel confirms the latest pending runtime configuration for one
// model. The FIFO scheduler makes this idempotent while a restart is active.
func (b *baseRouter) RestartModel(modelID string) error {
	req := restartReq{modelID: modelID, respond: make(chan error, 1)}
	select {
	case b.restartCh <- req:
	case <-b.runDone:
		return fmt.Errorf("%s router is stopped", b.name)
	}
	return <-req.respond
}

// ForceRestartModel confirms a pending configuration restart while explicitly
// discarding old requests. The scheduler performs the generation fence; the
// process replacement itself still goes through RestartProcess's normal
// Stop/EnsureReady/rollback lifecycle.
func (b *baseRouter) ForceRestartModel(modelID string) error {
	req := restartReq{modelID: modelID, force: true, respond: make(chan error, 1)}
	select {
	case b.restartCh <- req:
	case <-b.runDone:
		return fmt.Errorf("%s router is stopped", b.name)
	}
	return <-req.respond
}

// ModelLifecycleStatuses returns a consistent copy of all non-default model
// lifecycle states. A read goes through the run loop when it is alive, so the
// returned counters cannot observe a half-applied queue transition.
func (b *baseRouter) ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus {
	b.statusMu.RLock()
	defer b.statusMu.RUnlock()
	return cloneLifecycleStatuses(b.statuses)
}

// SetConfigRevisions publishes reconciler revisions through the same run loop
// that owns scheduler lifecycle state. The unbuffered channel makes the
// publisher wait until the scheduler has observed the revision pair.
func (b *baseRouter) SetConfigRevisions(applied, desired uint64) {
	req := configRevisionReq{applied: applied, desired: desired}
	select {
	case b.configRevisionCh <- req:
	case <-b.runDone:
	}
}

func cloneLifecycleStatuses(source map[string]scheduler.ModelLifecycleStatus) map[string]scheduler.ModelLifecycleStatus {
	result := make(map[string]scheduler.ModelLifecycleStatus, len(source))
	for id, status := range source {
		result[id] = status
	}
	return result
}

func (b *baseRouter) statusSnapshotFromRunLoop() map[string]scheduler.ModelLifecycleStatus {
	if provider, ok := b.schedule.(interface {
		ModelStatuses() map[string]scheduler.ModelLifecycleStatus
	}); ok {
		return provider.ModelStatuses()
	}
	return nil
}

func (b *baseRouter) publishLifecycleStatuses() {
	snapshot := b.statusSnapshotFromRunLoop()
	b.statusMu.Lock()
	// Cheap length pre-filter before the deep compare; the full clone is only
	// paid when something actually changed.
	changed := len(b.statuses) != len(snapshot) || !reflect.DeepEqual(b.statuses, snapshot)
	if changed {
		b.statuses = cloneLifecycleStatuses(snapshot)
	}
	b.statusMu.Unlock()
	if changed {
		// Dedicated lifecycle event: status changes are not config reloads.
		// The cache is updated before publishing, so subscribers observe the
		// new counters/state.
		event.Emit(swaputil.ModelStatusesChangedEvent{})
	}
}

func (b *baseRouter) handleReconfigure(conf config.Config, planner scheduler.Swapper, modelIDs map[string]struct{}) error {
	if len(modelIDs) == 0 {
		modelIDs = make(map[string]struct{}, len(conf.Models))
		for id := range conf.Models {
			modelIDs[id] = struct{}{}
		}
	}

	b.processMu.RLock()
	oldConfig := b.config
	oldProcesses := make(map[string]process.Process, len(b.processes))
	oldProcessConfigs := make(map[string]config.ModelConfig, len(b.processConfigs))
	oldProcessRuntime := make(map[string]processRuntimeSnapshot, len(b.processRuntime))
	for id, managed := range b.processes {
		oldProcesses[id] = managed
	}
	for id, modelConfig := range b.processConfigs {
		oldProcessConfigs[id] = modelConfig
	}
	for id, snapshot := range b.processRuntime {
		oldProcessRuntime[id] = snapshot
	}
	b.processMu.RUnlock()

	// Construct every process object that this candidate would add or refresh
	// before publishing any scheduler/config state. A factory error must leave
	// the old generation and its active config untouched; otherwise a failed
	// reconciliation could strand the router with a desired-only process map.
	prepared := make(map[string]process.Process)
	preparedRuntime := make(map[string]processRuntimeSnapshot)
	runtimeChanged := make(map[string]bool)
	lifecycleBusy := make(map[string]bool)
	removalInProgress := make(map[string]bool)
	removalPending := make(map[string]bool)
	// swapInFlight detects a manual start whose swap goroutine is registered
	// but has not yet flipped the process out of StateStopped. Swaps are
	// registered synchronously in OnRequest on this same run loop, so the
	// probe is race-free where a State() read alone is not.
	var swapInFlight func(string) bool
	if provider, ok := b.schedule.(interface{ SwapInFlight(string) bool }); ok {
		swapInFlight = provider.SwapInFlight
	}
	if statuses, ok := b.schedule.(interface {
		ModelStatuses() map[string]scheduler.ModelLifecycleStatus
	}); ok {
		for id, status := range statuses.ModelStatuses() {
			switch status.ConfigStatus {
			case scheduler.ConfigStatusDraining, scheduler.ConfigStatusRestarting, scheduler.ConfigStatusRollingBack:
				lifecycleBusy[id] = true
			case scheduler.ConfigStatusRemoving:
				removalPending[id] = true
			case scheduler.ConfigStatusUnloading:
				removalPending[id] = true
				removalInProgress[id] = true
			}
		}
	}
	if removalProvider, ok := b.schedule.(interface{ ModelRemovalPending(string) bool }); ok {
		for id := range removalPending {
			if !removalProvider.ModelRemovalPending(id) {
				delete(removalPending, id)
				delete(removalInProgress, id)
			}
		}
	}
	readdedDuringRemoval := make(map[string]struct{})
	for id := range modelIDs {
		desiredModel, exists := conf.Models[id]
		if !exists {
			continue
		}
		managed, hadProcess := oldProcesses[id]
		if !hadProcess {
			created, err := b.newManagedProcess(id, desiredModel)
			if err != nil {
				return fmt.Errorf("creating process for %q: %w", id, err)
			}
			prepared[id] = created
			preparedRuntime[id] = runtimeSnapshot(conf, desiredModel)
			continue
		}

		actualModel, actualKnown := oldProcessConfigs[id]
		if !actualKnown {
			actualModel, actualKnown = oldConfig.Models[id]
		}
		actualRuntime, runtimeKnown := oldProcessRuntime[id]
		if !runtimeKnown && actualKnown {
			actualRuntime = runtimeSnapshot(oldConfig, actualModel)
		}
		// Once RemoveProcess has been launched, the old process may still report
		// Ready while its asynchronous Stop is pending. Prepare a stopped
		// replacement now so a re-add never puts that dying object back in the
		// registry, and fence its late completion below.
		if removalInProgress[id] {
			created, err := b.newManagedProcess(id, desiredModel)
			if err != nil {
				return fmt.Errorf("re-adding process for %q: %w", id, err)
			}
			carryProcessLogHistory(managed, created)
			prepared[id] = created
			preparedRuntime[id] = runtimeSnapshot(conf, desiredModel)
			readdedDuringRemoval[id] = struct{}{}
			runtimeChanged[id] = actualKnown && processRuntimeChanged(actualModel, actualRuntime, desiredModel, conf)
			continue
		}
		changed := actualKnown && processRuntimeChanged(actualModel, actualRuntime, desiredModel, conf)
		runtimeChanged[id] = changed
		if !changed {
			continue
		}
		// A manual start's swap goroutine may sit between its registry read and
		// EnsureReady while the process still reads as StateStopped. Replacing
		// the process object in that window would orphan the old-generation
		// process the goroutine is about to start: the registry would show a
		// stopped replacement while the orphan serves traffic outside every
		// lifecycle fence. Leave the old generation in place; MarkConfigModified
		// below schedules the replacement restart once the swap settles.
		if swapInFlight != nil && swapInFlight(id) {
			continue
		}
		state := managed.State()
		if state == process.StateStopped && !lifecycleBusy[id] {
			created, err := b.newManagedProcess(id, desiredModel)
			if err != nil {
				return fmt.Errorf("refreshing stopped process %q: %w", id, err)
			}
			carryProcessLogHistory(managed, created)
			prepared[id] = created
			preparedRuntime[id] = runtimeSnapshot(conf, desiredModel)
			continue
		}
	}

	// Publish the desired model/process snapshots before lifecycle events. The
	// scheduler's Reconfigure method drains its queue and may start a pending
	// restart, so it must see the newest desired process before it performs any
	// maintenance work.
	for id := range readdedDuringRemoval {
		b.invalidateRemoval(id)
	}
	b.processMu.Lock()
	b.config = conf
	b.desiredConfigs = make(map[string]config.ModelConfig, len(conf.Models))
	for id, modelConfig := range conf.Models {
		b.desiredConfigs[id] = modelConfig
	}
	for id, created := range prepared {
		if previous, existed := oldProcesses[id]; existed && previous != created {
			discardProcessLogMonitor(previous)
		}
		b.processes[id] = created
		b.processConfigs[id] = conf.Models[id]
		b.processRuntime[id] = preparedRuntime[id]
	}
	b.processMu.Unlock()

	// Mark runtime changes after all prepared processes have been installed.
	// Running generations are fenced by the scheduler; stopped generations are
	// already represented by the replacement object and can be considered
	// applied immediately.
	for id := range modelIDs {
		_, exists := conf.Models[id]
		if !exists {
			continue
		}
		if _, wasPrepared := prepared[id]; wasPrepared || !runtimeChanged[id] {
			if applied, ok := b.schedule.(interface{ MarkConfigApplied(string) }); ok {
				applied.MarkConfigApplied(id)
			}
			continue
		}
		if modified, ok := b.schedule.(interface{ MarkConfigModified(string) }); ok {
			modified.MarkConfigModified(id)
		}
	}

	// A model that left the selected topology is retained in the process map
	// until its old generation drains. This gives deletion the same safe stop
	// semantics as an explicit unload and lets a quick re-add cancel the fence
	// before RemoveProcess is invoked.
	for id := range oldProcesses {
		if _, stillManaged := modelIDs[id]; stillManaged {
			continue
		}
		if removing, ok := b.schedule.(interface{ MarkRemoving(string) }); ok {
			removing.MarkRemoving(id)
		}
	}

	if schedulerConfig, ok := b.schedule.(interface {
		Reconfigure(config.Config, scheduler.Swapper)
	}); ok {
		schedulerConfig.Reconfigure(conf, planner)
	}
	return nil
}

func runtimeSnapshot(conf config.Config, model config.ModelConfig) processRuntimeSnapshot {
	name := strings.TrimSpace(model.Backend.Runtime)
	if name == "" {
		return processRuntimeSnapshot{}
	}
	value, known := conf.Runtimes[name]
	return processRuntimeSnapshot{name: name, value: value, known: known}
}

func processRuntimeChanged(actual config.ModelConfig, actualRuntime processRuntimeSnapshot, desired config.ModelConfig, desiredConf config.Config) bool {
	if config.ModelRuntimeConfigChanged(actual, desired) {
		return true
	}
	actualName := strings.TrimSpace(actual.Backend.Runtime)
	desiredName := strings.TrimSpace(desired.Backend.Runtime)
	if actualName != desiredName {
		return true
	}
	if desiredName == "" {
		return false
	}
	desiredRuntime, desiredKnown := desiredConf.Runtimes[desiredName]
	if actualRuntime.name != desiredName || actualRuntime.known != desiredKnown {
		return true
	}
	if !actualRuntime.known {
		return false
	}
	return !reflect.DeepEqual(actualRuntime.value, desiredRuntime)
}

// processReplacedMarker separates a replacement process's inherited log
// history from its own output in the operator-facing log tail.
const processReplacedMarker = "\n— process replaced —\n"

// carryProcessLogHistory copies the previous process's captured log tail into
// the replacement's monitor. Without this, every process replacement resets
// the model's operator-visible log to an empty buffer and the previous run's
// output becomes unrecoverable.
func carryProcessLogHistory(previous, replacement process.Process) {
	if previous == nil || replacement == nil || previous == replacement {
		return
	}
	previousLog := previous.Logger()
	replacementLog := replacement.Logger()
	if previousLog == nil || replacementLog == nil || previousLog == replacementLog {
		return
	}
	history := previousLog.GetHistory()
	if len(history) == 0 {
		return
	}
	replacementLog.SeedHistory(append(history, processReplacedMarker...))
}

// discardProcessLogMonitor releases the broadcast goroutine of a discarded
// process's log monitor so the monitor and its buffer can be garbage
// collected. A monitor may still be referenced by an in-flight log stream;
// Close only stops the broadcast loop, and the stream's periodic monitor
// re-resolution in the server ends those streams.
func discardProcessLogMonitor(p process.Process) {
	if p == nil {
		return
	}
	if monitor := p.Logger(); monitor != nil {
		monitor.Close()
	}
}

// EnterMaintenance marks a model as in maintenance mode: it keeps its
// configuration but refuses to be started by incoming requests. The reason is
// surfaced to operators and clients alongside the state.
func (b *baseRouter) EnterMaintenance(modelID, reason string) {
	b.maintenanceMu.Lock()
	b.maintenance[modelID] = reason
	b.maintenanceMu.Unlock()
}

// ExitMaintenance clears a model's maintenance mode. It is called by the
// serving path once a start has succeeded and a request has actually been
// served, which is the condition the operator opted into.
func (b *baseRouter) ExitMaintenance(modelID string) {
	b.maintenanceMu.Lock()
	delete(b.maintenance, modelID)
	b.maintenanceMu.Unlock()
}

// InMaintenance reports the model's maintenance reason, if any.
func (b *baseRouter) InMaintenance(modelID string) (string, bool) {
	b.maintenanceMu.RLock()
	defer b.maintenanceMu.RUnlock()
	reason, ok := b.maintenance[modelID]
	return reason, ok
}

// MaintenanceSnapshot returns a copy of the maintenance state for status
// reporting.
func (b *baseRouter) MaintenanceSnapshot() map[string]string {
	b.maintenanceMu.RLock()
	defer b.maintenanceMu.RUnlock()
	out := make(map[string]string, len(b.maintenance))
	for id, reason := range b.maintenance {
		out[id] = reason
	}
	return out
}

func (b *baseRouter) newManagedProcess(modelID string, modelConfig config.ModelConfig) (process.Process, error) {
	b.processMu.RLock()
	factory := b.makeProcess
	decorator := b.processDecorator
	b.processMu.RUnlock()
	var (
		created process.Process
		err     error
	)
	if factory != nil {
		created, err = factory(b.procCtx, modelID, modelConfig)
	} else {
		processLogger := logmon.NewWriter(logmon.NewLinePrefixWriter("["+modelID+"] ", b.logger))
		created, err = process.New(b.procCtx, modelID, modelConfig, processLogger, b.logger)
	}
	if err != nil {
		return nil, err
	}
	if cmd, ok := created.(*process.ProcessCommand); ok && decorator != nil {
		decorator(modelID, cmd)
	}
	return created, nil
}

func (b *baseRouter) unloadTimeoutLocked(modelID string) time.Duration {
	if modelConfig, ok := b.processConfigs[modelID]; ok && modelConfig.UnloadTimeout > 0 {
		return time.Duration(modelConfig.UnloadTimeout) * time.Second
	}
	if modelConfig, ok := b.config.Models[modelID]; ok && modelConfig.UnloadTimeout > 0 {
		return time.Duration(modelConfig.UnloadTimeout) * time.Second
	}
	return time.Duration(b.config.UnloadTimeout) * time.Second
}

// RestartProcess is the scheduler Effects seam. It is intentionally
// asynchronous: the run loop must continue accepting cancellation, status and
// config events while the old process is stopping or the replacement is
// health-checking.
func (b *baseRouter) RestartProcess(modelID string) {
	b.restartProcess(modelID, false)
}

// RestartProcessWithGeneration is the generation-aware form of the
// scheduler's replacement seam. The legacy RestartProcess method remains so
// external Effects implementations continue to work, while lifecycle events
// from this router can be discarded after a competing unload/removal.
func (b *baseRouter) RestartProcessWithGeneration(modelID string) uint64 {
	return b.restartProcess(modelID, false)
}

// ForceRestartProcessWithGeneration is the replacement seam selected after a
// force confirmation. It uses the same process lifecycle as a normal restart,
// but does not wait for ignored compatibility websocket requests first.
func (b *baseRouter) ForceRestartProcessWithGeneration(modelID string) uint64 {
	return b.restartProcess(modelID, true)
}

func (b *baseRouter) restartProcess(modelID string, force bool) uint64 {
	b.processMu.RLock()
	oldProcess := b.processes[modelID]
	oldConfig := b.processConfigs[modelID]
	oldRuntime := b.processRuntime[modelID]
	desiredConfig := b.desiredConfigs[modelID]
	desiredRuntime := runtimeSnapshot(b.config, desiredConfig)
	b.processMu.RUnlock()
	restartCtx, cancel := context.WithCancel(b.shutdownCtx)
	b.restartMu.Lock()
	if previous, ok := b.restartCancels[modelID]; ok {
		previous.suppressCompletion()
		previous.cancel()
	}
	b.restartNextGen++
	generation := b.restartNextGen
	done := make(chan struct{})
	b.restartCancels[modelID] = &restartControl{
		cancel:     cancel,
		ctx:        restartCtx,
		generation: generation,
		done:       done,
		suppressCh: make(chan struct{}),
		forceCh:    make(chan struct{}),
	}
	b.restartMu.Unlock()
	go b.doRestart(restartCtx, done, generation, modelID, oldProcess, oldConfig, oldRuntime, desiredConfig, desiredRuntime, force)
	return generation
}

// ForceCurrentRestart upgrades a restart that already passed the scheduler's
// start gate. It is intentionally a side channel rather than a second restart:
// the existing replacement goroutine remains the sole owner of Stop/EnsureReady.
func (b *baseRouter) ForceCurrentRestart(modelID string) {
	b.restartMu.Lock()
	control := b.restartCancels[modelID]
	if control != nil {
		control.forceRestart()
	}
	b.restartMu.Unlock()
}

// CancelRestart is used by an explicit unload to supersede a restart before
// it can publish a replacement process.
func (b *baseRouter) CancelRestart(modelID string) {
	b.restartMu.Lock()
	control, ok := b.restartCancels[modelID]
	if ok {
		control.suppressCompletion()
		control.cancel()
		delete(b.restartCancels, modelID)
	}
	b.restartMu.Unlock()
	if ok {
		// Explicit unload runs on the router loop. Wait until the restart
		// goroutine has left its process operation before StopProcesses is
		// allowed to touch the same generation.
		<-control.done
	}
}

func (b *baseRouter) finishRestart(modelID string, generation uint64) {
	b.restartMu.Lock()
	control, ok := b.restartCancels[modelID]
	current := ok && control.generation == generation
	if current {
		delete(b.restartCancels, modelID)
	}
	b.restartMu.Unlock()
	if current {
		// Release the child context. context.WithCancel registers it with
		// b.shutdownCtx and only cancel() unregisters it, so dropping the map
		// entry alone leaks one context and its done channel per completed
		// restart for the life of the daemon. Called outside restartMu so the
		// parent's lock is never taken while holding this one.
		control.cancel()
	}
}

func (b *baseRouter) restartIsCurrent(modelID string, generation uint64) bool {
	b.restartMu.Lock()
	control, ok := b.restartCancels[modelID]
	b.restartMu.Unlock()
	return ok && control.generation == generation && control.ctx.Err() == nil
}

func (control *restartControl) suppressCompletion() {
	if control == nil || control.suppressed.Swap(true) {
		return
	}
	close(control.suppressCh)
}

func (control *restartControl) forceRestart() {
	if control == nil || control.forced.Swap(true) {
		return
	}
	close(control.forceCh)
}

func (b *baseRouter) restartCompletionControl(modelID string, generation uint64) *restartControl {
	b.restartMu.Lock()
	control, ok := b.restartCancels[modelID]
	b.restartMu.Unlock()
	if !ok || control == nil || control.generation != generation {
		return nil
	}
	return control
}

func (b *baseRouter) sendRestartProgress(ctx context.Context, event scheduler.RestartProgress) {
	select {
	case b.restartProgressCh <- event:
	case <-ctx.Done():
	case <-b.shutdownCtx.Done():
	}
}

func (b *baseRouter) setRestartProcess(modelID string, generation uint64, p process.Process) {
	if p == nil {
		return
	}
	b.processMu.Lock()
	b.restartProcesses[modelID] = restartProcessState{generation: generation, process: p}
	b.processMu.Unlock()
}

func (b *baseRouter) clearRestartProcess(modelID string, generation uint64) {
	b.processMu.Lock()
	if pending, ok := b.restartProcesses[modelID]; ok && pending.generation == generation {
		delete(b.restartProcesses, modelID)
	}
	b.processMu.Unlock()
}

// installRestartProcess commits a ready replacement only while its restart
// control is still current. The restart mutex fences this check against an
// unload/removal cancellation, so a canceled generation cannot publish a
// process after the scheduler has moved on.
func (b *baseRouter) installRestartProcess(modelID string, generation uint64, replacement process.Process, desiredConfig config.ModelConfig, desiredRuntime processRuntimeSnapshot) bool {
	b.restartMu.Lock()
	control, ok := b.restartCancels[modelID]
	if !ok || control == nil || control.generation != generation || control.ctx.Err() != nil {
		b.restartMu.Unlock()
		return false
	}
	b.processMu.Lock()
	b.processes[modelID] = replacement
	b.processConfigs[modelID] = desiredConfig
	b.processRuntime[modelID] = desiredRuntime
	if pending, ok := b.restartProcesses[modelID]; ok && pending.generation == generation {
		delete(b.restartProcesses, modelID)
	}
	b.processMu.Unlock()
	b.restartMu.Unlock()
	return true
}

func (b *baseRouter) restartCanceledError(modelID string, ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("model %s restart canceled: %w", modelID, err)
	}
	return fmt.Errorf("model %s restart superseded", modelID)
}

func (b *baseRouter) doRestart(restartCtx context.Context, done chan struct{}, generation uint64, modelID string, oldProcess process.Process, oldConfig config.ModelConfig, oldRuntime processRuntimeSnapshot, desiredConfig config.ModelConfig, desiredRuntime processRuntimeSnapshot, force bool) {
	defer func() {
		b.finishRestart(modelID, generation)
		close(done)
	}()
	oldStopTimeout := b.modelUnloadTimeout(oldConfig)
	desiredStopTimeout := b.modelUnloadTimeout(desiredConfig)
	desiredReadyTimeout := b.modelHealthCheckTimeout(desiredConfig)
	oldReadyTimeout := b.modelHealthCheckTimeout(oldConfig)
	if oldProcess == nil {
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: fmt.Errorf("model %s is not handled", modelID)})
		return
	}
	if !force && !b.waitIgnoredWebsocketsForRestart(restartCtx, modelID, generation) {
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
		return
	}
	if err := oldProcess.Stop(oldStopTimeout); err != nil && b.shutdownCtx.Err() == nil && b.logger != nil {
		b.logger.Warnf("%s: stopping %s for restart failed: %v", b.name, modelID, err)
	}
	if restartCtx.Err() != nil {
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
		return
	}

	replacement, err := b.newManagedProcess(modelID, desiredConfig)
	if err == nil {
		carryProcessLogHistory(oldProcess, replacement)
		b.setRestartProcess(modelID, generation, replacement)
		err = replacement.EnsureReady(restartCtx, desiredReadyTimeout)
	}
	if err == nil {
		if restartCtx.Err() != nil {
			_ = replacement.Stop(desiredStopTimeout)
			b.clearRestartProcess(modelID, generation)
			discardProcessLogMonitor(replacement)
			b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
			return
		}
		if !b.restartIsCurrent(modelID, generation) {
			_ = replacement.Stop(desiredStopTimeout)
			b.clearRestartProcess(modelID, generation)
			discardProcessLogMonitor(replacement)
			return
		}
		if !b.installRestartProcess(modelID, generation, replacement, desiredConfig, desiredRuntime) {
			_ = replacement.Stop(desiredStopTimeout)
			b.clearRestartProcess(modelID, generation)
			discardProcessLogMonitor(replacement)
			b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
			return
		}
		discardProcessLogMonitor(oldProcess)
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation})
		return
	}
	if replacement != nil {
		_ = replacement.Stop(desiredStopTimeout)
		b.clearRestartProcess(modelID, generation)
		discardProcessLogMonitor(replacement)
	}
	if restartCtx.Err() != nil {
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
		return
	}
	if b.logger != nil {
		b.logger.Warnf("%s: starting %s with updated configuration failed: %v", b.name, modelID, err)
	}

	// A failed desired start is recoverable when the last process definition can
	// be brought back. The scheduler will keep the waiting room open and expose
	// apply_failed while the restored process serves.
	//
	// With auto-rollback disabled the operator asked for the other trade: keep
	// the new configuration and take the model out of service instead of
	// silently restoring a definition the operator just replaced. The model
	// enters maintenance mode, refuses to be started by incoming requests, and
	// leaves on its own once a later start succeeds and serves a request.
	if !b.rollbackOnModelStartFailure(desiredConfig) {
		if replacement != nil {
			_ = replacement.Stop(desiredStopTimeout)
		}
		b.clearRestartProcess(modelID, generation)
		if oldProcess != nil {
			_ = oldProcess.Stop(oldStopTimeout)
		}
		b.EnterMaintenance(modelID, fmt.Sprintf(
			"starting with the new configuration failed: %v", err))
		if b.logger != nil {
			b.logger.Warnf("%s: %s left in maintenance mode after a failed configuration start (rollbackOnModelStartFailure is false)", b.name, modelID)
		}
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: err, Maintenance: true})
		return
	}

	b.sendRestartProgress(restartCtx, scheduler.RestartProgress{ModelID: modelID, Generation: generation, Status: scheduler.ConfigStatusRollingBack, Err: err})
	rollback, rollbackErr := b.newManagedProcess(modelID, oldConfig)
	if rollbackErr == nil {
		// The failed replacement's monitor carries the inherited history plus
		// the failed attempt's own output — exactly what an operator needs to
		// diagnose the rollback — so hand it to the restored process. When the
		// replacement could not even be created (an invalid desired process
		// definition leaves it nil), the old process still holds the last good
		// run's log, and the rollback must inherit that instead of starting
		// from an empty buffer.
		if replacement != nil {
			carryProcessLogHistory(replacement, rollback)
		} else {
			carryProcessLogHistory(oldProcess, rollback)
		}
		b.setRestartProcess(modelID, generation, rollback)
		rollbackErr = rollback.EnsureReady(restartCtx, oldReadyTimeout)
	}
	if rollbackErr == nil {
		if !b.installRestartProcess(modelID, generation, rollback, oldConfig, oldRuntime) {
			_ = rollback.Stop(oldStopTimeout)
			b.clearRestartProcess(modelID, generation)
			discardProcessLogMonitor(rollback)
			b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
			return
		}
		discardProcessLogMonitor(oldProcess)
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: err, RolledBack: true})
		return
	}
	if rollback != nil {
		_ = rollback.Stop(oldStopTimeout)
		b.clearRestartProcess(modelID, generation)
		discardProcessLogMonitor(rollback)
	}
	if restartCtx.Err() != nil {
		b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: b.restartCanceledError(modelID, restartCtx)})
		return
	}
	if b.logger != nil {
		b.logger.Errorf("%s: restoring %s after configuration restart failed: %v", b.name, modelID, rollbackErr)
	}
	b.sendRestartDone(restartCtx, scheduler.RestartDone{ModelID: modelID, Generation: generation, Err: errors.Join(err, fmt.Errorf("rollback failed: %w", rollbackErr))})
}

func (b *baseRouter) sendRestartDone(ctx context.Context, event scheduler.RestartDone) {
	control := b.restartCompletionControl(event.ModelID, event.Generation)
	if control == nil {
		return
	}
	select {
	case b.restartDoneCh <- event:
	case <-b.shutdownCtx.Done():
	case <-b.runDone:
	case <-control.suppressCh:
	case <-ctx.Done():
		// A context cancellation is not necessarily a scheduler cancellation.
		// If the generation is still registered, deliver the terminal event so a
		// restart cannot remain stuck in "restarting". CancelRestart marks the
		// control suppressed before canceling it, so its unload path still exits
		// without waiting on a run-loop event that it cannot receive yet.
		select {
		case b.restartDoneCh <- event:
		case <-b.shutdownCtx.Done():
		case <-b.runDone:
		case <-control.suppressCh:
		}
	}
}

// RemoveProcess is the scheduler Effects seam for topology deletion. The
// process is deleted from the registry only after Stop completes and the event
// has returned to the run loop.
func (b *baseRouter) RemoveProcess(modelID string) {
	b.processMu.Lock()
	managed := b.processes[modelID]
	timeout := b.unloadTimeoutLocked(modelID)
	b.removalNextGen++
	generation := b.removalNextGen
	stopDone := make(chan struct{})
	b.removalGenerations[modelID] = generation
	b.removalStopWaits[modelID] = removalStopWait{generation: generation, done: stopDone}
	b.processMu.Unlock()
	go func() {
		var err error
		if managed == nil {
			err = fmt.Errorf("model %s is not handled", modelID)
		} else {
			err = managed.Stop(timeout)
		}
		close(stopDone)
		b.processMu.Lock()
		if current, ok := b.removalStopWaits[modelID]; ok && current.generation == generation {
			delete(b.removalStopWaits, modelID)
		}
		b.processMu.Unlock()
		select {
		case b.removeDoneCh <- scheduler.RemovalDone{ModelID: modelID, Generation: generation, Err: err}:
		case <-b.shutdownCtx.Done():
		}
	}()
}

func (b *baseRouter) invalidateRemoval(modelID string) {
	b.processMu.Lock()
	delete(b.removalGenerations, modelID)
	b.processMu.Unlock()
}

func (b *baseRouter) removalIsCurrent(modelID string, generation uint64) bool {
	b.processMu.RLock()
	current, ok := b.removalGenerations[modelID]
	b.processMu.RUnlock()
	return ok && current == generation
}

func (b *baseRouter) waitForRemovalStop(ctx context.Context, modelID string) bool {
	b.processMu.RLock()
	wait, pending := b.removalStopWaits[modelID]
	b.processMu.RUnlock()
	if !pending {
		return true
	}
	select {
	case <-wait.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// grant sends a response back to the caller of ServeHTTP and tells us
// whether the caller was still there to receive it.
//
// Each ServeHTTP creates a fresh, UNBUFFERED respond channel and parks in
// a select waiting on it. "Unbuffered" is the important word: a send only
// completes when the other side is actively receiving. So if this send
// succeeds, we know for a fact the caller picked up the response and will
// act on it. If the caller has already given up (its request context was
// cancelled, e.g. the HTTP client disconnected) or the router is shutting
// down, the send never lands, one of the other select cases fires, and we
// report back that the grant did NOT happen.
//
// That distinction matters for in-flight bookkeeping — see GrantServe.
func (b *baseRouter) grant(req scheduler.HandlerReq, resp scheduler.HandlerResp) bool {
	select {
	case req.Respond <- resp:
		return true
	case <-req.Ctx.Done():
		return false
	case <-b.shutdownCtx.Done():
		return false
	}
}

// ModelState implements scheduler.Effects.
func (b *baseRouter) ModelState(modelID string) (process.ProcessState, bool) {
	b.processMu.RLock()
	if pending, ok := b.restartProcesses[modelID]; ok && pending.process != nil {
		p := pending.process
		b.processMu.RUnlock()
		return p.State(), true
	}
	p, ok := b.processes[modelID]
	b.processMu.RUnlock()
	if !ok {
		var zero process.ProcessState
		return zero, false
	}
	return p.State(), true
}

// StartSwap implements scheduler.Effects, launching the swap goroutine.
func (b *baseRouter) StartSwap(modelID string, evict []string) {
	go b.doSwap(modelID, evict)
}

// GrantError implements scheduler.Effects.
func (b *baseRouter) GrantError(req scheduler.HandlerReq, err error) {
	b.grant(req, scheduler.HandlerResp{Err: err})
}

// GrantServe implements scheduler.Effects. It hands the caller a wrapped
// p.ServeHTTP (via trackedServe) so the run loop hears about the request
// finishing, and reports whether the caller received it. The scheduler bumps
// its in-flight count only on a true return: if grant() returns false the
// caller already walked away and trackedServe will never run, so no matching
// decrement will ever arrive — incrementing would strand the counter at >0 and
// the router would never again be willing to evict this model.
func (b *baseRouter) GrantServe(req scheduler.HandlerReq, modelID string) bool {
	return b.grantServe(req, modelID, 0)
}

// GrantServeWithGeneration is an optional extension used by the FIFO scheduler
// when a force restart needs to fence a late completion from the old process.
// Keeping GrantServe's original Effects signature preserves compatibility with
// other scheduler embedders.
func (b *baseRouter) GrantServeWithGeneration(req scheduler.HandlerReq, modelID string, generation uint64) bool {
	return b.grantServe(req, modelID, generation)
}

func (b *baseRouter) grantServe(req scheduler.HandlerReq, modelID string, generation uint64) bool {
	b.processMu.RLock()
	p := b.processes[modelID]
	b.processMu.RUnlock()
	if p == nil {
		b.grant(req, scheduler.HandlerResp{Err: scheduler.ErrModelNotFound})
		return false
	}
	return b.grant(req, scheduler.HandlerResp{HandleFunc: b.trackedServe(modelID, p, generation)})
}

// StopProcesses implements scheduler.Effects, stopping the named processes in
// parallel and blocking until all have stopped.
func (b *baseRouter) StopProcesses(timeout time.Duration, ids []string) {
	var wg sync.WaitGroup
	for _, id := range ids {
		b.processMu.RLock()
		p, ok := b.processes[id]
		b.processMu.RUnlock()
		if !ok {
			continue
		}
		wg.Add(1)
		go func(id string, p process.Process) {
			defer wg.Done()
			if err := p.Stop(timeout); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
			}
		}(id, p)
	}
	wg.Wait()
}

// AdoptDesiredProcessConfig replaces a stopped model's process object with one
// built from the current desired configuration, so the next start uses what the
// operator already saved rather than the configuration the object was created
// with. It reports false when the model is not stopped — a start may be in
// flight, or a failed stop may have left the old process running — in which
// case the caller keeps the pending-restart state.
//
// This is the stopped half of the same rule handleReconfigure applies: a
// generation that is not running has nothing left to disturb.
func (b *baseRouter) AdoptDesiredProcessConfig(modelID string) bool {
	var swapInFlight func(string) bool
	if provider, ok := b.schedule.(interface{ SwapInFlight(string) bool }); ok {
		swapInFlight = provider.SwapInFlight
	}
	if swapInFlight != nil && swapInFlight(modelID) {
		return false
	}

	b.processMu.RLock()
	managed, hasManaged := b.processes[modelID]
	desired, hasDesired := b.desiredConfigs[modelID]
	conf := b.config
	b.processMu.RUnlock()
	if !hasManaged || !hasDesired {
		return false
	}
	if managed.State() != process.StateStopped {
		return false
	}

	created, err := b.newManagedProcess(modelID, desired)
	if err != nil {
		b.logger.Warnf("%s: adopting the desired configuration for stopped %s failed: %v", b.name, modelID, err)
		return false
	}
	carryProcessLogHistory(managed, created)

	b.processMu.Lock()
	previous := b.processes[modelID]
	b.processes[modelID] = created
	b.processConfigs[modelID] = desired
	b.processRuntime[modelID] = runtimeSnapshot(conf, desired)
	b.processMu.Unlock()
	if previous != created {
		discardProcessLogMonitor(previous)
	}
	return true
}

// trackedServe is the wrapper that closes the loop on in-flight tracking.
// It runs p.ServeHTTP normally; the only added behaviour is a deferred
// send on serveDoneCh after the handler returns. That send is what tells
// the run loop "this model now has one fewer request in flight — go look
// at the queue again, you may be able to start a swap you previously had
// to defer."
//
// The select on shutdownCtx.Done() is a release valve: if the router is
// already shutting down, nobody is reading serveDoneCh, so we drop the
// notification rather than blocking the HTTP goroutine forever.
func (b *baseRouter) trackedServe(modelID string, p process.Process, generation uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case b.serveDoneCh <- scheduler.ServeDoneEvent{ModelID: modelID, Generation: generation}:
			case <-b.shutdownCtx.Done():
			}
		}()
		// The scheduler only grants a handler once the process is serving, so
		// reaching this point proves the model both started and accepted a
		// request: the condition under which maintenance mode ends. Clearing
		// before the response is written keeps a request that fails inside the
		// upstream from spuriously exiting maintenance.
		if _, ok := b.InMaintenance(modelID); ok {
			b.ExitMaintenance(modelID)
		}
		swaputil.ReportInferencePhase(r.Context(), "prefill", "prefill started")
		p.ServeHTTP(w, r)
	}
}

func (b *baseRouter) beginIgnoredWebsocket(modelID string) {
	b.ignoredWebsocketMu.Lock()
	b.ignoredWebsockets[modelID]++
	b.ignoredWebsocketMu.Unlock()
}

func (b *baseRouter) endIgnoredWebsocket(modelID string) {
	b.ignoredWebsocketMu.Lock()
	if b.ignoredWebsockets[modelID] <= 1 {
		delete(b.ignoredWebsockets, modelID)
	} else {
		b.ignoredWebsockets[modelID]--
	}
	close(b.ignoredWebsocketDone)
	b.ignoredWebsocketDone = make(chan struct{})
	b.ignoredWebsocketMu.Unlock()
}

// IgnoredRequests is an optional scheduler Effects seam used for lifecycle
// status counters and for protecting a confirmed restart from compatibility
// websocket handlers that intentionally bypass ordinary admission.
func (b *baseRouter) IgnoredRequests(modelID string) int {
	b.ignoredWebsocketMu.Lock()
	defer b.ignoredWebsocketMu.Unlock()
	return b.ignoredWebsockets[modelID]
}

func (b *baseRouter) waitIgnoredWebsocketsForRestart(ctx context.Context, modelID string, generation uint64) bool {
	for {
		b.ignoredWebsocketMu.Lock()
		if b.ignoredWebsockets[modelID] == 0 {
			b.ignoredWebsocketMu.Unlock()
			return true
		}
		done := b.ignoredWebsocketDone
		b.ignoredWebsocketMu.Unlock()

		b.restartMu.Lock()
		control := b.restartCancels[modelID]
		var force <-chan struct{}
		if control != nil && control.generation == generation {
			force = control.forceCh
		}
		b.restartMu.Unlock()
		select {
		case <-done:
		case <-force:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func (b *baseRouter) ignoredWebsocketHandler(modelID string, p process.Process) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.beginIgnoredWebsocket(modelID)
		defer func() {
			b.endIgnoredWebsocket(modelID)
			select {
			case b.serveDoneCh <- scheduler.ServeDoneEvent{ModelID: modelID, Ignored: true}:
			case <-b.shutdownCtx.Done():
			}
		}()
		p.ServeHTTP(w, r)
	}
}

func (b *baseRouter) lifecycleBlocksIgnoredWebsocket(modelID string) bool {
	b.statusMu.RLock()
	status, ok := b.statuses[modelID]
	b.statusMu.RUnlock()
	if !ok {
		return false
	}
	switch status.ConfigStatus {
	case scheduler.ConfigStatusDraining,
		scheduler.ConfigStatusRestarting,
		scheduler.ConfigStatusRollingBack,
		scheduler.ConfigStatusRemoving,
		scheduler.ConfigStatusUnloading:
		return true
	default:
		return false
	}
}

func (b *baseRouter) doSwap(modelID string, toStop []string) {
	timeout := b.healthCheckTimeout()

	var wg sync.WaitGroup
	for _, mID := range toStop {
		b.processMu.RLock()
		managed := b.processes[mID]
		b.processMu.RUnlock()
		if managed == nil {
			continue
		}
		wg.Add(1)
		go func(p process.Process, id string) {
			defer wg.Done()
			if err := p.Stop(timeout); err != nil {
				b.logger.Warnf("%s: stopping %s failed: %v", b.name, id, err)
			}
		}(managed, mID)
	}
	wg.Wait()

	// EnsureReady rather than a State() check followed by Run: the router must
	// not assume anything about the process. Deciding out here means acting on
	// a snapshot that the process's own run loop can invalidate at any moment —
	// a TTL unload landing in that window used to leave the swap waiting on a
	// process nobody was ever going to start (issue #946). EnsureReady makes
	// the same decision inside the process, where the state is owned.
	var err error
	if !b.waitForRemovalStop(b.shutdownCtx, modelID) {
		err = b.shutdownCtx.Err()
	} else {
		b.processMu.RLock()
		target := b.processes[modelID]
		b.processMu.RUnlock()
		if target == nil {
			err = scheduler.ErrModelNotFound
		} else {
			err = target.EnsureReady(b.shutdownCtx, timeout)
		}
	}
	if err != nil && b.shutdownCtx.Err() == nil {
		// Quiet during shutdown: every in-flight swap fails at once there, and
		// that is expected rather than worth a warning per model.
		b.logger.Warnf("%s: starting %s failed: %v", b.name, modelID, err)
	}

	select {
	case b.swapDoneCh <- scheduler.SwapDone{ModelID: modelID, Err: err}:
	case <-b.shutdownCtx.Done():
	}
}
func (b *baseRouter) handleShutdown(req shutdownReq) {
	shutdownErr := fmt.Errorf("%s is shutting down", b.name)

	// Cancel shutdownCtx first so any waiter that is currently parked on
	// its respond channel can exit via its own shutdownCtx.Done() branch.
	// The OnShutdown grants below then either land (waiter happened to receive
	// before noticing shutdown) or fall through immediately via grant's
	// shutdownCtx case — either way the waiter sees a non-OK response.
	// This does NOT touch processes: their lifetime is procCtx, cancelled
	// only after the graceful Stop() calls below have reaped them.
	b.shutdownFn()

	b.schedule.OnShutdown(shutdownErr)

	stopTimeout := req.timeout
	if stopTimeout <= 0 {
		stopTimeout = b.healthCheckTimeout()
	}

	b.processMu.RLock()
	processes := make(map[string]process.Process, len(b.processes))
	for id, managed := range b.processes {
		processes[id] = managed
	}
	b.processMu.RUnlock()

	var wg sync.WaitGroup
	for i, p := range processes {
		wg.Add(1)
		go func(id string, p process.Process) {
			defer wg.Done()
			if err := p.Stop(stopTimeout); err != nil {
				b.logger.Warnf("%s failed to stop process %s: %v", b.name, id, err)
			}
		}(i, p)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	if req.timeout > 0 {
		select {
		case <-done:
		case <-time.After(req.timeout):
			<-done
		}
	} else {
		<-done
	}

	// Every process is stopped (children reaped via Stop()). Cancel procCtx so
	// the process run-loop goroutines exit; they are already StateStopped, so
	// this is a clean no-op kill rather than a forced teardown.
	b.procCancel()

	req.respond <- nil
}

func (b *baseRouter) healthCheckTimeout() time.Duration {
	b.processMu.RLock()
	seconds := b.config.HealthCheckTimeout
	b.processMu.RUnlock()
	t := time.Duration(seconds) * time.Second
	if t <= 0 {
		return 30 * time.Second
	}
	return t
}

func (b *baseRouter) modelHealthCheckTimeout(model config.ModelConfig) time.Duration {
	seconds := model.HealthCheckTimeout
	if seconds <= 0 {
		b.processMu.RLock()
		seconds = b.config.HealthCheckTimeout
		b.processMu.RUnlock()
	}
	if seconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(seconds) * time.Second
}

func (b *baseRouter) modelUnloadTimeout(model config.ModelConfig) time.Duration {
	seconds := model.UnloadTimeout
	if seconds <= 0 {
		b.processMu.RLock()
		seconds = b.config.UnloadTimeout
		b.processMu.RUnlock()
	}
	return time.Duration(seconds) * time.Second
}

// rollbackOnModelStartFailure reports whether a failed configuration start
// should restore the last configuration that started. The model argument is
// accepted so a future per-model override has a place to live; the setting is
// global today. Config loading normalizes the pointer, so a nil value (a config
// assembled outside the loader) means the historical behaviour.
func (b *baseRouter) rollbackOnModelStartFailure(model config.ModelConfig) bool {
	b.processMu.RLock()
	flag := b.config.RollbackOnModelStartFailure
	b.processMu.RUnlock()
	return flag == nil || *flag
}

// unloadTimeout returns the graceful stop timeout for a model. Config parsing
// guarantees both the global and per-model unloadTimeout are populated (a zero
// model value is rewritten to the global default on parse), so no zero handling
// is needed here.
func (b *baseRouter) unloadTimeout(modelID string) time.Duration {
	b.processMu.RLock()
	defer b.processMu.RUnlock()
	return b.unloadTimeoutLocked(modelID)
}

func (b *baseRouter) Handles(model string) bool {
	b.processMu.RLock()
	defer b.processMu.RUnlock()
	_, ok := b.processes[model]
	return ok
}

func (b *baseRouter) ProcessLogger(modelID string) (*logmon.Monitor, bool) {
	b.processMu.RLock()
	defer b.processMu.RUnlock()
	if pending, ok := b.restartProcesses[modelID]; ok && pending.process != nil {
		return pending.process.Logger(), true
	}
	if p, ok := b.processes[modelID]; ok {
		return p.Logger(), true
	}
	return nil, false
}

// ModelSleeping reports the optional sleep projection of a local process.
// The process itself remains StateReady while sleeping so normal dispatch can
// wake it instead of starting a duplicate generation.
func (b *baseRouter) ModelSleeping(modelID string) bool {
	b.processMu.RLock()
	p, ok := b.processes[modelID]
	b.processMu.RUnlock()
	if !ok {
		return false
	}
	controller, ok := p.(process.SleepController)
	return ok && controller.Sleeping()
}

// SleepModel delegates an optional vLLM sleep operation to the process that
// owns the model. Keeping this outside LocalRouter preserves compatibility
// with embedders that provide their own router implementations.
func (b *baseRouter) SleepModel(ctx context.Context, modelID string, level int) error {
	b.processMu.RLock()
	p, ok := b.processes[modelID]
	b.processMu.RUnlock()
	if !ok {
		return ErrNoLocalModelFound
	}
	controller, ok := p.(process.SleepController)
	if !ok {
		return process.ErrSleepUnsupported
	}
	return controller.Sleep(ctx, level)
}

// WakeModel delegates an optional vLLM wake operation to the local process.
func (b *baseRouter) WakeModel(ctx context.Context, modelID string) error {
	b.processMu.RLock()
	p, ok := b.processes[modelID]
	b.processMu.RUnlock()
	if !ok {
		return ErrNoLocalModelFound
	}
	controller, ok := p.(process.SleepController)
	if !ok {
		return process.ErrSleepUnsupported
	}
	return controller.Wake(ctx)
}

// RunningModels returns the current state of every process that is not stopped
// or shut down. The processes map keys are fixed at construction and State()
// is a snapshot, so this is safe to call without the run loop.
func (b *baseRouter) RunningModels() map[string]process.ProcessState {
	running := make(map[string]process.ProcessState)
	b.processMu.RLock()
	defer b.processMu.RUnlock()
	for id, p := range b.processes {
		st := p.State()
		if st == process.StateStopped || st == process.StateShutdown {
			continue
		}
		running[id] = st
	}
	for id, pending := range b.restartProcesses {
		if pending.process == nil {
			continue
		}
		st := pending.process.State()
		if st == process.StateStopped || st == process.StateShutdown {
			continue
		}
		// A restart candidate is created after the old process has been stopped;
		// prefer its state so the control plane does not report "stopped" while
		// the replacement is actually starting or ready.
		running[id] = st
	}
	return running
}

// LoadConflicts returns the running local models that the active topology
// would evict before loading modelID. It is a read-only preview for the
// control-plane confirmation dialog; the scheduler still recomputes the
// eviction set when the actual request is admitted.
func (b *baseRouter) LoadConflicts(modelID string) []string {
	if b == nil || strings.TrimSpace(modelID) == "" {
		return nil
	}
	b.processMu.RLock()
	conf := b.config
	logger := b.logger
	b.processMu.RUnlock()

	planner, _, err := plannerForConfig(conf, logger)
	if err != nil {
		return nil
	}
	running := b.RunningModels()
	ids := make([]string, 0, len(running))
	for id := range running {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return planner.EvictionFor(modelID, ids)
}

// Unload stops the named models, or every running model when none are named.
// It blocks until each targeted process has stopped.
//
// The request is funneled through the run loop so eviction is coordinated
// with the rest of the router's state: pending swap waiters for an
// unloaded model are released with an error, queued requests for unloaded
// models are dropped, and any deferred swaps that were waiting on those
// models become eligible to start.
//
// In-flight requests being served by an unloaded process are not waited
// for — Stop kills the upstream, those callers see whatever error the
// reverse proxy surfaces and may retry. Their trackedServe defers fire
// normally and decrement inFlight as the dying handlers return.
//
// A timeout <= 0 unloads each targeted model with its configured
// unloadTimeout: targets sharing a timeout are stopped in parallel within one
// unload request, and the requests are processed smallest timeout first. The
// requests are sequential, so a hung stop on a large model (long timeouts
// usually mean multi-node unloads) cannot delay reclaiming the quick ones
// queued behind it. A positive timeout overrides the configured values and
// stops every target with that timeout.
func (b *baseRouter) Unload(timeout time.Duration, models ...string) {
	targets := models
	if len(targets) == 0 {
		b.processMu.RLock()
		targets = make([]string, 0, len(b.processes))
		for id := range b.processes {
			targets = append(targets, id)
		}
		b.processMu.RUnlock()
	}
	if len(targets) == 0 {
		return
	}

	if timeout > 0 {
		b.sendUnload(targets, timeout)
		return
	}
	buckets := make(map[time.Duration][]string)
	for _, id := range targets {
		t := b.unloadTimeout(id)
		buckets[t] = append(buckets[t], id)
	}
	timeouts := make([]time.Duration, 0, len(buckets))
	for t := range buckets {
		timeouts = append(timeouts, t)
	}
	sort.Slice(timeouts, func(i, j int) bool { return timeouts[i] < timeouts[j] })
	for _, t := range timeouts {
		b.sendUnload(buckets[t], t)
	}
}

// sendUnload funnels one unload request through the run loop and blocks until
// the scheduler has stopped the targeted processes.
func (b *baseRouter) sendUnload(targets []string, timeout time.Duration) {
	req := unloadReq{targets: targets, timeout: timeout, respond: make(chan struct{})}
	select {
	case b.unloadCh <- req:
	case <-b.runDone:
		return
	}
	<-req.respond
}

func (b *baseRouter) Shutdown(timeout time.Duration) error {
	if !b.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("%s shutdown already in progress", b.name)
	}
	req := shutdownReq{timeout: timeout, respond: make(chan error, 1)}
	select {
	case b.shutdownCh <- req:
	case <-b.runDone:
		return nil
	}
	return <-req.respond
}

func (b *baseRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if b.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	b.processMu.RLock()
	conf := b.config
	b.processMu.RUnlock()
	data, err := swaputil.FetchContext(req, conf)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	// Maintenance mode: the model failed to start with the configuration the
	// operator applied and auto-rollback is disabled, so it keeps that
	// configuration and is taken out of service. Incoming requests must not
	// start it — that would silently re-attempt a configuration known to fail
	// and hide the failure behind a queue. The model leaves maintenance on its
	// own once an explicit start succeeds and serves a request.
	//
	// An operator's explicit start is the exception: the load button in the UI
	// goes through /upstream/<model>/, and without that path the model could
	// never leave the state it is supposed to exit on its own. Those requests
	// carry the control-plane marker and are allowed to attempt the start.
	if reason, ok := b.InMaintenance(data.ModelID); ok && !swaputil.IsOperatorStart(req) {
		swaputil.SendResponse(w, req, http.StatusServiceUnavailable,
			fmt.Sprintf("model %s is in maintenance mode: %s", data.ModelID, reason))
		return
	}

	// Ignored websocket connections remain outside ordinary scheduler admission
	// for backwards compatibility, but a confirmed config restart/removal must
	// route new handshakes through the lifecycle wait room. Existing ignored
	// websocket handlers are counted separately and drained before a confirmed
	// replacement or unload stops their old process.
	if swaputil.ShouldIgnoreWebsocket(req, conf) && !b.lifecycleBlocksIgnoredWebsocket(data.ModelID) {
		b.processMu.RLock()
		p, ok := b.processes[data.ModelID]
		b.processMu.RUnlock()
		if !ok {
			swaputil.SendError(w, req, scheduler.ErrModelNotFound)
			return
		}
		if p.State() != process.StateReady {
			swaputil.SendResponse(w, req, http.StatusConflict,
				fmt.Sprintf("model %s is not loaded; ignored websocket requests cannot start it", data.ModelID))
			return
		}
		b.ignoredWebsocketHandler(data.ModelID, p)(w, req)
		return
	}

	hr := scheduler.HandlerReq{
		Model: data.ModelID,
		Ctx:   req.Context(),
		// Unbuffered: a successful send on Respond proves the waiter is
		// alive and consuming. grant() relies on this to avoid handing a
		// handleFunc to a cancelled waiter and leaking the inFlight count.
		Admit:      make(chan error, 1),
		Respond:    make(chan scheduler.HandlerResp),
		PositionCh: make(chan int, 1),
	}

	select {
	case b.handlerCh <- hr:
	case <-req.Context().Done():
		return
	case <-b.shutdownCtx.Done():
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}

	var admissionErr error
	select {
	case admissionErr = <-hr.Admit:
	case <-req.Context().Done():
		select {
		case b.cancelCh <- hr:
		case <-b.shutdownCtx.Done():
		}
		return
	case <-b.shutdownCtx.Done():
		swaputil.SendError(w, req, fmt.Errorf("%s is shutting down", b.name))
		return
	}
	if admissionErr != nil {
		swaputil.SendError(w, req, admissionErr)
		return
	}

	isModelReady := false
	b.processMu.RLock()
	if p, ok := b.processes[data.ModelID]; ok {
		isModelReady = p.State() == process.StateReady
	}
	b.processMu.RUnlock()
	shouldShowLoading := data.Streaming && data.SendLoadingState && isLoadingPath(req.URL.Path) && !isModelReady

	var lw *loadingWriter
	cancelLoad := func() {}
	if shouldShowLoading {
		var swapCtx context.Context
		swapCtx, cancelLoad = context.WithCancel(req.Context())
		lw = newLoadingWriter(b.logger, data.ModelID, w, req)
		go lw.start(swapCtx)
		go func() {
			for {
				select {
				case pos := <-hr.PositionCh:
					lw.setUpdate(fmt.Sprintf("Queue position: #%d", pos))
				case <-swapCtx.Done():
					return
				}
			}
		}()
	}

	// finishLoading stops the loading stream and fences its goroutine off from
	// the ResponseWriter before the real handler (or ServeHTTP's return)
	// reclaims it. release() must run even when waitForCompletion times out:
	// otherwise a still-streaming goroutine flushes a finalized response and
	// panics on the recycled *bufio.Writer.
	//
	// A non-nil streamErr is framed into the stream first, while writes still
	// reach the client: the 200 is already committed, so this is the only way
	// the error can be reported at all.
	finishLoading := func(streamErr error) {
		cancelLoad()
		if lw != nil {
			lw.waitForCompletion(1 * time.Second)
			if streamErr != nil {
				lw.sendError(streamErr)
			}
			lw.release()
		}
	}

	// reportError sends err as a normal error response, for the paths where no
	// loading stream was live to carry it in-band. When one was, finishLoading
	// has already framed it into the stream and a second report would append a
	// bare JSON line that SSE parsers discard.
	reportError := func(err error) {
		if lw == nil {
			swaputil.SendError(w, req, err)
		}
	}

	var resp scheduler.HandlerResp
	select {
	case resp = <-hr.Respond:
		// Pass the dispatch error in so it is framed into the stream before
		// release fences the writer; a nil error just ends the stream.
		finishLoading(resp.Err)
	case <-req.Context().Done():
		// The client is gone, so there is nobody to report to.
		finishLoading(nil)
		// Notify the scheduler so it can prune this request from its queue
		// and swap waiters. Without this, a queued request whose client left
		// would sit in the scheduler until drainQueue eventually starts a
		// wasted model load for it.
		select {
		case b.cancelCh <- hr:
		case <-b.shutdownCtx.Done():
		}
		return
	case <-b.shutdownCtx.Done():
		shutdownErr := fmt.Errorf("%s is shutting down", b.name)
		finishLoading(shutdownErr)
		reportError(shutdownErr)
		return
	}

	if resp.Err != nil {
		reportError(resp.Err)
		return
	}
	resp.HandleFunc(w, req)
}
