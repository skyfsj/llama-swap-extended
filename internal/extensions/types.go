package extensions

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Match struct {
	Models        []string `json:"models" yaml:"models"`
	ExcludeModels []string `json:"excludeModels" yaml:"excludeModels"`
	Profiles      []string `json:"profiles" yaml:"profiles"`
	Providers     []string `json:"providers" yaml:"providers"`
	Endpoints     []string `json:"endpoints" yaml:"endpoints"`
}

type Permissions struct {
	NetworkHosts []string `json:"networkHosts" yaml:"networkHosts"`
	ReadRoots    []string `json:"readRoots" yaml:"readRoots"`
	WriteRoots   []string `json:"writeRoots" yaml:"writeRoots"`
	Storage      string   `json:"storage,omitempty" yaml:"storage,omitempty"`
}

// Storage permission values:
//
//	""            ephemeral in-memory KV (the default; no data survives restart)
//	"persistent"  ephemeral KV plus a durable per-extension store
//	"none"        storage disabled entirely
const (
	StorageEphemeral  = "ephemeral"
	StoragePersistent = "persistent"
	StorageNone       = "none"
)

type Manifest struct {
	ID                   string         `json:"id" yaml:"id"`
	Name                 string         `json:"name" yaml:"name"`
	Description          string         `json:"description" yaml:"description"`
	Enabled              bool           `json:"enabled" yaml:"enabled"`
	Priority             int            `json:"priority" yaml:"priority"`
	Match                Match          `json:"match" yaml:"match"`
	Permissions          Permissions    `json:"permissions" yaml:"permissions"`
	Config               map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
	Timeout              string         `json:"timeout" yaml:"timeout"`
	MaxCPUMillis         int            `json:"maxCpuMillis" yaml:"maxCpuMillis"`
	MaxMemoryMiB         int            `json:"maxMemoryMiB" yaml:"maxMemoryMiB"`
	ContinueOnError      bool           `json:"continueOnError" yaml:"continueOnError"`
	ToolConflict         string         `json:"toolConflict" yaml:"toolConflict"`
	InterceptClientTools bool           `json:"interceptClientTools" yaml:"interceptClientTools"`
	// Directories lists empty directories the extension keeps on disk. They
	// survive reloads; anything non-empty lives in the extension's file tree.
	Directories []string `json:"directories,omitempty" yaml:"directories,omitempty"`
}

type Tool struct {
	Type      string          `json:"type"`
	Function  json.RawMessage `json:"function"`
	Execution string          `json:"execution"`
}

// toolHandlerMarker tags a defineTool entry: the SDK sets this key so the
// runtime can strip the handler from the model-facing definition and route
// calls to the bound function.
const toolHandlerMarker = "__llamaSwapTool"

// IsToolHandler reports whether a raw tools-array entry is an SDK defineTool
// definition carrying a bound handler.
func IsToolHandler(raw json.RawMessage) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, ok := probe[toolHandlerMarker]
	return ok
}

// Definition is the wire format of one extension. Files carries the whole
// source tree keyed by its slash-separated path relative to the extension
// directory, so an extension can span several modules that import each other.
// Saving an extension replaces the tree: a file present on disk but absent from
// Files is removed.
type Definition struct {
	Manifest Manifest          `json:"manifest"`
	Files    map[string]string `json:"files"`
	// Directories lists empty directories the extension keeps for structure
	// (e.g. a data dir a script writes into at runtime). They are materialized
	// on save and survive reloads; anything non-empty lives in Files.
	Directories []string       `json:"directories,omitempty"`
	ETag        string         `json:"etag"`
	Status      string         `json:"status"`
	Settings    []SettingField `json:"settings,omitempty"`
	LastError   string         `json:"lastError,omitempty"`
}

// SessionContext is a read-only snapshot of who is calling. It enables
// per-session / per-key behavior (scoped storage, quota checks); it is never
// an authorization token — the chain already decided what the caller may do.
type SessionContext struct {
	ID        string `json:"id"`
	KeyID     string `json:"keyId"`
	Anonymous bool   `json:"anonymous"`
}

// ModelInfo is one model in the snapshot the caller may reach. Scripts use it
// to discover targets for forwarding and routing decisions.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Context struct {
	RequestID      string          `json:"requestId"`
	RequestedModel string          `json:"requestedModel"`
	ResolvedModel  string          `json:"resolvedModel"`
	Profile        string          `json:"profile"`
	Provider       string          `json:"provider"`
	Endpoint       string          `json:"endpoint"`
	Stream         bool            `json:"stream"`
	DryRun         bool            `json:"dryRun,omitempty"`
	Session        *SessionContext `json:"session,omitempty"`
	Locale         string          `json:"locale,omitempty"`
	Models         []ModelInfo     `json:"models,omitempty"`
}

