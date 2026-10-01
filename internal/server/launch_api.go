package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/launchspec"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleAPILaunchParse parses a pasted full launch command (or a bare
// argument list) into the structured fields plus the remaining extra
// arguments. Parsing is purely lexical — the text is never executed.
func (s *Server) handleAPILaunchParse(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if err := decodeJSONBody(w, r, &request, 64<<10); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	kind := strings.ToLower(strings.TrimSpace(request.Kind))
	if kind != "vllm" && kind != "llamacpp" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "kind must be vllm or llamacpp")
		return
	}
	parsed, err := launchspec.Parse(kind, request.Text)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":                parsed.Model,
		"servedModelName":      parsed.ServedModelName,
		"contextPerRequest":    parsed.ContextPerRequest,
		"maxConcurrency":       parsed.MaxConcurrency,
		"gpuMemoryUtilization": parsed.GPUMemoryUtilization,
		"tensorParallelSize":   parsed.TensorParallelSize,
		"cudaVisibleDevices":   parsed.CUDAVisibleDevices,
		"extraArgs":            parsed.ExtraArgs,
		"environment":          parsed.Environment,
		"warnings":             parsed.Warnings,
	})
}

// handleAPILaunchPreview renders the final managed launch for a draft model
// configuration: the environment assignments, the argv and a copy-ready
// command line. The draft uses the same shape as the public model config.
func (s *Server) handleAPILaunchPreview(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind   string                    `json:"kind"`
		Proxy  string                    `json:"proxy"`
		Args   []string                  `json:"args"`
		Launch *config.ModelLaunchConfig `json:"launch"`
	}
	if err := decodeJSONBody(w, r, &request, 256<<10); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	kind := strings.ToLower(strings.TrimSpace(request.Kind))
	if kind != "vllm" && kind != "llamacpp" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "kind must be vllm or llamacpp")
		return
	}
	launch := request.Launch
	if launch != nil && len(launch.GPUs) > 0 {
		// Same vendor rule as the real launch: only NVIDIA selections reach
		// CUDA_VISIBLE_DEVICES, so the preview mirrors the bound argv/env.
		boundLaunch := *launch
		boundLaunch.GPUs = nvidiaLaunchDevices(s.hardware, launch.GPUs)
		launch = &boundLaunch
	}
	argv, env, err := config.BuildManagedLaunchArguments(request.Args, launch, kind, request.Proxy)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	quoted := make([]string, 0, len(argv))
	for _, token := range argv {
		if strings.ContainsAny(token, " \t'\"") {
			quoted = append(quoted, "'"+strings.ReplaceAll(token, "'", `'\''`)+"'")
			continue
		}
		quoted = append(quoted, token)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"env":     env,
		"args":    argv,
		"command": strings.Join(quoted, " "),
	})
}

// handleAPIGPUs serves the accelerator inventory for the GPU card selector.
// The snapshot is captured at process start; memory.used reflects the same
// boot-time sample.
func (s *Server) handleAPIGPUs(w http.ResponseWriter, r *http.Request) {
	type GPUInfo struct {
		Index            int    `json:"index"`
		UUID             string `json:"uuid"`
		Name             string `json:"name"`
		Vendor           string `json:"vendor"`
		MemoryTotalBytes uint64 `json:"memoryTotalBytes"`
	}
	gpus := []GPUInfo{}
	if s.hardware != nil {
		for _, accelerator := range s.hardware.Accelerators {
			if !strings.EqualFold(accelerator.Kind, "gpu") {
				continue
			}
			info := GPUInfo{Index: accelerator.Index}
			if accelerator.UUID != nil {
				info.UUID = *accelerator.UUID
			}
			if accelerator.Model != nil {
				info.Name = *accelerator.Model
			}
			if accelerator.Vendor != nil {
				info.Vendor = *accelerator.Vendor
			}
			if accelerator.Memory.CapacityBytes != nil {
				info.MemoryTotalBytes = *accelerator.Memory.CapacityBytes
			}
			gpus = append(gpus, info)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"gpus": gpus})
}
