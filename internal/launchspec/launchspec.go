// Package launchspec parses full engine launch commands into a structured
// configuration and builds launch argv back from it. Parsing is purely
// lexical: pasted text is never executed, and unknown arguments are always
// preserved so future runtime flags survive a round-trip.
package launchspec

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/billziss-gh/golib/shlex"
)

// Spec is the structured subset of an engine launch command that the model
// configuration owns. Everything the basic UI does not manage stays in
// ExtraArgs verbatim.
type Spec struct {
	Model                string
	ServedModelName      string
	ContextPerRequest    int
	MaxConcurrency       int
	GPUMemoryUtilization *float64
	TensorParallelSize   int
	KVUnifiedPerSlot     int
	CUDAVisibleDevices   []string
	ExtraArgs            []string
	Environment          map[string]string
	Warnings             []string
}

// reservedArgOwners reports which structured field owns a canonical flag so
// the config validation can reject duplicates between launch and extra args.
func reservedArgs(kind string) map[string]string {
	vllm := map[string]string{
		"model": "--model", "model-path": "--model",
		"served-model-name": "--served-model-name",
		"max-model-len":     "--max-model-len", "max-num-seqs": "--max-num-seqs",
		"gpu-memory-utilization": "--gpu-memory-utilization", "tensor-parallel-size": "--tensor-parallel-size",
	}
	llamacpp := map[string]string{
		"model": "--model", "alias": "--alias", "ctx-size": "--ctx-size",
		"parallel": "--parallel", "kv-unified-per-slot": "--kv-unified-per-slot", "device": "--device",
	}
	if kind == "vllm" {
		return vllm
	}
	return llamacpp
}

// ReservedFlags lists the raw flag names (aliases included) that the
// structured launch block owns for a runtime kind.
func ReservedFlags(kind string) map[string]bool {
	reserved := make(map[string]bool, 8)
	for _, flag := range reservedArgs(kind) {
		reserved[flag] = true
	}
	if kind == "llamacpp" {
		reserved["-m"] = true
		reserved["-a"] = true
		reserved["-c"] = true
		reserved["-np"] = true
		return reserved
	}
	reserved["-tp"] = true
	return reserved
}

// Tokenize splits a pasted command into argv tokens. Line continuations are
// joined first so a multi-line shell command tokenizes as one line; quoting
// is handled by shlex, so spaced paths and quoted JSON stay single tokens.
// Tokenization is line-aware: a stray unbalanced quote (the doubled two
// single quotes people copy out of terminals) can only absorb the rest of
// its own line,
// never swallow the following lines the way a whole-input split would.
func Tokenize(command string) []string {
	joined := strings.ReplaceAll(command, "\\\n", " ")
	tokens := make([]string, 0, 8)
	for _, line := range strings.Split(joined, "\n") {
		tokens = append(tokens, shlexSplit(line)...)
	}
	return tokens
}

func shlexSplit(line string) []string {
	if runtime.GOOS == "windows" {
		return shlex.Windows.Split(line)
	}
	return shlex.Posix.Split(line)
}

// environmentAssignments consumes leading NAME=VALUE tokens the way a shell
// would before the executable, recording them in the spec environment.
func environmentAssignments(spec *Spec, tokens []string) []string {
	index := 0
	for ; index < len(tokens); index++ {
		token := tokens[index]
		equals := strings.Index(token, "=")
		if equals <= 0 {
			break
		}
		name := token[:equals]
		if !validEnvName(name) {
			break
		}
		if spec.Environment == nil {
			spec.Environment = make(map[string]string)
		}
		spec.Environment[name] = token[equals+1:]
	}
	return tokens[index:]
}

func validEnvName(name string) bool {
	for _, char := range name {
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char == '_':
		case char >= '0' && char <= '9':
		default:
			return false
		}
	}
	return name != ""
}

// stripInvocation removes the launcher prefix: environment is already split
// off, so what remains may start with vllm/serve, a python -m module
// invocation, or a llama-server path. The executable only decides the backend
// mapping; the managed runtime still owns the real path at start.
func stripInvocation(kind string, tokens []string) []string {
	rest := tokens
	for len(rest) > 0 {
		bare := invocationBase(rest[0])
		switch {
		case bare == "python" || bare == "python3" || strings.HasSuffix(bare, "python"):
			rest = rest[1:]
			if len(rest) > 0 && (rest[0] == "-m" || rest[0] == "--module") {
				rest = rest[1:]
				if len(rest) > 0 {
					rest = rest[1:]
				}
			}
			continue
		case bare == "vllm" || bare == "llama-server" || bare == "llama.cpp/llama-server":
			rest = rest[1:]
			continue
		case bare == "serve" && kind == "vllm":
			rest = rest[1:]
			continue
		}
		break
	}
	return rest
}

