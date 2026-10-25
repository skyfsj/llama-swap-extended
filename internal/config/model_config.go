package config

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	MODEL_CONFIG_DEFAULT_TTL   = -1
	MODEL_CONFIG_DEFAULT_PROXY = "http://localhost:${PORT}"
	comfyUIConcurrencyLimit    = 999

	// ComfyUIModelID identifies the model used by the /comfyui endpoint.
	ComfyUIModelID = "comfyui_auto"

	// AudioCppModelID identifies the model used by the /audiocpp endpoint.
	AudioCppModelID = "audiocpp_auto"
)

var validModalities = map[string]struct{}{
	"text":  {},
	"audio": {},
	"image": {},
	"video": {},
}

// validOutModalities adds "decisions", the only output of llama.cpp decision
// models, which answer /v1/systemone and generate no text.
var validOutModalities = map[string]struct{}{
	"text":      {},
	"audio":     {},
	"image":     {},
	"video":     {},
	"decisions": {},
}

// ModelCapConfig defines what modalities and features a model supports.
// Used in /v1/models to inform clients. An empty block (all zero values) is
// treated as not configured.
type ModelCapConfig struct {
	In       []string `yaml:"in" json:"in,omitempty"`
	Out      []string `yaml:"out" json:"out,omitempty"`
	Tools    bool     `yaml:"tools" json:"tools,omitempty"`
	Reranker bool     `yaml:"reranker" json:"reranker,omitempty"`
	// Translation marks the model as a translation model (e.g. Hy-MT2).
	// Clients use it to group the model under translation tooling.
	Translation bool `yaml:"translation" json:"translation,omitempty"`
	Context     int  `yaml:"context" json:"context,omitempty"`

	// Voices, when set, is returned as the response to
	// GET /v1/audio/voices?model={id} instead of asking the upstream. It is
	// never discovered and never rendered into /v1/models, so Empty and render
	// leave it alone.
	Voices []string `yaml:"voices" json:"-"`

	// DisableAuto turns off automatic capability discovery for this model.
	// It is a switch, not a capability, so Empty ignores it and it is never
	// rendered into /v1/models.
	DisableAuto bool `yaml:"disableAuto" json:"-"`
}

// Merge returns c with every capability field left at its zero value filled in
// from auto, the value discovered from the running upstream. Configured values
// always win, field by field, so a model that sets only capabilities.tools
// still picks up a discovered context length.
//
// A field explicitly set to its zero value in the config is indistinguishable
// from an omitted one, so `tools: false` cannot override a discovered true.
// Use capabilities.disableAuto to suppress discovery for the model instead.
//
// DisableAuto and Voices are carried over from c unchanged; discovery never
// sets them.
func (c ModelCapConfig) Merge(auto ModelCapConfig) ModelCapConfig {
	merged := c
	if len(merged.In) == 0 {
		merged.In = auto.In
	}
	if len(merged.Out) == 0 {
		merged.Out = auto.Out
	}
	if !merged.Tools {
		merged.Tools = auto.Tools
	}
	if !merged.Reranker {
		merged.Reranker = auto.Reranker
	}
	if merged.Context == 0 {
		merged.Context = auto.Context
	}
	return merged
}

// Empty returns true when all fields are at their zero values.
func (c ModelCapConfig) Empty() bool {
	return len(c.In) == 0 && len(c.Out) == 0 && !c.Tools && !c.Reranker && !c.Translation && c.Context == 0
}

// Validate checks that all modality values are recognized and context is
// non-negative. Returns an error if any value is invalid.
func (c ModelCapConfig) Validate() error {
	for _, m := range c.In {
		if _, ok := validModalities[m]; !ok {
			return fmt.Errorf("capabilities.in: invalid modality %q, must be one of: text, audio, image, video", m)
		}
	}
	for _, m := range c.Out {
		if _, ok := validOutModalities[m]; !ok {
			return fmt.Errorf("capabilities.out: invalid modality %q, must be one of: text, audio, image, video, decisions", m)
		}
	}
	if c.Context < 0 {
		return errors.New("capabilities.context: must be >= 0")
	}
	return nil
}

// TimeoutsConfig holds timeout settings for proxy connections
// 0 = no timeout
type TimeoutsConfig struct {
	Connect        int `yaml:"connect"`
	KeepAlive      int `yaml:"keepalive"`
	ResponseHeader int `yaml:"responseHeader"`
	TLSHandshake   int `yaml:"tlsHandshake"`
	ExpectContinue int `yaml:"expectContinue"`
	IdleConn       int `yaml:"idleConn"`
}

