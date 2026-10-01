package extensions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/shirou/gopsutil/v4/process"
)

// inspectBudget bounds the compile-time metadata pass, which has to start a
// worker process rather than execute a hook.
const inspectBudget = 30 * time.Second

// publicHostCheck is the seam the host guard sits behind. It is a variable so a
// test can exercise the allowlist against a loopback server, which the real
// guard refuses on purpose.
var publicHostCheck = checkPublicHost

type workerRequest struct {
	Bundle   string          `json:"bundle"`
	Manifest Manifest        `json:"manifest"`
	Settings []SettingField  `json:"settings,omitempty"`
	Hook     string          `json:"hook"`
	Context  Context         `json:"context"`
	Input    json.RawMessage `json:"input"`
}

type workerResponse struct {
	Output       json.RawMessage `json:"output,omitempty"`
	Tools        []Tool          `json:"tools,omitempty"`
	ToolHandlers []string        `json:"toolHandlers,omitempty"`
	Hooks        []string        `json:"hooks,omitempty"`
	Settings     []SettingField  `json:"settings,omitempty"`
	Logs         []string        `json:"logs,omitempty"`
	Error        string          `json:"error,omitempty"`
}

// The worker protocol is line-based JSON in both directions. The first stdin
// line is the workerRequest; the final stdout line is a responseLine. Between
// them the worker may emit hostCallMessages, which the host answers with
// hostResultMessages on stdin — that round trip is what lets a hook await
// storage, forwarding and other host-side capabilities.

type responseLine struct {
	Type string `json:"type"`
	workerResponse
}

type hostCallMessage struct {
	Type      string          `json:"type"` // "call"
	ID        int64           `json:"id"`
	Fn        string          `json:"fn"`
	Args      json.RawMessage `json:"args,omitempty"`
	Extension string          `json:"extension,omitempty"` // worker-written, trusted by the host
}

