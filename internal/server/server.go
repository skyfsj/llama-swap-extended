package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/logarchive"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/modeldownload"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/pricing"
	"github.com/mostlygeek/llama-swap/internal/process"
	resourcePlanner "github.com/mostlygeek/llama-swap/internal/resource"
	"github.com/mostlygeek/llama-swap/internal/route"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
	"github.com/mostlygeek/llama-swap/internal/settings"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Server owns the HTTP mux, cross-cutting middleware, and the local/peer model
// dispatch. It supersedes router.Server: it builds the local and peer routers
// directly and dispatches between them itself.
type Server struct {
	cfg   config.Config
	cfgMu sync.RWMutex

	muxlog      *logmon.Monitor
	proxylog    *logmon.Monitor
	upstreamlog *logmon.Monitor

	incidentMu              sync.Mutex
	incidentArchive         *logarchive.Store
	incidentArchiveDir      string
	incidentArchiveMaxFiles int
	incidentThrottle        map[string]time.Time

	perf     *perf.Monitor
	inflight *inflightTracker
	metrics  *metricsMonitor
	keys     *keyCache
	// auditDirty is the single-slot wake-up signal for the background
	// audit maintenance task; bursts coalesce into one pass.
	auditDirty         chan struct{}
	auditMaintMu       sync.Mutex
	auditLastMaint     time.Time
	auditLastGC        time.Time
	store              *store.Store
	extensionsMu       sync.RWMutex
	extensions         *extensions.Manager
	configManager      *config.ConfigManager
	settingsManager    *settings.Manager
	configReconciler   *ConfigReconciler
	reconcileMu        sync.Mutex
	daemonRestartPaths []string
	runtime            *runtimeManager.Manager
	modelFiles         *modelmanager.Manager
	downloads          *modeldownload.Manager
	modelFilesMu       sync.RWMutex
	pricing            *pricing.Syncer
	resources          *resourcePlanner.Planner
	resourcesMu        sync.RWMutex
	resourceMu         sync.Mutex
	lmcacheMod         *lmcacheService
	backendAdapters    map[string]backend.BackendAdapter
	backendAdapterMu   sync.RWMutex
	cacheController    *backend.CacheController
	progressMu         sync.RWMutex
	backendProgress    map[string]swaputil.BackendProgressEvent
	progressCancel     context.CancelFunc
	// processProgressCancel bridges the local process lifecycle into the
	// common BackendProgress stream. Keep this subscription separate from the
	// runtime/download progress listener so either can be stopped independently
	// during shutdown and tests can exercise the mapping in isolation.
	processProgressCancel context.CancelFunc
	build                 BuildInfo
	hardware              *hw.HardwareSnapshot

	profileMu     sync.RWMutex
	activeProfile string

	local router.LocalRouter
	peer  router.Router

	mux       *http.ServeMux
	handler   http.Handler
	handlerMu sync.RWMutex

	// modelChainHandler is the fully assembled inference chain (auth through
	// backend dispatch) rebuilt by every routes() pass. The extension debug
	// chat reuses it so a debugged conversation behaves exactly like a real
	// client request.
	modelChainHandler http.Handler
	// debugCaptures holds per-request extension activity buffers for the debug
	// chat; entries self-reclaim debugArtifactTTL after creation.
	debugCaptures sync.Map
	// extensionKVOnce gates the lazy per-process ephemeral KV used by ctx.kv
	// for extensions without the persistent storage permission.
	extensionKVOnce sync.Once
	extensionKV     *ephemeralKV
	// extensionLogs owns the per-extension log monitors and rolling store.
	extensionLogs *extensionLogRegistry

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	// controlPlaneActive is a short-lived activity gate for managed-runtime
	// updates. Runtime activation must wait until no authorized control-plane
	// operation (config, unload, cache, model-file, key, audit, or runtime
	// mutation) is still executing, in addition to the inference/process idle
	// checks below.
	controlPlaneActive atomic.Int64
}

// ActiveProfile returns the active runtime profile, or an empty string when no
// profile is active.
func (s *Server) ActiveProfile() string {
	s.profileMu.RLock()
	defer s.profileMu.RUnlock()
	return s.activeProfile
}

func (s *Server) currentConfig() config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) currentExtensions() *extensions.Manager {
	s.extensionsMu.RLock()
	defer s.extensionsMu.RUnlock()
	return s.extensions
}

func (s *Server) setConfig(cfg config.Config) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
}

// runtimeIdle is the shared inference-plane idle gate. LMCache adds its live
// model references and process transition states on top of this baseline via
// Manager.SetRuntimeIdleProbe.
func (s *Server) runtimeIdle(name string) bool {
	if s == nil {
		return false
	}
	return len(s.runtimeIdleReasons(name)) == 0
}

// ReconcileModelRestart is the confirmation seam used by the control plane.
// It intentionally does not start a process itself; the local router owns the
// generation fence, drain and rollback sequence on its run loop.
func (s *Server) ReconcileModelRestart(modelID string) error {
	if s == nil || s.configReconciler == nil {
		return errors.New("model restart is unavailable")
	}
	return s.configReconciler.RestartModel(modelID)
}

// ReconcileModelForceRestart confirms a force restart, removes its tracked
// requests from the control-plane view, and leaves process replacement to the
// router's normal Stop/EnsureReady/rollback lifecycle.
func (s *Server) ReconcileModelForceRestart(modelID string) error {
	if s == nil || s.configReconciler == nil {
		return errors.New("model restart is unavailable")
	}
	return s.configReconciler.ForceRestartModel(modelID, func(id string) {
		if s.inflight != nil {
			s.inflight.CancelModelAndRemove(id)
		}
	})
}

