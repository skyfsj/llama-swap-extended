package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/launchspec"
	"gopkg.in/yaml.v3"
)

const (
	AdapterProtocolNative          = "native"
	AdapterProtocolResponsesToChat = "responsesToChat"
)

// EffectiveProtocol resolves the upstream text format. Protocol is retained
// for existing configurations, but inference backends receive Chat requests.
func (BackendConfig) EffectiveProtocol() string {
	return AdapterProtocolResponsesToChat
}

func (BackendConfig) AdaptsResponses() bool { return true }

// BackendConfig describes a managed backend without changing the legacy
// command-based model contract. An empty backend block keeps the existing
// cmd/cmdStop lifecycle intact.
type BackendConfig struct {
	Type    string `yaml:"type" json:"type"`
	Runtime string `yaml:"runtime" json:"runtime"`
	// EntryPoint is an optional custom launch head for a native managed
	// runtime: instead of the runtime kind's canonical entrypoint
	// (`vllm serve` / `llama-server`), the process runs exactly these tokens
	// followed by backend.args. It exists for custom servers that must run
	// under a managed runtime's environment (for example a Python ASR server
	// started with the runtime's interpreter). The runtime still supplies the
	// PATH, library paths and version management; the operator supplies the
	// command head. Ports are written explicitly as ${PORT} in the entrypoint
	// or args because no engine-specific flags are injected for it.
	EntryPoint string `yaml:"entrypoint" json:"entrypoint,omitempty"`
	// RuntimeVersion pins the model to one installed version of its managed
	// runtime (backend.runtime). Empty follows the runtime's current pointer.
	// The pinned version must stay installed: deletion of a pinned version is
	// refused while any model references it.
	RuntimeVersion string                 `yaml:"runtimeVersion" json:"runtimeVersion"`
	Protocol       string                 `yaml:"protocol" json:"protocol"`
	APIs           []string               `yaml:"apis" json:"apis"`
	Discover       *bool                  `yaml:"discover" json:"discover"`
	Arguments      []string               `yaml:"args" json:"args"`
	Launch         *ModelLaunchConfig     `yaml:"launch" json:"launch"`
	Container      RuntimeContainerConfig `yaml:"container" json:"container"`
	Lifecycle      LifecycleConfig        `yaml:"lifecycle" json:"lifecycle"`
	Resources      ResourceConfig         `yaml:"resources" json:"resources"`
	Pricing        PricingRef             `yaml:"pricing" json:"pricing"`
	LMCache        *LMCacheModelConfig    `yaml:"lmcache" json:"lmcache"`
}

