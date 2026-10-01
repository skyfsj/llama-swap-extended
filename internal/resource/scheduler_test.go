package resource

import (
	"math"
	"testing"
)

func TestPlannerNeverEvictsInflightAndOrdersPriority(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100, RAMMiB: 100})
	p.Upsert(Model{ID: "busy", Loaded: true, Inflight: 1, Footprint: Footprint{VRAMMiB: 60, EvictionPriority: -10}})
	p.Upsert(Model{ID: "idle", Loaded: true, Footprint: Footprint{VRAMMiB: 30, EvictionPriority: 2}})
	if got := p.EvictionCandidates(); len(got) != 1 || got[0].ID != "idle" {
		t.Fatalf("candidates=%+v", got)
	}
	selected, err := p.RequireFit(Footprint{VRAMMiB: 20})
	if err != nil || len(selected) != 1 || selected[0].ID != "idle" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestPlannerPlanLoadDoesNotDoubleCountWarmTarget(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100, RAMMiB: 100})
	p.Upsert(Model{ID: "warm", Loaded: true, Footprint: Footprint{VRAMMiB: 90}})
	selected, err := p.PlanLoad("warm", Footprint{VRAMMiB: 90})
	if err != nil {
		t.Fatalf("warm target should already fit: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("warm target selected for eviction: %+v", selected)
	}
}

func TestPlannerReconcileAndSnapshotAreDeterministicCopies(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100, RAMMiB: 200})
	gpus := []string{"1", "0"}
	p.Reconcile([]Model{{ID: "b", Loaded: true, Footprint: Footprint{GPUs: gpus}}, {ID: "a"}})
	gpus[0] = "changed"
	snapshot := p.Snapshot()
	if len(snapshot) != 2 || snapshot[0].ID != "a" || snapshot[1].ID != "b" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot[1].Footprint.GPUs[0] != "1" {
		t.Fatalf("snapshot shares GPU slice: %+v", snapshot[1].Footprint.GPUs)
	}
}

func TestPlannerZeroBudgetDimensionIsUnlimited(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100})
	p.Upsert(Model{ID: "warm", Loaded: true, Footprint: Footprint{VRAMMiB: 40, RAMMiB: 4096}})
	if !p.CanFit(Footprint{VRAMMiB: 20, RAMMiB: 1 << 30}) {
		t.Fatal("zero RAM budget should be treated as unlimited")
	}
	selected, err := p.PlanLoad("new", Footprint{VRAMMiB: 80, RAMMiB: 1 << 30})
	if err != nil || len(selected) != 1 || selected[0].ID != "warm" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestPlannerNormalizesNegativeFootprints(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100, RAMMiB: 100})
	p.Upsert(Model{ID: "malformed", Loaded: true, Footprint: Footprint{VRAMMiB: -50, RAMMiB: -10}})
	if gotVRAM, gotRAM := p.Usage(); gotVRAM != 0 || gotRAM != 0 {
		t.Fatalf("negative footprint reduced usage: vram=%d ram=%d", gotVRAM, gotRAM)
	}
	if p.CanFit(Footprint{VRAMMiB: 100, RAMMiB: 100}) == false {
		t.Fatal("normalized negative footprint should not consume budget")
	}
}

func TestPlannerSaturatesUsageBeforeBudgetCheck(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: math.MaxInt - 1})
	p.Upsert(Model{ID: "max", Loaded: true, Footprint: Footprint{VRAMMiB: math.MaxInt}})
	p.Upsert(Model{ID: "extra", Loaded: true, Footprint: Footprint{VRAMMiB: 1}})
	if got, _ := p.Usage(); got != math.MaxInt {
		t.Fatalf("usage wrapped instead of saturating: got=%d", got)
	}
	if p.CanFit(Footprint{VRAMMiB: 1}) {
		t.Fatal("saturated usage must not fit a finite smaller budget")
	}
}

