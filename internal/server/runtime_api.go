package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func (s *Server) handleAPIRuntimes(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	identity := identityFromContext(r.Context())
	statuses := s.runtime.List()
	if _, restricted := allowedCanonicalModels(cfg, identity); restricted {
		visible := statuses[:0]
		for _, status := range statuses {
			if runtimeAllowedForIdentity(cfg, identity, status.Name) {
				visible = append(visible, status)
			}
		}
		statuses = visible
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": statuses})
}

func (s *Server) handleAPIRuntime(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	detail, ok, err := s.runtime.Detail(name)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "runtime not found")
		return
	}
	json.NewEncoder(w).Encode(detail)
}

func (s *Server) handleAPIRuntimeVersions(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	catalog, err := s.runtime.VersionCatalog(ctx, name)
	if err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadGateway), err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data":       catalog.Versions,
		"sourceType": catalog.SourceType,
		"supported":  catalog.Supported,
		"fetchedAt":  time.Now().UTC(),
	})
}

func (s *Server) handleAPIRuntimeCheck(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	status, err := s.runtime.Check(ctx, name)
	if err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadGateway), err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

type runtimeStageRequest struct {
	Kind        string                          `json:"kind"`
	Mode        string                          `json:"mode"`
	Version     string                          `json:"version"`
	SourceType  string                          `json:"sourceType"`
	Source      string                          `json:"source"`
	Repository  string                          `json:"repository"`
	TrackRef    string                          `json:"trackRef"`
	Image       string                          `json:"image"`
	Platform    string                          `json:"platform"`
	PullPolicy  string                          `json:"pullPolicy"`
	Ref         string                          `json:"ref"`
	Commit      string                          `json:"commit"`
	Digest      string                          `json:"digest"`
	Checksum    string                          `json:"checksum"`
	Build       map[string]string               `json:"build"`
	BuildArgs   map[string][]string             `json:"buildArgs"`
	BuildEnv    map[string]string               `json:"buildEnv"`
	BuildSteps  []runtimeManager.BuildStep      `json:"buildSteps"`
	InstallArgs []string                        `json:"installArgs"`
	Artifacts   []runtimeManager.Artifact       `json:"artifacts"`
	Container   *runtimeManager.ContainerLaunch `json:"container"`
	Metadata    map[string]string               `json:"metadata"`
}

const maxRuntimeStageJSONBody = 1 << 20

