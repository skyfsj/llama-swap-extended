package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompare_ClassifiesOnlineRuntimeAndDaemonChanges(t *testing.T) {
	active := Config{
		StartPort: 8000,
		Models: map[string]ModelConfig{
			"a": {Cmd: "server-a", Name: "old", ConcurrencyLimit: 1},
		},
	}
	desired := active
	desired.StartPort = 9000
	desired.Models = map[string]ModelConfig{
		"a": {Cmd: "server-a", Name: "new", ConcurrencyLimit: 4},
		"b": {Cmd: "server-b"},
	}

	changes := Compare(active, desired)
	if len(changes.AddedModels) != 1 || changes.AddedModels[0] != "b" {
		t.Fatalf("added models=%v want [b]", changes.AddedModels)
	}
	if len(changes.RemovedModels) != 0 {
		t.Fatalf("removed models=%v want none", changes.RemovedModels)
	}
	if len(changes.RuntimeChangedModels) != 0 {
		t.Fatalf("metadata/concurrency/startPort changes incorrectly require restart: %v", changes.RuntimeChangedModels)
	}

	desired.Models["a"] = ModelConfig{Cmd: "server-a-v2", Name: "new", ConcurrencyLimit: 4}
	changes = Compare(active, desired)
	if len(changes.RuntimeChangedModels) != 1 || changes.RuntimeChangedModels[0] != "a" {
		t.Fatalf("runtime changes=%v want [a]", changes.RuntimeChangedModels)
	}
}

func TestCompare_IncludesBackendRuntimeFields(t *testing.T) {
	active := Config{Models: map[string]ModelConfig{
		"a": {Cmd: "server-a", Backend: BackendConfig{Runtime: "vllm"}},
	}}
	desired := active
	desired.Models = map[string]ModelConfig{
		"a": {Cmd: "server-a", Backend: BackendConfig{Runtime: "sglang"}},
	}

	changes := Compare(active, desired)
	if len(changes.RuntimeChangedModels) != 1 || changes.RuntimeChangedModels[0] != "a" {
		t.Fatalf("backend runtime changes=%v want [a]", changes.RuntimeChangedModels)
	}
}

func TestCompare_IncludesReferencedRuntimeDefinitionChanges(t *testing.T) {
	active := Config{
		Runtimes: map[string]RuntimeConfig{"vllm": {Kind: "vllm", Mode: "native"}},
		Models: map[string]ModelConfig{
			"a": {Cmd: "server-a", Backend: BackendConfig{Runtime: "vllm"}},
		},
	}
	desired := active
	desired.Runtimes = map[string]RuntimeConfig{"vllm": {Kind: "vllm", Mode: "container"}}
	desired.Models = map[string]ModelConfig{
		"a": {Cmd: "server-a", Backend: BackendConfig{Runtime: "vllm"}},
	}

	changes := Compare(active, desired)
	if len(changes.RuntimeChangedModels) != 1 || changes.RuntimeChangedModels[0] != "a" {
		t.Fatalf("referenced runtime changes=%v want [a]", changes.RuntimeChangedModels)
	}
}

func TestApplyDaemonRestartPolicyPreservesActiveDaemonFields(t *testing.T) {
	activeStore := &Store{Path: "/var/lib/llama-swap-old"}
	desiredStore := &Store{Path: "/var/lib/llama-swap-new"}
	active := Config{
		Store:          activeStore,
		RuntimeManager: RuntimeManagerConfig{Root: "/opt/runtimes-old"},
		LogToStdout:    LogToStdoutProxy,
		LogLevel:       "info",
	}
	desired := Config{
		Store:          desiredStore,
		RuntimeManager: RuntimeManagerConfig{Root: "/opt/runtimes-new"},
		LogToStdout:    LogToStdoutBoth,
		LogLevel:       "debug",
	}

	effective, paths := ApplyDaemonRestartPolicy(active, desired)
	if effective.Store == nil || effective.Store.Path != activeStore.Path {
		t.Fatalf("effective store=%+v want active path", effective.Store)
	}
	if effective.RuntimeManager.Root != active.RuntimeManager.Root || effective.LogToStdout != active.LogToStdout {
		t.Fatalf("effective daemon fields=%+v want active values", effective)
	}
	if effective.LogLevel != desired.LogLevel {
		t.Fatalf("online log level=%q want %q", effective.LogLevel, desired.LogLevel)
	}
	wantPaths := []string{"/store/path", "/runtimeManager/root", "/logToStdout"}
	if len(paths) != len(wantPaths) {
		t.Fatalf("restart paths=%v want %v", paths, wantPaths)
	}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Fatalf("restart paths=%v want %v", paths, wantPaths)
		}
	}
}

