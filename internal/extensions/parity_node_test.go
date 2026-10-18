package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runNodeTest executes one hook through the Node executor and returns the
// response, skipping the test when node is unavailable.
func runNodeTest(t *testing.T, item *Compiled, req workerRequest) (workerResponse, error) {
	t.Helper()
	nodePath, err := execLookPath()
	if err != nil {
		t.Skip("node not available for parity test")
	}
	script := executorScriptPath()
	if script == "" {
		t.Skip("executor script not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return item.runNodeHook(ctx, req, 20*time.Second, nodePath, script)
}

func execLookPath() (string, error) {
	return lookPathCached()
}

func TestParity_NodeExecutorHookBasics(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "parity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: parity\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `export default {
  async onRequest(ctx, request) {
    ctx.log.info("parity-log");
    request.temperature = 0.5;
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("parity")
	if !ok {
		t.Fatal("no compile")
	}
	req := workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Settings: item.Settings, Hook: "onRequest", Context: Context{ResolvedModel: "m", Endpoint: "chat.completions"}, Input: json.RawMessage(`{"model":"m"}`)}

	// goja baseline
	gojaResp, gojaErr := item.run(context.Background(), req, 10*time.Second)
	if gojaErr != nil {
		t.Fatalf("goja: %v", gojaErr)
	}

	// node run
	nodeResp, nodeErr := runNodeTest(t, item, req)
	if nodeErr != nil {
		t.Fatalf("node: %v", nodeErr)
	}

	if !strings.Contains(string(nodeResp.Output), `"temperature":0.5`) {
		t.Fatalf("node output = %s", string(nodeResp.Output))
	}
	if string(nodeResp.Output) != string(gojaResp.Output) {
		t.Fatalf("parity drift:\n goja: %s\n node: %s", string(gojaResp.Output), string(nodeResp.Output))
	}
	found := false
	for _, line := range nodeResp.Logs {
		if strings.Contains(line, "parity-log") {
			found = true
		}
	}
	if !found {
		t.Fatalf("node logs missing parity-log: %v", nodeResp.Logs)
	}
}

// TestParity_NodeExecutorSeesConfig pins ctx.config across the two runtimes.
// The goja worker derives it from the manifest and the declared settings; the
// Node executor has no settings parser, so its request carries the finished map.
// Without that field a debug run saw an empty ctx.config while production saw
// the real one, which is the divergence this test exists to catch.
func TestParity_NodeExecutorSeesConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cfgsync")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "id: cfgsync\nenabled: true\nconfig:\n  greeting: from-the-manifest\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `export const settings = {
  greeting: { type: "string", label: "Greeting" },
  limit: { type: "number", label: "Limit", default: 7 }
};
export default {
  onRequest(ctx, request) {
    request.seen = { greeting: ctx.config.greeting, limit: ctx.config.limit };
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("cfgsync")
	if !ok {
		t.Fatal("no compile")
	}
	req := workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Settings: item.Settings, Hook: "onRequest", Context: Context{ResolvedModel: "m"}, Input: json.RawMessage(`{"model":"m"}`)}

	type seen struct {
		Greeting any `json:"greeting"`
		Limit    any `json:"limit"`
	}
	type output struct {
		Seen seen `json:"seen"`
	}
	decode := func(t *testing.T, what string, response workerResponse) output {
		t.Helper()
		var decoded output
		if err := json.Unmarshal(response.Output, &decoded); err != nil {
			t.Fatalf("%s output %s: %v", what, string(response.Output), err)
		}
		return decoded
	}

	gojaResp, err := item.run(context.Background(), req, 10*time.Second)
	if err != nil {
		t.Fatalf("goja: %v", err)
	}
	nodeResp, err := runNodeTest(t, item, req)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	fromGoja := decode(t, "goja", gojaResp)
	fromNode := decode(t, "node", nodeResp)

	if fromGoja.Seen.Greeting != "from-the-manifest" || fromGoja.Seen.Limit != float64(7) {
		t.Fatalf("goja saw %+v, want the manifest value and the declared default", fromGoja.Seen)
	}
	if fromNode != fromGoja {
		t.Fatalf("parity drift:\n goja: %+v\n node: %+v", fromGoja, fromNode)
	}
}

func TestParity_NodeExecutorSDKDispatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sdkg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: sdkg\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `import { defineTool, defineExtension } from "@llama-swap/extension";
export default defineExtension({
  tools: [
    defineTool({
      name: "echo",
      parameters: { type: "object", properties: { text: { type: "string" } } },
      async handler(args, ctx) { return { content: "echo:" + (args.text ?? "") }; },
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
	item, ok := m.Compiled("sdkg")
	if !ok {
		t.Fatal("no compile")
	}
	req := workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Settings: item.Settings, Hook: "onToolCall", Context: Context{ResolvedModel: "m"}, Input: json.RawMessage(`{"id":"c1","name":"echo","arguments":{"text":"hi"}}`)}
	resp, err := runNodeTest(t, item, req)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(resp.Output, &result); err != nil {
		t.Fatalf("output: %s", string(resp.Output))
	}
	if result.Content != "echo:hi" {
		t.Fatalf("content = %q", result.Content)
	}
}

// TestParity_NodeExecutorHostCalls exercises ctx.kv + ctx.http.fetch through
// the Node executor against a fake host, catching wire-format drift (the
// double-encoded args regression).
func TestParity_NodeExecutorHostCalls(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hcall")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: hcall\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `export default {
  async onRequest(ctx, request) {
    await ctx.kv.set("greeting", "hello", { scope: "global" });
    const readBack = await ctx.kv.get("greeting", { scope: "global" });
    request.kv = readBack;
    const fetched = await ctx.http.fetch("https://api.example.com/v1", { method: "GET" });
    request.status = fetched.status;
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("hcall")
	if !ok {
		t.Fatal("no compile")
	}

	fake := &routeFakeHost{}
	req := workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Settings: item.Settings, Hook: "onRequest", Context: Context{ResolvedModel: "m"}, Input: json.RawMessage(`{"model":"m"}`)}
	nodePath, err := lookPathCached()
	if err != nil {
		t.Skip("node not available")
	}
	script := executorScriptPath()
	if script == "" {
		t.Skip("executor script not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := item.runNodeHook(WithHostCaller(ctx, fake), req, 20*time.Second, nodePath, script)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var result struct {
		KV     string `json:"kv"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(resp.Output, &result); err != nil {
		t.Fatalf("output: %s", string(resp.Output))
	}
	if result.KV != "hello" {
		t.Fatalf("kv round trip = %q (host-call args encoding broken)", result.KV)
	}
	if result.Status != 200 {
		t.Fatalf("fetch status = %d", result.Status)
	}
	if len(fake.calls) == 0 {
		t.Fatal("host received no calls")
	}
}

// routeFakeHost serves the kv/http functions the parity script uses.
type routeFakeHost struct {
	calls []string
}

func (f *routeFakeHost) Exec(_ context.Context, fn string, args json.RawMessage) (any, error) {
	f.calls = append(f.calls, fn)
	switch fn {
	case "kv.set":
		return true, nil
	case "kv.get":
		return "hello", nil
	case "http.fetch":
		return map[string]any{"status": 200, "ok": true, "body": "{}"}, nil
	}
	return nil, nil
}