func TestPlannerReservationsCloseAdmissionGap(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100})
	if err := p.Reserve("first", Footprint{VRAMMiB: 80}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Usage(); got != 80 {
		t.Fatalf("reserved usage=%d, want 80", got)
	}
	if _, err := p.PlanLoad("second", Footprint{VRAMMiB: 80}); err == nil {
		t.Fatal("second cold load fit despite first reservation")
	}
	p.Release("first")
	if got, _ := p.Usage(); got != 0 {
		t.Fatalf("released usage=%d, want 0", got)
	}
	if _, err := p.PlanLoad("second", Footprint{VRAMMiB: 80}); err != nil {
		t.Fatalf("released reservation still blocks load: %v", err)
	}
}

func TestPlannerSetBudgetPreservesAccounting(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100})
	p.Upsert(Model{ID: "warm", Loaded: true, Footprint: Footprint{VRAMMiB: 80}})
	p.SetBudget(Budget{VRAMMiB: 70})

	if got := p.Budget(); got.VRAMMiB != 70 {
		t.Fatalf("budget=%+v want 70 MiB", got)
	}
	if got, _ := p.Usage(); got != 80 {
		t.Fatalf("usage=%d want 80 after budget update", got)
	}
	if _, err := p.PlanLoad("new", Footprint{VRAMMiB: 80}); err == nil {
		t.Fatal("updated budget should still reject an unsafe load")
	}
}

func TestPlannerReservationReferenceCountAndEvictionProtection(t *testing.T) {
	p := NewPlanner(Budget{VRAMMiB: 100})
	p.Upsert(Model{ID: "warm", Loaded: true, Footprint: Footprint{VRAMMiB: 80}})
	if err := p.Reserve("warm", Footprint{VRAMMiB: 80}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve("warm", Footprint{VRAMMiB: 80}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Usage(); got != 80 {
		t.Fatalf("warm reservation double-counted usage=%d", got)
	}
	if candidates := p.EvictionCandidates(); len(candidates) != 0 {
		t.Fatalf("reserved warm model became eviction candidate: %+v", candidates)
	}
	p.Release("warm")
	if candidates := p.EvictionCandidates(); len(candidates) != 0 {
		t.Fatalf("warm model was evictable while one reservation remained: %+v", candidates)
	}
	p.Release("warm")
	if candidates := p.EvictionCandidates(); len(candidates) != 1 || candidates[0].ID != "warm" {
		t.Fatalf("released warm model candidates=%+v", candidates)
	}
}

func TestPlannerZeroValueAndNilReceiverAreSafe(t *testing.T) {
	var zero Planner
	zero.Upsert(Model{ID: "m", Loaded: true, Footprint: Footprint{VRAMMiB: 10}})
	if got, _ := zero.Usage(); got != 10 {
		t.Fatalf("zero-value planner usage=%d, want 10", got)
	}
	if !zero.CanFit(Footprint{VRAMMiB: 1}) {
		t.Fatal("zero budget should remain unlimited for zero-value planner")
	}
	if snapshot := zero.Snapshot(); len(snapshot) != 1 || snapshot[0].ID != "m" {
		t.Fatalf("zero-value planner snapshot=%+v", snapshot)
	}
	var nilPlanner *Planner
	nilPlanner.Upsert(Model{ID: "ignored"})
	nilPlanner.Remove("ignored")
	if got := nilPlanner.Budget(); got != (Budget{}) {
		t.Fatalf("nil planner budget=%+v, want zero", got)
	}
	if vram, ram := nilPlanner.Usage(); vram != 0 || ram != 0 {
		t.Fatalf("nil planner usage=(%d,%d), want zero", vram, ram)
	}
	if nilPlanner.CanFit(Footprint{VRAMMiB: 1}) {
		t.Fatal("nil planner must not admit a request")
	}
	if got := nilPlanner.Snapshot(); got != nil {
		t.Fatalf("nil planner snapshot=%+v, want nil", got)
	}
	if got := nilPlanner.EvictionCandidates(); got != nil {
		t.Fatalf("nil planner candidates=%+v, want nil", got)
	}
	if _, err := nilPlanner.PlanLoad("m", Footprint{VRAMMiB: 1}); err == nil {
		t.Fatal("nil planner PlanLoad should fail")
	}
}