// TestCompare_IgnoresProxyOnlyBackendFields pins the boundary between backend
// fields that shape the process and those that only shape how llama-swap serves
// a request. Protocol, APIs and Discover are read per request and never reach the
// command line, so flagging a restart for them asked the operator to restart
// inference for a change that was already in effect — and a restart could not
// apply anything.
func TestCompare_IgnoresProxyOnlyBackendFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*BackendConfig)
	}{
		{"protocol", func(b *BackendConfig) { b.Protocol = AdapterProtocolResponsesToChat }},
		{"apis", func(b *BackendConfig) { b.APIs = []string{"chat", "embeddings"} }},
		{"discover", func(b *BackendConfig) { off := false; b.Discover = &off }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active := Config{Models: map[string]ModelConfig{
				"a": {Cmd: "server-a", Backend: BackendConfig{Runtime: "vllm"}},
			}}
			desired := active
			backend := BackendConfig{Runtime: "vllm"}
			tc.change(&backend)
			desired.Models = map[string]ModelConfig{"a": {Cmd: "server-a", Backend: backend}}

			if changes := Compare(active, desired); len(changes.RuntimeChangedModels) != 0 {
				t.Fatalf("%s change flagged a restart: %v", tc.name, changes.RuntimeChangedModels)
			}
		})
	}

	// Fields that do reach the process still have to be reported.
	active := Config{Models: map[string]ModelConfig{"a": {Backend: BackendConfig{Runtime: "vllm"}}}}
	for _, tc := range []struct {
		name   string
		change func(*BackendConfig)
	}{
		{"arguments", func(b *BackendConfig) { b.Arguments = []string{"--max-model-len", "8192"} }},
		{"runtime", func(b *BackendConfig) { b.Runtime = "sglang" }},
	} {
		t.Run(tc.name+" still restarts", func(t *testing.T) {
			desired := active
			backend := BackendConfig{Runtime: "vllm"}
			tc.change(&backend)
			desired.Models = map[string]ModelConfig{"a": {Backend: backend}}

			if changes := Compare(active, desired); len(changes.RuntimeChangedModels) != 1 {
				t.Fatalf("%s change was not flagged: %v", tc.name, changes.RuntimeChangedModels)
			}
		})
	}
}

