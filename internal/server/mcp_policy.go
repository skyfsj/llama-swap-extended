package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
)

type mcpPolicyKey struct{}
type mcpRequestPolicy struct {
	Disabled       bool  `json:"disabled"`
	Allowed        any   `json:"allowed,omitempty"`
	HasAllowed     bool  `json:"has_allowed"`
	Approval       any   `json:"approval,omitempty"`
	PriorApprovals []any `json:"prior_approvals,omitempty"`
	ReadOnly       bool  `json:"read_only"`
}

func withMCPRequestPolicy(ctx context.Context, cfg config.Config, request map[string]json.RawMessage) context.Context {
	var tools, servers []map[string]any
	_ = json.Unmarshal(request["tools"], &tools)
	_ = json.Unmarshal(request["mcp_servers"], &servers)
	rules := map[string]mcpRequestPolicy{}
	if inherited, ok := ctx.Value(mcpPolicyKey{}).(map[string]mcpRequestPolicy); ok {
		for id, rule := range inherited {
			rules[id] = rule
		}
	}
	for _, decl := range append(tools, servers...) {
		kind, _ := decl["type"].(string)
		if kind != "mcp" && kind != "mcp_server" && kind != "remote_mcp" && kind != "url" {
			continue
		}
		endpoint, _ := decl["server_url"].(string)
		if endpoint == "" {
			endpoint, _ = decl["url"].(string)
		}
		if endpoint == "" {
			continue
		}
		for id, server := range cfg.ProtocolRuntime.MCP.Servers {
			if strings.TrimRight(endpoint, "/") != strings.TrimRight(server.URL, "/") {
				continue
			}
			rule := mcpRequestPolicy{Approval: decl["require_approval"]}
			if _, exists := decl["require_approval"]; !exists && kind != "mcp" {
				rule.Approval = "never"
			}
			for _, key := range []string{"authorization", "authorization_token", "headers"} {
				if value, ok := decl[key]; ok && value != nil && value != "" {
					rule.Disabled = true
				}
			}
			rule.Allowed, rule.HasAllowed = decl["allowed_tools"]
			if object, ok := rule.Allowed.(map[string]any); ok {
				if names, exists := object["tool_names"]; exists {
					rule.Allowed = names
				} else if _, known := object["read_only"].(bool); known {
					rule.HasAllowed = false
				}
				rule.ReadOnly, _ = object["read_only"].(bool)
			}
			if toolConfig, ok := decl["tool_configuration"].(map[string]any); ok {
				if enabled, ok := toolConfig["enabled"].(bool); ok && !enabled {
					rule.Disabled = true
				}
				if names, ok := toolConfig["allowed_tools"]; ok {
					rule.Allowed = names
					rule.HasAllowed = true
				}
			}
			if old, ok := rules[id]; ok {
				rule.Disabled = rule.Disabled || old.Disabled
				if old.HasAllowed {
					rule.Allowed = intersectMCPNames(old.Allowed, rule.Allowed, rule.HasAllowed)
					rule.HasAllowed = true
				}
				rule.ReadOnly = rule.ReadOnly || old.ReadOnly
				rule.PriorApprovals = mergeMCPApprovalPolicies(rule.Approval, old.Approval, old.PriorApprovals)
			}
			rules[id] = rule
		}
	}
	return context.WithValue(ctx, mcpPolicyKey{}, rules)
}
func mcpNameAllowed(value any, name string) bool {
	names, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range names {
		if item == name {
			return true
		}
	}
	return false
}
func intersectMCPNames(old, next any, hasNext bool) any {
	if !hasNext {
		return old
	}
	result := []any{}
	names, _ := old.([]any)
	for _, name := range names {
		if text, ok := name.(string); ok && mcpNameAllowed(next, text) {
			result = append(result, text)
		}
	}
	return result
}
func applyMCPRequestPolicy(ctx context.Context, b *mcpruntime.Binding) bool {
	rules, _ := ctx.Value(mcpPolicyKey{}).(map[string]mcpRequestPolicy)
	rule, ok := rules[b.Server]
	if !ok {
		return true
	}
	if rule.Disabled || rule.HasAllowed && !mcpNameAllowed(rule.Allowed, b.Tool.Name) || rule.ReadOnly && (b.Tool.Annotations == nil || !b.Tool.Annotations.ReadOnlyHint) {
		return false
	}
	required := mcpApprovalPolicyRequires(rule.Approval, b.Tool.Name)
	for _, prior := range rule.PriorApprovals {
		required = required || mcpApprovalPolicyRequires(prior, b.Tool.Name)
	}
	b.RequireApproval = required
	return true
}

func mergeMCPApprovalPolicies(current, old any, priors []any) []any {
	seen := map[string]bool{}
	encoded, _ := json.Marshal(current)
	seen[string(encoded)] = true
	result := []any{}
	for _, value := range append(append([]any(nil), priors...), old) {
		encoded, _ := json.Marshal(value)
		if !seen[string(encoded)] {
			seen[string(encoded)] = true
			result = append(result, value)
		}
	}
	return result
}
func mcpApprovalPolicyRequires(value any, name string) bool {
	if text, ok := value.(string); ok && text == "never" {
		return false
	}
	required := true
	if object, ok := value.(map[string]any); ok {
		if never, ok := object["never"].(map[string]any); ok && mcpNameAllowed(never["tool_names"], name) {
			required = false
		}
		if always, ok := object["always"].(map[string]any); ok && mcpNameAllowed(always["tool_names"], name) {
			required = true
		}
	}
	return required
}