// refreshManagedRuntimeLaunchConfig publishes the current immutable bindings
// after startup seeded a bundled runtime. Normal activate/rollback operations
// use switchRuntimeProcesses so a running candidate is restarted and verified
// before the Manager commits the transition.
func (s *Server) refreshManagedRuntimeLaunchConfig(ctx context.Context) error {
	if s == nil {
		return errors.New("server is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, err := bindManagedRuntimeLaunchConfig(s.currentConfig(), s.hardware)
	if err != nil {
		return err
	}
	if s.local == nil {
		return nil
	}
	if local, ok := s.local.(interface{ Reconfigure(config.Config) error }); ok {
		if err := local.Reconfigure(bound); err != nil {
			return err
		}
		return ctx.Err()
	}
	return errors.New("local router cannot refresh managed runtime models")
}

func (s *Server) modelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus {
	if s == nil || s.local == nil {
		return nil
	}
	provider, ok := s.local.(interface {
		ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus
	})
	if !ok {
		return nil
	}
	return provider.ModelLifecycleStatuses()
}

// ReconcileConfig is the single entry point for file-watch, SIGHUP and Config
// API candidates. It publishes the long-lived Server modules in place and
// never closes the active store, download queue, listener or SSE machinery.
func (s *Server) ReconcileConfig(candidate config.Config) error {
	if s == nil {
		return errors.New("server is nil")
	}
	if s.configReconciler == nil {
		active := s.currentConfig()
		return s.applyConfigCandidate(active, candidate, config.Compare(active, candidate))
	}
	return s.configReconciler.Reconcile(candidate)
}

// reconfigureModelFileServices replaces the source catalog and its download
// queue as one control-plane operation. Model files are intentionally not part
// of the daemon-restart policy: users expect a corrected cache/model directory
// to appear in the UI after a successful config reload.
func (s *Server) reconfigureModelFileServices(active, desired config.ModelFilesConfig) error {
	if s == nil || reflect.DeepEqual(active.Effective(), desired.Effective()) {
		return nil
	}

	desiredConfig := desired.Effective()
	nextSources, err := modelmanager.New(desiredConfig)
	if err != nil {
		return fmt.Errorf("creating reconfigured model file manager: %w", err)
	}
	nextDownloads, err := modeldownload.New(modeldownload.Config{
		Store:    s.store,
		Sources:  nextSources,
		Settings: desiredConfig.Downloads,
		Logger:   s.proxylog,
		Progress: backend.PublishProgress,
	})
	if err != nil {
		return fmt.Errorf("creating reconfigured model download queue: %w", err)
	}

	// Hold the same lock used by the model-file and download handlers so no
	// request can enqueue against the old catalog while its workers are stopped.
	s.modelFilesMu.Lock()
	defer s.modelFilesMu.Unlock()
	previousSources, previousDownloads := s.modelFiles, s.downloads

	var restoreDownloads *modeldownload.Manager
	if previousDownloads != nil {
		activeConfig := active.Effective()
		restoreDownloads, err = modeldownload.New(modeldownload.Config{
			Store:    s.store,
			Sources:  previousSources,
			Settings: activeConfig.Downloads,
			Logger:   s.proxylog,
			Progress: backend.PublishProgress,
		})
		if err != nil {
			return fmt.Errorf("preparing previous model download queue: %w", err)
		}
		previousDownloads.Close()
	}
	if err := nextDownloads.Start(s.shutdownCtx); err != nil {
		if restoreDownloads != nil {
			if restoreErr := restoreDownloads.Start(s.shutdownCtx); restoreErr == nil {
				s.modelFiles = previousSources
				s.downloads = restoreDownloads
			} else {
				return errors.Join(
					fmt.Errorf("starting reconfigured model download queue: %w", err),
					fmt.Errorf("restoring previous model download queue: %w", restoreErr),
				)
			}
		}
		return fmt.Errorf("starting reconfigured model download queue: %w", err)
	}
	s.modelFiles = nextSources
	s.downloads = nextDownloads
	return nil
}

func (s *Server) applyConfigCandidate(active, desired config.Config, changes config.ConfigChangeSet) error {
	effective, restartPaths := config.ApplyDaemonRestartPolicy(active, desired)
	var nextExtensions *extensions.Manager
	if effective.Extensions.Enabled {
		var err error
		nextExtensions, err = extensions.NewManager(effective.Extensions.Directory)
		if err != nil {
			return fmt.Errorf("loading extensions: %w", err)
		}
	}
	activeRouterConfig, err := bindManagedRuntimeLaunchConfig(active, s.hardware)
	if err != nil {
		return fmt.Errorf("binding active managed runtime launch configuration: %w", err)
	}
	routerConfig, err := bindManagedRuntimeLaunchConfig(effective, s.hardware)
	if err != nil {
		return fmt.Errorf("binding managed runtime launch configuration: %w", err)
	}
	if err := s.syncManagedRuntimeDefinitions(active, effective); err != nil {
		if rollbackErr := s.syncManagedRuntimeDefinitions(effective, active); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("restore runtime definitions: %w", rollbackErr))
		}
		return err
	}
	// Mirror startup keys from the candidate config before the topology is
	// touched, so a hot reload that adds or changes startup_api_keys behaves
	// like startup: the durable key table and the auth snapshot converge
	// without a restart. Upserts are idempotent for unchanged entries.
	if !reflect.DeepEqual(active.StartupAPIKeys, effective.StartupAPIKeys) || !reflect.DeepEqual(active.RequiredAPIKeys, effective.RequiredAPIKeys) {
		if err := s.seedStartupAPIKeys(effective); err != nil {
			return fmt.Errorf("seeding startup api keys: %w", err)
		}
		if s.keys != nil {
			s.keys.refresh()
		}
	}
	// The LMCache manager follows its whole config section: version/policy
	// changes reconcile the derived runtime, server parameter changes
	// re-execute the supervised server, and disabling stops it. Failures are
	// logged rather than rejected so the model topology reload can proceed.
	if !reflect.DeepEqual(active.LMCache, effective.LMCache) {
		if syncErr := s.syncLMCacheModule(active, effective); syncErr != nil && s.proxylog != nil {
			s.proxylog.Warnf("lmcache module resync: %v", syncErr)
		}
	}
	rollbackRuntimeDefinitions := func() {
		if rollbackErr := s.syncManagedRuntimeDefinitions(effective, active); rollbackErr != nil && s.proxylog != nil {
			s.proxylog.Errorf("config candidate rejected after runtime apply; runtime definition rollback failed: %v", rollbackErr)
		}
	}
	localReconfigured := false
	local, localOK := s.local.(interface{ Reconfigure(config.Config) error })
	if localOK {
		if err := local.Reconfigure(routerConfig); err != nil {
			rollbackRuntimeDefinitions()
			return err
		}
		localReconfigured = true
	}
	if peer, ok := s.peer.(interface{ Reconfigure(config.Config) error }); ok {
		if err := peer.Reconfigure(effective); err != nil {
			// The local router is updated first because it owns the process
			// generation fence. If peer topology validation fails afterward, put
			// the local planner/config back before reporting the candidate invalid;
			// otherwise a rejected config would still change the running topology.
			if localReconfigured && localOK {
				if rollbackErr := local.Reconfigure(activeRouterConfig); rollbackErr != nil && s.proxylog != nil {
					s.proxylog.Errorf("config candidate rejected after local apply; local rollback failed: %v", rollbackErr)
				}
			}
			rollbackRuntimeDefinitions()
			return err
		}
	}
	if err := s.reconfigureModelFileServices(active.ModelFiles, effective.ModelFiles); err != nil {
		if peer, ok := s.peer.(interface{ Reconfigure(config.Config) error }); ok {
			if rollbackErr := peer.Reconfigure(active); rollbackErr != nil && s.proxylog != nil {
				s.proxylog.Errorf("config candidate rejected after model-file reload; peer rollback failed: %v", rollbackErr)
			}
		}
		if localReconfigured && localOK {
			if rollbackErr := local.Reconfigure(activeRouterConfig); rollbackErr != nil && s.proxylog != nil {
				s.proxylog.Errorf("config candidate rejected after model-file reload; local rollback failed: %v", rollbackErr)
			}
		}
		rollbackRuntimeDefinitions()
		return err
	}

	s.setConfig(effective)
	s.extensionsMu.Lock()
	previousExtensions := s.extensions
	s.extensions = nextExtensions
	s.extensionsMu.Unlock()
	if nextExtensions != nil {
		nextExtensions.StartWatch(s.shutdownCtx)
	}
	if previousExtensions != nil {
		previousExtensions.Close()
	}
	// The router replaced or created model processes during Reconfigure above,
	// while the previous configuration was still the current one, so the
	// decoration ran against a stale snapshot and skipped models introduced by
	// this candidate (the LMCache pre-start gate in particular). Run the
	// decoration again against the committed configuration.
	s.installModelTracking()
	s.updateResourceBudget(effective.ResourceBudget)
	s.reconcileMu.Lock()
	s.daemonRestartPaths = append([]string(nil), restartPaths...)
	s.reconcileMu.Unlock()
	if s.metrics != nil {
		s.metrics.configureAudit(effective.EffectiveAudit())
		s.metrics.configurePricing(s.store, effective)
	}
	if s.perf != nil {
		s.perf.UpdateConfig(effective.Performance)
	}
	s.refreshBackendAdapters(effective)
	s.routes()
	event.Emit(swaputil.ConfigFileChangedEvent{State: swaputil.ReloadingStateEnd})
	return nil
}

func (s *Server) resourcePlanner() *resourcePlanner.Planner {
	if s == nil {
		return nil
	}
	s.resourcesMu.RLock()
	planner := s.resources
	s.resourcesMu.RUnlock()
	return planner
}

func (s *Server) updateResourceBudget(budget config.ResourceBudgetConfig) {
	if s == nil {
		return
	}
	s.resourcesMu.Lock()
	if s.resources == nil {
		s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: budget.VRAMMiB, RAMMiB: budget.RAMMiB})
	} else {
		s.resources.SetBudget(resourcePlanner.Budget{VRAMMiB: budget.VRAMMiB, RAMMiB: budget.RAMMiB})
	}
	s.resourcesMu.Unlock()
}

func (s *Server) refreshBackendAdapters(cfg config.Config) {
	adapters := make(map[string]backend.BackendAdapter, len(cfg.Models))
	for modelID, modelConfig := range cfg.Models {
		adapter, err := newModelBackendAdapter(modelID, modelConfig)
		if err != nil {
			if s.proxylog != nil {
				s.proxylog.Warnf("backend adapter unavailable for %s: %v", modelID, err)
			}
			continue
		}
		if backend.IsNilAdapter(adapter) {
			continue
		}
		adapters[modelID] = adapter
		if s.cacheController != nil {
			s.cacheController.Register(modelID, adapter)
		}
	}
	s.backendAdapterMu.Lock()
	s.backendAdapters = adapters
	s.backendAdapterMu.Unlock()
}

// setActiveProfile updates the runtime selection. An empty name deactivates
// profiles. It returns whether the selection changed.
func (s *Server) setActiveProfile(name string) (bool, error) {
	cfg := s.currentConfig()
	if name != "" {
		if _, ok := cfg.Profiles[name]; !ok {
			return false, fmt.Errorf("profile %q not found", name)
		}
	}
	s.profileMu.Lock()
	if s.activeProfile == name {
		s.profileMu.Unlock()
		return false, nil
	}
	s.activeProfile = name
	s.profileMu.Unlock()

	s.proxylog.Infof("active profile changed to %q", name)
	event.Emit(swaputil.ProfileChangedEvent{Active: name})
	return true, nil
}

// Standard model-dispatched routes are generated from RouteDescriptor. Keep
// only compatibility endpoints that are intentionally outside the registry
// (Stable Diffusion, audio.cpp, and /props) in the small lists below. This
// makes the descriptor registry the source of truth for every vLLM family and
// keeps metrics/model extraction in lockstep with mux registration.
var (
	modelPostJSONRoutes = appendRoutePaths(
		routeModelPaths(http.MethodPost, route.BodyJSON, "body"),
		"/v1/audio/voices", // legacy POST accepted by older audio servers
		"/sdapi/v1/txt2img", "/sdapi/v1/img2img",
		"/audioapi/v1/tasks/run",
		// Versionless aliases are retained for compatibility with older clients;
		// stripVersionPrefix removes /v before forwarding upstream.
		"/v/completions", "/v/messages", "/v/messages/count_tokens",
		"/v/embeddings", "/v/rerank", "/v/reranking",
	)
	modelPostFormRoutes = appendRoutePaths(
		routeModelPaths(http.MethodPost, route.BodyForm, "form"),
	)
	modelGetRoutes = appendRoutePaths(
		routeModelPaths(http.MethodGet, route.BodyQuery, "query"),
		"/v1/realtime", "/v/realtime", "/sdapi/v1/loras", "/props",
	)
)

