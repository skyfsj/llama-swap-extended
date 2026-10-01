package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/tidwall/gjson"
)

func TestServer_ApplyFilters(t *testing.T) {
	t.Run("useModelName rewrite", func(t *testing.T) {
		out, err := applyFilters([]byte(`{"model":"alias","temp":1}`), "alias", "real-model", config.Filters{})
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if got := gjson.GetBytes(out, "model").String(); got != "real-model" {
			t.Errorf("model = %q, want real-model", got)
		}
	})

	t.Run("strip and set params", func(t *testing.T) {
		f := config.Filters{
			StripParams: "temperature",
			SetParams:   map[string]any{"top_p": 0.9},
		}
		out, err := applyFilters([]byte(`{"model":"m","temperature":0.7}`), "m", "", f)
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if gjson.GetBytes(out, "temperature").Exists() {
			t.Error("temperature should be stripped")
		}
		if got := gjson.GetBytes(out, "top_p").Float(); got != 0.9 {
			t.Errorf("top_p = %v, want 0.9", got)
		}
	})

	t.Run("setParamsByID overrides setParams", func(t *testing.T) {
		f := config.Filters{
			SetParams:     map[string]any{"top_p": 0.5},
			SetParamsByID: map[string]map[string]any{"alias": {"top_p": 0.1}},
		}
		out, err := applyFilters([]byte(`{"model":"alias"}`), "alias", "", f)
		if err != nil {
			t.Fatalf("applyFilters: %v", err)
		}
		if got := gjson.GetBytes(out, "top_p").Float(); got != 0.1 {
			t.Errorf("top_p = %v, want 0.1", got)
		}
	})
}

func TestServer_ResolveFilters_QualifiedPeer(t *testing.T) {
	want := config.Filters{StripParams: "temperature"}
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"remote": {
			Models:  []string{"org/model"},
			Filters: want,
		},
	}}

	useModelName, got, ok := resolveFilters(cfg, "remote/org/model")
	if !ok {
		t.Fatal("qualified peer filters were not resolved")
	}
	if useModelName != "" {
		t.Fatalf("useModelName = %q, want empty for peer", useModelName)
	}
	if got.StripParams != want.StripParams {
		t.Fatalf("StripParams = %q, want %q", got.StripParams, want.StripParams)
	}
}

// TestServer_ResolveFilters_AliasRewrite is the regression test for the alias
// routing bug: an alias resolved to a model correctly, but the request body was
// forwarded with the alias intact, so the engine — which serves only its
// canonical name — answered "The model X does not exist". resolveFilters must
// now report the name the engine actually serves so the body can be rewritten.
func TestServer_ResolveFilters_AliasRewrite(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen/Qwen3.8-27B-FP8": {
				Aliases: []string{"qwen38", "Qwen/Qwen3.8-27B"},
			},
		},
	}

	cases := []struct {
		name     string
		request  string
		upstream string
	}{
		// An alias must be rewritten to the name the engine serves.
		{"alias rewrites to the canonical id", "qwen38", "Qwen/Qwen3.8-27B-FP8"},
		{"slash alias rewrites to the canonical id", "Qwen/Qwen3.8-27B", "Qwen/Qwen3.8-27B-FP8"},
		// The canonical name already names what the engine serves, so no
		// rewrite is needed and the body must not be touched.
		{"canonical name needs no rewrite", "Qwen/Qwen3.8-27B-FP8", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream, _, ok := resolveFilters(cfg, tc.request)
			if !ok {
				t.Fatalf("resolveFilters(%q) did not resolve", tc.request)
			}
			if upstream != tc.upstream {
				t.Errorf("upstream name = %q, want %q", upstream, tc.upstream)
			}
		})
	}
}

// TestServer_ResolveFilters_UseModelNameWins pins the precedence: when the
// operator configured useModelName the engine serves that name, so it wins
// over the canonical id the alias rewrite would otherwise produce.
func TestServer_ResolveFilters_UseModelNameWins(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen/Qwen3.8-27B-FP8": {
				Aliases:      []string{"qwen38"},
				UseModelName: "served-upstream-name",
			},
		},
	}
	upstream, _, ok := resolveFilters(cfg, "qwen38")
	if !ok {
		t.Fatal("resolveFilters did not resolve the alias")
	}
	if upstream != "served-upstream-name" {
		t.Errorf("upstream name = %q, want served-upstream-name", upstream)
	}
}

