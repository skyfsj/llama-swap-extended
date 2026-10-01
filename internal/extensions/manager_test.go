package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "-extension-worker" {
		RunWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExtensions_MatchOrderAndExclude(t *testing.T) {
	root := t.TempDir()
	for _, item := range []struct {
		id       string
		priority int
		exclude  string
	}{{"second", 20, ""}, {"first", 10, "*guard*"}} {
		dir := filepath.Join(root, item.id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := "id: " + item.id + "\nenabled: true\npriority: " + string(rune('0'+item.priority/10)) + "0\nmatch:\n  models: ['qwen*']\n"
		if item.exclude != "" {
			manifest += "  excludeModels: ['" + item.exclude + "']\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("export default { tools: [] };"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	matched := m.Match(Context{ResolvedModel: "qwen-coder"})
	if len(matched) != 2 || matched[0].Manifest.ID != "first" || matched[1].Manifest.ID != "second" {
		t.Fatalf("match order = %+v", matched)
	}
	matched = m.Match(Context{ResolvedModel: "qwen-guard"})
	if len(matched) != 1 || matched[0].Manifest.ID != "second" {
		t.Fatalf("exclusion = %+v", matched)
	}
}

func TestExtensions_MatchProfileProviderAndEndpoint(t *testing.T) {
	manifest := Manifest{ID: "scoped", Match: Match{Models: []string{"qwen*"}, Profiles: []string{"coding"}, Providers: []string{"vllm"}, Endpoints: []string{"anthropic.messages"}}}
	matched := Context{ResolvedModel: "qwen-27b", Profile: "coding", Provider: "vllm", Endpoint: "anthropic.messages"}
	if !manifest.Matches(matched) {
		t.Fatal("matching context was rejected")
	}
	matched.Provider = "llamacpp"
	if manifest.Matches(matched) {
		t.Fatal("provider mismatch was accepted")
	}
	matched.Provider = "vllm"
	matched.Endpoint = "responses"
	if manifest.Matches(matched) {
		t.Fatal("endpoint mismatch was accepted")
	}
}

func TestExtensions_WorkerRunsAsyncHook(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: example\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(`export default { async onRequest(ctx, request) { request.messages.push({role: "system", content: "added"}); return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Hooks) == 0 {
		t.Fatalf("no hooks in bundle: %s", item.Bundle)
	}
	input, _ := json.Marshal(workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Hook: "onRequest", Input: json.RawMessage(`{"model":"m","messages":[]}`)})
	var output bytes.Buffer
	RunWorker(bytes.NewReader(input), &output)
	var response workerResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	if !bytes.Contains(response.Output, []byte("added")) {
		t.Fatalf("output = %s", response.Output)
	}
}

func TestExtensions_WorkerNetworkPermission(t *testing.T) {
	// The real guard refuses loopback addresses, so this test runs against a
	// loopback server with the guard swapped out: what it covers is the host
	// allowlist. TestCheckPublicHost covers the guard itself.
	restore := publicHostCheck
	publicHostCheck = func(host string) error { return nil }
	defer func() { publicHostCheck = restore }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("result")) }))
	defer upstream.Close()
	address, _ := url.Parse(upstream.URL)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: network\nenabled: true\npermissions:\n  networkHosts: ["+address.Hostname()+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default { async onRequest(ctx, request) { const response = await ctx.http.fetch("` + upstream.URL + `"); request.result = await response.text(); return request; } };`
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Hook: "onRequest", Input: json.RawMessage(`{"model":"m"}`)})
	var output bytes.Buffer
	RunWorker(bytes.NewReader(input), &output)
	var response workerResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || !bytes.Contains(response.Output, []byte("result")) {
		t.Fatalf("response = %+v", response)
	}
	item.Manifest.Permissions.NetworkHosts = nil
	input, _ = json.Marshal(workerRequest{Bundle: item.Bundle, Manifest: item.Manifest, Hook: "onRequest", Input: json.RawMessage(`{"model":"m"}`)})
	output.Reset()
	RunWorker(bytes.NewReader(input), &output)
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == "" {
		t.Fatal("network access was not denied")
	}
}

func TestExtensions_WorkerSubprocessTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: timeout\nenabled: true\ntimeout: 100ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(`export default { onRequest() { for (;;) {} } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	if err == nil {
		t.Fatal("infinite loop was not stopped")
	}
}

func TestExtensions_WorkerContextCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: cancelled\nenabled: true\ntimeout: 10s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(`export default { onRequest() { for (;;) {} } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := item.Invoke(ctx, "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestExtensions_ConcurrentRequestsDoNotShareState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: isolated\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(`let count = 0; export default { onRequest(ctx, request) { request.count = ++count; return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			out, _, err := item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
			if err != nil {
				errors <- err
				return
			}
			var result map[string]any
			if err := json.Unmarshal(out, &result); err != nil {
				errors <- err
				return
			}
			if result["count"] != float64(1) {
				errors <- fmt.Errorf("count = %v", result["count"])
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestExtensions_InvalidReloadKeepsActiveVersion(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "example")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: example\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "index.js")
	if err := os.WriteFile(script, []byte("export default { tools: [] };"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := m.Compiled("example")
	if !ok {
		t.Fatal("extension was not loaded")
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if unchanged, _ := m.Compiled("example"); unchanged != before {
		t.Fatal("unchanged extension was recompiled")
	}
	if err := os.WriteFile(script, []byte("export default {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, ok := m.Compiled("example")
	if !ok || after.ETag != before.ETag {
		t.Fatal("invalid reload replaced active version")
	}
	if len(m.List()) != 1 || m.List()[0].Status != "stale" {
		t.Fatalf("status = %+v", m.List())
	}
	draft, err := m.Get("example")
	if err != nil {
		t.Fatal(err)
	}
	draft.Files["index.js"] = "export default { tools: [] };"
	if _, err := m.Save(context.Background(), draft, draft.ETag); err != nil {
		t.Fatalf("repair invalid source: %v", err)
	}
	if got := m.List()[0].Status; got != "ready" {
		t.Fatalf("repaired status=%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	draft, err = m.Get("example")
	if err != nil || draft.Manifest.ID != "example" {
		t.Fatalf("invalid manifest detail=%+v err=%v", draft, err)
	}
	draft.Manifest.Enabled = true
	if _, err := m.Save(context.Background(), draft, draft.ETag); err != nil {
		t.Fatalf("repair invalid manifest: %v", err)
	}
}

func TestExtensions_NPMInstallScriptsAreDisabled(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: npm-example\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte("export default { tools: [] };"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"extension-fixture","version":"1.0.0","scripts":{"postinstall":"exit 99"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte(`{"name":"extension-fixture","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"extension-fixture","version":"1.0.0","hasInstallScript":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := compileDirectory(context.Background(), root); err != nil {
		t.Fatal(err)
	}
}

func TestExtensions_NPMThirdPartyPackage(t *testing.T) {
	if os.Getenv("LLAMA_SWAP_TEST_NPM_NETWORK") != "1" {
		t.Skip("requires npm registry")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: package-example\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(`import isNumber from "is-number"; export default { onRequest(ctx, request) { request.valid = isNumber("42"); return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"package-example","version":"1.0.0","dependencies":{"is-number":"7.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("npm", "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate lock: %v %s", err, output)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	if err != nil || !bytes.Contains(out, []byte(`"valid":true`)) {
		t.Fatalf("package output=%s err=%v", out, err)
	}
}