func routeModelPaths(method string, body route.BodyKind, location string) []string {
	registry, err := route.NewRegistry(route.DefaultDescriptors())
	if err != nil {
		// DefaultDescriptors is compiled into the binary. Returning no generated
		// paths is safer than panicking during package initialization; the route
		// package tests make malformed descriptors visible during development.
		return nil
	}
	paths := make([]string, 0)
	for _, descriptor := range registry.List() {
		if !strings.EqualFold(descriptor.Method, method) || descriptor.Body != body || descriptor.ModelLocation != location || descriptor.ModelField == "" {
			continue
		}
		// Parameterized response retrieval/cancel routes carry no model field;
		// they are handled by the affinity API rather than model dispatch.
		if strings.Contains(descriptor.Pattern, "{") {
			continue
		}
		paths = append(paths, descriptor.Pattern)
	}
	return paths
}

func appendRoutePaths(paths []string, extras ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(extras))
	result := make([]string, 0, len(paths)+len(extras))
	for _, path := range append(paths, extras...) {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

// isMetricsRecordPath reports whether path is one of the model-dispatched
// endpoints that the metrics middleware records in the activity log.
func isMetricsRecordPath(path string) bool {
	for _, p := range modelPostJSONRoutes {
		if p == path {
			return true
		}
	}
	for _, p := range modelPostFormRoutes {
		if p == path {
			return true
		}
	}
	for _, p := range modelGetRoutes {
		if p == path {
			return true
		}
	}
	return false
}

// BuildInfo carries version metadata surfaced by GET /api/version.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

func New(cfg config.Config, muxlog *logmon.Monitor, proxylog *logmon.Monitor, upstreamlog *logmon.Monitor, perfMon *perf.Monitor, st *store.Store, build BuildInfo, hardware *hw.HardwareSnapshot) (*Server, error) {
	var extensionManager *extensions.Manager
	if cfg.Extensions.Enabled {
		var err error
		extensionManager, err = extensions.NewManager(cfg.Extensions.Directory)
		if err != nil {
			return nil, fmt.Errorf("loading extensions: %w", err)
		}
	}
	routerConfig, err := bindManagedRuntimeLaunchConfig(cfg, hardware)
	if err != nil {
		return nil, fmt.Errorf("binding managed runtime launch configuration: %w", err)
	}
	var local router.LocalRouter

	switch routerConfig.Routing.Router.Use {
	case "matrix":
		local, err = router.NewMatrix(routerConfig, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating matrix router: %w", err)
		}
	case "gpus":
		local, err = router.NewGpus(routerConfig, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating gpus router: %w", err)
		}
	default: // "group"
		local, err = router.NewGroup(routerConfig, proxylog, upstreamlog)
		if err != nil {
			return nil, fmt.Errorf("creating group router: %w", err)
		}
	}

	peer, err := router.NewPeer(cfg, proxylog)
	if err != nil {
		return nil, fmt.Errorf("creating peer router: %w", err)
	}

	if st == nil {
		return nil, fmt.Errorf("store is required")
	}
	if err := st.ResetExtensionPendingClaims(context.Background()); err != nil {
		return nil, fmt.Errorf("resetting extension continuation claims: %w", err)
	}
	manager, managerErr := runtimeManager.NewManager(managedRuntimeRoot(cfg), runtimeProvidersForConfig(cfg))
	if managerErr != nil {
		return nil, fmt.Errorf("creating runtime manager: %w", managerErr)
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	s := &Server{
		cfg:             cfg,
		muxlog:          muxlog,
		proxylog:        proxylog,
		upstreamlog:     upstreamlog,
		perf:            perfMon,
		inflight:        newInflightTracker(),
		metrics:         newMetricsMonitor(proxylog, cfg.MetricsMaxInMemory, st),
		store:           st,
		extensions:      extensionManager,
		runtime:         manager,
		build:           build,
		hardware:        hardware,
		local:           local,
		peer:            peer,
		shutdownCtx:     shutdownCtx,
		shutdownFn:      shutdownFn,
		backendProgress: make(map[string]swaputil.BackendProgressEvent),
	}
	s.configReconciler = NewConfigReconciler(cfg,
		func(active, desired config.Config, changes config.ConfigChangeSet) error {
			return s.applyConfigCandidate(active, desired, changes)
		},
		func(modelID string) error {
			if local, ok := s.local.(interface{ RestartModel(string) error }); ok {
				return local.RestartModel(modelID)
			}
			return configRestartUnavailableError{}
		},
	)
	s.configReconciler.SetForceRestart(func(modelID string) error {
		if local, ok := s.local.(interface{ ForceRestartModel(string) error }); ok {
			return local.ForceRestartModel(modelID)
		}
		return configRestartUnavailableError{}
	})
	s.configReconciler.SetRevisionPublisher(func(appliedRevision, desiredRevision uint64) {
		if local, ok := s.local.(interface {
			SetConfigRevisions(uint64, uint64)
		}); ok {
			local.SetConfigRevisions(appliedRevision, desiredRevision)
		}
	})
	s.inflight.SetProgressPublisher(backend.PublishProgress)
	// Runtime, model-download and backend lifecycle operations all publish the
	// same event envelope. Keep only the latest event per model so the point
	// query endpoint remains useful between SSE updates without turning the
	// control plane into an unbounded event store.
	s.progressCancel = event.On(func(progress swaputil.BackendProgressEvent) {
		s.recordBackendProgress(progress)
	})
	s.processProgressCancel = event.On(func(change swaputil.ProcessStateChangeEvent) {
		s.recordProcessProgress(change)
		s.releaseLMCacheReference(change)
	})
	if err := s.seedStartupAPIKeys(cfg); err != nil {
		return nil, err
	}
	// Authentication state must be in memory before the first request. The
	// load is synchronous and fails startup on error: a server that cannot
	// read its key table must not guess whether it is anonymous or locked.
	// From this point on every auth decision reads the snapshot; a storage
	// failure degrades to the last known snapshot instead of fail-closing.
	s.keys = newKeyCache(st, proxylog)
	if err := s.keys.load(); err != nil {
		return nil, fmt.Errorf("load api key cache: %w", err)
	}
	// Audit maintenance runs off the request path. Persistence signals the
	// task instead of purging inline; a single slot coalesces write bursts.
	s.auditDirty = make(chan struct{}, 1)
	s.metrics.dirtyAudit = s.signalAuditDirty
	modelFilesConfig := cfg.ModelFiles.Effective()
	modelFiles, modelFilesErr := modelmanager.New(modelFilesConfig)
	if modelFilesErr != nil {
		return nil, fmt.Errorf("creating model file manager: %w", modelFilesErr)
	}
	s.modelFiles = modelFiles
	downloads, downloadsErr := modeldownload.New(modeldownload.Config{
		Store:    st,
		Sources:  modelFiles,
		Settings: modelFilesConfig.Downloads,
		Logger:   proxylog,
		Progress: backend.PublishProgress,
	})
	if downloadsErr != nil {
		return nil, fmt.Errorf("creating model download queue: %w", downloadsErr)
	}
	if downloadsErr := downloads.Start(s.shutdownCtx); downloadsErr != nil {
		return nil, fmt.Errorf("starting model download queue: %w", downloadsErr)
	}
	s.downloads = downloads
	if cfg.ResourceBudget.Enabled() {
		s.resources = resourcePlanner.NewPlanner(resourcePlanner.Budget{VRAMMiB: cfg.ResourceBudget.VRAMMiB, RAMMiB: cfg.ResourceBudget.RAMMiB})
	}
	s.backendAdapters = make(map[string]backend.BackendAdapter)
	s.cacheController = backend.NewCacheController()
	s.metrics.setCacheObserver(s.cacheController.Observe)
	for modelID, modelConfig := range cfg.Models {
		adapter, adapterErr := newModelBackendAdapter(modelID, modelConfig)
		if adapterErr != nil {
			if proxylog != nil {
				proxylog.Warnf("backend adapter unavailable for %s: %v", modelID, adapterErr)
			}
			continue
		}
		if backend.IsNilAdapter(adapter) {
			continue
		}
		s.backendAdapters[modelID] = adapter
		s.cacheController.Register(modelID, adapter)
	}
	s.metrics.configureAudit(cfg.EffectiveAudit())
	s.metrics.configurePricing(st, cfg)
	pricingConfig := cfg.EffectivePricing().ModelsDev
	if pricingConfig.Enabled {
		s.pricing = &pricing.Syncer{Store: st, URL: pricingConfig.URL}
		// Populate the last-successful snapshot as soon as the control plane is
		// ready; the periodic loop then refreshes it using ETag validation.
		go func() {
			ctx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Minute)
			if _, syncErr := s.pricing.Sync(ctx); syncErr != nil && s.proxylog != nil {
				s.proxylog.Warnf("initial pricing sync failed: %v", syncErr)
			}
			cancel()
		}()
		go s.runPricingSync(pricingConfig.RefreshEvery)
	}
	manager.SetActivationHooks(s.switchRuntimeBefore, s.switchRuntimeProcesses)
	manager.SetVersionProtectionProbe(s.protectedRuntimeVersion)
	manager.SetProgress(backend.PublishProgress)
	manager.SetOperationSink(func(operation runtimeManager.Operation) {
		// Runtime Manager's JSONL journal is fsynced before this callback;
		// SQLite is a query-friendly projection. Keep this write bounded and
		// independent from the manager so a sink failure never changes the
		// runtime journal's recovery semantics.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := st.UpsertRuntimeOperation(ctx, store.RuntimeOperation{
			ID:          operation.ID,
			RuntimeName: operation.Name,
			Action:      operation.Action,
			State:       string(operation.State),
			Version:     operation.Version,
			Error:       operation.Error,
			StartedAt:   operation.Timestamp,
			UpdatedAt:   operation.Timestamp,
		})
		if err != nil && proxylog != nil {
			proxylog.Warnf("persist runtime operation %s: %v", operation.ID, err)
		}
	})
	manager.SetBuildWhileBusy(cfg.RuntimeManager.BuildWhileBusy)
	manager.SetIdleProbe(func() bool {
		return s.runtimeIdle("")
	})
	seededRuntime := false
	if seeded, discoverErr := registerDiscoveredBundledLlamaCPP(manager, cfg); discoverErr != nil {
		if proxylog != nil {
			proxylog.Warnf("discovered bundled llama.cpp registration failed: %v", discoverErr)
		}
	} else if seeded {
		seededRuntime = true
		if proxylog != nil {
			proxylog.Infof("llamacpp runtime registered from bundled image asset")
		}
	}
	for name, runtimeConfig := range cfg.Runtimes {
		if registerErr := manager.Register(name, runtimeConfig.Kind); registerErr != nil && proxylog != nil {
			proxylog.Warnf("runtime %s registration failed: %v", name, registerErr)
			continue
		}
		if runtimeConfig.Kind == "" {
			continue
		}
		runtimeSpec := runtimeSpecFromConfig(name, runtimeConfig)
		if configureErr := manager.Configure(name, runtimeSpec, runtimePolicyFromConfig(runtimeConfig.Update)); configureErr != nil && proxylog != nil {
			proxylog.Warnf("runtime %s automatic update configuration failed: %v", name, configureErr)
		}
		// A bundled runtime is an image seed, not an update source. Copy it
		// only when the persistent current pointer is absent; an image upgrade
		// must never replace an operator-managed current runtime. Source.Path
		// is intentionally required so a missing image asset is observable
		// rather than interpreted as a path relative to the process cwd.
		if strings.EqualFold(strings.TrimSpace(runtimeConfig.Source.Type), "bundled") && strings.TrimSpace(runtimeConfig.Source.Path) != "" {
			if _, seeded, seedErr := manager.SeedIfMissing(context.Background(), runtimeSpec); seedErr != nil {
				if proxylog != nil {
					proxylog.Warnf("runtime %s bundled seed failed: %v", name, seedErr)
				}
			} else if seeded {
				seededRuntime = true
				if proxylog != nil {
					proxylog.Infof("runtime %s seeded from bundled image asset", name)
				}
			}
		}
	}
	// The local router is created before bundled assets are seeded. Refresh
	// its private launch snapshot once so a newly created current pointer,
	// especially a container image digest, is visible before the first
	// request starts a process.
	if seededRuntime {
		if bindErr := s.refreshManagedRuntimeLaunchConfig(context.Background()); bindErr != nil && proxylog != nil {
			proxylog.Warnf("refresh managed runtime launch configuration after bundled seed: %v", bindErr)
		}
	}
	s.lmcacheMod = newLMCacheService(s)
	// LMCache is a derived managed runtime. Register/configure it during every
	// daemon boot so persisted current/previous/staged state is addressable by
	// the update loop instead of being lost until the next config reload.
	if cfg.LMCache.Enabled {
		if registerErr := manager.Register(config.LMCacheRuntimeName, "lmcache"); registerErr != nil {
			if proxylog != nil {
				proxylog.Warnf("lmcache runtime registration failed: %v", registerErr)
			}
		} else if configureErr := manager.Configure(config.LMCacheRuntimeName, lmcacheRuntimeSpec(cfg), lmcacheRuntimePolicy(cfg.LMCache.Update)); configureErr != nil && proxylog != nil {
			proxylog.Warnf("lmcache runtime configuration failed: %v", configureErr)
		}
	} else if unconfigureErr := manager.Unconfigure(config.LMCacheRuntimeName); unconfigureErr != nil && proxylog != nil {
		proxylog.Warnf("lmcache runtime unconfiguration failed: %v", unconfigureErr)
	}
	manager.SetRuntimeIdleProbe(config.LMCacheRuntimeName, func() bool {
		return s.runtimeIdle(config.LMCacheRuntimeName)
	})
	// Install the LMCache dependency tracker before the auto-start below and
	// before any model process can exist, so a boot-time stop decision can
	// never miss an in-flight model start.
	s.installModelTracking()
	if cfg.LMCache.Enabled && cfg.LMCache.Server.Enabled && cfg.LMCache.EffectiveAutoStart() {
		// Bootstrap and readiness are asynchronous so daemon startup remains
		// available while uv/torch work is in progress. The model gate reuses
		// the same ensure flow when autoStart is false or boot installation fails.
		go func() {
			ctx, cancel := context.WithTimeout(s.shutdownCtx, 30*time.Minute)
			defer cancel()
			if _, ensureErr := s.lmcacheMod.ensureServerRuntime(ctx, cfg); ensureErr != nil {
				if proxylog != nil {
					proxylog.Warnf("lmcache server runtime bootstrap: %v", ensureErr)
				}
				return
			}
			if startErr := s.lmcacheMod.ensureServerRunning(ctx, cfg); startErr != nil && proxylog != nil {
				proxylog.Warnf("lmcache server auto-start: %v", startErr)
			}
		}()
	}
	go manager.RunAutoUpdate(s.shutdownCtx)
	go s.keys.runRefresh(s.shutdownCtx)
	if s.extensions != nil {
		s.extensions.StartWatch(s.shutdownCtx)
	}
	s.routes()
	s.startPreload()
	go s.runResponseAffinityCleanup()
	go s.runAuditMaintenance()
	go s.runKeyTouchFlusher()
	go s.runExtensionPendingCleanup()
	return s, nil
}

