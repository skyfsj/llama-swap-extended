package config

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"

	"github.com/billziss-gh/golib/shlex"
	"github.com/mostlygeek/llama-swap/internal/launchspec"
)

// ManagedLaunchEntrypoint is the canonical executable name the launch binding
// lookups use for a managed runtime kind. The operator-written argv[0] is
// ignored at launch time: the bound process always executes the
// runtime-owned entrypoint, never an ambient host executable.
func ManagedLaunchEntrypoint(kind string) string {
	if kind == "vllm" {
		return "vllm"
	}
	return "llama-server"
}

// SplitManagedLaunchTokens flattens operator-written backend args into argv
// tokens. A list item that holds a whole "flag value" line, for example
// "--gpu-memory-utilization 0.90", is shell-split so hand-edited configs
// written one flag per line behave like the one token per item form. Items
// without whitespace, including model paths that contain spaces, stay single
// argv elements; a fully quoted item still has its wrapping quotes stripped
// so a stored '"0.90"' or '{"json":true}' reaches the engine unquoted.
func SplitManagedLaunchTokens(args []string) []string {
	tokens := make([]string, 0, len(args))
	for _, argument := range args {
		trimmed := strings.TrimSpace(argument)
		if trimmed == "" {
			continue
		}
		if !strings.ContainsAny(trimmed, " \t") || !strings.HasPrefix(trimmed, "-") {
			if trimmed[0] == '"' || trimmed[0] == '\'' {
				if split := shlexSplit(trimmed); len(split) == 1 {
					tokens = append(tokens, split[0])
					continue
				}
			}
			tokens = append(tokens, trimmed)
			continue
		}
		tokens = append(tokens, shlexSplit(trimmed)...)
	}
	return tokens
}

func shlexSplit(line string) []string {
	if runtime.GOOS == "windows" {
		return shlex.Windows.Split(line)
	}
	return shlex.Posix.Split(line)
}

var managedModelPairFlags = map[string]struct{}{
	"--model":      {},
	"--model-path": {},
}

var managedHostPortPairFlags = map[string]struct{}{
	"--host": {},
	"-hp":    {},
	"--port": {},
}

// EnsureVLLMSleepModeArgument adds the vLLM flag required by the sleep API
// when a model has an automatic TTL. It returns a copy and preserves an
// explicitly configured flag, including an explicit --enable-sleep-mode=false.
// A zero TTL deliberately does not opt the backend into sleep mode.
func EnsureVLLMSleepModeArgument(args []string, backendType string, ttl int) []string {
	result := append([]string(nil), args...)
	if !strings.EqualFold(strings.TrimSpace(backendType), "vllm") || ttl <= 0 {
		return result
	}
	for _, token := range SplitManagedLaunchTokens(args) {
		name := token
		if equals := strings.IndexByte(token, '='); equals > 0 {
			name = token[:equals]
		}
		if name == "--enable-sleep-mode" {
			return result
		}
	}
	return append(result, "--enable-sleep-mode")
}

// RebuildManagedLaunchArguments normalizes operator-written backend args into
// the canonical managed launch form. The entrypoint, subcommand, model target,
// bind host and port are system-owned: operator duplicates are dropped and
// re-injected, with host and port taken from the model's resolved proxy. The
// remaining engine arguments keep their relative order.
func RebuildManagedLaunchArguments(args []string, kind, proxy string) ([]string, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	tokens := SplitManagedLaunchTokens(args)
	rest := launchspec.StripInvocation(kind, tokens)

	var model string
	extras := make([]string, 0, len(rest))
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if _, ok := managedModelPairFlags[token]; ok {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("backend.args %s is missing its value", token)
			}
			if model == "" {
				model = rest[i+1]
			}
			i++
			continue
		}
		if _, ok := managedHostPortPairFlags[token]; ok {
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("backend.args %s is missing its value", token)
			}
			i++
			continue
		}
		switch {
		case strings.HasPrefix(token, "--model="):
			if model == "" {
				model = strings.TrimPrefix(token, "--model=")
			}
		case strings.HasPrefix(token, "--model-path="):
			if model == "" {
				model = strings.TrimPrefix(token, "--model-path=")
			}
		case strings.HasPrefix(token, "--host="), strings.HasPrefix(token, "-hp="), strings.HasPrefix(token, "--port="):
		default:
			extras = append(extras, token)
		}
	}
	// The leading non-flag token is the positional model target.
	if model == "" && len(extras) > 0 && extras[0] != "" && !strings.HasPrefix(extras[0], "-") {
		model = extras[0]
		extras = extras[1:]
	}
	if model == "" {
		return nil, errors.New("backend.args must include a model target (--model) for the managed runtime")
	}

	host, port := "", ""
	if trimmed := strings.TrimSpace(proxy); trimmed != "" {
		parsed := trimmed
		if !strings.Contains(parsed, "://") {
			parsed = "http://" + parsed
		}
		if target, err := url.Parse(parsed); err == nil {
			host = target.Hostname()
			port = target.Port()
		}
		// A previewed draft may still carry the unresolved ${PORT} macro
		// (allocation happens at config load). Keep it verbatim so the echoed
		// command shows where the port will be injected.
		if port == "" && strings.Contains(trimmed, "${PORT}") {
			port = "${PORT}"
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}

	var rebuilt []string
	if kind == "vllm" {
		rebuilt = []string{"vllm", "serve", "--model", model, "--host", host}
	} else {
		rebuilt = []string{"llama-server", "--model", model, "--host", host}
	}
	if port != "" {
		rebuilt = append(rebuilt, "--port", port)
	}
	return append(rebuilt, extras...), nil
}

