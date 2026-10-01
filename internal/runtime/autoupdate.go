package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	DefaultCheckEvery = 24 * time.Hour
	DefaultMinIdle    = 30 * time.Minute
	DefaultKeep       = 2
)

// UpdatePolicy controls the manager-owned update loop. Providers may expose a
// remote checker through UpdateChecker; without one, a non-empty desired
// version/commit in Spec is treated as the declarative candidate. This keeps
// automatic updates deterministic for local and air-gapped deployments.
type UpdatePolicy struct {
	Policy            string
	Channel           string
	CheckEvery        time.Duration
	MinIdle           time.Duration
	ActivateOnlyIdle  bool
	KeepVersions      int
	RollbackOnFailure bool

	// The presence bits let callers preserve an explicit false/zero while the
	// zero-value policy still receives the safe product defaults.
	CheckEverySet        bool
	MinIdleSet           bool
	ActivateOnlyIdleSet  bool
	KeepVersionsSet      bool
	RollbackOnFailureSet bool
}

// Validate checks the provider-neutral update policy before it is persisted.
// Configuration loaders already validate the YAML representation, but
// Manager.Configure is also a public entry point used by embedders and the
// control plane. Keeping this check here prevents an invalid policy from
// entering the durable runtime state and only failing much later in the
// automatic update loop.
func (p UpdatePolicy) Validate() error {
	policy := strings.TrimSpace(p.Policy)
	if p.Policy != "" && p.Policy != policy {
		return fmt.Errorf("runtime update policy %q is not normalized", p.Policy)
	}
	if policy != "" {
		switch policy {
		case "automatic", "manual", "disabled", "pinned":
		default:
			return fmt.Errorf("runtime update policy %q is invalid", p.Policy)
		}
	}
	channel := strings.TrimSpace(p.Channel)
	if p.Channel != "" && p.Channel != channel {
		return fmt.Errorf("runtime update channel %q is not normalized", p.Channel)
	}
	if channel != "" && channel != "stable" && channel != "prerelease" {
		return fmt.Errorf("runtime update channel %q is invalid", p.Channel)
	}
	for field, value := range map[string]string{"policy": p.Policy, "channel": p.Channel} {
		for _, r := range value {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
				return fmt.Errorf("runtime update %s contains whitespace or control characters", field)
			}
		}
	}
	if p.CheckEvery < 0 {
		return errors.New("runtime update check interval must be >= 0")
	}
	if p.MinIdle < 0 {
		return errors.New("runtime update idle interval must be >= 0")
	}
	if p.KeepVersions < 0 {
		return errors.New("runtime update keepVersions must be >= 0")
	}
	return nil
}

func (p UpdatePolicy) effective() UpdatePolicy {
	if strings.TrimSpace(p.Policy) == "" {
		p.Policy = "automatic"
	}
	if strings.TrimSpace(p.Channel) == "" {
		p.Channel = "stable"
	}
	if !p.CheckEverySet && p.CheckEvery == 0 {
		p.CheckEvery = DefaultCheckEvery
	}
	if !p.MinIdleSet && p.MinIdle == 0 {
		p.MinIdle = DefaultMinIdle
	}
	if !p.KeepVersionsSet && p.KeepVersions == 0 {
		p.KeepVersions = DefaultKeep
	}
	if !p.ActivateOnlyIdleSet {
		p.ActivateOnlyIdle = true
	}
	if !p.RollbackOnFailureSet {
		p.RollbackOnFailure = true
	}
	return p
}

// Definition binds a desired runtime spec to its update policy.
type Definition struct {
	Spec   Spec
	Policy UpdatePolicy
}

// UpdateChecker is optional. Implement it on a provider when checking a
// release channel requires network/provider-specific metadata. The returned
// spec must contain a concrete version before it can be staged.
type UpdateChecker interface {
	CheckForUpdate(context.Context, Manifest, Spec, UpdatePolicy) (Spec, bool, error)
}

