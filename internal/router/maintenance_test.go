package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// boolPtr is a small helper for the pointer-shaped config flags.
func boolPtr(value bool) *bool { return &value }

// TestBaseRouter_MaintenanceRefusesIncomingRequests covers the maintenance
// contract: a model that failed to start because auto-rollback is disabled
// keeps the new configuration and refuses to be started by incoming requests,
// so a client never silently re-attempts a configuration known to fail.
func TestBaseRouter_MaintenanceRefusesIncomingRequests(t *testing.T) {
	ready := newFakeProcess("broken")
	ready.markReady()
	b := newTestBase(t, map[string]process.Process{"broken": ready}, &stubPlanner{})
	conf := config.Config{HealthCheckTimeout: 5, Models: map[string]config.ModelConfig{"broken": {}}}
	b.Reconfigure(conf, &stubPlanner{}, map[string]struct{}{"broken": {}})

	b.EnterMaintenance("broken", "starting with the new configuration failed: exit 3")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"broken"}`))
	r.Header.Set("Content-Type", "application/json")
	b.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the request is not queued behind a start that will fail", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "maintenance") || !strings.Contains(body, "exit 3") {
		t.Errorf("body = %q, want the maintenance reason carried to the client", body)
	}

	if _, ok := b.InMaintenance("broken"); !ok {
		t.Error("model should still be in maintenance")
	}
}

// TestBaseRouter_MaintenanceExitsAfterServedRequest covers the exit condition:
// maintenance ends on its own once a start has succeeded and a request has
// actually been served, which is the state the operator opted into.
func TestBaseRouter_MaintenanceExitsAfterServedRequest(t *testing.T) {
	ready := newFakeProcess("model")
	ready.markReady()
	b := newTestBase(t, map[string]process.Process{"model": ready}, &stubPlanner{})
	b.Reconfigure(config.Config{
		HealthCheckTimeout: 5,
		Models:             map[string]config.ModelConfig{"model": {}},
	}, &stubPlanner{}, map[string]struct{}{"model": {}})

	b.EnterMaintenance("model", "start failed")
	if _, ok := b.InMaintenance("model"); !ok {
		t.Fatal("model should be in maintenance")
	}

	// The serving path is the only place that can observe both a successful
	// start and a served request, so it is what clears the state.
	b.trackedServe("model", ready, 0)(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if _, ok := b.InMaintenance("model"); ok {
		t.Error("maintenance should be cleared once a request has been served")
	}
}

// TestBaseRouter_MaintenanceSnapshotIsACopy guards the status publisher: a
// caller that mutates the returned map must not be able to clear the router's
// own state.
func TestBaseRouter_MaintenanceSnapshotIsACopy(t *testing.T) {
	b := newTestBase(t, nil, &stubPlanner{})
	b.EnterMaintenance("a", "first")

	snapshot := b.MaintenanceSnapshot()
	if got := snapshot["a"]; got != "first" {
		t.Fatalf("snapshot[a] = %q, want first", got)
	}
	delete(snapshot, "a")
	if _, ok := b.InMaintenance("a"); !ok {
		t.Error("deleting from the snapshot cleared the router state")
	}
}

// TestBaseRouter_RollbackPolicyDefaultsToEnabled pins the default: a config
// assembled without the flag (or with it absent) keeps the historical
// behaviour of restoring the last configuration that started.
func TestBaseRouter_RollbackPolicyDefaultsToEnabled(t *testing.T) {
	cases := []struct {
		name string
		flag *bool
		want bool
	}{
		{name: "nil flag rolls back", flag: nil, want: true},
		{name: "explicit true rolls back", flag: boolPtr(true), want: true},
		{name: "explicit false opts into maintenance", flag: boolPtr(false), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBaseWithConfig(t, config.Config{
				HealthCheckTimeout:          5,
				RollbackOnModelStartFailure: tc.flag,
			}, nil, &stubPlanner{})
			if got := b.rollbackOnModelStartFailure(config.ModelConfig{}); got != tc.want {
				t.Errorf("rollbackOnModelStartFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBaseRouter_MaintenanceWorksWithoutLogger guards a bare router: the
// maintenance transition must not depend on a logger being present.
func TestBaseRouter_MaintenanceWorksWithoutLogger(t *testing.T) {
	b, err := newBaseRouter("test", config.Config{HealthCheckTimeout: 5}, nil, logmon.NewWriter(io.Discard), &stubPlanner{})
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	b.EnterMaintenance("x", "reason")
	if _, ok := b.InMaintenance("x"); !ok {
		t.Error("maintenance should be recorded without a logger")
	}
	b.ExitMaintenance("x")
	if _, ok := b.InMaintenance("x"); ok {
		t.Error("maintenance should be cleared")
	}
}