// CompatConfig holds compatibility settings for upstream applications.
type CompatConfig struct {
	// IgnoreWebsockets keeps websocket connections outside ordinary swapping,
	// concurrency, and TTL tracking. A confirmed config restart or unload still
	// drains existing connections and queues new handshakes behind the new
	// generation so the old process is not stopped underneath them.
	IgnoreWebsockets bool `yaml:"ignoreWebsockets"`
}

type ModelConfig struct {
	Cmd           string   `yaml:"cmd"`
	CmdStop       string   `yaml:"cmdStop"`
	Proxy         string   `yaml:"proxy"`
	Aliases       []string `yaml:"aliases"`
	Env           []string `yaml:"env"`
	CheckEndpoint string   `yaml:"checkEndpoint"`
	UnloadAfter   int      `yaml:"ttl"`
	UnloadTimeout int      `yaml:"unloadTimeout"`
	Unlisted      bool     `yaml:"unlisted"`
	UseModelName  string   `yaml:"useModelName"`

	// Disabled is maintenance mode: the model keeps its configuration but is
	// not servable. It is hidden from /v1/models, requests naming it are
	// rejected instead of routed, and it is never started — not by preload,
	// warmup or a manual load. Re-enabling is a one-field change, so this is the
	// alternative to deleting a model you intend to bring back.
	Disabled bool `yaml:"disabled"`

	// #179 for /v1/models
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Limit concurrency of HTTP requests to process
	ConcurrencyLimit int `yaml:"concurrencyLimit"`

	// Model filters see issue #174
	Filters ModelFilters `yaml:"filters"`

	// Macros: see #264
	// Model level macros take precedence over the global macros
	Macros MacroList `yaml:"macros"`

	// Metadata: see #264
	// Arbitrary metadata that can be exposed through the API
	Metadata map[string]any `yaml:"metadata"`

	// override global setting
	SendLoadingState *bool `yaml:"sendLoadingState"`

	// Timeout settings for proxy connections
	Timeouts TimeoutsConfig `yaml:"timeouts"`

	// DisableKeepAlives opens a new upstream connection for every request
	// instead of reusing pooled ones. Works around upstreams that advertise
	// keep-alive and then close the connection (#1205).
	DisableKeepAlives bool `yaml:"disableKeepAlives"`

	// Compatibility settings for upstream applications.
	Compat CompatConfig `yaml:"compat"`

	// Capabilities defines what modalities and features the model supports.
	Capabilities ModelCapConfig `yaml:"capabilities"`

	// Copy of HealthCheckTimeout from global config
	HealthCheckTimeout int `yaml:"healthCheckTimeout"`

	// Backend is optional. When present it enables the first-class backend
	// adapter/runtime path; when absent the existing cmd/cmdStop path is used.
	Backend BackendConfig `yaml:"backend"`
}

