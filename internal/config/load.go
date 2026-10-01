package config

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"gopkg.in/yaml.v3"
)

func LoadConfigFromReader(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	yamlStr := string(data)

	// Phase 1: Substitute all ${env.VAR} macros at string level
	// This is safe because env values are simple strings without YAML formatting
	yamlStr, err = substituteEnvMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	raw, macroConfig, err := resolveConfigMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	var node yaml.Node
	if err = node.Encode(raw); err != nil {
		return Config{}, err
	}

	// Decode the resolved values into the full Config with defaults.
	config := Config{
		HealthCheckTimeout: 120,
		StartPort:          5800,
		LogLevel:           "info",
		LogTimeFormat:      "",
		LogToStdout:        LogToStdoutProxy,
		LogStorage:         LogStorageConfig{MaxFiles: LogStorageDefaultMaxFiles},
		MetricsMaxInMemory: 1000,
		GlobalTTL:          0,
		UnloadTimeout:      DEFAULT_UNLOAD_TIMEOUT,
		UI: UIConfig{Activity: UIActivityConfig{SessionID: []string{
			"X-Session-ID",
			"X-Litellm-Session-Id",
		}}},
	}
	if err = node.Decode(&config); err != nil {
		return Config{}, err
	}
	config.Macros = macroConfig.Macros
	for modelID, modelConfig := range config.Models {
		modelConfig.Macros = macroConfig.Models[modelID].Macros
		config.Models[modelID] = modelConfig
	}
	// Managed runtime definitions use the automatic stable update policy unless a
	// field is explicitly configured. Applying this after YAML decoding keeps
	// the legacy zero-value config untouched while making the runtime contract
	// deterministic for callers and the control API.
	for name, runtimeConfig := range config.Runtimes {
		config.Runtimes[name] = runtimeConfig.Effective()
	}

	if config.HealthCheckTimeout < 15 {
		config.HealthCheckTimeout = 15
	}

	// Apply defaults for performance config when section is missing
	if config.Performance.Every == 0 {
		config.Performance.Every = 5 * time.Second
	}
	if err = config.Performance.Validate(); err != nil {
		return Config{}, fmt.Errorf("performance: %w", err)
	}

	if config.StartPort < 1 {
		return Config{}, fmt.Errorf("startPort must be greater than 1")
	}

	if config.GlobalTTL < 0 {
		return Config{}, fmt.Errorf("globalTTL must be >= 0")
	}

	if config.UnloadTimeout < 0 {
		return Config{}, fmt.Errorf("unloadTimeout must be >= 0")
	}
	if config.UnloadTimeout == 0 {
		config.UnloadTimeout = DEFAULT_UNLOAD_TIMEOUT
	}

	// A nil RollbackOnModelStartFailure keeps the historical behaviour
	// (restore the last configuration that started). Only an explicit false
	// opts into maintenance mode, so existing configurations are unchanged.
	rollback := true
	if config.RollbackOnModelStartFailure != nil {
		rollback = *config.RollbackOnModelStartFailure
	}
	config.RollbackOnModelStartFailure = &rollback

	if config.LogStorage.MaxFiles < 1 || config.LogStorage.MaxFiles > LogStorageMaxFiles {
		return Config{}, fmt.Errorf("logStorage.maxFiles must be between 1 and %d", LogStorageMaxFiles)
	}

	config.UI.Activity.SessionID = normalizeHeaderNames(config.UI.Activity.SessionID)

	if config.Store != nil {
		if err := validateStorePath(config.Store.Path); err != nil {
			return Config{}, err
		}
	}

	// Apply default for upstream.ignorePaths when not specified. The default
	// matches common static-asset suffixes so they do not trigger a swap.
	if len(config.Upstream.IgnorePaths) == 0 {
		config.Upstream.IgnorePaths = DefaultUpstreamIgnorePaths()
	}

	switch config.LogToStdout {
	case LogToStdoutProxy, LogToStdoutUpstream, LogToStdoutBoth, LogToStdoutNone:
	default:
		return Config{}, fmt.Errorf("logToStdout must be one of: proxy, upstream, both, none")
	}

	// Populate the aliases map
	config.aliases = make(map[string]string)
	for modelName, modelConfig := range config.Models {
		for _, alias := range modelConfig.Aliases {
			if _, found := config.aliases[alias]; found {
				return Config{}, fmt.Errorf("duplicate alias %s found in model: %s", alias, modelName)
			}
			config.aliases[alias] = modelName
		}
	}

	// Sort model IDs for deterministic validation and normalization.
	modelIds := make([]string, 0, len(config.Models))
	for modelId := range config.Models {
		modelIds = append(modelIds, modelId)
	}
	sort.Strings(modelIds)

	for _, modelId := range modelIds {
		modelConfig := config.Models[modelId]
		modelConfig.HealthCheckTimeout = config.HealthCheckTimeout
		if modelId == ComfyUIModelID {
			if modelConfig.ConcurrencyLimit < comfyUIConcurrencyLimit {
				modelConfig.ConcurrencyLimit = comfyUIConcurrencyLimit
			}
			modelConfig.Compat.IgnoreWebsockets = true
		}

		// set model TTL to globalTTL it is the default value
		if modelConfig.UnloadAfter == MODEL_CONFIG_DEFAULT_TTL {
			modelConfig.UnloadAfter = config.GlobalTTL
		}

		if modelConfig.UnloadAfter < 0 {
			return Config{}, fmt.Errorf("model %s: invalid TTL value %d", modelId, modelConfig.UnloadAfter)
		}

		// set model unloadTimeout to the global value when left at the default
		if modelConfig.UnloadTimeout < 0 {
			return Config{}, fmt.Errorf("model %s: invalid unloadTimeout value %d", modelId, modelConfig.UnloadTimeout)
		}
		if modelConfig.UnloadTimeout == 0 {
			modelConfig.UnloadTimeout = config.UnloadTimeout
		}

		if err := modelConfig.Capabilities.Validate(); err != nil {
			return Config{}, fmt.Errorf("model %s: %w", modelId, err)
		}
		if err := modelConfig.Backend.Validate(modelId); err != nil {
			return Config{}, err
		}

		// Auto-register setParamsByID keys as aliases (skip the model's own ID)
		for key := range modelConfig.Filters.SetParamsByID {
			if key == modelId {
				continue
			}
			if _, exists := config.Models[key]; exists {
				return Config{}, fmt.Errorf("model %s filters.setParamsByID: key '%s' conflicts with an existing model ID", modelId, key)
			}
			if existingModel, exists := config.aliases[key]; exists {
				if existingModel != modelId {
					return Config{}, fmt.Errorf("duplicate alias '%s' in model %s filters.setParamsByID, already used by model %s", key, modelId, existingModel)
				}
				continue // already registered as explicit alias for this model
			}
			config.aliases[key] = modelId
			modelConfig.Aliases = append(modelConfig.Aliases, key)
		}

		if _, err := url.Parse(modelConfig.Proxy); err != nil {
			return Config{}, fmt.Errorf("model %s: invalid proxy URL: %w", modelId, err)
		}

		// checkEndpoint is dereferenced during every start; a value that
		// cannot form a request would panic the start goroutine, so reject it
		// here. "none" disables the health check; empty means the user wrote
		// an explicit "" (the unmarshal default is "/health").
		if check := strings.TrimSpace(modelConfig.CheckEndpoint); check == "" {
			return Config{}, fmt.Errorf("model %s: checkEndpoint must be a URL path or \"none\", got an empty string", modelId)
		} else if check != "none" {
			if _, err := url.Parse(check); err != nil {
				return Config{}, fmt.Errorf("model %s: invalid checkEndpoint %q: %w", modelId, check, err)
			}
		}

		if modelConfig.SendLoadingState == nil {
			v := config.SendLoadingState
			modelConfig.SendLoadingState = &v
		}

		config.Models[modelId] = modelConfig
	}

	// Normalize routing config. The legacy top-level `matrix`/`groups` keys and
	// the new `routing.router` block are mutually exclusive: a config may use
	// either style, never both.
	hasTopLevel := config.Matrix != nil || len(config.Groups) > 0
	rtr := config.Routing.Router
	hasRouting := rtr.Use != "" || rtr.Settings.Matrix != nil || rtr.Settings.Gpus != nil || len(rtr.Settings.Groups) > 0

	if hasTopLevel && hasRouting {
		return Config{}, fmt.Errorf("config uses both the legacy top-level 'matrix'/'groups' keys and the new 'routing.router' block; please migrate the top-level keys into 'routing.router' and remove them")
	}

	if !hasTopLevel {
		// groups, matrix, and gpus may all be defined under
		// routing.router.settings; routing.router.use selects which one is
		// active, so there is no conflict. But a settings block without a
		// selection silently runs the group router — almost always a missed
		// `use:` key, so reject it instead of ignoring the block.
		rs := config.Routing.Router.Settings
		if config.Routing.Router.Use == "" && (rs.Matrix != nil || rs.Gpus != nil) {
			return Config{}, fmt.Errorf("routing.router.settings defines matrix/gpus but routing.router.use is not set; set routing.router.use to matrix, gpus, or group")
		}
		switch config.Routing.Router.Use {
		case "matrix":
			if rs.Matrix == nil {
				return Config{}, fmt.Errorf("routing.router.use is 'matrix' but routing.router.settings.matrix is not set")
			}
			config.Matrix = rs.Matrix
		case "gpus":
			if rs.Gpus == nil {
				return Config{}, fmt.Errorf("routing.router.use is 'gpus' but routing.router.settings.gpus is not set")
			}
		case "group", "":
			config.Groups = rs.Groups
		default:
			return Config{}, fmt.Errorf("routing.router.use: unknown router %q (valid: group, matrix, gpus)", config.Routing.Router.Use)
		}
	}

	// groups XOR matrix
	if config.Matrix != nil && len(config.Groups) > 0 {
		return Config{}, fmt.Errorf("config cannot use both 'groups' and 'matrix'")
	}

	switch {
	case config.Matrix != nil:
		if err := ValidateMatrix(config.Matrix, config.Models); err != nil {
			return Config{}, fmt.Errorf("matrix: %w", err)
		}
	case rtr.Use == "gpus":
		if err := ValidateGpus(config.Routing.Router.Settings.Gpus, config.Models); err != nil {
			return Config{}, fmt.Errorf("gpus: %w", err)
		}
	default:
		config = AddDefaultGroupToConfig(config)

		// Validate group members
		memberUsage := make(map[string]string)
		for groupID, groupConfig := range config.Groups {
			prevSet := make(map[string]bool)
			for _, member := range groupConfig.Members {
				if _, found := prevSet[member]; found {
					return Config{}, fmt.Errorf("duplicate model member %s found in group: %s", member, groupID)
				}
				prevSet[member] = true

				// Reject unknown members here rather than letting a typo'd
				// name fail late (cold start) or vanish silently (hot reload,
				// which skips models missing from conf.Models). Members may be
				// aliases, so resolve through the same lookup the router uses.
				if _, _, found := config.FindConfig(member); !found {
					return Config{}, fmt.Errorf("group %s: member %s does not match any configured model or alias", groupID, member)
				}

				if existingGroup, exists := memberUsage[member]; exists {
					return Config{}, fmt.Errorf("model member %s is used in multiple groups: %s and %s", member, existingGroup, groupID)
				}
				memberUsage[member] = groupID
			}
		}
	}

	// Build the canonical Config.Routing from the effective result. Both legacy
	// and new-style configs converge here. The Matrix pointer is shared so the
	// compiled matrix program stays in one place. Only the settings block of
	// the active engine survives normalization.
	switch {
	case config.Matrix != nil:
		config.Routing.Router.Use = "matrix"
		config.Routing.Router.Settings.Gpus = nil
	case rtr.Use == "gpus":
		config.Routing.Router.Use = "gpus"
	default:
		config.Routing.Router.Use = "group"
		config.Routing.Router.Settings.Gpus = nil
	}
	config.Routing.Router.Settings.Matrix = config.Matrix
	config.Routing.Router.Settings.Groups = config.Groups

	if config.Routing.Scheduler.Use == "" {
		config.Routing.Scheduler.Use = "fifo"
	}

	if err := config.ValidateExtensionConfig(); err != nil {
		return Config{}, err
	}
	if config.Routing.Scheduler.Use != "fifo" {
		return Config{}, fmt.Errorf("routing.scheduler.use: unknown scheduler %q (valid: fifo)", config.Routing.Scheduler.Use)
	}
	for modelID := range config.Routing.Scheduler.Settings.Fifo.Priority {
		if _, found := config.RealModelName(modelID); !found {
			return Config{}, fmt.Errorf("routing.scheduler.settings.fifo.priority references unknown model %q", modelID)
		}
	}

	// Normalize hooks preload. An entry naming no configured model is an
	// operator mistake, and dropping it silently meant the model simply never
	// warmed up with nothing recorded anywhere — the same dangling-reference
	// case the fifo priority check above and the profile pin check below
	// reject.
	if len(config.Hooks.OnStartup.Preload) > 0 {
		var toPreload []string
		for _, modelID := range config.Hooks.OnStartup.Preload {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			real, found := config.RealModelName(modelID)
			if !found {
				return Config{}, fmt.Errorf("hooks.on_startup.preload references unknown model %q", modelID)
			}
			toPreload = append(toPreload, real)
		}
		config.Hooks.OnStartup.Preload = toPreload
	}

	// Validate API keys (env macros already substituted at string level)
	for i, apikey := range config.RequiredAPIKeys {
		if apikey == "" {
			return Config{}, fmt.Errorf("empty api key found in apiKeys")
		}
		if unsafeAPIKeyRune(apikey) {
			return Config{}, fmt.Errorf("apiKeys[%d]: api key cannot contain spaces", i)
		}
		config.RequiredAPIKeys[i] = apikey
	}
	startupIDs := make(map[string]struct{}, len(config.StartupAPIKeys))
	for i := range config.StartupAPIKeys {
		entry := &config.StartupAPIKeys[i]
		if unsafeAPIKeyUnicodeRune(entry.Key) {
			return Config{}, fmt.Errorf("apiKeys[%d]: key cannot contain whitespace", i)
		}
		entry.Key = strings.TrimSpace(entry.Key)
		if entry.Key == "" {
			return Config{}, fmt.Errorf("apiKeys[%d]: empty key", i)
		}
		if unsafeAPIKeyRune(entry.Key) {
			return Config{}, fmt.Errorf("apiKeys[%d]: key cannot contain whitespace", i)
		}
		if unsafeAPIKeyUnicodeRune(entry.ID) {
			return Config{}, fmt.Errorf("apiKeys[%d]: id must be at most 128 characters and contain no whitespace", i)
		}
		entry.ID = strings.TrimSpace(entry.ID)
		if entry.ID != "" {
			if len(entry.ID) > 128 || strings.ContainsAny(entry.ID, " \t\r\n\x00") {
				return Config{}, fmt.Errorf("apiKeys[%d]: id must be at most 128 characters and contain no whitespace", i)
			}
			if _, duplicate := startupIDs[entry.ID]; duplicate {
				return Config{}, fmt.Errorf("apiKeys[%d]: duplicate id %q", i, entry.ID)
			}
			startupIDs[entry.ID] = struct{}{}
		}
		var nameErr error
		entry.Name, nameErr = auth.NormalizeKeyName(entry.Name)
		if nameErr != nil {
			return Config{}, fmt.Errorf("apiKeys[%d]: %w", i, nameErr)
		}
		if entry.ExpiresAt != nil && !entry.ExpiresAt.After(time.Now()) {
			return Config{}, fmt.Errorf("apiKeys[%d]: expiresAt must be in the future", i)
		}
		scopes, scopeErr := auth.NormalizeScopes(entry.Scopes)
		if scopeErr != nil {
			return Config{}, fmt.Errorf("apiKeys[%d]: %w", i, scopeErr)
		}
		entry.Scopes = entry.Scopes[:0]
		for scope := range scopes {
			entry.Scopes = append(entry.Scopes, scope)
		}
		sort.Strings(entry.Scopes)
		models, modelErr := auth.NormalizeModels(entry.Models)
		if modelErr != nil {
			return Config{}, fmt.Errorf("apiKeys[%d]: %w", i, modelErr)
		}
		entry.Models = models
	}

	if err := ValidatePeerNamespace(config); err != nil {
		return Config{}, err
	}

	if err := validateSelectors(config); err != nil {
		return Config{}, err
	}

	if err := validateProfiles(config); err != nil {
		return Config{}, err
	}

	return config, nil
}

func unsafeAPIKeyRune(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func unsafeAPIKeyUnicodeRune(value string) bool {
	for _, r := range value {
		if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func validateProfiles(config Config) error {
	for profileName, profile := range config.Profiles {
		if strings.TrimSpace(profileName) == "" {
			return fmt.Errorf("profiles: profile names cannot be empty")
		}
		if len(profile.Pins) == 0 {
			return fmt.Errorf("profiles.%s.pins must contain at least one entry", profileName)
		}
		for pin, target := range profile.Pins {
			if strings.TrimSpace(pin) == "" {
				return fmt.Errorf("profiles.%s.pins: pin names cannot be empty", profileName)
			}
			if target == "" {
				continue
			}
			if _, found := config.ResolveBaseModel(target); !found {
				if _, found := config.Selectors[target]; found {
					continue
				}
				return fmt.Errorf("profiles.%s.pins.%s references unknown model %q", profileName, pin, target)
			}
		}
	}
	return nil
}

func normalizeHeaderNames(names []string) []string {
	normalized := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized
}
