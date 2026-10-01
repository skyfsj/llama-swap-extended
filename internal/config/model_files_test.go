package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfig_ModelFilesLoadAndNormalizeHFCacheAlias(t *testing.T) {
	modelRoot := filepath.ToSlash(filepath.Join(t.TempDir(), "models"))
	cfg, err := LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
modelFiles:
  maxFiles: 12
  maxDepth: 4
  downloads:
    enabled: true
    workers: 2
    fileWorkers: 6
    chunkWorkers: 5
    chunkSizeMiB: 32
    chunkThresholdMiB: 128
    maxRetries: 3
    maxTaskRetries: 4
    retryBackoff: 1s
    hfTokenEnv: TEST_HF_TOKEN
    hfToken: configured-hf-token
    modelScopeBaseURL: https://modelscope.example.test
    modelScopeTokenEnv: TEST_MODELSCOPE_TOKEN
    modelScopeToken: configured-modelscope-token
  sources:
    local:
      type: directory
      path: %q
      recursive: false
    hf:
      type: hf_ceche
startPort: 5800
logToStdout: proxy
models:
  test:
    cmd: echo --port ${PORT}
`, modelRoot)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelFiles.MaxFiles != 12 || cfg.ModelFiles.MaxDepth != 4 {
		t.Fatalf("model file limits = %+v", cfg.ModelFiles)
	}
	if cfg.ModelFiles.Downloads.Enabled == nil || !*cfg.ModelFiles.Downloads.Enabled || cfg.ModelFiles.Downloads.Workers != 2 || cfg.ModelFiles.Downloads.FileWorkers != 6 || cfg.ModelFiles.Downloads.ChunkWorkers != 5 || cfg.ModelFiles.Downloads.ChunkSizeMiB != 32 || cfg.ModelFiles.Downloads.ChunkThresholdMiB != 128 || cfg.ModelFiles.Downloads.MaxRetries != 3 || cfg.ModelFiles.Downloads.MaxTaskRetries != 4 || cfg.ModelFiles.Downloads.RetryBackoff != time.Second || cfg.ModelFiles.Downloads.HFTokenEnv != "TEST_HF_TOKEN" || cfg.ModelFiles.Downloads.HFToken != "configured-hf-token" || cfg.ModelFiles.Downloads.ModelScopeBaseURL != "https://modelscope.example.test" || cfg.ModelFiles.Downloads.ModelScopeTokenEnv != "TEST_MODELSCOPE_TOKEN" || cfg.ModelFiles.Downloads.ModelScopeToken != "configured-modelscope-token" {
		t.Fatalf("download config = %+v", cfg.ModelFiles.Downloads)
	}
	if cfg.ModelFiles.Sources["local"].Recursive == nil || *cfg.ModelFiles.Sources["local"].Recursive {
		t.Fatalf("recursive = %v, want false", cfg.ModelFiles.Sources["local"].Recursive)
	}
	if got := NormalizeModelFileSourceType(cfg.ModelFiles.Sources["hf"].Type); got != ModelFileSourceHFCache {
		t.Fatalf("HF source type = %q", got)
	}
}

func TestConfig_NormalizeModelScopeCacheAliases(t *testing.T) {
	for _, value := range []string{"modelscope_cache", "modelscope-cache", "modelscope", "ms_cache"} {
		if got := NormalizeModelFileSourceType(value); got != ModelFileSourceMSCache {
			t.Fatalf("NormalizeModelFileSourceType(%q) = %q", value, got)
		}
	}
}

func TestModelFilesConfig_ValidateRejectsRelativeDirectory(t *testing.T) {
	err := (ModelFilesConfig{Sources: map[string]ModelFileSourceConfig{
		"local": {Type: ModelFileSourceDirectory, Path: "models"},
	}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("Validate error = %v, want absolute-path error", err)
	}
}
