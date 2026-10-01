package config

import (
	"strings"
	"testing"
)

func TestConfig_LogStorageDefaultsAndLoads(t *testing.T) {
	defaultConfig, err := LoadConfigFromReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if got := defaultConfig.LogStorage.MaxFiles; got != LogStorageDefaultMaxFiles {
		t.Fatalf("default logStorage.maxFiles=%d, want %d", got, LogStorageDefaultMaxFiles)
	}

	configured, err := LoadConfigFromReader(strings.NewReader(`
logStorage:
  path: /var/lib/llama-swap/incidents
  maxFiles: 12
`))
	if err != nil {
		t.Fatalf("load configured log storage: %v", err)
	}
	if configured.LogStorage.Path != "/var/lib/llama-swap/incidents" || configured.LogStorage.MaxFiles != 12 {
		t.Fatalf("log storage=%+v", configured.LogStorage)
	}
}

func TestConfig_LogStorageRejectsInvalidMaxFiles(t *testing.T) {
	for _, value := range []string{"0", "101", "-1"} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader("logStorage:\n  maxFiles: " + value + "\n"))
			if err == nil || !strings.Contains(err.Error(), "logStorage.maxFiles") {
				t.Fatalf("maxFiles=%s accepted: %v", value, err)
			}
		})
	}
}
