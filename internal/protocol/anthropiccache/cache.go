// Package anthropiccache implements a conservative Go-native Claude Code
// prompt-cache repair pipeline.
package anthropiccache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Mode string

const (
	ModeOff   Mode = "off"
	ModeAuto  Mode = "auto"
	ModeForce Mode = "force"
)

type TransformConfig struct {
	FingerprintStrip      bool
	SortStabilization     bool
	FreshSessionSort      bool
	IdentityNormalization bool
	CacheControlNormalize bool
	TTLManagement         bool
	ThinkingSanitize      string
	CCVersionNormalize    string
	HighRisk              string
}

type Options struct {
	Mode       Mode
	Transforms TransformConfig
}

type TransformResult struct {
	Name    string `json:"name"`
	Applied bool   `json:"applied"`
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
	Anomaly string `json:"anomaly,omitempty"`
}

type Result struct {
	Body       []byte            `json:"-"`
	Enabled    bool              `json:"enabled"`
	Detected   bool              `json:"detected"`
	Transforms []TransformResult `json:"transforms"`
	PrefixHash string            `json:"prefixHash,omitempty"`
	Anomalies  []string          `json:"anomalies,omitempty"`
	Changed    bool              `json:"changed"`
}

// DetectClaudeCode requires both a recognizable client signal and Claude
// shaped request fields. Generic Anthropic SDK requests are left untouched.
func DetectClaudeCode(headers http.Header, body map[string]any) bool {
	userAgent := headerValue(headers, "user-agent")
	xClaudeCode := headerValue(headers, "x-claude-code")
	xApp := headerValue(headers, "x-app")
	signal := strings.Contains(strings.ToLower(userAgent), "claude-code") ||
		strings.EqualFold(strings.TrimSpace(xClaudeCode), "true") ||
		strings.Contains(strings.ToLower(xApp), "claude-code") ||
		strings.TrimSpace(headerValue(headers, "x-claude-code-version")) != "" ||
		strings.TrimSpace(headerValue(headers, "x-claude-code-session-id")) != "" ||
		strings.TrimSpace(headerValue(headers, "x-claude-session-id")) != ""
	if !signal {
		return false
	}
	if messageCount(body["messages"]) == 0 {
		return false
	}
	// A signal alone is not enough: ordinary SDK callers can forward a
	// Claude-Code-like user-agent. Require at least one Anthropic context
	// structure that Claude Code emits on Messages requests.
	if system, exists := body["system"]; exists && hasRequestContent(system) {
		return true
	}
	if tools, exists := body["tools"]; exists && hasRequestContent(tools) {
		return true
	}
	return false
}

func messageCount(value any) int {
	switch messages := value.(type) {
	case []any:
		return len(messages)
	case []map[string]any:
		return len(messages)
	default:
		return 0
	}
}

func headerValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return values[0]
	}
	return ""
}

func hasRequestContent(value any) bool {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	case []map[string]any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return value != nil
	}
}

// Apply applies configured transforms and returns a stable, idempotent JSON
// body plus per-transform telemetry.
func Apply(input []byte, headers http.Header, options Options) (Result, error) {
	if options.Mode == "" {
		options.Mode = ModeAuto
	}
	// Apply is also a public protocol primitive and can be called without the
	// config loader.  Do not silently treat a typo (for example, "automatic")
	// as auto mode: an unknown value would otherwise enable cache rewriting
	// while making the caller believe it had selected a different policy.
	switch options.Mode {
	case ModeOff, ModeAuto, ModeForce:
	default:
		return Result{}, fmt.Errorf("unsupported anthropic cache mode %q", options.Mode)
	}
	// Disabled mode is a strict pass-through. In particular, do not decode
	// the body first: callers may use this switch while proxying non-JSON
	// payloads or malformed requests and must still receive the original bytes.
	if options.Mode == ModeOff {
		return Result{Body: append([]byte(nil), input...), Transforms: skippedAll("disabled")}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(input, &body); err != nil {
		return Result{}, fmt.Errorf("decode anthropic messages request: %w", err)
	}
	detected := options.Mode == ModeForce || DetectClaudeCode(headers, body)
	if !detected {
		return Result{Body: append([]byte(nil), input...), Transforms: skippedAll("client-not-confirmed")}, nil
	}
	if options.Transforms == (TransformConfig{}) {
		options.Transforms = SafeDefaults()
	}
	result := Result{Detected: true, Enabled: true}
	apply := func(name string, enabled bool, fn func(map[string]any) (bool, string, string)) {
		tr := TransformResult{Name: name}
		if !enabled {
			tr.Skipped, tr.Reason = true, "disabled"
			result.Transforms = append(result.Transforms, tr)
			return
		}
		changed, reason, anomaly := fn(body)
		tr.Applied, tr.Reason, tr.Anomaly = changed, reason, anomaly
		if anomaly != "" {
			result.Anomalies = append(result.Anomalies, anomaly)
		}
		result.Changed = result.Changed || changed
		result.Transforms = append(result.Transforms, tr)
	}
	apply("fingerprint-strip", options.Transforms.FingerprintStrip, stripFingerprint)
	apply("sort-stabilization", options.Transforms.SortStabilization, stabilizeSort)
	apply("fresh-session-sort", options.Transforms.FreshSessionSort, stabilizeSession)
	apply("identity-normalization", options.Transforms.IdentityNormalization, normalizeIdentity)
	apply("cache-control-normalize", options.Transforms.CacheControlNormalize, normalizeCacheControl)
	apply("ttl-management", options.Transforms.TTLManagement, manageTTL)
	apply("thinking-sanitize", options.Transforms.ThinkingSanitize == "safe" || options.Transforms.ThinkingSanitize == "experimental", sanitizeThinking)
	// Version normalization and aggressive rewrites stay visible in telemetry
	// but are never enabled by the safe registry. Callers can explicitly opt
	// into the version transform with "on"; high-risk remains audit-only until
	// a backend-specific golden fixture approves it.
	apply("cc-version-normalize", options.Transforms.CCVersionNormalize == "on", normalizeCCVersion)
	apply("high-risk", false, func(map[string]any) (bool, string, string) {
		if options.Transforms.HighRisk == "" || options.Transforms.HighRisk == "off" {
			return false, "disabled", ""
		}
		return false, "audit-only transform", "high-risk cache transform was not applied"
	})
	canonical, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("encode anthropic messages request: %w", err)
	}
	result.Body = canonical
	result.PrefixHash = PrefixHash(body)
	return result, nil
}

