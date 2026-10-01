package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
)

// resourceModelStatus is intentionally a read-only projection. Lifecycle
// operations still go through the local router so the scheduler remains the
// single owner of process state.
type resourceModelStatus struct {
	ID        string                    `json:"id"`
	Loaded    bool                      `json:"loaded"`
	Inflight  int                       `json:"inflight"`
	Sleeping  bool                      `json:"sleeping"`
	State     string                    `json:"state,omitempty"`
	Footprint resourcePlanner.Footprint `json:"footprint"`
}

type resourceStatus struct {
	Enabled    bool                   `json:"enabled"`
	Budget     resourcePlanner.Budget `json:"budget"`
	UsageVRAM  int                    `json:"usageVRAMMiB"`
	UsageRAM   int                    `json:"usageRAMMiB"`
	Models     []resourceModelStatus  `json:"models"`
	Candidates []string               `json:"evictionCandidates"`
}

// resourceFootprint converts the per-model backend declaration into the
// planner-neutral shape. Keeping this conversion in one place makes the
// admission path and the control-plane projection agree exactly.
func resourceFootprint(value config.ResourceConfig) resourcePlanner.Footprint {
	vram, ram := value.VRAMMiB, value.RAMMiB
	if vram < 0 {
		vram = 0
	}
	if ram < 0 {
		ram = 0
	}
	return resourcePlanner.Footprint{
		VRAMMiB:          vram,
		RAMMiB:           ram,
		GPUs:             append([]string(nil), value.GPUAffinity...),
		Priority:         value.Priority,
		EvictionPriority: value.EvictionPriority,
	}
}

func isLoadedProcessState(state process.ProcessState) bool {
	return state == process.StateStarting || state == process.StateReady || state == process.StateSleeping || state == process.StateStopping
}

// reconcileResources takes a point-in-time view of process and HTTP inflight
// state. It is deliberately cheap and side-effect free; the planner is
// reconciled again after any eviction before a request is admitted.
func (s *Server) reconcileResources() []resourceModelStatus {
	planner := s.resourcePlanner()
	if planner == nil {
		return nil
	}
	running := map[string]process.ProcessState{}
	cfg := s.currentConfig()
	adapters := s.backendAdaptersSnapshot()
	if s.local != nil {
		running = s.local.RunningModels()
	}
	inflight := make(map[string]int)
	if s.inflight != nil {
		for _, request := range s.inflight.Current().Requests {
			model, found := cfg.RealModelName(request.Model)
			if found {
				inflight[model]++
			}
		}
	}
	models := make([]resourcePlanner.Model, 0, len(cfg.Models))
	status := make([]resourceModelStatus, 0, len(cfg.Models))
	for id, modelConfig := range cfg.Models {
		state, loaded := running[id]
		plannerInflight := inflight[id]
		// Starting/stopping processes are already in a lifecycle transition. They
		// may have no HTTP request in the tracker, but unloading them again would
		// race the process scheduler. Mark them protected in the planner while
		// preserving the real inflight count in the control-plane projection.
		if state == process.StateStarting || state == process.StateStopping {
			plannerInflight = 1
		}
		model := resourcePlanner.Model{
			ID:        id,
			Footprint: resourceFootprint(modelConfig.Backend.Resources),
			Inflight:  plannerInflight,
			Loaded:    loaded && isLoadedProcessState(state),
			Sleeping:  s.modelSleeping(id),
		}
		// A backend sleep mode can keep the process ready while its weights are
		// released. The adapter is authoritative when it can report that state;
		// a short timeout prevents a broken admin endpoint from stalling normal
		// request admission.
		if model.Loaded {
			if adapter := adapters[id]; !backend.IsNilAdapter(adapter) {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				if cacheState, err := adapter.CacheState(ctx); err == nil && cacheState.Sleeping {
					model.Sleeping = true
				}
				cancel()
			}
		}
		models = append(models, model)
		status = append(status, resourceModelStatus{ID: id, Loaded: model.Loaded, Inflight: model.Inflight, Sleeping: model.Sleeping, State: string(state), Footprint: model.Footprint})
	}
	planner.Reconcile(models)
	return status
}