func (m *ModelConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelConfig ModelConfig
	defaults := rawModelConfig{
		Cmd:              "",
		CmdStop:          "",
		Proxy:            MODEL_CONFIG_DEFAULT_PROXY,
		Aliases:          []string{},
		Env:              []string{},
		CheckEndpoint:    "/health",
		UnloadAfter:      MODEL_CONFIG_DEFAULT_TTL, // use GlobalTTL
		UnloadTimeout:    0,                        // use global UnloadTimeout
		Unlisted:         false,
		Disabled:         false,
		UseModelName:     "",
		ConcurrencyLimit: 0,
		Name:             "",
		Description:      "",

		// matches http.DefaultTransport
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 0,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	// the default cmdStop to taskkill /f /t /pid ${PID}
	if runtime.GOOS == "windows" {
		defaults.CmdStop = "taskkill /f /t /pid ${PID}"
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*m = ModelConfig(defaults)
	return nil
}

func (m *ModelConfig) SanitizedCommand() ([]string, error) {
	if strings.TrimSpace(m.Cmd) == "" && strings.TrimSpace(m.Backend.Container.Image) != "" {
		command := append([]string(nil), m.Backend.Container.Command...)
		if len(command) == 0 && len(m.Backend.Arguments) > 0 {
			command = append(command, m.Backend.Arguments...)
		}
		command = EnsureVLLMSleepModeArgument(command, m.Backend.Type, m.UnloadAfter)
		return m.Backend.ContainerCommand(command)
	}
	if strings.TrimSpace(m.Cmd) == "" && len(m.Backend.Arguments) > 0 {
		args := make([]string, len(m.Backend.Arguments))
		copy(args, m.Backend.Arguments)
		for i, arg := range args {
			if strings.TrimSpace(arg) == "" {
				return nil, fmt.Errorf("backend.args[%d] cannot be empty", i)
			}
			if strings.ContainsRune(arg, '\x00') {
				return nil, fmt.Errorf("backend.args[%d] contains NUL", i)
			}
		}
		return EnsureVLLMSleepModeArgument(args, m.Backend.Type, m.UnloadAfter), nil
	}
	args, err := SanitizeCommand(m.Cmd)
	if err != nil {
		return nil, err
	}
	return EnsureVLLMSleepModeArgument(args, m.Backend.Type, m.UnloadAfter), nil
}

// ContainerCommand converts the structured model container block into a
// shell-free Docker/Podman argv. It is used when cmd is omitted, allowing the
// process layer to manage the container as a normal child process while the
// runtime manager controls image provenance separately.
func (b BackendConfig) ContainerCommand(command []string) ([]string, error) {
	c := b.Container
	if strings.TrimSpace(c.Image) == "" {
		return nil, errors.New("backend.container.image is required")
	}
	if err := validateRuntimeContainer("model", c); err != nil {
		return nil, err
	}
	engine := strings.ToLower(strings.TrimSpace(c.Engine))
	if engine == "" {
		engine = "docker"
	}
	args := []string{"run", "--init", "--rm"}
	if c.PullPolicy != "" {
		policy := strings.ToLower(strings.TrimSpace(c.PullPolicy))
		if policy == "if-missing" {
			policy = "missing"
		}
		args = append(args, "--pull", policy)
	}
	if c.Name != "" {
		args = append(args, "--name", c.Name)
	}
	if c.Platform != "" {
		args = append(args, "--platform", c.Platform)
	}
	if c.GPUs != "" {
		args = append(args, "--gpus", c.GPUs)
	}
	if c.ShmSize != "" {
		args = append(args, "--shm-size", c.ShmSize)
	}
	if c.StopTimeout < 0 {
		return nil, errors.New("backend.container.stopTimeout must be >= 0")
	}
	if c.StopTimeout > 0 {
		seconds := int64(c.StopTimeout / time.Second)
		if c.StopTimeout%time.Second != 0 {
			seconds++
		}
		args = append(args, "--stop-timeout", strconv.FormatInt(seconds, 10))
	}
	keys := make([]string, 0, len(c.Env))
	for key := range c.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-e", key+"="+c.Env[key])
	}
	for _, mount := range c.Mounts {
		value := mount.Source + ":" + mount.Target
		if mount.ReadOnly {
			value += ":ro"
		}
		args = append(args, "-v", value)
	}
	for _, port := range c.Ports {
		args = append(args, "-p", fmt.Sprintf("%d:%d", port.Host, port.Container))
	}
	if len(c.Entrypoint) > 0 {
		args = append(args, "--entrypoint", c.Entrypoint[0])
	}
	args = append(args, c.Image)
	if len(c.Entrypoint) > 1 {
		args = append(args, c.Entrypoint[1:]...)
	}
	for _, value := range command {
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("backend.container command contains NUL")
		}
		args = append(args, value)
	}
	return append([]string{engine}, args...), nil
}

// ModelFilters embeds Filters and adds legacy support for strip_params field
// See issue #174
type ModelFilters struct {
	Filters `yaml:",inline"`
}

func (m *ModelFilters) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawModelFilters ModelFilters
	defaults := rawModelFilters{}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	// Try to unmarshal with the old field name for backwards compatibility
	if defaults.StripParams == "" {
		var legacy struct {
			StripParams string `yaml:"strip_params"`
		}
		if legacyErr := unmarshal(&legacy); legacyErr != nil {
			return errors.New("failed to unmarshal legacy filters.strip_params: " + legacyErr.Error())
		}
		defaults.StripParams = legacy.StripParams
	}

	*m = ModelFilters(defaults)
	return nil
}

// SanitizedStripParams wraps Filters.SanitizedStripParams for backwards compatibility
// Returns ([]string, error) to match existing API
func (f ModelFilters) SanitizedStripParams() ([]string, error) {
	return f.Filters.SanitizedStripParams(), nil
}
