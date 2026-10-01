package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfig_ModelConfigSanitizedCommand(t *testing.T) {
	config := &ModelConfig{
		Cmd: `python model1.py \
    --arg1 value1 \
    --arg2 value2`,
	}

	args, err := config.SanitizedCommand()
	assert.NoError(t, err)
	assert.Equal(t, []string{"python", "model1.py", "--arg1", "value1", "--arg2", "value2"}, args)
}

func TestConfig_ModelConfigStructuredContainerCommand(t *testing.T) {
	model := &ModelConfig{Backend: BackendConfig{
		Container: RuntimeContainerConfig{
			Engine: "docker", Image: "ghcr.io/ggml-org/llama.cpp:server", Name: "llama-model",
			PullPolicy: "if-missing", Env: map[string]string{"Z": "last", "A": "first"},
			Command: []string{"--model", "/models/model.gguf"},
		},
	}}
	args, err := model.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "run", "--init", "--rm", "--pull", "missing", "--name", "llama-model", "-e", "A=first", "-e", "Z=last", "ghcr.io/ggml-org/llama.cpp:server", "--model", "/models/model.gguf"}
	if len(args) != len(want) {
		t.Fatalf("args=%q, want %q", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args=%q, want %q", args, want)
		}
	}
}

func TestConfig_EnsureVLLMSleepModeArgument(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		backendType string
		ttl         int
		want        []string
	}{
		{
			name:        "positive ttl adds flag",
			args:        []string{"vllm", "serve", "--model", "qwen"},
			backendType: "vllm",
			ttl:         60,
			want:        []string{"vllm", "serve", "--model", "qwen", "--enable-sleep-mode"},
		},
		{
			name:        "zero ttl does not add flag",
			args:        []string{"vllm", "serve", "--model", "qwen"},
			backendType: "vllm",
			want:        []string{"vllm", "serve", "--model", "qwen"},
		},
		{
			name:        "existing flag is not duplicated",
			args:        []string{"vllm", "serve", "--model", "qwen", "--enable-sleep-mode"},
			backendType: "vllm",
			ttl:         60,
			want:        []string{"vllm", "serve", "--model", "qwen", "--enable-sleep-mode"},
		},
		{
			name:        "non vllm is unchanged",
			args:        []string{"llama-server", "--model", "qwen"},
			backendType: "llamacpp",
			ttl:         60,
			want:        []string{"llama-server", "--model", "qwen"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.args...)
			got := EnsureVLLMSleepModeArgument(tt.args, tt.backendType, tt.ttl)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("args=%q, want %q", got, tt.want)
			}
			if strings.Join(tt.args, "\n") != strings.Join(original, "\n") {
				t.Fatalf("input args mutated: got %q, original %q", tt.args, original)
			}
		})
	}
}

func TestConfig_ModelConfigSanitizedCommandInjectsVLLMSleepMode(t *testing.T) {
	withTTL := &ModelConfig{
		Backend: BackendConfig{
			Type:      "vllm",
			Arguments: []string{"vllm", "serve", "--model", "qwen"},
		},
		UnloadAfter: 30,
	}
	args, err := withTTL.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "--enable-sleep-mode" {
		t.Fatalf("last argv=%q, want --enable-sleep-mode; argv=%q", got, args)
	}

	withoutTTL := *withTTL
	withoutTTL.UnloadAfter = 0
	args, err = withoutTTL.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if arg == "--enable-sleep-mode" {
			t.Fatalf("ttl=0 unexpectedly injected sleep flag: %q", args)
		}
	}
}

func TestConfig_ModelConfigSanitizedLegacyCommandInjectsVLLMSleepMode(t *testing.T) {
	model := &ModelConfig{
		Cmd:         "vllm serve --model qwen",
		Backend:     BackendConfig{Type: "vllm"},
		UnloadAfter: 30,
	}

	args, err := model.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "--enable-sleep-mode" {
		t.Fatalf("last argv=%q, want --enable-sleep-mode; argv=%q", got, args)
	}
}

