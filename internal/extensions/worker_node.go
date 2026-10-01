package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Node-based executor used ONLY for debug sessions: production hook execution
// stays on the goja worker. The Node runtime talks the same line protocol, so
// the parity harness can compare the two runtimes record by record.

// NodeExecutorEnabled reports whether debug sessions should use the Node
// executor instead of goja. Opt-in via LLAMA_SWAP_NODE_DEBUG=1: the Node
// runtime requires the system node and the committed executor script.
func NodeExecutorEnabled() bool {
	return os.Getenv("LLAMA_SWAP_NODE_DEBUG") == "1"
}

// DebugNodePath returns the node executable for debug sessions.
func DebugNodePath() string { return nodeBinaryPath(os.Getenv("LLAMA_SWAP_DEBUG_NODE_PATH")) }

// DebugExecutorScript returns the committed executor script path.
func DebugExecutorScript() string { return executorScriptPath() }

// InvokeWithNode executes one hook under the Node executor with the same
// contract as Invoke: returns (output, logs, error). Debug sessions use this
// when NodeExecutorEnabled is on; production always uses goja Invoke.
func (c *Compiled) InvokeWithNode(ctx context.Context, hook string, meta Context, input json.RawMessage, nodePath, executorScript string) (json.RawMessage, []string, error) {
	req := workerRequest{Bundle: c.Bundle, Manifest: c.Manifest, Settings: c.Settings, Hook: hook, Context: meta, Input: input}
	response, err := c.runNodeHook(ctx, req, c.hookBudget(), nodePath, executorScript)
	c.setLastError(err)
	return response.Output, response.Logs, err
}

// nodeBinaryPath returns the configured (or discovered) node executable.
func nodeBinaryPath(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return "node"
}

// runNodeHook executes one hook invocation under the Node executor.
// The executor script is the committed internal/extensions/executor/executor.js.
// ctx carries the per-request HostCaller; failures surface as rejections.
func (c *Compiled) runNodeHook(ctx context.Context, req workerRequest, budget time.Duration, nodePath, executorScript string) (workerResponse, error) {
	select {
	case c.poolSlots <- struct{}{}:
		defer func() { <-c.poolSlots }()
	case <-ctx.Done():
		return workerResponse{}, ctx.Err()
	}
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	// The extension bundle is evaluated with new Function() by the executor
	// itself, so --disallow-code-generation-from-strings cannot be used here.
	// Hardening instead: no Node builtins are reachable from the bundle (the
	// executor imports its own readline only), env is minimal, and the
	// host-call channel is the only authority.
	cmd := exec.CommandContext(ctx, nodePath, executorScript)
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=--no-warnings")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workerResponse{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workerResponse{}, err
	}
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return workerResponse{}, fmt.Errorf("node executor start: %w", err)
	}

	var logMu sync.Mutex
	var logs []string
	responseCh := make(chan workerResponse, 1)
	go func() {
		defer stdin.Close()
		writer := bufio.NewWriter(stdin)
		encoder := json.NewEncoder(writer)
		if err := encoder.Encode(req); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), maxWorkerLine+1)
		for scanner.Scan() {
			line := scanner.Bytes()
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(line, &probe); err != nil || probe.Type == "" {
				responseCh <- workerResponse{Error: "node executor protocol: malformed line"}
				return
			}
			switch probe.Type {
			case "log":
				var record logLine
				if err := json.Unmarshal(line, &record); err != nil {
					responseCh <- workerResponse{Error: "node executor protocol: malformed log line"}
					return
				}
				logMu.Lock()
				if len(logs) < 100 {
					logs = append(logs, record.Level+": "+record.Message)
				}
				logMu.Unlock()
				if c.LogSink != nil {
					c.LogSink(record)
				}
			case "call":
				var call hostCallMessage
				if err := json.Unmarshal(line, &call); err != nil {
					responseCh <- workerResponse{Error: "node executor protocol: malformed host call"}
					return
				}
				result := answerHostCall(ctx, HostCallerFrom(ctx), call)
				if err := encoder.Encode(result); err != nil {
					return
				}
				if err := writer.Flush(); err != nil {
					return
				}
			case "response":
				var response workerResponse
				if err := json.Unmarshal(line, &response); err != nil {
					responseCh <- workerResponse{Error: "node executor protocol: malformed response"}
					return
				}
				responseCh <- response
				return
			default:
				responseCh <- workerResponse{Error: fmt.Sprintf("node executor protocol: unknown message type %s", probe.Type)}
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	var response workerResponse
	gotResponse := false
	// The budget deadline is only armed when a budget exists; the debug
	// session may pause hooks, so a zero/absent budget means rely on ctx.
	var deadline <-chan time.Time
	if budget > 0 {
		timer := time.NewTimer(budget)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case response = <-responseCh:
			gotResponse = true
		case waitErr = <-done:
			if !gotResponse {
				return workerResponse{}, fmt.Errorf("node executor ended without a response: %s", strings.TrimSpace(stderr.String()))
			}
			goto complete
		case <-deadline:
			_ = cmd.Process.Kill()
			<-done
			return workerResponse{}, fmt.Errorf("node executor budget exceeded")
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return workerResponse{}, ctx.Err()
		}
		if gotResponse {
			select {
			case waitErr = <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
			}
			goto complete
		}
	}
complete:
	if waitErr != nil && !gotResponse {
		return workerResponse{}, fmt.Errorf("node executor stopped: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if response.Error != "" {
		return response, fmt.Errorf("%s", response.Error)
	}
	logMu.Lock()
	response.Logs = logs
	logMu.Unlock()
	return response, nil
}

// executorScriptPath locates the committed executor script relative to the
// binary's own source tree. Embedded deployments use the working directory.
func executorScriptPath() string {
	for _, candidate := range []string{
		filepath.Join("internal", "extensions", "executor", "executor.js"),
		filepath.Join("..", "..", "internal", "extensions", "executor", "executor.js"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// lookPathCached resolves node once per process.
var (
	nodeOnce     sync.Once
	nodeResolved string
	nodeErr      error
)

func lookPathCached() (string, error) {
	nodeOnce.Do(func() { nodeResolved, nodeErr = exec.LookPath("node") })
	return nodeResolved, nodeErr
}