type hostResultMessage struct {
	Type  string          `json:"type"` // "result"
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// maxHostCalls bounds the round trips one hook invocation may make; the hook's
// wall-clock budget remains the real limit, this just stops pathological
// ping-pong early with a clear message.
const maxHostCalls = 512

// maxWorkerLine caps one protocol line: the request (bundle + input) fits in
// 8 MiB, results and responses reuse the same bound.
const maxWorkerLine = 8 << 20

// workerBridge is the worker-side end of the host-call channel. Calls are
// fire-and-register: the promise is settled later by settleOne when the host's
// result line arrives, all on the single goroutine that runs the VM.
type workerBridge struct {
	vm        *goja.Runtime
	out       io.Writer
	extension string
	pending   map[int64]func(ok bool, value any, err string)
	nextID    int64
	count     int
}

func newWorkerBridge(vm *goja.Runtime, out io.Writer, extension string) *workerBridge {
	return &workerBridge{vm: vm, out: out, extension: extension, pending: map[int64]func(ok bool, value any, err string){}}
}

// call emits one host call and returns its promise. It never blocks: like
// ctx.http.fetch, the JS side must await the returned promise.
func (b *workerBridge) call(fn string, args json.RawMessage) (*goja.Promise, error) {
	if fn == "" {
		return nil, errors.New("host function is required")
	}
	if b.count >= maxHostCalls {
		return nil, fmt.Errorf("too many host calls (limit %d)", maxHostCalls)
	}
	b.count++
	id := b.nextID
	b.nextID++
	encoded, err := json.Marshal(hostCallMessage{Type: "call", ID: id, Fn: fn, Args: args, Extension: b.extension})
	if err != nil {
		return nil, err
	}
	if _, err := b.out.Write(append(encoded, '\n')); err != nil {
		return nil, err
	}
	promise, resolve, reject := b.vm.NewPromise()
	b.pending[id] = func(ok bool, value any, err string) {
		if ok {
			_ = resolve(value)
		} else {
			_ = reject(err)
		}
	}
	return promise, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

// nilWriter discards protocol lines; used for the module-init console where no
// host bridge exists yet. Records still land in the response's log slice.
type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("extension worker output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func (c *Compiled) Inspect(ctx context.Context) error {
	// The metadata pass gets its own budget. Starting a worker process on a
	// loaded machine can easily take longer than a hook budget as short as
	// 100ms, and rejecting a valid script for that reason would make saving it
	// impossible.
	ctx, cancel := context.WithTimeout(ctx, inspectBudget)
	defer cancel()
	response, err := c.run(ctx, workerRequest{Bundle: c.Bundle, Manifest: c.Manifest, Hook: "__metadata"}, inspectBudget)
	if err != nil {
		return err
	}
	c.Tools, c.Hooks = response.Tools, response.Hooks
	c.ToolHandlers = response.ToolHandlers
	c.Settings = response.Settings
	return nil
}

// hookBudget is the wall-clock budget a hook gets; api defaults to 10s.
func (c *Compiled) hookBudget() time.Duration {
	if c.Manifest.Timeout == "" {
		return 10 * time.Second
	}
	timeout, err := time.ParseDuration(c.Manifest.Timeout)
	if err != nil || timeout <= 0 {
		return 10 * time.Second
	}
	return timeout
}

func (c *Compiled) run(ctx context.Context, req workerRequest, budget time.Duration) (workerResponse, error) {
	if c.poolSlots == nil {
		c.poolSlots = make(chan struct{}, 2)
	}
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
	self, err := os.Executable()
	if err != nil {
		return workerResponse{}, err
	}
	cmd := exec.CommandContext(ctx, self, "-extension-worker")
	limit := c.Manifest.MaxMemoryMiB
	if limit == 0 {
		limit = 256
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), fmt.Sprintf("GOMEMLIMIT=%dMiB", limit)}
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
		return workerResponse{}, err
	}
	cleanupCgroup := attachExtensionCgroup(cmd.Process.Pid, limit)
	defer cleanupCgroup()
	// pumpWorkerStdio owns stdin and answers host calls until the terminal
	// response line arrives. Live log records go to the extension's sink.
	logSink := c.LogSink
	responseCh := pumpWorkerStdio(ctx, stdin, stdout, &req, HostCallerFrom(ctx), func(record logLine) {
		if logSink != nil {
			logSink(record)
		}
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid, _ := process.NewProcess(int32(cmd.Process.Pid))
	maxCPU := c.Manifest.MaxCPUMillis
	if maxCPU == 0 {
		maxCPU = 2000
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var waitErr error
	var limitReason string
	var response workerResponse
	gotResponse := false
	for {
		select {
		case response = <-responseCh:
			gotResponse = true
		case waitErr = <-done:
			goto complete
		case <-ticker.C:
			if pid == nil {
				continue
			}
			if memory, err := pid.MemoryInfo(); err == nil && memory.RSS > uint64(limit)<<20 {
				limitReason = "memory limit exceeded"
				_ = cmd.Process.Kill()
			}
			if times, err := pid.Times(); err == nil && (times.User+times.System)*1000 > float64(maxCPU) {
				limitReason = "CPU limit exceeded"
				_ = cmd.Process.Kill()
			}
		case <-ctx.Done():
			// A response already received is good work: return it even at the
			// edge of the budget instead of discarding it.
			if gotResponse {
				return response, nil
			}
			_ = cmd.Process.Kill()
			<-done
			return workerResponse{}, ctx.Err()
		}
		if gotResponse {
			// The worker exits right after emitting the response; reap it so
			// the CPU/memory limits of the remaining budget stay enforced.
			select {
			case waitErr = <-done:
			case <-ctx.Done():
				_ = cmd.Process.Kill()
				<-done
				return workerResponse{}, ctx.Err()
			}
			goto complete
		}
	}
complete:
	if limitReason != "" {
		return workerResponse{}, errors.New(limitReason)
	}
	if waitErr != nil {
		// A hook failure (response.Error set) still ends the worker with a
		// non-zero exit; the error text is in the response, not stderr.
		if gotResponse && response.Error != "" {
			return response, errors.New(response.Error)
		}
		return workerResponse{}, fmt.Errorf("extension worker stopped: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if !gotResponse {
		// EOF without a response line: a hook that rejected with a proper
		// error still emits its terminal response first, so this can only be
		// an abnormal worker death. But a benign os.Exit path raced the
		// scanner in rare loads — accept a captured error response too.
		if response.Error != "" {
			return response, errors.New(response.Error)
		}
		return workerResponse{}, errors.New("extension worker ended without a response")
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func RunWorker(in io.Reader, out io.Writer) {
	response := workerResponse{}
	// emit writes the terminal response line exactly once, including on panic.
	emit := func(resp workerResponse) {
		encoded, err := json.Marshal(responseLine{Type: "response", workerResponse: resp})
		if err != nil {
			encoded, _ = json.Marshal(responseLine{Type: "response", workerResponse: workerResponse{Error: err.Error()}})
		}
		_, _ = out.Write(append(encoded, '\n'))
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response.Error = fmt.Sprint(recovered)
			if os.Getenv("LLAMA_SWAP_WORKER_PANIC_STACK") != "" {
				fmt.Fprintf(os.Stderr, "%+v\n", recovered)
				debug.PrintStack()
			}
		}
		emit(response)
	}()
	reader := bufio.NewReaderSize(io.LimitReader(in, 16<<20), 64<<10)
	requestLine, err := readWorkerLine(reader, maxWorkerLine)
	if err != nil {
		response.Error = err.Error()
		return
	}
	var req workerRequest
	if err := json.Unmarshal(requestLine, &req); err != nil {
		response.Error = err.Error()
		return
	}
	vm := goja.New()
	// Module-init code may log before any hook runs; give it a bare console
	// wired to the same stream (later hook dispatches replace it with a
	// hook-labeled one).
	moduleLogs := makeLogEmit(nilWriter{}, "module", &response.Logs)
	initConsole := map[string]any{}
	for name, level := range map[string]string{"debug": "debug", "log": "info", "info": "info", "warn": "warn", "error": "error"} {
		initConsole[name] = moduleLogs(level)
	}
	vm.Set("console", initConsole)
	if _, err := vm.RunString(req.Bundle); err != nil {
		response.Error = err.Error()
		return
	}
	exported := vm.Get("__llamaSwapExtension")
	if goja.IsUndefined(exported) {
		response.Error = "extension has no default export"
		return
	}
	module := exported.ToObject(vm).Get("default")
	// A namespace without a default export yields a Go nil *Object here (not
	// an undefined/null value), which would nil-panic in ToObject below.
	if module == nil {
		response.Error = "extension has no default export (export default { ... } is required)"
		return
	}
	if goja.IsUndefined(module) || goja.IsNull(module) {
		response.Error = "extension has no default export"
		return
	}
	object := module.ToObject(vm)
	// Named exports live beside `default` on the module namespace, which is
	// where a script declares its settings.
	namespace := exported.ToObject(vm)
	if req.Hook == "__metadata" {
		for _, name := range []string{"onRequest", "onBeforeForward", "onToolCall", "onToolResult", "onResponse", "onStreamEvent", "onError"} {
			if _, ok := goja.AssertFunction(object.Get(name)); ok {
				response.Hooks = append(response.Hooks, name)
			}
		}
		if tools := object.Get("tools"); tools != nil && !goja.IsUndefined(tools) && !goja.IsNull(tools) {
			// JSON.stringify on the VM side drops functions (SDK handlers) and
			// keeps everything else; Export() would fail on goja function values.
			stringify, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify"))
			encoded, err := stringify(goja.Undefined(), tools)
			if err != nil {
				response.Error = err.Error()
				return
			}
			if err := json.Unmarshal([]byte(encoded.String()), &response.Tools); err != nil {
				response.Error = err.Error()
				return
			}
			// SDK validation runs on the live entries (not the stringified
			// copy, which has dropped the handlers): name presence, duplicates
			// and bound-handler checks are the SDK's compile-time contract.
			validateProgram, validatorErr := vm.RunString(`(function (tools) {
  var seen = {};
  var bound = [];
  for (var index = 0; index < tools.length; index++) {
    var entry = tools[index];
    if (!entry || typeof entry !== "object") {
      return "tool entry must be an object";
    }
    if (entry.__llamaSwapTool !== true) {
      // Legacy shape ({type, function, execution}) skips SDK validation.
      continue;
    }
    if (typeof entry.name !== "string" || entry.name === "") {
      return "tool entry is missing a name";
    }
    if (seen[entry.name]) return "duplicate tool \"" + entry.name + "\"";
    seen[entry.name] = true;
    if (typeof entry.handler !== "function") {
      return "defineTool(" + entry.name + ") requires a handler function";
    }
    bound.push(entry.name);
  }
  return bound;
})`)
			if validatorErr != nil {
				response.Error = validatorErr.Error()
				return
			}
			validate, ok := goja.AssertFunction(validateProgram)
			if !ok {
				response.Error = "sdk validator unavailable"
				return
			}
			validatorResult, err := validate(goja.Undefined(), tools)
			if err != nil {
				response.Error = err.Error()
				return
			}
			switch typed := validatorResult.Export().(type) {
			case []any:
				for _, name := range typed {
					if text, ok := name.(string); ok {
						response.ToolHandlers = append(response.ToolHandlers, text)
					}
				}
			case string:
				response.Error = typed
				return
			}
			// SDK defineTool entries are normalized to the runtime Tool shape
			// (handler stripped, execution forced) from the stringified copy.
			// Legacy entries ({type, function, execution}) pass through as-is.
			var raw []map[string]any
			if err := json.Unmarshal([]byte(encoded.String()), &raw); err == nil && len(raw) > 0 {
				normalized := make([]Tool, 0, len(raw))
				legacy := false
				for index, entry := range raw {
					if _, bound := entry[toolHandlerMarker]; !bound {
						if _, hasType := entry["type"]; hasType {
							// Legacy shape: keep the original wire form.
							if index < len(response.Tools) {
								normalized = append(normalized, response.Tools[index])
							}
							legacy = true
							continue
						}
						response.Error = "tools entry must use the legacy {type, function} shape or the SDK defineTool form"
						return
					}
					name, _ := entry["name"].(string)
					if name == "" {
						continue
					}
					parameters := entry["parameters"]
					function := map[string]any{"name": name}
					if description, ok := entry["description"]; ok {
						function["description"] = description
					}
					if parameters != nil {
						function["parameters"] = parameters
					}
					if strict, ok := entry["strict"]; ok {
						function["strict"] = strict
					}
					execution := "client"
					if requested, ok := entry["execution"].(string); ok && requested != "" {
						execution = requested
					}
					for _, bound := range response.ToolHandlers {
						if bound == name {
							execution = "server"
							break
						}
					}
					functionJSON, err := json.Marshal(function)
					if err != nil {
						response.Error = err.Error()
						return
					}
					normalized = append(normalized, Tool{Type: "function", Function: functionJSON, Execution: execution})
				}
				if !legacy {
					response.Tools = normalized
				}
			}
		}
		if declared := namespace.Get("settings"); declared != nil && !goja.IsUndefined(declared) && !goja.IsNull(declared) {
			encoded, err := json.Marshal(declared.Export())
			if err != nil {
				response.Error = err.Error()
				return
			}
			var spec any
			if err := json.Unmarshal(encoded, &spec); err != nil {
				response.Error = err.Error()
				return
			}
			fields, err := parseSettingsSpec(spec)
			if err != nil {
				response.Error = err.Error()
				return
			}
			response.Settings = fields
		}
		return
	}
	// SDK dispatch: a tool call for a defineTool-bound name runs its handler
	// directly when no explicit onToolCall hook exists.
	if req.Hook == "onToolCall" {
		if _, hasHook := goja.AssertFunction(object.Get("onToolCall")); !hasHook {
			inputParsed := parseHookInput(vm, req.Input)
			if inputParsed.ErrorString != "" {
				response.Error = inputParsed.ErrorString
				return
			}
			if bound := boundToolHandler(vm, object, req.Input); bound != nil {
				bridge := newWorkerBridge(vm, out, req.Manifest.ID)
				ctxObj := makeContext(vm, req.Manifest, req.Settings, req.Context, &response.Logs, bridge, req.Hook)
				// handler(args, ctx): args are the call's parsed arguments, the
				// request context the second parameter.
				args := goja.Null()
				if parse, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse")); ok {
					var callPayload struct {
						Arguments json.RawMessage `json:"arguments"`
					}
					_ = json.Unmarshal(req.Input, &callPayload)
					if len(callPayload.Arguments) > 0 {
						if parsed, err := parse(goja.Undefined(), vm.ToValue(string(callPayload.Arguments))); err == nil {
							args = parsed
						}
					}
				}
				value, err := bound(module, args, ctxObj)
				finishHookValue(vm, req, &response, bridge, reader, value, err)
				return
			}
		}
	}
	fn, ok := goja.AssertFunction(object.Get(req.Hook))
	if !ok {
		response.Output = req.Input
		return
	}
	input := parseHookInput(vm, req.Input)
	if input.ErrorString != "" {
		response.Error = input.ErrorString
		return
	}
	bridge := newWorkerBridge(vm, out, req.Manifest.ID)
	ctxObj := makeContext(vm, req.Manifest, req.Settings, req.Context, &response.Logs, bridge, req.Hook)
	value, err := fn(module, ctxObj, input.Value)
	finishHookValue(vm, req, &response, bridge, reader, value, err)
}

// finishHookValue marshals one hook invocation's result: promise pumping
// (settling host calls as their result lines arrive), pass-through for
// null/undefined, and JSON output otherwise. It mutates response and returns
// once the terminal state is known.
func finishHookValue(vm *goja.Runtime, req workerRequest, response *workerResponse, bridge *workerBridge, reader *bufio.Reader, value goja.Value, err error) {
	if err != nil {
		response.Error = err.Error()
		return
	}
	promise, isPromise := value.Export().(*goja.Promise)
	if isPromise {
		// Pump pending promises: settle host calls as their result lines
		// arrive, run the queued continuations, repeat until the hook settles
		// or stalls. Everything stays on this goroutine — the VM is not
		// thread-safe, and the host only writes results in response to calls
		// the worker already emitted.
		for {
			switch promise.State() {
			case goja.PromiseStateFulfilled:
				value = promise.Result()
			case goja.PromiseStateRejected:
				// The rejection reason is reported as text: exporting an Error
				// object yields its property bag, whose enumerable properties are
				// none, so the message would otherwise be lost as "map[]".
				response.Error = rejectionMessage(promise.Result())
				return
			default:
				if len(bridge.pending) == 0 {
					response.Error = "asynchronous hook did not settle"
					return
				}
				if err := settleHostCall(reader, bridge); err != nil {
					response.Error = err.Error()
					return
				}
				// This goja build has no public RunJobs: every top-level
				// runtime entry drains the promise job queue on return, so an
				// empty program is the pump. Continuations may issue further
				// host calls, which the loop picks up.
				if _, err := vm.RunString(";"); err != nil {
					response.Error = err.Error()
					return
				}
				continue
			}
			break
		}
	}
	if goja.IsUndefined(value) || goja.IsNull(value) {
		response.Output = req.Input
		return
	}
	var marshalErr error
	response.Output, marshalErr = json.Marshal(value.Export())
	if marshalErr != nil {
		response.Error = marshalErr.Error()
	}
}

// hookInput carries the parsed hook payload. A parse failure surfaces through
// ErrorString so callers can reject the invocation.
type hookInput struct {
	Value       goja.Value
	ErrorString string
}

// consoleLabel reads the first argument as a label, defaulting when absent.
func consoleLabel(call goja.FunctionCall, fallback string) string {
	if len(call.Arguments) > 0 && !goja.IsUndefined(call.Arguments[0]) && !goja.IsNull(call.Arguments[0]) {
		if label := call.Arguments[0].String(); label != "" {
			return label
		}
	}
	return fallback
}

// parseHookInput parses the raw JSON payload into a VM value.
func parseHookInput(vm *goja.Runtime, raw json.RawMessage) hookInput {
	if len(raw) == 0 {
		return hookInput{Value: goja.Null()}
	}
	parse, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
	parsed, parseErr := parse(goja.Undefined(), vm.ToValue(string(raw)))
	if parseErr != nil {
		return hookInput{ErrorString: parseErr.Error()}
	}
	return hookInput{Value: parsed}
}

// boundToolHandler finds the SDK handler for a tool call input. The input is
// {id, name, arguments}; only a name bound through defineTool resolves. The
// returned callable is the handler straight off the tools entry — no
// export/round-trip that would detach it from the VM realm.
func boundToolHandler(vm *goja.Runtime, object *goja.Object, input json.RawMessage) goja.Callable {
	if len(input) == 0 {
		return nil
	}
	var call struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &call); err != nil || call.Name == "" {
		return nil
	}
	tools := object.Get("tools")
	if tools == nil || goja.IsUndefined(tools) || goja.IsNull(tools) {
		return nil
	}
	lengthValue := tools.ToObject(vm).Get("length")
	if lengthValue == nil || goja.IsUndefined(lengthValue) {
		return nil
	}
	length := int(lengthValue.ToInteger())
	for index := 0; index < length; index++ {
		entry := tools.ToObject(vm).Get(fmt.Sprintf("%d", index))
		if entry == nil || goja.IsUndefined(entry) || goja.IsNull(entry) {
			continue
		}
		entryObject := entry.ToObject(vm)
		nameValue := entryObject.Get("name")
		if nameValue == nil || nameValue.String() != call.Name {
			continue
		}
		marker := entryObject.Get(toolHandlerMarker)
		if marker == nil || goja.IsUndefined(marker) || marker.ToBoolean() != true {
			return nil
		}
		handler, ok := goja.AssertFunction(entryObject.Get("handler"))
		if !ok {
			return nil
		}
		return handler
	}
	return nil
}

// settleHostCall reads one result line and resolves the matching pending call.
func settleHostCall(reader *bufio.Reader, bridge *workerBridge) error {
	line, err := readWorkerLine(reader, maxWorkerLine)
	if err != nil {
		return fmt.Errorf("host call channel: %w", err)
	}
	var result hostResultMessage
	if err := json.Unmarshal(line, &result); err != nil || result.Type != "result" {
		return errors.New("host call channel: malformed result line")
	}
	settle, ok := bridge.pending[result.ID]
	if !ok {
		return fmt.Errorf("host call channel: unexpected result for call %d", result.ID)
	}
	delete(bridge.pending, result.ID)
	if result.OK {
		var value any
		if len(result.Value) > 0 {
			if err := json.Unmarshal(result.Value, &value); err != nil {
				settle(false, nil, "host call result is invalid: "+err.Error())
				return nil
			}
		}
		settle(true, bridge.vm.ToValue(value), "")
		return nil
	}
	failure := result.Error
	if failure == "" {
		failure = "host call failed with no reason"
	}
	settle(false, nil, failure)
	return nil
}

// readWorkerLine reads one newline-terminated protocol line with a size cap.
func readWorkerLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var buffer []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		buffer = append(buffer, chunk...)
		if len(buffer) > limit {
			return nil, errors.New("protocol line exceeds size limit")
		}
		if err == nil {
			return bytes.TrimRight(buffer, "\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buffer) > 0 {
			return bytes.TrimRight(buffer, "\n"), nil
		}
		return nil, err
	}
}

// rejectionMessage renders why a rejected promise rejected, preferring the
// message property of an Error and falling back to the value's own string.
func rejectionMessage(value goja.Value) string {
	if object, ok := value.(*goja.Object); ok {
		if message := object.Get("message"); message != nil && !goja.IsUndefined(message) && !goja.IsNull(message) {
			if text := message.String(); text != "" {
				if name := object.Get("name"); name != nil && !goja.IsUndefined(name) {
					if prefix := name.String(); prefix != "" && !strings.HasPrefix(text, prefix) {
						return prefix + ": " + text
					}
				}
				return text
			}
		}
	}
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return "hook rejected without a reason"
	}
	return value.String()
}

// logMessage renders a console argument the way a developer expects: objects
// are serialized, and anything else is stringified. goja's default String()
// would turn an object into "[object Object]".
func logMessage(call goja.FunctionCall) string {
	parts := make([]string, 0, len(call.Arguments))
	for _, argument := range call.Arguments {
		switch {
		case argument == nil || goja.IsUndefined(argument) || goja.IsNull(argument):
			parts = append(parts, argument.String())
		default:
			value := argument.Export()
			switch typed := value.(type) {
			case string:
				parts = append(parts, typed)
			case map[string]any, []any:
				encoded, err := json.Marshal(typed)
				if err != nil {
					parts = append(parts, argument.String())
					continue
				}
				parts = append(parts, string(encoded))
			default:
				parts = append(parts, argument.String())
			}
		}
	}
	return strings.Join(parts, " ")
}

// logLine is one structured log record the worker emits immediately when a
// script calls ctx.log / console.* (protocol line {"type":"log",...}).
type logLine struct {
	Type      string    `json:"type"` // "log"
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	Hook      string    `json:"hook,omitempty"`
	Timestamp time.Time `json:"ts"`
}

// LogRecord is the host-side view of one logLine, enriched by the pump.
type LogRecord = logLine

// makeLogEmit builds the log function shared by ctx.log and console. Every
// call appends to the response's bounded log slice (the legacy contract) AND
// writes an immediate {"type":"log"} protocol line so the host can stream logs
// live instead of waiting for the hook to return.
func makeLogEmit(out io.Writer, hook string, logs *[]string) func(level string) func(goja.FunctionCall) goja.Value {
	return func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			message := logMessage(call)
			if len(*logs) < 100 {
				*logs = append(*logs, level+": "+message)
			}
			encoded, err := json.Marshal(logLine{Type: "log", Level: level, Message: message, Hook: hook, Timestamp: time.Now().UTC()})
			if err == nil {
				_, _ = out.Write(append(encoded, '\n'))
			}
			return goja.Undefined()
		}
	}
}

