package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Gpus_LoadValid(t *testing.T) {
	yaml := twoModels + `
routing:
  router:
    use: gpus
    settings:
      gpus:
        "0": [gemma]
        1: [qwen]
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.Equal(t, "gpus", cfg.Routing.Router.Use)
	gpus := cfg.Routing.Router.Settings.Gpus
	require.NotNil(t, gpus)
	assert.Equal(t, []string{"gemma"}, gpus.Cards["0"])
	assert.Equal(t, []string{"qwen"}, gpus.Cards["1"])
}

func TestConfig_Gpus_MissingSettings(t *testing.T) {
	yaml := twoModels + `
routing:
  router:
    use: gpus
`
	_, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routing.router.settings.gpus is not set")
}

func TestConfig_Gpus_IgnoredWhenInactive(t *testing.T) {
	yaml := twoModels + `
routing:
  router:
    use: group
    settings:
      groups:
        g1:
          members: [gemma]
      gpus:
        "0": [qwen]
`
	cfg, err := LoadConfigFromReader(strings.NewReader(yaml))
	require.NoError(t, err)
	assert.Equal(t, "group", cfg.Routing.Router.Use)
	assert.Nil(t, cfg.Routing.Router.Settings.Gpus)
}

func TestValidateGpus_NoCards(t *testing.T) {
	err := ValidateGpus(&GpusConfig{Cards: map[string][]string{}}, map[string]ModelConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one card")
}

func TestValidateGpus_UnknownModel(t *testing.T) {
	gpus := &GpusConfig{Cards: map[string][]string{"0": {"gemma", "nope"}}}
	models := map[string]ModelConfig{"gemma": {}}
	err := ValidateGpus(gpus, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
}

func TestValidateGpus_DuplicateModelOnCard(t *testing.T) {
	gpus := &GpusConfig{Cards: map[string][]string{"0": {"gemma", "gemma"}}}
	models := map[string]ModelConfig{"gemma": {}}
	err := ValidateGpus(gpus, models)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listed more than once")
}

func TestValidateGpus_ModelOnMultipleCards(t *testing.T) {
	gpus := &GpusConfig{Cards: map[string][]string{"0": {"wide"}, "1": {"wide"}}}
	models := map[string]ModelConfig{"wide": {}}
	err := ValidateGpus(gpus, models)
	require.NoError(t, err)
	assert.Equal(t, []string{"0", "1"}, gpus.CardsOf("wide"))
}

func TestGpus_CardsOfUnlistedModel(t *testing.T) {
	gpus := &GpusConfig{Cards: map[string][]string{"0": {"gemma"}}}
	assert.Empty(t, gpus.CardsOf("cpu-only"))
	assert.False(t, gpus.SharesCard("cpu-only", "gemma"))
	assert.True(t, gpus.SharesCard("gemma", "gemma"))
}