func SafeDefaults() TransformConfig {
	return TransformConfig{FingerprintStrip: true, SortStabilization: true, FreshSessionSort: true, IdentityNormalization: true, CacheControlNormalize: true, TTLManagement: true, ThinkingSanitize: "safe"}
}

func skippedAll(reason string) []TransformResult {
	names := []string{"fingerprint-strip", "sort-stabilization", "fresh-session-sort", "identity-normalization", "cache-control-normalize", "ttl-management", "thinking-sanitize", "cc-version-normalize", "high-risk"}
	out := make([]TransformResult, 0, len(names))
	for _, name := range names {
		out = append(out, TransformResult{Name: name, Skipped: true, Reason: reason})
	}
	return out
}

func normalizeCCVersion(body map[string]any) (bool, string, string) {
	// Keep this transform intentionally narrow: only trim a leading "v" from
	// an explicit client version. It is opt-in because changing version strings
	// can affect upstream feature negotiation and cache keys.
	for _, key := range []string{"claude_code_version", "claudeCodeVersion", "client_version"} {
		if value, ok := body[key].(string); ok && strings.HasPrefix(value, "v") && len(value) > 1 {
			body[key] = strings.TrimPrefix(value, "v")
			return true, "client version normalized", ""
		}
	}
	return false, "version already stable", ""
}

func stripFingerprint(body map[string]any) (bool, string, string) {
	changed := stabilizeBillingFingerprint(body)
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		if changed {
			return true, "stabilized billing fingerprint", ""
		}
		return false, "no metadata", ""
	}
	for _, key := range []string{"request_id", "requestId", "session_id", "sessionId", "cache_fingerprint", "cacheFingerprint"} {
		if _, exists := metadata[key]; exists {
			delete(metadata, key)
			changed = true
		}
	}
	if changed {
		return true, "removed transient cache fingerprint fields", ""
	}
	return false, "already stable", ""
}

const fingerprintSalt = "59cf53e54c78"

var billingVersionPattern = regexp.MustCompile(`cc_version=([^;\s]+)`)

func computeFingerprint(messageText, version string) string {
	indices := [...]int{4, 7, 20}
	var chars [len(indices)]byte
	for i, index := range indices {
		if index < len(messageText) {
			chars[i] = messageText[index]
		} else {
			chars[i] = '0'
		}
	}
	sum := sha256.Sum256([]byte(fingerprintSalt + string(chars[:]) + version))
	return hex.EncodeToString(sum[:])[:3]
}

func stabilizeBillingFingerprint(body map[string]any) bool {
	system, ok := body["system"].([]any)
	if !ok {
		return false
	}
	messages, _ := body["messages"].([]any)
	realText := realUserMessageText(messages)
	legacyText := firstUserMessageText(messages)
	if realText == "" && legacyText == "" {
		return false
	}
	changed := false
	for index, raw := range system {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, ok := block["text"].(string)
		if !ok || !strings.Contains(text, "x-anthropic-billing-header:") {
			continue
		}
		match := billingVersionPattern.FindStringSubmatch(text)
		if len(match) != 2 {
			continue
		}
		parts := strings.Split(match[1], ".")
		if len(parts) < 4 {
			continue
		}
		baseVersion := strings.Join(parts[:3], ".")
		oldFingerprint := parts[3]
		verified := computeFingerprint(realText, baseVersion) == oldFingerprint
		if !verified {
			verified = computeFingerprint(legacyText, baseVersion) == oldFingerprint
		}
		if !verified {
			continue
		}
		// Some Claude Code continuation requests begin with a user message that
		// contains only a system-reminder. In that shape realUserMessageText is
		// intentionally empty and verification falls back to the legacy first
		// user block. Use the same source for the rewritten fingerprint; hashing
		// an empty string here would manufacture a new, unstable cache key after
		// a successful legacy verification.
		stableText := realText
		if stableText == "" {
			stableText = legacyText
		}
		stable := computeFingerprint(stableText, baseVersion)
		if stable == oldFingerprint {
			continue
		}
		newVersion := baseVersion + "." + stable
		newText := strings.Replace(text, "cc_version="+match[1], "cc_version="+newVersion, 1)
		if newText != text {
			block["text"] = newText
			system[index] = block
			changed = true
		}
	}
	return changed
}

