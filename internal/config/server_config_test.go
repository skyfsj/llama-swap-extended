package config

import (
	"strings"
	"testing"
	"time"
)

func TestNetworkConfig_TrustedProxiesRequiresTrustForwardedFor(t *testing.T) {
	cfg := NetworkConfig{TrustedProxies: []string{"127.0.0.1/32"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("trustedProxies without trustForwardedFor was accepted")
	} else if !strings.Contains(err.Error(), "trustForwardedFor") {
		t.Fatalf("error = %v, want it to name the missing switch", err)
	}

	cfg.TrustForwardedFor = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid trusted-proxy config rejected: %v", err)
	}
}

func TestNetworkConfig_TrustedProxiesRejectsBadEntries(t *testing.T) {
	cases := []string{"not-an-ip", "10.0.0.0/33", "127.0.0.1/abc"}
	for _, entry := range cases {
		cfg := NetworkConfig{TrustForwardedFor: true, TrustedProxies: []string{entry}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("trustedProxies entry %q was accepted", entry)
		}
	}
}

func TestNetworkConfig_EffectiveTrimsBlankAndDuplicateEntries(t *testing.T) {
	cfg := NetworkConfig{
		TrustForwardedFor: true,
		TrustedProxies:    []string{" 127.0.0.1/32 ", "", "127.0.0.1/32", "10.0.0.0/8"},
	}
	effective := cfg.Effective()
	if len(effective.TrustedProxies) != 2 {
		t.Fatalf("trustedProxies = %v, want two unique trimmed entries", effective.TrustedProxies)
	}
	// An absent list stays nil so a code-built config compares equal to one
	// decoded from YAML.
	if empty := (NetworkConfig{}).Effective(); empty.TrustedProxies != nil {
		t.Fatalf("empty trustedProxies = %v, want nil", empty.TrustedProxies)
	}
}

func TestHTTPServerConfig_EffectiveAndValidate(t *testing.T) {
	if got := (HTTPServerConfig{}).Effective().MaxHeaderBytes; got != DefaultMaxHeaderBytes {
		t.Fatalf("default maxHeaderBytes = %d, want %d", got, DefaultMaxHeaderBytes)
	}
	if !(HTTPServerConfig{}).HeadersEnabled() {
		t.Fatal("security headers must default to enabled")
	}
	disabled := false
	if (HTTPServerConfig{SecurityHeaders: &disabled}).HeadersEnabled() {
		t.Fatal("explicitly disabled headers reported as enabled")
	}

	tooSmall := HTTPServerConfig{MaxHeaderBytes: MinMaxHeaderBytes - 1}
	if err := tooSmall.Validate(); err == nil {
		t.Fatal("maxHeaderBytes below the minimum was accepted")
	}
	tooBig := HTTPServerConfig{MaxHeaderBytes: MaxMaxHeaderBytes + 1}
	if err := tooBig.Validate(); err == nil {
		t.Fatal("maxHeaderBytes above the maximum was accepted")
	}
	if err := (HTTPServerConfig{ReadTimeout: -time.Second}).Validate(); err == nil {
		t.Fatal("negative readTimeout was accepted")
	}
}

func TestAuthRateLimitConfig_RejectsTooShortWindows(t *testing.T) {
	// A window this short can never accumulate enough failures, so accepting it
	// would leave the limiter looking enabled while it never blocks.
	cfg := AuthRateLimitConfig{MaxFailures: 3, Window: time.Nanosecond, BlockFor: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("1ns window was accepted")
	}
	shortBlock := AuthRateLimitConfig{MaxFailures: 3, Window: time.Minute, BlockFor: time.Nanosecond}
	if err := shortBlock.Validate(); err == nil {
		t.Fatal("1ns blockFor was accepted")
	}
	ok := AuthRateLimitConfig{MaxFailures: 3, Window: time.Second, BlockFor: time.Second}
	if err := ok.Validate(); err != nil {
		t.Fatalf("minimum valid config rejected: %v", err)
	}
}

func TestAuthRateLimitConfig_EffectiveDefaults(t *testing.T) {
	effective := (AuthRateLimitConfig{}).Effective()
	if effective.MaxFailures != DefaultAuthMaxFailures || effective.Window != DefaultAuthWindow || effective.BlockFor != DefaultAuthBlockFor {
		t.Fatalf("defaults = %+v", effective)
	}
}

func TestDaemonRestartPaths_CoverAcceptLoopLimits(t *testing.T) {
	base := Config{Server: HTTPServerConfig{MaxHeaderBytes: 65536, ReadTimeout: 0}}
	changed := base
	changed.Server.MaxHeaderBytes = 131072
	paths := daemonRestartPaths(base, changed)
	if !containsPath(paths, "/server/maxHeaderBytes") {
		t.Fatalf("maxHeaderBytes change produced restart paths %v", paths)
	}

	timeout := base
	timeout.Server.ReadTimeout = 30 * time.Second
	paths = daemonRestartPaths(base, timeout)
	if !containsPath(paths, "/server/readTimeout") {
		t.Fatalf("readTimeout change produced restart paths %v", paths)
	}

	// The policy must hold the active value until the restart happens,
	// otherwise the reported state and the running state disagree.
	result, paths := ApplyDaemonRestartPolicy(base, changed)
	if len(paths) == 0 {
		t.Fatal("no restart paths reported")
	}
	if result.Server.MaxHeaderBytes != base.Server.MaxHeaderBytes {
		t.Fatalf("published maxHeaderBytes = %d, want the active value %d",
			result.Server.MaxHeaderBytes, base.Server.MaxHeaderBytes)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
