// Package resource contains the deterministic resource-budget decisions used
// by backend lifecycle managers. It never kills an in-flight model: callers
// must perform the returned sleep/unload operation and then retry reservation.
package resource

import (
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
)

type Footprint struct {
	VRAMMiB          int      `json:"vramMiB"`
	RAMMiB           int      `json:"ramMiB"`
	GPUs             []string `json:"gpus,omitempty"`
	Priority         int      `json:"priority"`
	EvictionPriority int      `json:"evictionPriority"`
}

type Budget struct {
	VRAMMiB int `json:"vramMiB"`
	RAMMiB  int `json:"ramMiB"`
}

type Model struct {
	ID        string
	Footprint Footprint
	Inflight  int
	Sleeping  bool
	Loaded    bool
}

type Planner struct {
	mu           sync.Mutex
	budget       Budget
	models       map[string]Model
	reservations map[string]reservation
}

// reservation represents an admission that has passed the budget check but
// has not necessarily reached the process scheduler yet.  Keeping a reference
// count lets concurrent requests for the same cold model share one footprint
// while ensuring that the reservation is not released when only one request
// finishes.
type reservation struct {
	footprint Footprint
	count     int
}

func NewPlanner(budget Budget) *Planner {
	return &Planner{budget: budget, models: make(map[string]Model), reservations: make(map[string]reservation)}
}

func (p *Planner) Upsert(model Model) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if model.ID == "" {
		return
	}
	if p.models == nil {
		p.models = make(map[string]Model)
	}
	model.Footprint = normalizeFootprint(model.Footprint)
	p.models[model.ID] = model
}

func (p *Planner) Remove(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.models, id)
}

// Budget returns the immutable budget configured for this planner.
func (p *Planner) Budget() Budget {
	if p == nil {
		return Budget{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.budget
}

// SetBudget publishes a new global budget without replacing the planner. The
// model accounting and in-flight reservations therefore survive an online
// configuration update; the next admission decision observes the new limit.
func (p *Planner) SetBudget(budget Budget) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.budget = budget
	p.mu.Unlock()
}

// Reconcile replaces the planner's snapshot atomically. Reconciliation is
// intentionally separate from loading/unloading side effects: callers can
// derive a plan from a current process/inflight snapshot, perform only safe
// evictions, then reconcile again before admitting the request.
func (p *Planner) Reconcile(models []Model) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.models == nil {
		p.models = make(map[string]Model, len(models))
	} else {
		for id := range p.models {
			delete(p.models, id)
		}
	}
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		model.Footprint = normalizeFootprint(model.Footprint)
		p.models[model.ID] = model
	}
}

