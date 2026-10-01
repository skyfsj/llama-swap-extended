package config

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

const DEFAULT_GROUP_ID = "(default)"
const DEFAULT_UNLOAD_TIMEOUT = 10
const (
	LogToStdoutProxy          = "proxy"
	LogToStdoutUpstream       = "upstream"
	LogToStdoutBoth           = "both"
	LogToStdoutNone           = "none"
	LogStorageDefaultMaxFiles = 5
	LogStorageMaxFiles        = 100
)

type MacroEntry struct {
	Name  string
	Value any
}

type MacroList []MacroEntry

// UnmarshalYAML implements custom YAML unmarshaling that preserves macro definition order
func (ml *MacroList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("macros must be a mapping")
	}

	// yaml.Node.Content for a mapping contains alternating key/value nodes
	entries := make([]MacroEntry, 0, len(value.Content)/2)
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valueNode := value.Content[i+1]

		var name string
		if err := keyNode.Decode(&name); err != nil {
			return fmt.Errorf("failed to decode macro name: %w", err)
		}

		var val any
		if err := valueNode.Decode(&val); err != nil {
			return fmt.Errorf("failed to decode macro value for '%s': %w", name, err)
		}

		entries = append(entries, MacroEntry{Name: name, Value: val})
	}

	*ml = entries
	return nil
}