// newModelBackendAdapter maps the explicit backend block to the narrow
// BackendAdapter boundary used by the control plane. HTTP-backed vLLM and
// llama.cpp runtimes get lifecycle/cache probes; an explicitly declared
// generic backend still participates in the registry with a no-op passthrough
// adapter so its declared capabilities and progress/authz surface are not
// silently omitted from /api/backends. Models without a backend block retain
// the legacy cmd/cmdStop/proxy path and therefore intentionally return nil.
func newModelBackendAdapter(modelID string, modelConfig config.ModelConfig) (backend.BackendAdapter, error) {
	switch strings.ToLower(strings.TrimSpace(modelConfig.Backend.Type)) {
	case "vllm", "llamacpp":
		return backend.NewHTTPAdapter(modelID, modelConfig.Proxy)
	case "generic":
		caps := make(backend.CapabilitySet, len(modelConfig.Backend.APIs))
		for _, capability := range modelConfig.Backend.APIs {
			capability = strings.ToLower(strings.TrimSpace(capability))
			if capability == "" {
				continue
			}
			if capability == "all" || capability == "*" {
				for _, descriptor := range route.DefaultDescriptors() {
					if descriptor.Capability != "" {
						caps[descriptor.Capability] = true
					}
				}
				continue
			}
			caps[capability] = true
		}
		return backend.Passthrough{ID: modelID, Caps: caps}, nil
	default:
		return nil, nil
	}
}

// localPeerHandler dispatches a model-routed request to the local or peer
// router. The model is resolved once via swaputil.FetchContext.
func (s *Server) localPeerHandler(w http.ResponseWriter, r *http.Request) {
	s.localPeerHandlerWithConfig(w, r, s.currentConfig())
}

// localPeerHandlerWithConfig is bound into a generated HTTP handler with the
// config snapshot used to build that handler. Requests that already crossed
// the old handler boundary therefore keep their parsing/selection semantics
// while a concurrent reconciliation atomically publishes the next handler.
func (s *Server) localPeerHandlerWithConfig(w http.ResponseWriter, r *http.Request, cfg config.Config) {
	restoreResponsesPath := rewriteResponsesChatBackendPath(r)
	defer restoreResponsesPath()
	stripVersionPrefix(r)
	stripAudioAPIPrefix(r)

	data, err := swaputil.FetchContext(r, cfg)
	if err != nil {
		// Preserve the dedicated 413 body-size error instead of collapsing it
		// into the legacy 404 "no model" envelope. FetchContext already
		// classifies malformed/missing model requests as ErrNoModelInContext,
		// so this keeps historical status codes while making oversized input
		// observable to clients.
		swaputil.SendError(w, r, err)
		return
	}
	if err := s.prepareResourceLoadContext(r.Context(), data.ModelID); err != nil {
		// 507 is intentionally explicit: the request was valid, but the
		// configured local resource budget could not be satisfied without
		// touching an in-flight/protected model. Callers may retry after an
		// unload or budget change.
		swaputil.SendResponse(w, r, http.StatusInsufficientStorage, err.Error())
		return
	}
	// Admission reservations cover the small window before the local process
	// scheduler publishes Starting/Ready. Release one reference when dispatch
	// returns; the planner will continue accounting for a model that remains
	// loaded, while failed/canceled cold starts do not leak reserved capacity.
	defer s.releaseResourceReservation(data.ModelID)

	switch {
	case s.local.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using local process for model: %s", data.ModelID)
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(data.ModelID):
		s.proxylog.Debugf("dispatch: using peer for model: %s", data.ModelID)
		s.peer.ServeHTTP(w, r)
	default:
		swaputil.SendError(w, r, router.ErrNoRouterFound)
	}
}

