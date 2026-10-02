package server

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// This file implements the thermal guard: when a GPU card crosses the
// configured stop temperature, every model occupying that card is stopped and
// parked in maintenance mode; when the card cools below the resume point the
// models leave maintenance again. Requests for parked models are forwarded to
// configured providers/peers by the dispatch failover in api.go.
//
// Card-to-model association comes from the same sources the energy
// attribution uses (launch.gpus / container.gpus resolved against the
// hardware snapshot), plus the routing.gpus card declarations whose keys
// match a card's UUID or index.

// thermalGuardReason is the maintenance reason prefix recorded on models
// parked by the guard. The dispatch failover matches on this prefix so only
// thermally parked models are forwarded, not models parked for other reasons.
const thermalGuardReason = "thermal guard"

// thermalCardCooldown keeps a card in its tripped state until it cools below
// the resume point; while tripped, samples do not extend the consecutive
// counter and models are not re-stopped.
type thermalCardState struct {
	// consecutive counts adjacent hot samples for the current episode.
	consecutive int
	// tripped marks a card whose models are currently parked.
	tripped bool
}

// thermalMonitor watches GPU temperatures and parks/repairs models.
type thermalMonitor struct {
	mu      sync.Mutex
	stop    chan struct{}
	stopped chan struct{}

	guard     *config.ThermalGuardConfig
	cardState map[string]*thermalCardState
}

func newThermalMonitor(guard *config.ThermalGuardConfig) *thermalMonitor {
	return &thermalMonitor{
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
		guard:     guard,
		cardState: make(map[string]*thermalCardState),
	}
}

// run subscribes to GPU samples and applies the guard. It exits when Stop is
// called or the perf monitor closes its channel (config reload restarts it).
func (t *thermalMonitor) run(s *Server) {
	defer close(t.stopped)

	_, gpuCh, unsub := s.perf.Subscribe()
	defer unsub()

	for {
		select {
		case <-t.stop:
			return
		case stats, ok := <-gpuCh:
			if !ok {
				return
			}
			t.applySamples(s, stats)
		}
	}
}

// Stop asks the monitor goroutine to exit and waits for it.
func (t *thermalMonitor) Stop() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
	}
	<-t.stopped
}

// applySamples folds one batch of GPU samples into per-card trip logic.
func (t *thermalMonitor) applySamples(s *Server, stats []perf.GpuStat) {
	for _, stat := range stats {
		key := gpuStatKey(stat.ID, stat.UUID)
		t.applySample(s, key, stat.TempC)
	}
}

func (t *thermalMonitor) applySample(s *Server, cardKey string, tempC int) {
	t.mu.Lock()
	state := t.cardState[cardKey]
	if state == nil {
		state = &thermalCardState{}
		t.cardState[cardKey] = state
	}
	if state.tripped {
		// Stay tripped until the resume point; ignore samples in between.
		t.mu.Unlock()
		if float64(tempC) <= t.guard.ResumeC {
			t.resume(s, cardKey)
		}
		return
	}
	if float64(tempC) >= t.guard.StopC {
		state.consecutive++
	} else {
		state.consecutive = 0
	}
	trip := state.consecutive >= t.guard.Consecutive
	if trip {
		state.tripped = true
		state.consecutive = 0
	}
	t.mu.Unlock()

	if trip {
		t.trip(s, cardKey, tempC)
	}
}

// trip stops every model on the card and parks it in maintenance.
func (t *thermalMonitor) trip(s *Server, cardKey string, tempC int) {
	models := modelsOnCard(s, cardKey)
	if len(models) == 0 {
		if s.proxylog != nil {
			s.proxylog.Warnf("thermal guard: GPU %s reached %d°C but no configured model maps to it", cardKey, tempC)
		}
		return
	}
	reason := fmt.Sprintf("%s: GPU %s reached %d°C (stop at %v°C)", thermalGuardReason, cardKey, tempC, t.guard.StopC)
	for _, modelID := range models {
		if s.proxylog != nil {
			s.proxylog.Warnf("thermal guard: stopping model %q (%s)", modelID, reason)
		}
		s.local.EnterMaintenance(modelID, reason)
		// Unload blocks until the process stopped; a share of models on the
		// same card stop in parallel per the router's timeout groups.
		s.local.Unload(0, modelID)
	}
	// The statuses changed (models unloaded + parked); wake the SSE feed.
	event.Emit(swaputil.ModelStatusesChangedEvent{})
}