func realUserMessageText(messages []any) string {
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if !strings.HasPrefix(content, "<system-reminder>") {
				return content
			}
		case []any:
			for _, blockRaw := range content {
				block, ok := blockRaw.(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				text, _ := block["text"].(string)
				if text != "" && !strings.HasPrefix(text, "<system-reminder>") {
					return text
				}
			}
		}
	}
	return ""
}

func firstUserMessageText(messages []any) string {
	if len(messages) == 0 {
		return ""
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" {
		return ""
	}
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		for _, blockRaw := range content {
			block, ok := blockRaw.(map[string]any)
			if ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					return text
				}
			}
		}
	}
	return ""
}

func stabilizeSort(body map[string]any) (bool, string, string) {
	changed := false
	if tools, ok := body["tools"].([]any); ok && len(tools) >= 2 {
		before, _ := json.Marshal(tools)
		sort.SliceStable(tools, func(i, j int) bool {
			leftName, rightName := toolName(tools[i]), toolName(tools[j])
			if leftName != rightName {
				return leftName < rightName
			}
			left, _ := json.Marshal(tools[i])
			right, _ := json.Marshal(tools[j])
			return string(left) < string(right)
		})
		after, _ := json.Marshal(tools)
		changed = string(before) != string(after)
	}
	if system, ok := body["system"].([]any); ok {
		for _, raw := range system {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			sorted, didChange := sortReminderText(text)
			if didChange {
				block["text"] = sorted
				changed = true
			}
		}
	}
	if changed {
		return true, "tools and system reminders stabilized", ""
	}
	return false, "already sorted", ""
}

// sortReminderText ports the two deterministic textual reorderings used by
// claude-code-cache-fix. It intentionally requires the complete
// <system-reminder> envelope and a recognizable header, so ordinary system
// prose is never rearranged.
func sortReminderText(text string) (string, bool) {
	if !strings.Contains(text, "User-invocable skills") && !strings.Contains(text, "following skills are available") && !strings.Contains(text, "deferred tools are now available") {
		return text, false
	}
	end := strings.LastIndex(text, "\n</system-reminder>")
	if end < 0 {
		return text, false
	}
	prefixEnd := strings.Index(text, "\n\n")
	if strings.Contains(text, "deferred tools are now available") {
		prefixEnd = strings.Index(text, "\n")
	}
	if prefixEnd < 0 || prefixEnd+2 >= end {
		return text, false
	}
	prefixWidth := 2
	if strings.Contains(text, "deferred tools are now available") {
		prefixWidth = 1
	}
	prefix := text[:prefixEnd+prefixWidth]
	body := text[prefixEnd+prefixWidth : end]
	suffix := text[end:]
	var entries []string
	if strings.Contains(text, "deferred tools are now available") {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				entries = append(entries, line)
			}
		}
	} else {
		var current []string
		flush := func() {
			if len(current) > 0 {
				entries = append(entries, strings.Join(current, "\n"))
				current = nil
			}
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "- ") && len(current) > 0 {
				flush()
			}
			current = append(current, line)
		}
		flush()
	}
	if len(entries) < 2 {
		return text, false
	}
	sort.Strings(entries)
	result := prefix + strings.Join(entries, "\n") + suffix
	return result, result != text
}

func stabilizeSession(body map[string]any) (bool, string, string) {
	changed := false
	if metadata, ok := body["metadata"].(map[string]any); ok {
		if raw, exists := metadata["session_tags"]; exists {
			if tags, ok := raw.([]any); ok && len(tags) == 0 {
				delete(metadata, "session_tags")
				changed = true
			}
		}
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		if changed {
			return true, "removed empty session tags", ""
		}
		return false, "already stable", ""
	}
	firstUser := -1
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if ok && message["role"] == "user" {
			firstUser = index
			break
		}
	}
	if firstUser < 0 {
		if changed {
			return true, "removed empty session tags", ""
		}
		return false, "already stable", ""
	}
	firstMessage, ok := messages[firstUser].(map[string]any)
	if !ok {
		return changed, "removed empty session tags", ""
	}
	firstContent, ok := firstMessage["content"].([]any)
	if !ok {
		return changed, "session has no structured content", ""
	}
	// Clear artifacts are request-local and should not occupy a stable cache
	// prefix on the first user turn.
	filteredFirst := make([]any, 0, len(firstContent))
	for _, raw := range firstContent {
		block, ok := raw.(map[string]any)
		if ok {
			if text, _ := block["text"].(string); isClearArtifact(text) {
				changed = true
				continue
			}
		}
		filteredFirst = append(filteredFirst, raw)
	}
	if len(filteredFirst) != len(firstContent) {
		firstMessage["content"] = filteredFirst
		firstContent = filteredFirst
	}

	// Only relocate when a known block is scattered beyond the first user
	// message. With no scattered block, normalize recognized blocks in place and
	// retain ordinary message order.
	scattered := false
	for index := firstUser + 1; index < len(messages) && !scattered; index++ {
		message, ok := messages[index].(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if ok {
				text, _ := block["text"].(string)
				if freshBlockType(text) != "" {
					scattered = true
					break
				}
			}
		}
	}
	if !scattered {
		for index, raw := range firstContent {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, _ := block["text"].(string)
			fixed := fixFreshBlockText(text)
			if fixed != text {
				block["text"] = fixed
				firstContent[index] = block
				changed = true
			}
		}
		if changed {
			firstMessage["content"] = firstContent
		}
		if changed {
			messages[firstUser] = firstMessage
			return true, "fresh-session blocks stabilized", ""
		}
		return false, "already stable", ""
	}

	found := make(map[string]map[string]any)
	for index := len(messages) - 1; index >= firstUser; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for contentIndex := len(content) - 1; contentIndex >= 0; contentIndex-- {
			block, ok := content[contentIndex].(map[string]any)
			if !ok {
				continue
			}
			text, _ := block["text"].(string)
			blockType := freshBlockType(text)
			if blockType == "" {
				continue
			}
			if _, exists := found[blockType]; exists {
				continue
			}
			copyBlock := cloneAnyMap(block)
			copyBlock["text"] = fixFreshBlockText(text)
			delete(copyBlock, "cache_control")
			found[blockType] = copyBlock
		}
	}
	if len(found) == 0 {
		return changed, "session blocks unchanged", ""
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(content))
		for _, child := range content {
			block, ok := child.(map[string]any)
			if ok {
				text, _ := block["text"].(string)
				if freshBlockType(text) != "" {
					changed = true
					continue
				}
			}
			filtered = append(filtered, child)
		}
		if len(filtered) != len(content) {
			message["content"] = filtered
			messages[index] = message
		}
	}
	order := []string{"deferred", "mcp", "skills", "hooks"}
	prepend := make([]any, 0, len(found))
	for _, blockType := range order {
		if block, exists := found[blockType]; exists {
			prepend = append(prepend, block)
		}
	}
	firstMessage, _ = messages[firstUser].(map[string]any)
	content, _ := firstMessage["content"].([]any)
	firstMessage["content"] = append(prepend, content...)
	messages[firstUser] = firstMessage
	return true, "scattered session blocks relocated", ""
}

