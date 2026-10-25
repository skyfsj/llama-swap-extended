package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestServer_ProtocolFilesCrossProvider(t *testing.T) {
	for _, format := range protocolFixtureFormats {
		t.Run(string(format), func(t *testing.T) {
			var uploads atomic.Int64
			var native []byte
			expired := false
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/upload/v1beta/files" {
					w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/data")
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == "/upload/data" {
					body, _ := io.ReadAll(r.Body)
					if string(body) != "%PDF-content" {
						t.Errorf("upload content=%s", body)
					}
					n := uploads.Add(1)
					fmt.Fprintf(w, `{"file":{"name":"files/%d","uri":"http://%s/v1beta/files/%d","state":"ACTIVE","expirationTime":%q}}`, n, r.Host, n, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				if r.URL.Path == "/v1/files" && r.Method == "POST" {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
					}
					file, _, err := r.FormFile("file")
					if err != nil {
						t.Error(err)
						return
					}
					body, _ := io.ReadAll(file)
					file.Close()
					r.MultipartForm.RemoveAll()
					if string(body) != "%PDF-content" {
						t.Errorf("upload content=%s", body)
					}
					fmt.Fprintf(w, `{"id":"native-file-%d"}`, uploads.Add(1))
					return
				}
				if strings.Contains(r.URL.Path, "/files/") && r.Method == "GET" {
					if expired {
						w.WriteHeader(404)
					} else {
						w.Write([]byte(`{"state":"ACTIVE"}`))
					}
					return
				}
				native, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Write(protocolFixtureFinalResponse(format))
			}))
			defer backend.Close()
			cfg := config.Config{Providers: config.ProviderDictionaryConfig{"provider": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: protocolFixtureSurface(format), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			f := store.ProtocolFile{ID: "gfile_test", Owner: "anonymous", Filename: "document.pdf", MIME: "application/pdf", Content: []byte("%PDF-content")}
			if err := s.store.PutProtocolFile(t.Context(), f); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if i == 2 {
					expired = true
				}
				body := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"gfile_test"}]}]}`)
				w := postJSON(t, s, "/v1/responses", string(body))
				if w.Code != 200 {
					t.Fatalf("turn %d: %d %s", i, w.Code, w.Body.String())
				}
				if bytes.Contains(native, []byte("gfile_test")) || !bytes.Contains(native, []byte("native-file-")) && (format == dialect.OpenAIChat || format == dialect.OpenAIResponses || format == dialect.ClaudeMessages) || !bytes.Contains(native, []byte("/v1beta/files/")) && (format == dialect.Gemini || format == dialect.GeminiInteractions) {
					t.Fatalf("provider reference missing: %s", native)
				}
			}
			if uploads.Load() != 2 {
				t.Fatalf("upload count=%d", uploads.Load())
			}
		})
	}
}

func TestServer_ProtocolFilesOwnershipAndDeletion(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	defer s.store.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "document.pdf")
	part.Write([]byte("%PDF-content"))
	writer.Close()
	req := httptest.NewRequest("POST", "/v1/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var f store.ProtocolFile
	if json.Unmarshal(w.Body.Bytes(), &f) != nil || f.ID == "" {
		t.Fatal("missing file")
	}
	if _, err := s.store.GetProtocolFile(t.Context(), f.ID, "other"); err == nil {
		t.Fatal("cross-owner read")
	}
	if err := s.store.PutProtocolFileReference(t.Context(), f.ID, "other", "scope", "ref", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("cross-owner reference write")
	}
	if err := s.store.DeleteProtocolFile(t.Context(), f.ID, "other"); err == nil {
		t.Fatal("cross-owner delete")
	}
	if err := s.store.DeleteProtocolFile(t.Context(), f.ID, "anonymous"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetProtocolFile(t.Context(), f.ID, "anonymous"); err == nil {
		t.Fatal("deleted file remains")
	}
}

func TestServer_ProtocolInlineFileShapes(t *testing.T) {
	request := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,JVBERg=="}]}]}`)
	for _, format := range protocolFixtureFormats {
		converted, diags, err := dialect.ConvertRequest(dialect.OpenAIResponses, format, request, dialect.ConvertOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range diags {
			if strings.HasSuffix(d.Code, "_dropped") {
				t.Fatalf("%s lost PDF: %v", format, d)
			}
		}
		parsed, _, err := dialect.DecodeRequest(format, converted)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range parsed.Messages {
			for _, part := range m.Parts {
				if part.Base64 == base64.StdEncoding.EncodeToString([]byte("%PDF")) {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s lost inline data: %s", format, converted)
		}
		if format == dialect.OpenAIResponses && bytes.Contains(converted, []byte(`"file_url":"data:`)) {
			t.Fatalf("wrong file field: %s", converted)
		}
	}
}