// BuildManagedLaunchArguments renders the final managed argv for a model,
// merging the structured launch block with the operator's extra arguments.
// The first return value is the argv; the second carries the environment the
// structured block owns (CUDA_VISIBLE_DEVICES from the GPU selection). A nil
// launch block falls back to the legacy args-only rebuild.
func BuildManagedLaunchArguments(args []string, launch *ModelLaunchConfig, kind, proxy string) ([]string, []string, error) {
	if launch == nil || (launch.Model == "" && launch.ServedModelName == "" &&
		launch.ContextPerRequest == 0 && launch.MaxConcurrency == 0 &&
		launch.GPUMemoryUtilization == nil && launch.TensorParallelSize == 0 &&
		len(launch.GPUs) == 0) {
		rebuilt, err := RebuildManagedLaunchArguments(args, kind, proxy)
		return rebuilt, nil, err
	}

	tokens := SplitManagedLaunchTokens(args)
	parsed, err := launchspec.ParseTokens(kind, tokens)
	if err != nil {
		return nil, nil, fmt.Errorf("parse backend.args: %w", err)
	}

	spec := launch.Spec()
	// Existing configurations can contain both backend.launch and the old
	// whole-line backend.args representation. Fill only missing structured
	// fields from the parsed args so that those values are not left behind as
	// positional tokens (the exact failure seen with vLLM's TP value).
	if strings.TrimSpace(spec.Model) == "" {
		spec.Model = parsed.Model
	}
	if spec.ServedModelName == "" {
		spec.ServedModelName = parsed.ServedModelName
	}
	if spec.ContextPerRequest == 0 {
		spec.ContextPerRequest = parsed.ContextPerRequest
	}
	if spec.MaxConcurrency == 0 {
		spec.MaxConcurrency = parsed.MaxConcurrency
	}
	if spec.GPUMemoryUtilization == nil {
		spec.GPUMemoryUtilization = parsed.GPUMemoryUtilization
	}
	if spec.TensorParallelSize == 0 {
		spec.TensorParallelSize = parsed.TensorParallelSize
	}
	if len(spec.CUDAVisibleDevices) == 0 {
		spec.CUDAVisibleDevices = parsed.CUDAVisibleDevices
	}

	model := strings.TrimSpace(spec.Model)
	if model == "" {
		model = strings.TrimSpace(parsed.Model)
	}
	if model == "" {
		return nil, nil, errors.New("backend.launch must set a model target")
	}

	host, port := "", ""
	if trimmed := strings.TrimSpace(proxy); trimmed != "" {
		parsed := trimmed
		if !strings.Contains(parsed, "://") {
			parsed = "http://" + parsed
		}
		if target, err := url.Parse(parsed); err == nil {
			host = target.Hostname()
			port = target.Port()
		}
		// A previewed draft may still carry the unresolved ${PORT} macro
		// (allocation happens at config load). Keep it verbatim so the echoed
		// command shows where the port will be injected.
		if port == "" && strings.Contains(trimmed, "${PORT}") {
			port = "${PORT}"
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}

	// The canonical prefix already carries --model; the structured build
	// only adds the remaining managed flags.
	spec.Model = ""
	if kind == "vllm" && spec.TensorParallelSize == 0 && len(spec.CUDAVisibleDevices) > 1 {
		// Auto parallel strategy: one shard per selected GPU unless the
		// operator pinned an explicit tensor-parallel-size.
		spec.TensorParallelSize = len(spec.CUDAVisibleDevices)
	}

	var rebuilt []string
	if kind == "vllm" {
		rebuilt = []string{"vllm", "serve", "--model", model, "--host", host}
	} else {
		rebuilt = []string{"llama-server", "--model", model, "--host", host}
	}
	if port != "" {
		rebuilt = append(rebuilt, "--port", port)
	}
	rebuilt = append(rebuilt, spec.BuildArgs(kind, false)...)
	// ParseTokens has already consumed every managed pair as a unit. This is
	// important for list entries such as "--tensor-parallel-size 4": the
	// value must never be emitted as a bare positional argument after the
	// canonical launch prefix.
	rebuilt = append(rebuilt, parsed.ExtraArgs...)
	return rebuilt, spec.BuildEnv(), nil
}