// VersionCandidate is one version that an operator can select from the
// provider-owned catalog. The manager adds lifecycle markers (installed,
// current, staged, and so on) after the provider has resolved the remote
// metadata. Ref and Commit are deliberately part of the candidate: selecting
// a Git tag must stage the immutable commit that was listed, rather than
// resolving a mutable tag again after the operator clicks Install.
type VersionCandidate struct {
	Version     string `json:"version"`
	Label       string `json:"label,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Installed   bool   `json:"installed,omitempty"`
	Current     bool   `json:"current,omitempty"`
	Previous    bool   `json:"previous,omitempty"`
	Staged      bool   `json:"staged,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// VersionCataloger is optional. Providers implement it when their source
// format has a discoverable version index, such as PyPI, Git refs, or GitHub
// releases. Sources without a safe catalog (local paths and arbitrary wheel
// URLs) still expose installed versions, while the control plane may offer an
// explicit advanced ref as a fallback.
type VersionCataloger interface {
	ListVersions(context.Context, Spec, UpdatePolicy) ([]VersionCandidate, error)
}

// VersionCatalogResult is the manager's source-aware catalog response. A
// provider can be registered without catalog support for backwards
// compatibility with embedders; Supported lets the UI distinguish an empty
// remote catalog from a source that cannot enumerate versions.
type VersionCatalogResult struct {
	Versions   []VersionCandidate
	SourceType string
	Supported  bool
}

// RefreshProvider is an optional second phase for providers whose mutable
// source needs an admitted pull/refresh after the metadata check. The manager
// calls NeedsRefresh without side effects, then invokes RefreshForUpdate only
// after the normal idle/build gate has allowed network or installation work.
// This keeps a busy inference process on the check-only path.
type RefreshProvider interface {
	NeedsRefresh(Spec) bool
	RefreshForUpdate(context.Context, Manifest, Spec, UpdatePolicy) (Spec, bool, error)
}

// validateRuntimeControl checks and normalizes a desired runtime definition
// without touching manager state. Configuration reloads validate the whole
// candidate set before enqueueing it, so a malformed definition rejects the
// candidate while the reload path never blocks on the manager lock.
func validateRuntimeControl(name string, spec *Spec, policy *UpdatePolicy) error {
	if err := validateName(name); err != nil {
		return err
	}
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.Name != name {
		return fmt.Errorf("runtime spec name %q does not match %q", spec.Name, name)
	}
	if mode := normalizeRuntimeMode(spec.Mode); mode != RuntimeModeNative && mode != RuntimeModeContainer {
		return fmt.Errorf("runtime spec mode %q is invalid", spec.Mode)
	}
	spec.Mode = normalizeRuntimeMode(spec.Mode)
	if strings.TrimSpace(spec.Kind) == "" {
		return errors.New("runtime spec kind is required")
	}
	if strings.TrimSpace(spec.Kind) != spec.Kind {
		return fmt.Errorf("runtime spec kind %q is not normalized", spec.Kind)
	}
	for _, r := range spec.Kind {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return fmt.Errorf("runtime spec kind %q contains whitespace or control characters", spec.Kind)
		}
	}
	if spec.Version != "" {
		if err := validateVersion(spec.Version); err != nil {
			return err
		}
	}
	if strings.ContainsRune(spec.SourceType, '\x00') || strings.ContainsRune(spec.Source, '\x00') || strings.ContainsRune(spec.Ref, '\x00') || strings.ContainsRune(spec.Commit, '\x00') {
		return errors.New("runtime spec contains NUL")
	}
	if err := validateRuntimeSpecReferences(*spec, spec.Kind); err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	*policy = policy.effective()
	return nil
}

// Configure registers or replaces the desired runtime definition. It does not
// stage, activate, or restart a backend; the next automatic pass decides when
// the candidate is safe to install.
func (m *Manager) Configure(name string, spec Spec, policy UpdatePolicy) error {
	if m == nil {
		return errors.New("runtime manager is nil")
	}
	if err := validateRuntimeControl(name, &spec, &policy); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configureLocked(name, spec, policy)
}

