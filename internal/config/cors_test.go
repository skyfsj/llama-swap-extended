package config

import (
	"strings"
	"testing"
)

func TestCORSConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     CORSConfig
		wantErr string
	}{
		{name: "absent block keeps the permissive default", cfg: CORSConfig{}},
		{
			name: "explicit wildcard",
			cfg:  CORSConfig{AllowedOrigins: []string{"*"}},
		},
		{
			name: "explicit origin list",
			cfg:  CORSConfig{AllowedOrigins: []string{"https://dashboard.example.com", "http://localhost:8080"}},
		},
		{
			name:    "empty list is rejected",
			cfg:     CORSConfig{AllowedOrigins: []string{}},
			wantErr: "allowedOrigins must list at least one origin",
		},
		{
			name:    "other settings require origins",
			cfg:     CORSConfig{AllowCredentials: true},
			wantErr: "allowedOrigins is required",
		},
		{
			name:    "wildcard cannot allow credentials",
			cfg:     CORSConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true},
			wantErr: "may not contain \"*\" when allowCredentials",
		},
		{
			name:    "wildcard cannot allow private network",
			cfg:     CORSConfig{AllowedOrigins: []string{"*"}, AllowPrivateNetwork: true},
			wantErr: "may not contain \"*\" when allowPrivateNetwork",
		},
		{
			name:    "origin with a path is rejected",
			cfg:     CORSConfig{AllowedOrigins: []string{"https://example.com/app"}},
			wantErr: "must not include a path",
		},
		{
			name:    "origin without a scheme is rejected",
			cfg:     CORSConfig{AllowedOrigins: []string{"example.com"}},
			wantErr: "must be a scheme://host",
		},
		{
			name:    "origin with whitespace is rejected",
			cfg:     CORSConfig{AllowedOrigins: []string{" https://example.com"}},
			wantErr: "leading or trailing whitespace",
		},
		{
			name:    "invalid method",
			cfg:     CORSConfig{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET POST"}},
			wantErr: "not a valid HTTP method",
		},
		{
			name:    "invalid header",
			cfg:     CORSConfig{AllowedOrigins: []string{"*"}, AllowedHeaders: []string{"Bad Header"}},
			wantErr: "not a valid HTTP header name",
		},
		{
			name:    "negative maxAge",
			cfg:     CORSConfig{AllowedOrigins: []string{"*"}, MaxAge: -1},
			wantErr: "maxAge must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A security.cors block in YAML must load and validate end to end, and a bad
// one must fail the whole config load.
func TestConfig_CORSSection(t *testing.T) {
	t.Run("loads", func(t *testing.T) {
		content := `
security:
  cors:
    allowedOrigins:
      - https://dashboard.example.com
    allowCredentials: true
    maxAge: 600
models:
  model1:
    cmd: server --port ${PORT}
`
		cfg, err := LoadConfigFromReader(strings.NewReader(content))
		if err != nil {
			t.Fatalf("LoadConfigFromReader: %v", err)
		}
		got := cfg.Security.CORS
		if len(got.AllowedOrigins) != 1 || got.AllowedOrigins[0] != "https://dashboard.example.com" {
			t.Fatalf("allowedOrigins = %v", got.AllowedOrigins)
		}
		if !got.AllowCredentials || got.MaxAge != 600 {
			t.Fatalf("cors = %+v", got)
		}
	})

	t.Run("half-written block fails the load", func(t *testing.T) {
		content := `
security:
  cors:
    allowCredentials: true
models:
  model1:
    cmd: server --port ${PORT}
`
		_, err := LoadConfigFromReader(strings.NewReader(content))
		if err == nil || !strings.Contains(err.Error(), "allowedOrigins is required") {
			t.Fatalf("err = %v, want allowedOrigins is required", err)
		}
	})

	t.Run("invalid origin fails the load", func(t *testing.T) {
		content := `
security:
  cors:
    allowedOrigins:
      - https://example.com/app
models:
  model1:
    cmd: server --port ${PORT}
`
		_, err := LoadConfigFromReader(strings.NewReader(content))
		if err == nil || !strings.Contains(err.Error(), "must not include a path") {
			t.Fatalf("err = %v, want the path rejection", err)
		}
	})
}
