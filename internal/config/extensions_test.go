package config

import (
	"strings"
	"testing"
)

func TestConfig_ExtensionsDisabledByDefault(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader("models:\n  m:\n    cmd: echo ${PORT}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Extensions.Enabled || cfg.Extensions.EffectiveMaxToolRounds() != 4 {
		t.Fatalf("extensions = %+v", cfg.Extensions)
	}
}

func TestConfig_ExtensionsRequireAbsoluteDirectory(t *testing.T) {
	_, err := LoadConfigFromReader(strings.NewReader("models:\n  m:\n    cmd: echo ${PORT}\nextensions:\n  enabled: true\n  directory: relative\n"))
	if err == nil || !strings.Contains(err.Error(), "extensions.directory") {
		t.Fatalf("error = %v", err)
	}
}

// The continuation ceiling holds the whole conversation, so its default must
// clear the gateway's request body limit: the old fixed 2 MiB failed long
// conversations with a 502 in the middle of a turn.
func TestConfig_ExtensionsContinuationLimit(t *testing.T) {
	defaultBytes := DefaultMaxContinuationMiB << 20
	if got := (ExtensionsConfig{}).EffectiveMaxContinuationBytes(); got != defaultBytes {
		t.Fatalf("default ceiling = %d, want %d", got, defaultBytes)
	}
	if defaultBytes < 64<<20 {
		t.Fatalf("default ceiling %d is below the 64 MiB request body limit", defaultBytes)
	}
	if got := (ExtensionsConfig{MaxContinuationMiB: 8}).EffectiveMaxContinuationBytes(); got != 8<<20 {
		t.Fatalf("configured ceiling = %d, want %d", got, 8<<20)
	}
	if got := (ExtensionsConfig{MaxContinuationMiB: -1}).EffectiveMaxContinuationBytes(); got != defaultBytes {
		t.Fatalf("negative ceiling = %d, want the default %d", got, defaultBytes)
	}
	if err := (ExtensionsConfig{MaxContinuationMiB: 2048}).Validate(); err == nil || !strings.Contains(err.Error(), "maxContinuationMiB") {
		t.Fatalf("error = %v", err)
	}
}
