package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigManager_DeleteModelPrunesMatrix is the end-to-end version of the
// reported failure: the delete endpoint issues exactly one patch op, removing a
// model the matrix still names, and expects it to apply.
func TestConfigManager_DeleteModelPrunesMatrix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	document := `
models:
  Qwen/Qwen3.8-27B-FP8:
    cmd: echo ${PORT}
  HY-MT2:
    cmd: echo ${PORT}
routing:
  router:
    use: matrix
    settings:
      matrix:
        vars:
          big: Qwen/Qwen3.8-27B-FP8
        evict_costs:
          big: 5
          HY-MT2: 1
        sets:
          pair: "big & HY-MT2"
          solo: "big"
`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_ = snapshot

	// The single op the delete endpoint sends.
	patch, err := json.Marshal([]PatchOp{{Op: "remove", Path: "/models/Qwen~1Qwen3.8-27B-FP8"}})
	if err != nil {
		t.Fatal(err)
	}
	validation, err := manager.ValidatePatch(ctx, patch)
	if err != nil {
		t.Fatalf("ValidatePatch: %v", err)
	}
	if !validation.Valid {
		t.Fatalf("delete patch rejected: %+v", validation.Issues)
	}
	if len(validation.Pruned) == 0 {
		t.Error("the cascade did not report any adjusted location")
	}
	t.Logf("pruned: %v", validation.Pruned)

	applied, _, err := manager.ApplyPatch(ctx, patch, snapshot.ETag)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if len(applied.Config) == 0 {
		t.Fatal("ApplyPatch returned no config")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := LoadConfigFromReader(strings.NewReader(string(onDisk)))
	if err != nil {
		t.Fatalf("the applied configuration does not load: %v\n%s", err, onDisk)
	}
	if _, found := after.Models["Qwen/Qwen3.8-27B-FP8"]; found {
		t.Error("the removed model is still configured")
	}
	if _, found := after.Models["HY-MT2"]; !found {
		t.Error("the surviving model disappeared")
	}
	if strings.Contains(string(onDisk), "big") {
		t.Errorf("the written config still names the removed model through a var:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), "HY-MT2") {
		t.Errorf("the written config lost the surviving model:\n%s", onDisk)
	}
	// One set survives, so the matrix engine stays selected with the removed
	// model's leaf gone.
	if after.Routing.Router.Use != "matrix" {
		t.Errorf("router = %q, want the matrix engine kept for the surviving set", after.Routing.Router.Use)
	}
	if after.Matrix == nil || len(after.Matrix.Sets) != 1 || after.Matrix.Sets[0].Name != "pair" {
		t.Errorf("matrix sets = %+v, want only the surviving set", after.Matrix)
	}
	if after.Matrix != nil && len(after.Matrix.Sets) == 1 && after.Matrix.Sets[0].DSL != "HY-MT2" {
		t.Errorf("surviving set DSL = %q, want HY-MT2", after.Matrix.Sets[0].DSL)
	}
	if after.Matrix != nil && after.Matrix.Var != nil {
		if _, found := after.Matrix.Var["big"]; found {
			t.Error("the pruned var is still configured")
		}
	}
}