func makeContext(vm *goja.Runtime, manifest Manifest, settings []SettingField, meta Context, logs *[]string, bridge *workerBridge, hook string) goja.Value {
	// The script sees its config with declared defaults filled in, so an
	// untouched setting still has a usable value while the manifest only
	// stores what the user actually chose.
	config := applySettingDefaults(settings, manifest.Config)
	logFn := makeLogEmit(bridge.out, hook, logs)
	// console is a familiar habit, so it lands in the same log stream as
	// ctx.log instead of throwing on a script that forgets to use ctx.
	console := map[string]any{}
	for name, level := range map[string]string{"debug": "debug", "log": "info", "info": "info", "warn": "warn", "error": "error"} {
		console[name] = logFn(level)
	}
	ctx := map[string]any{
		"requestId": meta.RequestID, "requestedModel": meta.RequestedModel,
		"resolvedModel": meta.ResolvedModel, "profile": meta.Profile,
		"provider": meta.Provider, "endpoint": meta.Endpoint, "stream": meta.Stream,
		"locale":    meta.Locale,
		"extension": manifest.ID, "config": config, "abortSignal": map[string]any{"aborted": false},
		"log":     map[string]any{"debug": logFn("debug"), "info": logFn("info"), "warn": logFn("warn"), "error": logFn("error")},
		"console": console,
	}
	// The rest of the console family maps onto the same stream: assert logs on
	// failure, time/timeLog/timeEnd and count/countReset keep per-label state,
	// group* adjust an indent prefix, table/trace/clear degrade gracefully.
	timerState := map[string]time.Time{}
	countState := map[string]int{}
	groupDepth := 0
	groupPrefix := func() string { return strings.Repeat("  ", groupDepth) }
	wrapped := func(level string, inner func(goja.FunctionCall) goja.Value) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			call.Arguments = append([]goja.Value{vm.ToValue(groupPrefix())}, call.Arguments...)
			return inner(call)
		}
	}
	console["assert"] = func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) > 0 && call.Arguments[0].ToBoolean() {
			return goja.Undefined()
		}
		rest := goja.Null()
		if len(call.Arguments) > 1 {
			rest = call.Arguments[1]
		}
		return logFn("error")(goja.FunctionCall{This: call.This, Arguments: append([]goja.Value{rest}, call.Arguments[2:]...)})
	}
	console["trace"] = wrapped("info", logFn("info"))
	console["table"] = wrapped("info", logFn("info"))
	console["group"] = func(call goja.FunctionCall) goja.Value {
		logFn("info")(goja.FunctionCall{This: call.This, Arguments: append([]goja.Value{vm.ToValue("▼ " + groupPrefix())}, call.Arguments...)})
		groupDepth++
		return goja.Undefined()
	}
	console["groupCollapsed"] = console["group"]
	console["groupEnd"] = func(call goja.FunctionCall) goja.Value {
		if groupDepth > 0 {
			groupDepth--
		}
		return goja.Undefined()
	}
	console["time"] = func(call goja.FunctionCall) goja.Value {
		timerState[consoleLabel(call, "default")] = time.Now()
		return goja.Undefined()
	}
	console["timeLog"] = func(call goja.FunctionCall) goja.Value {
		label := consoleLabel(call, "default")
		if started, ok := timerState[label]; ok {
			elapsed := time.Since(started).Round(time.Millisecond)
			logFn("info")(goja.FunctionCall{This: call.This, Arguments: []goja.Value{vm.ToValue(groupPrefix() + label + ": " + elapsed.String())}})
		}
		return goja.Undefined()
	}
	console["timeEnd"] = func(call goja.FunctionCall) goja.Value {
		label := consoleLabel(call, "default")
		if started, ok := timerState[label]; ok {
			elapsed := time.Since(started).Round(time.Millisecond)
			logFn("info")(goja.FunctionCall{This: call.This, Arguments: []goja.Value{vm.ToValue(groupPrefix() + label + ": " + elapsed.String())}})
			delete(timerState, label)
		}
		return goja.Undefined()
	}
	console["count"] = func(call goja.FunctionCall) goja.Value {
		label := consoleLabel(call, "default")
		countState[label]++
		logFn("info")(goja.FunctionCall{This: call.This, Arguments: []goja.Value{vm.ToValue(fmt.Sprintf("%s%s: %d", groupPrefix(), label, countState[label]))}})
		return goja.Undefined()
	}
	console["countReset"] = func(call goja.FunctionCall) goja.Value {
		delete(countState, consoleLabel(call, "default"))
		return goja.Undefined()
	}
	console["clear"] = func(call goja.FunctionCall) goja.Value {
		*logs = (*logs)[:0]
		return goja.Undefined()
	}
	models := make([]any, 0, len(meta.Models))
	for _, model := range meta.Models {
		entry := map[string]any{"id": model.ID}
		if model.Name != "" {
			entry["name"] = model.Name
		}
		models = append(models, entry)
	}
	ctx["models"] = models
	if meta.Session != nil {
		ctx["session"] = map[string]any{"id": meta.Session.ID, "keyId": meta.Session.KeyID, "anonymous": meta.Session.Anonymous}
	}
	ctx["http"] = map[string]any{"fetch": func(call goja.FunctionCall) goja.Value {
		p, resolve, reject := vm.NewPromise()
		if meta.DryRun {
			_ = reject("network is disabled in extension test mode")
			return vm.ToValue(p)
		}
		target := call.Argument(0).String()
		options := map[string]any{}
		if value := call.Argument(1); value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
			if exported, ok := value.Export().(map[string]any); ok {
				options = exported
			}
		}
		body, status, err := FetchAllowed(manifest.Permissions.NetworkHosts, target, options)
		if err != nil {
			_ = reject(err.Error())
		} else {
			_ = resolve(map[string]any{"status": status, "ok": status >= 200 && status < 300, "text": func() string { return string(body) }, "json": func() any { var result any; _ = json.Unmarshal(body, &result); return result }})
		}
		return vm.ToValue(p)
	}}
	ctx["files"] = map[string]any{
		"read": func(call goja.FunctionCall) goja.Value {
			p, resolve, reject := vm.NewPromise()
			if meta.DryRun {
				_ = reject("files are disabled in extension test mode")
				return vm.ToValue(p)
			}
			file, err := AllowedPath(manifest.Permissions.ReadRoots, call.Argument(0).String(), false)
			if err == nil {
				var data []byte
				var opened *os.File
				opened, err = os.Open(file)
				if err == nil {
					data, err = io.ReadAll(io.LimitReader(opened, 10<<20+1))
					_ = opened.Close()
				}
				if len(data) > 10<<20 {
					err = errors.New("file exceeds 10 MiB")
				}
				if err == nil {
					_ = resolve(string(data))
				}
			}
			if err != nil {
				_ = reject(err.Error())
			}
			return vm.ToValue(p)
		},
		"write": func(call goja.FunctionCall) goja.Value {
			p, resolve, reject := vm.NewPromise()
			if meta.DryRun {
				_ = reject("files are disabled in extension test mode")
				return vm.ToValue(p)
			}
			file, err := AllowedPath(manifest.Permissions.WriteRoots, call.Argument(0).String(), true)
			data := call.Argument(1).String()
			if len(data) > 10<<20 {
				err = errors.New("file exceeds 10 MiB")
			}
			if err == nil {
				err = os.WriteFile(file, []byte(data), 0o600)
			}
			if err != nil {
				_ = reject(err.Error())
			} else {
				_ = resolve(true)
			}
			return vm.ToValue(p)
		},
	}
	if bridge != nil {
		// ctx.host.call is the raw end of the host-call channel; the public
		// namespaces (ctx.kv, ctx.models, ...) are thin wrappers over it. The
		// host validates every call, so this surface adds no authority.
		ctx["host"] = map[string]any{"call": func(call goja.FunctionCall) goja.Value {
			fn := call.Argument(0).String()
			var args json.RawMessage
			if argument := call.Argument(1); argument != nil && !goja.IsUndefined(argument) && !goja.IsNull(argument) {
				encoded, err := json.Marshal(argument.Export())
				if err != nil {
					p, _, reject := vm.NewPromise()
					_ = reject("host call arguments are invalid: " + err.Error())
					return vm.ToValue(p)
				}
				args = encoded
			}
			promise, err := bridge.call(fn, args)
			if err != nil {
				p, _, reject := vm.NewPromise()
				_ = reject(err.Error())
				return vm.ToValue(p)
			}
			return vm.ToValue(promise)
		}}
		// kvOperation builds the host call payload from positional script
		// arguments: kv.get(key, opts?), kv.delete(key, opts?), keys(prefix, opts?).
		kvOperation := func(fn string, positional string) func(goja.FunctionCall) goja.Value {
			return func(call goja.FunctionCall) goja.Value {
				payload := map[string]any{}
				if positional == "prefix" {
					if argument := call.Argument(0); argument != nil && !goja.IsUndefined(argument) && !goja.IsNull(argument) {
						payload["prefix"] = argument.String()
					}
				} else if argument := call.Argument(0); argument != nil && !goja.IsUndefined(argument) {
					payload["key"] = argument.String()
				}
				if opts := call.Argument(1); opts != nil && !goja.IsUndefined(opts) && !goja.IsNull(opts) {
					if exported, ok := opts.Export().(map[string]any); ok {
						for name, entry := range exported {
							payload[name] = entry
						}
					}
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					p, _, reject := vm.NewPromise()
					_ = reject("host call arguments are invalid: " + err.Error())
					return vm.ToValue(p)
				}
				promise, err := bridge.call(fn, encoded)
				if err != nil {
					p, _, reject := vm.NewPromise()
					_ = reject(err.Error())
					return vm.ToValue(p)
				}
				return vm.ToValue(promise)
			}
		}
		ctx["kv"] = map[string]any{
			"get":    kvOperation("kv.get", "key"),
			"delete": kvOperation("kv.delete", "key"),
			"keys":   kvOperation("kv.keys", "prefix"),
			"set": func(call goja.FunctionCall) goja.Value {
				// set(key, value, opts?) — value is passed positionally so any
				// JSON shape round-trips.
				payload := map[string]any{}
				if argument := call.Argument(0); argument != nil && !goja.IsUndefined(argument) {
					payload["key"] = argument.String()
				}
				if argument := call.Argument(1); argument != nil && !goja.IsUndefined(argument) {
					var value any
					if parsed, err := json.Marshal(argument.Export()); err == nil {
						_ = json.Unmarshal(parsed, &value)
					}
					payload["value"] = value
				}
				if opts := call.Argument(2); opts != nil && !goja.IsUndefined(opts) && !goja.IsNull(opts) {
					if exported, ok := opts.Export().(map[string]any); ok {
						for name, entry := range exported {
							payload[name] = entry
						}
					}
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					p, _, reject := vm.NewPromise()
					_ = reject("host call arguments are invalid: " + err.Error())
					return vm.ToValue(p)
				}
				promise, err := bridge.call("kv.set", encoded)
				if err != nil {
					p, _, reject := vm.NewPromise()
					_ = reject(err.Error())
					return vm.ToValue(p)
				}
				return vm.ToValue(promise)
			},
		}
		// forward(model, request) runs one non-streaming model call on behalf
		// of the caller's identity and resolves to {status, body}.
		ctx["forward"] = func(call goja.FunctionCall) goja.Value {
			payload := map[string]any{}
			if argument := call.Argument(0); argument != nil && !goja.IsUndefined(argument) {
				payload["model"] = argument.String()
			}
			if argument := call.Argument(1); argument != nil && !goja.IsUndefined(argument) && !goja.IsNull(argument) {
				if exported, ok := argument.Export().(map[string]any); ok {
					payload["request"] = exported
				}
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				p, _, reject := vm.NewPromise()
				_ = reject("host call arguments are invalid: " + err.Error())
				return vm.ToValue(p)
			}
			promise, err := bridge.call("models.forward", encoded)
			if err != nil {
				p, _, reject := vm.NewPromise()
				_ = reject(err.Error())
				return vm.ToValue(p)
			}
			return vm.ToValue(promise)
		}
		// usage() resolves to the caller's 24h aggregate request/token counts.
		ctx["usage"] = func(call goja.FunctionCall) goja.Value {
			_ = call
			promise, err := bridge.call("session.usage", nil)
			if err != nil {
				p, _, reject := vm.NewPromise()
				_ = reject(err.Error())
				return vm.ToValue(p)
			}
			return vm.ToValue(promise)
		}
	}
	// Bare `console` must resolve too (the Node executor exposes console
	// globally): install the per-hook console on the VM so scripts that use
	// console.* without ctx see the same stream instead of a ReferenceError.
	vm.Set("console", console)
	return vm.ToValue(ctx)
}

