package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modeldownload"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
)

func TestServer_ModelDownloadsAPI_EnqueueCancelAndRetry(t *testing.T) {
	root := t.TempDir()
	modelFiles, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: filepath.Join(root, "models")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	queue, err := modeldownload.New(modeldownload.Config{Store: s.store, Sources: modelFiles, Settings: config.ModelDownloadsConfig{HFBaseURL: "http://127.0.0.1:1", ModelScopeBaseURL: "http://127.0.0.1:1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		queue.Close()
		s.store.Close()
	})
	s.modelFiles = modelFiles
	s.downloads = queue

	body := bytes.NewBufferString(`{"provider":"modelscope","repo_id":"acme/demo","source_id":"local","include":["*.gguf"]}`)
	request := httptest.NewRequest(http.MethodPost, "/api/model-downloads", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("enqueue status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Task struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Revision string `json:"revision"`
			Status   string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Task.ID == "" || created.Task.Provider != "modelscope" || created.Task.Revision != "master" || created.Task.Status != "queued" {
		t.Fatalf("created task = %+v", created.Task)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/model-downloads/"+created.Task.ID, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("delete active task status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/model-downloads", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(created.Task.ID)) {
		t.Fatalf("list status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/model-downloads/"+created.Task.ID+"/cancel", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"status":"canceled"`)) {
		t.Fatalf("cancel status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/model-downloads/"+created.Task.ID+"/retry", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"status":"queued"`)) {
		t.Fatalf("retry status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/api/model-downloads/"+created.Task.ID+"/cancel", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"status":"canceled"`)) {
		t.Fatalf("second cancel status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/model-downloads/"+created.Task.ID, nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete terminal task status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/model-downloads", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || bytes.Contains(recorder.Body.Bytes(), []byte(created.Task.ID)) {
		t.Fatalf("deleted task remains in list: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServer_ModelDownloadCredentialsAPI_RedactsAndPersists(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	initial := `models: {}
modelFiles:
  downloads:
    hfToken: existing-hf-token
    modelScopeToken: existing-modelscope-token
`
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.configManager = manager
	s.cfg = config.Config{ModelFiles: config.ModelFilesConfig{Downloads: config.ModelDownloadsConfig{
		HFToken:            "existing-hf-token",
		HFTokenEnv:         "TEST_HF_TOKEN",
		ModelScopeToken:    "existing-modelscope-token",
		ModelScopeTokenEnv: "TEST_MODELSCOPE_TOKEN",
	}}}

	request := httptest.NewRequest(http.MethodGet, "/api/model-download-credentials", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials GET status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "existing-hf-token") || strings.Contains(recorder.Body.String(), "existing-modelscope-token") {
		t.Fatalf("credentials GET leaked a token: %s", recorder.Body.String())
	}
	var status struct {
		ETag        string `json:"etag"`
		Writable    bool   `json:"writable"`
		HuggingFace struct {
			Configured bool `json:"configured"`
		} `json:"huggingface"`
		ModelScope struct {
			Configured bool `json:"configured"`
		} `json:"modelscope"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.ETag == "" || !status.Writable || !status.HuggingFace.Configured || !status.ModelScope.Configured {
		t.Fatalf("credentials status = %+v", status)
	}

	body := bytes.NewBufferString(`{"hfToken":"new-hf-token","modelScopeToken":""}`)
	request = httptest.NewRequest(http.MethodPatch, "/api/model-download-credentials", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", status.ETag)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials PATCH status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "new-hf-token") || strings.Contains(recorder.Body.String(), "existing-modelscope-token") {
		t.Fatalf("credentials PATCH leaked a token: %s", recorder.Body.String())
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "new-hf-token") || strings.Contains(string(updated), "existing-modelscope-token") {
		t.Fatalf("credentials PATCH did not persist the requested values: %s", updated)
	}
}

func TestServer_ModelDownloadCredentialsAPI_CreatesMissingParents(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.configManager = manager

	request := httptest.NewRequest(http.MethodGet, "/api/model-download-credentials", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials GET status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var status struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"hfToken":"created-hf-token","modelScopeToken":"created-modelscope-token"}`)
	request = httptest.NewRequest(http.MethodPatch, "/api/model-download-credentials", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", status.ETag)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials PATCH status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	updated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "created-hf-token") || !strings.Contains(string(updated), "created-modelscope-token") {
		t.Fatalf("credentials PATCH did not create missing parents: %s", updated)
	}
}

func TestServer_ModelDownloadCredentialsAPI_EmptyConfiguredTokensAreNotReported(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := `models: {}
modelFiles:
  downloads:
    hfToken: ""
    modelScopeToken: ""
`
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(configPath, "")
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.configManager = manager
	s.cfg = config.Config{ModelFiles: config.ModelFilesConfig{Downloads: config.ModelDownloadsConfig{
		HFTokenEnv:         "TEST_HF_TOKEN_EMPTY",
		ModelScopeTokenEnv: "TEST_MODELSCOPE_TOKEN_EMPTY",
	}}}

	request := httptest.NewRequest(http.MethodGet, "/api/model-download-credentials", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credentials GET status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var status struct {
		HuggingFace struct {
			Configured bool `json:"configured"`
		} `json:"huggingface"`
		ModelScope struct {
			Configured bool `json:"configured"`
		} `json:"modelscope"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.HuggingFace.Configured || status.ModelScope.Configured {
		t.Fatalf("empty configured tokens were reported as configured: %+v", status)
	}
}