func freshBlockType(text string) string {
	if !strings.HasPrefix(text, "<system-reminder>") {
		return ""
	}
	if strings.HasPrefix(text, "<system-reminder>\nThe following skills are available") {
		return "skills"
	}
	if strings.HasPrefix(text, "<system-reminder>\nThe following deferred tools are now available") {
		return "deferred"
	}
	if strings.HasPrefix(text, "<system-reminder>\n# MCP Server Instructions") {
		return "mcp"
	}
	limit := len(text)
	if limit > 200 {
		limit = 200
	}
	if strings.Contains(text[:limit], "hook success") {
		return "hooks"
	}
	return ""
}

func isClearArtifact(text string) bool {
	return strings.HasPrefix(text, "<local-command-caveat>") ||
		strings.HasPrefix(text, "<command-name>") ||
		strings.HasPrefix(text, "<local-command-stdout>")
}

var sessionKnowledgePattern = regexp.MustCompile(`\n<session_knowledge[^>]*>[\s\S]*?</session_knowledge>`)
var sessionResumePattern = regexp.MustCompile(`SessionStart:resume hook success:`)
var sessionIDPattern = regexp.MustCompile(`\n?<session-id>[^<]*</session-id>`)
var lastActivePattern = regexp.MustCompile(`\nLast active:[^\n]*`)

func fixFreshBlockText(text string) string {
	if blockType := freshBlockType(text); blockType == "skills" || blockType == "deferred" {
		text, _ = sortReminderText(text)
	}
	if freshBlockType(text) == "hooks" {
		text = sessionKnowledgePattern.ReplaceAllString(text, "")
	}
	if end := strings.LastIndex(text, "</system-reminder>"); end >= 0 {
		prefix := strings.TrimRight(text[:end], " \t\r\n")
		text = prefix + "\n</system-reminder>"
	}
	return text
}

func normalizeIdentity(body map[string]any) (bool, string, string) {
	changed := false
	metadata, ok := body["metadata"].(map[string]any)
	if ok {
		for _, key := range []string{"user_id", "userId", "client_id", "clientId"} {
			if value, exists := metadata[key]; exists {
				if s, valid := value.(string); !valid || strings.TrimSpace(s) == "" {
					delete(metadata, key)
					changed = true
				}
			}
		}
	}
	if system, ok := body["system"].([]any); ok {
		for index, raw := range system {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			fixed := sessionKnowledgePattern.ReplaceAllString(text, "")
			fixed = normalizeSessionStartText(fixed)
			if fixed != text {
				block["text"] = fixed
				system[index] = block
				changed = true
			}
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			content, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for index, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				text, ok := block["text"].(string)
				if !ok {
					continue
				}
				fixed := normalizeSessionStartText(text)
				if fixed != text {
					block["text"] = fixed
					content[index] = block
					changed = true
				}
			}
		}
	}
	if changed {
		return true, "volatile identity normalized", ""
	}
	if !ok {
		return false, "no identity metadata", ""
	}
	return false, "identity stable", ""
}

func normalizeSessionStartText(text string) string {
	if !strings.Contains(text, "SessionStart:") {
		return text
	}
	text = sessionResumePattern.ReplaceAllString(text, "SessionStart:startup hook success:")
	text = sessionIDPattern.ReplaceAllString(text, "")
	text = lastActivePattern.ReplaceAllString(text, "")
	return text
}