// stripVersionPrefix rewrites versionless /v/... requests to their /... form
// before forwarding upstream (issue #728).
func stripVersionPrefix(r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v/") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v")
	}
}

// stripAudioAPIPrefix rewrites /audioapi/... requests to their /... form
// before forwarding upstream, so /audioapi/v1/tasks/run reaches the upstream
// as /v1/tasks/run.
func stripAudioAPIPrefix(r *http.Request) {
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/audioapi")
}

// lookupIdentity resolves one presented credential through the in-memory key
// cache when available, falling back to the direct store lookup for servers
// constructed without a cache.
func (s *Server) lookupIdentity(ctx context.Context, cfg config.Config, provided string) (auth.Identity, bool, error) {
	if s.keys != nil {
		identity, valid := s.keys.validate(provided, cfg)
		return identity, valid, nil
	}
	return lookupAPIIdentity(ctx, cfg, s.store, provided)
}

// authConfigured reports whether authentication is enforced for the current
// configuration and key table, as seen by the in-memory cache.
func (s *Server) authConfigured() bool {
	cfg := s.currentConfig()
	return len(cfg.RequiredAPIKeys) > 0 || s.keys.configured()
}

// refreshKeyCache re-reads the key table immediately after a management
// mutation so revocations and creations take effect on the next request
// instead of waiting for the periodic refresh.
func (s *Server) refreshKeyCache() {
	if s.keys != nil {
		s.keys.refresh()
	}
}