// TestCompare_UnrelatedModelDoesNotShiftPortsIntoARestart covers the false
// "configuration changed, restart inference" prompt an operator got for every
// running model after adding one more. ${PORT} is handed out by position in
// sorted-ID order, so a new model whose ID sorts earlier moves every later
// port; the survivors' own configuration did not change and none of them should
// be asked to restart.
func TestCompare_UnrelatedModelDoesNotShiftPortsIntoARestart(t *testing.T) {
	// Both backend shapes are covered: the legacy command lifecycle, where the
	// port reaches the command line through cmd, and the managed backend, where
	// it reaches it through the argv rebuilt from the model's proxy. Each
	// snapshot goes through the real loader so the ports are the ones the
	// counter actually handed out — a hand-written ${PORT} placeholder would
	// compare equal on both sides and prove nothing.
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"legacy command", `
startPort: 5800
models:
  alpha:
    cmd: "server --port ${PORT} --model alpha"
  mike:
    cmd: "server --port ${PORT} --model mike"
`},
		{"managed backend", `
startPort: 5800
runtimes:
  vllm:
    kind: vllm
    mode: native
models:
  alpha:
    backend:
      type: vllm
      runtime: vllm
      args: [serve, /models/alpha]
  mike:
    backend:
      type: vllm
      runtime: vllm
      args: [serve, /models/mike]
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active, err := LoadConfigFromReader(strings.NewReader(tc.yaml))
			if err != nil {
				t.Fatalf("load active: %v", err)
			}
			// "aaa" sorts first, so it takes the port alpha had and every
			// later model moves up by one.
			added := tc.yaml + `  aaa:
    cmd: "server --port ${PORT} --model aaa"
`
			if tc.name == "managed backend" {
				added = tc.yaml + `  aaa:
    backend:
      type: vllm
      runtime: vllm
      args: [serve, /models/aaa]
`
			}
			desired, err := LoadConfigFromReader(strings.NewReader(added))
			if err != nil {
				t.Fatalf("load desired: %v", err)
			}
			// Sanity: the survivor really did move, or this test would pass
			// against the bug it is meant to catch.
			if desired.Models["alpha"].Proxy == active.Models["alpha"].Proxy {
				t.Fatalf("fixture did not shift alpha's port: %q", active.Models["alpha"].Proxy)
			}

			changes := Compare(active, desired)
			if len(changes.RuntimeChangedModels) != 0 {
				t.Fatalf("adding an unrelated model asked %v to restart", changes.RuntimeChangedModels)
			}
			if len(changes.AddedModels) != 1 || changes.AddedModels[0] != "aaa" {
				t.Fatalf("added models=%v want [aaa]", changes.AddedModels)
			}

			// Removing the model again must be equally quiet.
			if changes := Compare(desired, active); len(changes.RuntimeChangedModels) != 0 {
				t.Fatalf("removing an unrelated model asked %v to restart", changes.RuntimeChangedModels)
			}
		})
	}
}

// TestCompare_ReportsEditsThatSurviveAPortShift is the other half of the
// contract: a shift is only assumed while it is uniform, so an operator's own
// edit still reaches the model it belongs to.
func TestCompare_ReportsEditsThatSurviveAPortShift(t *testing.T) {
	active := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"mike":  {Cmd: "server --port ${PORT} --model mike"},
			"alpha": {Cmd: "server --port ${PORT} --model alpha"},
		},
	}
	// "aaa" shifts both survivors by one, and alpha also gains a flag of its
	// own. The flag must still be reported even though alpha's port moved.
	desired := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"aaa":   {Cmd: "server --port ${PORT} --model aaa"},
			"mike":  {Cmd: "server --port ${PORT} --model mike"},
			"alpha": {Cmd: "server --port ${PORT} --model alpha --ctx 8192"},
		},
	}

	changes := Compare(active, desired)
	if len(changes.RuntimeChangedModels) != 1 || changes.RuntimeChangedModels[0] != "alpha" {
		t.Fatalf("runtime changes=%v want [alpha]", changes.RuntimeChangedModels)
	}
}

// TestCompare_StartPortEditStillRestartsModels pins that moving the counter's
// start is a real change. Every port moves, but the topology does not, so the
// move cannot be attributed to an insert or a remove and the models are
// restarted onto the ports the operator asked for.
func TestCompare_StartPortEditStillRestartsModels(t *testing.T) {
	// Both snapshots are shaped the way the loader leaves them, with ${PORT}
	// already expanded, so the comparison sees the resolved commands rather
	// than the placeholder text.
	active := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port 5800 --model alpha", Proxy: "http://localhost:5800"},
			"mike":  {Cmd: "server --port 5801 --model mike", Proxy: "http://localhost:5801"},
		},
	}
	desired := Config{
		StartPort: 5900,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port 5900 --model alpha", Proxy: "http://localhost:5900"},
			"mike":  {Cmd: "server --port 5901 --model mike", Proxy: "http://localhost:5901"},
		},
	}

	changes := Compare(active, desired)
	if len(changes.RuntimeChangedModels) != 2 {
		t.Fatalf("runtime changes=%v want both models", changes.RuntimeChangedModels)
	}
}

// TestCompare_PinnedPortEditStillRestartsModel covers a model that names its
// own port: the number is the operator's, so editing it is always a change even
// when the rest of the topology moved at the same time.
func TestCompare_PinnedPortEditStillRestartsModel(t *testing.T) {
	active := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port 8000", Proxy: "http://127.0.0.1:8000"},
		},
	}
	desired := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port 8001", Proxy: "http://127.0.0.1:8001"},
		},
	}

	if changes := Compare(active, desired); len(changes.RuntimeChangedModels) != 1 {
		t.Fatalf("runtime changes=%v want [alpha]", changes.RuntimeChangedModels)
	}
}

// TestPortShift_ZeroWithoutATopologyChange pins the condition that separates a
// counter shift from any other reason ports moved. A startPort edit moves every
// port too, and must not be mistaken for one.
func TestPortShift_ZeroWithoutATopologyChange(t *testing.T) {
	active := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5800"},
			"mike":  {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5801"},
		},
	}
	movedStart := Config{
		StartPort: 5900,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5900"},
			"mike":  {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5901"},
		},
	}
	if shift := PortShift(active, movedStart); shift != 0 {
		t.Fatalf("startPort edit reported as a shift of %d", shift)
	}

	added := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"aaa":   {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5800"},
			"alpha": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5801"},
			"mike":  {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5802"},
		},
	}
	if shift := PortShift(active, added); shift != 1 {
		t.Fatalf("added model reported as a shift of %d, want 1", shift)
	}

	// Removing the model that sorts last moves nobody: alpha keeps the port it
	// had, so the shift is 0 and the survivors are compared as they stand.
	removed := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5800"},
		},
	}
	if shift := PortShift(active, removed); shift != 0 {
		t.Fatalf("removing the last model reported as a shift of %d, want 0", shift)
	}

	// Removing one that sorts first moves the survivor back by one.
	removedFirst := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"mike": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5800"},
		},
	}
	first := Config{
		StartPort: 5800,
		Models: map[string]ModelConfig{
			"alpha": {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5800"},
			"mike":  {Cmd: "server --port ${PORT}", Proxy: "http://localhost:5801"},
		},
	}
	if shift := PortShift(first, removedFirst); shift != -1 {
		t.Fatalf("removing the first model reported as a shift of %d, want -1", shift)
	}
}

// TestModelRuntimeSpecOfWithPorts_DoesNotRewriteTheModel pins that reverting
// the port for a comparison cannot reach back into the configuration the
// comparison was given. The spec shares its Arguments slice with the model, so
// rewriting in place would silently rewrite the operator's own argv.
func TestModelRuntimeSpecOfWithPorts_DoesNotRewriteTheModel(t *testing.T) {
	model, err := LoadConfigFromReader(strings.NewReader(`
startPort: 5800
runtimes:
  vllm:
    kind: vllm
    mode: native
models:
  alpha:
    backend:
      type: vllm
      runtime: vllm
      args: [serve, /models/alpha, --port, "5800"]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := append([]string(nil), model.Models["alpha"].Backend.Arguments...)

	_ = modelRuntimeSpecOfWithPorts(model.Models["alpha"])

	got := model.Models["alpha"].Backend.Arguments
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("model arguments were rewritten by the comparison: %v -> %v", want, got)
	}
}