// Get retrieves a macro value by name
func (ml MacroList) Get(name string) (any, bool) {
	for _, entry := range ml {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return nil, false
}

type GroupConfig struct {
	Swap       bool     `yaml:"swap"`
	Exclusive  bool     `yaml:"exclusive"`
	Persistent bool     `yaml:"persistent"`
	Members    []string `yaml:"members"`
}

// set default values for GroupConfig
func (c *GroupConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawGroupConfig GroupConfig
	defaults := rawGroupConfig{
		Swap:       true,
		Exclusive:  true,
		Persistent: false,
		Members:    []string{},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*c = GroupConfig(defaults)
	return nil
}

type HooksConfig struct {
	OnStartup HookOnStartup `yaml:"on_startup"`
}

type HookOnStartup struct {
	Preload []string `yaml:"preload"`
}

type Store struct {
	Path string `yaml:"path"`
}

// LogStorageConfig controls durable snapshots created for inference crashes
// and failed HTTP requests. MaxFiles applies independently to each event type.
type LogStorageConfig struct {
	Path     string `yaml:"path"`
	MaxFiles int    `yaml:"maxFiles"`
}

func (c LogStorageConfig) Effective() LogStorageConfig {
	if c.MaxFiles == 0 {
		c.MaxFiles = LogStorageDefaultMaxFiles
	}
	return c
}

type UIConfig struct {
	Activity UIActivityConfig `yaml:"activity" json:"activity"`
}

type UIActivityConfig struct {
	SessionID []string `yaml:"session_id" json:"session_id"`
}

// StartupAPIKey is the structured form accepted in apiKeys. The secret is
// consumed at startup and is never exposed by JSON serialization. The scalar
// form remains available through RequiredAPIKeys for backwards compatibility.
type StartupAPIKey struct {
	ID        string     `yaml:"id" json:"id,omitempty"`
	Name      string     `yaml:"name" json:"name,omitempty"`
	Key       string     `yaml:"key" json:"-"`
	Value     string     `yaml:"value" json:"-"`
	Secret    string     `yaml:"secret" json:"-"`
	Scopes    []string   `yaml:"scopes" json:"scopes,omitempty"`
	Models    []string   `yaml:"models" json:"models,omitempty"`
	ExpiresAt *time.Time `yaml:"expiresAt" json:"expiresAt,omitempty"`
}

// ProfileConfig describes a runtime-selectable set of model ID rewrites.
// Empty pin targets disable the corresponding model ID while the profile is
// active. YAML null values decode to the same empty string representation.
type ProfileConfig struct {
	Description string            `yaml:"description" json:"description"`
	Pins        map[string]string `yaml:"pins" json:"pins"`
}

// UnmarshalYAML rejects the removed list-shaped profile syntax with a useful
// migration error while allowing null pin values to normalize to empty strings.
func (c *ProfileConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("profile must be a mapping with description and pins; the legacy list syntax is no longer supported")
	}
	type rawProfileConfig ProfileConfig
	var raw rawProfileConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*c = ProfileConfig(raw)
	return nil
}

type Config struct {
	HealthCheckTimeout int               `yaml:"healthCheckTimeout"`
	LogRequests        bool              `yaml:"logRequests"`
	LogLevel           string            `yaml:"logLevel"`
	LogTimeFormat      string            `yaml:"logTimeFormat"`
	LogToStdout        string            `yaml:"logToStdout"`
	LogStorage         LogStorageConfig  `yaml:"logStorage"`
	MetricsMaxInMemory int               `yaml:"metricsMaxInMemory"`
	Store              *Store            `yaml:"store"`
	UI                 UIConfig          `yaml:"ui"`
	Performance        PerformanceConfig `yaml:"performance"`
	GlobalTTL          int               `yaml:"globalTTL"`
	UnloadTimeout      int               `yaml:"unloadTimeout"`

	// RollbackOnModelStartFailure decides what happens when a model fails to
	// start with a newly applied configuration. nil (the default) means true,
	// which restores the last configuration that did start so the model keeps
	// serving while the operator fixes the change. false switches to
	// maintenance mode: the model keeps the new configuration, is marked
	// in-maintenance, and refuses to be started by incoming requests until a
	// later start succeeds and serves a request.
	RollbackOnModelStartFailure *bool `yaml:"rollbackOnModelStartFailure"`

	Models     map[string]ModelConfig    `yaml:"models"` /* key is model ID */
	Profiles   map[string]ProfileConfig  `yaml:"profiles"`
	Selectors  map[string]SelectorConfig `yaml:"selectors"`
	Extensions ExtensionsConfig          `yaml:"extensions" json:"extensions"`

	// routing is the canonical source for swap/scheduling configuration.
	// New code must read Routing, never the backwards-compat fields below.
	Routing RoutingConfig `yaml:"routing"`

	// Groups and Matrix are permanent backwards-compat input fields for the
	// legacy top-level `groups:`/`matrix:` keys. They are normalized into
	// Routing by LoadConfigFromReader. New code must not read them directly.
	Groups map[string]GroupConfig `yaml:"groups"` /* key is group ID */
	Matrix *MatrixConfig          `yaml:"matrix"`

	// for key/value replacements in model's cmd, cmdStop, proxy, checkEndPoint
	Macros MacroList `yaml:"macros"`

	// map aliases to actual model IDs
	aliases map[string]string

	// automatic port assignments
	StartPort int `yaml:"startPort"`

	// hooks, see: #209
	Hooks HooksConfig `yaml:"hooks"`

	// send loading state in reasoning
	SendLoadingState bool `yaml:"sendLoadingState"`

	// present aliases to /v1/models OpenAI API listing
	IncludeAliasesInList bool `yaml:"includeAliasesInList"`

	// support API keys, see issue #433, #50, #251
	RequiredAPIKeys []string `yaml:"apiKeys" json:"-"`
	// StartupAPIKeys contains structured apiKeys entries. It is populated by
	// Config.UnmarshalYAML and intentionally omitted from the effective config
	// JSON so secrets cannot be returned by the control API.
	StartupAPIKeys []StartupAPIKey `yaml:"-" json:"-"`

	// support remote peers, see issue #433, #296
	Peers PeerDictionaryConfig `yaml:"peers"`

	// upstream controls behaviour of the /upstream passthrough endpoint
	Upstream UpstreamConfig `yaml:"upstream"`

	// Extension configuration. Runtime management is always initialized; these
	// fields configure it without changing legacy command-based model behavior.
	ModelFiles     ModelFilesConfig         `yaml:"modelFiles"`
	RuntimeManager RuntimeManagerConfig     `yaml:"runtimeManager"`
	Runtimes       map[string]RuntimeConfig `yaml:"runtimes"`
	// LMCache is the optional KV-cache accelerator module. It is not
	// installed by default; enabling it is a control-plane operation.
	LMCache        LMCacheModuleConfig  `yaml:"lmcache"`
	ResourceBudget ResourceBudgetConfig `yaml:"resourceBudget" json:"resourceBudget"`
	Anthropic      AnthropicConfig      `yaml:"anthropic"`
	Audit          AuditConfig          `yaml:"audit"`
	Pricing        PricingConfig        `yaml:"pricing"`
}

// UnmarshalYAML accepts both the historical scalar apiKeys list and the
// structured startup-key form. We decode all other fields through an alias
// after removing apiKeys from a shallow node copy so a mapping entry cannot be
// coerced into []string by yaml.v3.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type plainConfig Config
	root := value
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config must be a mapping")
	}
	withoutKeys := *root
	withoutKeys.Content = make([]*yaml.Node, 0, len(root.Content))
	var keysNode *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, child := root.Content[i], root.Content[i+1]
		if key.Value == "apiKeys" {
			keysNode = child
			continue
		}
		withoutKeys.Content = append(withoutKeys.Content, key, child)
	}
	// The loader initializes legacy defaults before decoding. Preserve those
	// values here; decoding into a zero alias would accidentally erase them.
	raw := plainConfig(*c)
	if err := withoutKeys.Decode((*plainConfig)(&raw)); err != nil {
		return err
	}
	*c = Config(raw)
	c.RequiredAPIKeys = nil
	c.StartupAPIKeys = nil
	if keysNode == nil {
		return nil
	}
	if keysNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("apiKeys must be a sequence of strings or objects")
	}
	for i, item := range keysNode.Content {
		if item.Kind == yaml.ScalarNode {
			var secret string
			if err := item.Decode(&secret); err != nil {
				return fmt.Errorf("apiKeys[%d]: %w", i, err)
			}
			c.RequiredAPIKeys = append(c.RequiredAPIKeys, secret)
			continue
		}
		if item.Kind != yaml.MappingNode {
			return fmt.Errorf("apiKeys[%d] must be a string or object", i)
		}
		var entry StartupAPIKey
		if err := item.Decode(&entry); err != nil {
			return fmt.Errorf("apiKeys[%d]: %w", i, err)
		}
		if entry.Key == "" {
			entry.Key = entry.Value
		}
		if entry.Key == "" {
			entry.Key = entry.Secret
		}
		if entry.Key == "" {
			return fmt.Errorf("apiKeys[%d]: key is required", i)
		}
		c.StartupAPIKeys = append(c.StartupAPIKeys, entry)
	}
	return nil
}

