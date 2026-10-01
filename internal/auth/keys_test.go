package auth

import (
	"strings"
	"testing"
)

func TestAuth_KeyHashAndIdentity(t *testing.T) {
	key, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) < 20 || key[:3] != "sk-" {
		t.Fatalf("generated key = %q", key)
	}
	if !Verify(key, Hash(key)) || Verify(key+"x", Hash(key)) {
		t.Fatal("hash verification mismatch")
	}
	identity := Identity{Scopes: map[string]struct{}{ScopeInference: {}}, Models: []string{"qwen*"}}
	if !identity.Has(ScopeInference) || identity.Has(ScopeRuntimeAdmin) || !identity.HasModel("qwen-7b") || identity.HasModel("llama") {
		t.Fatal("identity matching mismatch")
	}
}

func TestAuth_SaltedHashUsesDifferentDigests(t *testing.T) {
	firstSalt, first, err := GenerateSalted("secret")
	if err != nil {
		t.Fatal(err)
	}
	secondSalt, second, err := GenerateSalted("secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(firstSalt) == string(secondSalt) || first == second {
		t.Fatal("salted API key digests should differ")
	}
	if !VerifySalted("secret", firstSalt, first) || VerifySalted("wrong", firstSalt, first) {
		t.Fatal("salted API key verification mismatch")
	}
}

func TestAuth_NormalizeModelsRestrictsPatterns(t *testing.T) {
	models, err := NormalizeModels([]string{" qwen* ", "qwen*", "llama"})
	if err != nil || len(models) != 2 || models[0] != "qwen*" || models[1] != "llama" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if _, err := NormalizeModels([]string{"qwen*7b"}); err == nil {
		t.Fatal("internal wildcard should be rejected")
	}
	for _, value := range []string{"qwen\u00a0", "qwen\u200b", "qwen\u202e", "qwen\n"} {
		if _, err := NormalizeModels([]string{value}); err == nil {
			t.Fatalf("unsafe model restriction accepted: %q", value)
		}
	}
}

func TestAuth_NormalizeKeyNameBoundsAndControls(t *testing.T) {
	if got, err := NormalizeKeyName("  web ui  "); err != nil || got != "web ui" {
		t.Fatalf("normalized key name = %q, err=%v", got, err)
	}
	for _, value := range []string{"line\nbreak", "tab\tname", "delete\x7f", "zero\u200bwidth", "non\u00a0breaking"} {
		if _, err := NormalizeKeyName(value); err == nil {
			t.Fatalf("control character key name accepted: %q", value)
		}
	}
	if _, err := NormalizeKeyName(strings.Repeat("x", MaxAPIKeyNameLength+1)); err == nil {
		t.Fatal("overlong key name accepted")
	}
}

func TestAuth_NormalizeScopesRejectsInvisibleWhitespace(t *testing.T) {
	if scopes, err := NormalizeScopes([]string{" inference ", "logs"}); err != nil || len(scopes) != 2 {
		t.Fatalf("ordinary scope padding was not normalized: %#v, %v", scopes, err)
	}
	for _, value := range []string{"inference\u00a0", "inference\u200b", "inference\u202e", "inference\n"} {
		if _, err := NormalizeScopes([]string{value}); err == nil {
			t.Fatalf("unsafe scope accepted: %q", value)
		}
	}
}

func TestAuth_NormalizeAccessIPs(t *testing.T) {
	got, err := NormalizeAccessIPs([]string{" 203.0.113.7 ", "10.0.0.0/8", "203.0.113.7", "::1", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("normalize access ips: %v", err)
	}
	want := []string{"10.0.0.0/8", "2001:db8::/32", "203.0.113.7", "::1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("normalized access ips = %v, want %v", got, want)
	}
	for _, value := range []string{"203.0.113.0/33", "not-an-ip", "10.0.0.0/8 with space", "203.0.113.7\n", strings.Repeat("9", MaxAccessIPTextLength+1)} {
		if _, err := NormalizeAccessIPs([]string{value}); err == nil {
			t.Fatalf("unsafe access ip accepted: %q", value)
		}
	}
	if _, err := NormalizeAccessIPs(make([]string, MaxAccessIPEntries+1)); err == nil {
		t.Fatal("overlong access ip allowlist accepted")
	}
}

func TestAuth_IPAllowed(t *testing.T) {
	cases := []struct {
		allowed   []string
		caller    string
		permitted bool
	}{
		{nil, "203.0.113.7", true},
		{[]string{"203.0.113.7"}, "203.0.113.7", true},
		{[]string{"203.0.113.7"}, "203.0.113.8", false},
		{[]string{"10.0.0.0/8"}, "10.9.8.7:44321", true},
		{[]string{"10.0.0.0/8"}, "11.0.0.1", false},
		{[]string{"::1"}, "[::1]:44321", true},
		{[]string{"fe80::/64"}, "fe80::1%eth0", true},
		{[]string{"203.0.113.7"}, "", false},
		{[]string{"203.0.113.7"}, "not-an-ip", false},
	}
	for _, testCase := range cases {
		if got := IPAllowed(testCase.allowed, testCase.caller); got != testCase.permitted {
			t.Fatalf("IPAllowed(%v, %q) = %v, want %v", testCase.allowed, testCase.caller, got, testCase.permitted)
		}
	}
}

func TestAuth_PatternsOverlap(t *testing.T) {
	cases := []struct {
		access   string
		managed  string
		overlaps bool
	}{
		{"gpt", "gpt", true},
		{"*", "gpt", true},
		{"gpt", "*", true},
		{"gpt-*", "gpt-4", true},
		{"gpt-4", "gpt-*", true},
		{"gpt-*", "gpt-4*", true},
		{"gpt-*", "claude-*", false},
		{"gpt", "claude", false},
		{"*-latest", "gpt-latest", true},
		{"*-latest", "gpt-old", false},
		{"", "gpt", false},
	}
	for _, testCase := range cases {
		if got := PatternsOverlap(testCase.access, testCase.managed); got != testCase.overlaps {
			t.Fatalf("PatternsOverlap(%q, %q) = %v, want %v", testCase.access, testCase.managed, got, testCase.overlaps)
		}
	}
}

func TestAuth_NormalizeGroupAndConcurrency(t *testing.T) {
	if got, err := NormalizeGroup("  team-a  "); err != nil || got != "team-a" {
		t.Fatalf("normalized group = %q, err=%v", got, err)
	}
	for _, value := range []string{"team\ta", "team\nb", strings.Repeat("x", MaxAccessGroupLength+1)} {
		if _, err := NormalizeGroup(value); err == nil {
			t.Fatalf("unsafe group accepted: %q", value)
		}
	}
	if got := NormalizeMaxConcurrency(0); got != 0 {
		t.Fatalf("unbounded concurrency normalized to %d", got)
	}
	if got := NormalizeMaxConcurrency(-5); got != 0 {
		t.Fatalf("negative concurrency normalized to %d", got)
	}
	if got := NormalizeMaxConcurrency(4); got != 4 {
		t.Fatalf("concurrency normalized to %d", got)
	}
	if got := NormalizeMaxConcurrency(1 << 20); got != 1024 {
		t.Fatalf("overlarge concurrency normalized to %d", got)
	}
}