// routes builds the mux, registers every route, and wraps the mux with the
// global CORS middleware.
func (s *Server) routes() {
	cfg := s.currentConfig()

	// Extension logging needs a registry even when logging storage is off:
	// the live stream works regardless of persistence settings.
	if s.extensionLogs == nil {
		s.extensionLogs = newExtensionLogRegistry()
		// The rolling store's retention prune runs hourly; the interval is
		// deliberately coarser than the per-request appends. Tied to the
		// server's shutdown context when one exists.
		if s.shutdownCtx != nil {
			stop := make(chan struct{})
			go func() {
				<-s.shutdownCtx.Done()
				close(stop)
			}()
			newExtensionLogRotation(extensionLogRoot(&cfg), cfg.Extensions).startPruner(stop, time.Hour)
		}
	}

	// Embedders and tests construct a Server without going through New. Give
	// any such server that has a store a key cache too, so authentication
	// never depends on the per-request store probe.
	if s.keys == nil && s.store != nil {
		s.keys = newKeyCache(s.store, s.proxylog)
		if err := s.keys.load(); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("initial api key cache load failed: %v", err)
		}
	}

	authMW := CreateScopedAuthMiddleware(cfg, s.store, s.keys)
	controlActivity := CreateControlActivityMiddleware(&s.controlPlaneActive)
	modelMiddlewares := []chain.Middleware{
		authMW,
		CreateRealtimeMiddleware(cfg),
		RequireScope("inference"),
	}
	extensionManager := s.currentExtensions()
	if cfg.Extensions.Enabled && extensionManager != nil {
		modelMiddlewares = append(modelMiddlewares, CreateExtensionOriginMiddleware(s), CreateExtensionContinuationMiddleware(s, cfg))
	}
	modelMiddlewares = append(modelMiddlewares,
		CreateProfileMiddleware(s),
		CreateSelectorMiddleware(s),
		CreateRequestContextMiddleware(cfg),
	)
	if cfg.Extensions.Enabled && extensionManager != nil {
		modelMiddlewares = append(modelMiddlewares,
			CreateInflightMiddleware(s.inflight, cfg),
			CreateMetricsMiddleware(s.metrics, cfg),
			CreateExtensionsMiddleware(s, cfg, extensionManager),
		)
	}
	modelMiddlewares = append(modelMiddlewares,
		CreateBackendCapabilityMiddleware(cfg),
		CreateAnthropicCacheMiddleware(cfg),
		CreateResponsesAdapterMiddleware(cfg, s.store),
		CreateResponseAffinityMiddleware(cfg, s.store),
	)
	if cfg.Extensions.Enabled && extensionManager != nil {
		modelMiddlewares = append(modelMiddlewares, CreateFilterMiddleware(cfg), CreateFormFilterMiddleware(cfg))
	} else {
		modelMiddlewares = append(modelMiddlewares,
			CreateInflightMiddleware(s.inflight, cfg),
			CreateFilterMiddleware(cfg),
			CreateFormFilterMiddleware(cfg),
			CreateMetricsMiddleware(s.metrics, cfg),
		)
	}
	if cfg.Extensions.Enabled && extensionManager != nil {
		modelMiddlewares = append(modelMiddlewares, CreateExtensionsBeforeForwardMiddleware())
	}
	modelMiddlewares = append(modelMiddlewares,
		CreateBackendAdapterMiddleware(cfg, s.backendAdaptersSnapshot()),
	)
	modelChain := chain.New(modelMiddlewares...)
	// Custom endpoints use the narrowest scope that matches their operation.
	inferenceAPI := chain.New(authMW, RequireScope("inference"))
	logsAPI := chain.New(authMW, RequireScope("logs"))
	auditAPI := chain.New(authMW, RequireScope("audit-read"))
	configAPI := chain.New(authMW, RequireScope("config-admin"), controlActivity)
	keysAPI := chain.New(authMW, RequireScope("keys-admin"), controlActivity)
	auditAdminAPI := chain.New(authMW, RequireScope("audit-admin"), controlActivity)
	auditReadAPI := chain.New(authMW, RequireScope("audit-read"))
	runtimeReadAPI := chain.New(authMW, RequireScope("runtime-read"))
	// Runtime Manager operations are serialized by Manager.mu themselves. Do
	// not count the request currently invoking Stage/Activate/Rollback as a
	// separate control-plane activity: the idle probe is evaluated inside that
	// same operation, and counting this request would make every runtime
	// operation reject itself as "busy". Other control-plane chains still gate
	// runtime activation through controlActivity.
	runtimeAdminAPI := chain.New(authMW, RequireScope("runtime-admin"))
	// Model file inspection/deletion is part of the runtime/config control
	// plane. Keep the public scope vocabulary fixed while still protecting the
	// potentially destructive unlink operation.
	modelFilesReadAPI := chain.New(authMW, RequireScope(auth.ScopeRuntimeRead))
	modelFilesAdminAPI := chain.New(authMW, RequireScope(auth.ScopeConfigAdmin), controlActivity)
	cacheAdminAPI := chain.New(authMW, RequireScope("cache-admin"), controlActivity)
	modelUnloadAPI := chain.New(authMW, RequireScope("model-unload"), controlActivity)

	mux := http.NewServeMux()
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.localPeerHandlerWithConfig(w, r, cfg)
	})
	s.modelChainHandler = modelChain.Then(dispatch)

	for _, path := range modelPostJSONRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelPostFormRoutes {
		mux.Handle("POST "+path, modelChain.Then(dispatch))
	}
	for _, path := range modelGetRoutes {
		mux.Handle("GET "+path, modelChain.Then(dispatch))
	}
	// Responses retrieval/cancellation carry the response id in the path, so
	// they cannot use the model-in-body dispatcher.  The affinity record
	// supplies the model for a native backend and lets the chat adapter serve a
	// stored response after a restart.
	mux.Handle("GET /v1/responses/{response_id}", inferenceAPI.ThenFunc(s.handleAPIResponseGet))
	mux.Handle("POST /v1/responses/{response_id}/cancel", inferenceAPI.ThenFunc(s.handleAPIResponseCancel))
	mux.Handle("GET /v/responses/{response_id}", inferenceAPI.ThenFunc(s.handleAPIResponseGet))
	mux.Handle("POST /v/responses/{response_id}/cancel", inferenceAPI.ThenFunc(s.handleAPIResponseCancel))

	// llama-swap API + custom endpoints.
	mux.Handle("GET /v1/models", inferenceAPI.ThenFunc(s.handleListModels))
	mux.Handle("GET /models", inferenceAPI.ThenFunc(s.handleListModels))
	mux.Handle("GET /logs", logsAPI.ThenFunc(s.handleLogs))
	mux.Handle("GET /logs/stream", logsAPI.ThenFunc(s.handleLogStream))
	mux.Handle("GET /logs/stream/{logMonitorID...}", logsAPI.ThenFunc(s.handleLogStream))
	mux.Handle("GET /api/logs/incidents", logsAPI.ThenFunc(s.handleAPIIncidentLogs))
	mux.Handle("GET /api/logs/incidents/{name}", logsAPI.ThenFunc(s.handleAPIIncidentLog))

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /wol-health", handleHealth)
	mux.HandleFunc("GET /{$}", handleRootRedirect)

	// Embedded UI is deliberately public. The SPA must be able to load its
	// login screen before it exchanges an API key for the HttpOnly session
	// cookie. Every data and control API remains scope-protected below.
	mux.HandleFunc("GET /ui/", s.handleUI)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	// Browser session bootstrap is deliberately unauthenticated so a user can
	// exchange a known startup/managed key for the HttpOnly cookie required by
	// EventSource. The handler validates the presented secret before setting it.
	mux.HandleFunc("GET /api/auth/session", s.handleAPIAuthSession)
	mux.HandleFunc("POST /api/auth/session", s.handleAPIAuthSession)
	mux.HandleFunc("DELETE /api/auth/session", s.handleAPIAuthSession)

	// Prometheus metrics are protected by the logs scope, matching the legacy endpoint.
	mux.Handle("GET /metrics", logsAPI.ThenFunc(s.handleMetrics))

	// Operations endpoints.
	mux.Handle("GET /unload", modelUnloadAPI.ThenFunc(s.handleUnload))
	mux.Handle("GET /running", logsAPI.ThenFunc(s.handleRunning))

	// Upstream passthrough. Meter only the model-dispatched endpoints that can
	// produce token usage/timings. The narrow name-rewrite middleware rides
	// along so an alias in a JSON body reaches the engine; the full filter set
	// deliberately does not, because this route forwards the body as written.
	upstreamChain := chain.New(authMW, RequireScope(auth.ScopeInference)).Append(
		CreateProfileMiddleware(s),
		CreateModelNameRewriteMiddleware(cfg),
		CreateUpstreamInflightMiddleware(s.inflight, cfg),
		CreateMetricsMiddleware(s.metrics, cfg),
	)
	mux.HandleFunc("GET /upstream", handleUpstreamRedirect)
	mux.Handle("/upstream/{upstreamPath...}", upstreamChain.ThenFunc(s.handleUpstream))

	// ComfyUI compatibility passthrough. This uses the fixed comfyui_auto model,
	// whose compatibility settings are applied while loading config. Only the
	// root path may start an unloaded model.
	comfyUIChain := chain.New(authMW, RequireScope(auth.ScopeInference))
	mux.Handle("/comfyui", comfyUIChain.ThenFunc(handleComfyUIRedirect))
	mux.Handle("/comfyui/{comfyPath...}", comfyUIChain.ThenFunc(s.handleComfyUI))

	// API group (API-key protected) consumed by the UI.
	mux.Handle("POST /api/models/unload", modelUnloadAPI.ThenFunc(s.handleAPIUnloadAll))
	mux.Handle("GET /api/models/conflicts/{model...}", modelUnloadAPI.ThenFunc(s.handleAPIModelLoadConflicts))
	mux.Handle("POST /api/models/unload/{model...}", modelUnloadAPI.ThenFunc(s.handleAPIUnloadModel))
	mux.Handle("POST /api/models/restart/{model...}", modelUnloadAPI.ThenFunc(s.handleAPIModelRestart))
	mux.Handle("POST /api/models/force-restart/{model...}", modelUnloadAPI.ThenFunc(s.handleAPIModelForceRestart))
	mux.Handle("GET /api/profiles", configAPI.ThenFunc(s.handleAPIProfiles))
	mux.Handle("PUT /api/profiles/active", configAPI.ThenFunc(s.handleAPIActiveProfile))
	mux.Handle("POST /api/inflight/{id}/cancel", modelUnloadAPI.ThenFunc(s.handleAPICancelInflight))
	mux.Handle("GET /api/events", logsAPI.ThenFunc(s.handleAPIEvents))
	mux.Handle("GET /api/metrics/activity", auditAPI.ThenFunc(s.handleAPIActivity))
	mux.Handle("GET /api/metrics/stats", auditAPI.ThenFunc(s.handleAPIActivityStats))
	mux.Handle("GET /api/metrics/speed", auditAPI.ThenFunc(s.handleAPIMetricsSpeed))
	mux.Handle("GET /api/performance", logsAPI.ThenFunc(s.handleAPIPerformance))
	mux.Handle("GET /api/version", inferenceAPI.ThenFunc(s.handleAPIVersion))
	mux.Handle("GET /api/hardware", runtimeReadAPI.ThenFunc(s.handleAPIHardware))
	mux.Handle("POST /api/launch/parse", runtimeReadAPI.ThenFunc(s.handleAPILaunchParse))
	mux.Handle("POST /api/launch/preview", runtimeReadAPI.ThenFunc(s.handleAPILaunchPreview))
	mux.Handle("GET /api/gpus", runtimeReadAPI.ThenFunc(s.handleAPIGPUs))
	mux.Handle("GET /api/config", configAPI.ThenFunc(s.handleAPIConfig))
	mux.Handle("GET /api/extensions", configAPI.ThenFunc(s.handleAPIExtensions))
	mux.Handle("POST /api/extensions", configAPI.ThenFunc(s.handleAPIExtensions))
	mux.Handle("GET /api/extensions/{id}", configAPI.ThenFunc(s.handleAPIExtension))
	mux.Handle("PUT /api/extensions/{id}", configAPI.ThenFunc(s.handleAPIExtension))
	mux.Handle("DELETE /api/extensions/{id}", configAPI.ThenFunc(s.handleAPIExtension))
	mux.Handle("POST /api/extensions/{id}/duplicate", configAPI.ThenFunc(s.handleAPIExtensionDuplicate))
	mux.Handle("POST /api/extensions/{id}/test", configAPI.ThenFunc(s.handleAPIExtensionTest))
	mux.Handle("GET /api/extensions/logs/{id}", logsAPI.ThenFunc(s.handleAPIExtensionLogs))
	mux.Handle("GET /api/extensions/logs/{id}/stream", logsAPI.ThenFunc(s.handleAPIExtensionLogStream))
	mux.Handle("POST /api/extensions/import/preview", configAPI.ThenFunc(s.handleAPIExtensionImportPreview))
	mux.Handle("POST /api/extensions/import", configAPI.ThenFunc(s.handleAPIExtensionImport))
	mux.Handle("GET /api/extensions/export/{id}", configAPI.ThenFunc(s.handleAPIExtensionExport))
	mux.Handle("POST /api/extensions/{id}/debug/chat", configAPI.ThenFunc(s.handleAPIExtensionDebugChat))
	mux.Handle("GET /api/extensions/{id}/debug/artifact", configAPI.ThenFunc(s.handleAPIExtensionDebugArtifact))
	mux.Handle("POST /api/extensions/check", configAPI.ThenFunc(s.handleAPIExtensionCheck))
	mux.Handle("GET /api/extensions/presets", configAPI.ThenFunc(s.handleAPIExtensionPresets))
	mux.Handle("GET /api/extensions/presets/{id}", configAPI.ThenFunc(s.handleAPIExtensionPreset))
	mux.Handle("POST /api/extensions/presets/{id}/install", configAPI.ThenFunc(s.handleAPIExtensionPresetInstall))
	mux.Handle("POST /api/extensions/reload", configAPI.ThenFunc(s.handleAPIExtensionReload))
	mux.Handle("GET /api/settings/schema", configAPI.ThenFunc(s.handleAPISettingsSchema))
	mux.Handle("GET /api/settings/options/{provider}", configAPI.ThenFunc(s.handleAPISettingsOptions))
	mux.Handle("GET /api/settings/permission", configAPI.ThenFunc(s.handleAPISettingsPermission))
	mux.Handle("GET /api/settings/config", configAPI.ThenFunc(s.handleAPISettingsConfig))
	mux.Handle("POST /api/settings/config/preview", configAPI.ThenFunc(s.handleAPISettingsPreview))
	mux.Handle("POST /api/settings/config/commit", configAPI.ThenFunc(s.handleAPISettingsCommit))
	mux.Handle("GET /api/settings/config/history", configAPI.ThenFunc(s.handleAPISettingsHistory))
	mux.Handle("GET /api/settings/config/history/{revision}", configAPI.ThenFunc(s.handleAPISettingsRevision))
	mux.Handle("POST /api/config/validate", configAPI.ThenFunc(s.handleAPIConfigValidate))
	mux.Handle("PATCH /api/config", configAPI.ThenFunc(s.handleAPIConfigPatch))
	mux.Handle("DELETE /api/config/models/{model...}", configAPI.ThenFunc(s.handleAPIDeleteModelConfig))
	mux.Handle("POST /api/config/yaml/validate", configAPI.ThenFunc(s.handleAPIConfigYAMLValidate))
	mux.Handle("PUT /api/config/yaml", configAPI.ThenFunc(s.handleAPIConfigYAML))
	mux.Handle("POST /api/config/rollback", configAPI.ThenFunc(s.handleAPIConfigRollback))
	mux.Handle("GET /api/keys", keysAPI.ThenFunc(s.handleAPIKeys))
	mux.Handle("POST /api/keys", keysAPI.ThenFunc(s.handleAPICreateKey))
	mux.Handle("GET /api/keys/{id}/usage", auditReadAPI.ThenFunc(s.handleAPIKeyUsage))
	mux.Handle("PATCH /api/keys/{id}", keysAPI.ThenFunc(s.handleAPIUpdateKey))
	mux.Handle("DELETE /api/keys/{id}", keysAPI.ThenFunc(s.handleAPIRevokeKey))
	mux.Handle("POST /api/keys/{id}/rotate", keysAPI.ThenFunc(s.handleAPIRotateKey))
	mux.Handle("GET /api/audit/conversations", auditReadAPI.ThenFunc(s.handleAPIAudit))
	mux.Handle("GET /api/audit/conversations/{id}", auditReadAPI.ThenFunc(s.handleAPIAuditConversation))
	// Transcript bodies are streamed separately, so opening a conversation with
	// attachments no longer carries every turn's payload in one response.
	mux.Handle("GET /api/audit/conversations/{id}/body/{which}", auditReadAPI.ThenFunc(s.handleAPIAuditConversationBody))
	mux.Handle("DELETE /api/audit/conversations/{id}", auditAdminAPI.ThenFunc(s.handleAPIDeleteAudit))
	mux.Handle("POST /api/audit/clear", auditAdminAPI.ThenFunc(s.handleAPIClearAudit))
	mux.Handle("GET /api/usage", auditReadAPI.ThenFunc(s.handleAPIUsage))
	// The usage records page reads its aggregates, filter options and detail
	// table through separate endpoints so changing a filter only re-fetches the
	// part of the dashboard that depends on it.
	mux.Handle("GET /api/usage/analytics", auditReadAPI.ThenFunc(s.handleAPIUsageAnalytics))
	mux.Handle("GET /api/usage/options", auditReadAPI.ThenFunc(s.handleAPIUsageOptions))
	mux.Handle("GET /api/usage/records", auditReadAPI.ThenFunc(s.handleAPIUsageRecords))
	mux.Handle("GET /api/usage/export.csv", auditReadAPI.ThenFunc(s.handleAPIUsageExport))
	mux.Handle("GET /api/pricing/catalog", configAPI.ThenFunc(s.handleAPIPricingCatalog))
	mux.Handle("GET /api/pricing/prices", configAPI.ThenFunc(s.handleAPIPricingPrices))
	mux.Handle("PUT /api/pricing/prices", configAPI.ThenFunc(s.handleAPIPricingPrices))
	mux.Handle("DELETE /api/pricing/prices", configAPI.ThenFunc(s.handleAPIPricingPrices))
	mux.Handle("POST /api/pricing/sync", configAPI.ThenFunc(s.handleAPIPricingSync))
	// Importing a provider link is an inference action, not a configuration
	// mutation. Keep it usable for a management-panel session created with the
	// normal inference scope, without granting that key config-admin access.
	mux.Handle("GET /api/cc-switch/options", inferenceAPI.ThenFunc(s.handleAPICCOptions))
	mux.Handle("POST /api/cc-switch/deeplink", inferenceAPI.ThenFunc(s.handleAPICCLinks))
	mux.Handle("GET /api/runtimes", runtimeReadAPI.ThenFunc(s.handleAPIRuntimes))
	mux.Handle("GET /api/runtimes/{name}", runtimeReadAPI.ThenFunc(s.handleAPIRuntime))
	mux.Handle("GET /api/runtimes/{name}/versions", runtimeReadAPI.ThenFunc(s.handleAPIRuntimeVersions))
	mux.Handle("GET /api/runtimes/{name}/readiness", runtimeReadAPI.ThenFunc(s.handleAPIRuntimeReadiness))
	mux.Handle("GET /api/runtimes/{name}/logs", runtimeReadAPI.ThenFunc(s.handleAPIRuntimeLogs))
	mux.Handle("GET /api/lmcache/server/logs", runtimeReadAPI.ThenFunc(s.handleAPILMCacheServerLogs))
	mux.Handle("GET /api/model-files", modelFilesReadAPI.ThenFunc(s.handleAPIModelFiles))
	mux.Handle("DELETE /api/model-files/{id}", modelFilesAdminAPI.ThenFunc(s.handleAPIDeleteModelFile))
	mux.Handle("GET /api/model-download-credentials", modelFilesReadAPI.ThenFunc(s.handleAPIModelDownloadCredentials))
	mux.Handle("PATCH /api/model-download-credentials", modelFilesAdminAPI.ThenFunc(s.handleAPIUpdateModelDownloadCredentials))
	mux.Handle("GET /api/model-downloads", modelFilesReadAPI.ThenFunc(s.handleAPIModelDownloads))
	mux.Handle("POST /api/model-downloads", modelFilesAdminAPI.ThenFunc(s.handleAPICreateModelDownload))
	mux.Handle("POST /api/model-downloads/{id}/cancel", modelFilesAdminAPI.ThenFunc(s.handleAPICancelModelDownload))
	mux.Handle("POST /api/model-downloads/{id}/retry", modelFilesAdminAPI.ThenFunc(s.handleAPIRetryModelDownload))
	mux.Handle("DELETE /api/model-downloads/{id}", modelFilesAdminAPI.ThenFunc(s.handleAPIDeleteModelDownload))
	mux.Handle("POST /api/runtimes/{name}/stage", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeStage))
	mux.Handle("DELETE /api/runtimes/{name}/versions/{version}", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeDeleteVersion))
	mux.Handle("POST /api/runtimes/{name}/check", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeCheck))
	mux.Handle("POST /api/runtimes/{name}/activate/{version}", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeActivate))
	mux.Handle("POST /api/runtimes/{name}/rollback", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeRollback))
	mux.Handle("POST /api/runtimes/{name}/pin/{version}", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimePin))
	mux.Handle("POST /api/runtimes/{name}/unpin", runtimeAdminAPI.ThenFunc(s.handleAPIRuntimeUnpin))
	// LMCache optional module: status plus install/manager operations.
	mux.Handle("GET /api/lmcache", runtimeReadAPI.ThenFunc(s.handleAPILMCacheStatus))
	mux.Handle("GET /api/lmcache/dashboard", runtimeReadAPI.ThenFunc(s.handleAPILMCacheDashboard))
	mux.Handle("POST /api/lmcache/check", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheCheck))
	mux.Handle("POST /api/lmcache/enable", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheEnable))
	mux.Handle("POST /api/lmcache/disable", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheDisable))
	mux.Handle("POST /api/lmcache/server/start", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheServerStart))
	mux.Handle("POST /api/lmcache/server/stop", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheServerStop))
	mux.Handle("POST /api/lmcache/server/restart", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheServerRestart))
	mux.Handle("POST /api/lmcache/update", runtimeAdminAPI.ThenFunc(s.handleAPILMCacheUpdate))
	mux.Handle("GET /api/backends", runtimeReadAPI.ThenFunc(s.handleAPIBackends))
	mux.Handle("GET /api/backends/{model}", runtimeReadAPI.ThenFunc(s.handleAPIBackend))
	mux.Handle("GET /api/backends/{model}/capabilities", runtimeReadAPI.ThenFunc(s.handleAPIBackendCapabilities))
	mux.Handle("GET /api/backends/{model}/cache", runtimeReadAPI.ThenFunc(s.handleAPIBackendCache))
	mux.Handle("GET /api/backends/{model}/progress", runtimeReadAPI.ThenFunc(s.handleAPIBackendProgress))
	mux.Handle("GET /api/resources", runtimeReadAPI.ThenFunc(s.handleAPIResources))
	mux.Handle("POST /api/backends/{model}/cache/reset", cacheAdminAPI.ThenFunc(s.handleAPIBackendAction))
	mux.Handle("POST /api/backends/{model}/sleep", modelUnloadAPI.ThenFunc(s.handleAPIBackendAction))
	mux.Handle("POST /api/backends/{model}/wake", modelUnloadAPI.ThenFunc(s.handleAPIBackendAction))

	handler := chain.New(CreateRequestLogMiddleware(s.proxylog, s.recordRequestError), CreateCORSMiddleware()).Then(mux)
	s.handlerMu.Lock()
	s.mux = mux
	s.handler = handler
	s.handlerMu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handlerMu.RLock()
	handler := s.handler
	s.handlerMu.RUnlock()
	if handler == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "server handler is not ready")
		return
	}
	handler.ServeHTTP(w, r)
}

