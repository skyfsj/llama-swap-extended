// Package ccswitch builds CC Switch V1 provider deep links. The builder never
// includes a key unless the caller explicitly supplies one; the WebUI resolves
// retained secrets only inside its authorized import endpoint.
package ccswitch

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

type Provider struct {
	App         string
	Name        string
	Endpoint    string
	Model       string
	HaikuModel  string
	SonnetModel string
	OpusModel   string
	APIKey      string
	IncludeKey  bool
}

func (p Provider) Validate() error {
	if p.App != "claude" && p.App != "codex" && p.App != "gemini" {
		return errors.New("ccswitch app must be claude, codex, or gemini")
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
	if err := p.Validate(); err != nil {
		return "", err
	}
	endpoint, err := normalizeEndpoint(p.Endpoint)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("resource", "provider")
	values.Set("app", p.App)
	values.Set("name", p.Name)
	values.Set("endpoint", endpoint)
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