// configureLocked applies one desired definition. Callers hold m.mu.
func (m *Manager) configureLocked(name string, spec Spec, policy UpdatePolicy) error {
	// Resolve the existing state before mutating the in-memory definition and
	// status maps.  Configure is also used by embedders and the control plane;
	// a malformed/symlinked state file or a later atomic-write failure must not
	// make a failed configuration call look successful on the next automatic
	// pass.  Keep the previous map entries so the operation is transactional at
	// the manager boundary even when the filesystem rejects the new state.
	previousDefinition, hadDefinition := m.definitions[name]
	previousStatus, hadStatus := m.status[name]
	state, err := m.statusState(name)
	if err != nil {
		return err
	}
	if m.definitions == nil {
		m.definitions = make(map[string]Definition)
	}
	m.definitions[name] = Definition{Spec: spec, Policy: policy}
	status := m.statusOrDefault(name, spec.Kind)
	status.Configured = true
	if status.Kind == "" {
		status.Kind = spec.Kind
	}
	status.Mode = normalizeRuntimeMode(spec.Mode)
	if status.Source == "" {
		status.Source = spec.Source
	}
	status.UpdatedAt = m.now()
	m.setStatusLocked(name, status)
	state.Status = status
	if err := m.writeState(name, state); err != nil {
		if hadDefinition {
			m.definitions[name] = previousDefinition
		} else {
			delete(m.definitions, name)
		}
		if hadStatus {
			m.setStatusLocked(name, previousStatus)
		} else {
			m.deleteStatusLocked(name)
		}
		return err
	}
	return nil
}

// Unconfigure removes a runtime from the automatic update set without touching
// its durable status, versions, or current/previous pointers. This is used by
// configuration reloads when an operator removes a runtime definition: the
// historical versions remain inspectable and can still be explicitly rolled
// back or pruned, but the background loop no longer stages new candidates.
func (m *Manager) Unconfigure(name string) error {
	if m == nil {
		return errors.New("runtime manager is nil")
	}
	if err := validateName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unconfigureLocked(name)
}

// unconfigureLocked removes a desired definition while preserving installed
// versions and pointers. The configured marker is persisted with the status so
// the control plane does not offer a stage form for a runtime that no longer
// has a source definition after a reload.
func (m *Manager) unconfigureLocked(name string) error {
	previousDefinition, hadDefinition := m.definitions[name]
	previousStatus, hadStatus := m.status[name]
	delete(m.definitions, name)
	if !hadStatus {
		return nil
	}
	status := previousStatus
	status.Configured = false
	status.UpdatedAt = m.now()
	m.setStatusLocked(name, status)
	state, err := m.statusState(name)
	if err == nil {
		state.Status = status
		err = m.writeState(name, state)
	}
	if err != nil {
		if hadDefinition {
			m.definitions[name] = previousDefinition
		}
		m.setStatusLocked(name, previousStatus)
		return err
	}
	return nil
}

// ControlUpdate is an immutable snapshot of the runtime control state desired
// by the configuration layer: the full managed definition set, names that
// must exist but configure nothing, names that lose their definition, the
// provider kinds to replace, and the build-while-busy flag.
type ControlUpdate struct {
	Definitions    map[string]Definition
	Registered     []string
	Removed        []string
	Providers      map[string]Provider
	BuildWhileBusy *bool
}

// QueueControlUpdate validates and records a desired control state for the
// automatic update loop to apply. Validation is synchronous so a malformed
// candidate is rejected before anything is recorded; the state change itself
// is applied by the manager's own loop while holding m.mu. The enqueue must
// stay lock-free with respect to m.mu: an in-flight activation holds m.mu
// while its switch hook waits for the configuration apply to finish, so an
// apply that waited on m.mu here would deadlock the pair. A newer update
// supersedes an older queued one.
func (m *Manager) QueueControlUpdate(update ControlUpdate) error {
	if m == nil {
		return errors.New("runtime manager is nil")
	}
	normalized, err := normalizeControlUpdate(update)
	if err != nil {
		return err
	}
	m.controlUpdateMu.Lock()
	m.pendingControlUpdate = normalized
	m.hasPendingControlUpdate = true
	m.controlUpdateMu.Unlock()
	select {
	case m.controlUpdateSignal <- struct{}{}:
	default:
	}
	return nil
}