// runtimeStageSpecFromRequest overlays the small, user-selectable portion of a
// configured runtime definition onto the provider-neutral spec. The UI should
// not have to echo build flags, credentials, source allowlists, or container
// mounts back to the server just to install another version; the manager's
// configured definition remains the source of truth for those fields.
func (s *Server) runtimeStageSpecFromRequest(name string, req runtimeStageRequest) runtimeManager.Spec {
	spec, configured := runtimeManager.Spec{Name: name}, false
	if definition, ok := s.runtime.Definition(name); ok {
		spec = definition.Spec
		configured = true
	}
	spec.Name = name
	if value := strings.TrimSpace(req.Kind); value != "" {
		spec.Kind = value
	}
	if value := strings.TrimSpace(req.Mode); value != "" {
		spec.Mode = value
	}
	if value := strings.TrimSpace(req.SourceType); value != "" {
		spec.SourceType = value
	}
	if value := strings.TrimSpace(req.Source); value != "" {
		spec.Source = value
	} else if value := strings.TrimSpace(req.Image); value != "" {
		spec.Source = value
	} else if req.Container != nil && strings.TrimSpace(req.Container.Image) != "" {
		spec.Source = strings.TrimSpace(req.Container.Image)
	} else if value := strings.TrimSpace(req.Repository); value != "" {
		spec.Source = value
	}
	if strings.TrimSpace(req.Repository) != "" && strings.TrimSpace(req.SourceType) == "" {
		spec.SourceType = "git"
	}
	if strings.TrimSpace(req.Image) != "" || (req.Container != nil && strings.TrimSpace(req.Container.Image) != "") {
		if strings.TrimSpace(req.SourceType) == "" {
			spec.SourceType = "image"
		}
		if spec.Mode == "" {
			spec.Mode = runtimeManager.RuntimeModeContainer
		}
	}
	if value := strings.TrimSpace(req.Ref); value != "" {
		spec.Ref = value
	} else if value := strings.TrimSpace(req.TrackRef); value != "" {
		spec.Ref = value
	}
	if value := strings.TrimSpace(req.Commit); value != "" {
		spec.Commit = value
	}
	if value := strings.TrimSpace(req.Checksum); value != "" {
		spec.Checksum = value
	}
	if req.Build != nil {
		spec.Build = req.Build
	}
	if req.BuildArgs != nil {
		spec.BuildArgs = req.BuildArgs
	}
	if req.BuildEnv != nil {
		spec.BuildEnv = req.BuildEnv
	}
	if req.BuildSteps != nil {
		spec.BuildSteps = req.BuildSteps
	}
	if req.InstallArgs != nil {
		spec.InstallArgs = req.InstallArgs
	}
	if req.Artifacts != nil {
		spec.Artifacts = req.Artifacts
	}
	if req.Container != nil {
		spec.Container = req.Container
	}
	metadata := cloneStringMap(spec.Metadata)
	if metadata == nil && (req.Metadata != nil || strings.TrimSpace(req.Image) != "" || strings.TrimSpace(req.Platform) != "" || strings.TrimSpace(req.PullPolicy) != "" || strings.TrimSpace(req.TrackRef) != "" || strings.TrimSpace(req.Digest) != "") {
		metadata = make(map[string]string)
	}
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	if value := strings.TrimSpace(req.Image); value != "" {
		metadata["image"] = value
	} else if req.Container != nil {
		if value := strings.TrimSpace(req.Container.Image); value != "" {
			metadata["image"] = value
		}
	}
	if value := strings.TrimSpace(req.Platform); value != "" {
		metadata["platform"] = value
	}
	if value := strings.TrimSpace(req.PullPolicy); value != "" {
		metadata["pullPolicy"] = value
	}
	if value := strings.TrimSpace(req.TrackRef); value != "" {
		metadata["trackRef"] = value
	}
	if value := strings.TrimSpace(req.Digest); value != "" {
		metadata["expectedImageDigest"] = value
		metadata["resolvedImageDigest"] = value
	}
	spec.Metadata = metadata
	if version := strings.TrimSpace(req.Version); version != "" {
		spec = runtimeSpecForSelectedVersion(spec, version)
		// Explicit ref/commit fields are higher precedence than the convenient
		// source-aware version shorthand above.
		if value := strings.TrimSpace(req.Ref); value != "" {
			spec.Ref = value
		}
		if value := strings.TrimSpace(req.Commit); value != "" {
			spec.Commit = value
		}
	}
	if !configured && spec.Mode == "" && strings.EqualFold(strings.TrimSpace(spec.SourceType), "image") {
		spec.Mode = runtimeManager.RuntimeModeContainer
	}
	return spec
}

func runtimeSpecForSelectedVersion(spec runtimeManager.Spec, version string) runtimeManager.Spec {
	spec.Version = strings.TrimSpace(version)
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType != "git" && sourceType != "tag" && sourceType != "commit" {
		return spec
	}
	commit := ""
	if strings.HasPrefix(spec.Version, "git-") {
		commit = strings.TrimPrefix(spec.Version, "git-")
	} else if looksLikeGitCommit(spec.Version) {
		commit = spec.Version
		spec.Version = "git-" + spec.Version
	}
	if commit != "" && looksLikeGitCommit(commit) {
		spec.Ref = commit
		spec.Commit = commit
	} else if sourceType != "commit" {
		// For tag/branch-backed definitions, entering a release/tag name means
		// checkout that exact ref instead of silently retaining the tracked ref.
		spec.Ref = strings.TrimSpace(version)
	}
	return spec
}

func looksLikeGitCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func (s *Server) handleAPIRuntimeStage(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.lmcacheRuntimeOperationGuard(cfg, name); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	var req runtimeStageRequest
	if err := decodeJSONBody(w, r, &req, maxRuntimeStageJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid runtime stage request")
		return
	}
	ctx, cancel := s.stageOperationContext()
	defer cancel()
	manifest, err := s.runtime.Stage(ctx, s.runtimeStageSpecFromRequest(name, req))
	if err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadRequest), err.Error())
		return
	}
	json.NewEncoder(w).Encode(manifest)
}

func (s *Server) handleAPIRuntimeActivate(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.lmcacheRuntimeOperationGuard(cfg, name); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	if err := s.runtime.Activate(ctx, name, r.PathValue("version")); err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusConflict), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIRuntimeRollback(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.lmcacheRuntimeOperationGuard(cfg, name); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := s.runtimeOperationContext(r)
	defer cancel()
	if err := s.runtime.Rollback(ctx, name); err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusConflict), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// runtimeOperationContext applies the configured deadline to potentially
// long-running runtime checks, downloads, and health checks. A zero timeout
// intentionally preserves the historical unbounded operation.
func (s *Server) runtimeOperationContext(r *http.Request) (context.Context, context.CancelFunc) {
	if r == nil {
		return context.Background(), func() {}
	}
	timeout := s.currentConfig().RuntimeManager.OperationTimeout
	if timeout <= 0 {
		return r.Context(), func() {}
	}
	return context.WithTimeout(r.Context(), timeout)
}

// stageOperationContext bounds a stage operation the same way as
// runtimeOperationContext, but it never derives from the request context.
// Source builds routinely outlive the invoking client (a curl timeout must
// not SIGKILL a multi-hour compile and leave the runtime half built); the
// operation continues after a client disconnect and records its outcome in
// the runtime state, which callers can poll. It does derive from the daemon
// shutdown context so a full shutdown cancels in-flight builds instead of
// leaving them running while the process tries to exit.
func (s *Server) stageOperationContext() (context.Context, context.CancelFunc) {
	timeout := s.currentConfig().RuntimeManager.OperationTimeout
	if timeout <= 0 {
		return s.shutdownCtx, func() {}
	}
	return context.WithTimeout(s.shutdownCtx, timeout)
}

func runtimeOperationStatus(err error, fallback int) int {
	if errors.Is(err, runtimeManager.ErrRuntimeBusy) {
		return http.StatusConflict
	}
	if errors.Is(err, runtimeManager.ErrVersionProtected) {
		return http.StatusConflict
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return http.StatusRequestTimeout
	}
	return fallback
}

