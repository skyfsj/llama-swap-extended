package config

import (
	"fmt"
	"time"
)

// HTTP server defaults. The header cap is deliberately below Go's 1 MiB
// default: llama-swap is an API gateway, and no legitimate client needs a
// megabyte of request headers.
const (
	DefaultMaxHeaderBytes = 64 << 10
	MinMaxHeaderBytes     = 4 << 10
	MaxMaxHeaderBytes     = 1 << 20

	DefaultAuthMaxFailures = 10
	DefaultAuthWindow      = time.Minute
	DefaultAuthBlockFor    = 5 * time.Minute

	// The lower bounds keep a mistyped duration from silently disabling the
	// limiter: a window this short can never accumulate failures, and a block
	// this short is indistinguishable from no block at all.
	MinAuthWindow   = time.Second
	MinAuthBlockFor = time.Second
)

// HTTPServerConfig tunes the accept loop and the defensive response headers.
// Timeouts that would cut a streaming response are intentionally absent.
type HTTPServerConfig struct {
	// MaxHeaderBytes bounds the request line plus headers. 0 uses
	// DefaultMaxHeaderBytes.
	MaxHeaderBytes int `yaml:"maxHeaderBytes" json:"maxHeaderBytes"`
	// ReadTimeout bounds reading one complete request (headers and body).
	// 0 disables it: a large chat payload on a slow link must not be cut
	// off. Enable it when clients are known to be fast.
	ReadTimeout time.Duration `yaml:"readTimeout" json:"readTimeout"`
	// SecurityHeaders controls the defensive response headers (nosniff,
	// frame denial, referrer policy, permissions policy and the UI content
	// security policy). nil means enabled.
	SecurityHeaders *bool `yaml:"securityHeaders" json:"securityHeaders"`
}

// Effective fills the defaults the accept loop uses. It is applied during
// configuration loading so every consumer sees the same values.
func (c HTTPServerConfig) Effective() HTTPServerConfig {
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	return c
}

// HeadersEnabled reports whether the defensive response headers are on.
func (c HTTPServerConfig) HeadersEnabled() bool {
	return c.SecurityHeaders == nil || *c.SecurityHeaders
}

// Validate rejects values the accept loop cannot honour.
func (c HTTPServerConfig) Validate() error {
	effective := c.Effective()
	if effective.MaxHeaderBytes < MinMaxHeaderBytes || effective.MaxHeaderBytes > MaxMaxHeaderBytes {
		return fmt.Errorf("server.maxHeaderBytes must be between %d and %d", MinMaxHeaderBytes, MaxMaxHeaderBytes)
	}
	if c.ReadTimeout < 0 {
		return fmt.Errorf("server.readTimeout must be >= 0")
	}
	return nil
}

// SecurityConfig groups request-time protections that are not storage or
// network policy.
type SecurityConfig struct {
	AuthRateLimit AuthRateLimitConfig `yaml:"authRateLimit" json:"authRateLimit"`
	CORS          CORSConfig          `yaml:"cors" json:"cors"`
}

// Effective fills the limiter defaults.
func (c SecurityConfig) Effective() SecurityConfig {
	c.AuthRateLimit = c.AuthRateLimit.Effective()
	return c
}

// Validate rejects a configuration that could not possibly work.
func (c SecurityConfig) Validate() error {
	if err := c.AuthRateLimit.Validate(); err != nil {
		return err
	}
	return c.CORS.Validate()
}

// AuthRateLimitConfig throttles failed credential checks per caller address.
// A successful authentication clears the counter, so an operator who mistypes
// a key a few times is not locked out; the limit exists to make online
// guessing of weak YAML keys expensive.
type AuthRateLimitConfig struct {
	// Disabled turns the limiter off entirely.
	Disabled bool `yaml:"disabled" json:"disabled"`
	// MaxFailures is the number of failed checks allowed inside Window
	// before BlockFor starts.
	MaxFailures int `yaml:"maxFailures" json:"maxFailures"`
	// Window is the sliding window the failures are counted over.
	Window time.Duration `yaml:"window" json:"window"`
	// BlockFor is how long a caller is rejected once the limit is reached.
	BlockFor time.Duration `yaml:"blockFor" json:"blockFor"`
}

// Effective fills the limiter defaults.
func (c AuthRateLimitConfig) Effective() AuthRateLimitConfig {
	if c.MaxFailures <= 0 {
		c.MaxFailures = DefaultAuthMaxFailures
	}
	if c.Window <= 0 {
		c.Window = DefaultAuthWindow
	}
	if c.BlockFor <= 0 {
		c.BlockFor = DefaultAuthBlockFor
	}
	return c
}

// Validate bounds the limiter values so a typo cannot disable protection by
// accident (for example a 1ns window or a million attempts). The lower bounds
// matter as much as the upper ones: a window shorter than a few hundred
// milliseconds can never accumulate enough failures to trip, which would look
// like the limiter is on while it silently never blocks.
func (c AuthRateLimitConfig) Validate() error {
	effective := c.Effective()
	if effective.MaxFailures > 100000 {
		return fmt.Errorf("security.authRateLimit.maxFailures must be <= 100000")
	}
	if effective.Window < MinAuthWindow || effective.Window > 24*time.Hour {
		return fmt.Errorf("security.authRateLimit.window must be between %s and 24h", MinAuthWindow)
	}
	if effective.BlockFor < MinAuthBlockFor || effective.BlockFor > 24*time.Hour {
		return fmt.Errorf("security.authRateLimit.blockFor must be between %s and 24h", MinAuthBlockFor)
	}
	return nil
}
