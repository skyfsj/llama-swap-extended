package config

import (
	"net/url"
	"reflect"
	"sort"
	"strconv"
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
// process serves and are intentionally absent from modelRuntimeSpec; the
// lifecycle fields (ttl, unloadTimeout, backend.lifecycle) joined them there
// and are pushed into the live process by the router instead — see
// ModelLifecycleChanged.
func Compare(active, desired Config) ConfigChangeSet {
	changes := ConfigChangeSet{RestartPaths: daemonRestartPaths(active, desired)}
	// The ${PORT} counter hands ports out by position in sorted-ID order, so
	// adding a model whose ID sorts earlier moves every later port. A survivor
	// whose port moved only because of that is not asking to be restarted, and
	// the shift has to be measured across the whole set to be told apart from a
	// real edit: see PortShift.
	shift := PortShift(active, desired)
	for id, desiredModel := range desired.Models {
		activeModel, exists := active.Models[id]
		if !exists {
			changes.AddedModels = append(changes.AddedModels, id)
			continue
		}
		if ModelRuntimeConfigChangedInPorts(activeModel, desiredModel, shift) || runtimeReferenceChanged(active, desired, id) {
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

// PortShift reports how far the ${PORT} counter moved for the models both
// configurations still have, when that move can only have come from the
// topology changing.
//
// The counter hands ports out by position in sorted-ID order, so a model that
// gains a port-consuming neighbour sorting before it moves up by one, and one
// that loses such a neighbour moves back. The operator's own text still asks for
// the same port either way, so the move is not something a restart can apply.
//
// Two conditions have to hold for the move to be the counter's: every survivor
// moved by the same amount, and the topology gained or lost exactly that many
// port-consuming models. Anything else returns 0, which makes the ports compare
// as they stand — that covers an edit of startPort (every port moves while the
// topology does not), a model the operator edited by hand (its port breaks the
// uniformity), and a configuration that was not using the counter at all.
func PortShift(active, desired Config) int {
	activePorts := allocatedPorts(active)
	desiredPorts := allocatedPorts(desired)
	// The topology has to have changed for the counter to have moved. Without a
	// gain or a loss, a uniform move is a startPort edit or a hand edit, not a
	// shift.
	netChange := len(desiredPorts) - len(activePorts)
	if netChange == 0 {
		return 0
	}
	shift, found := 0, false
	for id, activePort := range activePorts {
		desiredPort, survives := desiredPorts[id]
		if !survives {
			continue
		}
		delta := desiredPort - activePort
		if !found {
			shift, found = delta, true
			continue
		}
		if delta != shift {
			// Not a uniform move: at least one model changed for its own
			// reasons, so no shift can be assumed for the others either.
			return 0
		}
	}
	if !found {
		return 0
	}
	if shift != netChange {
		// The survivors moved by an amount the topology change does not
		// account for, so the counter is not what moved them.
		return 0
	}
	return shift
}

// allocatedPorts returns the port each model's proxy points at, for the models
// both configurations can compare. A model with no proxy, or one that names no
// port, is left out: there is nothing to shift.
func allocatedPorts(conf Config) map[string]int {
	ports := make(map[string]int, len(conf.Models))
	for id, model := range conf.Models {
		if port, ok := allocatedPort(model); ok {
			ports[id] = port
		}
	}
	return ports
}

// ModelRuntimeConfigChanged reports whether a running process must be
// recreated to observe the new model configuration. It includes both the
// legacy command lifecycle and first-class Backend/Runtime parameters.
func ModelRuntimeConfigChanged(active, desired ModelConfig) bool {
	if !reflect.DeepEqual(modelRuntimeSpecOf(active), modelRuntimeSpecOf(desired)) {
		return true
	}
	return vllmSleepModeArgChanged(active, desired)
}

// ModelLifecycleChanged reports whether the control-plane lifecycle fields —
// idle TTL, unload timeout and the vLLM sleep policy — differ between two
// snapshots of the same model. These fields never reach the process command
// line (the one exception, the injected --enable-sleep-mode flag, is what
// ModelRuntimeConfigChanged's vllmSleepModeArgChanged guard exists for), so
// the router applies them into a live process instead of scheduling a
// restart.
func ModelLifecycleChanged(active, desired ModelConfig) bool {
	return active.UnloadAfter != desired.UnloadAfter ||
		active.UnloadTimeout != desired.UnloadTimeout ||
		active.Backend.Lifecycle != desired.Backend.Lifecycle
}

// vllmSleepModeArgChanged reports whether the launch argv would gain or lose
// the injected --enable-sleep-mode flag. The flag is derived from the TTL at
// start time (see EnsureVLLMSleepModeArgument), so a TTL edit that crosses
// zero changes the command line of a vLLM process even though every other
// lifecycle field is hot-appliable; only that transition is reported as a
// runtime change.
func vllmSleepModeArgChanged(active, desired ModelConfig) bool {
	return vllmSleepModeArgWanted(active) != vllmSleepModeArgWanted(desired)
}

// vllmSleepModeArgWanted reports whether a start of this configuration would
// inject --enable-sleep-mode. Bound managed-runtime snapshots carry the flag
// inside backend.args, where the spec comparison already sees it, so the
// probe only needs to model the start-time injection for operator-written
// argv: an explicit flag in any argv shape suppresses the injection.
func vllmSleepModeArgWanted(model ModelConfig) bool {
	if !strings.EqualFold(strings.TrimSpace(model.Backend.Type), "vllm") || model.UnloadAfter <= 0 {
		return false
	}
	if HasExplicitSleepModeArgument(model.Backend.Arguments) || HasExplicitSleepModeArgument(model.Backend.Container.Command) {
		return false
	}
	if strings.TrimSpace(model.Cmd) != "" {
		if tokens, err := SanitizeCommand(model.Cmd); err == nil && HasExplicitSleepModeArgument(tokens) {
			return false
		}
	}
	return true
}

// ModelRuntimeConfigChangedInPorts is ModelRuntimeConfigChanged with a pure
// ${PORT} counter shift excluded from the comparison. It is the variant a
// reconciliation must use: the router compares a running generation against the
// freshly loaded snapshot, and the counter moves for reasons that have nothing
// to do with the model being compared.
//
// shift is the uniform counter move PortShift measured across both
// configurations. When it is 0 nothing moved as a block, so the ports are
// compared as they stand. Otherwise this model's port moved with the counter,
// and the ports are compared with the operator's own ${PORT} placeholder put
// back so that move is not counted against the model.
func ModelRuntimeConfigChangedInPorts(active, desired ModelConfig, shift int) bool {
	if shift == 0 {
		return ModelRuntimeConfigChanged(active, desired)
	}
	if !reflect.DeepEqual(modelRuntimeSpecOfWithPorts(active), modelRuntimeSpecOfWithPorts(desired)) {
		return true
	}
	return vllmSleepModeArgChanged(active, desired)
}

// modelRuntimeSpecOfWithPorts is modelRuntimeSpecOf with the port reverted to
// the operator's ${PORT} placeholder. Two snapshots of the same model taken
// before and after an unrelated model was added differ only in that number,
// because the counter that hands it out is positional; reverting it makes that
// shift invisible so the comparison answers the question the caller is actually
// asking, which is whether this model's own configuration changed.
func modelRuntimeSpecOfWithPorts(model ModelConfig) modelRuntimeSpec {
	spec := modelRuntimeSpecOf(model)
	port, ok := allocatedPort(model)
	if !ok {
		return spec
	}
	revert := func(value string) string {
		return strings.ReplaceAll(value, strconv.Itoa(port), portPlaceholder)
	}
	spec.Cmd = revert(spec.Cmd)
	spec.CmdStop = revert(spec.CmdStop)
	spec.Proxy = revert(spec.Proxy)
	spec.CheckEndpoint = revert(spec.CheckEndpoint)
	spec.Env = revertPortInSlice(spec.Env, port)
	// The spec shares its Arguments slice with the model it came from, so
	// reverted values go into a copy: rewriting in place would rewrite the
	// operator's configuration under them.
	if len(spec.Backend.Arguments) > 0 {
		reverted := make([]string, len(spec.Backend.Arguments))
		for i, argument := range spec.Backend.Arguments {
			reverted[i] = revert(argument)
		}
		spec.Backend.Arguments = reverted
	}
	return spec
}

// portPlaceholder stands in for the allocated ${PORT} value while comparing two
// snapshots. It is the same token the operator wrote, so a reverted spec reads
// back as the pre-expansion configuration.
const portPlaceholder = "${PORT}"

// allocatedPort returns the port this model's proxy points at. Every model that
// consumed a ${PORT} carries it there, because the loader writes the default
// proxy — or expands the operator's own — with the same value it substituted
// into the command, so the proxy is the one place the number is guaranteed to
// appear however the command is spelled.
func allocatedPort(model ModelConfig) (int, bool) {
	trimmed := strings.TrimSpace(model.Proxy)
	if trimmed == "" {
		return 0, false
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return 0, false
	}
	return port, true
}

// revertPortInSlice replaces an allocated port with the operator's placeholder
// so a shifted counter does not read as a change to the environment.
func revertPortInSlice(values []string, port int) []string {
	if len(values) == 0 {
		return values
	}
	reverted := make([]string, len(values))
	for i, value := range values {
		reverted[i] = strings.ReplaceAll(value, strconv.Itoa(port), portPlaceholder)
	}
	return reverted
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
	return ModelRuntimeConfigChangedInPorts(activeModel, desiredModel, PortShift(active, desired)) ||
		runtimeReferenceChanged(active, desired, modelID)
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
// Lifecycle (sleep mode/sleep level) is stripped for the same reason: the
// process reads it at TTL expiry through the router's hot-applied snapshot.
func processBackendSpec(backend BackendConfig) BackendConfig {
	backend.Protocol = ""
	backend.APIs = nil
	backend.Discover = nil
	backend.Lifecycle = LifecycleConfig{}
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
	// The accept-loop limits are read once when http.Server is constructed, so
	// a reload cannot apply them. Reporting them as hot-applied would leave an
	// operator believing a tightened limit is already in force.
	if active.Server.MaxHeaderBytes != desired.Server.MaxHeaderBytes {
		paths = append(paths, "/server/maxHeaderBytes")
	}
	if active.Server.ReadTimeout != desired.Server.ReadTimeout {
		paths = append(paths, "/server/readTimeout")
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
	// Keep the active accept-loop limits until the daemon restarts; the
	// restart paths above tell the operator a restart is pending.
	result.Server.MaxHeaderBytes = active.Server.MaxHeaderBytes
	result.Server.ReadTimeout = active.Server.ReadTimeout
	return result, paths
}