func TestConfig_ModelConfigSanitizedContainerCommandInjectsVLLMSleepMode(t *testing.T) {
	model := &ModelConfig{
		Backend: BackendConfig{
			Type: "vllm",
			Container: RuntimeContainerConfig{
				Engine:  "docker",
				Image:   "vllm/vllm-openai:latest",
				Command: []string{"serve", "qwen"},
			},
		},
		UnloadAfter: 30,
	}

	args, err := model.SanitizedCommand()
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "--enable-sleep-mode" {
		t.Fatalf("last argv=%q, want --enable-sleep-mode; argv=%q", got, args)
	}
}

func TestConfig_ModelFilters(t *testing.T) {
	content := `
macros:
  default_strip: "temperature, top_p"
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    filters:
      # macros inserted and list is cleaned of duplicates and empty strings
      stripParams: "model, top_k, top_k, temperature, ${default_strip}, , ,"
  # check for strip_params (legacy field name) compatibility
  legacy:
    cmd: path/to/cmd --port ${PORT}
    filters:
      strip_params: "model, top_k, top_k, temperature, ${default_strip}, , ,"
`
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	for modelId, modelConfig := range config.Models {
		t.Run(fmt.Sprintf("Testing macros in filters for model %s", modelId), func(t *testing.T) {
			assert.Equal(t, "model, top_k, top_k, temperature, temperature, top_p, , ,", modelConfig.Filters.StripParams)
			sanitized, err := modelConfig.Filters.SanitizedStripParams()
			if assert.NoError(t, err) {
				// model has been removed
				// empty strings have been removed
				// duplicates have been removed
				assert.Equal(t, []string{"temperature", "top_k", "top_p"}, sanitized)
			}
		})
	}
}

func TestConfig_ModelSendLoadingState(t *testing.T) {
	content := `
sendLoadingState: true
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    sendLoadingState: false
  model2:
    cmd: path/to/cmd --port ${PORT}
`
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	assert.True(t, config.SendLoadingState)
	if assert.NotNil(t, config.Models["model1"].SendLoadingState) {
		assert.False(t, *config.Models["model1"].SendLoadingState)
	}
	if assert.NotNil(t, config.Models["model2"].SendLoadingState) {
		assert.True(t, *config.Models["model2"].SendLoadingState)
	}
}

func TestConfig_ComfyUIOverrides(t *testing.T) {
	if ComfyUIModelID != "comfyui_auto" {
		t.Fatalf("ComfyUIModelID=%q want comfyui_auto", ComfyUIModelID)
	}

	tests := []struct {
		name             string
		concurrencyLimit int
		wantLimit        int
	}{
		{name: "unset limit", wantLimit: 50},
		{name: "lower limit", concurrencyLimit: 10, wantLimit: 50},
		{name: "minimum limit", concurrencyLimit: 50, wantLimit: 50},
		{name: "higher limit", concurrencyLimit: 60, wantLimit: 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := fmt.Sprintf(`
models:
  %s:
    cmd: comfyui --port ${PORT}
    concurrencyLimit: %d
    compat:
      ignoreWebsockets: false
  regular:
    cmd: regular --port ${PORT}
    concurrencyLimit: 10
`, ComfyUIModelID, tt.concurrencyLimit)
			cfg, err := LoadConfigFromReader(strings.NewReader(content))
			assert.NoError(t, err)
			assert.Equal(t, tt.wantLimit, cfg.Models[ComfyUIModelID].ConcurrencyLimit)
			assert.True(t, cfg.Models[ComfyUIModelID].Compat.IgnoreWebsockets)
			assert.Equal(t, 10, cfg.Models["regular"].ConcurrencyLimit)
			assert.False(t, cfg.Models["regular"].Compat.IgnoreWebsockets)
		})
	}
}

func TestConfig_ModelCompat(t *testing.T) {
	content := `
models:
  enabled:
    cmd: enabled --port ${PORT}
    compat:
      ignoreWebsockets: true
  default:
    cmd: default --port ${PORT}
`

	cfg, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)
	assert.True(t, cfg.Models["enabled"].Compat.IgnoreWebsockets)
	assert.False(t, cfg.Models["default"].Compat.IgnoreWebsockets)
}