func (s *Server) backendAdaptersSnapshot() map[string]backend.BackendAdapter {
	s.backendAdapterMu.RLock()
	defer s.backendAdapterMu.RUnlock()
	adapters := make(map[string]backend.BackendAdapter, len(s.backendAdapters))
	for modelID, adapter := range s.backendAdapters {
		adapters[modelID] = adapter
	}
	return adapters
}

func (s *Server) recordBackendProgress(progress swaputil.BackendProgressEvent) {
	normalized, ok := progress.Normalize()
	if !ok {
		return
	}
	key := strings.TrimSpace(normalized.Model)
	if key == "" {
		key = strings.TrimSpace(normalized.Runtime)
	}
	if key == "" {
		return
	}
	s.progressMu.Lock()
	if s.backendProgress == nil {
		s.backendProgress = make(map[string]swaputil.BackendProgressEvent)
	}
	s.backendProgress[key] = normalized
	s.progressMu.Unlock()
}

// recordProcessProgress turns the process state machine's coarse lifecycle
// events into the shared BackendProgress envelope. Runtime providers and
// remote adapters already publish detailed phases; this bridge fills the
// equivalent visibility gap for the built-in llama-swap process routers.
// ProcessName is the configured model ID, so the event can be queried through
// GET /api/backends/{model}/progress and is visible to the normal SSE stream.
func (s *Server) recordProcessProgress(change swaputil.ProcessStateChangeEvent) {
	model := strings.TrimSpace(change.ProcessName)
	if model == "" {
		return
	}

	progress := swaputil.BackendProgressEvent{Model: model, Progress: 0}
	switch strings.ToLower(strings.TrimSpace(change.NewState)) {
	case string(process.StateStarting):
		progress.Phase = "loading"
		progress.Message = "model process starting"
	case string(process.StateReady):
		progress.Phase = "active"
		progress.Progress = 1
		progress.Message = "model ready"
	case string(process.StateSleeping):
		progress.Phase = "sleeping"
		progress.Progress = 1
		progress.Message = "model process sleeping"
	case string(process.StateStopping):
		progress.Phase = "unloading"
		progress.Message = "model process stopping"
	case string(process.StateStopped):
		progress.Phase = "idle"
		progress.Progress = 1
		progress.Message = "model process stopped"
	case string(process.StateShutdown):
		progress.Phase = "error"
		progress.Error = "model process entered shutdown state"
		progress.Message = "model process shut down"
	default:
		// Keep unknown future states observable without allowing an arbitrary
		// state string to become an unbounded progress phase. The event
		// normalizer applies the final length/control-character guard.
		progress.Phase = "state"
		progress.Message = "model process state changed"
	}
	backend.PublishProgress(progress)
}