// TestCompare_LifecycleEditsDoNotRequireRestart pins the operator-visible
// contract: editing the idle TTL, the unload timeout or the vLLM sleep
// policy never marks a model as needing a restart. These fields are pushed
// into the live process by the router (process.LifecycleConfigApplier);
// only the TTL crossing zero on a vLLM backend — which adds or removes the
// injected --enable-sleep-mode launch flag — is a runtime change.
func TestCompare_LifecycleEditsDoNotRequireRestart(t *testing.T) {
	base := func(model ModelConfig) Config {
		return Config{Models: map[string]ModelConfig{"a": model}}
	}
	for _, tc := range []struct {
		name    string
		active  ModelConfig
		desired ModelConfig
	}{
		{
			name:    "ttl value change both positive",
			active:  ModelConfig{Cmd: "server-a", UnloadAfter: 600},
			desired: ModelConfig{Cmd: "server-a", UnloadAfter: 900},
		},
		{
			name:    "ttl disabled to disabled",
			active:  ModelConfig{Cmd: "server-a", UnloadAfter: 0},
			desired: ModelConfig{Cmd: "server-a", UnloadAfter: 0},
		},
		{
			name:    "unload timeout",
			active:  ModelConfig{Cmd: "server-a", UnloadTimeout: 30},
			desired: ModelConfig{Cmd: "server-a", UnloadTimeout: 60},
		},
		{
			name:    "sleep level",
			active:  ModelConfig{Cmd: "server-a", Backend: BackendConfig{Type: "vllm", Lifecycle: LifecycleConfig{Mode: "sleep", SleepLevel: 1}}},
			desired: ModelConfig{Cmd: "server-a", Backend: BackendConfig{Type: "vllm", Lifecycle: LifecycleConfig{Mode: "sleep", SleepLevel: 2}}},
		},
		{
			name:    "lifecycle mode",
			active:  ModelConfig{Cmd: "server-a", Backend: BackendConfig{Type: "vllm", Lifecycle: LifecycleConfig{Mode: "process"}}},
			desired: ModelConfig{Cmd: "server-a", Backend: BackendConfig{Type: "vllm", Lifecycle: LifecycleConfig{Mode: "sleep"}}},
		},
		{
			// Non-vLLM backends never gain the injected flag, so even the
			// zero-crossing is hot-appliable: the TTL watcher reads the new
			// value from the hot lifecycle snapshot.
			name:    "llamacpp ttl crosses zero",
			active:  ModelConfig{Cmd: "llama-server", UnloadAfter: 0},
			desired: ModelConfig{Cmd: "llama-server", UnloadAfter: 600},
		},
		{
			// An explicit flag is preserved by EnsureVLLMSleepModeArgument,
			// so the argv does not move with the TTL.
			name:    "vllm ttl crosses zero with explicit flag",
			active:  ModelConfig{Cmd: "vllm", UnloadAfter: 0, Backend: BackendConfig{Type: "vllm", Arguments: []string{"--enable-sleep-mode=false"}}},
			desired: ModelConfig{Cmd: "vllm", UnloadAfter: 600, Backend: BackendConfig{Type: "vllm", Arguments: []string{"--enable-sleep-mode=false"}}},
		},
	} {
		if changes := Compare(base(tc.active), base(tc.desired)); len(changes.RuntimeChangedModels) != 0 {
			t.Errorf("%s change flagged a restart: %v", tc.name, changes.RuntimeChangedModels)
		}
	}
}

