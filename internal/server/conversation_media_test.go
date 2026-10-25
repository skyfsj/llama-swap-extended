package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestServer_ConversationOwnedMediaReferences(t *testing.T) {
	for _, format := range []dialect.Format{dialect.OpenAIChat, dialect.ClaudeMessages, dialect.Gemini} {
		t.Run(string(format), func(t *testing.T) {
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files" {
					w.Write([]byte(`{"id":"file-owned"}`))
					return
				}
				if r.Method == "GET" && strings.Contains(r.URL.Path, "/files/") {
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == "/upload/v1beta/files" {
					w.Header().Set("X-Goog-Upload-URL", "http://"+r.Host+"/upload/data")
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == "/upload/data" {
					fmt.Fprintf(w, `{"file":{"uri":"http://%s/v1beta/files/owned","state":"ACTIVE"}}`, r.Host)
					return
				}
				calls++
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte("gfile_")) {
					t.Errorf("gateway reference reached provider: %s", body)
				}
				if calls == 2 && !strings.Contains(string(body), "file-owned") && format != dialect.Gemini {
					t.Errorf("history file missing: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(protocolFixtureFinalResponse(format))
			}))
			defer backend.Close()
			cfg := config.Config{Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: protocolFixtureSurface(format), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			content := append([]byte("%PDF-"), bytes.Repeat([]byte{'x'}, 1<<20)...)
			if err := s.store.PutProtocolFile(t.Context(), store.ProtocolFile{ID: "gfile_owned", Owner: "anonymous", Filename: "owned.pdf", MIME: "application/pdf", Content: content}); err != nil {
				t.Fatal(err)
			}
			created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
			id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
			for _, body := range [][]byte{[]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"gfile_owned"}]}]}`), []byte(`{"model":"m","input":"NEXT"}`)} {
				w := postConversationTurn(t, s, id, dialect.OpenAIResponses, body)
				if w.Code != 200 {
					t.Fatalf("turn=%d %s", w.Code, w.Body.String())
				}
			}
			record, err := s.store.GetConversation(t.Context(), id, "anonymous")
			if err != nil {
				t.Fatal(err)
			}
			if len(record.State) > 64<<10 || !bytes.Contains(record.State, []byte("gfile_owned")) || bytes.Contains(record.State, []byte(base64.StdEncoding.EncodeToString(content[:64]))) {
				t.Fatalf("owned bytes duplicated in checkpoint: %d bytes", len(record.State))
			}
		})
	}
}
