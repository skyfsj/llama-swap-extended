package config

import (
	"reflect"
	"sort"
	"strings"
)

// ConfigChangeSet describes the parts of a loaded, effective configuration
// that need special treatment during an in-process reconciliation. The
// comparison deliberately operates on normalized Config values: derived
// defaults and macro expansion have already happened by the time this helper
// is called, so changing startPort cannot create a false model restart.
type ConfigChangeSet struct {
	AddedModels          []string
	RemovedModels        []string
	RuntimeChangedModels []string
	RestartPaths         []string
}

// Compare returns model and daemon changes that cannot be applied by simply
// publishing a new HTTP/config snapshot. Metadata, capabilities, filters,
// selectors, UI and concurrency limits are safe to apply while an existing
// process serves and are intentionally absent from modelRuntimeSpec.
func Compare(active, desired Config) ConfigChangeSet {
	changes := ConfigChangeSet{RestartPaths: daemonRestartPaths(active, desired)}
	for id, desiredModel := range desired.Models {
		activeModel, exists := active.Models[id]
		if !exists {
			changes.AddedModels = append(changes.AddedModels, id)
			continue
		}
		if ModelRuntimeConfigChanged(activeModel, desiredModel) || runtimeReferenceChanged(active, desired, id) {
			changes.RuntimeChangedModels = append(changes.RuntimeChangedModels, id)
		}
	}
	for id := range active.Models {
		if _, exists := desired.Models[id]; !exists {
			changes.RemovedModels = append(changes.RemovedModels, id)
		}
	}
	sort.Strings(changes.AddedModels)
	sort.Strings(changes.RemovedModels)
	sort.Strings(changes.RuntimeChangedModels)
	return changes
}

// ModelRuntimeConfigChanged reports whether a running process must be
// recreated to observe the new model configuration. It includes both the
// legacy command lifecycle and first-class Backend/Runtime parameters.
func ModelRuntimeConfigChanged(active, desired ModelConfig) bool {
	return !reflect.DeepEqual(modelRuntimeSpecOf(active), modelRuntimeSpecOf(desired))
}

// ModelRuntimeConfigChangedInConfig includes the named runtime definition in
// the comparison. A model can keep the same Backend.Runtime reference while
// the runtime's executable/container/build parameters change underneath it;
// that still requires recreating a running model process.
func ModelRuntimeConfigChangedInConfig(active, desired Config, modelID string) bool {
	activeModel, activeOK := active.Models[modelID]
	desiredModel, desiredOK := desired.Models[modelID]
	if !activeOK || !desiredOK {
		return false
	}
	return ModelRuntimeConfigChanged(activeModel, desiredModel) || runtimeReferenceChanged(active, desired, modelID)
}

func runtimeReferenceChanged(active, desired Config, modelID string) bool {
	activeModel, activeOK := active.Models[modelID]
	desiredModel, desiredOK := desired.Models[modelID]
	if !activeOK || !desiredOK {
		return false
	}
	activeRuntime := strings.TrimSpace(activeModel.Backend.Runtime)
	desiredRuntime := strings.TrimSpace(desiredModel.Backend.Runtime)
	if activeRuntime != desiredRuntime || desiredRuntime == "" {
		return false
	}
	return !reflect.DeepEqual(active.Runtimes[activeRuntime], desired.Runtimes[desiredRuntime])
}

type modelRuntimeSpec struct {
	Cmd                string
	CmdStop            string
	Proxy              string
	Env                []string
	CheckEndpoint      string
	UnloadAfter        int
	UnloadTimeout      int
	HealthCheckTimeout int
	Timeouts           TimeoutsConfig
	Compat             CompatConfig
	Backend            BackendConfig
}

func modelRuntimeSpecOf(model ModelConfig) modelRuntimeSpec {
	return modelRuntimeSpec{
		Cmd:                model.Cmd,
		CmdStop:            model.CmdStop,
		Proxy:              model.Proxy,
		Env:                model.Env,
		CheckEndpoint:      model.CheckEndpoint,
		UnloadAfter:        model.UnloadAfter,
		UnloadTimeout:      model.UnloadTimeout,
		HealthCheckTimeout: model.HealthCheckTimeout,
		Timeouts:           model.Timeouts,
		Compat:             model.Compat,
		Backend:            processBackendSpec(model.Backend),
	}
}

// processBackendSpec strips the backend fields that only shape how llama-swap
// serves a request. Protocol selects the request/response adapter, APIs gates
// which endpoints are exposed, and Discover only drives backend probing: all
// three are read per request, never reach the process command line, and are
// applied the moment the config reloads. Comparing them made an adapter change
// ask the operator to restart inference — a restart that cannot apply anything.
func processBackendSpec(backend BackendConfig) BackendConfig {
	backend.Protocol = ""
	backend.APIs = nil
	backend.Discover = nil
	return backend
}

func daemonRestartPaths(active, desired Config) []string {
	paths := make([]string, 0, 3)
	activeStore, desiredStore := "", ""
	if active.Store != nil {
		activeStore = active.Store.Path
	}
	if desired.Store != nil {
		desiredStore = desired.Store.Path
	}
	if activeStore != desiredStore {
		paths = append(paths, "/store/path")
	}
	if active.RuntimeManager.Root != desired.RuntimeManager.Root {
		paths = append(paths, "/runtimeManager/root")
	}
	if active.LogToStdout != desired.LogToStdout {
		paths = append(paths, "/logToStdout")
	}
	return paths
}

// ApplyDaemonRestartPolicy returns the configuration that may be published to
// the current daemon. Fields requiring a process restart remain at their
// active values; returned paths tell the control plane what applies after the
// daemon restarts. Model-level changes are not filtered.
func ApplyDaemonRestartPolicy(active, desired Config) (Config, []string) {
	paths := daemonRestartPaths(active, desired)
	if len(paths) == 0 {
		return desired, nil
	}
	result := desired
	if active.Store == nil {
		result.Store = nil
	} else {
		store := *active.Store
		result.Store = &store
	}
	result.RuntimeManager.Root = active.RuntimeManager.Root
	result.LogToStdout = active.LogToStdout
	return result, paths
}
