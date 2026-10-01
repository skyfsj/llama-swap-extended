package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

type ExtensionsConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Directory     string `yaml:"directory" json:"directory"`
	MaxToolRounds int    `yaml:"maxToolRounds" json:"maxToolRounds"`
	// MaxForwardDepth bounds nested ctx.models.forward chains; undefined means
	// the default of 2.
	MaxForwardDepth int `yaml:"maxForwardDepth" json:"maxForwardDepth"`
	// Storage limits bound ctx.kv: keys per extension across all scopes, one
	// value's size in MiB, and the sliding TTL of ephemeral entries in minutes.
	StorageMaxKeys      int `yaml:"storageMaxKeys" json:"storageMaxKeys"`
	StorageMaxValueMIB  int `yaml:"storageMaxValueMiB" json:"storageMaxValueMiB"`
	StorageEphemeralTTL int `yaml:"storageEphemeralTtlMinutes" json:"storageEphemeralTtlMinutes"`
	// LogStorage bounds the per-extension log store: total size in MiB and
	// retention in days. Zero values take the defaults (100 MiB / 7 days).
	LogMaxDiskMiB int `yaml:"logMaxDiskMiB" json:"logMaxDiskMiB"`
	LogRetainDays int `yaml:"logRetainDays" json:"logRetainDays"`
}

func (c ExtensionsConfig) EffectiveLogMaxDiskMiB() int {
	if c.LogMaxDiskMiB <= 0 {
		return 100
	}
	return c.LogMaxDiskMiB
}

func (c ExtensionsConfig) EffectiveLogRetainDays() int {
	if c.LogRetainDays <= 0 {
		return 7
	}
	return c.LogRetainDays
}

func (c ExtensionsConfig) EffectiveMaxToolRounds() int {
	if c.MaxToolRounds == 0 {
		return 4
	}
	return c.MaxToolRounds
}

func (c ExtensionsConfig) EffectiveMaxForwardDepth() int {
	if c.MaxForwardDepth <= 0 {
		return 2
	}
	return c.MaxForwardDepth
}

func (c ExtensionsConfig) EffectiveStorageMaxKeys() int {
	if c.StorageMaxKeys <= 0 {
		return 2000
	}
	return c.StorageMaxKeys
}

func (c ExtensionsConfig) EffectiveStorageMaxValueBytes() int {
	if c.StorageMaxValueMIB <= 0 {
		return 256 * 1024 * 1024
	}
	return c.StorageMaxValueMIB * 1024 * 1024
}

func (c ExtensionsConfig) EffectiveStorageEphemeralTTL() int {
	if c.StorageEphemeralTTL <= 0 {
		return 30
	}
	return c.StorageEphemeralTTL
}

func (c ExtensionsConfig) Validate() error {
	if c.MaxToolRounds < 0 || c.MaxToolRounds > 16 {
		return fmt.Errorf("extensions.maxToolRounds must be between 1 and 16")
	}
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Directory) == "" || !filepath.IsAbs(c.Directory) {
		return fmt.Errorf("extensions.directory must be an absolute path when enabled")
	}
	return nil
}
