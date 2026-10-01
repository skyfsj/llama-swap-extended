package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

// TestRuntime_PruneKeepsProbeProtectedVersion covers the deletion-protection
// probe: a version the server side still references (running model, config
// pin, live update) is kept on disk by the prune pass even though the
// manager's own current/previous/pinned pointers do not protect it, while an
// unprotected extra version is removed.
func TestRuntime_PruneKeepsProbeProtectedVersion(t *testing.T) {
	manager, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"vllm": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetIdleProbe(func() bool { return true })
	manager.SetVersionProtectionProbe(func(name, version string) (protected bool, reason string) {
		if version == "1.1.0" {
			return true, "bound to a running model"
		}
		return false, ""
	})
	for _, version := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		if _, err := manager.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: version, Source: "fixture"}); err != nil {
			t.Fatalf("stage %s: %v", version, err)
		}
	}
	if err := manager.Activate(context.Background(), "vllm", "1.2.0"); err != nil {
		t.Fatalf("activate 1.2.0: %v", err)
	}
	// keep=1: only the newest version is retained by the count, current is
	// protected by the pointer, 1.1.0 only by the probe.
	if err := manager.prune("vllm", 1); err != nil {
		t.Fatalf("prune: %v", err)
	}
	detail, ok, err := manager.Detail("vllm")
	if err != nil || !ok {
		t.Fatalf("detail after prune: ok=%v err=%v", ok, err)
	}
	kept := map[string]bool{}
	for version := range detail.Versions {
		kept[version] = true
	}
	if !kept["1.2.0"] {
		t.Fatalf("versions after prune = %v, want current 1.2.0 kept", kept)
	}
	if !kept["1.1.0"] {
		t.Fatalf("versions after prune = %v, want probe-protected 1.1.0 kept", kept)
	}
	if kept["1.0.0"] {
		t.Fatalf("versions after prune = %v, want unprotected 1.0.0 removed", kept)
	}
}