// prepareResourceLoadContext enforces the configured global budget while
// preserving the request's cancellation boundary. QueueLoads turns an
// otherwise immediate 507 into a bounded admission wait: the planner is
// reconciled on every tick so an operator-initiated unload, an external sleep,
// or a completed inference can make the request eligible without allowing a
// stale snapshot to admit two oversized models concurrently.
func (s *Server) prepareResourceLoadContext(ctx context.Context, requested string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	planner := s.resourcePlanner()
	if planner == nil {
		return nil
	}
	// Admission is serialized so two simultaneous cold loads cannot both
	// observe the same free budget and then evict different candidates (or
	// neither) before either request reaches the router. This lock covers only
	// the budget decision and safe eviction; it is independent of the process
	// scheduler and never participates in model restart/drain state.
	cfg := s.currentConfig()
	queue := cfg.ResourceBudget.QueueLoads
	var waitTimer *time.Timer
	var waitTicker *time.Ticker
	if queue {
		wait := time.Duration(cfg.HealthCheckTimeout) * time.Second
		if wait <= 0 {
			wait = 30 * time.Second
		}
		waitTimer = time.NewTimer(wait)
		waitTicker = time.NewTicker(100 * time.Millisecond)
		defer waitTimer.Stop()
		defer waitTicker.Stop()
	}
	model, local := cfg.RealModelName(strings.TrimSpace(requested))
	if !local || s.local == nil || !s.local.Handles(model) {
		return nil
	}
	modelConfig, found := cfg.Models[model]
	if !found {
		return nil
	}
	footprint := resourceFootprint(modelConfig.Backend.Resources)
	if footprint.VRAMMiB == 0 && footprint.RAMMiB == 0 {
		return nil
	}
	for {
		s.resourceMu.Lock()
		err := s.tryPrepareResourceLoadWithPlanner(planner, model, footprint)
		s.resourceMu.Unlock()
		if err == nil {
			return nil
		}
		if !queue {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("resource budget wait canceled for model %q: %w", model, ctx.Err())
		case <-waitTimer.C:
			return fmt.Errorf("resource budget wait timed out for model %q: %w", model, err)
		case <-waitTicker.C:
			// Retry with a fresh process/backend snapshot. The error is only
			// returned after the bounded wait, so callers can distinguish a
			// protected-but-eventually-releasable model from a hard 507.
		}
	}
}

func (s *Server) tryPrepareResourceLoadWithPlanner(planner *resourcePlanner.Planner, model string, footprint resourcePlanner.Footprint) error {
	if planner == nil {
		return nil
	}
	cfg := s.currentConfig()
	s.reconcileResources()
	candidates, planErr := planner.PlanLoad(model, footprint)
	if planErr == nil && len(candidates) == 0 {
		if err := planner.Reserve(model, footprint); err != nil {
			return fmt.Errorf("reserve resources for model %q: %w", model, err)
		}
		return nil
	}
	if len(candidates) > 0 && cfg.ResourceBudget.AutoEvict {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.Inflight == 0 {
				ids = append(ids, candidate.ID)
			}
		}
		var evictionErrs []error
		if len(ids) > 0 {
			for _, id := range ids {
				if err := s.releaseResourceCandidate(id); err != nil {
					evictionErrs = append(evictionErrs, err)
				}
			}
			s.reconcileResources()
			candidates, planErr = planner.PlanLoad(model, footprint)
			if planErr == nil && len(candidates) == 0 {
				if err := planner.Reserve(model, footprint); err != nil {
					return fmt.Errorf("reserve resources for model %q: %w", model, err)
				}
				return nil
			}
			if len(evictionErrs) > 0 {
				// Keep the admission error explicit about the safe-eviction
				// postcondition that failed. This is useful to callers using
				// queueLoads (and to operators) because a generic 507 would not
				// distinguish a protected in-flight model from an unload failure.
				return fmt.Errorf("resource budget cannot admit model %q after eviction attempts: %w", model, errors.Join(evictionErrs...))
			}
		}
	}
	if planErr != nil {
		return fmt.Errorf("resource budget cannot admit model %q: %w", model, planErr)
	}
	return fmt.Errorf("resource budget cannot admit model %q while protected models are active", model)
}

func (s *Server) releaseResourceReservation(model string) {
	planner := s.resourcePlanner()
	if planner == nil {
		return
	}
	cfg := s.currentConfig()
	if canonical, ok := cfg.RealModelName(strings.TrimSpace(model)); ok {
		model = canonical
	}
	planner.Release(model)
}