func (s *Server) handleAPIRuntimeDeleteVersion(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	version := strings.TrimSpace(r.PathValue("version"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.lmcacheRuntimeOperationGuard(cfg, name); err != nil {
		swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		return
	}
	if protected, reason := s.protectedRuntimeVersion(name, version); protected {
		if strings.TrimSpace(reason) == "" {
			reason = "runtime version is still referenced"
		}
		swaputil.SendResponse(w, r, http.StatusConflict, reason)
		return
	}
	if err := s.runtime.RemoveVersion(name, version); err != nil {
		swaputil.SendResponse(w, r, runtimeOperationStatus(err, http.StatusBadRequest), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIRuntimePin(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.runtime.Pin(name, r.PathValue("version")); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIRuntimeUnpin(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	if err := s.runtime.Unpin(name); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type backendStatus struct {
	Model        string                  `json:"model"`
	Type         string                  `json:"type,omitempty"`
	Runtime      string                  `json:"runtime,omitempty"`
	Protocol     string                  `json:"protocol,omitempty"`
	APIs         []string                `json:"apis,omitempty"`
	Lifecycle    *config.LifecycleConfig `json:"lifecycle,omitempty"`
	Resources    *config.ResourceConfig  `json:"resources,omitempty"`
	Pricing      *config.PricingRef      `json:"pricing,omitempty"`
	Capabilities map[string]bool         `json:"capabilities,omitempty"`
	CacheState   string                  `json:"cacheState"`
	Cache        *backend.CacheState     `json:"cache,omitempty"`
	CacheReport  *backend.CacheReport    `json:"cacheReport,omitempty"`
	Discovery    *backend.Discovery      `json:"discovery,omitempty"`
}

// resolveBackendModel accepts both a concrete local model ID and one of its
// configured aliases. Backend adapters and cache/progress snapshots are keyed
// by the concrete ID, so resolving once at the control-plane boundary keeps
// every backend endpoint consistent with normal inference dispatch.
func (s *Server) resolveBackendModel(requested string) (string, config.ModelConfig, bool) {
	cfg := s.currentConfig()
	requested = strings.TrimPrefix(strings.TrimSpace(requested), "/")
	model, ok := cfg.RealModelName(requested)
	if !ok {
		return "", config.ModelConfig{}, false
	}
	modelConfig, ok := cfg.Models[model]
	if !ok {
		return "", config.ModelConfig{}, false
	}
	return model, modelConfig, true
}

var discoveredCapabilityNames = []string{
	"chat", "completions", "responses", "embeddings", "transcriptions", "translations",
	"speech", "images", "realtime", "anthropic", "embed", "rerank", "classify", "score", "pooling",
	"generative_scoring",
}

// effectiveBackendCapabilities merges safe discovery with explicit backend
// configuration. An explicit APIs list is an allowlist: every known
// capability omitted from it is set to false so discovery cannot re-enable an
// endpoint the operator deliberately disabled.
func effectiveBackendCapabilities(modelConfig config.ModelConfig, discovered *backend.Discovery) map[string]bool {
	explicit := backend.CapabilitySet{}
	if len(modelConfig.Backend.APIs) > 0 {
		for _, name := range discoveredCapabilityNames {
			explicit[name] = false
		}
		for _, name := range modelConfig.Backend.APIs {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				if name == "all" || name == "*" {
					for _, known := range discoveredCapabilityNames {
						explicit[known] = true
					}
					continue
				}
				explicit[name] = true
			}
		}
	}
	if modelConfig.Capabilities.Tools {
		explicit["tools"] = true
	}
	if modelConfig.Capabilities.Reranker {
		explicit["reranker"] = true
	}
	if discovered == nil {
		out := make(map[string]bool, len(explicit))
		for name, value := range explicit {
			out[name] = value
		}
		return out
	}
	merged := backend.MergeDiscovery(explicit, *discovered)
	out := make(map[string]bool, len(merged))
	for name, value := range merged {
		out[name] = value
	}
	return out
}

func (s *Server) backendStatusFor(ctx context.Context, model string, modelConfig config.ModelConfig) backendStatus {
	if ctx == nil {
		ctx = context.Background()
	}
	status := backendStatus{Model: model, Type: modelConfig.Backend.Type, Runtime: modelConfig.Backend.Runtime, Protocol: modelConfig.Backend.Protocol, APIs: append([]string(nil), modelConfig.Backend.APIs...), CacheState: "unknown"}
	if backendLifecycleConfigured(modelConfig.Backend.Lifecycle) {
		lifecycle := modelConfig.Backend.Lifecycle
		status.Lifecycle = &lifecycle
	}
	if backendResourcesConfigured(modelConfig.Backend.Resources) {
		resources := modelConfig.Backend.Resources
		resources.GPUAffinity = append([]string(nil), resources.GPUAffinity...)
		status.Resources = &resources
	}
	if modelConfig.Backend.Pricing.Provider != "" || modelConfig.Backend.Pricing.Model != "" {
		pricing := modelConfig.Backend.Pricing
		status.Pricing = &pricing
	}
	var discovered *backend.Discovery
	if modelConfig.Backend.Type != "" && (modelConfig.Backend.Discover == nil || *modelConfig.Backend.Discover) {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		value := backend.Discover(probeCtx, modelConfig.Proxy, nil)
		cancel()
		discovered = &value
	}
	status.Capabilities = effectiveBackendCapabilities(modelConfig, discovered)
	status.Discovery = discovered
	localSleeping := s.modelSleeping(model)
	if adapter := s.backendAdaptersSnapshot()[model]; !backend.IsNilAdapter(adapter) {
		if cacheState, cacheErr := adapter.CacheState(ctx); cacheErr == nil {
			cacheState = cacheState.Normalize()
			status.Cache = &cacheState
			if cacheState.Sleeping {
				status.CacheState = "sleeping"
			} else {
				status.CacheState = "awake"
			}
		}
	}
	if localSleeping {
		if status.Cache == nil {
			status.Cache = &backend.CacheState{Supported: true, Sleeping: true}
		} else {
			status.Cache.Supported = true
			status.Cache.Sleeping = true
		}
		status.CacheState = "sleeping"
	}
	if s.cacheController != nil {
		if report, found := s.cacheController.Report(model); found {
			reportCopy := report
			status.CacheReport = &reportCopy
		}
	}
	return status
}

func (s *Server) handleAPIBackends(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	models := make([]string, 0, len(cfg.Models))
	for model := range cfg.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	identity := identityFromContext(r.Context())
	data := make([]backendStatus, 0, len(models))
	for _, model := range models {
		if !modelAllowedForIdentity(cfg, identity, model) {
			continue
		}
		modelConfig := cfg.Models[model]
		data = append(data, s.backendStatusFor(r.Context(), model, modelConfig))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// handleAPIBackend serves the detail form of the backend control-plane API.
// The list endpoint remains unchanged, while this resource endpoint resolves
// aliases to the canonical model id so its cache/discovery data agrees with
// actions and progress queries.
func (s *Server) handleAPIBackend(w http.ResponseWriter, r *http.Request) {
	model, modelConfig, ok := s.resolveBackendModel(r.PathValue("model"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(s.currentConfig(), identity, model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": s.backendStatusFor(r.Context(), model, modelConfig)})
}

func backendLifecycleConfigured(value config.LifecycleConfig) bool {
	return strings.TrimSpace(value.Mode) != "" || value.SleepLevel != 0
}

func backendResourcesConfigured(value config.ResourceConfig) bool {
	return value.VRAMMiB != 0 || value.RAMMiB != 0 || value.Priority != 0 || value.EvictionPriority != 0 || len(value.GPUAffinity) > 0
}

// modelSleeping reads the local process-owned sleep projection when one is
// available. It is intentionally optional so servers embedding a custom
// LocalRouter keep working with adapter-only lifecycle control.
func (s *Server) modelSleeping(model string) bool {
	if s == nil || s.local == nil {
		return false
	}
	provider, ok := s.local.(interface{ ModelSleeping(string) bool })
	return ok && provider.ModelSleeping(model)
}

func (s *Server) sleepBackendModel(ctx context.Context, model string, level int) error {
	if s != nil && s.local != nil {
		if provider, ok := s.local.(interface {
			SleepModel(context.Context, string, int) error
		}); ok {
			err := provider.SleepModel(ctx, model, level)
			if !errors.Is(err, process.ErrSleepUnsupported) {
				return err
			}
		}
	}
	adapter := s.backendAdaptersSnapshot()[model]
	if backend.IsNilAdapter(adapter) {
		return backend.ErrUnsupported
	}
	return adapter.Sleep(ctx, level)
}

func (s *Server) wakeBackendModel(ctx context.Context, model string) error {
	if s != nil && s.local != nil {
		if provider, ok := s.local.(interface {
			WakeModel(context.Context, string) error
		}); ok {
			err := provider.WakeModel(ctx, model)
			if !errors.Is(err, process.ErrSleepUnsupported) {
				return err
			}
		}
	}
	adapter := s.backendAdaptersSnapshot()[model]
	if backend.IsNilAdapter(adapter) {
		return backend.ErrUnsupported
	}
	return adapter.Wake(ctx)
}

func (s *Server) handleAPIBackendAction(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.resolveBackendModel(r.PathValue("model"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	// Backend mutations are model-scoped control-plane operations.  The route
	// middleware authenticates the caller's scope, but a key may also carry an
	// explicit model allowlist; enforce that allowlist here before touching a
	// cache or changing the backend lifecycle.  The read-only endpoints perform
	// the same check in their handlers, so aliases and canonical IDs follow one
	// consistent policy.
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(s.currentConfig(), identity, model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
		return
	}
	adapter := s.backendAdaptersSnapshot()[model]
	resetCache := strings.HasSuffix(r.URL.Path, "/cache/reset")
	sleepAction := strings.HasSuffix(r.URL.Path, "/sleep")
	wakeAction := strings.HasSuffix(r.URL.Path, "/wake")
	if !resetCache && !sleepAction && !wakeAction {
		swaputil.SendResponse(w, r, http.StatusNotFound, "unknown backend action")
		return
	}
	localSleepProvider := false
	if s.local != nil {
		_, localSleepProvider = s.local.(interface {
			SleepModel(context.Context, string, int) error
			WakeModel(context.Context, string) error
		})
	}
	if (resetCache && (backend.IsNilAdapter(adapter) || s.cacheController == nil)) ||
		((sleepAction || wakeAction) && backend.IsNilAdapter(adapter) && !localSleepProvider) {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "backend action is not available for this adapter")
		return
	}
	if (sleepAction || wakeAction || resetCache) && s.backendActionBusy(model) {
		// Sleeping releases backend weights.  Calling it while a request or a
		// process transition is still active can invalidate the generation or
		// race the router's own load/unload path. Resetting a prefix cache has the
		// same safety boundary: an in-flight request may still be reading or
		// writing the cache that is about to be discarded. Expose a retryable
		// conflict instead of relying on whichever backend happens to reject the
		// request. This is intentionally a point-in-time check, not a drain
		// state machine (model restart remains owned by the separate task).
		swaputil.SendResponse(w, r, http.StatusConflict, "backend model "+model+" has in-flight or lifecycle activity")
		return
	}
	var err error
	phase := ""
	message := ""
	switch {
	case resetCache:
		phase = "cache_reset"
		message = "resetting backend prefix cache"
		backend.PublishProgress(swaputil.BackendProgressEvent{Model: model, Phase: phase, Progress: 0, Message: message})
		err = s.cacheController.Reset(r.Context(), model)
	case sleepAction:
		level := 1
		if raw := strings.TrimSpace(r.URL.Query().Get("level")); raw != "" {
			parsed, parseErr := strconv.Atoi(raw)
			if parseErr != nil || parsed < 1 || parsed > 2 {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "sleep level must be 1 or 2")
				return
			}
			level = parsed
		}
		phase = "sleep"
		message = "putting backend to sleep"
		backend.PublishProgress(swaputil.BackendProgressEvent{Model: model, Phase: phase, Progress: 0, Message: message})
		err = s.sleepBackendModel(r.Context(), model, level)
	case wakeAction:
		phase = "wake"
		message = "waking backend"
		backend.PublishProgress(swaputil.BackendProgressEvent{Model: model, Phase: phase, Progress: 0, Message: message})
		err = s.wakeBackendModel(r.Context(), model)
	default:
		swaputil.SendResponse(w, r, http.StatusNotFound, "unknown backend action")
		return
	}
	if err != nil {
		if phase != "" {
			backend.PublishProgress(swaputil.BackendProgressEvent{Model: model, Phase: "error", Progress: 0, Message: message, Error: err.Error()})
		}
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, backend.ErrUnsupported), errors.Is(err, process.ErrSleepUnsupported):
			status = http.StatusNotImplemented
		case errors.Is(err, process.ErrSleepUnavailable):
			status = http.StatusConflict
		case errors.Is(err, swaputil.ErrNoLocalModelFound):
			status = http.StatusNotFound
		}
		swaputil.SendResponse(w, r, status, err.Error())
		return
	}
	if phase != "" {
		backend.PublishProgress(swaputil.BackendProgressEvent{Model: model, Phase: phase, Progress: 1, Message: message + " completed"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"model": model, "ok": true})
}

// backendActionBusy provides a point-in-time safety check for destructive
// backend lifecycle operations.  The normal request pipeline records the
// canonical model id after selector resolution; aliases are still resolved
// defensively here because upstream passthroughs may carry a user spelling.
// This is intentionally a check (rather than a drain/restart state machine):
// the latter is owned by the separate model-restart implementation.
func (s *Server) backendActionBusy(model string) bool {
	if s == nil {
		return false
	}
	if s.inflight != nil {
		cfg := s.currentConfig()
		for _, request := range s.inflight.Current().Requests {
			requestModel := strings.TrimSpace(request.Model)
			if canonical, ok := cfg.RealModelName(requestModel); ok {
				requestModel = canonical
			}
			if requestModel == model {
				return true
			}
		}
	}
	if s.local != nil {
		if state, ok := s.local.RunningModels()[model]; ok && (state == process.StateStarting || state == process.StateStopping) {
			return true
		}
	}
	return false
}

func (s *Server) handleAPIBackendCapabilities(w http.ResponseWriter, r *http.Request) {
	model, cfg, ok := s.resolveBackendModel(r.PathValue("model"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(s.currentConfig(), identity, model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
		return
	}
	var discovered *backend.Discovery
	if cfg.Backend.Type != "" && (cfg.Backend.Discover == nil || *cfg.Backend.Discover) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		value := backend.Discover(ctx, cfg.Proxy, nil)
		cancel()
		discovered = &value
	}
	publicBackend, err := publicBackendConfig(cfg.Backend)
	if err != nil {
		// The client gets a deliberately generic 500, so the cause has to reach
		// the log — this path also covers credential redaction, and without the
		// error there is nothing for an operator to act on.
		s.proxylog.Errorf("serialize backend configuration for %s: %v", model, err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "could not serialize backend configuration")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"model": model, "capabilities": effectiveBackendCapabilities(cfg, discovered), "modelCapabilities": cfg.Capabilities, "backend": publicBackend, "discovery": discovered})
}

// publicBackendConfig returns a JSON-shaped backend definition suitable for a
// read-only runtime endpoint. BackendConfig is also used as a launch contract,
// so it may contain container environment values or argv credentials that must
// never be exposed to a runtime-read key. Reuse the same field-aware redactor
// as /api/config and additionally scrub secret-bearing command arguments.
func publicBackendConfig(value config.BackendConfig) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var public any
	if err := decoder.Decode(&public); err != nil {
		return nil, err
	}
	redactConfigJSON(public, "backend")
	redactBackendCommandSecrets(public)
	return public, nil
}

// redactBackendCommandSecrets handles the one secret shape that generic
// config redaction cannot identify: credentials embedded in argv values such
// as --api-key=... or passed as the argument immediately after --token.
// Preserve the option name so the UI can still explain the configured launch
// contract without returning the credential itself.
func redactBackendCommandSecrets(value any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range object {
		switch strings.ToLower(key) {
		case "args", "command", "entrypoint":
			if values, ok := child.([]any); ok {
				redactBackendArgList(values)
			}
		default:
			redactBackendCommandSecrets(child)
		}
	}
}

func redactBackendArgList(values []any) {
	redactNext := false
	for index, raw := range values {
		text, ok := raw.(string)
		if !ok {
			redactNext = false
			redactBackendCommandSecrets(raw)
			continue
		}
		if redactNext {
			values[index] = "[REDACTED]"
			redactNext = false
			continue
		}
		redacted, consumesNext := redactBackendArg(text)
		values[index] = redacted
		redactNext = consumesNext
	}
}

func redactBackendArg(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	for _, marker := range []string{
		"--api-key=", "--apikey=", "--token=", "--auth-token=", "--password=", "--secret=",
		"api_key=", "api-key=", "auth_token=", "token=", "password=", "secret=",
	} {
		if strings.HasPrefix(lower, marker) {
			leading := value[:len(value)-len(trimmed)]
			prefix := leading + trimmed[:len(marker)]
			return prefix + "[REDACTED]", false
		}
	}
	for _, marker := range []string{"--api-key", "--apikey", "--token", "--auth-token", "--password", "--secret", "api_key", "api-key", "auth_token", "token", "password", "secret"} {
		if lower == marker {
			return value, true
		}
	}
	if strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") {
		leading := value[:len(value)-len(strings.TrimLeft(value, " \t"))]
		separator := strings.IndexAny(trimmed, " \t")
		if separator > 0 {
			return leading + trimmed[:separator] + " [REDACTED]", false
		}
	}
	return value, false
}

func (s *Server) handleAPIBackendProgress(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.resolveBackendModel(r.PathValue("model"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(s.currentConfig(), identity, model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
		return
	}
	if progress, ok := s.backendProgressSnapshot(model); ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": model, "progress": progress})
		return
	}
	if adapter := s.backendAdaptersSnapshot()[model]; !backend.IsNilAdapter(adapter) {
		if progress, err := adapter.Progress(r.Context()); err == nil {
			progress = progress.Normalize()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"model": model, "progress": progress})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"model": model, "state": "idle", "progress": 1.0})
}

func (s *Server) handleAPIBackendCache(w http.ResponseWriter, r *http.Request) {
	model, _, ok := s.resolveBackendModel(r.PathValue("model"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(s.currentConfig(), identity, model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
		return
	}
	state := backend.CacheState{}
	if adapter := s.backendAdaptersSnapshot()[model]; !backend.IsNilAdapter(adapter) {
		value, err := adapter.CacheState(r.Context())
		if err != nil && !errors.Is(err, backend.ErrUnsupported) {
			swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
			return
		}
		if err == nil {
			state = value.Normalize()
		}
	}
	if s.modelSleeping(model) {
		state.Supported = true
		state.Sleeping = true
	}
	response := map[string]any{"model": model, "cache": state}
	if s.cacheController != nil {
		if report, found := s.cacheController.Report(model); found {
			response["report"] = report
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// runtimeLogResponse is the captured output of a runtime's most recent
// operation. Output is bounded: the manager keeps a per-runtime tail and the
// handler trims it again so a response can never exceed the SSE event cap.
type runtimeLogResponse struct {
	Runtime     string `json:"runtime"`
	OperationID string `json:"operationId,omitempty"`
	Output      string `json:"output"`
}

// sanitizeLogTail prepares a log tail for the browser's ANSI renderer. It
// strips NUL bytes and, when the tail was cut mid-file, drops the partial
// first line so the response never begins inside a split escape sequence.
// Unlike progress-event normalization it deliberately keeps escape
// sequences intact: the log panel renders them as terminal colors.
func sanitizeLogTail(output string, cutFromMiddle bool) string {
	output = strings.ReplaceAll(output, "\x00", "")
	if cutFromMiddle {
		if index := strings.IndexByte(output, '\n'); index >= 0 {
			output = output[index+1:]
		}
	}
	return output
}

// handleAPIRuntimeLogs serves the captured download/build/install output of a
// runtime's most recent operation. The SSE stream already delivers log chunks
// live; this endpoint gives a panel opened mid-build — or after it finished —
// the history those clients never received.
func (s *Server) handleAPIRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.runtime == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "runtime manager is unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if !runtimeAllowedForIdentity(cfg, identityFromContext(r.Context()), name) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for runtime "+name)
		return
	}
	operationID, output := s.runtime.RuntimeOperationLog(name)
	if output != "" {
		// Keep the tail: showing the oldest bytes of a long build instead of
		// the newest would defeat the panel.
		cutFromMiddle := false
		if len(output) > 24<<10 {
			output = output[len(output)-24<<10:]
			cutFromMiddle = true
		}
		output = sanitizeLogTail(output, cutFromMiddle)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(runtimeLogResponse{Runtime: name, OperationID: operationID, Output: output})
}
