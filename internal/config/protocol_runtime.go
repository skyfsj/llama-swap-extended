package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

type ProtocolRuntimeConfig struct {
	MCP MCPRuntimeConfig `yaml:"mcp" json:"mcp"`
}

type MCPRuntimeConfig struct {
	Enabled bool                       `yaml:"enabled" json:"enabled"`
	Servers map[string]MCPServerConfig `yaml:"servers" json:"servers"`
}

type MCPServerConfig struct {
	Transport string            `yaml:"transport" json:"transport"`
	URL       string            `yaml:"url" json:"url"`
	Command   string            `yaml:"command" json:"command"`
	Args      []string          `yaml:"args" json:"args"`
	Env       []string          `yaml:"env" json:"env"`
	Headers   map[string]string `yaml:"headers" json:"headers"`
	Models    []string          `yaml:"models" json:"models"`
	Tools     []string          `yaml:"tools" json:"tools"`
	Resources []string          `yaml:"resources" json:"resources"`
	Prompts   []string          `yaml:"prompts" json:"prompts"`
	Approval  string            `yaml:"approval" json:"approval"`
}

func (c ProtocolRuntimeConfig) Validate() error {
	if len(c.MCP.Servers) > 32 {
		return fmt.Errorf("protocolRuntime.mcp supports at most 32 configured servers")
	}
	name := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)
	variableName := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)
	for id, s := range c.MCP.Servers {
		if !name.MatchString(id) {
			return fmt.Errorf("protocolRuntime.mcp: invalid server name %q", id)
		}
		if s.Approval != "" && s.Approval != "always" && s.Approval != "never" {
			return fmt.Errorf("MCP server %s: approval must be always or never", id)
		}
		switch s.Transport {
		case "stdio":
			if !filepath.IsAbs(s.Command) {
				return fmt.Errorf("MCP server %s: command must be absolute", id)
			}
		case "http", "sse":
			u, err := url.Parse(s.URL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
				return fmt.Errorf("MCP server %s: invalid HTTP URL", id)
			}
			for key := range u.Query() {
				switch strings.ToLower(key) {
				case "key", "token", "api_key", "apikey", "access_token", "authorization", "auth_token", "secret":
					return fmt.Errorf("MCP server %s: credentials must use header environment references", id)
				}
			}
		default:
			return fmt.Errorf("MCP server %s: unsupported transport %q", id, s.Transport)
		}
		for header, variable := range s.Headers {
			if header == "" || !variableName.MatchString(variable) {
				return fmt.Errorf("MCP server %s: headers map names to environment variable names", id)
			}
		}
		for _, variable := range s.Env {
			if !variableName.MatchString(variable) {
				return fmt.Errorf("MCP server %s: invalid environment variable name", id)
			}
		}
	}
	return nil
}
