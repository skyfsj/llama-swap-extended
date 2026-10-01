package server

import (
	"context"
	"reflect"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// ConfigReconciler is the process-local configuration coordination seam. It
// owns immutable desired/active/last-known-good snapshots and serializes all
// callers (file watcher, SIGHUP and Config API) through one apply callback.
// The callback is responsible for publishing the snapshot to long-lived
// runtime modules; this type never constructs a Server or listener.
type ConfigReconciler struct {
	mu sync.Mutex

	active   config.Config
	desired  config.Config
	lastGood config.Config

	activeRevision  uint64
	desiredRevision uint64
	restartPaths    []string

	applying          bool
	applyDone         chan struct{}
	lastApplyError    error
	completedRevision uint64
	apply             func(active, desired config.Config, changes config.ConfigChangeSet) error
	restart           func(modelID string) error
	forceRestart      func(modelID string) error
	onApplied         func(activeRevision, desiredRevision uint64)
}

type ConfigReconcilerStatus struct {
	ActiveRevision  uint64
	DesiredRevision uint64
	RestartRequired bool
	RestartPaths    []string
}

func NewConfigReconciler(initial config.Config, apply func(active, desired config.Config, changes config.ConfigChangeSet) error, restart func(string) error) *ConfigReconciler {
	return &ConfigReconciler{
		active:   initial,
		desired:  initial,
		lastGood: initial,
		apply:    apply,
		restart:  restart,
	}
}

// Reconcile serializes and coalesces candidates. If another source submits a
// newer candidate while the apply callback is running, the callback is invoked
// again with that newest candidate before Reconcile returns.
func (r *ConfigReconciler) Reconcile(candidate config.Config) error {
	if r == nil {
		return nil
	}
	for {
		r.mu.Lock()
		r.desired = candidate
		r.desiredRevision++
		candidateRevision := r.desiredRevision
		if r.applying {
			done := r.applyDone
			r.mu.Unlock()
			<-done
			r.mu.Lock()
			if r.completedRevision >= candidateRevision {
				err := r.lastApplyError
				r.mu.Unlock()
				return err
			}
			r.mu.Unlock()
			// The owner finished a previous candidate before this one arrived.
			// Loop once to become the owner of the still-new desired snapshot.
			continue
		}
		r.applying = true
		r.applyDone = make(chan struct{})
		done := r.applyDone
		r.mu.Unlock()
		return r.applyLoop(done)
	}
}

func (r *ConfigReconciler) applyLoop(done chan struct{}) error {
	var applyErr error

	for {
		r.mu.Lock()
		desired := r.desired
		active := r.active
		desiredRevision := r.desiredRevision
		r.mu.Unlock()

		changes := config.Compare(active, desired)
		// Daemon-restart keys (/store/path, /runtimeManager/root, /logToStdout)
		// stay at their active values in the effective snapshot. Comparing the
		// effective view instead of the raw desired keeps a pending daemon
		// restart from replaying the whole apply callback on every unrelated
		// edit; the callback only needs to run when something hot-applicable
		// really changed.
		effective, restartPaths := config.ApplyDaemonRestartPolicy(active, desired)
		applyErr = nil
		if r.apply != nil && !reflect.DeepEqual(active, effective) {
			applyErr = r.apply(active, desired, changes)
		}

		r.mu.Lock()
		if applyErr != nil {
			// A newer file/API/SIGHUP candidate may have arrived while the
			// callback was validating or publishing this snapshot. Do not let
			// an error for the stale candidate discard that newer desired
			// version; retry the newest candidate through this same owner.
			if r.desiredRevision != desiredRevision {
				r.mu.Unlock()
				continue
			}
			r.applying = false
			r.lastApplyError = applyErr
			r.completedRevision = desiredRevision
			close(done)
			r.mu.Unlock()
			return applyErr
		}
		r.active = effective
		r.lastGood = effective
		r.restartPaths = append([]string(nil), restartPaths...)
		r.activeRevision++
		activeRevision := r.activeRevision
		desiredRevisionNow := r.desiredRevision
		onApplied := r.onApplied
		r.mu.Unlock()
		if onApplied != nil {
			onApplied(activeRevision, desiredRevisionNow)
		}
		r.mu.Lock()
		if reflect.DeepEqual(desired, r.desired) {
			r.applying = false
			r.lastApplyError = nil
			r.completedRevision = desiredRevision
			close(done)
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
	}
}

func (r *ConfigReconciler) RestartModel(modelID string) error {
	if r == nil || r.restart == nil {
		return configRestartUnavailableError{}
	}
	// Do not race a config callback that is still publishing a newer desired
	// snapshot. Waiting outside the mutex lets the apply loop finish normally;
	// taking the mutex again before invoking restart makes the confirmation and
	// the next reconcile deterministic with respect to one another.
	r.mu.Lock()
	for r.applying {
		done := r.applyDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
	restart := r.restart
	err := restart(modelID)
	r.mu.Unlock()
	return err
}

// ForceRestartModel confirms a pending model restart through the optional force
// callback, then cancels the model's tracked requests. Keeping both operations
// under the reconciler lock makes the transition atomic with respect to
// configuration applies; the router still owns process replacement and
// rollback.
func (r *ConfigReconciler) ForceRestartModel(modelID string, cancel func(string)) error {
	if r == nil || (r.restart == nil && r.forceRestart == nil) {
		return configRestartUnavailableError{}
	}
	// Do not race a config callback that is still publishing a newer desired
	// snapshot. Waiting outside the mutex lets the apply loop finish normally;
	// taking the mutex again before invoking restart makes the confirmation and
	// the next reconcile deterministic with respect to one another.
	r.mu.Lock()
	for r.applying {
		done := r.applyDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
	restart := r.forceRestart
	if restart == nil {
		restart = r.restart
	}
	err := restart(modelID)
	if err == nil && cancel != nil {
		cancel(modelID)
	}
	r.mu.Unlock()
	return err
}

// SetForceRestart attaches the router operation used by the force-restart
// control path. A nil callback keeps ForceRestartModel compatible with older
// embedders by falling back to the ordinary restart callback.
func (r *ConfigReconciler) SetForceRestart(fn func(string) error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.forceRestart = fn
	r.mu.Unlock()
}

// WithRestartLock serializes a runtime-triggered model replacement with the
// same configuration apply lock used by file watches and the Config API. The
// callback may wait for a FIFO drain/restart; keeping the lock for that whole
// transaction prevents a concurrent YAML edit from changing the desired
// process definition between a runtime pointer switch and its rollback.
func (r *ConfigReconciler) WithRestartLock(ctx context.Context, fn func() error) error {
	if fn == nil {
		return nil
	}
	if r == nil {
		return fn()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	for r.applying {
		done := r.applyDone
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		r.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	err := fn()
	r.mu.Unlock()
	return err
}

// SetRevisionPublisher attaches the bridge that publishes reconciler revision
// numbers to the long-lived router. It is separate from the constructor so
// tests and embedders can install it after creating the reconciler without
// changing the existing constructor contract.
func (r *ConfigReconciler) SetRevisionPublisher(fn func(activeRevision, desiredRevision uint64)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.onApplied = fn
	r.mu.Unlock()
}

func (r *ConfigReconciler) Status() ConfigReconcilerStatus {
	if r == nil {
		return ConfigReconcilerStatus{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return ConfigReconcilerStatus{
		ActiveRevision:  r.activeRevision,
		DesiredRevision: r.desiredRevision,
		RestartRequired: len(r.restartPaths) > 0,
		RestartPaths:    append([]string(nil), r.restartPaths...),
	}
}

type configRestartUnavailableError struct{}

func (configRestartUnavailableError) Error() string { return "model restart is unavailable" }