// releaseResourceCandidate prefers the backend's safe sleep primitive when a
// model explicitly selected sleep lifecycle. If that primitive is unavailable
// or fails, a normal router unload is the conservative fallback; both paths
// are restricted to candidates already proven to have zero inflight requests.
func (s *Server) releaseResourceCandidate(model string) error {
	if s == nil {
		return fmt.Errorf("resource eviction: server is nil")
	}
	cfg := s.currentConfig()
	modelConfig, ok := cfg.Models[model]
	if !ok {
		return fmt.Errorf("resource eviction model %q is not configured", model)
	}
	var sleepErr error
	lifecycleMode := strings.TrimSpace(modelConfig.Backend.Lifecycle.Mode)
	sleepLifecycle := strings.EqualFold(lifecycleMode, "sleep") ||
		(strings.EqualFold(strings.TrimSpace(modelConfig.Backend.Type), "vllm") &&
			modelConfig.UnloadAfter > 0 && !strings.EqualFold(lifecycleMode, "process"))
	if sleepLifecycle {
		adapter := s.backendAdaptersSnapshot()[model]
		level := modelConfig.Backend.Lifecycle.SleepLevel
		if level <= 0 {
			level = 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		sleepErr = s.sleepBackendModel(ctx, model, level)
		cancel()
		if sleepErr == nil {
			if s.modelSleeping(model) {
				return nil
			}
			if backend.IsNilAdapter(adapter) {
				sleepErr = errors.New("backend sleep completed without a verifiable sleeping state")
			} else {
				// A successful control response only means that the backend
				// accepted the request. It does not prove that weights have
				// actually been released yet. Admission must not count a model
				// as evicted until the backend reports an authoritative sleeping
				// state; otherwise two cold loads can both pass the budget check
				// while the first backend is still holding VRAM.
				verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 2*time.Second)
				cacheState, verifyErr := adapter.CacheState(verifyCtx)
				verifyCancel()
				if verifyErr == nil && cacheState.Normalize().Sleeping {
					return nil
				}
				if verifyErr != nil {
					sleepErr = fmt.Errorf("backend sleep state verification failed: %w", verifyErr)
				} else {
					sleepErr = errors.New("backend sleep completed but backend still reports awake")
				}
			}
		}
	}
	// A planner can be used by embedders before the local router is attached.
	// Never turn a safe admission failure into a nil-router panic; the caller
	// will reconcile and either queue or return the explicit budget error. The
	// LocalRouter contract does not return an error, so verify its postcondition
	// from RunningModels instead of silently assuming an asynchronous unload
	// released the footprint.
	if s.local == nil {
		if sleepErr != nil {
			return fmt.Errorf("evict resource model %q: backend sleep failed and local router is unavailable: %w", model, sleepErr)
		}
		return fmt.Errorf("evict resource model %q: local router is unavailable", model)
	}
	s.local.Unload(0, model)
	if running := s.local.RunningModels(); running != nil {
		if state, stillRunning := running[model]; stillRunning && isLoadedProcessState(state) {
			if sleepErr != nil {
				return fmt.Errorf("evict resource model %q: backend sleep failed (%v) and unload left process in %s", model, sleepErr, state)
			}
			return fmt.Errorf("evict resource model %q: unload left process in %s", model, state)
		}
	}
	if sleepErr != nil {
		// A failed sleep is recoverable when the conservative process unload
		// fallback actually removed the model. Keep the failure in diagnostics
		// only when neither mechanism made progress; a successful fallback is a
		// valid eviction outcome and should not block admission.
		return nil
	}
	return nil
}

func (s *Server) handleAPIResources(w http.ResponseWriter, r *http.Request) {
	planner := s.resourcePlanner()
	budgetEnabled := false
	if planner != nil {
		budget := planner.Budget()
		budgetEnabled = budget.VRAMMiB > 0 || budget.RAMMiB > 0
	}
	if planner == nil || !budgetEnabled {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":false,"models":[]}`))
		return
	}
	models := s.reconcileResources()
	usageVRAM, usageRAM := planner.Usage()
	candidates := planner.EvictionCandidates()
	if allowed, restricted := allowedCanonicalModels(s.currentConfig(), identityFromContext(r.Context())); restricted {
		visible := make(map[string]struct{}, len(allowed))
		for _, id := range allowed {
			visible[id] = struct{}{}
		}
		filtered := models[:0]
		usageVRAM, usageRAM = 0, 0
		for _, model := range models {
			if _, ok := visible[model.ID]; !ok {
				continue
			}
			filtered = append(filtered, model)
			if model.Loaded && !model.Sleeping {
				usageVRAM = saturatingResourceAdd(usageVRAM, model.Footprint.VRAMMiB)
				usageRAM = saturatingResourceAdd(usageRAM, model.Footprint.RAMMiB)
			}
		}
		models = filtered
		candidateFiltered := candidates[:0]
		for _, candidate := range candidates {
			if _, ok := visible[candidate.ID]; ok {
				candidateFiltered = append(candidateFiltered, candidate)
			}
		}
		candidates = candidateFiltered
	}
	candidateIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidateIDs = append(candidateIDs, candidate.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resourceStatus{Enabled: true, Budget: planner.Budget(), UsageVRAM: usageVRAM, UsageRAM: usageRAM, Models: models, Candidates: candidateIDs})
}

func saturatingResourceAdd(total, value int) int {
	if total < 0 {
		total = 0
	}
	if value < 0 {
		value = 0
	}
	maxInt := int(^uint(0) >> 1)
	if total > maxInt-value {
		return maxInt
	}
	return total + value
}