func (m Manifest) Validate() error {
	if m.ID == "" || len(m.ID) > 64 {
		return fmt.Errorf("extension id must contain 1-64 characters")
	}
	for _, ch := range m.ID {
		if ch != '-' && ch != '_' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return fmt.Errorf("extension id %q contains invalid characters", m.ID)
		}
	}
	if m.ToolConflict != "" && m.ToolConflict != "skip" && m.ToolConflict != "override" && m.ToolConflict != "error" {
		return fmt.Errorf("extension %s: toolConflict must be skip, override, or error", m.ToolConflict)
	}
	switch m.Permissions.Storage {
	case "", StorageEphemeral, StoragePersistent, StorageNone:
	default:
		return fmt.Errorf("extension %s: permissions.storage must be ephemeral, persistent, or none", m.ID)
	}
	if m.MaxCPUMillis < 0 || m.MaxMemoryMiB < 0 {
		return fmt.Errorf("extension %s: resource limits must be non-negative", m.ID)
	}
	if m.Timeout != "" {
		value, err := time.ParseDuration(m.Timeout)
		if err != nil {
			return fmt.Errorf("extension %s: timeout: %w", m.ID, err)
		}
		if value <= 0 || value > 5*time.Minute {
			return fmt.Errorf("extension %s: timeout must be between 1ns and 5m", m.ID)
		}
	}
	for _, root := range append(append([]string{}, m.Permissions.ReadRoots...), m.Permissions.WriteRoots...) {
		if !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
			return fmt.Errorf("extension %s: file permission root must be an absolute path", m.ID)
		}
	}
	for _, set := range [][]string{m.Match.Models, m.Match.ExcludeModels, m.Match.Profiles, m.Match.Providers, m.Match.Endpoints} {
		for _, pattern := range set {
			if _, err := path.Match(pattern, "example"); err != nil {
				return fmt.Errorf("extension %s: invalid pattern %q: %w", m.ID, pattern, err)
			}
		}
	}
	for _, pattern := range m.Permissions.NetworkHosts {
		if err := validateHostPattern(pattern); err != nil {
			return fmt.Errorf("extension %s: %w", m.ID, err)
		}
	}
	return nil
}

// validateHostPattern accepts a hostname or a single leading subdomain wildcard
// such as "*.pinecone.io". A bare "*" would hand the extension every host on
// the network, which the per-host check exists to prevent.
func validateHostPattern(pattern string) error {
	if pattern == "" {
		return errors.New("network host pattern must not be empty")
	}
	body := pattern
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		body = rest
		if body == "" || strings.Contains(body, "*") {
			return fmt.Errorf("network host pattern %q may only use a leading wildcard", pattern)
		}
	}
	for _, char := range body {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '-' || char == '.' || char == ':':
		default:
			return fmt.Errorf("network host pattern %q contains invalid characters", pattern)
		}
	}
	if strings.Contains(body, "..") || strings.HasPrefix(body, ".") || strings.HasSuffix(body, ".") {
		return fmt.Errorf("network host pattern %q is not a hostname", pattern)
	}
	return nil
}

func (m Manifest) Matches(ctx Context) bool {
	return matchesAny(m.Match.Models, ctx.ResolvedModel) &&
		!matchesExcluded(m.Match.ExcludeModels, ctx.ResolvedModel) &&
		matchesAny(m.Match.Profiles, ctx.Profile) &&
		matchesAny(m.Match.Providers, ctx.Provider) &&
		matchesAny(m.Match.Endpoints, ctx.Endpoint)
}

func matchesAny(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == value {
			return true
		}
		if ok, _ := path.Match(p, value); ok {
			return true
		}
	}
	return false
}

func matchesExcluded(patterns []string, value string) bool {
	for _, p := range patterns {
		if p == value {
			return true
		}
		if ok, _ := path.Match(p, value); ok {
			return true
		}
	}
	return false
}

func sortedIDs(defs map[string]*Compiled) []string {
	ids := make([]string, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := defs[ids[i]].Manifest, defs[ids[j]].Manifest
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return strings.Compare(a.ID, b.ID) < 0
	})
	return ids
}
