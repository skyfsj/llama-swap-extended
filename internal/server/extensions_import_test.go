package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

const importSource = "export default { onRequest(ctx, request) { return request; } };\n"

func importTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "fixture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: fixture\nenabled: true\npriority: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(importSource), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.cfg.UI.Activity.SessionID = []string{"X-Session-ID"}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

// uploadRequest builds a multipart zip POST and returns the request.
func uploadRequest(t *testing.T, path string, archive []byte, extra ...func(*http.Request)) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "extension.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	for _, apply := range extra {
		apply(request)
	}
	return request
}

func TestServer_ExtensionImportExportRoundTrip(t *testing.T) {
	s := importTestServer(t)

	archive := exportArchive(t, s, "fixture")

	previewRecorder := httptest.NewRecorder()
	s.ServeHTTP(previewRecorder, uploadRequest(t, "/api/extensions/import/preview", archive))
	if previewRecorder.Code != http.StatusOK {
		t.Fatalf("preview status = %d body %s", previewRecorder.Code, previewRecorder.Body.String())
	}
	var parsed struct {
		Manifest struct {
			ID string `json:"id"`
		} `json:"manifest"`
		Files    []string `json:"files"`
		Conflict bool     `json:"conflict"`
	}
	if err := json.Unmarshal(previewRecorder.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	// The archive carries "fixture", which already exists, so preview reports
	// a conflict and the import below renames to a fresh id.
	if parsed.Manifest.ID != "fixture" || !parsed.Conflict {
		t.Fatalf("preview = %+v", parsed)
	}
	if len(parsed.Files) != 1 || parsed.Files[0] != "index.js" {
		t.Fatalf("preview files = %v", parsed.Files)
	}

	importRecorder := httptest.NewRecorder()
	s.ServeHTTP(importRecorder, uploadRequest(t, "/api/extensions/import?id=fresh", archive))
	if importRecorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d body %s", importRecorder.Code, importRecorder.Body.String())
	}
	var saved extensions.Definition
	if err := json.Unmarshal(importRecorder.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Manifest.ID != "fresh" || saved.Manifest.Enabled {
		t.Fatalf("imported manifest = %+v", saved.Manifest)
	}
	if !strings.Contains(saved.Files["index.js"], "onRequest") {
		t.Fatalf("imported source = %q", saved.Files["index.js"])
	}
}

func TestServer_ExtensionImportReplaceUsesEtag(t *testing.T) {
	s := importTestServer(t)
	archive := exportArchive(t, s, "fixture")
	before, err := s.currentExtensions().Get("fixture")
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, uploadRequest(t, "/api/extensions/import?mode=replace", archive))
	if recorder.Code != http.StatusPreconditionRequired {
		t.Fatalf("replace without etag status = %d body %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, uploadRequest(t, "/api/extensions/import?mode=replace", archive, func(request *http.Request) {
		request.Header.Set("If-Match", "stale-etag")
	}))
	if recorder.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale etag status = %d body %s", recorder.Code, recorder.Body.String())
	}
	after, err := s.currentExtensions().Get("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag != before.ETag {
		t.Fatal("a conflicting replace modified the extension")
	}
}

func TestServer_ExtensionImportRejectsBadArchive(t *testing.T) {
	s := importTestServer(t)

	// A traversal entry built by hand: manifest.yaml and a ../evil.js.
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range map[string]string{"manifest.yaml": "id: bad\nenabled: false\n", "index.js": importSource, "../evil.js": "x"} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, uploadRequest(t, "/api/extensions/import", buffer.Bytes()))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "archive path is not allowed") {
		t.Fatalf("bad archive: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/extensions/import/preview", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("no upload status = %d", recorder.Code)
	}
}

func exportArchive(t *testing.T, s *Server, id string) []byte {
	t.Helper()
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/extensions/export/"+id, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("export status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Header().Get("Content-Disposition"), id+".zip") {
		t.Fatalf("disposition = %q", recorder.Header().Get("Content-Disposition"))
	}
	return recorder.Body.Bytes()
}
