package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_HandleAPIConfigYAMLEditor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.configManager = manager

	req := func(method, endpoint string, body []byte, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, endpoint, bytes.NewReader(body))
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		response := httptest.NewRecorder()
		if endpoint == "/api/config/yaml/validate" {
			s.handleAPIConfigYAMLValidate(response, r)
		} else {
			s.handleAPIConfigYAML(response, r)
		}
		return response
	}

	validBody, _ := json.Marshal(map[string]string{"yaml": "startPort: [broken\n"})
	response := req(http.MethodPost, "/api/config/yaml/validate", validBody, "")
	if response.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", response.Code, response.Body.String())
	}
	var validation config.YAMLValidation
	if err := json.Unmarshal(response.Body.Bytes(), &validation); err != nil {
		t.Fatal(err)
	}
	if validation.Valid || len(validation.Issues) == 0 {
		t.Fatalf("invalid YAML unexpectedly accepted: %+v", validation)
	}

	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	applyBody, _ := json.Marshal(map[string]string{"yaml": "startPort: 5900\nmodels: {}\n"})
	response = req(http.MethodPut, "/api/config/yaml", applyBody, snapshot.ETag)
	if response.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "startPort: 5900") {
		t.Fatalf("YAML endpoint did not write config: %s", updated)
	}
}

func TestServer_HandleAPIConfigWithoutManagerRedactsResolvedSecrets(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{
		RequiredAPIKeys: []string{"legacy-secret"},
		Models: map[string]config.ModelConfig{
			"local": {
				Env: []string{"OPENAI_API_KEY=resolved-secret", "MODEL_ROOT=/models"},
				Metadata: map[string]any{
					"api_key": "metadata-secret",
					"public":  "visible",
				},
				Macros: config.MacroList{
					{Name: "API_KEY", Value: "macro-secret"},
					{Name: "MODEL_ROOT", Value: "/models"},
				},
			},
		},
		Peers: config.PeerDictionaryConfig{
			"remote": {
				Proxy:  "http://127.0.0.1:9000",
				ApiKey: "peer-secret",
				Models: []string{"remote-model"},
			},
		},
	}

	response := httptest.NewRecorder()
	s.handleAPIConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{"legacy-secret", "resolved-secret", "metadata-secret", "macro-secret", "peer-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked secret %q: %s", secret, body)
		}
	}
	var payload struct {
		Config    map[string]any    `json:"config"`
		YAML      string            `json:"yaml"`
		Writable  bool              `json:"writable"`
		Restart   bool              `json:"restartRequired"`
		Ownership map[string]string `json:"ownership"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Writable {
		t.Fatal("embedded config snapshot unexpectedly writable")
	}
	if payload.Restart {
		t.Fatal("read-only snapshot unexpectedly requires restart")
	}
	if payload.YAML == "" {
		t.Fatal("embedded config snapshot did not include YAML")
	}
	if payload.Config["Models"] == nil {
		t.Fatal("embedded config snapshot omitted effective models")
	}
	if !strings.Contains(payload.YAML, "[REDACTED]") {
		t.Fatalf("redacted YAML does not show a redaction marker: %s", payload.YAML)
	}
}

func TestServer_DeleteModelConfigOptionallyDeletesAssociatedFiles(t *testing.T) {
	t.Run("deletes config and exclusive model file", func(t *testing.T) {
		root := t.TempDir()
		modelPath := filepath.Join(root, "model.gguf")
		if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(root, "config.yaml")
		if err := os.WriteFile(configPath, []byte("models:\n  model/one:\n    cmd: llama-server --model "+modelPath+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, err := config.NewConfigManager(configPath, "")
		if err != nil {
			t.Fatal(err)
		}
		modelFiles, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
			"local": {Type: config.ModelFileSourceDirectory, Path: root},
		}})
		if err != nil {
			t.Fatal(err)
		}
		s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
		t.Cleanup(func() { _ = s.store.Close() })
		s.cfg = config.Config{Models: map[string]config.ModelConfig{
			"model/one": {Cmd: "llama-server --model " + modelPath},
		}}
		s.configManager = manager
		s.modelFiles = modelFiles
		snapshot, err := manager.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		body := bytes.NewBufferString(`{"delete_model_file":true}`)
		request := httptest.NewRequest(http.MethodDelete, "/api/config/models/model%2Fone", body)
		request.SetPathValue("model", "model/one")
		request.Header.Set("If-Match", snapshot.ETag)
		response := httptest.NewRecorder()
		s.handleAPIDeleteModelConfig(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var result deleteModelConfigResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Deleted != "model/one" || len(result.DeletedModelFiles) != 1 || result.DeletedModelFiles[0] != modelPath {
			t.Fatalf("delete result=%+v", result)
		}
		if _, err := os.Stat(modelPath); !os.IsNotExist(err) {
			t.Fatalf("model file still exists, stat error=%v", err)
		}
		updated, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(updated), "model/one") {
			t.Fatalf("model configuration still exists: %s", updated)
		}
	})

	t.Run("keeps file when checkbox is not selected", func(t *testing.T) {
		root := t.TempDir()
		modelPath := filepath.Join(root, "model.gguf")
		if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(root, "config.yaml")
		if err := os.WriteFile(configPath, []byte("models:\n  model:\n    cmd: llama-server --model "+modelPath+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, err := config.NewConfigManager(configPath, "")
		if err != nil {
			t.Fatal(err)
		}
		modelFiles, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
			"local": {Type: config.ModelFileSourceDirectory, Path: root},
		}})
		if err != nil {
			t.Fatal(err)
		}
		s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
		t.Cleanup(func() { _ = s.store.Close() })
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"model": {Cmd: "llama-server --model " + modelPath}}}
		s.configManager = manager
		s.modelFiles = modelFiles
		snapshot, err := manager.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		request := httptest.NewRequest(http.MethodDelete, "/api/config/models/model", bytes.NewBufferString(`{"delete_model_file":false}`))
		request.SetPathValue("model", "model")
		request.Header.Set("If-Match", snapshot.ETag)
		response := httptest.NewRecorder()
		s.handleAPIDeleteModelConfig(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if _, err := os.Stat(modelPath); err != nil {
			t.Fatalf("model file was removed despite unchecked option: %v", err)
		}
	})
}

func TestServer_DeleteModelConfigRejectsSharedAssociatedFile(t *testing.T) {
	root := t.TempDir()
	modelPath := filepath.Join(root, "shared.gguf")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("models:\n  first:\n    cmd: llama-server --model "+modelPath+"\n  second:\n    cmd: llama-server --model "+modelPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	modelFiles, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"first":  {Cmd: "llama-server --model " + modelPath},
		"second": {Cmd: "llama-server --model " + modelPath},
	}}
	s.configManager = manager
	s.modelFiles = modelFiles
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/config/models/first", bytes.NewBufferString(`{"delete_model_file":true}`))
	request.SetPathValue("model", "first")
	request.Header.Set("If-Match", snapshot.ETag)
	response := httptest.NewRecorder()
	s.handleAPIDeleteModelConfig(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var conflict struct {
		Registered []string `json:"registered"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if len(conflict.Registered) != 1 || conflict.Registered[0] != "second" {
		t.Fatalf("conflict=%+v", conflict)
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Fatalf("shared model file was removed: %v", err)
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "first:") {
		t.Fatalf("model configuration was removed despite conflict: %s", current)
	}
}

