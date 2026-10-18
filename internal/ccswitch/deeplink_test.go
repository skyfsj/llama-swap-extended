package ccswitch

import (
	"net/url"
	"strings"
	"testing"
)

func TestBuildProviderOmitsKeyByDefault(t *testing.T) {
	link, err := BuildProvider(Provider{App: "codex", Name: "local", Endpoint: "http://127.0.0.1:8080/v1"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("apiKey") != "" || u.Query().Get("resource") != "provider" || u.Query().Get("app") != "codex" {
		t.Fatalf("link=%s", link)
	}
}

func TestBuildProviderIncludesKeyOnlyWhenRequested(t *testing.T) {
	link, err := BuildProvider(Provider{App: "claude", Name: "p", Endpoint: "https://x", APIKey: "secret", IncludeKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := url.Parse(link); got.Query().Get("apiKey") != "secret" {
		t.Fatalf("link=%s", link)
	}
}

func TestBuildProviderSupportsGeminiAndClaudeModelOverrides(t *testing.T) {
	claude, err := BuildProvider(Provider{
		App:         "claude",
		Name:        "local",
		Endpoint:    "https://x/v1",
		Model:       "main-model",
		HaikuModel:  "haiku-model",
		SonnetModel: "sonnet-model",
		OpusModel:   "opus-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	claudeURL, err := url.Parse(claude)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := claudeURL.Query().Get("haikuModel"), "haiku-model"; got != want {
		t.Fatalf("haikuModel=%q, want %q", got, want)
	}
	if got, want := claudeURL.Query().Get("sonnetModel"), "sonnet-model"; got != want {
		t.Fatalf("sonnetModel=%q, want %q", got, want)
	}
	if got, want := claudeURL.Query().Get("opusModel"), "opus-model"; got != want {
		t.Fatalf("opusModel=%q, want %q", got, want)
	}

	gemini, err := BuildProvider(Provider{App: "gemini", Name: "local", Endpoint: "https://x/v1", Model: "gemini-model"})
	if err != nil {
		t.Fatal(err)
	}
	geminiURL, err := url.Parse(gemini)
	if err != nil {
		t.Fatal(err)
	}
	if got := geminiURL.Query().Get("app"); got != "gemini" {
		t.Fatalf("gemini app=%q", got)
	}
	if got := geminiURL.Query().Get("model"); got != "gemini-model" {
		t.Fatalf("gemini model=%q", got)
	}
}

func TestBuildProviderNormalizesSingleEndpoint(t *testing.T) {
	link, err := BuildProvider(Provider{
		App:      "codex",
		Name:     "local",
		Endpoint: " https://local.example/v1 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Query().Get("endpoint"), "https://local.example/v1"; got != want {
		t.Fatalf("endpoint=%q, want %q", got, want)
	}
}

func TestBuildProviderRejectsMalformedOrMultipleEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"https://primary.example,https://backup.example",
		"https://primary.example,",
		"ftp://primary.example",
		"https://user:secret@primary.example",
		"https://primary.example/v1?api_key=secret",
		"https://primary.example/v1#fragment",
		"https://primary.example/v1 backup",
		"https://primary.example\n",
		"https://primary.example/\u200b",
	} {
		if _, err := BuildProvider(Provider{App: "claude", Name: "local", Endpoint: endpoint}); err == nil {
			t.Fatalf("endpoint list %q was accepted", endpoint)
		}
	}
}

func TestBuildProviderRejectsInvisibleProviderFields(t *testing.T) {
	for _, provider := range []Provider{
		{App: "codex", Name: "local\u200b", Endpoint: "https://x"},
		{App: "codex", Name: "local", Model: "model\u00a0name", Endpoint: "https://x"},
		{App: "codex", Name: "local", Endpoint: "https://x", IncludeKey: true, APIKey: "secret\nvalue"},
	} {
		if _, err := BuildProvider(provider); err == nil {
			t.Fatalf("provider with invisible/control field was accepted: %+v", provider)
		}
	}
	if _, err := BuildProvider(Provider{App: "codex", Name: strings.Repeat("x", 4), Endpoint: "https://x", Model: "model name"}); err != nil {
		t.Fatalf("ordinary ASCII spaces should remain valid in display/model fields: %v", err)
	}
}

func TestBuildProviderResolvesEndpointPerApp(t *testing.T) {
	// Each application appends its own request path to the configured base:
	// Claude Code posts to {base}/v1/messages and the Gemini CLI to
	// {base}/v1beta/models/..., while Codex posts to {base}/responses and the
	// AI-SDK clients to {base}/chat/completions. The historical OpenAI-style
	// /v1 endpoint is accepted as input for either shape.
	for _, testCase := range []struct {
		app      string
		endpoint string
		want     string
	}{
		{"claude", "https://local.example:9292/v1", "https://local.example:9292"},
		{"claude", "https://local.example:9292", "https://local.example:9292"},
		{"gemini", "https://local.example:9292/v1", "https://local.example:9292"},
		{"codex", "https://local.example:9292", "https://local.example:9292/v1"},
		{"codex", "https://local.example:9292/v1", "https://local.example:9292/v1"},
		{"opencode", "https://local.example:9292", "https://local.example:9292/v1"},
		{"openclaw", "https://local.example:9292/v1", "https://local.example:9292/v1"},
		{"hermes", "https://local.example:9292", "https://local.example:9292/v1"},
		{"pi", "https://local.example:9292", "https://local.example:9292/v1"},
		{"grokbuild", "https://local.example:9292", "https://local.example:9292/v1"},
	} {
		link, err := BuildProvider(Provider{App: testCase.app, Name: "local", Endpoint: testCase.endpoint, Model: "m"})
		if err != nil {
			t.Fatalf("%s: %v", testCase.app, err)
		}
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		if got := u.Query().Get("endpoint"); got != testCase.want {
			t.Fatalf("%s endpoint from %q = %q, want %q", testCase.app, testCase.endpoint, got, testCase.want)
		}
	}
}

func TestBuildProviderDeclaresSupportedApiFormats(t *testing.T) {
	// CC Switch validates apiFormat against each application's supported set;
	// declaring a format it rejects makes the whole import fail. llama-swap
	// always declares the target application's native format because it
	// converts between client surfaces itself, so a declared non-native format
	// would additionally force CC Switch's local router on.
	for _, app := range Apps {
		link, err := BuildProvider(Provider{App: app, Name: "local", Endpoint: "https://x/v1", Model: "m"})
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := u.Query().Get("apiFormat"), NativeFormat(app); got != want {
			t.Fatalf("%s default apiFormat=%q, want %q", app, got, want)
		}
		// Re-declaring the native format stays valid so callers may be explicit.
		if _, err := BuildProvider(Provider{
			App: app, ApiFormat: NativeFormat(app), Name: "local", Endpoint: "https://x/v1", Model: "m",
		}); err != nil {
			t.Fatalf("%s explicit native apiFormat: %v", app, err)
		}
	}

	for _, testCase := range []struct {
		app       string
		apiFormat string
	}{
		{"claude", "openai_chat"},
		{"codex", "anthropic"},
		{"gemini", "anthropic"},
		{"opencode", "gemini_native"},
		{"pi", "openai_responses"},
		{"claude", "bogus"},
	} {
		if _, err := BuildProvider(Provider{
			App: testCase.app, ApiFormat: testCase.apiFormat, Name: "local",
			Endpoint: "https://x/v1", Model: "m",
		}); err == nil {
			t.Fatalf("%s/%s was accepted, want an error", testCase.app, testCase.apiFormat)
		}
	}
}

func TestBuildProviderRequiresModelForPi(t *testing.T) {
	// CC Switch rejects Pi imports without a model because the imported node
	// would expose no selectable model.
	if _, err := BuildProvider(Provider{App: "pi", Name: "local", Endpoint: "https://x/v1"}); err == nil {
		t.Fatal("Pi import without a model was accepted")
	}
	if _, err := BuildProvider(Provider{App: "pi", Name: "local", Endpoint: "https://x/v1", Model: "m"}); err != nil {
		t.Fatalf("Pi import with a model: %v", err)
	}
}

func TestEndpointForAppRejectsMalformedEndpoints(t *testing.T) {
	for _, endpoint := range []string{"", "   ", "ftp://x", "https://a/v1,https://b/v1", "https://user:pass@x/y"} {
		if _, err := EndpointForApp("codex", endpoint); err == nil {
			t.Fatalf("endpoint %q was accepted", endpoint)
		}
	}
}