// TestServer_AliasRequestRewritesModelBody is the end-to-end version: a JSON
// request that names an alias must reach the next handler with the body's model
// field set to the name the engine serves. Without it the upstream rejects the
// request with a model-not-found error even though routing succeeded.
func TestServer_AliasRequestRewritesModelBody(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen/Qwen3.8-27B-FP8": {
				Aliases: []string{"qwen38"},
			},
		},
	}

	var forwarded []byte
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mw := CreateFilterMiddleware(cfg)

	for _, requested := range []string{"qwen38", "Qwen/Qwen3.8-27B-FP8"} {
		forwarded = nil
		body := `{"model":"` + requested + `","messages":[]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mw(final).ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d for %q", w.Code, requested)
		}
		if got := gjson.GetBytes(forwarded, "model").String(); got != "Qwen/Qwen3.8-27B-FP8" {
			t.Errorf("forwarded model = %q for request %q, want Qwen/Qwen3.8-27B-FP8", got, requested)
		}
	}
}

// TestServer_UpstreamAliasRequest covers the /upstream passthrough, where the
// model is named in the URL path instead of the body. Both spellings must
// reach the engine with the body's model field rewritten to the name the engine
// serves, and a request with no model field at all must still be forwarded
// rather than rejected (the path already identified the model).
func TestServer_UpstreamAliasRequest(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			"Qwen/Qwen3.8-27B-FP8": {Aliases: []string{"qwen38"}},
		},
	}

	run := func(path, body string) (int, []byte) {
		var forwarded []byte
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		})
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		CreateFilterMiddleware(cfg)(final).ServeHTTP(w, r)
		return w.Code, forwarded
	}

	t.Run("alias in the path and body", func(t *testing.T) {
		code, body := run("/upstream/qwen38/v1/chat/completions", `{"model":"qwen38"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if got := gjson.GetBytes(body, "model").String(); got != "Qwen/Qwen3.8-27B-FP8" {
			t.Errorf("forwarded model = %q, want Qwen/Qwen3.8-27B-FP8", got)
		}
	})

	t.Run("canonical name in the path", func(t *testing.T) {
		code, body := run("/upstream/Qwen%2FQwen3.8-27B-FP8/v1/chat/completions", `{"model":"Qwen/Qwen3.8-27B-FP8"}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if got := gjson.GetBytes(body, "model").String(); got != "Qwen/Qwen3.8-27B-FP8" {
			t.Errorf("forwarded model = %q, want it unchanged", got)
		}
	})

	t.Run("no model field in the body is forwarded", func(t *testing.T) {
		code, body := run("/upstream/qwen38/v1/chat/completions", `{"messages":[]}`)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want the passthrough to accept it", code)
		}
		if gjson.GetBytes(body, "model").Exists() {
			t.Errorf("forwarded body = %q, want no model field invented", body)
		}
	})
}

// TestServer_PeerRequestKeepsModelName proves the rewrite stays local to
// models: a peer decides its own naming, so the request must be forwarded
// exactly as the client sent it.
func TestServer_PeerRequestKeepsModelName(t *testing.T) {
	cfg := config.Config{
		Peers: config.PeerDictionaryConfig{
			"remote": {Models: []string{"org/model"}},
		},
	}
	upstream, _, ok := resolveFilters(cfg, "remote/org/model")
	if !ok {
		t.Fatal("peer did not resolve")
	}
	if upstream != "" {
		t.Errorf("upstream name = %q, want empty so the peer's own name is kept", upstream)
	}
}

func TestServer_FormFilterMiddleware(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"whisper": {UseModelName: "whisper-large-v3"},
	}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "whisper")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write([]byte("xx"))
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())

	var gotModel, gotFilename, gotFileBody string
	var gotContext swaputil.ReqContextData
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(swaputil.MaxMultiPartSize); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		gotModel = r.MultipartForm.Value["model"][0]
		fileHeader := r.MultipartForm.File["file"][0]
		gotFilename = fileHeader.Filename
		file, err := fileHeader.Open()
		if err != nil {
			t.Errorf("open file: %v", err)
			return
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Errorf("read file: %v", err)
			return
		}
		gotFileBody = string(data)
		gotContext, _ = swaputil.ReadContext(r.Context())
	})
	CreateFormFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if gotModel != "whisper-large-v3" {
		t.Errorf("model rewritten to %q, want whisper-large-v3", gotModel)
	}
	if gotFilename != "a.wav" {
		t.Errorf("filename = %q, want a.wav", gotFilename)
	}
	if gotFileBody != "xx" {
		t.Errorf("file body = %q, want xx", gotFileBody)
	}
	if gotContext.Model != "whisper" || gotContext.ModelID != "whisper" {
		t.Errorf("request context = %+v, want original whisper model", gotContext)
	}
}

// TestServer_FormFilterMiddleware_Alias covers the same rewrite for a form
// field: an alias the engine does not serve must reach it as the canonical
// name, exactly as the JSON path does.
func TestServer_FormFilterMiddleware_Alias(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"Qwen/Qwen3.8-27B-FP8": {Aliases: []string{"qwen38"}},
	}}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", "qwen38")
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())

	var gotModel string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(swaputil.MaxMultiPartSize); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		gotModel = r.MultipartForm.Value["model"][0]
	})
	CreateFormFilterMiddleware(cfg)(final).ServeHTTP(httptest.NewRecorder(), r)

	if gotModel != "Qwen/Qwen3.8-27B-FP8" {
		t.Errorf("model rewritten to %q, want Qwen/Qwen3.8-27B-FP8", gotModel)
	}
}