func (s *Server) backendProgressSnapshot(model string) (swaputil.BackendProgressEvent, bool) {
	s.progressMu.RLock()
	progress, ok := s.backendProgress[strings.TrimSpace(model)]
	s.progressMu.RUnlock()
	return progress, ok
}

// SetConfigManager attaches the source-aware YAML transaction manager. It is
// intentionally separate from New so existing embedders that construct a
// Server directly retain the old constructor signature.
func (s *Server) SetConfigManager(manager *config.ConfigManager) {
	s.configManager = manager
	s.settingsManager = nil
	if manager != nil {
		if next, err := settings.NewManager(manager); err == nil {
			s.settingsManager = next
		}
		manager.SetReload(func(candidate config.Config) error {
			return s.ReconcileConfig(candidate)
		})
	}
	// New builds the initial mux before the external source manager is
	// attached. Rebuild it here so the unified settings routes are available
	// in normal startup as well as in embedders that attach the manager later.
	if s.mux != nil {
		s.routes()
	}
}

func (s *Server) runPricingSync(interval time.Duration) {
	if s.pricing == nil {
		return
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Minute)
			if _, err := s.pricing.Sync(ctx); err != nil && s.proxylog != nil {
				s.proxylog.Warnf("pricing sync failed: %v", err)
			}
			cancel()
		}
	}
}

// runResponseAffinityCleanup keeps the durable Responses chain bounded even
// when no later GET request happens to touch an expired id. GetResponseAffinity
// still performs lazy expiry, but a periodic sweep is needed for long-running
// servers with persistent stores. The cleanup is deliberately independent from
// ConfigReconciler/model lifecycle work and stops with the server context.
func (s *Server) runResponseAffinityCleanup() {
	if s == nil || s.store == nil || s.shutdownCtx == nil {
		return
	}
	purge := func() {
		ctx, cancel := context.WithTimeout(s.shutdownCtx, 5*time.Second)
		if err := s.store.DeleteExpiredResponseAffinities(ctx, time.Now()); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("response affinity cleanup failed: %v", err)
		}
		cancel()
	}
	purge()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

const (
	// auditMaintenanceInterval is the longest gap between maintenance
	// passes regardless of traffic.
	auditMaintenanceInterval = 10 * time.Minute

	// auditPurgeMinInterval coalesces dirty signals: a burst of large
	// requests must not serialize the store behind a sweep per insert.
	auditPurgeMinInterval = 1 * time.Minute

	// auditMaintenanceTimeout bounds one pass. The task runs in the
	// background; an overrunning pass simply retries on the next trigger.
	auditMaintenanceTimeout = 30 * time.Second

	// auditBlobGCInterval caps how often unreferenced blob files are
	// reclaimed; walking the blob tree is cheap but still pointless at
	// request rate.
	auditBlobGCInterval = time.Hour
)

// signalAuditDirty wakes the maintenance task after audit data changed (a
// conversation was persisted, or rows were deleted/cleared). The single-slot
// channel coalesces bursts into one pass.
func (s *Server) signalAuditDirty() {
	if s == nil || s.auditDirty == nil {
		return
	}
	select {
	case s.auditDirty <- struct{}{}:
	default:
	}
}

// runAuditMaintenance performs audit retention, the byte-budget sweep and
// (workflow: blob GC) as a background, low-priority task. It intentionally
// runs away from the request path: purging after every persisted insert held
// the store's single connection behind a full-table sweep, and a large table
// turned those sweeps into multi-second holds that starved unrelated readers.
func (s *Server) runAuditMaintenance() {
	if s == nil || s.store == nil || s.shutdownCtx == nil {
		return
	}
	s.maintainAudit()
	ticker := time.NewTicker(auditMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
			s.maintainAudit()
		case <-s.auditDirty:
			s.maintainAudit()
		}
	}
}

// keyTouchFlushInterval is how often buffered API-key last-used timestamps
// are persisted. In between, the hot path touches only memory.
const keyTouchFlushInterval = time.Minute

// runKeyTouchFlusher persists the authentication hot path's buffered
// last-used timestamps. One final flush drains anything left at shutdown.
func (s *Server) runKeyTouchFlusher() {
	if s == nil || s.shutdownCtx == nil || s.keys == nil {
		return
	}
	ticker := time.NewTicker(keyTouchFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			s.keys.flushTouches(context.WithoutCancel(s.shutdownCtx))
			return
		case <-ticker.C:
			s.keys.flushTouches(s.shutdownCtx)
		}
	}
}

// maintainAudit runs one maintenance pass under the configured audit
// retention and byte budget. Each step is best-effort with a warn log:
// maintenance must never fail a request, and a missed pass is retried by the
// ticker or the next dirty signal.
func (s *Server) maintainAudit() {
	if s == nil || s.store == nil || s.shutdownCtx == nil {
		return
	}
	s.auditMaintMu.Lock()
	defer s.auditMaintMu.Unlock()
	if time.Since(s.auditLastMaint) < auditPurgeMinInterval {
		return
	}
	s.auditLastMaint = time.Now()

	ctx, cancel := context.WithTimeout(s.shutdownCtx, auditMaintenanceTimeout)
	defer cancel()
	// Blob reclamation runs regardless of the current audit policy: rows
	// purged or deleted while auditing was on leave their files behind, and
	// an in-memory store simply reports no blobs to reclaim.
	s.gcAuditBlobs(ctx)

	policy := s.currentConfig().EffectiveAudit()
	if !policy.Enabled {
		return
	}
	if policy.Retention > 0 {
		if err := s.store.PurgeAuditBefore(ctx, time.Now().Add(-policy.Retention)); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("audit retention cleanup failed: %v", err)
		}
	}
	if policy.MaxBytes > 0 {
		total, err := s.store.AuditTotalBytes(ctx)
		if err != nil {
			if s.proxylog != nil {
				s.proxylog.Warnf("audit byte total failed: %v", err)
			}
		} else if total > policy.MaxBytes {
			deleted, err := s.store.PurgeAuditOverBudget(ctx, policy.MaxBytes)
			if err != nil && s.proxylog != nil {
				s.proxylog.Warnf("audit byte-budget cleanup failed: %v", err)
			} else if deleted > 0 && s.proxylog != nil {
				s.proxylog.Infof("audit byte-budget cleanup removed %d conversation(s)", deleted)
			}
		}
	}
}

// gcAuditBlobs reclaims blob files that lost their referencing conversation.
// It is rate-limited to at most once per auditBlobGCInterval so a busy
// maintenance trigger cannot turn every pass into a directory walk.
func (s *Server) gcAuditBlobs(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	if time.Since(s.auditLastGC) < auditBlobGCInterval {
		return
	}
	s.auditLastGC = time.Now()
	deleted, err := s.store.GCUnusedBlobs(ctx)
	if err != nil && s.proxylog != nil {
		s.proxylog.Warnf("audit blob garbage collection failed: %v", err)
	} else if deleted > 0 && s.proxylog != nil {
		s.proxylog.Infof("audit blob garbage collection removed %d unreferenced file(s)", deleted)
	}
}

// CloseStreams cancels long-lived response streams (Server-Sent Events) so a
// graceful httpServer.Shutdown can drain without blocking on them. It does not
// tear down routers; call Shutdown for that. Safe to call repeatedly.
func (s *Server) CloseStreams() {
	s.shutdownFn()
	if manager := s.currentExtensions(); manager != nil {
		manager.Close()
	}
}

// Shutdown stops the local and peer routers in parallel. It is idempotent;
// repeated calls return nil without re-running shutdown.
//
// Callers must drain inflight HTTP requests (httpServer.Shutdown) before
// calling this, otherwise inflight requests 502 when their processes are torn
// down. Call CloseStreams before httpServer.Shutdown so SSE streams do not
// block the drain.
func (s *Server) Shutdown(timeout time.Duration) error {
	if !s.shuttingDown.CompareAndSwap(false, true) {
		return nil
	}
	s.shutdownFn()
	if s.progressCancel != nil {
		s.progressCancel()
		s.progressCancel = nil
	}
	if s.processProgressCancel != nil {
		s.processProgressCancel()
		s.processProgressCancel = nil
	}
	s.modelFilesMu.RLock()
	downloads := s.downloads
	s.modelFilesMu.RUnlock()
	if downloads != nil {
		downloads.Close()
	}
	if s.lmcacheMod != nil {
		if lmErr := s.lmcacheMod.stopServer(); lmErr != nil && s.proxylog != nil {
			s.proxylog.Warnf("lmcache server shutdown: %v", lmErr)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, rt := range []router.Router{s.local, s.peer} {
		if rt == nil {
			continue
		}
		wg.Add(1)
		go func(rt router.Router) {
			defer wg.Done()
			if err := rt.Shutdown(timeout); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(rt)
	}

	wg.Wait()
	return errors.Join(errs...)
}