// RoutingConfig is the canonical, normalized routing/scheduling configuration.
type RoutingConfig struct {
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Router    RouterConfig    `yaml:"router"`
}

type SchedulerConfig struct {
	Use      string            `yaml:"use"` // default "fifo"
	Settings SchedulerSettings `yaml:"settings"`
}

type SchedulerSettings struct {
	Fifo FifoConfig `yaml:"fifo"`
}

type FifoConfig struct {
	Priority map[string]int `yaml:"priority"` // model ID -> priority, default 0
}

type RouterConfig struct {
	Use      string         `yaml:"use"` // "group" (default) | "matrix" | "gpus"
	Settings RouterSettings `yaml:"settings"`
}

type RouterSettings struct {
	Groups map[string]GroupConfig `yaml:"groups"`
	Matrix *MatrixConfig          `yaml:"matrix"`
	Gpus   *GpusConfig            `yaml:"gpus"`
}

func (c *Config) RealModelName(search string) (string, bool) {
	if _, found := c.Models[search]; found {
		return search, true
	} else if name, found := c.aliases[search]; found {
		return name, found
	}
	// Config values assembled by tests, embedders, or a future hot-loader may
	// not have gone through the normalization pass that populates the private
	// aliases index. Keep aliases declared on each model authoritative in that
	// case so routing and authorization do not diverge merely because the
	// config came from a different construction path.
	for name, model := range c.Models {
		for _, alias := range model.Aliases {
			if alias == search {
				return name, true
			}
		}
	}
	return "", false
}

func (c *Config) FindConfig(modelName string) (ModelConfig, string, bool) {
	if realName, found := c.RealModelName(modelName); !found {
		return ModelConfig{}, "", false
	} else {
		return c.Models[realName], realName, true
	}
}

// ResolveBaseModel resolves a name without applying profiles. Local model IDs
// and aliases take precedence over peer model IDs, matching server dispatch.
func (c *Config) ResolveBaseModel(search string) (string, bool) {
	if realName, found := c.RealModelName(search); found {
		return realName, true
	}
	if _, _, found := c.ResolvePeerModel(search); found {
		return search, true
	}
	return "", false
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	return LoadConfigFromReader(file)
}

// rewrites the yaml to include a default group with any orphaned models
func AddDefaultGroupToConfig(config Config) Config {

	if config.Groups == nil {
		config.Groups = make(map[string]GroupConfig)
	}

	defaultGroup := GroupConfig{
		Swap:      true,
		Exclusive: true,
		Members:   []string{},
	}
	// if groups is empty, create a default group and put
	// all models into it
	if len(config.Groups) == 0 {
		for modelName := range config.Models {
			defaultGroup.Members = append(defaultGroup.Members, modelName)
		}
	} else {
		// iterate over existing group members and add non-grouped models into the default group
		for modelName := range config.Models {
			foundModel := false
		found:
			// search for the model in existing groups
			for _, groupConfig := range config.Groups {
				for _, member := range groupConfig.Members {
					if member == modelName {
						foundModel = true
						break found
					}
				}
			}

			if !foundModel {
				defaultGroup.Members = append(defaultGroup.Members, modelName)
			}
		}
	}

	sort.Strings(defaultGroup.Members) // make consistent ordering for testing
	config.Groups[DEFAULT_GROUP_ID] = defaultGroup

	return config
}