// lmcachePackageNamePattern accepts a conservative pip requirement name: a
// project name with optional extras and an exact version pin. It is the
// only install-spec form the module accepts so the generated uv arguments
// stay auditable.
var lmcachePackageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9._,-]+\])?(==[A-Za-z0-9.*+!-]+)?$`)

// lmcachePythonVersionPattern accepts a CPython version selector such as
// 3.11 or 3.12.10; it is passed verbatim to uv, which resolves and downloads
// the interpreter when it is not installed locally.
var lmcachePythonVersionPattern = regexp.MustCompile(`^3\.\d+(\.\d+)?$`)

// LMCache deployment modes and kv roles. The multiprocess (mp) mode is the
// documented LMCache recommendation: a standalone lmcache server that one or
// more vLLM engines attach to through LMCacheMPConnector. inProcess keeps
// the legacy single-process LMCacheConnectorV1 integration.
const (
	LMCacheModeMP        = "mp"
	LMCacheModeInProcess = "inProcess"

	LMCacheRoleBoth     = "kv_both"
	LMCacheRoleProducer = "kv_producer"
	LMCacheRoleConsumer = "kv_consumer"
)

// LMCacheModelConfig attaches a vLLM model to LMCache. The pip package itself
// is an optional module installed separately through the LMCache control
// panel; this block only shapes the --kv-transfer-config launch arguments.
type LMCacheModelConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	Mode      string `yaml:"mode" json:"mode"`           // mp | inProcess
	Role      string `yaml:"role" json:"role"`           // kv_both | kv_producer | kv_consumer
	Host      string `yaml:"host" json:"host"`           // mp: ZMQ server host
	Port      int    `yaml:"port" json:"port"`           // mp: ZMQ server port
	ChunkSize int    `yaml:"chunkSize" json:"chunkSize"` // inProcess: 0 keeps the library default
}

// EffectiveRole returns the normalized kv role, defaulting to kv_both.
func (c LMCacheModelConfig) EffectiveRole() string {
	role := strings.ToLower(strings.TrimSpace(c.Role))
	if role == "" {
		return LMCacheRoleBoth
	}
	return role
}

// EffectiveMode returns the normalized LMCache mode, defaulting to the
// recommended multiprocess deployment.
func (c LMCacheModelConfig) EffectiveMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	switch mode {
	case "":
		return LMCacheModeMP
	case strings.ToLower(LMCacheModeMP):
		return LMCacheModeMP
	case strings.ToLower(LMCacheModeInProcess):
		return LMCacheModeInProcess
	default:
		return mode
	}
}

// EffectiveMPHost returns the multiprocess server host, defaulting to
// localhost so the model connects to the locally managed LMCache server.
func (c LMCacheModelConfig) EffectiveMPHost() string {
	host := strings.TrimSpace(c.Host)
	if host == "" {
		return "localhost"
	}
	return host
}

// EffectiveMPPort returns the multiprocess ZMQ port, defaulting to the
// LMCache server default of 5555.
func (c LMCacheModelConfig) EffectiveMPPort() int {
	if c.Port <= 0 {
		return 5555
	}
	return c.Port
}

// LMCacheRuntimeName is the reserved managed-runtime name of the derived
// LMCache server runtime. It is staged from the top-level lmcache module,
// never from the user's runtimes map, so user configs must not claim it.
const LMCacheRuntimeName = "lmcache"

// LMCacheModuleConfig is the optional top-level LMCache module. It is not
// installed by default: enabling it stages a dedicated version-managed
// virtualenv (<runtimeRoot>/lmcache/versions/<version>/.venv) for the
// standalone LMCache multiprocess server. The module is self-contained and
// does not require a managed vLLM runtime; the per-model connector is
// installed into the model's own vLLM runtime at model start, pinned to the
// server's current version, and models attach to the server through the
// per-model backend.lmcache settings.
type LMCacheModuleConfig struct {
	Package       string              `yaml:"package" json:"package"`                         // pip requirement name
	Version       string              `yaml:"version" json:"version"`                         // pinned package version; empty resolves the newest published release at install time
	IndexURL      string              `yaml:"indexURL" json:"indexURL"`                       // optional package index mirror
	PythonVersion string              `yaml:"pythonVersion" json:"pythonVersion"`             // interpreter for the module venv
	Enabled       bool                `yaml:"enabled" json:"enabled"`                         // enable the module (install and supervise the server)
	AutoStart     *bool               `yaml:"autoStart,omitempty" json:"autoStart,omitempty"` // start the server at daemon boot; default true
	AutoStop      *bool               `yaml:"autoStop,omitempty" json:"autoStop,omitempty"`   // stop the server when the last model stops; default false keeps the cache warm
	Server        LMCacheServerConfig `yaml:"server" json:"server"`
	// Update is nil when the block is absent: the derived server runtime then
	// stays on the conservative "disabled" policy (updates are explicit
	// operations only). A present block with an empty policy is also disabled,
	// because the server is a dependency of running models, not a swappable
	// engine.
	Update *RuntimeUpdateConfig `yaml:"update,omitempty" json:"update,omitempty"`
}

// EffectiveAutoStart reports whether the server should start at daemon boot
// when the module is enabled. Unset defaults to true, preserving the
// historical behavior; false defers the first start to a model's pre-start
// gate.
func (m LMCacheModuleConfig) EffectiveAutoStart() bool {
	return m.AutoStart == nil || *m.AutoStart
}

// EffectiveAutoStop reports whether the server should stop when the last
// referencing model stops. Unset (nil) defaults to false: the cache is kept
// warm across model turnover.
func (m LMCacheModuleConfig) EffectiveAutoStop() bool {
	return m.AutoStop != nil && *m.AutoStop
}

// EffectivePackage returns the pip requirement name, defaulting to lmcache.
func (m LMCacheModuleConfig) EffectivePackage() string {
	if name := strings.TrimSpace(m.Package); name != "" {
		return name
	}
	return "lmcache"
}

// EffectivePythonVersion returns the interpreter version used for the module
// virtualenv, defaulting to the documented LMCache baseline.
func (m LMCacheModuleConfig) EffectivePythonVersion() string {
	if v := strings.TrimSpace(m.PythonVersion); v != "" {
		return v
	}
	return "3.11"
}

// PackageName returns the bare pip distribution name with any extras or
// version pin stripped, so the module venv and the connector installs always
// reference the same package.
func (m LMCacheModuleConfig) PackageName() string {
	name := strings.TrimSpace(m.Package)
	if name == "" {
		return "lmcache"
	}
	if i := strings.IndexAny(name, "[=<>~!"); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "lmcache"
	}
	return name
}

// EffectiveVersion returns the version target for the module venv. The
// update.version field is authoritative; version and a pin embedded in
// package are compatibility aliases. Validation rejects conflicting aliases,
// so this method never rewrites the user's YAML.
func (m LMCacheModuleConfig) EffectiveVersion() string {
	if m.Update != nil {
		if v := strings.TrimSpace(m.Update.Version); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(m.Version); v != "" {
		return v
	}
	if i := strings.IndexByte(m.Package, '='); i >= 0 {
		pin := strings.TrimLeft(strings.TrimSpace(m.Package[i:]), "=")
		if pin != "" {
			return pin
		}
	}
	return ""
}

// LMCacheServerConfig configures the standalone lmcache server process
// supervised by llama-swap in the recommended multiprocess deployment.
// LMCacheL2Config is the spec's "L2 (system memory)" tier — the server's
// pinned-DRAM pool, rendered as the required --l1-size-gb flag. MaxBytes
// accepts an optional unit suffix (KB/MB/GB, KiB/MiB/GiB); a bare number is
// a byte count. The pool itself can never be removed: enabled=false falls
// back to the default size.
type LMCacheL2Config struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	MaxBytes string `yaml:"maxBytes" json:"maxBytes"`
}

// LMCacheL3Config is the spec's "L3 (disk)" tier — the filesystem L2 adapter
// (LMCache --l2-adapter '{"type":"fs","base_path":...}'). The fs adapter
// has no size cap in LMCache 0.5.4, so MaxBytes, when set, is used for
// free-disk-space validation only (documented capability gap).
type LMCacheL3Config struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	Path     string `yaml:"path" json:"path"`
	MaxBytes string `yaml:"maxBytes" json:"maxBytes"`
}

type LMCacheServerConfig struct {
	Enabled        bool            `yaml:"enabled" json:"enabled"`
	Host           string          `yaml:"host" json:"host"`         // ZMQ bind host
	Port           int             `yaml:"port" json:"port"`         // ZMQ port
	HTTPHost       string          `yaml:"httpHost" json:"httpHost"` // management frontend bind host
	HTTPPort       int             `yaml:"httpPort" json:"httpPort"` // management frontend port
	L1SizeGB       int             `yaml:"l1SizeGB" json:"l1SizeGB"` // legacy: pinned-DRAM pool size in GB
	L2             LMCacheL2Config `yaml:"l2" json:"l2"`             // pinned-DRAM pool ("L2", system memory)
	L3             LMCacheL3Config `yaml:"l3" json:"l3"`             // disk tier ("L3", fs adapter)
	EvictionPolicy string          `yaml:"evictionPolicy" json:"evictionPolicy"`
	ChunkSize      int             `yaml:"chunkSize" json:"chunkSize"` // tokens per chunk; 0 = auto (server default)
	// SeparateObjectGroups keeps hybrid-attention state such as Mamba/GDN in
	// distinct cache objects, as required by LMCache for Qwen hybrid models.
	SeparateObjectGroups bool `yaml:"separateObjectGroups" json:"separateObjectGroups"`
}

// lmcacheMaxBytes caps a parseable size at 1 PiB: anything larger is a typo
// that would render a nonsense pool size.
const lmcacheMaxBytes int64 = 1 << 50

// ParseByteSize parses a byte size with an optional unit suffix: KB/MB/GB
// (powers of 1000) or KiB/MiB/GiB (powers of 1024). A bare number is a byte
// count. Sizes are integral; the result must be positive.
func ParseByteSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	var (
		num  string
		mult int64 = 1
	)
	switch {
	case strings.HasSuffix(t, "KiB"):
		num, mult = t[:len(t)-3], 1<<10
	case strings.HasSuffix(t, "MiB"):
		num, mult = t[:len(t)-3], 1<<20
	case strings.HasSuffix(t, "GiB"):
		num, mult = t[:len(t)-3], 1<<30
	case strings.HasSuffix(t, "KB"):
		num, mult = t[:len(t)-2], 1000
	case strings.HasSuffix(t, "MB"):
		num, mult = t[:len(t)-2], 1000*1000
	case strings.HasSuffix(t, "GB"):
		num, mult = t[:len(t)-2], 1000*1000*1000
	default:
		num = t
	}
	num = strings.TrimSpace(num)
	if num == "" {
		return 0, fmt.Errorf("size %q is missing its number", s)
	}
	for _, r := range num {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("size %q must be an integer with an optional KB/MB/GB/KiB/MiB/GiB suffix", s)
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q overflows: %v", s, err)
	}
	b := n * mult
	if b <= 0 {
		return 0, fmt.Errorf("size %q must be positive", s)
	}
	if b > lmcacheMaxBytes {
		return 0, fmt.Errorf("size %q exceeds the 1 PiB limit", s)
	}
	return b, nil
}

// EffectiveL2MaxBytes resolves the pinned-DRAM pool size in bytes. The
// explicit l2.maxBytes wins; otherwise the legacy l1SizeGB (GiB, matching
// LMCache's --l1-size-gb conversion); otherwise the 20 GB default.
func (s LMCacheServerConfig) EffectiveL2MaxBytes() (int64, error) {
	if s.L2.Enabled && strings.TrimSpace(s.L2.MaxBytes) != "" {
		return ParseByteSize(s.L2.MaxBytes)
	}
	gb := s.L1SizeGB
	if gb <= 0 {
		gb = 20
	}
	return int64(gb) << 30, nil
}

// Effective returns the server configuration with the documented LMCache
// defaults applied. The HTTP frontend defaults away from llama-swap's own
// 8080 control port.
func (s LMCacheServerConfig) Effective() LMCacheServerConfig {
	out := s
	if host := strings.TrimSpace(out.Host); host != "" {
		out.Host = host
	} else {
		out.Host = "localhost"
	}
	if out.Port <= 0 {
		out.Port = 5555
	}
	if host := strings.TrimSpace(out.HTTPHost); host != "" {
		out.HTTPHost = host
	} else {
		out.HTTPHost = "127.0.0.1"
	}
	if out.HTTPPort <= 0 {
		out.HTTPPort = 8900
	}
	if out.L1SizeGB <= 0 {
		out.L1SizeGB = 20
	}
	if policy := strings.TrimSpace(out.EvictionPolicy); policy != "" {
		out.EvictionPolicy = policy
	} else {
		out.EvictionPolicy = "LRU"
	}
	// ChunkSize intentionally keeps 0: it means auto — the --chunk-size flag
	// is omitted and the server applies its own library default.
	return out
}

// Validate checks the top-level LMCache module configuration.
func (m LMCacheModuleConfig) Validate() error {
	if name := strings.TrimSpace(m.Package); name != "" {
		if len(name) > 256 || !lmcachePackageNamePattern.MatchString(name) {
			return fmt.Errorf("lmcache.package %q must be a simple pip requirement name like lmcache or lmcache==0.4.1", m.Package)
		}
	}
	if m.IndexURL != "" {
		u, err := url.Parse(m.IndexURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("lmcache.indexURL must be an absolute HTTPS URL without credentials")
		}
	}
	if v := strings.TrimSpace(m.PythonVersion); v != "" && !lmcachePythonVersionPattern.MatchString(v) {
		return fmt.Errorf("lmcache.pythonVersion %q must be a CPython version like 3.11 or 3.12.10", m.PythonVersion)
	}
	if v := m.Version; v != "" {
		if strings.TrimSpace(v) != v {
			return fmt.Errorf("lmcache.version %q must be normalized", m.Version)
		}
		if !safeRuntimeToken(v) {
			return fmt.Errorf("lmcache.version %q contains invalid characters", m.Version)
		}
	}
	if m.Update != nil && m.Update.Version != "" {
		if strings.TrimSpace(m.Update.Version) != m.Update.Version {
			return fmt.Errorf("lmcache.update.version %q must be normalized", m.Update.Version)
		}
		if !safeRuntimeToken(m.Update.Version) {
			return fmt.Errorf("lmcache.update.version %q contains invalid characters", m.Update.Version)
		}
	}
	// LMCache has accumulated three version spellings over time. Keep parsing
	// all of them, but fail closed when operators supplied more than one
	// different pin instead of silently selecting one.
	versionPins := make([]struct {
		field string
		value string
	}, 0, 3)
	if m.Update != nil {
		if v := strings.TrimSpace(m.Update.Version); v != "" {
			versionPins = append(versionPins, struct {
				field string
				value string
			}{"lmcache.update.version", v})
		}
	}
	if v := strings.TrimSpace(m.Version); v != "" {
		versionPins = append(versionPins, struct {
			field string
			value string
		}{"lmcache.version", v})
	}
	if i := strings.IndexByte(m.Package, '='); i >= 0 {
		if pin := strings.TrimLeft(strings.TrimSpace(m.Package[i:]), "="); pin != "" {
			versionPins = append(versionPins, struct {
				field string
				value string
			}{"lmcache.package", pin})
		}
	}
	if len(versionPins) > 1 {
		canonical := versionPins[0]
		for _, pin := range versionPins[1:] {
			if pin.value != canonical.value {
				return fmt.Errorf("%s %q conflicts with %s %q; use one consistent LMCache version pin", canonical.field, canonical.value, pin.field, pin.value)
			}
		}
	}
	if m.Server.Enabled && !m.Enabled {
		// The manager only runs from an installed venv; reject the
		// combination at config time instead of failing at process start.
		return fmt.Errorf("lmcache.server.enabled requires lmcache.enabled")
	}
	for field, host := range map[string]string{"lmcache.server.host": m.Server.Host, "lmcache.server.httpHost": m.Server.HTTPHost} {
		trimmed := strings.TrimSpace(host)
		if trimmed != "" && trimmed != host {
			return fmt.Errorf("%s must be normalized", field)
		}
		if trimmed != "" && (strings.ContainsAny(trimmed, " \t\n") || strings.ContainsRune(trimmed, '\x00')) {
			return fmt.Errorf("%s must be a single host or IP literal", field)
		}
	}
	if port := m.Server.Port; port < 0 || port > 65535 {
		return fmt.Errorf("lmcache.server.port must be 0..65535")
	}
	if port := m.Server.HTTPPort; port < 0 || port > 65535 {
		return fmt.Errorf("lmcache.server.httpPort must be 0..65535")
	}
	if gb := m.Server.L1SizeGB; gb < 0 || gb > 1048576 {
		return fmt.Errorf("lmcache.server.l1SizeGB must be within 0..1048576")
	}
	if chunk := m.Server.ChunkSize; chunk < 0 || chunk > 1<<20 {
		return fmt.Errorf("lmcache.server.chunkSize must be within 0..1048576")
	}
	if policy := strings.TrimSpace(m.Server.EvictionPolicy); policy != "" && policy != "LRU" && policy != "IsolatedLRU" && policy != "noop" {
		return fmt.Errorf("lmcache.server.evictionPolicy must be LRU, IsolatedLRU, or noop")
	}
	if mb := strings.TrimSpace(m.Server.L2.MaxBytes); mb != "" {
		if _, err := ParseByteSize(m.Server.L2.MaxBytes); err != nil {
			return fmt.Errorf("lmcache.server.l2.maxBytes: %v", err)
		}
	}
	if l3 := m.Server.L3; l3.Enabled {
		p := strings.TrimSpace(l3.Path)
		if p == "" {
			return errors.New("lmcache.server.l3.path is required when lmcache.server.l3.enabled")
		}
		if l3.Path != p {
			return errors.New("lmcache.server.l3.path must be normalized (no leading/trailing spaces)")
		}
		if !isAbsConfigPath(l3.Path) {
			return fmt.Errorf("lmcache.server.l3.path %q must be an absolute path", l3.Path)
		}
	} else if strings.TrimSpace(l3.Path) != "" {
		return errors.New("lmcache.server.l3.path requires lmcache.server.l3.enabled")
	}
	if mb := strings.TrimSpace(m.Server.L3.MaxBytes); mb != "" {
		if _, err := ParseByteSize(m.Server.L3.MaxBytes); err != nil {
			return fmt.Errorf("lmcache.server.l3.maxBytes: %v", err)
		}
	}
	if u := m.Update; u != nil {
		if u.Policy != "" && u.Policy != "disabled" && u.Policy != "manual" && u.Policy != "automatic" && u.Policy != "pinned" {
			return errors.New("lmcache.update.policy is invalid")
		}
		if u.Channel != "" && u.Channel != "stable" && u.Channel != "prerelease" {
			return errors.New("lmcache.update.channel is invalid")
		}
		if u.CheckEvery < 0 || u.MinIdle < 0 || u.KeepVersions < 0 {
			return errors.New("lmcache.update durations and keepVersions must be >= 0")
		}
		if u.Version != "" && !safeRuntimeToken(u.Version) {
			return errors.New("lmcache.update.version contains invalid characters")
		}
		if u.Commit != "" && !safeRuntimeToken(u.Commit) {
			return errors.New("lmcache.update.commit contains invalid characters")
		}
		if u.rollbackOnFailureSet && !u.RollbackOnFailure {
			return errors.New("lmcache.update.rollbackOnFailure cannot be false for LMCache")
		}
	}
	return nil
}

// Validate checks a per-model LMCache attachment.
func (c LMCacheModelConfig) Validate(model string) error {
	// Mode and role comparisons are case-insensitive: "inProcess" is the
	// documented spelling and "inprocess"/"IN_PROCESS" are the same mode.
	if mode := strings.ToLower(strings.TrimSpace(c.Mode)); mode != "" && mode != LMCacheModeMP && mode != strings.ToLower(LMCacheModeInProcess) {
		return fmt.Errorf("model %s: backend.lmcache.mode must be %s or %s", model, LMCacheModeMP, LMCacheModeInProcess)
	}
	if role := strings.ToLower(strings.TrimSpace(c.Role)); role != "" && role != LMCacheRoleBoth && role != LMCacheRoleProducer && role != LMCacheRoleConsumer {
		return fmt.Errorf("model %s: backend.lmcache.role must be %s, %s, or %s", model, LMCacheRoleBoth, LMCacheRoleProducer, LMCacheRoleConsumer)
	}
	trimmed := strings.TrimSpace(c.Host)
	if trimmed != "" && trimmed != c.Host {
		return fmt.Errorf("model %s: backend.lmcache.host must be normalized", model)
	}
	if trimmed != "" && (strings.ContainsAny(trimmed, " \t\n") || strings.ContainsRune(trimmed, '\x00')) {
		return fmt.Errorf("model %s: backend.lmcache.host must be a single host or IP literal", model)
	}
	if port := c.Port; port < 0 || port > 65535 {
		return fmt.Errorf("model %s: backend.lmcache.port must be 0..65535", model)
	}
	if chunk := c.ChunkSize; chunk < 0 || chunk > 1<<20 {
		return fmt.Errorf("model %s: backend.lmcache.chunkSize must be within 0..1048576", model)
	}
	return nil
}

// backendAPINames is the capability vocabulary understood by the route
// registry. Keeping the allowlist at the config boundary catches typos such
// as "respones" before an operator believes a route is enabled while
// discovery silently reports a different capability. The wildcard spellings
// remain supported for backwards-compatible opt-in of every known route.
var backendAPINames = map[string]struct{}{
	"chat": {}, "completions": {}, "embeddings": {}, "responses": {},
	"transcriptions": {}, "translations": {}, "speech": {}, "images": {},
	"realtime": {}, "anthropic": {}, "embed": {}, "rerank": {},
	"classify": {}, "score": {}, "pooling": {}, "generative_scoring": {},
	"all": {}, "*": {},
}

type LifecycleConfig struct {
	Mode       string `yaml:"mode" json:"mode"` // process|sleep
	SleepLevel int    `yaml:"sleepLevel" json:"sleepLevel"`
}

type ResourceConfig struct {
	VRAMMiB          int      `yaml:"vramMiB" json:"vramMiB"`
	RAMMiB           int      `yaml:"ramMiB" json:"ramMiB"`
	GPUAffinity      []string `yaml:"gpuAffinity" json:"gpuAffinity"`
	Priority         int      `yaml:"priority" json:"priority"`
	EvictionPriority int      `yaml:"evictionPriority" json:"evictionPriority"`
}

type PricingRef struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

type RuntimeManagerConfig struct {
	Root                string        `yaml:"root" json:"root"`
	BuildWhileBusy      bool          `yaml:"buildWhileBusy" json:"buildWhileBusy"`
	SourceAllowlist     []string      `yaml:"sourceAllowlist" json:"sourceAllowlist"`
	ContainerRegistries []string      `yaml:"containerRegistries" json:"containerRegistries"`
	OperationTimeout    time.Duration `yaml:"operationTimeout" json:"operationTimeout"`
}

// UnmarshalYAML accepts human-readable durations for the manager operation
// timeout while retaining numeric nanoseconds for callers that construct the
// config programmatically. yaml.v3 does not parse a time.Duration alias from
// strings by default, so decode the one duration-bearing field explicitly.
func (c *RuntimeManagerConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawRuntimeManagerConfig struct {
		Root                string   `yaml:"root"`
		BuildWhileBusy      bool     `yaml:"buildWhileBusy"`
		SourceAllowlist     []string `yaml:"sourceAllowlist"`
		ContainerRegistries []string `yaml:"containerRegistries"`
		OperationTimeout    any      `yaml:"operationTimeout"`
	}
	var raw rawRuntimeManagerConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	timeout, err := decodeDuration(raw.OperationTimeout, "operationTimeout")
	if err != nil {
		return err
	}
	*c = RuntimeManagerConfig{
		Root: raw.Root, BuildWhileBusy: raw.BuildWhileBusy,
		SourceAllowlist:     append([]string(nil), raw.SourceAllowlist...),
		ContainerRegistries: append([]string(nil), raw.ContainerRegistries...),
		OperationTimeout:    timeout,
	}
	return nil
}

// ResourceBudgetConfig is an opt-in global guard for locally managed model
// footprints. A zero budget preserves the legacy scheduler behavior. When a
// dimension is set, the planner accounts only for configured model footprints
// in that dimension and refuses a load that cannot be made safe by sleeping or
// unloading a model with no in-flight requests.
type ResourceBudgetConfig struct {
	VRAMMiB    int  `yaml:"vramMiB" json:"vramMiB"`
	RAMMiB     int  `yaml:"ramMiB" json:"ramMiB"`
	AutoEvict  bool `yaml:"autoEvict" json:"autoEvict"`
	QueueLoads bool `yaml:"queueLoads" json:"queueLoads"`
}

func (c ResourceBudgetConfig) Enabled() bool { return c.VRAMMiB > 0 || c.RAMMiB > 0 }

func (c ResourceBudgetConfig) Validate() error {
	if c.VRAMMiB < 0 || c.RAMMiB < 0 {
		return fmt.Errorf("resourceBudget.vramMiB and ramMiB must be >= 0")
	}
	return nil
}

// ModelFilesConfig controls the model file catalog and guarded deletion API
// exposed by the control plane. Sources are keyed by a stable name so they can
// be selected from the UI and extended without changing the API shape.
type ModelFilesConfig struct {
	Sources   map[string]ModelFileSourceConfig `yaml:"sources" json:"sources"`
	MaxFiles  int                              `yaml:"maxFiles" json:"maxFiles"`
	MaxDepth  int                              `yaml:"maxDepth" json:"maxDepth"`
	Downloads ModelDownloadsConfig             `yaml:"downloads" json:"downloads"`
}

// ModelFileSourceConfig describes one filesystem-backed model source. A
// directory source scans model-looking files below Path. Cache sources scan
// their provider's snapshot layout; Path may be omitted to use the provider's
// standard environment or platform cache directory.
type ModelFileSourceConfig struct {
	Type      string `yaml:"type" json:"type"`
	Path      string `yaml:"path" json:"path"`
	Recursive *bool  `yaml:"recursive" json:"recursive"`
}

const (
	ModelFileSourceDirectory = "directory"
	ModelFileSourceFile      = "file"
	ModelFileSourceHFCache   = "hf_cache"
	ModelFileSourceMSCache   = "modelscope_cache"
)

// ModelDownloadsConfig controls the persistent model repository download
// queue. Provider tokens may be stored in the protected configuration file;
// config snapshots redact them before returning them to control-plane clients,
// and download tasks never contain credential material. When a configured
// token is empty, the corresponding environment variable remains the fallback
// for deployments that keep secrets outside the configuration file.
type ModelDownloadsConfig struct {
	Enabled            *bool         `yaml:"enabled" json:"enabled"`
	Workers            int           `yaml:"workers" json:"workers"`
	FileWorkers        int           `yaml:"fileWorkers" json:"fileWorkers"`
	ChunkWorkers       int           `yaml:"chunkWorkers" json:"chunkWorkers"`
	ChunkSizeMiB       int           `yaml:"chunkSizeMiB" json:"chunkSizeMiB"`
	ChunkThresholdMiB  int           `yaml:"chunkThresholdMiB" json:"chunkThresholdMiB"`
	MaxRetries         int           `yaml:"maxRetries" json:"maxRetries"`
	MaxTaskRetries     int           `yaml:"maxTaskRetries" json:"maxTaskRetries"`
	RetryBackoff       time.Duration `yaml:"retryBackoff" json:"retryBackoff"`
	HFBaseURL          string        `yaml:"hfBaseURL" json:"hfBaseURL"`
	HFTokenEnv         string        `yaml:"hfTokenEnv" json:"hfTokenEnv"`
	HFToken            string        `yaml:"hfToken" json:"hfToken"`
	ModelScopeBaseURL  string        `yaml:"modelScopeBaseURL" json:"modelScopeBaseURL"`
	ModelScopeTokenEnv string        `yaml:"modelScopeTokenEnv" json:"modelScopeTokenEnv"`
	ModelScopeToken    string        `yaml:"modelScopeToken" json:"modelScopeToken"`
}

// Effective returns download defaults without mutating the loaded config.
func (c ModelDownloadsConfig) Effective() ModelDownloadsConfig {
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	if c.Workers == 0 {
		c.Workers = 1
	}
	if c.FileWorkers == 0 {
		c.FileWorkers = 4
	}
	if c.ChunkWorkers == 0 {
		c.ChunkWorkers = 4
	}
	if c.ChunkSizeMiB == 0 {
		c.ChunkSizeMiB = 16
	}
	if c.ChunkThresholdMiB == 0 {
		c.ChunkThresholdMiB = 64
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 5
	}
	if c.MaxTaskRetries == 0 {
		c.MaxTaskRetries = 3
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = 2 * time.Second
	}
	if strings.TrimSpace(c.HFBaseURL) == "" {
		c.HFBaseURL = "https://huggingface.co"
	}
	if strings.TrimSpace(c.HFTokenEnv) == "" {
		c.HFTokenEnv = "HF_TOKEN"
	}
	if strings.TrimSpace(c.ModelScopeBaseURL) == "" {
		c.ModelScopeBaseURL = "https://modelscope.cn"
	}
	if strings.TrimSpace(c.ModelScopeTokenEnv) == "" {
		c.ModelScopeTokenEnv = "MODELSCOPE_API_TOKEN"
	}
	return c
}

// Validate checks queue limits and provider endpoints without making a network
// request. HTTP mirrors are permitted for local deployments, but callers
// should use HTTPS whenever an access token is involved.
func (c ModelDownloadsConfig) Validate() error {
	if c.Workers < 0 || c.Workers > 32 {
		return fmt.Errorf("modelFiles.downloads.workers must be between 0 and 32")
	}
	if c.FileWorkers < 0 || c.FileWorkers > 16 {
		return fmt.Errorf("modelFiles.downloads.fileWorkers must be between 0 and 16")
	}
	if c.ChunkWorkers < 0 || c.ChunkWorkers > 16 {
		return fmt.Errorf("modelFiles.downloads.chunkWorkers must be between 0 and 16")
	}
	if c.ChunkSizeMiB < 0 || c.ChunkSizeMiB > 1024 {
		return fmt.Errorf("modelFiles.downloads.chunkSizeMiB must be between 0 and 1024")
	}
	if c.ChunkThresholdMiB < 0 || c.ChunkThresholdMiB > 4096 {
		return fmt.Errorf("modelFiles.downloads.chunkThresholdMiB must be between 0 and 4096")
	}
	if c.MaxRetries < 0 || c.MaxRetries > 20 {
		return fmt.Errorf("modelFiles.downloads.maxRetries must be between 0 and 20")
	}
	if c.MaxTaskRetries < 0 || c.MaxTaskRetries > 10 {
		return fmt.Errorf("modelFiles.downloads.maxTaskRetries must be between 0 and 10")
	}
	if c.RetryBackoff < 0 || c.RetryBackoff > time.Hour {
		return fmt.Errorf("modelFiles.downloads.retryBackoff must be between 0 and 1h")
	}
	for field, baseURL := range map[string]string{
		"hfBaseURL":         c.HFBaseURL,
		"modelScopeBaseURL": c.ModelScopeBaseURL,
	} {
		baseURL = strings.TrimSpace(baseURL)
		if baseURL == "" {
			continue
		}
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("modelFiles.downloads.%s must be an HTTP(S) URL without credentials, query, or fragment", field)
		}
	}
	for field, tokenEnv := range map[string]string{
		"hfTokenEnv":         c.HFTokenEnv,
		"modelScopeTokenEnv": c.ModelScopeTokenEnv,
	} {
		tokenEnv = strings.TrimSpace(tokenEnv)
		if tokenEnv != "" && !isEnvName(tokenEnv) {
			return fmt.Errorf("modelFiles.downloads.%s must be a valid environment variable name", field)
		}
	}
	for field, token := range map[string]string{
		"hfToken":         c.HFToken,
		"modelScopeToken": c.ModelScopeToken,
	} {
		if len(token) > 4096 {
			return fmt.Errorf("modelFiles.downloads.%s must be at most 4096 characters", field)
		}
		if strings.IndexFunc(token, unicode.IsControl) >= 0 {
			return fmt.Errorf("modelFiles.downloads.%s must not contain control characters", field)
		}
	}
	return nil
}

func isEnvName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// NormalizeModelFileSourceType accepts the canonical source types plus common
// spellings. The hf_ceche alias is kept for compatibility with the original
// UI wording/typo and is normalized before scanning.
func NormalizeModelFileSourceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "directory", "dir", "folder", "file_directory":
		return ModelFileSourceDirectory
	case "file":
		return ModelFileSourceFile
	case "hf_cache", "hf-cache", "hf", "huggingface", "huggingface_cache", "hf_ceche", "hf-ceche":
		return ModelFileSourceHFCache
	case "modelscope_cache", "modelscope-cache", "modelscope", "ms_cache", "ms-cache":
		return ModelFileSourceMSCache
	default:
		return ""
	}
}

// Effective returns scan defaults without mutating the loaded configuration.
func (c ModelFilesConfig) Effective() ModelFilesConfig {
	if c.MaxFiles == 0 {
		c.MaxFiles = 2000
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = 8
	}
	if c.Sources == nil {
		c.Sources = map[string]ModelFileSourceConfig{}
	}
	c.Downloads = c.Downloads.Effective()
	return c
}

// Validate checks the model file catalog settings without touching the
// filesystem. Missing roots are reported by the catalog as unavailable so a
// typo in an optional source does not prevent llama-swap from starting.
func (c ModelFilesConfig) Validate() error {
	if c.MaxFiles < 0 || c.MaxFiles > 100000 {
		return fmt.Errorf("modelFiles.maxFiles must be between 0 and 100000")
	}
	if c.MaxDepth < 0 || c.MaxDepth > 64 {
		return fmt.Errorf("modelFiles.maxDepth must be between 0 and 64")
	}
	if err := c.Downloads.Validate(); err != nil {
		return err
	}
	for name, source := range c.Sources {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("modelFiles.sources cannot contain an empty name")
		}
		if len(name) > 128 {
			return fmt.Errorf("modelFiles.sources.%s name is too long", name)
		}
		typeName := NormalizeModelFileSourceType(source.Type)
		if typeName == "" {
			return fmt.Errorf("modelFiles.sources.%s.type must be directory, file, hf_cache, or modelscope_cache", name)
		}
		path := strings.TrimSpace(source.Path)
		if strings.ContainsRune(path, '\x00') {
			return fmt.Errorf("modelFiles.sources.%s.path contains NUL", name)
		}
		if (typeName == ModelFileSourceDirectory || typeName == ModelFileSourceFile) && path == "" {
			return fmt.Errorf("modelFiles.sources.%s.path is required for %s sources", name, typeName)
		}
		if path != "" && !isAbsConfigPath(path) {
			return fmt.Errorf("modelFiles.sources.%s.path must be an absolute path", name)
		}
	}
	return nil
}

type RuntimeConfig struct {
	Kind      string                 `yaml:"kind" json:"kind"`
	Mode      string                 `yaml:"mode" json:"mode"` // native|container
	Source    RuntimeSource          `yaml:"source" json:"source"`
	Build     RuntimeBuildConfig     `yaml:"build" json:"build"`
	Container RuntimeContainerConfig `yaml:"container" json:"container"`
	Update    RuntimeUpdateConfig    `yaml:"update" json:"update"`
	Verify    RuntimeVerifyConfig    `yaml:"verify" json:"verify"`
	// Metadata is the runtime-level launch metadata every model on this
	// runtime inherits: the launch head for runtimes that front a custom
	// server, and the quick-config engine defaults. A model overrides any of
	// them — its own entrypoint wins, and a flag written in its args always
	// beats the corresponding default.
	Metadata RuntimeMetadataConfig `yaml:"metadata" json:"metadata"`
}

// Validate checks the runtime's launch metadata: the entrypoint only makes
// sense for a native runtime, and the quick-config defaults must be sane.
func (m RuntimeMetadataConfig) Validate(runtimeName, mode string) error {
	if strings.ContainsRune(m.EntryPoint, '\x00') {
		return fmt.Errorf("runtime %s: metadata.entrypoint contains NUL", runtimeName)
	}
	if strings.TrimSpace(m.EntryPoint) != "" && mode == "container" {
		return fmt.Errorf("runtime %s: metadata.entrypoint requires mode native", runtimeName)
	}
	if m.ContextPerRequest < 0 {
		return fmt.Errorf("runtime %s: metadata.contextPerRequest must be >= 0", runtimeName)
	}
	if m.MaxConcurrency < 0 {
		return fmt.Errorf("runtime %s: metadata.maxConcurrency must be >= 0", runtimeName)
	}
	if m.TensorParallelSize < 0 {
		return fmt.Errorf("runtime %s: metadata.tensorParallelSize must be >= 0", runtimeName)
	}
	if m.GPUMemoryUtilization != nil && (*m.GPUMemoryUtilization <= 0 || *m.GPUMemoryUtilization > 1) {
		return fmt.Errorf("runtime %s: metadata.gpuMemoryUtilization must be within (0, 1]", runtimeName)
	}
	return nil
}

// EffectiveLaunchMetadata resolves the launch head and quick-config defaults a
// model inherits from its runtime. The model's own entrypoint wins over the
// runtime's. Defaults are rendered as engine flags and are empty when a custom
// head is in effect (a custom server has no engine grammar to inject into) or
// when the model already pins the flag in its args.
func EffectiveLaunchMetadata(model ModelConfig, runtime RuntimeConfig, kind string) (string, []string) {
	entrypoint := strings.TrimSpace(model.Backend.EntryPoint)
	if entrypoint == "" {
		entrypoint = strings.TrimSpace(runtime.Metadata.EntryPoint)
	}
	if entrypoint != "" || runtime.Metadata.Empty() {
		return entrypoint, nil
	}
	spec := launchspec.Spec{
		ServedModelName:      "",
		ContextPerRequest:    runtime.Metadata.ContextPerRequest,
		MaxConcurrency:       runtime.Metadata.MaxConcurrency,
		GPUMemoryUtilization: runtime.Metadata.GPUMemoryUtilization,
		TensorParallelSize:   runtime.Metadata.TensorParallelSize,
	}
	rendered := spec.BuildArgs(kind, false)
	if len(rendered) == 0 {
		return entrypoint, nil
	}
	present := make(map[string]bool, len(model.Backend.Arguments))
	for _, token := range SplitManagedLaunchTokens(kind, model.Backend.Arguments) {
		if name := flagName(token); name != "" {
			present[name] = true
		}
	}
	defaults := make([]string, 0, len(rendered))
	for index := 0; index < len(rendered); index++ {
		token := rendered[index]
		name := flagName(token)
		if name == "" || present[name] {
			continue
		}
		if strings.Contains(token, "=") {
			defaults = append(defaults, token)
			continue
		}
		if index+1 < len(rendered) {
			defaults = append(defaults, token, rendered[index+1])
			index++
			continue
		}
		defaults = append(defaults, token)
	}
	return entrypoint, defaults
}

// RuntimeMetadataConfig is a runtime's launch metadata.
type RuntimeMetadataConfig struct {
	// EntryPoint is the launch head for models on this runtime, for example
	// "python3 /scripts/server.py". Empty keeps the runtime kind's canonical
	// entrypoint (`vllm serve` / `llama-server`). When set, the quick-config
	// defaults below are not rendered: a custom head owns its whole command
	// line, so engine flags would not apply.
	EntryPoint string `yaml:"entrypoint" json:"entrypoint,omitempty"`
	// The quick-config defaults, rendered as engine flags for every model on
	// this runtime that does not pin the flag in its own args.
	ContextPerRequest    int      `yaml:"contextPerRequest" json:"contextPerRequest,omitempty"`
	MaxConcurrency       int      `yaml:"maxConcurrency" json:"maxConcurrency,omitempty"`
	TensorParallelSize   int      `yaml:"tensorParallelSize" json:"tensorParallelSize,omitempty"`
	GPUMemoryUtilization *float64 `yaml:"gpuMemoryUtilization" json:"gpuMemoryUtilization,omitempty"`
}

// Empty reports a metadata block that would add nothing.
func (m RuntimeMetadataConfig) Empty() bool {
	return strings.TrimSpace(m.EntryPoint) == "" && m.ContextPerRequest == 0 &&
		m.MaxConcurrency == 0 && m.TensorParallelSize == 0 && m.GPUMemoryUtilization == nil
}

type RuntimeSource struct {
	Type       string `yaml:"type" json:"type"` // bundled|release|channel|pypi|wheel|git|tag|commit|local|image
	Repository string `yaml:"repository" json:"repository"`
	// Asset selects one GitHub Release asset for a native vLLM release
	// channel. It is an exact asset-name template: {tag} expands to the
	// upstream tag (for example v1.5.0) and {version} to its normalized
	// version (for example 1.5.0).
	Asset      string `yaml:"asset" json:"asset"`
	TrackRef   string `yaml:"trackRef" json:"trackRef"`
	Ref        string `yaml:"ref" json:"ref"`
	URL        string `yaml:"url" json:"url"`
	Path       string `yaml:"path" json:"path"`
	Image      string `yaml:"image" json:"image"`
	Platform   string `yaml:"platform" json:"platform"`
	PullPolicy string `yaml:"pullPolicy" json:"pullPolicy"` // always|if-missing|never
	Checksum   string `yaml:"checksum" json:"checksum"`
}

type RuntimeBuildConfig struct {
	Driver            string             `yaml:"driver" json:"driver"` // cmake|make|uv|custom
	WorkDir           string             `yaml:"workDir" json:"workDir"`
	Backend           string             `yaml:"backend" json:"backend"`
	Compiler          string             `yaml:"compiler" json:"compiler"`
	CUDAArchitectures []string           `yaml:"cudaArchitectures" json:"cudaArchitectures"`
	CMake             []string           `yaml:"cmake" json:"cmake"`
	Python            string             `yaml:"python" json:"python"`
	Extras            []string           `yaml:"extras" json:"extras"`
	IndexURL          string             `yaml:"indexURL" json:"indexURL"`
	Env               map[string]string  `yaml:"env" json:"env"`
	Args              []string           `yaml:"args" json:"args"`
	InstallArgs       []string           `yaml:"installArgs" json:"installArgs"`
	Steps             []RuntimeBuildStep `yaml:"steps" json:"steps"`
	Artifacts         []RuntimeArtifact  `yaml:"artifacts" json:"artifacts"`
}

// RuntimeBuildStep is a shell-free command executed inside a staged runtime.
// WorkDir is relative to the candidate root and Env is merged over the
// process environment for this command only.
type RuntimeBuildStep struct {
	WorkDir string            `yaml:"workDir" json:"workDir"`
	Command string            `yaml:"command" json:"command"`
	Args    []string          `yaml:"args" json:"args"`
	Env     map[string]string `yaml:"env" json:"env"`
}

// RuntimeArtifact copies one build output into the immutable version. A
// provider validates both paths and refuses traversal or symlink escapes.
type RuntimeArtifact struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// RuntimeContainerConfig contains the structured subset of `docker run` that
// llama-swap manages. Arbitrary shell snippets are intentionally unsupported;
// additional flags can be added as typed fields without granting a runtime
// definition an implicit shell.
type RuntimeContainerConfig struct {
	Engine      string            `yaml:"engine" json:"engine"` // docker|podman
	Name        string            `yaml:"name" json:"name"`
	Image       string            `yaml:"image" json:"image"`
	Platform    string            `yaml:"platform" json:"platform"`
	PullPolicy  string            `yaml:"pullPolicy" json:"pullPolicy"` // always|if-missing|never
	Entrypoint  []string          `yaml:"entrypoint" json:"entrypoint"`
	Command     []string          `yaml:"command" json:"command"`
	Env         map[string]string `yaml:"env" json:"env"`
	Mounts      []RuntimeMount    `yaml:"mounts" json:"mounts"`
	Ports       []RuntimePort     `yaml:"ports" json:"ports"`
	GPUs        string            `yaml:"gpus" json:"gpus"`
	ShmSize     string            `yaml:"shmSize" json:"shmSize"`
	StopTimeout time.Duration     `yaml:"stopTimeout" json:"stopTimeout"`
}

// UnmarshalYAML keeps container launch settings typed while allowing
// stopTimeout values such as "10s" in YAML. All other fields are decoded via
// a non-recursive auxiliary representation so the public struct remains
// source-compatible with existing callers.
func (c *RuntimeContainerConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawRuntimeContainerConfig struct {
		Engine      string            `yaml:"engine"`
		Name        string            `yaml:"name"`
		Image       string            `yaml:"image"`
		Platform    string            `yaml:"platform"`
		PullPolicy  string            `yaml:"pullPolicy"`
		Entrypoint  []string          `yaml:"entrypoint"`
		Command     []string          `yaml:"command"`
		Env         map[string]string `yaml:"env"`
		Mounts      []RuntimeMount    `yaml:"mounts"`
		Ports       []RuntimePort     `yaml:"ports"`
		GPUs        string            `yaml:"gpus"`
		ShmSize     string            `yaml:"shmSize"`
		StopTimeout any               `yaml:"stopTimeout"`
	}
	var raw rawRuntimeContainerConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}
	stopTimeout, err := decodeDuration(raw.StopTimeout, "container.stopTimeout")
	if err != nil {
		return err
	}
	env := make(map[string]string, len(raw.Env))
	for key, value := range raw.Env {
		env[key] = value
	}
	if len(env) == 0 {
		env = nil
	}
	*c = RuntimeContainerConfig{
		Engine: raw.Engine, Name: raw.Name, Image: raw.Image, Platform: raw.Platform,
		PullPolicy: raw.PullPolicy, Entrypoint: append([]string(nil), raw.Entrypoint...),
		Command: append([]string(nil), raw.Command...), Env: env,
		Mounts: append([]RuntimeMount(nil), raw.Mounts...), Ports: append([]RuntimePort(nil), raw.Ports...),
		GPUs: raw.GPUs, ShmSize: raw.ShmSize, StopTimeout: stopTimeout,
	}
	return nil
}

type RuntimeMount struct {
	Source   string `yaml:"source" json:"source"`
	Target   string `yaml:"target" json:"target"`
	ReadOnly bool   `yaml:"readOnly" json:"readOnly"`
}

type RuntimePort struct {
	Host      int `yaml:"host" json:"host"`
	Container int `yaml:"container" json:"container"`
}

type RuntimeUpdateConfig struct {
	Policy            string        `yaml:"policy" json:"policy"`   // disabled|manual|automatic|pinned
	Channel           string        `yaml:"channel" json:"channel"` // stable|prerelease
	Version           string        `yaml:"version" json:"version"`
	Commit            string        `yaml:"commit" json:"commit"`
	CheckEvery        time.Duration `yaml:"checkEvery" json:"checkEvery"`
	MinIdle           time.Duration `yaml:"minIdle" json:"minIdle"`
	ActivateOnlyIdle  bool          `yaml:"activateOnlyWhenIdle" json:"activateOnlyWhenIdle"`
	KeepVersions      int           `yaml:"keepVersions" json:"keepVersions"`
	RollbackOnFailure bool          `yaml:"rollbackOnFailure" json:"rollbackOnFailure"`

	// YAML booleans and zero-valued numbers need presence bits so Effective
	// can apply product defaults without turning an explicitly configured false
	// or zero into a different policy. They stay private and are omitted from
	// JSON/YAML representations.
	activateOnlyIdleSet  bool
	keepVersionsSet      bool
	rollbackOnFailureSet bool
	checkEverySet        bool
	minIdleSet           bool
}

// UnmarshalYAML records which optional update fields were present. The
// exported fields remain plain bool/int/duration values for compatibility with
// existing callers, while Effective can still distinguish omitted defaults
// from an explicit false or zero.
func (c *RuntimeUpdateConfig) UnmarshalYAML(value *yaml.Node) error {
	root := value
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("runtime update must be a mapping")
	}
	// Decode scalar fields through an auxiliary representation so YAML duration
	// strings such as "30m" are accepted while numeric nanoseconds remain
	// backwards compatible with the zero-value Go representation.
	type rawUpdate struct {
		Policy            string `yaml:"policy"`
		Channel           string `yaml:"channel"`
		Version           string `yaml:"version"`
		Commit            string `yaml:"commit"`
		CheckEvery        any    `yaml:"checkEvery"`
		MinIdle           any    `yaml:"minIdle"`
		ActivateOnlyIdle  bool   `yaml:"activateOnlyWhenIdle"`
		KeepVersions      int    `yaml:"keepVersions"`
		RollbackOnFailure bool   `yaml:"rollbackOnFailure"`
	}
	var raw rawUpdate
	if err := value.Decode(&raw); err != nil {
		return err
	}
	checkEvery, err := decodeDuration(raw.CheckEvery, "checkEvery")
	if err != nil {
		return err
	}
	minIdle, err := decodeDuration(raw.MinIdle, "minIdle")
	if err != nil {
		return err
	}
	*c = RuntimeUpdateConfig{Policy: raw.Policy, Channel: raw.Channel, Version: raw.Version, Commit: raw.Commit, CheckEvery: checkEvery, MinIdle: minIdle, ActivateOnlyIdle: raw.ActivateOnlyIdle, KeepVersions: raw.KeepVersions, RollbackOnFailure: raw.RollbackOnFailure}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "activateOnlyWhenIdle":
			c.activateOnlyIdleSet = true
		case "keepVersions":
			c.keepVersionsSet = true
		case "rollbackOnFailure":
			c.rollbackOnFailureSet = true
		case "checkEvery":
			c.checkEverySet = true
		case "minIdle":
			c.minIdleSet = true
		}
	}
	return nil
}

func decodeDuration(value any, field string) (time.Duration, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case string:
		duration, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("runtime update %s: invalid duration %q", field, v)
		}
		return duration, nil
	case int:
		return time.Duration(v), nil
	case int64:
		return time.Duration(v), nil
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			return 0, fmt.Errorf("runtime update %s: duration is out of range", field)
		}
		return time.Duration(v), nil
	case float64:
		return time.Duration(v), nil
	default:
		return 0, fmt.Errorf("runtime update %s: duration must be a string or number", field)
	}
}

const (
	DefaultRuntimeCheckEvery = 24 * time.Hour
	DefaultRuntimeMinIdle    = 30 * time.Minute
	DefaultRuntimeKeep       = 2
)

// Effective returns the locked managed-runtime update defaults without
// mutating the loaded configuration. Explicit false/zero values survive when
// they were present in YAML; only omitted fields receive defaults.
func (c RuntimeUpdateConfig) Effective() RuntimeUpdateConfig {
	if strings.TrimSpace(c.Policy) == "" {
		c.Policy = "automatic"
	}
	if strings.TrimSpace(c.Channel) == "" {
		c.Channel = "stable"
	}
	if !c.checkEverySet && c.CheckEvery == 0 {
		c.CheckEvery = DefaultRuntimeCheckEvery
	}
	if !c.minIdleSet && c.MinIdle == 0 {
		c.MinIdle = DefaultRuntimeMinIdle
	}
	if !c.keepVersionsSet && c.KeepVersions == 0 {
		c.KeepVersions = DefaultRuntimeKeep
	}
	if !c.activateOnlyIdleSet {
		c.ActivateOnlyIdle = true
	}
	if !c.rollbackOnFailureSet {
		c.RollbackOnFailure = true
	}
	return c
}

// CheckEveryConfigured reports whether YAML explicitly supplied checkEvery.
// It is used by the runtime manager to preserve an intentional zero (an
// immediate check policy) when translating config into its provider-neutral
// update policy.
func (c RuntimeUpdateConfig) CheckEveryConfigured() bool { return c.checkEverySet }

// MinIdleConfigured reports whether YAML explicitly supplied minIdle. A zero
// value is meaningful for operators who want activation as soon as the idle
// gate opens, so it must not be confused with an omitted field.
func (c RuntimeUpdateConfig) MinIdleConfigured() bool { return c.minIdleSet }

// Effective returns a runtime definition with update policy defaults applied.
// It intentionally leaves source/build/verify values untouched.
func (c RuntimeConfig) Effective() RuntimeConfig {
	c.Update = c.Update.Effective()
	return c
}

type RuntimeVerifyConfig struct {
	SHA256       string `yaml:"sha256" json:"sha256"`
	HealthPath   string `yaml:"healthPath" json:"healthPath"`
	SmokeCommand string `yaml:"smokeCommand" json:"smokeCommand"`
}

type AnthropicConfig struct {
	CacheFix AnthropicCacheFixConfig `yaml:"cacheFix" json:"cacheFix"`
}

type AnthropicCacheFixConfig struct {
	Mode       string                   `yaml:"mode" json:"mode"` // off|auto|force
	Telemetry  bool                     `yaml:"telemetry" json:"telemetry"`
	Transforms AnthropicTransformConfig `yaml:"transforms" json:"transforms"`
}

type AnthropicTransformConfig struct {
	FingerprintStrip      bool   `yaml:"fingerprintStrip" json:"fingerprintStrip"`
	SortStabilization     bool   `yaml:"sortStabilization" json:"sortStabilization"`
	FreshSessionSort      bool   `yaml:"freshSessionSort" json:"freshSessionSort"`
	IdentityNormalization bool   `yaml:"identityNormalization" json:"identityNormalization"`
	CacheControlNormalize bool   `yaml:"cacheControlNormalize" json:"cacheControlNormalize"`
	TTLManagement         bool   `yaml:"ttlManagement" json:"ttlManagement"`
	ThinkingSanitize      string `yaml:"thinkingSanitize" json:"thinkingSanitize"`     // off|safe|experimental
	CCVersionNormalize    string `yaml:"ccVersionNormalize" json:"ccVersionNormalize"` // off|audit|on
	HighRisk              string `yaml:"highRisk" json:"highRisk"`                     // off|audit|on|experimental
}

type AuditConfig struct {
	Enabled       bool          `yaml:"enabled" json:"enabled"`
	Retention     time.Duration `yaml:"retention" json:"retention"`
	MaxBytes      int64         `yaml:"maxBytes" json:"maxBytes"`
	StoreMedia    bool          `yaml:"storeMedia" json:"storeMedia"`
	RedactHeaders bool          `yaml:"redactHeaders" json:"redactHeaders"`
	// ActivityRetention bounds the operational activity rows (the per-request
	// metrics behind the dashboards). It is independent of the audit
	// retention: a deployment can keep conversations for a month while
	// trimming the metric rows that stop being interesting much sooner. Zero
	// keeps them indefinitely, matching the historical behavior.
	ActivityRetention time.Duration `yaml:"activityRetention" json:"activityRetention"`
}

type PricingConfig struct {
	ModelsDev ModelsDevPricingConfig `yaml:"modelsDev" json:"modelsDev"`
}

type ModelsDevPricingConfig struct {
	// Enabled is a pointer so an absent key can be told apart from an explicit
	// false. A plain bool collapses the two, which made any configuration that
	// mentioned pricing without restating the switch silently disable the
	// syncer — and made an explicit `enabled: false` impossible to honor.
	Enabled      *bool         `yaml:"enabled" json:"enabled,omitempty"`
	RefreshEvery time.Duration `yaml:"refreshEvery" json:"refreshEvery"`
	URL          string        `yaml:"url" json:"url"`
	// Ambiguity decides what happens when a model name matches several catalog
	// entries (providers rarely agree on a price): "strict" never guesses,
	// "provider-first" prefers the catalog entry of the provider that served
	// the request and then the only entry with non-zero rates, "lowest" falls
	// back to the cheapest remaining entry after those steps. Empty reads as
	// "provider-first".
	Ambiguity string `yaml:"ambiguity" json:"ambiguity"`
	// Vendors maps a model-name prefix to the pricing-catalog provider id that
	// counts as the model's official vendor ("gemini" -> "google"). The lookup
	// prefers the official vendor's row over resellers' rows. It starts from
	// DefaultPricingVendors; entries here override or extend it.
	Vendors map[string]string `yaml:"vendors" json:"vendors,omitempty"`
	// CostBackfillOnSync runs a historical cost backfill after each successful
	// catalog sync: activity rows recorded before their model had a price are
	// recomputed from the current catalog and persisted. An absent key reads as
	// enabled, same as the syncer itself.
	CostBackfillOnSync *bool `yaml:"costBackfillOnSync" json:"costBackfillOnSync,omitempty"`
}

// DefaultPricingVendors is the built-in prefix -> catalog-provider table used
// to price a model at its official vendor's rate when several resellers list
// the same name. Every mapping was verified against the models.dev snapshot:
// the vendor's own provider entry lists models under that prefix. Prefixes
// match case-insensitively and the longest match wins.
var DefaultPricingVendors = map[string]string{
	// OpenAI: gpt-3.5 through gpt-6.x, the o-series, realtime and image models.
	"gpt": "openai", "o1": "openai", "o3": "openai", "o4": "openai",
	// Google: the gemini family (gemma is not listed under google).
	"gemini": "google",
	// Alibaba: qwen3.x/qwen/qvq/qwq.
	"qwen": "alibaba", "qvq": "alibaba", "qwq": "alibaba",
	// Zhipu (Z.ai is the same vendor's international brand).
	"glm": "zhipuai",
	// DeepSeek, Anthropic, Moonshot, MiniMax, xAI, Xiaomi, StepFun.
	"deepseek": "deepseek",
	"claude":   "anthropic",
	"kimi":     "moonshotai",
	"minimax":  "minimax",
	"grok":     "xai",
	"mimo":     "xiaomi",
	"step":     "stepfun",
	// Mistral: the whole trademark family, not just mistral-*.
	"mistral": "mistral", "ministral": "mistral", "codestral": "mistral",
	"devstral": "mistral", "magistral": "mistral", "pixtral": "mistral",
	"voxtral": "mistral",
	// ByteDance (Volcano Engine), Cohere, Perplexity, Meta, Microsoft.
	"doubao":  "volcengine",
	"command": "cohere",
	"sonar":   "perplexity",
	"muse":    "meta",
	"phi":     "azure",
	"nvidia":  "nvidia",
}

// PricingVendorTable merges the built-in vendor table with operator overrides.
// Prefixes are lowercased for case-insensitive matching; empty entries are
// dropped (configuration validation rejects them, but a programmatically built
// config should not poison the lookup).
func PricingVendorTable(user map[string]string) map[string]string {
	table := make(map[string]string, len(DefaultPricingVendors)+len(user))
	for prefix, vendor := range DefaultPricingVendors {
		table[strings.ToLower(prefix)] = vendor
	}
	for prefix, vendor := range user {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		vendor = strings.TrimSpace(vendor)
		if prefix == "" || vendor == "" {
			continue
		}
		table[prefix] = vendor
	}
	return table
}

// VendorForModel resolves the official vendor for a model name. The longest
// matching prefix wins, so overlapping entries ("step" and "stepfun") resolve
// deterministically.
func VendorForModel(vendors map[string]string, model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	match := ""
	vendor := ""
	for prefix, candidate := range vendors {
		if prefix != "" && strings.HasPrefix(lower, prefix) && len(prefix) > len(match) {
			match = prefix
			vendor = candidate
		}
	}
	return vendor, match != ""
}

// How multi-provider price matches are resolved. The values are part of the
// configuration surface (pricing.modelsDev.ambiguity) and of the schema.
const (
	PricingAmbiguityStrict        = "strict"
	PricingAmbiguityProviderFirst = "provider-first"
	PricingAmbiguityLowest        = "lowest"
)

// ModelsDevEnabled reports whether the catalog syncer should run, defaulting to
// on when the operator has not said otherwise. Writing only `url` or
// `refreshEvery` used to turn synchronization off, because the whole-section
// zero-value check that supplied the default no longer matched.
func (c ModelsDevPricingConfig) ModelsDevEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// CostBackfillEnabled reports whether a successful catalog sync should also
// backfill the cost of activity rows recorded before their model had a price.
// Defaults to on, matching the syncer.
func (c ModelsDevPricingConfig) CostBackfillEnabled() bool {
	return c.CostBackfillOnSync == nil || *c.CostBackfillOnSync
}

// EffectiveAudit returns the selected audit policy with the product defaults
// applied. Keeping the Config zero value unchanged is intentional: it
// preserves equality/serialization behavior for legacy callers and tests.
func (c Config) EffectiveAudit() AuditConfig {
	a := c.Audit
	if a.Retention == 0 {
		a.Retention = 30 * 24 * time.Hour
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = 5 * 1024 * 1024 * 1024
	}
	if c.Audit == (AuditConfig{}) {
		a.Enabled = true
		a.StoreMedia = true
		a.RedactHeaders = true
	}
	return a
}

func (c Config) EffectivePricing() PricingConfig {
	p := c.Pricing
	if p.ModelsDev.URL == "" {
		p.ModelsDev.URL = "https://models.dev/api.json"
	}
	if p.ModelsDev.RefreshEvery == 0 {
		p.ModelsDev.RefreshEvery = 24 * time.Hour
	}
	if strings.TrimSpace(p.ModelsDev.Ambiguity) == "" {
		p.ModelsDev.Ambiguity = PricingAmbiguityProviderFirst
	}
	return p
}

// ValidatePricing rejects ambiguity policies that are not part of the
// configuration surface, so a typo fails at load time instead of silently
// reading as the default, and vendor entries that could never match.
func (c Config) ValidatePricing() error {
	switch strings.TrimSpace(c.Pricing.ModelsDev.Ambiguity) {
	case "", PricingAmbiguityStrict, PricingAmbiguityProviderFirst, PricingAmbiguityLowest:
	default:
		return fmt.Errorf("pricing.modelsDev.ambiguity must be one of %q, %q or %q",
			PricingAmbiguityStrict, PricingAmbiguityProviderFirst, PricingAmbiguityLowest)
	}
	for prefix, vendor := range c.Pricing.ModelsDev.Vendors {
		if strings.TrimSpace(prefix) == "" {
			return fmt.Errorf("pricing.modelsDev.vendors: model-name prefix must not be empty")
		}
		if strings.TrimSpace(vendor) == "" {
			return fmt.Errorf("pricing.modelsDev.vendors: vendor for prefix %q must not be empty", prefix)
		}
	}
	return nil
}

// ValidateExtensionConfig validates only the extension fields. It is called
// by the existing loader after legacy normalization so old configurations are
// unaffected when these sections are absent.
func (c Config) ValidateExtensionConfig() error {
	if err := c.Extensions.Validate(); err != nil {
		return err
	}
	if err := c.ModelFiles.Validate(); err != nil {
		return err
	}
	if err := c.ResourceBudget.Validate(); err != nil {
		return err
	}
	if err := c.ValidatePricing(); err != nil {
		return err
	}
	if c.RuntimeManager.Root != "" {
		if !filepath.IsAbs(c.RuntimeManager.Root) {
			return fmt.Errorf("runtimeManager.root must be an absolute path")
		}
		if strings.ContainsRune(c.RuntimeManager.Root, '\x00') {
			return fmt.Errorf("runtimeManager.root contains NUL")
		}
	}
	if c.RuntimeManager.OperationTimeout < 0 {
		return fmt.Errorf("runtimeManager.operationTimeout must be >= 0")
	}
	for i, source := range c.RuntimeManager.SourceAllowlist {
		if err := validateRuntimeAllowlistEntry(source); err != nil {
			return fmt.Errorf("runtimeManager.sourceAllowlist[%d]: %w", i, err)
		}
	}
	for i, registry := range c.RuntimeManager.ContainerRegistries {
		if err := validateContainerRegistry(registry); err != nil {
			return fmt.Errorf("runtimeManager.containerRegistries[%d]: %w", i, err)
		}
	}
	if err := c.Anthropic.CacheFix.Validate(); err != nil {
		return err
	}
	if c.Audit.Retention < 0 || c.Audit.MaxBytes < 0 || c.Audit.ActivityRetention < 0 {
		return fmt.Errorf("audit retention, activityRetention and maxBytes must be >= 0")
	}
	if c.Pricing.ModelsDev.URL != "" {
		u, err := url.Parse(c.Pricing.ModelsDev.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("pricing.modelsDev.url must be an absolute HTTPS URL without credentials")
		}
	}
	for name, rt := range c.Runtimes {
		if !safeRuntimeToken(name) {
			return fmt.Errorf("runtime name %q contains invalid characters", name)
		}
		if name == LMCacheRuntimeName {
			return fmt.Errorf("runtime name %q is reserved for the derived LMCache server runtime; configure the lmcache section instead", name)
		}
		if err := rt.Validate(name); err != nil {
			return err
		}
	}
	if err := c.Network.Validate(); err != nil {
		return err
	}
	if err := c.LMCache.Validate(); err != nil {
		return err
	}
	// Migrate once, before the validation loop: the migration mutates the
	// shared Models map, so running it inside the loop validated a mix of
	// pre- and post-migration model copies and re-ran the whole scan per
	// model (O(n²)). A legacy structured launch block is folded into
	// backend.args here, making the command text the single source of truth
	// for the rest of the pipeline.
	c.MigrateLaunchBlocks()
	for name, model := range c.Models {
		if err := model.Backend.Validate(name); err != nil {
			return err
		}
		if err := c.validateManagedRuntimeReference(name, model); err != nil {
			return err
		}
	}
	return nil
}

// validateManagedRuntimeReference makes backend.runtime a real configuration
// contract rather than a best-effort process-launch hint. The Server binds a
// native runtime to its durable current pointer, so accepting a missing,
// mismatched, or legacy-shell configuration here would make config validation
// claim success and fail only when a user later loads the model.
func (c Config) validateManagedRuntimeReference(modelName string, model ModelConfig) error {
	runtimeName := strings.TrimSpace(model.Backend.Runtime)
	if runtimeName == "" {
		return nil
	}
	if !safeRuntimeToken(runtimeName) {
		return fmt.Errorf("model %s: backend.runtime contains invalid characters", modelName)
	}
	runtimeConfig, found := c.Runtimes[runtimeName]
	if !found {
		return fmt.Errorf("model %s: backend.runtime %q does not exist", modelName, runtimeName)
	}

	runtimeKind := strings.ToLower(strings.TrimSpace(runtimeConfig.Kind))
	backendKind := strings.ToLower(strings.TrimSpace(model.Backend.Type))
	if backendKind != runtimeKind {
		return fmt.Errorf("model %s: backend.type %q must match runtime %q kind %q", modelName, model.Backend.Type, runtimeName, runtimeConfig.Kind)
	}
	if runtimeKind != "vllm" && runtimeKind != "llamacpp" {
		return fmt.Errorf("model %s: runtime %q has unsupported kind %q", modelName, runtimeName, runtimeConfig.Kind)
	}
	if model.Backend.LMCache != nil && runtimeKind != "vllm" {
		return fmt.Errorf("model %s: backend.lmcache requires a vllm runtime", modelName)
	}

	if effectiveRuntimeMode(runtimeConfig) == "container" {
		if strings.TrimSpace(model.Cmd) != "" {
			return fmt.Errorf("model %s: container backend.runtime %q cannot use legacy cmd", modelName, runtimeName)
		}
		if strings.TrimSpace(model.Backend.Container.Image) == "" && strings.TrimSpace(runtimeConfig.Container.Image) == "" && strings.TrimSpace(runtimeConfig.Source.Image) == "" {
			return fmt.Errorf("model %s: container backend.runtime %q requires a runtime or backend container image", modelName, runtimeName)
		}
		if model.Backend.LMCache != nil {
			return fmt.Errorf("model %s: backend.lmcache requires a native vllm runtime", modelName)
		}
		return nil
	}

	if strings.TrimSpace(model.Cmd) != "" {
		return fmt.Errorf("model %s: native backend.runtime %q cannot use legacy cmd; use backend.args", modelName, runtimeName)
	}
	if strings.TrimSpace(model.Backend.Container.Image) != "" {
		return fmt.Errorf("model %s: native backend.runtime %q cannot use backend.container", modelName, runtimeName)
	}
	if len(model.Backend.Arguments) == 0 && strings.TrimSpace(model.Backend.EntryPoint) == "" {
		return fmt.Errorf("model %s: native backend.runtime %q requires backend.args", modelName, runtimeName)
	}
	// A custom launch head owns its whole command line: the flags a kind would
	// inject do not apply, so only the token list is validated.
	if strings.TrimSpace(model.Backend.EntryPoint) != "" {
		return nil
	}
	// Engine flags are owned by backend.args; the builder only needs a model
	// target, which may appear anywhere in the operator's argument list.
	if _, err := RebuildManagedLaunchArguments(model.Backend.Arguments, runtimeKind, model.Proxy); err != nil {
		return fmt.Errorf("model %s: native backend.runtime %q: %w", modelName, runtimeName, err)
	}
	return nil
}

func effectiveRuntimeMode(value RuntimeConfig) string {
	mode := strings.ToLower(strings.TrimSpace(value.Mode))
	if mode != "" {
		return mode
	}
	if strings.TrimSpace(value.Source.Image) != "" || strings.TrimSpace(value.Container.Image) != "" {
		return "container"
	}
	return "native"
}

func (c AnthropicCacheFixConfig) Validate() error {
	if c.Mode != "" && c.Mode != "off" && c.Mode != "auto" && c.Mode != "force" {
		return fmt.Errorf("anthropic.cacheFix.mode must be one of: off, auto, force")
	}
	if c.Transforms.ThinkingSanitize != "" && c.Transforms.ThinkingSanitize != "off" && c.Transforms.ThinkingSanitize != "safe" && c.Transforms.ThinkingSanitize != "experimental" {
		return fmt.Errorf("anthropic.cacheFix.transforms.thinkingSanitize must be one of: off, safe, experimental")
	}
	if c.Transforms.CCVersionNormalize != "" && c.Transforms.CCVersionNormalize != "off" && c.Transforms.CCVersionNormalize != "audit" && c.Transforms.CCVersionNormalize != "on" {
		return fmt.Errorf("anthropic.cacheFix.transforms.ccVersionNormalize must be one of: off, audit, on")
	}
	if c.Transforms.HighRisk != "" && c.Transforms.HighRisk != "off" && c.Transforms.HighRisk != "audit" && c.Transforms.HighRisk != "on" && c.Transforms.HighRisk != "experimental" {
		return fmt.Errorf("anthropic.cacheFix.transforms.highRisk has an invalid mode")
	}
	return nil
}

func (r RuntimeConfig) Validate(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("runtime name cannot be empty")
	}
	if r.Kind != "" && r.Kind != "llamacpp" && r.Kind != "vllm" {
		return fmt.Errorf("runtime %s: kind must be llamacpp or vllm", name)
	}
	mode := strings.ToLower(strings.TrimSpace(r.Mode))
	if r.Mode != "" && r.Mode != mode {
		return fmt.Errorf("runtime %s: mode must be normalized", name)
	}
	if mode != "" && mode != "native" && mode != "container" {
		return fmt.Errorf("runtime %s: mode must be native or container", name)
	}
	sourceType := strings.ToLower(strings.TrimSpace(r.Source.Type))
	if r.Source.Type != "" && r.Source.Type != sourceType {
		return fmt.Errorf("runtime %s: source.type must be normalized", name)
	}
	switch sourceType {
	case "", "bundled", "release", "channel", "pypi", "wheel", "git", "tag", "commit", "local", "image":
	default:
		return fmt.Errorf("runtime %s: source.type is invalid", name)
	}
	if sourceType == "image" && mode == "native" {
		return fmt.Errorf("runtime %s: source.type image requires mode container", name)
	}
	for field, value := range map[string]string{
		"source.repository":   r.Source.Repository,
		"source.asset":        r.Source.Asset,
		"source.trackRef":     r.Source.TrackRef,
		"source.ref":          r.Source.Ref,
		"source.url":          r.Source.URL,
		"source.path":         r.Source.Path,
		"source.image":        r.Source.Image,
		"source.platform":     r.Source.Platform,
		"source.pullPolicy":   r.Source.PullPolicy,
		"source.checksum":     r.Source.Checksum,
		"build.driver":        r.Build.Driver,
		"build.workDir":       r.Build.WorkDir,
		"build.backend":       r.Build.Backend,
		"build.compiler":      r.Build.Compiler,
		"build.python":        r.Build.Python,
		"build.indexURL":      r.Build.IndexURL,
		"verify.sha256":       r.Verify.SHA256,
		"verify.healthPath":   r.Verify.HealthPath,
		"verify.smokeCommand": r.Verify.SmokeCommand,
	} {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("runtime %s: %s contains NUL", name, field)
		}
	}
	for i, value := range append(append([]string{}, r.Build.CUDAArchitectures...), append(r.Build.CMake, r.Build.Extras...)...) {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("runtime %s: build argument %d contains NUL", name, i)
		}
	}
	if err := validateRuntimeBuild(name, r.Build); err != nil {
		return err
	}
	if err := validateRuntimeContainer(name, r.Container); err != nil {
		return err
	}
	if err := r.Metadata.Validate(name, mode); err != nil {
		return err
	}
	if r.Update.Policy != "" && r.Update.Policy != "disabled" && r.Update.Policy != "manual" && r.Update.Policy != "automatic" && r.Update.Policy != "pinned" {
		return fmt.Errorf("runtime %s: update.policy is invalid", name)
	}
	if r.Update.Channel != "" && r.Update.Channel != "stable" && r.Update.Channel != "prerelease" {
		return fmt.Errorf("runtime %s: update.channel is invalid", name)
	}
	if r.Update.CheckEvery < 0 || r.Update.MinIdle < 0 || r.Update.KeepVersions < 0 {
		return fmt.Errorf("runtime %s: update durations and keepVersions must be >= 0", name)
	}
	if r.Update.Version != "" && !safeRuntimeToken(r.Update.Version) {
		return fmt.Errorf("runtime %s: update.version contains invalid characters", name)
	}
	if r.Update.Commit != "" && !safeRuntimeToken(r.Update.Commit) {
		return fmt.Errorf("runtime %s: update.commit contains invalid characters", name)
	}
	if err := validateChecksum(r.Source.Checksum, "source.checksum", name); err != nil {
		return err
	}
	if err := validateChecksum(r.Verify.SHA256, "verify.sha256", name); err != nil {
		return err
	}
	if r.Source.Repository != "" && !validRepositoryURL(r.Source.Repository) {
		return fmt.Errorf("runtime %s: source.repository must be an HTTPS or git@ URL without credentials", name)
	}
	if r.Source.Asset != "" {
		if err := validateRuntimeReleaseAssetTemplate(r.Source.Asset); err != nil {
			return fmt.Errorf("runtime %s: source.asset %w", name, err)
		}
	}
	if r.Source.URL != "" {
		u, err := url.Parse(r.Source.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "file") || u.User != nil || (u.Scheme == "https" && u.Host == "") || (u.Scheme == "file" && u.Host != "") {
			return fmt.Errorf("runtime %s: source.url must be HTTPS or file URL", name)
		}
		if u.Scheme == "file" {
			pathValue := u.Path
			if escaped := u.EscapedPath(); escaped != "" {
				decoded, decodeErr := url.PathUnescape(escaped)
				if decodeErr != nil {
					return fmt.Errorf("runtime %s: source.url file path is invalid", name)
				}
				pathValue = decoded
			}
			if !isAbsConfigPath(pathValue) || hasParentPathSegment(pathValue) {
				return fmt.Errorf("runtime %s: source.url file path must be absolute and cannot contain parent segments", name)
			}
		}
	}
	if r.Source.Path != "" {
		if !isAbsConfigPath(r.Source.Path) {
			return fmt.Errorf("runtime %s: source.path must be an absolute path", name)
		}
		if hasParentPathSegment(r.Source.Path) {
			return fmt.Errorf("runtime %s: source.path cannot contain parent segments", name)
		}
	}
	if r.Source.TrackRef != "" && !safeRuntimeRef(r.Source.TrackRef) {
		return fmt.Errorf("runtime %s: source.trackRef contains invalid characters", name)
	}
	if r.Source.Ref != "" && !safeRuntimeRef(r.Source.Ref) {
		return fmt.Errorf("runtime %s: source.ref contains invalid characters", name)
	}
	configuredImage := strings.TrimSpace(r.Source.Image)
	if configuredImage == "" {
		configuredImage = strings.TrimSpace(r.Container.Image)
	}
	if r.Source.Image != "" {
		if err := validateContainerImage(r.Source.Image); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	if r.Source.Platform != "" {
		if err := validateContainerPlatform(r.Source.Platform); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	pullPolicy := strings.ToLower(strings.TrimSpace(r.Source.PullPolicy))
	if r.Source.PullPolicy != "" && r.Source.PullPolicy != pullPolicy {
		return fmt.Errorf("runtime %s: source.pullPolicy must be normalized", name)
	}
	if pullPolicy != "" && pullPolicy != "always" && pullPolicy != "if-missing" && pullPolicy != "never" {
		return fmt.Errorf("runtime %s: source.pullPolicy must be always, if-missing, or never", name)
	}
	if mode == "container" || sourceType == "image" {
		if configuredImage == "" {
			return fmt.Errorf("runtime %s: container mode requires source.image or container.image", name)
		}
		if sourceType != "" && sourceType != "image" {
			return fmt.Errorf("runtime %s: container mode requires source.type image", name)
		}
	}
	switch sourceType {
	case "local":
		if r.Source.Path == "" && r.Source.URL == "" {
			return fmt.Errorf("runtime %s: local source requires source.path or source.url", name)
		}
	case "release":
		if r.Kind == "vllm" {
			if r.Source.Repository == "" || r.Source.Asset == "" {
				return fmt.Errorf("runtime %s: vLLM release source requires source.repository and source.asset", name)
			}
			if r.Source.URL != "" || !validGitHubReleaseRepository(r.Source.Repository) {
				return fmt.Errorf("runtime %s: vLLM release source requires a canonical GitHub source.repository and no source.url", name)
			}
			break
		}
		if r.Source.URL == "" {
			return fmt.Errorf("runtime %s: %s source requires source.url", name, sourceType)
		}
	case "channel":
		if r.Source.URL == "" {
			return fmt.Errorf("runtime %s: %s source requires source.url", name, sourceType)
		}
	case "git", "tag", "commit":
		if r.Source.Repository == "" && r.Source.URL == "" {
			return fmt.Errorf("runtime %s: %s source requires source.repository or source.url", name, sourceType)
		}
		if strings.TrimSpace(r.Source.Ref) == "" && strings.TrimSpace(r.Source.TrackRef) == "" {
			return fmt.Errorf("runtime %s: %s source requires an exact source.ref", name, sourceType)
		}
	case "image":
		if configuredImage == "" {
			return fmt.Errorf("runtime %s: image source requires source.image or container.image", name)
		}
	}
	return nil
}

func validateRuntimeBuild(name string, build RuntimeBuildConfig) error {
	driver := strings.ToLower(strings.TrimSpace(build.Driver))
	if build.Driver != "" && build.Driver != driver {
		return fmt.Errorf("runtime %s: build.driver must be normalized", name)
	}
	switch driver {
	case "", "cmake", "make", "uv", "none", "custom":
	default:
		return fmt.Errorf("runtime %s: build.driver is invalid", name)
	}
	if driver == "none" && len(build.Steps) > 0 {
		return fmt.Errorf("runtime %s: build.driver none does not accept build.steps", name)
	}
	if build.WorkDir != "" {
		if err := validateRelativeRuntimePath(build.WorkDir, "build.workDir"); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	if build.Compiler != "" {
		if err := validateRuntimeToken(build.Compiler, "build.compiler"); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	for field, values := range map[string][]string{
		"build.cmake": build.CMake, "build.cudaArchitectures": build.CUDAArchitectures,
		"build.extras": build.Extras, "build.args": build.Args, "build.installArgs": build.InstallArgs,
	} {
		if err := validateRuntimeArgv(field, values); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	if err := validateRuntimeEnv("build.env", build.Env); err != nil {
		return fmt.Errorf("runtime %s: %w", name, err)
	}
	if len(build.Steps) > 128 {
		return fmt.Errorf("runtime %s: build.steps has too many entries", name)
	}
	for i, step := range build.Steps {
		if strings.TrimSpace(step.Command) == "" || strings.TrimSpace(step.Command) != step.Command {
			return fmt.Errorf("runtime %s: build.steps[%d].command is required and must be normalized", name, i)
		}
		if err := validateRuntimeToken(step.Command, fmt.Sprintf("build.steps[%d].command", i)); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
		if step.WorkDir != "" {
			if err := validateRelativeRuntimePath(step.WorkDir, fmt.Sprintf("build.steps[%d].workDir", i)); err != nil {
				return fmt.Errorf("runtime %s: %w", name, err)
			}
		}
		if err := validateRuntimeArgv(fmt.Sprintf("build.steps[%d].args", i), step.Args); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
		if err := validateRuntimeEnv(fmt.Sprintf("build.steps[%d].env", i), step.Env); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	if len(build.Artifacts) > 128 {
		return fmt.Errorf("runtime %s: build.artifacts has too many entries", name)
	}
	for i, artifact := range build.Artifacts {
		if err := validateRelativeRuntimePath(artifact.From, fmt.Sprintf("build.artifacts[%d].from", i)); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
		if err := validateRelativeRuntimePath(artifact.To, fmt.Sprintf("build.artifacts[%d].to", i)); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	return nil
}

func validateRuntimeContainer(name string, container RuntimeContainerConfig) error {
	engine := strings.ToLower(strings.TrimSpace(container.Engine))
	if container.Engine != "" && container.Engine != engine {
		return fmt.Errorf("runtime %s: container.engine must be normalized", name)
	}
	if engine != "" && engine != "docker" && engine != "podman" {
		return fmt.Errorf("runtime %s: container.engine must be docker or podman", name)
	}
	if container.Name != "" {
		if err := validateContainerName(container.Name); err != nil {
			return fmt.Errorf("runtime %s: %w", name, err)
		}
	}
	if container.Image != "" {
		if err := validateContainerImage(container.Image); err != nil {
			return fmt.Errorf("runtime %s: container.image: %w", name, err)
		}
	}
	if container.Platform != "" {
		if err := validateContainerPlatform(container.Platform); err != nil {
			return fmt.Errorf("runtime %s: container.platform: %w", name, err)
		}
	}
	pullPolicy := strings.ToLower(strings.TrimSpace(container.PullPolicy))
	if container.PullPolicy != "" && container.PullPolicy != pullPolicy {
		return fmt.Errorf("runtime %s: container.pullPolicy must be normalized", name)
	}
	if pullPolicy != "" && pullPolicy != "always" && pullPolicy != "if-missing" && pullPolicy != "never" {
		return fmt.Errorf("runtime %s: container.pullPolicy must be always, if-missing, or never", name)
	}
	if err := validateRuntimeArgv("container.entrypoint", container.Entrypoint); err != nil {
		return fmt.Errorf("runtime %s: %w", name, err)
	}
	if err := validateRuntimeArgv("container.command", container.Command); err != nil {
		return fmt.Errorf("runtime %s: %w", name, err)
	}
	if err := validateRuntimeEnv("container.env", container.Env); err != nil {
		return fmt.Errorf("runtime %s: %w", name, err)
	}
	for i, mount := range container.Mounts {
		if strings.TrimSpace(mount.Source) == "" || strings.TrimSpace(mount.Target) == "" {
			return fmt.Errorf("runtime %s: container.mounts[%d] requires source and target", name, i)
		}
		if strings.ContainsAny(mount.Source+mount.Target, "\x00\r\n") {
			return fmt.Errorf("runtime %s: container.mounts[%d] contains control characters", name, i)
		}
	}
	for i, port := range container.Ports {
		if port.Host < 1 || port.Host > 65535 || port.Container < 1 || port.Container > 65535 {
			return fmt.Errorf("runtime %s: container.ports[%d] must be between 1 and 65535", name, i)
		}
	}
	for field, value := range map[string]string{"container.gpus": container.GPUs, "container.shmSize": container.ShmSize} {
		if value != "" {
			if err := validateRuntimeToken(value, field); err != nil {
				return fmt.Errorf("runtime %s: %w", name, err)
			}
		}
	}
	if container.StopTimeout < 0 {
		return fmt.Errorf("runtime %s: container.stopTimeout must be >= 0", name)
	}
	return nil
}

func validateRuntimeArgv(field string, values []string) error {
	if len(values) > 512 {
		return fmt.Errorf("%s has too many arguments", field)
	}
	for i, value := range values {
		if err := validateRuntimeToken(value, fmt.Sprintf("%s[%d]", field, i)); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeEnv(field string, values map[string]string) error {
	if len(values) > 256 {
		return fmt.Errorf("%s has too many entries", field)
	}
	for key, value := range values {
		if !isEnvName(key) {
			return fmt.Errorf("%s has invalid environment key %q", field, key)
		}
		if err := validateRuntimeToken(value, field+"."+key); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeToken(value, field string) error {
	if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is too long or contains NUL", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}

func validateRelativeRuntimePath(value, field string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') || filepath.IsAbs(value) || hasParentPathSegment(value) {
		return fmt.Errorf("%s must be a non-empty relative path without parent segments", field)
	}
	if err := validateRuntimeToken(value, field); err != nil {
		return err
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == string(filepath.Separator) || clean == ".." {
		return fmt.Errorf("%s is invalid", field)
	}
	return nil
}

func validateContainerName(value string) error {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return errors.New("container.name is invalid")
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.", r)) {
			return fmt.Errorf("container.name %q contains invalid characters", value)
		}
	}
	return nil
}

func validateContainerImage(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n\t") || strings.Contains(value, "://") || strings.HasPrefix(value, "-") {
		return errors.New("source.image must be a valid OCI image reference")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("source.image contains whitespace or control characters")
		}
	}
	return nil
}

func validateContainerPlatform(value string) error {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return errors.New("source.platform is invalid")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("source.platform contains whitespace or control characters")
		}
	}
	return nil
}

func validateContainerRegistry(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\x00\r\n\t /@") || strings.Contains(value, "://") || len(value) > 255 {
		return errors.New("must be a registry host without credentials or path")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return errors.New("must be a registry host without whitespace")
		}
	}
	return nil
}

func safeRuntimeRef(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || len(value) > 1024 || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".") {
			return false
		}
		for _, r := range segment {
			if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return false
			}
		}
	}
	return true
}

func (b BackendConfig) Validate(model string) error {
	if b.Type != "" && b.Type != "llamacpp" && b.Type != "vllm" && b.Type != "generic" {
		return fmt.Errorf("model %s: backend.type is invalid", model)
	}
	if b.Protocol != "" && b.Protocol != AdapterProtocolNative && b.Protocol != AdapterProtocolResponsesToChat {
		return fmt.Errorf("model %s: backend.protocol is invalid", model)
	}
	if runtimeName := strings.TrimSpace(b.Runtime); runtimeName != "" && !safeRuntimeToken(runtimeName) {
		return fmt.Errorf("model %s: backend.runtime contains invalid characters", model)
	}
	if version := strings.TrimSpace(b.RuntimeVersion); version != "" {
		if !safeRuntimeToken(version) {
			return fmt.Errorf("model %s: backend.runtimeVersion contains invalid characters", model)
		}
		if strings.TrimSpace(b.Runtime) == "" {
			return fmt.Errorf("model %s: backend.runtimeVersion requires backend.runtime", model)
		}
	}
	if b.Lifecycle.Mode != "" && b.Lifecycle.Mode != "process" && b.Lifecycle.Mode != "sleep" {
		return fmt.Errorf("model %s: backend.lifecycle.mode is invalid", model)
	}
	if b.Lifecycle.SleepLevel < 0 || b.Lifecycle.SleepLevel > 2 {
		return fmt.Errorf("model %s: backend.lifecycle.sleepLevel must be 0..2", model)
	}
	if b.Resources.VRAMMiB < 0 || b.Resources.RAMMiB < 0 {
		return fmt.Errorf("model %s: backend resource sizes must be >= 0", model)
	}
	if err := validateRuntimeContainer(model, b.Container); err != nil {
		return err
	}
	for i, api := range b.APIs {
		apiName := strings.ToLower(strings.TrimSpace(api))
		if apiName == "" || strings.ContainsRune(api, '\x00') {
			return fmt.Errorf("model %s: backend.apis[%d] must be non-empty and contain no NUL", model, i)
		}
		if _, ok := backendAPINames[apiName]; !ok {
			return fmt.Errorf("model %s: backend.apis[%d] contains unsupported capability %q", model, i, api)
		}
	}
	for i, arg := range b.Arguments {
		if strings.ContainsRune(arg, '\x00') {
			return fmt.Errorf("model %s: backend.args[%d] contains NUL", model, i)
		}
	}
	for i, gpu := range b.Resources.GPUAffinity {
		if strings.TrimSpace(gpu) == "" || strings.ContainsRune(gpu, '\x00') {
			return fmt.Errorf("model %s: backend.resources.gpuAffinity[%d] must be non-empty and contain no NUL", model, i)
		}
	}
	if b.Pricing.Provider != "" && strings.ContainsRune(b.Pricing.Provider, '\x00') || b.Pricing.Model != "" && strings.ContainsRune(b.Pricing.Model, '\x00') {
		return fmt.Errorf("model %s: backend.pricing contains NUL", model)
	}
	if b.LMCache != nil {
		if b.Type != "" && b.Type != "vllm" {
			return fmt.Errorf("model %s: backend.lmcache is only supported for vllm backends", model)
		}
		if b.LMCache.Enabled && strings.TrimSpace(b.Runtime) == "" {
			return fmt.Errorf("model %s: backend.lmcache requires a native managed vllm backend.runtime", model)
		}
		if err := b.LMCache.Validate(model); err != nil {
			return err
		}
	}
	return nil
}

func validateChecksum(value, field, name string) error {
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "sha256:"))
	if value == "" {
		return nil
	}
	if len(value) != 64 {
		return fmt.Errorf("runtime %s: %s must be a SHA-256 hex value", name, field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("runtime %s: %s must be a SHA-256 hex value", name, field)
	}
	return nil
}

func safeRuntimeToken(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, r := range value {
		if r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

// validateRuntimeReleaseAssetTemplate accepts an exact GitHub Release asset
// name, with only {tag} and {version} placeholders. It deliberately rejects
// path separators so the template cannot be confused with a URL or later
// become a different release-download path after substitution.
func validateRuntimeReleaseAssetTemplate(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 255 || strings.ContainsAny(value, "/\\\x00") {
		return errors.New("must be a non-empty release asset name without path separators")
	}
	remaining := strings.NewReplacer("{tag}", "", "{version}", "").Replace(value)
	if strings.ContainsAny(remaining, "{}") {
		return errors.New("may use only {tag} and {version} placeholders")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("contains whitespace or control characters")
		}
	}
	return nil
}

// validGitHubReleaseRepository is intentionally narrower than the generic
// git repository validator. Native vLLM release tracking consumes GitHub's
// public releases API, so accepting an arbitrary URL here would produce a
// declaration that can never be safely resolved by the provider.
func validGitHubReleaseRepository(raw string) bool {
	if raw != strings.TrimSpace(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	repository := strings.TrimSuffix(parts[1], ".git")
	if repository == "" {
		return false
	}
	for _, value := range []string{parts[0], repository} {
		for _, r := range value {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r)) {
				return false
			}
		}
	}
	return true
}

func validRepositoryURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@") {
		return validGitAllowlistHost(raw)
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.Trim(u.Path, "/") != ""
}

// validateRuntimeAllowlistEntry keeps the source allowlist auditable at
// configuration load time. Entries may be absolute local roots, HTTPS URL
// roots, or git@ SSH repositories. HTTP, URL credentials, relative paths and
// opaque strings are rejected before a provider can use them.
func validateRuntimeAllowlistEntry(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsRune(raw, '\x00') {
		return errors.New("must be a non-empty path or URL")
	}
	if strings.HasPrefix(raw, "git@") {
		if strings.ContainsAny(raw, " \t\r\n") {
			return errors.New("git source must not contain whitespace")
		}
		if !validGitAllowlistHost(raw) {
			return errors.New("git source must include a host and repository")
		}
		return nil
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("URL allowlist entries must be HTTPS without credentials, query, or fragment")
		}
		return nil
	}
	if !isAbsConfigPath(raw) {
		return errors.New("path allowlist entries must be absolute")
	}
	return nil
}

func validGitAllowlistHost(raw string) bool {
	value := strings.TrimPrefix(raw, "git@")
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "[") {
		close := strings.IndexByte(value, ']')
		if close <= 1 || len(value) <= close+1 || value[close+1] != ':' {
			return false
		}
		host := value[1:close]
		repository := value[close+2:]
		return repository != "" && !strings.ContainsAny(host+repository, "[]\\\x00")
	}
	separator := strings.IndexByte(value, ':')
	if separator <= 0 || separator == len(value)-1 {
		return false
	}
	host := value[:separator]
	repository := value[separator+1:]
	return host != "" && repository != "" && !strings.ContainsAny(host+repository, "[]\\\x00")
}

func hasParentPathSegment(pathValue string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(pathValue), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}
