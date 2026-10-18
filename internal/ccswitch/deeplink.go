// Package ccswitch builds CC Switch V1 provider deep links. The builder never
// includes a key unless the caller explicitly supplies one; the WebUI resolves
// retained secrets only inside its authorized import endpoint.
package ccswitch

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// Apps lists the applications CC Switch v4 imports provider links into, in the
// order the WebUI offers them. The names mirror cc-switch's
// src/deeplink/parser.rs; mcode is excluded because CC Switch only accepts
// MCode providers from its own page.
var Apps = []string{"claude", "codex", "gemini", "grokbuild", "opencode", "openclaw", "hermes", "pi"}

// Provider describes one provider import.
type Provider struct {
	App string
	// ApiFormat declares the wire format of the imported endpoint:
	// anthropic | openai_chat | openai_responses | gemini_native. CC Switch has
	// no field for a mixed-surface gateway, so one link carries one format;
	// llama-swap converts whichever surface the application speaks.
	ApiFormat   string
	Name        string
	Endpoint    string
	Model       string
	HaikuModel  string
	SonnetModel string
	OpusModel   string
	APIKey      string
	IncludeKey  bool
}

// IsApp reports whether CC Switch accepts provider imports for this app name.
func IsApp(app string) bool {
	for _, candidate := range Apps {
		if app == candidate {
			return true
		}
	}
	return false
}

// NativeFormat is the format an application speaks without CC Switch routing,
// and the format a link defaults to when it declares none. The values mirror
// CC Switch's ApiFormat::default_for.
func NativeFormat(app string) string {
	switch app {
	case "claude":
		return "anthropic"
	case "codex", "grokbuild":
		return "openai_responses"
	case "gemini":
		return "gemini_native"
	default:
		return "openai_chat"
	}
}

// EndpointForApp resolves the base URL the imported application is configured
// with. Each application appends its own request path to that base:
//
//   - Claude Code posts to {base}/v1/messages and the Gemini CLI to
//     {base}/v1beta/models/<model>:generateContent, so their base carries no
//     version segment.
//   - Codex and Grok Build post to {base}/responses and the AI-SDK based
//     clients (OpenCode, OpenClaw, Hermes, Pi) to {base}/chat/completions, so
//     their base is the OpenAI-style base ending in /v1.
//
// The conversion is idempotent: callers may pass either the bare origin or the
// historical OpenAI-style endpoint.
func EndpointForApp(app, raw string) (string, error) {
	endpoint, err := normalizeEndpoint(raw)
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(strings.TrimSuffix(endpoint, "/"), "/v1")
	switch app {
	case "claude", "gemini":
		return base, nil
	default:
		return base + "/v1", nil
	}
}
func (p Provider) Validate() error {
	if !IsApp(p.App) {
		return fmt.Errorf("ccswitch app must be one of: %s", strings.Join(Apps, ", "))
	}
	if p.ApiFormat != "" && p.ApiFormat != NativeFormat(p.App) {
		// CC Switch supports declaring a non-native format and converting inside
		// its own local router, but that changes the meaning of the endpoint and
		// forces the operator to enable routing. llama-swap converts between all
		// four client surfaces itself, so a link always declares the format the
		// target application speaks natively.
		return fmt.Errorf("ccswitch apiFormat must be %q for app %q; llama-swap converts client surfaces itself", NativeFormat(p.App), p.App)
	}
	if err := validateProviderText(p.Name, "ccswitch provider name", true); err != nil {
		return err
	}
	if _, err := normalizeEndpoint(p.Endpoint); err != nil {
		return err
	}
	if p.Model != "" {
		if err := validateProviderText(p.Model, "ccswitch provider model", false); err != nil {
			return err
		}
	}
	if p.App == "pi" && strings.TrimSpace(p.Model) == "" {
		// CC Switch rejects Pi imports without a model: the imported node would
		// expose no model at all.
		return errors.New("ccswitch Pi imports require a model")
	}
	for field, value := range map[string]string{
		"ccswitch Haiku model":  p.HaikuModel,
		"ccswitch Sonnet model": p.SonnetModel,
		"ccswitch Opus model":   p.OpusModel,
	} {
		if value != "" {
			if err := validateProviderText(value, field, false); err != nil {
				return err
			}
		}
	}
	if p.IncludeKey {
		if strings.TrimSpace(p.APIKey) == "" {
			return errors.New("ccswitch API key is required when includeKey is true")
		}
		// A key is copied into a URI query value when explicitly requested. URL
		// escaping prevents delimiter injection, but rejecting invisible/control
		// runes keeps the one-time secret legible and avoids confusing imports.
		if err := validateProviderText(p.APIKey, "ccswitch API key", false); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderText(value, field string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return errors.New(field + " is required")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
			return errors.New(field + " contains invalid whitespace or control characters")
		}
	}
	return nil
}

func BuildProvider(p Provider) (string, error) {
	if p.ApiFormat == "" {
		p.ApiFormat = NativeFormat(p.App)
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	endpoint, err := EndpointForApp(p.App, p.Endpoint)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("resource", "provider")
	values.Set("app", p.App)
	values.Set("name", p.Name)
	values.Set("endpoint", endpoint)
	values.Set("apiFormat", p.ApiFormat)
	if p.Model != "" {
		values.Set("model", p.Model)
	}
	if p.App == "claude" {
		for name, value := range map[string]string{
			"haikuModel":  p.HaikuModel,
			"sonnetModel": p.SonnetModel,
			"opusModel":   p.OpusModel,
		} {
			if value != "" {
				values.Set(name, value)
			}
		}
	}
	if p.IncludeKey {
		values.Set("apiKey", p.APIKey)
	}
	return "ccswitch://v1/import?" + values.Encode(), nil
}

// normalizeEndpoint validates the single local inference endpoint used by a
// llama-swap provider deep link. The manager owns routing and failover; a deep
// link must not smuggle a second upstream endpoint into the imported provider.
// The normalized value avoids importing accidental surrounding whitespace.
func normalizeEndpoint(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("ccswitch provider endpoint is required")
	}
	if strings.ContainsAny(raw, ",") {
		return "", errors.New("ccswitch provider endpoint must be a single URL")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("ccswitch provider endpoint contains invalid control characters")
	}
	endpoint := strings.TrimSpace(raw)
	for _, r := range endpoint {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return "", errors.New("ccswitch provider endpoint contains invalid whitespace or control characters")
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("ccswitch provider endpoint must be an HTTP(S) URL without credentials")
	}
	return u.String(), nil
}

func BuildPair(endpoint, name, model string) (codex, claude string, err error) {
	codex, err = BuildProvider(Provider{App: "codex", Name: name, Endpoint: endpoint, Model: model})
	if err != nil {
		return "", "", err
	}
	claude, err = BuildProvider(Provider{App: "claude", Name: name, Endpoint: endpoint, Model: model})
	return codex, claude, err
}
