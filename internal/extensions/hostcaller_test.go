package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeHost struct {
	calls []string
}

func (f *fakeHost) Exec(_ context.Context, fn string, args json.RawMessage) (any, error) {
	f.calls = append(f.calls, fn)
	if fn == "test.echo" {
		var payload struct {
			Value int `json:"value"`
		}
		_ = json.Unmarshal(args, &payload)
		return map[string]any{"value": payload.Value + 1}, nil
	}
	if fn == "test.fail" {
		return nil, context.DeadlineExceeded
	}
	return nil, nil
}

func hostCallTestExtension(t *testing.T, source string) *Compiled {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "hosty")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: hosty\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("hosty")
	if !ok {
		t.Fatal("extension did not compile")
	}
	return item
}

func TestExtensions_HostCallRoundTrip(t *testing.T) {
	item := hostCallTestExtension(t, `export default {
  async onRequest(ctx, request) {
    const first = await ctx.host.call("test.echo", { value: 41 });
    const batch = await Promise.all([
      ctx.host.call("test.echo", { value: 1 }),
      ctx.host.call("test.echo", { value: 2 }),
      ctx.host.call("test.echo", { value: 3 }),
    ]);
    return { echo: first.value, batch: batch.map((entry) => entry.value) };
  }
};`)
	host := &fakeHost{}
	output, logs, err := item.Invoke(WithHostCaller(context.Background(), host), "onRequest", Context{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var result struct {
		Echo  int   `json:"echo"`
		Batch []int `json:"batch"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output: %s", string(output))
	}
	if result.Echo != 42 {
		t.Fatalf("echo = %d", result.Echo)
	}
	if len(result.Batch) != 3 || result.Batch[0] != 2 || result.Batch[1] != 3 || result.Batch[2] != 4 {
		t.Fatalf("batch = %v", result.Batch)
	}
	if len(host.calls) != 4 {
		t.Fatalf("host calls = %v", host.calls)
	}
	if len(logs) != 0 {
		t.Fatalf("unexpected logs %v", logs)
	}
}

func TestExtensions_ContextSurfaceReachesScript(t *testing.T) {
	item := hostCallTestExtension(t, `export default {
  onRequest(ctx, request) {
    return {
      locale: ctx.locale,
      session: ctx.session,
      models: ctx.models.map((entry) => entry.id),
      requestId: ctx.requestId,
    };
  }
};`)
	output, _, err := item.Invoke(context.Background(), "onRequest", Context{
		RequestID: "req-1",
		Locale:    "zh-TW",
		Session:   &SessionContext{ID: "lspg-1", KeyID: "key-1", Anonymous: false},
		Models:    []ModelInfo{{ID: "m1", Name: "Model One"}, {ID: "m2"}},
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var result struct {
		Locale    string         `json:"locale"`
		Session   SessionContext `json:"session"`
		Models    []string       `json:"models"`
		RequestID string         `json:"requestId"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output: %s", string(output))
	}
	if result.Locale != "zh-TW" || result.RequestID != "req-1" {
		t.Fatalf("locale/requestId = %q/%q", result.Locale, result.RequestID)
	}
	if result.Session.ID != "lspg-1" || result.Session.KeyID != "key-1" || result.Session.Anonymous {
		t.Fatalf("session = %+v", result.Session)
	}
	if len(result.Models) != 2 || result.Models[0] != "m1" {
		t.Fatalf("models = %v", result.Models)
	}
}

func TestExtensions_SDKDefineToolDispatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sdky")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: sdky\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `import { defineTool, defineExtension } from "@llama-swap/extension";
export default defineExtension({
  tools: [
    defineTool({
      name: "echo",
      description: "Return the input text",
      parameters: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
      async handler(args, ctx) {
        ctx.log.info("sdk-handler-ran");
        return { content: "echo:" + (args.text ?? "") };
      },
    }),
  ],
});`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("sdky")
	if !ok {
		t.Fatal("no compile")
	}
	// Metadata: the model-facing definition must not carry a handler and the
	// tool is marked server-executed.
	if len(item.Tools) != 1 || item.Tools[0].Execution != "server" {
		t.Fatalf("tools = %+v", item.Tools)
	}
	var function struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(item.Tools[0].Function, &function); err != nil || function.Name != "echo" {
		t.Fatalf("function = %s", string(item.Tools[0].Function))
	}
	if len(item.ToolHandlers) != 1 || item.ToolHandlers[0] != "echo" {
		t.Fatalf("toolHandlers = %v", item.ToolHandlers)
	}

	// Dispatch: a tool call for "echo" runs the bound handler without an
	// onToolCall hook.
	output, logs, err := item.Invoke(context.Background(), "onToolCall", Context{}, json.RawMessage(`{"id":"call-1","name":"echo","arguments":{"text":"hi"}}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("output: %s", string(output))
	}
	if result.Content != "echo:hi" {
		t.Fatalf("content = %q", result.Content)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "sdk-handler-ran") {
			found = true
		}
	}
	if !found {
		t.Fatalf("handler did not log: %v", logs)
	}
}

func TestExtensions_SDKValidationRejectsBrokenHandlers(t *testing.T) {
	cases := map[string]string{
		"missing handler": `import { defineTool } from "@llama-swap/extension";
export default { tools: [defineTool({ name: "broken", parameters: {} })] };`,
		"duplicate names": `import { defineTool } from "@llama-swap/extension";
export default { tools: [
  defineTool({ name: "same", handler: () => ({}) }),
  defineTool({ name: "same", handler: () => ({}) }),
] };`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "broken")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: broken\nenabled: true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			m, err := NewManager(root)
			if err != nil {
				t.Fatal(err)
			}
			// Reload collects compile failures in Manager.errors instead of
			// failing: a broken SDK definition must not compile into items.
			if _, ok := m.Compiled("broken"); ok {
				t.Fatalf("%s: accepted a broken SDK definition", name)
			}
			var messages []string
			for _, message := range m.List() {
				if message.Manifest.ID == "broken" {
					messages = append(messages, message.LastError)
				}
			}
			if len(messages) == 0 || messages[0] == "" {
				t.Fatalf("%s: no diagnostic recorded", name)
			}
		})
	}
}

func TestExtensions_HostCallWithoutHostRejectsCleanly(t *testing.T) {
	item := hostCallTestExtension(t, `export default {
  async onRequest(ctx, request) {
    await ctx.host.call("test.echo", { value: 1 });
    return { ok: true };
  }
};`)
	_, _, err := item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "host function test.echo is unavailable") {
		t.Fatalf("expected unavailable rejection, got %v", err)
	}
}

func TestExtensions_HostCallFailureBecomesRejection(t *testing.T) {
	item := hostCallTestExtension(t, `export default {
  async onRequest(ctx, request) {
    await ctx.host.call("test.fail", {});
    return { ok: true };
  }
};`)
	_, _, err := item.Invoke(WithHostCaller(context.Background(), &fakeHost{}), "onRequest", Context{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected host failure rejection, got %v", err)
	}
}

func TestExtensions_StalledHookStillReports(t *testing.T) {
	item := hostCallTestExtension(t, `export default {
  async onRequest(ctx, request) {
    await ctx.host.call("test.echo", { value: 1 });
    await new Promise(() => {});
    return { ok: true };
  }
};`)
	_, _, err := item.Invoke(WithHostCaller(context.Background(), &fakeHost{}), "onRequest", Context{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "asynchronous hook did not settle") {
		t.Fatalf("expected stall error, got %v", err)
	}
}