func TestConfig_SetParamsByIDAutoAlias(t *testing.T) {
	content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    filters:
      setParamsByID:
        "${MODEL_ID}:high":
          reasoning_effort: high
        "${MODEL_ID}:low":
          reasoning_effort: low
`
	cfg, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)

	// Keys (other than the model's own ID) should be registered as aliases
	realName, found := cfg.RealModelName("model1:high")
	assert.True(t, found, "model1:high should be an auto-registered alias")
	assert.Equal(t, "model1", realName)

	realName, found = cfg.RealModelName("model1:low")
	assert.True(t, found, "model1:low should be an auto-registered alias")
	assert.Equal(t, "model1", realName)

	// Auto-aliases should also appear in modelConfig.Aliases
	aliases := cfg.Models["model1"].Aliases
	assert.Contains(t, aliases, "model1:high")
	assert.Contains(t, aliases, "model1:low")
}

func TestConfig_SetParamsByIDAutoAliasConflictWithModelID(t *testing.T) {
	content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    filters:
      setParamsByID:
        model2:
          reasoning_effort: high
  model2:
    cmd: path/to/cmd --port ${PORT}
`
	_, err := LoadConfigFromReader(strings.NewReader(content))
	assert.ErrorContains(t, err, "conflicts with an existing model ID")
}

func TestConfig_SetParamsByIDAutoAliasConflictWithOtherModel(t *testing.T) {
	content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    filters:
      setParamsByID:
        "shared-alias":
          reasoning_effort: high
  model2:
    cmd: path/to/cmd --port ${PORT}
    filters:
      setParamsByID:
        "shared-alias":
          reasoning_effort: low
`
	_, err := LoadConfigFromReader(strings.NewReader(content))
	assert.ErrorContains(t, err, "duplicate alias")
}

func TestConfig_ModelFiltersWithSetParams(t *testing.T) {
	content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    filters:
      stripParams: "top_k"
      setParams:
        temperature: 0.7
        top_p: 0.9
        stop:
          - "<|end|>"
          - "<|stop|>"
 `
	config, err := LoadConfigFromReader(strings.NewReader(content))
	assert.NoError(t, err)

	modelConfig := config.Models["model1"]

	// Check stripParams
	stripParams, err := modelConfig.Filters.SanitizedStripParams()
	assert.NoError(t, err)
	assert.Equal(t, []string{"top_k"}, stripParams)

	// Check setParams
	setParams, keys := modelConfig.Filters.SanitizedSetParams()
	assert.NotNil(t, setParams)
	assert.Equal(t, []string{"stop", "temperature", "top_p"}, keys)
	assert.Equal(t, 0.7, setParams["temperature"])
	assert.Equal(t, 0.9, setParams["top_p"])
}

func TestConfig_ModelCapabilities(t *testing.T) {
	t.Run("all fields", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      in:
        - text
        - audio
        - image
      out:
        - text
        - audio
        - image
      tools: true
      context: 32000
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.False(t, mc.Capabilities.Empty())
		assert.Equal(t, []string{"text", "audio", "image"}, mc.Capabilities.In)
		assert.Equal(t, []string{"text", "audio", "image"}, mc.Capabilities.Out)
		assert.True(t, mc.Capabilities.Tools)
		assert.Equal(t, 32000, mc.Capabilities.Context)
	})

	t.Run("partial fields", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      tools: true
      context: 8192
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.False(t, mc.Capabilities.Empty())
		assert.Nil(t, mc.Capabilities.In)
		assert.Nil(t, mc.Capabilities.Out)
		assert.True(t, mc.Capabilities.Tools)
		assert.Equal(t, 8192, mc.Capabilities.Context)
	})

	t.Run("not set", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Empty())
	})

	t.Run("tools false is empty", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      tools: false
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Empty())
	})

	t.Run("reranker true is not empty", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      reranker: true
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.False(t, mc.Capabilities.Empty())
		assert.True(t, mc.Capabilities.Reranker)
	})

	t.Run("reranker false is empty", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      reranker: false
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Empty())
	})
}

