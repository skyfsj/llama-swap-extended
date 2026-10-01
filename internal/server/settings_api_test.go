package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestSettingsAPIUnifiedPreviewAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{}}
	s.SetConfigManager(manager)
	s.routes()
	request := httptest.NewRequest(http.MethodGet, "/api/settings/config", nil)
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot struct {
		ETag string `json:"etag"`
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.ETag == "" || snapshot.YAML == "" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	previewBody := `{"mode":"structured","changes":[{"op":"replace","path":"/startPort","value":5900}]}`
	previewRequest := httptest.NewRequest(http.MethodPost, "/api/settings/config/preview", strings.NewReader(previewBody))
	previewRequest.Header.Set("Content-Type", "application/json")
	previewResponse := httptest.NewRecorder()
	s.mux.ServeHTTP(previewResponse, previewRequest)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || !preview.Valid {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
}