func FetchAllowed(hosts []string, raw string, options map[string]any) ([]byte, int, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, 0, errors.New("invalid HTTP URL")
	}
	if u.Hostname() == "" {
		return nil, 0, errors.New("invalid HTTP URL")
	}
	if !matchesExcluded(hosts, u.Hostname()) {
		return nil, 0, errors.New("network host is not permitted")
	}
	// The allowlist names a host, not a machine: a URL that points at the
	// daemon's own host, at something on the loopback interface, or into a
	// private network is refused before a socket is opened.
	if err := publicHostCheck(u.Hostname()); err != nil {
		return nil, 0, err
	}
	method, _ := options["method"].(string)
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, 0, errors.New("HTTP method is not permitted")
	}
	var requestBody io.Reader
	if body, ok := options["body"].(string); ok {
		if len(body) > 10<<20 {
			return nil, 0, errors.New("HTTP request exceeds 10 MiB")
		}
		requestBody = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, raw, requestBody)
	if err != nil {
		return nil, 0, err
	}
	if headers, ok := options["headers"].(map[string]any); ok {
		for key, rawValue := range headers {
			value, ok := rawValue.(string)
			if !ok || len(key)+len(value) > 8192 || strings.EqualFold(key, "Host") {
				return nil, 0, errors.New("invalid HTTP header")
			}
			request.Header.Set(key, value)
		}
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !matchesExcluded(hosts, req.URL.Hostname()) {
			return errors.New("redirect host is not permitted")
		}
		if err := checkPublicHost(req.URL.Hostname()); err != nil {
			return err
		}
		if len(via) > 0 && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}}
	resp, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > 10<<20 {
		return nil, 0, errors.New("HTTP response exceeds 10 MiB")
	}
	return data, resp.StatusCode, nil
}

func AllowedPath(roots []string, file string, write bool) (string, error) {
	if !filepath.IsAbs(file) {
		return "", errors.New("file path must be absolute")
	}
	check := file
	if write {
		if info, err := os.Lstat(file); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return "", errors.New("write target must be a regular file")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		check = filepath.Dir(file)
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		base, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(base, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return file, nil
		}
	}
	return "", errors.New("file path is not permitted")
}
