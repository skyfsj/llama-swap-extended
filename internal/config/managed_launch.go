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

// BuildManagedLaunchArguments renders the final managed argv for a model.
// backend.args is the single source of truth for engine flags: the structured
// launch block is folded into it at config load (see MigrateLaunchBlocks), so
// this builder only owns the launch head and the system-injected bind address.
//
// Two shapes are supported:
//
//   - Default head (backend.entrypoint empty): the runtime kind owns the
//     entrypoint. The model target comes from args, and the bind host/port are
//     injected from the model's resolved proxy.
//   - Custom head (backend.entrypoint set): the operator's head runs verbatim,
//     followed by args. No engine-specific flags are injected — ports are
//     written explicitly as ${PORT} — so a custom server can run under the
//     managed runtime's environment without pretending to be vLLM or llama.cpp.
func BuildManagedLaunchArguments(args []string, entrypoint, kind, proxy string) ([]string, []string, error) {
	if trimmed := strings.TrimSpace(entrypoint); trimmed != "" {
		// The head is a shell-like command line, so it is tokenized with the
		// same lexer the operator's argument text uses — never executed.
		head := launchspec.Tokenize(trimmed)
		if len(head) == 0 {
			return nil, nil, errors.New("backend.entrypoint is empty")
		}
		// The args are appended verbatim: a custom head has no engine grammar to
		// inject into, so the operator owns the whole line — including the
		// --port that carries the allocated ${PORT}. Only the token split is
		// normalized (whole-line entries become pairs).
		rest := SplitManagedLaunchTokens(args)
		rebuilt := make([]string, 0, len(head)+len(rest))
		rebuilt = append(rebuilt, head...)
		rebuilt = append(rebuilt, rest...)
		return rebuilt, nil, nil
	}
	rebuilt, err := RebuildManagedLaunchArguments(args, kind, proxy)
	return rebuilt, nil, err
}