func TestConfig_ModelCapabilities_Validate(t *testing.T) {
	t.Run("valid_modalities", func(t *testing.T) {
		caps := ModelCapConfig{
			In:      []string{"text", "image"},
			Out:     []string{"text", "audio"},
			Tools:   true,
			Context: 100000,
		}
		assert.NoError(t, caps.Validate())
	})

	t.Run("video_input_modality", func(t *testing.T) {
		caps := ModelCapConfig{
			In:      []string{"text", "image", "video"},
			Out:     []string{"text", "audio"},
			Tools:   true,
			Context: 100000,
		}
		assert.NoError(t, caps.Validate())
	})

	t.Run("video_output_modality", func(t *testing.T) {
		// `video` is added to the shared modality set, so it is valid on `out`
		// as well as `in`. Asserted explicitly so that stays a decision rather
		// than a side effect of the two lists sharing one map.
		caps := ModelCapConfig{Out: []string{"video"}}
		assert.NoError(t, caps.Validate())
	})

	t.Run("video_with_other_modalities", func(t *testing.T) {
		caps := ModelCapConfig{
			In:  []string{"text", "image", "video"},
			Out: []string{"text", "video"},
		}
		assert.NoError(t, caps.Validate())
	})

	t.Run("empty_is_valid", func(t *testing.T) {
		caps := ModelCapConfig{}
		assert.NoError(t, caps.Validate())
	})

	t.Run("invalid_in_modality", func(t *testing.T) {
		caps := ModelCapConfig{In: []string{"foo"}}
		err := caps.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "capabilities.in")
		assert.Contains(t, err.Error(), "foo")
	})

	t.Run("invalid_out_modality", func(t *testing.T) {
		caps := ModelCapConfig{Out: []string{"foo"}}
		err := caps.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "capabilities.out")
		assert.Contains(t, err.Error(), "foo")
	})

	t.Run("negative_context", func(t *testing.T) {
		caps := ModelCapConfig{Context: -1}
		err := caps.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "capabilities.context")
	})

	t.Run("rejects_invalid_at_load", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      in:
        - text
        - foo
`
		_, err := LoadConfigFromReader(strings.NewReader(content))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "foo")
	})

	t.Run("video_accepted_at_load", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      in:
        - text
        - image
        - video
      out:
        - text
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.Equal(t, []string{"text", "image", "video"}, mc.Capabilities.In)
		assert.Equal(t, []string{"text"}, mc.Capabilities.Out)
	})
}

func TestConfig_ModelCapabilities_MacroResolution(t *testing.T) {
	t.Run("context from global macro", func(t *testing.T) {
		content := `
macros:
  default_ctx: 98304
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      in:
        - text
        - image
      out:
        - text
      tools: true
      context: ${default_ctx}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.False(t, mc.Capabilities.Empty())
		assert.Equal(t, []string{"text", "image"}, mc.Capabilities.In)
		assert.Equal(t, []string{"text"}, mc.Capabilities.Out)
		assert.True(t, mc.Capabilities.Tools)
		assert.Equal(t, 98304, mc.Capabilities.Context)
	})

	t.Run("context from model macro overrides global", func(t *testing.T) {
		content := `
macros:
  default_ctx: 4096
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    macros:
      default_ctx: 65536
    capabilities:
      context: ${default_ctx}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.Equal(t, 65536, mc.Capabilities.Context)
	})

	t.Run("macro in modalities", func(t *testing.T) {
		content := `
macros:
  img_modality: image
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      in:
        - text
        - ${img_modality}
      out:
        - text
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.Equal(t, []string{"text", "image"}, mc.Capabilities.In)
	})

	t.Run("macro in tools", func(t *testing.T) {
		content := `
macros:
  has_tools: true
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      tools: ${has_tools}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Tools)
	})

	t.Run("macro in reranker", func(t *testing.T) {
		content := `
macros:
  is_reranker: true
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      reranker: ${is_reranker}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Reranker)
	})

	t.Run("quoted direct macros preserve scalar types", func(t *testing.T) {
		content := `
macros:
  default_ctx: 32768
  has_tools: true
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      context: "${default_ctx}"
      tools: "${has_tools}"
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.Equal(t, 32768, mc.Capabilities.Context)
		assert.True(t, mc.Capabilities.Tools)
	})

	t.Run("no capabilities block is unchanged", func(t *testing.T) {
		content := `
macros:
  default_ctx: 98304
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
`
		config, err := LoadConfigFromReader(strings.NewReader(content))
		assert.NoError(t, err)

		mc := config.Models["model1"]
		assert.True(t, mc.Capabilities.Empty())
	})

	t.Run("unknown macro in capabilities errors", func(t *testing.T) {
		content := `
models:
  model1:
    cmd: path/to/cmd --port ${PORT}
    capabilities:
      context: ${undefined_macro}
`
		_, err := LoadConfigFromReader(strings.NewReader(content))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "capabilities: unknown macro '${undefined_macro}'")
	})
}
