package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// RuntimeReadiness is the read-only, operator-facing preflight projection for
// a runtime version. It deliberately keeps facts separate: detected hardware
// is not a budget, and a configured model footprint is not proof that the
// model is currently loaded.
type RuntimeReadiness struct {
	Runtime   RuntimeReadinessRuntime  `json:"runtime"`
	Selected  RuntimeReadinessVersion  `json:"selected"`
	Idle      RuntimeIdleReport        `json:"idle"`
	Models    []RuntimeModelImpact     `json:"models"`
	Resources RuntimeResourceReadiness `json:"resources"`
	Rollback  RuntimeRollbackReadiness `json:"rollback"`
	Checks    []RuntimeReadinessCheck  `json:"checks"`
	Generated time.Time                `json:"generatedAt"`
}

type RuntimeReadinessRuntime struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Mode       string `json:"mode"`
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	Current    string `json:"current,omitempty"`
	Candidate  string `json:"candidate,omitempty"`
	Staged     string `json:"staged,omitempty"`
	Previous   string `json:"previous,omitempty"`
	Pinned     string `json:"pinned,omitempty"`
}

type RuntimeReadinessVersion struct {
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Source    string `json:"source,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Installed bool   `json:"installed"`
	Current   bool   `json:"current"`
	Staged    bool   `json:"staged"`
	Previous  bool   `json:"previous"`
	Pinned    bool   `json:"pinned"`
}

type RuntimeIdleReport struct {
	Scope   string              `json:"scope"`
	Ready   bool                `json:"ready"`
	Reasons []RuntimeIdleReason `json:"reasons,omitempty"`
}

type RuntimeIdleReason struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Count  int    `json:"count,omitempty"`
}

type RuntimeModelImpact struct {
	ID             string                    `json:"id"`
	Type           string                    `json:"type"`
	Runtime        string                    `json:"runtime"`
	RuntimeVersion string                    `json:"runtimeVersion,omitempty"`
	State          string                    `json:"state,omitempty"`
	Loaded         bool                      `json:"loaded"`
	Sleeping       bool                      `json:"sleeping"`
	Inflight       int                       `json:"inflight"`
	LegacyCommand  bool                      `json:"legacyCommand"`
	Compatible     bool                      `json:"compatible"`
	Compatibility  string                    `json:"compatibility,omitempty"`
	WillRestart    bool                      `json:"willRestart"`
	Declared       bool                      `json:"declaredFootprint"`
	Footprint      resourcePlanner.Footprint `json:"footprint"`
}

type RuntimeResourceReadiness struct {
	Configured bool                   `json:"configured"`
	Budget     resourcePlanner.Budget `json:"budget"`
	UsageVRAM  int                    `json:"usageVRAMMiB"`
	UsageRAM   int                    `json:"usageRAMMiB"`
	Models     []RuntimeResourceModel `json:"models"`
	Hardware   RuntimeHardwareSummary `json:"hardware"`
}

type RuntimeResourceModel struct {
	ID        string                    `json:"id"`
	Loaded    bool                      `json:"loaded"`
	Sleeping  bool                      `json:"sleeping"`
	Inflight  int                       `json:"inflight"`
	Declared  bool                      `json:"declared"`
	Footprint resourcePlanner.Footprint `json:"footprint"`
}

type RuntimeHardwareSummary struct {
	Detected     bool `json:"detected"`
	Accelerators int  `json:"accelerators"`
	VRAMMiB      int  `json:"vramMiB"`
	SystemRAMMiB int  `json:"systemRAMMiB"`
}

type RuntimeRollbackReadiness struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Automatic bool   `json:"automatic"`
	Detail    string `json:"detail"`
}

type RuntimeReadinessCheck struct {
	Code   string `json:"code"`
	Level  string `json:"level"` // pass | warning | block
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

// runtimeIdleReasons is the single explanation source for the existing idle
// gate. Keep the checks and their order aligned with runtimeIdle so a readiness
// response never claims a safer state than the activation path enforces.
func (s *Server) runtimeIdleReasons(name string) []RuntimeIdleReason {
	if s == nil {
		return []RuntimeIdleReason{{Code: "server-unavailable", Title: "server unavailable", Detail: "the server state is not available"}}
	}
	reasons := make([]RuntimeIdleReason, 0, 5)
	if active := s.controlPlaneActive.Load(); active > 0 {
		reasons = append(reasons, RuntimeIdleReason{
			Code: "control-plane-busy", Title: "control-plane operation in progress",
			Detail: fmt.Sprintf("%d control-plane operation(s) are still running", active), Count: int(active),
		})
	}
	if s.inflight != nil {
		if count := len(s.inflight.Current().Requests); count > 0 {
			reasons = append(reasons, RuntimeIdleReason{
				Code: "inference-in-flight", Title: "inference requests are active",
				Detail: fmt.Sprintf("%d inference request(s) must finish first", count), Count: count,
			})
		}
	}
	if s.local != nil {
		transitions := 0
		for _, state := range s.local.RunningModels() {
			if state == process.StateStarting || state == process.StateStopping {
				transitions++
			}
		}
		if transitions > 0 {
			reasons = append(reasons, RuntimeIdleReason{
				Code: "model-lifecycle-transition", Title: "model lifecycle transition in progress",
				Detail: fmt.Sprintf("%d model process transition(s) must settle first", transitions), Count: transitions,
			})
		}
	}
	if name == config.LMCacheRuntimeName && s.lmcacheMod != nil {
		if models := s.lmcacheMod.inUseModels(s.currentConfig()); len(models) > 0 {
			reasons = append(reasons, RuntimeIdleReason{
				Code: "lmcache-model-in-use", Title: "LMCache models are in use",
				Detail: fmt.Sprintf("%d LMCache model reference(s) must be released first", len(models)), Count: len(models),
			})
		}
		switch s.lmcacheMod.proc.Status().State {
		case LMCacheStateStarting, LMCacheStateStopping, LMCacheStateUpdating:
			reasons = append(reasons, RuntimeIdleReason{
				Code: "lmcache-lifecycle-transition", Title: "LMCache lifecycle transition in progress",
				Detail: "the LMCache service must finish its current operation first",
			})
		}
	}
	return reasons
}

func (s *Server) handleAPIRuntimeReadiness(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	identity := identityFromContext(r.Context())
	if !runtimeAllowedForIdentity(cfg, identity, name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	readiness, ok, err := s.buildRuntimeReadiness(name, strings.TrimSpace(r.URL.Query().Get("version")), identity)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "runtime not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(readiness)
}

func (s *Server) buildRuntimeReadiness(name, requestedVersion string, identity auth.Identity) (RuntimeReadiness, bool, error) {
	if s == nil || s.runtime == nil {
		return RuntimeReadiness{}, false, nil
	}
	detail, ok, err := s.runtime.Detail(name)
	if err != nil || !ok {
		return RuntimeReadiness{}, ok, err
	}

	cfg := s.currentConfig()
	status := detail.Status
	definition, hasDefinition := s.runtime.Definition(name)
	runtimeCfg, hasRuntimeConfig := cfg.Runtimes[name]
	configured := status.Configured || hasDefinition || hasRuntimeConfig

	kind := status.Kind
	mode := status.Mode
	source := status.Source
	if hasRuntimeConfig {
		kind = runtimeCfg.Kind
		mode = runtimeCfg.Mode
		source = runtimeCfg.Source.Repository
		if source == "" {
			source = runtimeCfg.Source.Path
		}
		if source == "" {
			source = runtimeCfg.Source.Image
		}
		if source == "" {
			source = runtimeCfg.Source.URL
		}
	}
	if kind == "" && hasDefinition {
		kind = definition.Spec.Kind
	}
	if mode == "" && hasDefinition {
		mode = definition.Spec.Mode
	}
	if source == "" && hasDefinition {
		source = definition.Spec.Source
	}
	if mode == "" {
		mode = runtimeManager.RuntimeModeNative
	}

	selectedVersion := strings.TrimSpace(requestedVersion)
	if selectedVersion == "" {
		selectedVersion = firstNonEmpty(status.Staged, status.Available, status.Current)
	}
	manifest, installed := detail.Versions[selectedVersion]
	selected := RuntimeReadinessVersion{
		Version:   selectedVersion,
		Kind:      kind,
		Source:    source,
		Installed: installed,
		Current:   selectedVersion != "" && selectedVersion == status.Current,
		Staged:    selectedVersion != "" && selectedVersion == status.Staged,
		Previous:  selectedVersion != "" && selectedVersion == status.Previous,
		Pinned:    selectedVersion != "" && selectedVersion == status.Pinned,
	}
	if installed {
		if manifest.Kind != "" {
			selected.Kind = manifest.Kind
		}
		if manifest.Source != "" {
			selected.Source = manifest.Source
		}
		selected.Ref = manifest.Ref
		selected.Commit = manifest.Commit
	}
	// Bundled runtimes can have a current pointer without a persisted manifest.
	// The pointer is still a valid selected version for a read-only readiness
	// report; activation operations remain owned by the manager.
	if selected.Current {
		selected.Installed = true
	}

	readiness := RuntimeReadiness{
		Runtime: RuntimeReadinessRuntime{
			Name: name, Kind: kind, Mode: mode, State: string(status.State), Configured: configured,
			Current: status.Current, Candidate: status.Available, Staged: status.Staged,
			Previous: status.Previous, Pinned: status.Pinned,
		},
		Selected:  selected,
		Idle:      RuntimeIdleReport{Scope: "global"},
		Models:    make([]RuntimeModelImpact, 0),
		Resources: RuntimeResourceReadiness{Models: make([]RuntimeResourceModel, 0)},
		Rollback:  RuntimeRollbackReadiness{},
		Checks:    make([]RuntimeReadinessCheck, 0, 8),
		Generated: time.Now().UTC(),
	}

	addCheck := func(code, level, title, checkDetail string) {
		readiness.Checks = append(readiness.Checks, RuntimeReadinessCheck{Code: code, Level: level, Title: title, Detail: checkDetail})
	}
	if !configured {
		addCheck("runtime-not-configured", "block", "runtime is not configured", "add a runtime definition before selecting a version")
	}
	if selected.Version == "" {
		addCheck("version-unavailable", "block", "no version is selected", "check the source or stage a concrete version first")
	} else if !selected.Current && !selected.Staged && !selected.Installed {
		// A version that is already installed (recorded in the version
		// ledger) is activatable even when it is not the staged candidate:
		// activation re-validates the version directory before switching.
		// Only a version that is neither installed nor staged must be
		// staged first.
		addCheck("candidate-not-staged", "block", "candidate is not staged", fmt.Sprintf("version %q must be staged before activation", selected.Version))
	} else {
		addCheck("version-selected", "pass", "selected version is available", selected.Version)
	}

	// Reconcile the same planner snapshot used by the resource API when a
	// budget planner exists. If budgets are disabled, retain the declared
	// model facts without manufacturing a zero-capacity conclusion.
	resourceStatuses := make(map[string]resourceModelStatus)
	if s.resourcePlanner() != nil {
		for _, item := range s.reconcileResources() {
			resourceStatuses[item.ID] = item
		}
	}
	inflightByModel := make(map[string]int)
	if s.inflight != nil {
		for _, request := range s.inflight.Current().Requests {
			modelID, found := cfg.RealModelName(request.Model)
			if found {
				inflightByModel[modelID]++
			}
		}
	}
	running := map[string]process.ProcessState{}
	if s.local != nil {
		running = s.local.RunningModels()
	}

	modelIDs := make([]string, 0, len(cfg.Models))
	for id, modelCfg := range cfg.Models {
		if strings.TrimSpace(modelCfg.Backend.Runtime) != name || !modelAllowedForIdentity(cfg, identity, id) {
			continue
		}
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	incompatible := make([]string, 0)
	missingFootprint := 0
	for _, id := range modelIDs {
		modelCfg := cfg.Models[id]
		resourceState := resourceStatuses[id]
		state := string(running[id])
		if resourceState.State != "" {
			state = resourceState.State
		}
		footprint := resourceFootprint(modelCfg.Backend.Resources)
		declared := footprint.VRAMMiB > 0 || footprint.RAMMiB > 0
		if !declared {
			missingFootprint++
		}
		compatible, compatibilityDetail := runtimeModelCompatible(runtimeCfg, modelCfg)
		if !hasRuntimeConfig {
			compatible = false
			compatibilityDetail = "the model's runtime definition is not present in configuration"
		}
		if !compatible {
			incompatible = append(incompatible, id)
		}
		loaded := resourceState.Loaded
		sleeping := resourceState.Sleeping
		if _, exists := running[id]; exists && resourceState.ID == "" {
			loaded = true
		}
		pinnedVersion := strings.TrimSpace(modelCfg.Backend.RuntimeVersion)
		willRestart := selected.Version != "" && selected.Version != status.Current && (pinnedVersion == "" || pinnedVersion == status.Current)
		readiness.Models = append(readiness.Models, RuntimeModelImpact{
			ID: id, Type: modelCfg.Backend.Type, Runtime: name, RuntimeVersion: pinnedVersion,
			State: state, Loaded: loaded, Sleeping: sleeping, Inflight: inflightByModel[id],
			LegacyCommand: strings.TrimSpace(modelCfg.Cmd) != "", Compatible: compatible,
			Compatibility: compatibilityDetail, WillRestart: willRestart, Declared: declared,
			Footprint: footprint,
		})
		readiness.Resources.Models = append(readiness.Resources.Models, RuntimeResourceModel{
			ID: id, Loaded: loaded, Sleeping: sleeping, Inflight: inflightByModel[id],
			Declared: declared, Footprint: footprint,
		})
	}
	if len(incompatible) > 0 {
		addCheck("model-incompatible", "block", "one or more attached models are incompatible", "incompatible models: "+strings.Join(incompatible, ", "))
	}
	if missingFootprint > 0 {
		addCheck("model-footprint-unknown", "warning", "model footprint is not fully declared", fmt.Sprintf("%d attached model(s) have no VRAM/RAM footprint; capacity cannot be concluded", missingFootprint))
	}

	readiness.Idle.Reasons = s.runtimeIdleReasons(name)
	readiness.Idle.Ready = len(readiness.Idle.Reasons) == 0
	switching := selected.Version != "" && selected.Version != status.Current
	if switching && !readiness.Idle.Ready {
		reasons := make([]string, 0, len(readiness.Idle.Reasons))
		for _, reason := range readiness.Idle.Reasons {
			reasons = append(reasons, reason.Detail)
		}
		addCheck("global-idle-required", "block", "global idle is required", strings.Join(reasons, "; "))
	} else {
		addCheck("global-idle", "pass", "global idle gate is clear", "no inference, lifecycle, or control operation is blocking this check")
	}

	planner := s.resourcePlanner()
	budget := resourcePlanner.Budget{VRAMMiB: cfg.ResourceBudget.VRAMMiB, RAMMiB: cfg.ResourceBudget.RAMMiB}
	if planner != nil {
		budget = planner.Budget()
	}
	readiness.Resources.Configured = cfg.ResourceBudget.Enabled()
	readiness.Resources.Budget = budget
	if planner != nil {
		readiness.Resources.UsageVRAM, readiness.Resources.UsageRAM = planner.Usage()
	} else {
		for _, item := range readiness.Resources.Models {
			if item.Loaded && !item.Sleeping {
				readiness.Resources.UsageVRAM += item.Footprint.VRAMMiB
				readiness.Resources.UsageRAM += item.Footprint.RAMMiB
			}
		}
	}
	if !readiness.Resources.Configured {
		addCheck("budget-unconfigured", "warning", "resource budget is not enabled", "hardware is shown for reference; set a VRAM or RAM budget to enable admission accounting")
	} else {
		if budget.VRAMMiB > 0 && readiness.Resources.UsageVRAM > budget.VRAMMiB {
			addCheck("budget-vram-exceeded", "block", "declared VRAM usage exceeds the budget", fmt.Sprintf("%d MiB used against a %d MiB budget", readiness.Resources.UsageVRAM, budget.VRAMMiB))
		}
		if budget.RAMMiB > 0 && readiness.Resources.UsageRAM > budget.RAMMiB {
			addCheck("budget-ram-exceeded", "block", "declared RAM usage exceeds the budget", fmt.Sprintf("%d MiB used against a %d MiB budget", readiness.Resources.UsageRAM, budget.RAMMiB))
		}
		if budget.VRAMMiB <= 0 && budget.RAMMiB <= 0 {
			addCheck("budget-unconfigured", "warning", "resource budget is not enabled", "no VRAM or RAM limit is configured")
		}
	}

	readiness.Resources.Hardware = hardwareSummary(s.hardware)
	if !readiness.Resources.Hardware.Detected || readiness.Resources.Hardware.Accelerators == 0 {
		addCheck("hardware-unknown", "warning", "hardware telemetry is incomplete", "detected hardware is advisory and cannot prove runtime capacity")
	}

	update := config.RuntimeUpdateConfig{}
	if hasRuntimeConfig {
		update = runtimeCfg.Update.Effective()
	}
	readiness.Rollback = RuntimeRollbackReadiness{
		Available: status.Previous != "", Version: status.Previous,
		Automatic: update.RollbackOnFailure,
	}
	if readiness.Rollback.Available {
		readiness.Rollback.Detail = "previous version is available for recovery"
		addCheck("rollback-available", "pass", "rollback version is retained", status.Previous)
	} else if switching {
		readiness.Rollback.Detail = "this activation has no retained previous version"
		addCheck("rollback-unavailable", "warning", "no rollback version is available", "the first activation or a pruned history cannot be automatically restored")
	} else {
		readiness.Rollback.Detail = "no previous version is currently retained"
	}

	return readiness, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func runtimeModelCompatible(runtimeCfg config.RuntimeConfig, model config.ModelConfig) (bool, string) {
	runtimeKind := strings.ToLower(strings.TrimSpace(runtimeCfg.Kind))
	backendKind := strings.ToLower(strings.TrimSpace(model.Backend.Type))
	if runtimeKind == "" || backendKind != runtimeKind {
		return false, fmt.Sprintf("backend type %q must match runtime kind %q", model.Backend.Type, runtimeCfg.Kind)
	}
	if runtimeKind != "vllm" && runtimeKind != "llamacpp" {
		return false, fmt.Sprintf("runtime kind %q is not supported for model binding", runtimeCfg.Kind)
	}
	if strings.EqualFold(strings.TrimSpace(runtimeCfg.Mode), "container") {
		if strings.TrimSpace(model.Cmd) != "" {
			return false, "container bindings cannot use the legacy cmd field"
		}
		if strings.TrimSpace(model.Backend.Container.Image) == "" && strings.TrimSpace(runtimeCfg.Container.Image) == "" && strings.TrimSpace(runtimeCfg.Source.Image) == "" {
			return false, "container runtime requires a runtime or backend image"
		}
		if model.Backend.LMCache != nil {
			return false, "LMCache requires a native vLLM runtime"
		}
		return true, "container backend and runtime are compatible"
	}
	if strings.TrimSpace(model.Cmd) != "" {
		return false, "native bindings cannot use the legacy cmd field"
	}
	if strings.TrimSpace(model.Backend.Container.Image) != "" {
		return false, "native runtime cannot use a backend container image"
	}
	if len(model.Backend.Arguments) == 0 && model.Backend.Launch == nil {
		return false, "native runtime requires backend args or a structured launch block"
	}
	return true, "native backend and runtime are compatible"
}

func hardwareSummary(snapshot *hw.HardwareSnapshot) RuntimeHardwareSummary {
	if snapshot == nil {
		return RuntimeHardwareSummary{}
	}
	summary := RuntimeHardwareSummary{Detected: true, Accelerators: len(snapshot.Accelerators), SystemRAMMiB: bytesToMiB(snapshot.Memory.CapacityBytes)}
	for _, accelerator := range snapshot.Accelerators {
		if accelerator.Memory.CapacityBytes != nil {
			summary.VRAMMiB += bytesToMiB(*accelerator.Memory.CapacityBytes)
		}
	}
	return summary
}

func bytesToMiB(value uint64) int {
	const mib = uint64(1024 * 1024)
	value /= mib
	maxInt := uint64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}