func normalizeCacheControl(body map[string]any) (bool, string, string) {
	changed := false
	// Claude Code may scatter user-message breakpoints across blocks. Keep the
	// last user breakpoint only, at the end of the last user content block. The
	// marker is semantically equivalent but gives the prefix cache one stable
	// boundary. Preserve an explicitly selected supported TTL for the later TTL
	// transform; malformed values are replaced there.
	var lastMarker map[string]any
	var lastUserContent map[string]any
	lastUserIndex := -1
	markerSeen := false
	type userBlock struct {
		value  map[string]any
		marker map[string]any
	}
	var userBlocks []userBlock
	if messages, ok := body["messages"].([]any); ok {
		for _, rawMessage := range messages {
			message, ok := rawMessage.(map[string]any)
			if !ok || message["role"] != "user" {
				continue
			}
			content, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				entry := userBlock{value: block}
				if raw, exists := block["cache_control"]; exists {
					markerSeen = true
					if marker, ok := raw.(map[string]any); ok {
						entry.marker = marker
						if len(marker) > 0 {
							lastMarker = cloneAnyMap(marker)
						}
					}
				}
				userBlocks = append(userBlocks, entry)
				lastUserContent = block
				lastUserIndex = len(userBlocks) - 1
			}
		}
		if lastUserContent != nil && markerSeen {
			canonical := map[string]any{"type": "ephemeral"}
			if ttl, ok := lastMarker["ttl"].(string); ok && strings.TrimSpace(ttl) != "" {
				canonical["ttl"] = strings.TrimSpace(ttl)
			}
			for index, entry := range userBlocks {
				if index != lastUserIndex {
					if _, exists := entry.value["cache_control"]; exists {
						delete(entry.value, "cache_control")
						changed = true
					}
					continue
				}
				if entry.marker == nil || !sameAnyMap(entry.marker, canonical) {
					entry.value["cache_control"] = canonical
					changed = true
				}
			}
		}
	}
	walkMaps(body, func(m map[string]any) {
		if typ, _ := m["type"].(string); typ == "thinking" || typ == "redacted_thinking" {
			return
		}
		cc, ok := m["cache_control"].(map[string]any)
		if !ok {
			return
		}
		if typ, ok := cc["type"].(string); !ok || typ != "ephemeral" {
			cc["type"] = "ephemeral"
			changed = true
		}
	})
	if changed {
		return true, "cache_control canonicalized", ""
	}
	return false, "already canonical", ""
}

func cloneAnyMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func sameAnyMap(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}
	for key, expected := range right {
		actual, exists := left[key]
		if !exists {
			return false
		}
		switch expected := expected.(type) {
		case string:
			actualString, ok := actual.(string)
			if !ok || actualString != expected {
				return false
			}
		default:
			if fmt.Sprint(actual) != fmt.Sprint(expected) {
				return false
			}
		}
	}
	return true
}

func manageTTL(body map[string]any) (bool, string, string) {
	changed := false
	ttlParam := requestTTL(body)
	walkMaps(body, func(m map[string]any) {
		if typ, _ := m["type"].(string); typ == "thinking" || typ == "redacted_thinking" {
			return
		}
		cc, ok := m["cache_control"].(map[string]any)
		if !ok {
			return
		}
		ttl, ok := cc["ttl"].(string)
		if !ok || (ttl != "5m" && ttl != "1h") {
			cc["ttl"] = ttlParam
			changed = true
		}
	})
	if changed {
		return true, "ttl normalized to " + ttlParam, ""
	}
	return false, "ttl stable", ""
}

// requestTTL mirrors the cache-fix extension's conservative tier selection.
// The JavaScript reference receives an internal tier from its preceding
// telemetry stage; the protocol-only Go adapter has no such side channel, so
// it reuses an already-present five-minute marker when one exists and falls
// back to the stable one-hour tier. Arbitrary request fields are never used as
// control input and therefore cannot leak into the upstream Anthropic body.
func requestTTL(body map[string]any) string {
	ttl := "1h"
	walkMaps(body, func(m map[string]any) {
		if ttl == "5m" {
			return
		}
		cc, ok := m["cache_control"].(map[string]any)
		if !ok {
			return
		}
		if value, ok := cc["ttl"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "5m") {
			ttl = "5m"
		}
	})
	return ttl
}

func sanitizeThinking(body map[string]any) (bool, string, string) {
	changed := false
	anomaly := ""
	messages, ok := body["messages"].([]any)
	if !ok {
		return false, "no messages", ""
	}
	for index, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		activeToolContinuation := hasFollowingToolResult(messages, index, content)
		filtered := make([]any, 0, len(content))
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok || block["type"] != "thinking" || activeToolContinuation {
				filtered = append(filtered, rawBlock)
				continue
			}
			if thinking, exists := block["thinking"]; exists {
				text, isString := thinking.(string)
				if isString && text == "" {
					changed = true
					anomaly = "omitted thinking block removed"
					continue
				}
				if !isString {
					block["thinking"] = fmt.Sprint(thinking)
					changed = true
					anomaly = "non-string thinking content normalized"
				}
			}
			if sig, exists := block["signature"]; exists {
				if value, valid := sig.(string); !valid || strings.TrimSpace(value) == "" {
					delete(block, "signature")
					changed = true
					anomaly = "malformed thinking signature sanitized"
				}
			}
			filtered = append(filtered, block)
		}
		if len(filtered) != len(content) {
			message["content"] = filtered
		}
	}
	if changed {
		return true, "thinking blocks sanitized", anomaly
	}
	return false, "thinking blocks valid", ""
}