func TestServer_APIModelRestartStatusCodes(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	var restartErr error
	s.configReconciler = NewConfigReconciler(s.cfg, nil, func(string) error { return restartErr })

	request := func(model string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/models/restart/"+model, nil)
		r.SetPathValue("model", model)
		response := httptest.NewRecorder()
		s.handleAPIModelRestart(response, r)
		return response
	}
	forceRequest := func(model string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/models/force-restart/"+model, nil)
		r.SetPathValue("model", model)
		response := httptest.NewRecorder()
		s.handleAPIModelForceRestart(response, r)
		return response
	}

	if response := request("missing"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d want 404", response.Code)
	}
	if response := request("m"); response.Code != http.StatusAccepted {
		t.Fatalf("accepted restart status=%d want 202: %s", response.Code, response.Body.String())
	}
	if response := forceRequest("missing"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown force restart status=%d want 404", response.Code)
	}
	restartErr = nil
	if response := forceRequest("m"); response.Code != http.StatusAccepted {
		t.Fatalf("accepted force restart status=%d want 202: %s", response.Code, response.Body.String())
	}
	restartErr = scheduler.ErrRestartNotPending
	if response := request("m"); response.Code != http.StatusConflict {
		t.Fatalf("no-pending restart status=%d want 409", response.Code)
	}
	if response := forceRequest("m"); response.Code != http.StatusConflict {
		t.Fatalf("no-pending force restart status=%d want 409", response.Code)
	}
}

func TestServer_APIModelForceRestartCancelsTrackedRequests(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	s.configReconciler = NewConfigReconciler(s.cfg, nil, func(string) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(
		swaputil.SetContext(ctx, swaputil.ReqContextData{ModelID: "m"}),
	)
	id := s.inflight.Add(request, cancel)
	defer s.inflight.Remove(id)

	r := httptest.NewRequest(http.MethodPost, "/api/models/force-restart/m", nil)
	r.SetPathValue("model", "m")
	response := httptest.NewRecorder()
	s.handleAPIModelForceRestart(response, r)
	if response.Code != http.StatusAccepted {
		t.Fatalf("force restart status=%d body=%q, want 202", response.Code, response.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("force restart did not cancel the model request")
	}
	if got := s.inflight.Current().Requests; len(got) != 0 {
		t.Fatalf("force restart left %d tracked requests, want none", len(got))
	}
}

func TestRedactConfigJSONPreservesEnvironmentNames(t *testing.T) {
	value := map[string]any{
		"env": []any{"MODEL_ROOT=/models", "TOKEN=resolved-token", "INHERITED", "1", "2,3,4"},
	}
	redactConfigJSON(value, "")
	entries := value["env"].([]any)
	if got := entries[0]; got != "MODEL_ROOT=/models" {
		t.Fatalf("non-sensitive environment value was unexpectedly redacted: %v", got)
	}
	if got := entries[1]; got != "TOKEN=[REDACTED]" {
		t.Fatalf("sensitive environment value was not redacted: %v", got)
	}
	if got := entries[2]; got != "[REDACTED]" {
		t.Fatalf("bare environment entry was not redacted: %v", got)
	}
	if got := entries[3]; got != "1" {
		t.Fatalf("numeric environment entry was unexpectedly redacted: %v", got)
	}
	if got := entries[4]; got != "2,3,4" {
		t.Fatalf("numeric environment list was unexpectedly redacted: %v", got)
	}
}