// Reserve atomically records one request's resource admission.  Reservations
// count toward the configured budget until Release is called and are excluded
// from eviction candidates, closing the gap between a successful admission
// check and the process scheduler actually marking a model as loaded.
func (p *Planner) Reserve(id string, footprint Footprint) error {
	if p == nil {
		return errors.New("resource planner is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("resource reservation model id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reservations == nil {
		p.reservations = make(map[string]reservation)
	}
	footprint = normalizeFootprint(footprint)
	current := p.reservations[id]
	if current.count == 0 {
		current.footprint = footprint
	}
	current.count++
	p.reservations[id] = current
	return nil
}

// Release drops one request's admission reservation. It is intentionally
// idempotent for unknown ids so deferred cleanup remains safe when admission
// returned before a reservation was acquired.
func (p *Planner) Release(id string) {
	if p == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	reservation, ok := p.reservations[id]
	if !ok || reservation.count <= 1 {
		delete(p.reservations, id)
		return
	}
	reservation.count--
	p.reservations[id] = reservation
}

// Snapshot returns a deterministic copy of the current model accounting
// state. It is used by control-plane APIs and diagnostics, not as an
// authorization decision.
func (p *Planner) Snapshot() []Model {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Model, 0, len(p.models))
	for _, model := range p.models {
		model.Footprint.GPUs = append([]string(nil), model.Footprint.GPUs...)
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Planner) Usage() (vram, ram int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usageLocked()
}

func (p *Planner) usageLocked() (vram, ram int) {
	if p == nil {
		return 0, 0
	}
	active := make(map[string]bool, len(p.models))
	for _, model := range p.models {
		if model.Loaded && !model.Sleeping {
			active[model.ID] = true
			vram = saturatingAdd(vram, model.Footprint.VRAMMiB)
			ram = saturatingAdd(ram, model.Footprint.RAMMiB)
		}
	}
	for id, reservation := range p.reservations {
		if reservation.count <= 0 || active[id] {
			continue
		}
		vram = saturatingAdd(vram, reservation.footprint.VRAMMiB)
		ram = saturatingAdd(ram, reservation.footprint.RAMMiB)
	}
	return
}

func (p *Planner) CanFit(footprint Footprint) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	footprint = normalizeFootprint(footprint)
	vram, ram := p.usageLocked()
	return dimensionFits(saturatingAdd(vram, footprint.VRAMMiB), p.budget.VRAMMiB) && dimensionFits(saturatingAdd(ram, footprint.RAMMiB), p.budget.RAMMiB)
}

// EvictionCandidates returns the lowest-priority safe models first. Models
// with inflight requests are excluded even when they have a lower priority.
func (p *Planner) EvictionCandidates() []Model {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evictionCandidatesLocked()
}

func (p *Planner) evictionCandidatesLocked() []Model {
	if p == nil {
		return nil
	}
	out := make([]Model, 0)
	for _, model := range p.models {
		if model.Loaded && !model.Sleeping && model.Inflight == 0 && p.reservations[model.ID].count == 0 {
			out = append(out, model)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Footprint.EvictionPriority != out[j].Footprint.EvictionPriority {
			return out[i].Footprint.EvictionPriority < out[j].Footprint.EvictionPriority
		}
		if out[i].Footprint.Priority != out[j].Footprint.Priority {
			return out[i].Footprint.Priority < out[j].Footprint.Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (p *Planner) RequireFit(footprint Footprint) ([]Model, error) {
	return p.planLoad("", footprint)
}

// PlanLoad returns the safe eviction set required to load id. If id is
// already accounted as loaded, its existing footprint is replaced instead of
// being double-counted; this makes admission checks idempotent for warm
// models. The returned models are only candidates. Callers must unload/sleep
// them and reconcile before admitting the request.
func (p *Planner) PlanLoad(id string, footprint Footprint) ([]Model, error) {
	return p.planLoad(id, footprint)
}

func (p *Planner) planLoad(id string, footprint Footprint) ([]Model, error) {
	if p == nil {
		return nil, errors.New("resource planner is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	footprint = normalizeFootprint(footprint)
	vram, ram := p.usageExcludingLocked(id)
	if dimensionFits(saturatingAdd(vram, footprint.VRAMMiB), p.budget.VRAMMiB) && dimensionFits(saturatingAdd(ram, footprint.RAMMiB), p.budget.RAMMiB) {
		return nil, nil
	}
	neededVRAM, neededRAM := 0, 0
	if p.budget.VRAMMiB > 0 {
		if excess := saturatingAdd(vram, footprint.VRAMMiB) - p.budget.VRAMMiB; excess > 0 {
			neededVRAM = excess
		}
	}
	if p.budget.RAMMiB > 0 {
		if excess := saturatingAdd(ram, footprint.RAMMiB) - p.budget.RAMMiB; excess > 0 {
			neededRAM = excess
		}
	}
	var selected []Model
	for _, model := range p.evictionCandidatesLocked() {
		if id != "" && model.ID == id {
			continue
		}
		selected = append(selected, model)
		neededVRAM -= model.Footprint.VRAMMiB
		neededRAM -= model.Footprint.RAMMiB
		if neededVRAM <= 0 && neededRAM <= 0 {
			return selected, nil
		}
	}
	return selected, errors.New("resource budget cannot be satisfied without evicting an in-flight or protected model")
}

// dimensionFits treats an unset dimension (zero budget) as unlimited. The
// global budget is opt-in per dimension: configuring only VRAM must not make
// every non-zero RAM footprint impossible to admit, and vice versa.
func dimensionFits(usage, budget int) bool {
	return budget <= 0 || usage <= budget
}

func (p *Planner) usageExcludingLocked(id string) (vram, ram int) {
	if p == nil {
		return 0, 0
	}
	active := make(map[string]bool, len(p.models))
	for modelID, model := range p.models {
		if modelID == id || !model.Loaded || model.Sleeping {
			continue
		}
		active[modelID] = true
		vram = saturatingAdd(vram, model.Footprint.VRAMMiB)
		ram = saturatingAdd(ram, model.Footprint.RAMMiB)
	}
	for modelID, reservation := range p.reservations {
		if modelID == id || reservation.count <= 0 || active[modelID] {
			continue
		}
		vram = saturatingAdd(vram, reservation.footprint.VRAMMiB)
		ram = saturatingAdd(ram, reservation.footprint.RAMMiB)
	}
	return
}

// normalizeFootprint keeps the planner's arithmetic monotonic even when a
// caller constructs a Model directly instead of going through config
// validation. Negative memory would otherwise reduce the observed usage and
// let an oversized request bypass the budget; a nil/empty GPU list is kept as
// such while non-empty lists are copied to preserve snapshot ownership.
func normalizeFootprint(footprint Footprint) Footprint {
	if footprint.VRAMMiB < 0 {
		footprint.VRAMMiB = 0
	}
	if footprint.RAMMiB < 0 {
		footprint.RAMMiB = 0
	}
	footprint.GPUs = append([]string(nil), footprint.GPUs...)
	return footprint
}

// saturatingAdd prevents a malformed or simply very large collection of
// models from wrapping memory usage back into a small positive value. A
// saturated usage will fail any finite positive budget and remains safe to
// subtract from when calculating an eviction deficit.
func saturatingAdd(a, b int) int {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}