func hasFollowingToolResult(messages []any, assistantIndex int, content []any) bool {
	var toolID string
	for i := len(content) - 1; i >= 0; i-- {
		block, ok := content[i].(map[string]any)
		if !ok {
			continue
		}
		if block["type"] != "tool_use" {
			break
		}
		toolID, _ = block["id"].(string)
		if toolID != "" {
			break
		}
	}
	if toolID == "" {
		return false
	}
	for _, rawMessage := range messages[assistantIndex+1:] {
		message, ok := rawMessage.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			if id, _ := block["tool_use_id"].(string); id == toolID {
				return true
			}
		}
	}
	return false
}

func toolName(value any) string {
	m, _ := value.(map[string]any)
	if fn, ok := m["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	if name, ok := m["name"].(string); ok {
		return name
	}
	return "~"
}

func walkMaps(value any, fn func(map[string]any)) {
	switch v := value.(type) {
	case map[string]any:
		fn(v)
		for _, child := range v {
			walkMaps(child, fn)
		}
	case []any:
		for _, child := range v {
			walkMaps(child, fn)
		}
	}
}

// PrefixHash hashes only the cache-relevant prefix, not the final user turn.
func PrefixHash(body map[string]any) string {
	prefix := map[string]any{"system": body["system"], "tools": body["tools"]}
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		prefix["messages"] = messages[:len(messages)-1]
	}
	data, _ := json.Marshal(prefix)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type Usage struct {
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	ReasoningTokens          int64   `json:"reasoning_tokens"`
	UncachedInputTokens      int64   `json:"uncached_input_tokens"`
	HitRatio                 float64 `json:"cache_hit_ratio"`
	CreationRatio            float64 `json:"cache_creation_ratio"`
	// explicitUncached is true when the provider reported an uncached input
	// field directly. When false, UncachedInputTokens is derived from the
	// total and cache partitions and must be recomputed after merging
	// streaming snapshots.
	explicitUncached bool
}

func ExtractUsage(body []byte) Usage {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return Usage{}
	}
	var root map[string]any
	if json.Unmarshal(body, &root) == nil && root != nil {
		if usage, ok := usageMap(root); ok {
			return finalizeUsage(deriveUncachedPartition(usageFromMap(usage)))
		}
	}

	// Streaming providers commonly put usage in a final event, while
	// Anthropic-compatible gateways may split cache and output accounting over
	// more than one event. Parse data fields here as a fallback so the same
	// accounting path is used for JSON and SSE responses. Usage fields in SSE
	// events are treated as cumulative snapshots; taking the maximum prevents a
	// repeated final usage event from charging the same request twice.
	var total Usage
	found := false
	forEachSSEJSON(body, func(event map[string]any) {
		usage, ok := usageMap(event)
		if !ok {
			return
		}
		candidate := usageFromMap(usage)
		total = mergeUsage(total, candidate)
		found = true
	})
	if !found {
		return Usage{}
	}
	return finalizeUsage(deriveUncachedPartition(total))
}

// deriveUncachedPartition fills the uncached input partition from the total
// and cache partitions when the provider did not report it explicitly.
// OpenAI compatible usage reports the prompt total in input tokens and
// carries the cached portion in token details, while Anthropic reports input
// tokens as the uncached remainder; clamping the difference keeps both
// shapes bounded.
func deriveUncachedPartition(u Usage) Usage {
	if u.explicitUncached {
		return u
	}
	u.UncachedInputTokens = u.InputTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	if u.UncachedInputTokens < 0 {
		u.UncachedInputTokens = 0
	}
	return u
}

func usageMap(root map[string]any) (map[string]any, bool) {
	for _, key := range []string{"usage", "message", "response"} {
		value, ok := mapValue(root, key)
		if !ok {
			continue
		}
		if key == "usage" {
			if usage, ok := value.(map[string]any); ok {
				return usage, true
			}
			continue
		}
		container, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if usage, ok := mapValue(container, "usage"); ok {
			if usageMap, ok := usage.(map[string]any); ok {
				return usageMap, true
			}
		}
	}
	return nil, false
}

