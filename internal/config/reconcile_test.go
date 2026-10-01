package config

import "testing"

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