// StripInvocation removes an operator-written launcher prefix from an argv
// slice. It is exported for the legacy args-only normalizer, which must use
// the same path-aware rules as the structured parser.
func StripInvocation(kind string, tokens []string) []string {
	return stripInvocation(strings.ToLower(strings.TrimSpace(kind)), tokens)
}

func invocationBase(token string) string {
	normalized := strings.TrimRight(strings.ReplaceAll(token, "\\", "/"), "/")
	if slash := strings.LastIndex(normalized, "/"); slash >= 0 {
		normalized = normalized[slash+1:]
	}
	return strings.ToLower(normalized)
}

// Parse turns a pasted full command (or a bare argument list) into the
// structured spec plus the remaining unmanaged arguments. Unknown flags are
// never dropped; flags the basic UI owns are moved into their fields.
func Parse(kind string, command string) (Spec, error) {
	return ParseTokens(kind, Tokenize(command))
}

// ParseTokens parses an already tokenized argv. It is used by the managed
// runtime binder after flattening YAML list items, so a value containing
// spaces is not split a second time while the command is normalized.
func ParseTokens(kind string, tokens []string) (Spec, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	spec := Spec{}
	tokens = environmentAssignments(&spec, tokens)
	tokens = stripInvocation(kind, tokens)

	if devices, ok := spec.Environment["CUDA_VISIBLE_DEVICES"]; ok {
		spec.CUDAVisibleDevices = splitDeviceList(devices)
		delete(spec.Environment, "CUDA_VISIBLE_DEVICES")
	}

	extras := make([]string, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		name, value, inline := token, "", false
		if equals := strings.Index(token, "="); equals > 0 {
			name, value, inline = token[:equals], token[equals+1:], true
		}
		canonical := canonicalFlag(kind, name)
		owner := reservedArgs(kind)[canonical]
		if owner == "" {
			if canonical == "host" || canonical == "port" {
				if !inline {
					index++
				}
				continue
			}
			extras = append(extras, token)
			continue
		}
		if !inline {
			if index+1 >= len(tokens) {
				return spec, fmt.Errorf("parse command: %s is missing its value", name)
			}
			value = tokens[index+1]
			index++
		}
		applyFlag(&spec, kind, canonical, owner, value)
	}

	if kind == "vllm" && spec.Model == "" && len(extras) > 0 && !strings.HasPrefix(extras[0], "-") {
		spec.Model = extras[0]
		extras = extras[1:]
	}
	spec.ExtraArgs = extras
	if kind == "llamacpp" && spec.ContextPerRequest > 0 && spec.MaxConcurrency > 1 && spec.KVUnifiedPerSlot == 0 {
		if spec.ContextPerRequest%spec.MaxConcurrency != 0 {
			spec.Warnings = append(spec.Warnings, fmt.Sprintf(
				"ctx-size %d 无法平均分配给 %d 个并发槽位，已按原值保留，请确认",
				spec.ContextPerRequest, spec.MaxConcurrency))
		} else {
			spec.ContextPerRequest /= spec.MaxConcurrency
		}
	}
	return spec, nil
}

func applyFlag(spec *Spec, kind, canonical, owner, value string) {
	if owner == "" {
		return
	}
	switch owner {
	case "--model":
		if spec.Model == "" {
			spec.Model = value
		}
	case "--served-model-name", "--alias":
		if spec.ServedModelName == "" {
			spec.ServedModelName = value
		}
	case "--max-model-len", "--kv-unified-per-slot", "--ctx-size":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			spec.Warnings = append(spec.Warnings, fmt.Sprintf("%s 的值 %q 不是有效的 token 数", owner, value))
			return
		}
		if owner == "--kv-unified-per-slot" {
			spec.KVUnifiedPerSlot = parsed
			spec.ContextPerRequest = parsed
			return
		}
		if spec.ContextPerRequest == 0 {
			spec.ContextPerRequest = parsed
		}
	case "--max-num-seqs", "--parallel":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			spec.Warnings = append(spec.Warnings, fmt.Sprintf("%s 的值 %q 不是有效的并发数", owner, value))
			return
		}
		if spec.MaxConcurrency == 0 {
			spec.MaxConcurrency = parsed
		}
	case "--gpu-memory-utilization":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed <= 0 || parsed > 1 {
			spec.Warnings = append(spec.Warnings, fmt.Sprintf("--gpu-memory-utilization 的值 %q 不在 (0,1] 内", value))
			return
		}
		spec.GPUMemoryUtilization = &parsed
	case "--tensor-parallel-size":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			spec.Warnings = append(spec.Warnings, fmt.Sprintf("--tensor-parallel-size 的值 %q 无效", value))
			return
		}
		spec.TensorParallelSize = parsed
	case "--device":
		if spec.CUDAVisibleDevices == nil {
			spec.CUDAVisibleDevices = splitDeviceList(value)
		}
	}
}

