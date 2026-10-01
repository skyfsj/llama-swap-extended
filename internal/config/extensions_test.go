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