// resume releases the card's parked models from maintenance.
func (t *thermalMonitor) resume(s *Server, cardKey string) {
	t.mu.Lock()
	if state := t.cardState[cardKey]; state != nil {
		state.tripped = false
		state.consecutive = 0
	}
	t.mu.Unlock()

	models := modelsOnCard(s, cardKey)
	for _, modelID := range models {
		if reason, ok := s.local.InMaintenance(modelID); ok && strings.HasPrefix(reason, thermalGuardReason) {
			if s.proxylog != nil {
				s.proxylog.Infof("thermal guard: GPU %s cooled below %v°C, releasing model %q from maintenance", cardKey, t.guard.ResumeC, modelID)
			}
			s.local.ExitMaintenance(modelID)
		}
	}
	if len(models) > 0 {
		event.Emit(swaputil.ModelStatusesChangedEvent{})
	}
}

// modelsOnCard resolves the configured models occupying one card. It unions
// the launch/container GPU mapping (UUID keys) with routing.gpus declarations
// whose card key matches the card's UUID or bare index.
func modelsOnCard(s *Server, cardKey string) []string {
	cfg := s.currentConfig()
	affected := make(map[string]struct{})

	// 1. launch.gpus / container.gpus resolved to stat keys (UUID or index-N).
	for modelID := range cfg.Models {
		for _, key := range resolveModelGPUs(cfg, s.hardware, modelID) {
			if sameGPUKey(key, cardKey) {
				affected[modelID] = struct{}{}
			}
		}
	}

	// 2. routing.gpus card declarations. Card keys are operator-chosen
	// strings; match them against the stat key and the bare index.
	if gpus := cfg.Routing.Router.Settings.Gpus; gpus != nil {
		index := bareIndexFromGPUKey(cardKey)
		for card, models := range gpus.Cards {
			if card != cardKey && (index == "" || card != index) {
				continue
			}
			for _, modelID := range models {
				if _, known := cfg.Models[modelID]; known {
					affected[modelID] = struct{}{}
				}
			}
		}
	}

	out := make([]string, 0, len(affected))
	for modelID := range affected {
		out = append(out, modelID)
	}
	return out
}

// sameGPUKey compares two stat keys, treating a UUID and its index form as
// the same card when both resolve on this host.
func sameGPUKey(a, b string) bool {
	if a == b {
		return true
	}
	indexA, uuidA := splitGPUKey(a)
	indexB, uuidB := splitGPUKey(b)
	if uuidA != "" && uuidB != "" {
		return uuidA == uuidB
	}
	if indexA != "" && indexB != "" {
		return indexA == indexB
	}
	return false
}

// splitGPUKey breaks a stat key into its index and UUID parts. "index-N" is
// the synthetic form produced when a backend reports no UUID; "index:N" is
// the configured launch.gpus selection form, and a bare integer is the
// shorthand selection, all matching the same card.
func splitGPUKey(key string) (index, uuid string) {
	if rest, ok := strings.CutPrefix(key, "index-"); ok {
		if _, err := strconv.Atoi(rest); err == nil {
			return rest, ""
		}
	}
	if rest, ok := strings.CutPrefix(key, "index:"); ok {
		if _, err := strconv.Atoi(rest); err == nil {
			return rest, ""
		}
	}
	if _, err := strconv.Atoi(key); err == nil {
		return key, ""
	}
	return "", key
}

// bareIndexFromGPUKey returns the numeric index of a stat key, or "" when the
// key is a UUID.
func bareIndexFromGPUKey(key string) string {
	index, _ := splitGPUKey(key)
	return index
}

// maintenanceParked reports whether a model is parked on the router's runtime
// maintenance map (as opposed to the scheduler's lifecycle flag).
func (s *Server) maintenanceParked(modelID string) bool {
	if s.local == nil {
		return false
	}
	_, ok := s.local.InMaintenance(modelID)
	return ok
}