func usageFromMap(usage map[string]any) Usage {
	value := func(keys ...string) int64 {
		for _, key := range keys {
			if raw, exists := mapValue(usage, key); exists {
				if n, ok := integerValue(raw); ok {
					return n
				}
			}
		}
		return 0
	}
	u := Usage{CacheReadInputTokens: value("cache_read_input_tokens", "cache_read_tokens", "cacheReadInputTokens", "cacheReadTokens"), CacheCreationInputTokens: value("cache_creation_input_tokens", "cache_creation_tokens", "cacheCreationInputTokens", "cacheCreationTokens"), InputTokens: value("input_tokens", "prompt_tokens", "inputTokens", "promptTokens"), OutputTokens: value("output_tokens", "completion_tokens", "outputTokens", "completionTokens"), ReasoningTokens: value("reasoning_tokens", "reasoningTokens")}
	// vLLM and a few OpenAI-compatible gateways put the cached prompt portion
	// inside a token-details object rather than at the top level. Accept both
	// spellings without changing the Anthropic partition semantics.
	if u.CacheReadInputTokens == 0 {
		if details, ok := nestedMap(usage, "prompt_tokens_details", "promptTokensDetails", "input_tokens_details", "inputTokensDetails"); ok {
			u.CacheReadInputTokens = valueFromMap(details, "cached_tokens", "cache_read_input_tokens", "cachedTokens", "cacheReadInputTokens")
		}
	}
	if u.ReasoningTokens == 0 {
		if details, ok := nestedMap(usage, "completion_tokens_details", "completionTokensDetails", "output_tokens_details", "outputTokensDetails"); ok {
			u.ReasoningTokens = valueFromMap(details, "reasoning_tokens", "reasoningTokens")
		}
	}
	// Clamp the explicit partitions before the uncached partition is
	// derived. A negative cache counter must not turn into additional
	// uncached tokens.
	u = sanitizeUsage(u)
	// Anthropic's current response shape reports input_tokens as the uncached
	// portion, while a few gateways expose the complete partition explicitly.
	// Prefer an explicit uncached field whenever it is present; callers derive
	// the missing partition after merging so streaming snapshots that lack
	// cache details do not freeze a stale derived value in place.
	if uncached, ok := firstInteger(usage, "uncached_input_tokens", "uncached_tokens", "input_tokens_uncached", "uncachedInputTokens", "uncachedTokens", "inputTokensUncached"); ok {
		u.explicitUncached = true
		u.UncachedInputTokens = uncached
		if u.UncachedInputTokens < 0 {
			u.UncachedInputTokens = 0
		}
	}
	return u
}

func mapValue(values map[string]any, key string) (any, bool) {
	if values == nil {
		return nil, false
	}
	if value, ok := values[key]; ok {
		return value, true
	}
	wanted := normalizeKey(key)
	for candidate, value := range values {
		if normalizeKey(candidate) == wanted {
			return value, true
		}
	}
	return nil, false
}

func nestedMap(values map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := mapValue(values, key); ok {
			if nested, ok := value.(map[string]any); ok {
				return nested, true
			}
		}
	}
	return nil, false
}

func valueFromMap(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := mapValue(values, key); ok {
			if number, ok := integerValue(value); ok {
				return number
			}
		}
	}
	return 0
}

func normalizeKey(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func finalizeUsage(u Usage) Usage {
	// Usage is supplied by an upstream process and must not be allowed to
	// poison activity aggregates with negative values or an overflowing
	// denominator. A malformed counter is treated as unknown/zero; this keeps
	// cache hit ratios bounded while preserving the valid partitions.
	u = sanitizeUsage(u)
	denominator := saturatingAdd(u.CacheReadInputTokens, u.CacheCreationInputTokens, u.UncachedInputTokens)
	if denominator > 0 {
		u.HitRatio = float64(u.CacheReadInputTokens) / float64(denominator)
		if u.HitRatio < 0 {
			u.HitRatio = 0
		} else if u.HitRatio > 1 {
			u.HitRatio = 1
		}
		u.CreationRatio = float64(u.CacheCreationInputTokens) / float64(denominator)
		if u.CreationRatio < 0 {
			u.CreationRatio = 0
		} else if u.CreationRatio > 1 {
			u.CreationRatio = 1
		}
	} else {
		u.HitRatio = 0
		u.CreationRatio = 0
	}
	return u
}

func sanitizeUsage(u Usage) Usage {
	if u.CacheReadInputTokens < 0 {
		u.CacheReadInputTokens = 0
	}
	if u.CacheCreationInputTokens < 0 {
		u.CacheCreationInputTokens = 0
	}
	if u.InputTokens < 0 {
		u.InputTokens = 0
	}
	if u.OutputTokens < 0 {
		u.OutputTokens = 0
	}
	if u.ReasoningTokens < 0 {
		u.ReasoningTokens = 0
	}
	if u.UncachedInputTokens < 0 {
		u.UncachedInputTokens = 0
	}
	return u
}

func saturatingAdd(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func mergeUsage(total, candidate Usage) Usage {
	if candidate.CacheReadInputTokens > total.CacheReadInputTokens {
		total.CacheReadInputTokens = candidate.CacheReadInputTokens
	}
	if candidate.CacheCreationInputTokens > total.CacheCreationInputTokens {
		total.CacheCreationInputTokens = candidate.CacheCreationInputTokens
	}
	if candidate.InputTokens > total.InputTokens {
		total.InputTokens = candidate.InputTokens
	}
	if candidate.OutputTokens > total.OutputTokens {
		total.OutputTokens = candidate.OutputTokens
	}
	if candidate.ReasoningTokens > total.ReasoningTokens {
		total.ReasoningTokens = candidate.ReasoningTokens
	}
	// A derived uncached partition reflects only the event that reported it:
	// an intermediate streaming snapshot without cache details derives the
	// full prompt as uncached, which would stay inflated after the final
	// event's cache read arrives. Only an explicitly reported partition is
	// safe to merge; the derived value is recomputed from the merged totals
	// after all events are processed.
	if candidate.explicitUncached {
		total.explicitUncached = true
		if candidate.UncachedInputTokens > total.UncachedInputTokens {
			total.UncachedInputTokens = candidate.UncachedInputTokens
		}
	}
	return total
}

func forEachSSEJSON(body []byte, fn func(map[string]any)) {
	if fn == nil {
		return
	}
	var eventData bytes.Buffer
	flush := func() {
		data := bytes.TrimSpace(eventData.Bytes())
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			eventData.Reset()
			return
		}
		var event map[string]any
		if json.Unmarshal(data, &event) == nil && event != nil {
			fn(event)
		}
		eventData.Reset()
	}
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl < 0 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			flush()
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		// Some gateways omit the blank line between complete JSON events. Do
		// not concatenate two valid objects into invalid JSON.
		if eventData.Len() > 0 {
			if existing := bytes.TrimSpace(eventData.Bytes()); json.Valid(existing) {
				flush()
			}
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			flush()
			continue
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.Write(payload)
	}
	flush()
}

func firstInteger(values map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if value, exists := mapValue(values, key); exists {
			if number, ok := integerValue(value); ok {
				return number, true
			}
		}
	}
	return 0, false
}

