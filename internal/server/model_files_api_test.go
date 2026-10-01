package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_ModelFiles_ListAndDeleteGuards(t *testing.T) {
	root := t.TempDir()
	registeredPath := filepath.Join(root, "registered.gguf")
	inUsePath := filepath.Join(root, "in-use.gguf")
	safePath := filepath.Join(root, "safe.gguf")
	for _, path := range []string{registeredPath, inUsePath, safePath} {
		if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	local := newStubRouter(nil, "")
	local.running = map[string]process.ProcessState{"in-use": process.StateReady}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"registered": {Cmd: "llama-server --model " + registeredPath},
		"in-use":     {Cmd: "llama-server --model " + inUsePath},
	}}
	manager, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.modelFiles = manager

	request := httptest.NewRequest(http.MethodGet, "/api/model-files?source=local", nil)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var catalog modelmanager.Catalog
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 3 {
		t.Fatalf("catalog total = %d, want 3", catalog.Total)
	}
	byPath := make(map[string]modelmanager.File, len(catalog.Data))
	for _, file := range catalog.Data {
		byPath[file.Path] = file
	}
	if got := byPath[registeredPath].Registered; len(got) != 1 || got[0] != "registered" {
		t.Fatalf("registered models = %v", got)
	}
	if got := byPath[inUsePath].InUse; len(got) != 1 || got[0] != "in-use" {
		t.Fatalf("in-use models = %v", got)
	}

	deleteRequest := func(file modelmanager.File) *http.Request {
		body, err := json.Marshal(map[string]any{
			"source_id": file.SourceID,
			"path":      file.Path,
			"confirm":   true,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodDelete, "/api/model-files/"+file.ID, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		return request
	}

	for _, test := range []struct {
		name string
		file modelmanager.File
		want []string
	}{
		{name: "registered", file: byPath[registeredPath], want: []string{"registered"}},
		{name: "in use", file: byPath[inUsePath], want: []string{"in-use"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			s.ServeHTTP(response, deleteRequest(test.file))
			if response.Code != http.StatusConflict {
				t.Fatalf("delete status = %d, body = %s", response.Code, response.Body.String())
			}
			var conflict struct {
				Registered []string `json:"registered"`
				InUse      []string `json:"in_use"`
			}
			if err := json.NewDecoder(response.Body).Decode(&conflict); err != nil {
				t.Fatal(err)
			}
			if test.name == "registered" && len(conflict.Registered) != 1 {
				t.Fatalf("registered conflict = %+v", conflict)
			}
			if test.name == "in use" && len(conflict.InUse) != 1 {
				t.Fatalf("in-use conflict = %+v", conflict)
			}
			if _, err := os.Stat(test.file.Path); err != nil {
				t.Fatalf("blocked file stat = %v", err)
			}
		})
	}

	response = httptest.NewRecorder()
	s.ServeHTTP(response, deleteRequest(byPath[safePath]))
	if response.Code != http.StatusOK {
		t.Fatalf("safe delete status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(safePath); !os.IsNotExist(err) {
		t.Fatalf("safe file still exists, stat error = %v", err)
	}
}

func TestServer_ModelFiles_DeleteRequiresConfirmation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model.gguf")
	if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.List(t.Context(), modelmanager.ListOptions{SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.modelFiles = manager
	body, err := json.Marshal(map[string]any{
		"source_id": "local",
		"path":      path,
		"confirm":   false,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/model-files/"+catalog.Data[0].ID, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unconfirmed file stat = %v", err)
	}
}

func TestServer_ModelFiles_DeleteRejectsOversizedJSONBody(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	padding := bytes.Repeat([]byte{'x'}, maxAPIKeyJSONBody+1)
	body := append([]byte(`{"source_id":"local","path":"`), append(padding, []byte(`","confirm":true}`)...)...)
	request := httptest.NewRequest(http.MethodDelete, "/api/model-files/unknown", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400", response.Code, response.Body.String())
	}
}

func TestServer_ModelFiles_DeleteEnforcesModelRestriction(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "denied.gguf")
	if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.List(context.Background(), modelmanager.ListOptions{SourceID: "local"})
	if err != nil || len(catalog.Data) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"denied": {Cmd: "llama-server --model " + path},
	}}
	s.modelFiles = manager
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "file-scoped", KeyHash: store.KeyFingerprint("file-secret"),
		Scopes: []string{auth.ScopeConfigAdmin}, Models: []string{"other"},
	}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"source_id": catalog.Data[0].SourceID,
		"path":      catalog.Data[0].Path,
		"confirm":   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/model-files/"+catalog.Data[0].ID, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer file-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q, want 403", response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("denied file was removed: %v", err)
	}
}

func TestServer_ModelFiles_ListEnforcesModelRestriction(t *testing.T) {
	root := t.TempDir()
	paths := map[string]string{}
	for _, name := range []string{"allowed.gguf", "denied.gguf", "shared.gguf", "free.gguf"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	manager, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"allowed":  {Cmd: "llama-server --model " + paths["allowed.gguf"]},
		"denied":   {Cmd: "llama-server --model " + paths["denied.gguf"]},
		"shared-a": {Cmd: "llama-server --model " + paths["shared.gguf"]},
		"shared-b": {Cmd: "llama-server --model " + paths["shared.gguf"]},
	}}
	s.modelFiles = manager
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "files-read", KeyHash: store.KeyFingerprint("files-secret"),
		Scopes: []string{auth.ScopeRuntimeRead}, Models: []string{"allowed", "shared-a"},
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/model-files?source=local&limit=2&offset=1", nil)
	request.Header.Set("Authorization", "Bearer files-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var catalog modelmanager.Catalog
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 3 || catalog.Limit != 2 || catalog.Offset != 1 || len(catalog.Data) != 2 {
		t.Fatalf("scoped catalog pagination = %+v, want total=3 limit=2 offset=1 data=2", catalog)
	}
	for _, file := range catalog.Data {
		if file.Path == paths["denied.gguf"] {
			t.Fatalf("denied file leaked into catalog: %+v", file)
		}
		for _, model := range append(append([]string{}, file.Registered...), file.InUse...) {
			if model == "denied" || model == "shared-b" {
				t.Fatalf("denied model leaked in file metadata: %+v", file)
			}
		}
	}
	for _, source := range catalog.Sources {
		if source.ID == "local" && source.FileCount != 3 {
			t.Fatalf("visible source file count=%d, want 3", source.FileCount)
		}
	}
}