// TestCompare_VLLMTTLZeroCrossingRequiresRestart is the flip side: a vLLM
// backend's TTL crossing zero adds or removes --enable-sleep-mode from the
// launch command, which only a process recreation can apply.
func TestCompare_VLLMTTLZeroCrossingRequiresRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		active  ModelConfig
		desired ModelConfig
	}{
		{
			name:    "ttl enabled",
			active:  ModelConfig{Cmd: "vllm", UnloadAfter: 0, Backend: BackendConfig{Type: "vllm"}},
			desired: ModelConfig{Cmd: "vllm", UnloadAfter: 600, Backend: BackendConfig{Type: "vllm"}},
		},
		{
			name:    "ttl disabled",
			active:  ModelConfig{Cmd: "vllm", UnloadAfter: 600, Backend: BackendConfig{Type: "vllm"}},
			desired: ModelConfig{Cmd: "vllm", UnloadAfter: 0, Backend: BackendConfig{Type: "vllm"}},
		},
	} {
		active := Config{Models: map[string]ModelConfig{"a": tc.active}}
		desired := Config{Models: map[string]ModelConfig{"a": tc.desired}}
		changes := Compare(active, desired)
		if len(changes.RuntimeChangedModels) != 1 || changes.RuntimeChangedModels[0] != "a" {
			t.Errorf("%s: runtime changes=%v want [a]", tc.name, changes.RuntimeChangedModels)
		}
	}
}

// TestModelLifecycleChanged covers the router's hot-apply predicate.
func TestModelLifecycleChanged(t *testing.T) {
	if ModelLifecycleChanged(
		ModelConfig{UnloadAfter: 600, UnloadTimeout: 30, Backend: BackendConfig{Lifecycle: LifecycleConfig{Mode: "sleep", SleepLevel: 1}}},
		ModelConfig{UnloadAfter: 600, UnloadTimeout: 30, Backend: BackendConfig{Lifecycle: LifecycleConfig{Mode: "sleep", SleepLevel: 1}}},
	) {
		t.Fatal("identical lifecycle fields reported as changed")
	}
	if !ModelLifecycleChanged(
		ModelConfig{UnloadAfter: 600},
		ModelConfig{UnloadAfter: 900},
	) {
		t.Fatal("ttl change not reported")
	}
	if !ModelLifecycleChanged(
		ModelConfig{Backend: BackendConfig{Lifecycle: LifecycleConfig{SleepLevel: 1}}},
		ModelConfig{Backend: BackendConfig{Lifecycle: LifecycleConfig{SleepLevel: 2}}},
	) {
		t.Fatal("sleep level change not reported")
	}
	// A lifecycle change must not be conflated with a runtime change: the
	// restart-required comparison strips these same fields.
	if ModelRuntimeConfigChanged(
		ModelConfig{Cmd: "server-a", UnloadAfter: 600, UnloadTimeout: 30, Backend: BackendConfig{Lifecycle: LifecycleConfig{Mode: "sleep", SleepLevel: 1}}},
		ModelConfig{Cmd: "server-a", UnloadAfter: 900, UnloadTimeout: 60, Backend: BackendConfig{Lifecycle: LifecycleConfig{Mode: "process", SleepLevel: 2}}},
	) {
		t.Fatal("lifecycle-only edit was reported as a runtime change")
	}
}