// canonicalFlag maps aliases and long forms to one canonical name, so the
// rest of the code never repeats alias checks.
func canonicalFlag(kind, name string) string {
	if strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") {
		switch name {
		case "-m":
			return "model"
		case "-a":
			return "alias"
		case "-c":
			return "ctx-size"
		case "-np":
			return "parallel"
		case "-tp":
			return "tensor-parallel-size"
		case "-hp":
			return "host"
		}
		return strings.TrimPrefix(name, "-")
	}
	long := strings.TrimPrefix(name, "--")
	if kind == "vllm" {
		switch long {
		case "model", "served-model-name", "max-model-len", "max-num-seqs",
			"model-path", "gpu-memory-utilization", "tensor-parallel-size", "host", "port":
			return long
		}
		return long
	}
	switch long {
	case "model", "alias", "ctx-size", "parallel", "kv-unified-per-slot", "device", "host", "port":
		return long
	}
	return long
}

func splitDeviceList(value string) []string {
	parts := strings.Split(value, ",")
	devices := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			devices = append(devices, trimmed)
		}
	}
	return devices
}

// BuildArgs renders the structured spec into engine argv flags. When
// kvUnified is false (legacy llama.cpp), the per-request context is expanded
// to the total context the engine expects: context * parallel slots.
func (s Spec) BuildArgs(kind string, kvUnified bool) []string {
	args := make([]string, 0, 16)
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.ServedModelName != "" {
		if kind == "vllm" {
			args = append(args, "--served-model-name", s.ServedModelName)
		} else {
			args = append(args, "--alias", s.ServedModelName)
		}
	}
	if kind == "llamacpp" {
		if s.MaxConcurrency > 1 {
			args = append(args, "--parallel", strconv.Itoa(s.MaxConcurrency))
		}
		if s.ContextPerRequest > 0 {
			if kvUnified {
				args = append(args, "--kv-unified-per-slot", strconv.Itoa(s.ContextPerRequest))
			} else {
				total := s.ContextPerRequest * maxInt(s.MaxConcurrency, 1)
				args = append(args, "--ctx-size", strconv.Itoa(total))
			}
		}
	} else {
		if s.ContextPerRequest > 0 {
			args = append(args, "--max-model-len", strconv.Itoa(s.ContextPerRequest))
		}
		if s.MaxConcurrency > 0 {
			args = append(args, "--max-num-seqs", strconv.Itoa(s.MaxConcurrency))
		}
		if s.TensorParallelSize > 1 {
			args = append(args, "--tensor-parallel-size", strconv.Itoa(s.TensorParallelSize))
		}
		if s.GPUMemoryUtilization != nil {
			args = append(args, "--gpu-memory-utilization", strconv.FormatFloat(*s.GPUMemoryUtilization, 'f', -1, 64))
		}
	}
	return args
}

// BuildEnv renders the structured environment. GPU selections are persisted
// as UUIDs when available; the caller resolves them to current indexes
// before start and passes the physical list here. Only NVIDIA cards reach
// this list — the launch binding filters the selections against the
// machine's accelerator inventory, so on Apple or AMD hosts the CUDA
// variables stay absent.
func (s Spec) BuildEnv() []string {
	env := make([]string, 0, len(s.Environment)+2)
	if len(s.CUDAVisibleDevices) > 0 {
		// Pin the device order to the PCI bus so index-based
		// CUDA_VISIBLE_DEVICES selections stay stable across restarts and
		// match the card order nvidia-smi reports.
		env = append(env, "CUDA_DEVICE_ORDER=PCI_BUS_ID")
		env = append(env, "CUDA_VISIBLE_DEVICES="+strings.Join(s.CUDAVisibleDevices, ","))
	}
	for _, assignment := range s.Environment {
		env = append(env, assignment)
	}
	return env
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