func integerValue(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < float64(math.MinInt64) || n >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(n), true
	case float32:
		f := float64(n)
		if math.IsNaN(f) || math.IsInf(f, 0) || f < float64(math.MinInt64) || f >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		number, err := n.Int64()
		if err == nil {
			return number, true
		}
		// Some gateways serialize counters as a JSON decimal (for example
		// "12.0"). Accept only finite integral values; fractional tokens and
		// non-numeric strings remain unknown.
		f, floatErr := strconv.ParseFloat(string(n), 64)
		if floatErr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < float64(math.MinInt64) || f >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(f), true
	case string:
		text := strings.TrimSpace(n)
		if text == "" {
			return 0, false
		}
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			return number, true
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < float64(math.MinInt64) || f >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// DefaultMaxSessionEntries bounds the in-memory session accumulator. Session
// IDs are deliberately not persisted or exposed as metric labels; bounding the
// map also prevents an untrusted client from growing process memory forever by
// sending a new ID on every request.
const DefaultMaxSessionEntries = 10000

// maxSessionIDBytes bounds the key retained for one in-memory session. A
// count limit alone is insufficient when an untrusted client can submit a
// multi-megabyte session header on every request; oversized identifiers are
// folded to a deterministic digest so accounting remains stable without
// retaining attacker-controlled payloads.
const maxSessionIDBytes = 512

// SessionUsage accumulates cache counters by client session without exposing
// the session identifier as a Prometheus label. It is safe for concurrent
// response recorders and can be discarded with the rest of in-memory state.
// The zero value is ready to use and keeps the most recently active
// DefaultMaxSessionEntries sessions. When the bound is reached, the least
// recently updated session is evicted and starts fresh if it returns later.
type SessionUsage struct {
	mu       sync.Mutex
	data     map[string]Usage
	lastSeen map[string]uint64
	sequence uint64
	max      int
}

// NewSessionUsage constructs a bounded session accumulator. A non-positive
// limit selects DefaultMaxSessionEntries. The constructor is optional; the
// zero value of SessionUsage has the same default bound.
func NewSessionUsage(maxEntries int) *SessionUsage {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxSessionEntries
	}
	return &SessionUsage{max: maxEntries}
}

func (s *SessionUsage) Add(session string, usage Usage) Usage {
	session = strings.TrimSpace(session)
	if s == nil || session == "" {
		return finalizeUsage(usage)
	}
	if len(session) > maxSessionIDBytes {
		digest := sha256.Sum256([]byte(session))
		session = "sha256:" + hex.EncodeToString(digest[:])
	}
	usage = finalizeUsage(usage)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[string]Usage)
	}
	if s.lastSeen == nil {
		s.lastSeen = make(map[string]uint64)
	}
	if s.max <= 0 {
		s.max = DefaultMaxSessionEntries
	}
	if _, exists := s.data[session]; !exists && len(s.data) >= s.max {
		s.evictOldestLocked()
	}
	current := s.data[session]
	current.CacheReadInputTokens = saturatingAdd(current.CacheReadInputTokens, usage.CacheReadInputTokens)
	current.CacheCreationInputTokens = saturatingAdd(current.CacheCreationInputTokens, usage.CacheCreationInputTokens)
	current.InputTokens = saturatingAdd(current.InputTokens, usage.InputTokens)
	current.OutputTokens = saturatingAdd(current.OutputTokens, usage.OutputTokens)
	current.ReasoningTokens = saturatingAdd(current.ReasoningTokens, usage.ReasoningTokens)
	current.UncachedInputTokens = saturatingAdd(current.UncachedInputTokens, usage.UncachedInputTokens)
	current = finalizeUsage(current)
	s.data[session] = current
	s.sequence++
	// A uint64 wrap is not reachable in practice, but preserving ordering is
	// cheap and keeps the invariant explicit for long-lived processes/tests.
	if s.sequence == 0 {
		var next uint64
		for key := range s.data {
			next++
			s.lastSeen[key] = next
		}
		s.sequence = next
	}
	s.lastSeen[session] = s.sequence
	return current
}

func (s *SessionUsage) evictOldestLocked() {
	if len(s.data) == 0 {
		return
	}
	var oldest string
	var oldestSequence uint64
	for session := range s.data {
		seen := s.lastSeen[session]
		if oldest == "" || seen < oldestSequence || (seen == oldestSequence && session < oldest) {
			oldest, oldestSequence = session, seen
		}
	}
	delete(s.data, oldest)
	delete(s.lastSeen, oldest)
}

// Count reports the number of retained sessions without exposing their IDs.
// It is intended for bounded diagnostics and tests.
func (s *SessionUsage) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}
