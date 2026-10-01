package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

const (
	lmcacheKVTransferFlag  = "--kv-transfer-config"
	lmcacheConnectorMP     = "LMCacheMPConnector"
	lmcacheConnectorInProc = "LMCacheConnectorV1"
	lmcacheMPModulePathKey = "kv_connector_module_path"
	lmcacheMPModulePath    = "lmcache.integration.vllm.lmcache_mp_connector"
	lmcacheEnvChunkSize    = "LMCACHE_CHUNK_SIZE"
)

// vllmSupportsShippedMPConnector reports whether the vLLM version accepts the
// kv_connector_module_path key. The key was introduced in vLLM 0.20.0, where
// LMCache's shipped MP connector can be selected; older vLLM builds ignore
// the LMCache-bundled implementation. Unknown or unparseable versions are
// assumed current and therefore supported.
func vllmSupportsShippedMPConnector(version string) bool {
	parts := strings.SplitN(strings.TrimLeft(strings.TrimSpace(version), "v"), ".", 3)
	if len(parts) < 2 {
		return true
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return true
	}
	minor, err := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	if err != nil {
		return true
	}
	return major > 0 || minor >= 20
}

// lmcacheKVTransferConfig renders the compact --kv-transfer-config JSON for a
// model LMCache attachment. Map keys marshal in stable sorted order, so the
// generated argument is deterministic for a given configuration.
func lmcacheKVTransferConfig(lm config.LMCacheModelConfig, vllmVersion string) (string, error) {
	role := lm.EffectiveRole()
	connector := lmcacheConnectorMP
	if lm.EffectiveMode() == config.LMCacheModeInProcess {
		connector = lmcacheConnectorInProc
	}
	payload := map[string]any{
		"kv_connector": connector,
		"kv_role":      role,
	}
	if connector == lmcacheConnectorMP {
		extra := map[string]any{
			"lmcache.mp.host": lm.EffectiveMPHost(),
			"lmcache.mp.port": lm.EffectiveMPPort(),
		}
		if vllmSupportsShippedMPConnector(vllmVersion) {
			extra[lmcacheMPModulePathKey] = lmcacheMPModulePath
		}
		payload["kv_connector_extra_config"] = extra
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode lmcache kv-transfer config: %w", err)
	}
	return string(encoded), nil
}

// applyLMCacheLaunchConfig appends the LMCache kv-transfer arguments and
// environment to a bound native vLLM model. The public configuration is
// untouched; only the private launch snapshot picks up the generated flags.
// The vLLM version, when known from the runtime manifest, decides whether the
// LMCache-shipped MP connector module path is requested.
func applyLMCacheLaunchConfig(cfg config.Config, modelID, runtimeName, version string, model *config.ModelConfig) error {
	lm := model.Backend.LMCache
	if lm == nil || !lm.Enabled {
		return nil
	}
	root := managedRuntimeRoot(cfg)
	vllmVersion := ""
	if version != "" {
		if manifest, err := runtimeManager.ManifestForVersion(root, runtimeName, "vllm", version); err == nil {
			vllmVersion = manifest.VLLM
		}
	} else if manifest, found, err := runtimeManager.CurrentManifest(root, runtimeName, "vllm"); err == nil && found {
		vllmVersion = manifest.VLLM
	}
	kvConfig, err := lmcacheKVTransferConfig(*lm, vllmVersion)
	if err != nil {
		return err
	}
	model.Backend.Arguments = append(append([]string(nil), model.Backend.Arguments...), lmcacheKVTransferFlag, kvConfig)
	if lm.EffectiveMode() == config.LMCacheModeInProcess && lm.ChunkSize > 0 {
		model.Env = append(append([]string(nil), model.Env...), fmt.Sprintf("%s=%d", lmcacheEnvChunkSize, lm.ChunkSize))
	}
	return nil
}

// lmcacheModelVenvBinDir resolves the vLLM venv bin dir (holding python) that
// a LMCache-enabled model launches from: the pinned backend.runtimeVersion
// when set, otherwise the runtime's current pointer. It must match the launch
// binding exactly, because the model gate probes and installs the connector
// in the very venv the model will start from. Fail-closed: a model whose
// venv cannot be resolved cannot be guaranteed, so it cannot start.
func lmcacheModelVenvBinDir(root string, model *config.ModelConfig) (string, error) {
	runtimeName := strings.TrimSpace(model.Backend.Runtime)
	if runtimeName == "" {
		return "", errors.New("the model has no backend.runtime; LMCache requires a managed vLLM runtime")
	}
	if version := strings.TrimSpace(model.Backend.RuntimeVersion); version != "" {
		binding, err := runtimeManager.LaunchBindingForVersion(root, runtimeName, "vllm", "python", version)
		if err != nil {
			return "", fmt.Errorf("resolve the pinned vLLM version %s of runtime %s: %w", version, runtimeName, err)
		}
		return binding.BinDir, nil
	}
	binding, found, err := runtimeManager.CurrentVersionLaunchBinding(root, runtimeName, "vllm", "python")
	if err != nil {
		return "", fmt.Errorf("resolve the current vLLM version of runtime %s: %w", runtimeName, err)
	}
	if !found {
		return "", fmt.Errorf("vLLM runtime %s has no activated version; stage and activate it before starting a LMCache model", runtimeName)
	}
	return binding.BinDir, nil
}