// FlushControlUpdate applies the newest queued control update, if any, under
// the manager lock. The automatic update loop calls it before every pass so a
// queued reload is visible to the next check; embedders without the loop call
// it to observe queued updates synchronously. A failed apply is re-queued so
// the next wake retries it unless a newer update superseded it.
func (m *Manager) FlushControlUpdate() error {
	if m == nil {
		return errors.New("runtime manager is nil")
	}
	m.controlUpdateMu.Lock()
	if !m.hasPendingControlUpdate {
		m.controlUpdateMu.Unlock()
		return nil
	}
	update := m.pendingControlUpdate
	m.pendingControlUpdate = ControlUpdate{}
	m.hasPendingControlUpdate = false
	m.controlUpdateMu.Unlock()
	if update.BuildWhileBusy != nil {
		m.SetBuildWhileBusy(*update.BuildWhileBusy)
	}
	m.mu.Lock()
	err := m.applyControlUpdateLocked(update)
	m.mu.Unlock()
	if err == nil {
		return nil
	}
	m.controlUpdateMu.Lock()
	if !m.hasPendingControlUpdate {
		m.pendingControlUpdate = update
		m.hasPendingControlUpdate = true
	}
	m.controlUpdateMu.Unlock()
	return err
}

// normalizeControlUpdate validates a control update and returns a copy with
// the caller's maps left untouched, specs defaulted and policies resolved.
func normalizeControlUpdate(update ControlUpdate) (ControlUpdate, error) {
	if len(update.Definitions) > 0 {
		definitions := make(map[string]Definition, len(update.Definitions))
		for name, definition := range update.Definitions {
			spec := definition.Spec
			policy := definition.Policy
			if err := validateRuntimeControl(name, &spec, &policy); err != nil {
				return ControlUpdate{}, fmt.Errorf("runtime %s: %w", name, err)
			}
			definitions[name] = Definition{Spec: spec, Policy: policy}
		}
		update.Definitions = definitions
	}
	for _, name := range append(append([]string(nil), update.Registered...), update.Removed...) {
		if err := validateName(name); err != nil {
			return ControlUpdate{}, err
		}
	}
	for kind, provider := range update.Providers {
		if strings.TrimSpace(kind) == "" {
			return ControlUpdate{}, errors.New("control update provider kind is required")
		}
		if isNilProvider(provider) {
			return ControlUpdate{}, fmt.Errorf("runtime provider %q is nil", kind)
		}
	}
	return update, nil
}

// applyControlUpdateLocked applies one queued control update in
// deterministic name order. Callers hold m.mu. Removals only delete the
// definition, matching Unconfigure: durable versions and pointers remain.
func (m *Manager) applyControlUpdateLocked(update ControlUpdate) error {
	var errs []error
	registered := append([]string(nil), update.Registered...)
	sort.Strings(registered)
	for _, name := range registered {
		if err := m.registerLocked(name, ""); err != nil {
			errs = append(errs, fmt.Errorf("register runtime %s: %w", name, err))
		}
	}
	names := make([]string, 0, len(update.Definitions))
	for name := range update.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition := update.Definitions[name]
		if err := m.configureLocked(name, definition.Spec, definition.Policy); err != nil {
			errs = append(errs, fmt.Errorf("configure runtime %s: %w", name, err))
		}
	}
	for _, name := range update.Removed {
		if err := m.unconfigureLocked(name); err != nil {
			errs = append(errs, fmt.Errorf("unconfigure runtime %s: %w", name, err))
		}
	}
	for kind, provider := range update.Providers {
		if m.providers == nil {
			m.providers = make(map[string]Provider)
		}
		m.providers[kind] = provider
	}
	return errors.Join(errs...)
}

// Definition returns a copy of the configured desired runtime definition.
func (m *Manager) Definition(name string) (Definition, bool) {
	if m == nil {
		return Definition{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	definition, ok := m.definitions[name]
	return definition, ok
}

// AutoUpdateOnce performs one bounded update pass. A busy runtime is checked
// and marked WAITING_FOR_IDLE, but no download/build occurs until the manager
// has observed the configured idle window. Errors for independent runtimes are
// joined so one failed provider does not prevent other candidates from being
// processed.
func (m *Manager) AutoUpdateOnce(ctx context.Context) error {
	if m == nil {
		return errors.New("runtime manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	definitions := make(map[string]Definition, len(m.definitions))
	for name, definition := range m.definitions {
		definitions[name] = definition
	}
	m.mu.Unlock()
	names := make([]string, 0, len(definitions))
	for name := range definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := m.autoUpdateOne(ctx, name, definitions[name]); err != nil {
			errs = append(errs, fmt.Errorf("runtime %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// RunAutoUpdate starts the long-lived automatic update loop. It performs one
// immediate pass, then wakes periodically; the context is the only shutdown
// mechanism so Server can stop it without replacing the runtime manager.
func (m *Manager) RunAutoUpdate(ctx context.Context) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// A configuration reload queued during startup must be applied before the
	// first pass so the loop never checks stale definitions.
	if err := m.FlushControlUpdate(); err != nil {
		log.Printf("runtime: initial control update apply failed: %v", err)
	}
	if err := m.AutoUpdateOnce(ctx); err != nil {
		m.emitProgress("", "error", 0, "automatic update pass failed", err)
		log.Printf("runtime: automatic update pass failed: %v", err)
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := m.FlushControlUpdate(); err != nil {
			// A failed apply is re-queued by FlushControlUpdate and retried on
			// the next wake; surface it instead of retrying silently.
			log.Printf("runtime: control update apply failed: %v", err)
			m.emitProgress("", "error", 0, "queued configuration apply failed; retrying", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.controlUpdateSignal:
			if err := m.FlushControlUpdate(); err != nil {
				log.Printf("runtime: control update apply failed: %v", err)
				m.emitProgress("", "error", 0, "queued configuration apply failed; retrying", err)
			}
			if err := m.AutoUpdateOnce(ctx); err != nil {
				m.emitProgress("", "error", 0, "automatic update pass failed", err)
				log.Printf("runtime: automatic update pass failed: %v", err)
			}
		case <-ticker.C:
			if err := m.AutoUpdateOnce(ctx); err != nil {
				m.emitProgress("", "error", 0, "automatic update pass failed", err)
				log.Printf("runtime: automatic update pass failed: %v", err)
			}
		}
	}
}

// CheckForUpdate performs only the manager-side metadata check for one
// configured runtime. It never stages, activates, prunes, or invokes a
// side-effecting refresh. The check timestamp, candidate, and error are
// persisted so the control plane can expose the same result after a restart.
func (m *Manager) CheckForUpdate(ctx context.Context, name string) (Spec, bool, error) {
	if m == nil {
		return Spec{}, false, errors.New("runtime manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Spec{}, false, err
	}
	definition, ok := m.Definition(name)
	if !ok {
		return Spec{}, false, fmt.Errorf("runtime %s is not configured", name)
	}
	status, _, current, err := m.autoSnapshot(name)
	if err != nil {
		return Spec{}, false, err
	}
	now := m.now()
	status.LastCheck = now
	status.LastError = ""
	status.State = StateChecking
	status.UpdatedAt = now
	if err := m.saveAutoStatus(name, status); err != nil {
		return Spec{}, false, err
	}

	m.mu.Lock()
	provider, providerOK := m.providers[definition.Spec.Kind]
	m.mu.Unlock()
	if !providerOK || isNilProvider(provider) {
		err := fmt.Errorf("provider %q is not registered", definition.Spec.Kind)
		status.State = runtimeCheckFallbackState(current)
		status.LastError = err.Error()
		status.UpdatedAt = m.now()
		return Spec{}, false, errors.Join(err, m.saveAutoStatus(name, status))
	}

	candidate := definition.Spec
	available := desiredDiff(current, candidate)
	if checker, supportsCheck := provider.(UpdateChecker); supportsCheck {
		candidate, available, err = checker.CheckForUpdate(ctx, current, definition.Spec, definition.Policy)
		if err != nil {
			status.State = runtimeCheckFallbackState(current)
			status.LastError = err.Error()
			status.UpdatedAt = m.now()
			return candidate, false, errors.Join(err, m.saveAutoStatus(name, status))
		}
	}
	if candidate.Name == "" {
		candidate.Name = name
	}
	if candidate.Kind == "" {
		candidate.Kind = definition.Spec.Kind
	}
	status.Available = ""
	status.State = runtimeCheckFallbackState(current)
	if available && strings.TrimSpace(candidate.Version) != "" {
		status.Available = candidate.Version
		status.State = StateUpdateAvailable
	}
	status.UpdatedAt = m.now()
	if err := m.saveAutoStatus(name, status); err != nil {
		return candidate, available, err
	}
	return candidate, available, nil
}

func runtimeCheckFallbackState(current Manifest) State {
	if strings.TrimSpace(current.Version) == "" {
		return StateIdle
	}
	return StateActive
}

func (m *Manager) autoUpdateOne(ctx context.Context, name string, definition Definition) error {
	policy := definition.Policy.effective()
	if policy.Policy != "automatic" {
		return nil
	}
	status, _, current, err := m.autoSnapshot(name)
	if err != nil {
		return err
	}
	// A pinned version is an operator lock, not merely a prune protection
	// marker.  Do not even ask the provider for a candidate while the lock is
	// present: a mutable tag/container digest could otherwise be staged and
	// activated behind the operator's back.  Explicit Activate/Rollback calls
	// remain available and can intentionally move the serving pointer while the
	// automatic loop stays frozen.
	if strings.TrimSpace(status.Pinned) != "" {
		return nil
	}
	now := m.now()
	if !status.LastCheck.IsZero() && now.Sub(status.LastCheck) < policy.CheckEvery && status.Available == "" {
		return nil
	}

	m.mu.Lock()
	provider, ok := m.providers[definition.Spec.Kind]
	m.mu.Unlock()
	if !ok || isNilProvider(provider) {
		return fmt.Errorf("provider %q is not registered", definition.Spec.Kind)
	}
	status.State = StateChecking
	status.LastCheck = now
	status.LastError = ""
	status.UpdatedAt = now
	if err := m.saveAutoStatus(name, status); err != nil {
		m.emitProgress(name, "error", 0, "could not persist runtime check status", err)
		return err
	}
	m.emitProgress(name, "checking", 0, "checking runtime update", nil)

	candidate := definition.Spec
	available := desiredDiff(current, candidate)
	if checker, supportsCheck := provider.(UpdateChecker); supportsCheck {
		var checkErr error
		candidate, available, checkErr = checker.CheckForUpdate(ctx, current, definition.Spec, policy)
		if checkErr != nil {
			m.emitProgress(name, "error", 0, "runtime update check failed", checkErr)
			status.State = StateActive
			status.LastError = checkErr.Error()
			status.UpdatedAt = m.now()
			if saveErr := m.saveAutoStatus(name, status); saveErr != nil {
				m.emitProgress(name, "error", 0, "could not persist runtime check error", saveErr)
				return errors.Join(checkErr, saveErr)
			}
			return checkErr
		}
	}
	if refresher, supportsRefresh := provider.(RefreshProvider); supportsRefresh && !available && refresher.NeedsRefresh(definition.Spec) {
		// A mutable source may need a real pull to resolve its current digest,
		// but that is installation work rather than a metadata check. Honor the
		// same busy and minimum-idle gates used by Stage/Activate before calling
		// the provider's side-effecting refresh method.
		idle, idleFor := m.observeIdle(name, now)
		if !idle && !m.buildWhileBusyAllowed() {
			status.State = StateWaitingForIdle
			status.LastError = "runtime source refresh deferred while busy"
			status.UpdatedAt = m.now()
			if err := m.saveAutoStatus(name, status); err != nil {
				return err
			}
			m.emitProgress(name, "waiting_for_idle", 0.1, status.LastError, nil)
			return nil
		}
		if idle && policy.ActivateOnlyIdle && idleFor < policy.MinIdle {
			status.State = StateWaitingForIdle
			status.LastError = fmt.Sprintf("runtime must be idle for %s", policy.MinIdle)
			status.UpdatedAt = m.now()
			if err := m.saveAutoStatus(name, status); err != nil {
				return err
			}
			m.emitProgress(name, "waiting_for_idle", 0.1, status.LastError, nil)
			return nil
		}
		var refreshErr error
		candidate, available, refreshErr = refresher.RefreshForUpdate(ctx, current, definition.Spec, policy)
		if refreshErr != nil {
			status.State = StateActive
			status.LastError = refreshErr.Error()
			status.UpdatedAt = m.now()
			if saveErr := m.saveAutoStatus(name, status); saveErr != nil {
				return errors.Join(refreshErr, saveErr)
			}
			return refreshErr
		}
	}
	if candidate.Name == "" {
		candidate.Name = name
	}
	if candidate.Kind == "" {
		candidate.Kind = definition.Spec.Kind
	}
	if !available || strings.TrimSpace(candidate.Version) == "" {
		if status.Current != "" {
			status.State = StateActive
		} else {
			status.State = StateIdle
		}
		status.Available = ""
		status.UpdatedAt = m.now()
		if err := m.saveAutoStatus(name, status); err != nil {
			m.emitProgress(name, "error", 0, "could not persist runtime up-to-date status", err)
			return err
		}
		m.emitProgress(name, "active", 1, "runtime is up to date", nil)
		return nil
	}

	status.Available = candidate.Version
	status.State = StateUpdateAvailable
	status.UpdatedAt = m.now()
	m.emitProgress(name, "update_available", 0.1, "runtime update is available", nil)
	if err := m.saveAutoStatus(name, status); err != nil {
		m.emitProgress(name, "error", 0, "could not persist runtime update status", err)
		return err
	}

	idle, idleFor := m.observeIdle(name, now)
	if !idle && !m.buildWhileBusyAllowed() {
		status.State = StateWaitingForIdle
		status.LastError = "runtime is busy; update deferred"
		status.UpdatedAt = m.now()
		if err := m.saveAutoStatus(name, status); err != nil {
			m.emitProgress(name, "error", 0, "could not persist runtime idle wait", err)
			return err
		}
		m.emitProgress(name, "waiting_for_idle", 0.1, status.LastError, nil)
		return nil
	}
	// An explicitly enabled buildWhileBusy policy permits downloading and
	// compiling while inference is active, but activation is still a pointer
	// switch that must wait for an idle backend. Do not repeatedly stage the
	// same candidate on each minute tick; retain the STAGED marker and leave
	// the runtime in WAITING_FOR_IDLE until the idle window is satisfied.
	if idle && policy.ActivateOnlyIdle && idleFor < policy.MinIdle {
		status.State = StateWaitingForIdle
		status.LastError = fmt.Sprintf("runtime must be idle for %s", policy.MinIdle)
		status.UpdatedAt = m.now()
		if err := m.saveAutoStatus(name, status); err != nil {
			m.emitProgress(name, "error", 0, "could not persist runtime idle window", err)
			return err
		}
		m.emitProgress(name, "waiting_for_idle", 0.1, status.LastError, nil)
		return nil
	}

	if status.Staged != candidate.Version {
		if _, err := m.Stage(ctx, candidate); err != nil {
			m.emitProgress(name, "error", 0, "runtime update staging failed", err)
			var snapshotErr error
			status, _, _, snapshotErr = m.autoSnapshot(name)
			if snapshotErr != nil {
				return errors.Join(err, snapshotErr)
			}
			status.Available = candidate.Version
			status.LastError = err.Error()
			status.State = StateDegraded
			status.UpdatedAt = m.now()
			if saveErr := m.saveAutoStatus(name, status); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return err
		}
	}
	if !idle {
		status, _, _, snapshotErr := m.autoSnapshot(name)
		if snapshotErr != nil {
			return snapshotErr
		}
		status.Available = candidate.Version
		status.State = StateWaitingForIdle
		status.LastError = "runtime staged while busy; activation deferred"
		status.UpdatedAt = m.now()
		if err := m.saveAutoStatus(name, status); err != nil {
			m.emitProgress(name, "error", 0, "could not persist staged runtime wait", err)
			return err
		}
		m.emitProgress(name, "waiting_for_idle", 0.5, status.LastError, nil)
		return nil
	}
	if err := m.Activate(ctx, name, candidate.Version); err != nil {
		m.emitProgress(name, "error", 0, "runtime update activation failed", err)
		// Activation failure rolls back the serving pointers but deliberately
		// keeps the staged candidate for a later retry. Refresh both status and
		// versions before persisting the error; writing the pre-Stage snapshot
		// here would silently drop the candidate manifest from state.json.
		var snapshotErr error
		status, _, _, snapshotErr = m.autoSnapshot(name)
		if snapshotErr != nil {
			return errors.Join(err, snapshotErr)
		}
		status.Available = candidate.Version
		status.LastError = err.Error()
		status.UpdatedAt = m.now()
		if errors.Is(err, ErrRuntimeBusy) {
			status.State = StateWaitingForIdle
			if saveErr := m.saveAutoStatus(name, status); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			m.emitProgress(name, "waiting_for_idle", 0.5, status.LastError, nil)
			return nil
		}
		if saveErr := m.saveAutoStatus(name, status); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		return err
	}
	status, _, _, err = m.autoSnapshot(name)
	if err != nil {
		return err
	}
	status.Available = ""
	status.LastError = ""
	status.State = StateActive
	status.UpdatedAt = m.now()
	if err := m.saveAutoStatus(name, status); err != nil {
		m.emitProgress(name, "error", 0, "could not persist active runtime status", err)
		return err
	}
	m.emitProgress(name, "active", 1, "runtime update active", nil)
	return m.prune(name, policy.KeepVersions)
}

func desiredDiff(current Manifest, candidate Spec) bool {
	if strings.TrimSpace(candidate.Version) == "" {
		return false
	}
	if current.Version == "" {
		return true
	}
	return current.Version != candidate.Version || (candidate.Commit != "" && current.Commit != candidate.Commit) || (candidate.Ref != "" && current.Ref != candidate.Ref) || (candidate.Source != "" && current.Source != candidate.Source)
}

func (m *Manager) autoSnapshot(name string) (Status, persistedState, Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.statusState(name)
	if err != nil {
		return Status{}, persistedState{}, Manifest{}, err
	}
	status := state.Status
	if value, ok := m.status[name]; ok {
		status = value
	}
	var current Manifest
	if status.Current != "" {
		current, _ = state.Versions[status.Current]
		if current.Version == "" {
			for version, manifest := range state.Versions {
				if safeVersion(version) == safeVersion(status.Current) {
					current = manifest
					break
				}
			}
		}
	}
	return status, state, current, nil
}

// saveAutoStatus persists the caller's status for the named runtime. The
// caller's status is typically a snapshot taken before a slow provider check,
// so two protections apply before writing:
//
//   - The serving pointers and the operator pin (Current/Previous/Staged/
//     Pinned) are re-based on the live state. Clobbering a Pin with a stale
//     snapshot would expose the version to pruning, and clobbering Current
//     would misreport the serving pointer.
//   - The version ledger is left exactly as the live state holds it; callers
//     no longer pass their (stale) snapshot. Stage/Activate/Remove persist
//     their own ledger changes immediately, so any change made while the
//     check was running survives this write.
func (m *Manager) saveAutoStatus(name string, status Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, hadPrevious := m.status[name]
	state, err := m.statusState(name)
	if err != nil {
		return err
	}
	if current, ok := m.status[name]; ok {
		status.Current = current.Current
		status.Previous = current.Previous
		status.Staged = current.Staged
		status.Pinned = current.Pinned
	}
	m.setStatusLocked(name, status)
	state.Status = status
	if err := m.writeState(name, state); err != nil {
		if hadPrevious {
			m.setStatusLocked(name, previous)
		} else {
			m.deleteStatusLocked(name)
		}
		return err
	}
	return nil
}

func (m *Manager) observeIdle(name string, now time.Time) (bool, time.Duration) {
	idle := m.idleNowFor(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !idle {
		if name == "" {
			m.idleSince = time.Time{}
		} else {
			delete(m.runtimeIdleSince, name)
		}
		return false, 0
	}
	if name == "" {
		if m.idleSince.IsZero() {
			m.idleSince = now
		}
		return true, now.Sub(m.idleSince)
	}
	if m.runtimeIdleSince == nil {
		m.runtimeIdleSince = make(map[string]time.Time)
	}
	idleSince := m.runtimeIdleSince[name]
	if idleSince.IsZero() {
		idleSince = now
		m.runtimeIdleSince[name] = idleSince
	}
	return true, now.Sub(idleSince)
}

func (m *Manager) prune(name string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	detail, ok, err := m.Detail(name)
	if err != nil || !ok {
		return err
	}
	versions := make([]Manifest, 0, len(detail.Versions))
	for _, manifest := range detail.Versions {
		versions = append(versions, manifest)
	}
	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].InstalledAt.After(versions[j].InstalledAt)
	})
	protected := map[string]bool{detail.Status.Current: true, detail.Status.Previous: true, detail.Status.Pinned: true}
	kept := 0
	for _, manifest := range versions {
		if protected[manifest.Version] || kept < keep {
			kept++
			continue
		}
		// The probe carries server-side references the manager cannot see
		// (running models, config pins, live dependency counts): a protected
		// version stays on disk, counted as kept, for the next pass.
		if isProtected, reason := m.versionProtected(name, manifest.Version); isProtected {
			m.emitProgress(name, "active", 1, fmt.Sprintf("version %s kept: %s", manifest.Version, reason), nil)
			kept++
			continue
		}
		if err := m.RemoveVersion(name, manifest.Version); err != nil {
			return err
		}
	}
	return nil
}
