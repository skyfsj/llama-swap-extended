// Package responses contains the protocol conversion path used by the
// Responses adapter middleware. It is a Go port of the CC Switch
// "Codex ↔ Chat Completions" transform (src-tauri/src/proxy/providers/
// transform_codex_chat.rs, streaming_codex_chat.rs and their helper modules),
// adapted to llama-swap's JSON-value style: Responses and Chat evolve
// independently and the conversion works on decoded JSON values instead of
// framework structs so unknown fields survive a native pass-through.
//
// The conversion covers the bridge where a client speaks the OpenAI Responses
// API while the upstream only exposes Chat Completions:
//
//	Responses create request  ->  Chat Completions request  (ResponsesToChat)
//	Chat Completions response ->  Responses response         (ChatResponseToResponses)
//	Chat Completions SSE      ->  Responses SSE              (SSEConverter)
package responses

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Canonical JSON helpers (CC Switch proxy/json_canonical.rs)
// ---------------------------------------------------------------------------

// canonicalJSONString serializes a JSON value with object keys recursively
// sorted and no insignificant whitespace. Identical payloads therefore keep
// byte-identical tool content, which keeps upstream prompt caches stable even
// though Go map iteration order is randomized.
func canonicalJSONString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		// json.Marshal of decoded-JSON values cannot fail; fall back to a
		// stable placeholder rather than panicking on hand-built values.
		return "null"
	}
	return string(data)
}

// canonicalizeJSONStringIfParseable canonicalizes a string that is itself a
// JSON document while leaving plain text untouched. Whitespace-only input is
// returned verbatim.
func canonicalizeJSONStringIfParseable(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return value
	}
	return canonicalJSONString(parsed)
}

// canonicalizeToolArgumentsStr normalizes a tool-call arguments string into a
// valid JSON payload. An empty value is coerced to "{}": strict upstreams such
// as MiniMax reject arguments: "" with 400 invalid function arguments, while
// lenient ones treat it as an empty object.
func canonicalizeToolArgumentsStr(value string) string {
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return canonicalizeJSONStringIfParseable(value)
}

// canonicalizeToolArguments normalizes the arguments field of a Responses or
// Chat tool item: strings are canonicalized (empty coerced to "{}"),
// structured values are serialized canonically, and a missing field defaults
// to "{}".
func canonicalizeToolArguments(value any) string {
	switch typed := value.(type) {
	case nil:
		return "{}"
	case string:
		return canonicalizeToolArgumentsStr(typed)
	default:
		return canonicalJSONString(typed)
	}
}

func shortSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:8])
}

// ---------------------------------------------------------------------------
// JSON scalar helpers (serde as_u64 / as_str equivalents)
// ---------------------------------------------------------------------------

func stringOrEmpty(value any) string {
	text, _ := value.(string)
	return text
}

// firstPresentString returns the first key that is present as a string, even
// when the string is empty; call_id:"" must shadow a later id field the way
// serde_json's get().or_else() chain does.
func firstPresentString(values map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, exists := values[key]; exists {
			text, _ := value.(string)
			return text, true
		}
	}
	return "", false
}

// jsonUint64 mirrors serde_json's as_u64 over Go-decoded JSON values.
// Integers decode as float64 and are accepted when integral and
// non-negative; everything else maps to zero, matching the
// .and_then(as_u64).unwrap_or(0) chains in the Rust transform.
func jsonUint64(value any) uint64 {
	parsed, ok := jsonUint64Opt(value)
	if !ok {
		return 0
	}
	return parsed
}

func jsonUint64Opt(value any) (uint64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed >= 18446744073709551616.0 || typed != math.Trunc(typed) {
			return 0, false
		}
		return uint64(typed), true
	case float32:
		return jsonUint64Opt(float64(typed))
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int32:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case uint64:
		return typed, true
	case json.Number:
		parsed, err := strconv.ParseUint(string(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// Reasoning extraction (CC Switch providers/codex_chat_common.rs)
// ---------------------------------------------------------------------------

// extractReasoningFieldText reads the reasoning fields emitted by Chat
// Compatibles upstreams: reasoning_content > reasoning (string or object) >
// reasoning_details. Provider-agnostic on purpose so every Chat-compatible
// backend is covered without provider metadata.
func extractReasoningFieldText(values map[string]any) (string, bool) {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text, ok := values[key].(string); ok && text != "" {
			return text, true
		}
	}
	if reasoning, ok := values["reasoning"].(map[string]any); ok {
		for _, key := range []string{"content", "text", "summary"} {
			if text, ok := reasoning[key].(string); ok && text != "" {
				return text, true
			}
		}
	}
	if details, exists := values["reasoning_details"]; exists && details != nil {
		if text := extractReasoningDetailsText(details); text != "" {
			return text, true
		}
	}
	return "", false
}

func extractReasoningDetailsText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			if text := extractReasoningDetailPartText(raw); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		return extractReasoningDetailPartText(typed)
	default:
		return ""
	}
}

func extractReasoningDetailPartText(value any) string {
	part, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"text", "content", "summary"} {
		if text, ok := part[key].(string); ok && text != "" {
			return text
		}
	}
	if parts, ok := part["parts"].([]any); ok {
		nested := make([]string, 0, len(parts))
		for _, raw := range parts {
			if text := extractReasoningDetailPartText(raw); text != "" {
				nested = append(nested, text)
			}
		}
		return strings.Join(nested, "\n\n")
	}
	return ""
}

// extractReasoningSummaryText reads the reasoning summary of a Responses
// reasoning item: reasoning_content/content/text fields, then the summary
// field in its string or summary_text-part forms.
func extractReasoningSummaryText(item map[string]any) (string, bool) {
	for _, key := range []string{"reasoning_content", "content", "text"} {
		if text, ok := item[key].(string); ok && text != "" {
			return text, true
		}
	}
	switch summary := item["summary"].(type) {
	case string:
		if summary != "" {
			return summary, true
		}
	case []any:
		parts := make([]string, 0, len(summary))
		for _, raw := range summary {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
				if text, ok := part["content"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
			}
			if text, ok := raw.(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		if text := strings.Join(parts, "\n\n"); text != "" {
			return text, true
		}
	}
	return "", false
}

// appendReasoningContent appends reasoning to a Chat message's
// reasoning_content field, separating segments with a blank line.
func appendReasoningContent(message map[string]any, reasoning string) {
	trimmed := strings.TrimSpace(reasoning)
	if trimmed == "" {
		return
	}
	if existing, ok := message["reasoning_content"].(string); ok && existing != "" {
		message["reasoning_content"] = existing + "\n\n" + trimmed
		return
	}
	message["reasoning_content"] = trimmed
}

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// splitLeadingThinkBlock splits a leading <think>...</think> block from the
// assistant answer. It reports false when the text does not carry a complete
// leading block.
func splitLeadingThinkBlock(text string) (reasoning, answer string, ok bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return "", "", false
	}
	bodyStart := len(text) - len(trimmed) + len(thinkOpenTag)
	rest := text[bodyStart:]
	closeIndex := strings.Index(rest, thinkCloseTag)
	if closeIndex < 0 {
		return "", "", false
	}
	reasoning = strings.TrimSpace(rest[:closeIndex])
	answer = strings.TrimLeft(rest[closeIndex+len(thinkCloseTag):], " \t\r\n")
	return reasoning, answer, true
}

// stripLeadingThinkOpenTag removes a leading <think> open tag (used when a
// stream ends while still inside the think block).
func stripLeadingThinkOpenTag(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return "", false
	}
	return strings.TrimSpace(trimmed[len(thinkOpenTag):]), true
}

// ---------------------------------------------------------------------------
// Tool context (CC Switch CodexToolContext)
// ---------------------------------------------------------------------------

type toolKind int

const (
	toolKindFunction toolKind = iota
	toolKindNamespace
	toolKindCustom
	toolKindToolSearch
)

const (
	toolSearchProxyName        = "tool_search"
	customToolInputField       = "input"
	chatToolNameMaxLen         = 64
	customToolInputDescription = "Raw string input for the original custom tool. Preserve formatting exactly and follow the original tool definition embedded in the description."
	customToolMetadataHeading  = "Original tool definition:"
)

type toolSpec struct {
	kind      toolKind
	name      string
	namespace string
}

type namespaceKey struct {
	namespace string
	name      string
}

// ToolContext tracks the Chat-visible tool vocabulary produced from a
// Responses request so the response side can restore Responses-native tool
// items (namespaced functions, custom tools, tool search).
type ToolContext struct {
	chatTools               []any
	seenChatNames           map[string]bool
	chatNameToSpec          map[string]toolSpec
	namespaceNameToChatName map[namespaceKey]string
}

// NewToolContextFromRequestBody builds the context from a decoded Responses
// create request: the tools array plus any tool_search_output items embedded
// in the input history.
func NewToolContextFromRequestBody(body map[string]any) *ToolContext {
	context := &ToolContext{
		seenChatNames:           map[string]bool{},
		chatNameToSpec:          map[string]toolSpec{},
		namespaceNameToChatName: map[namespaceKey]string{},
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, raw := range tools {
			context.addResponseTool(raw)
		}
	}
	collectToolSearchOutputTools(body["input"], context)
	return context
}

// NewToolContextFromRequestBytes parses a Responses create request and builds
// the tool context from it.
func NewToolContextFromRequestBytes(request []byte) *ToolContext {
	var body map[string]any
	if json.Unmarshal(request, &body) != nil {
		return &ToolContext{
			seenChatNames:           map[string]bool{},
			chatNameToSpec:          map[string]toolSpec{},
			namespaceNameToChatName: map[namespaceKey]string{},
		}
	}
	return NewToolContextFromRequestBody(body)
}

// ChatTools returns the converted Chat Completions tool definitions.
func (c *ToolContext) ChatTools() []any {
	if c == nil {
		return nil
	}
	return c.chatTools
}

// LookupChatName resolves the Responses spec behind a flattened Chat name.
func (c *ToolContext) LookupChatName(chatName string) (toolSpec, bool) {
	if c == nil {
		return toolSpec{}, false
	}
	spec, ok := c.chatNameToSpec[chatName]
	return spec, ok
}

// IsCustomToolChatName reports whether chatName was published for a Responses
// custom (freeform) tool.
func (c *ToolContext) IsCustomToolChatName(chatName string) bool {
	spec, ok := c.LookupChatName(chatName)
	return ok && spec.kind == toolKindCustom
}

// ChatNameForResponseFunction maps a Responses function name (optionally
// namespaced) back to the flattened Chat name published for it.
func (c *ToolContext) ChatNameForResponseFunction(name, namespace string) string {
	if namespace != "" {
		if chatName, ok := c.namespaceNameToChatName[namespaceKey{namespace, name}]; ok {
			return chatName
		}
		return flattenNamespaceToolName(namespace, name)
	}
	return name
}

func (c *ToolContext) addChatTool(chatName string, spec toolSpec, chatTool any) {
	if strings.TrimSpace(chatName) == "" || c.seenChatNames[chatName] {
		return
	}
	c.seenChatNames[chatName] = true
	if spec.namespace != "" {
		c.namespaceNameToChatName[namespaceKey{spec.namespace, spec.name}] = chatName
	}
	c.chatNameToSpec[chatName] = spec
	c.chatTools = append(c.chatTools, chatTool)
}

func (c *ToolContext) addFunctionTool(tool map[string]any, namespace string) {
	originalName, ok := responsesToolName(tool)
	if !ok {
		return
	}
	chatName := originalName
	if namespace != "" {
		chatName = flattenNamespaceToolName(namespace, originalName)
	}
	chatTool, ok := responsesFunctionToolToChatTool(tool, chatName)
	if !ok {
		return
	}
	spec := toolSpec{kind: toolKindFunction, name: originalName}
	if namespace != "" {
		spec.kind = toolKindNamespace
		spec.namespace = namespace
	}
	c.addChatTool(chatName, spec, chatTool)
}

func (c *ToolContext) addCustomTool(tool map[string]any) {
	name, ok := responsesToolName(tool)
	if !ok {
		return
	}
	description := customToolMetadataHeading + "\n```json\n" + canonicalJSONString(tool) + "\n```"
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					customToolInputField: map[string]any{
						"type":        "string",
						"description": customToolInputDescription,
					},
				},
				"required": []any{customToolInputField},
			},
		},
	}
	c.addChatTool(name, toolSpec{kind: toolKindCustom, name: name}, chatTool)
}

func (c *ToolContext) addToolSearchTool() {
	chatTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": toolSearchProxyName,
			"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces " +
				"for the current task.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search query for tools or connectors to load.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of tool groups to return.",
					},
				},
				"required": []any{"query"},
			},
		},
	}
	c.addChatTool(toolSearchProxyName, toolSpec{kind: toolKindToolSearch, name: toolSearchProxyName}, chatTool)
}

func (c *ToolContext) addNamespaceTool(namespaceTool map[string]any) {
	namespace, _ := namespaceTool["name"].(string)
	if namespace == "" {
		return
	}
	children, ok := namespaceTool["tools"].([]any)
	if !ok {
		children, ok = namespaceTool["children"].([]any)
	}
	if !ok {
		return
	}
	for _, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if childType, _ := child["type"].(string); childType == "function" {
			c.addFunctionTool(child, namespace)
		}
	}
}

func (c *ToolContext) addResponseTool(tool any) {
	switch typed := tool.(type) {
	case string:
		c.addCustomTool(map[string]any{"type": "custom", "name": typed})
	case map[string]any:
		switch toolType, _ := typed["type"].(string); toolType {
		case "function":
			c.addFunctionTool(typed, "")
		case "custom":
			c.addCustomTool(typed)
		case "tool_search":
			c.addToolSearchTool()
		case "namespace":
			c.addNamespaceTool(typed)
		}
	}
}

// collectToolSearchOutputTools scans the request input recursively for
// tool_search_output items and publishes the tools they loaded, so follow-up
// function calls can be mapped back to their Responses shape.
func collectToolSearchOutputTools(value any, context *ToolContext) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectToolSearchOutputTools(item, context)
		}
	case map[string]any:
		if itemType, _ := typed["type"].(string); itemType == "tool_search_output" {
			if tools, ok := typed["tools"].([]any); ok {
				for _, tool := range tools {
					context.addResponseTool(tool)
				}
			}
		}
		for _, child := range typed {
			collectToolSearchOutputTools(child, context)
		}
	}
}

func responsesToolName(tool map[string]any) (string, bool) {
	if function, ok := tool["function"].(map[string]any); ok {
		if name, ok := function["name"].(string); ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name), true
		}
	}
	if name, ok := tool["name"].(string); ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name), true
	}
	return "", false
}

// flattenNamespaceToolName flattens a namespaced tool name with a stable
// separator because Chat Completions has no namespace field. Long names are
// truncated with a hash suffix so the result stays within the 64-character
// limit accepted by Chat upstreams while remaining collision-free.
func flattenNamespaceToolName(namespace, name string) string {
	fullName := namespace + "__" + name
	if len(fullName) <= chatToolNameMaxLen {
		return fullName
	}
	suffix := "__" + shortSHA256Hex([]byte(fullName))
	prefixLen := chatToolNameMaxLen - len(suffix)
	prefix := make([]rune, 0, prefixLen)
	used := 0
	for _, ch := range fullName {
		size := utf8RuneLen(ch)
		if used+size > prefixLen {
			break
		}
		prefix = append(prefix, ch)
		used += size
	}
	return string(prefix) + suffix
}

func utf8RuneLen(ch rune) int {
	switch {
	case ch < 0x80:
		return 1
	case ch < 0x800:
		return 2
	case ch < 0x10000:
		return 3
	default:
		return 4
	}
}

// normalizeFunctionParameters forces a function tool's parameters schema to
// type "object": some Responses tools carry parameters: null or
// {"type": null}, but Chat Completions requires an object schema.
func normalizeFunctionParameters(params any) any {
	out, ok := params.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	normalized := make(map[string]any, len(out)+1)
	for key, value := range out {
		normalized[key] = value
	}
	if typeName, _ := normalized["type"].(string); typeName != "object" {
		normalized["type"] = "object"
	}
	return normalized
}

func responsesFunctionToolToChatTool(tool map[string]any, chatName string) (any, bool) {
	if toolType, _ := tool["type"].(string); toolType != "function" {
		return nil, false
	}
	if function, ok := tool["function"].(map[string]any); ok {
		chatFunction := make(map[string]any, len(function)+2)
		for key, value := range function {
			chatFunction[key] = value
		}
		chatFunction["parameters"] = normalizeFunctionParameters(chatFunction["parameters"])
		chatFunction["name"] = chatName
		if strict, exists := tool["strict"]; exists {
			if _, hasStrict := chatFunction["strict"]; !hasStrict {
				chatFunction["strict"] = strict
			}
		}
		return map[string]any{"type": "function", "function": chatFunction}, true
	}
	chatFunction := map[string]any{
		"name":        chatName,
		"description": tool["description"],
		"parameters":  normalizeFunctionParameters(tool["parameters"]),
	}
	if strict, exists := tool["strict"]; exists {
		chatFunction["strict"] = strict
	}
	return map[string]any{"type": "function", "function": chatFunction}, true
}

// toolChoiceToChat maps a Responses tool_choice to the Chat Completions form,
// restoring the flattened Chat name for namespaced/custom/tool_search
// selections. Unrecognized forms pass through unchanged.
func (c *ToolContext) toolChoiceToChat(toolChoice any) any {
	object, ok := toolChoice.(map[string]any)
	if !ok {
		return toolChoice
	}
	switch choiceType, _ := object["type"].(string); choiceType {
	case "function":
		name, _ := object["name"].(string)
		namespace, _ := object["namespace"].(string)
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": c.ChatNameForResponseFunction(name, namespace)},
		}
	case "tool_search":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": toolSearchProxyName},
		}
	case "custom":
		name, _ := object["name"].(string)
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}
	default:
		return toolChoice
	}
}

// ---------------------------------------------------------------------------
// Request conversion: Responses -> Chat Completions
// ---------------------------------------------------------------------------

// extraChatPassthroughFields is the allowlist of top-level fields forwarded
// to the Chat request unchanged. Everything else (previous_response_id,
// reasoning objects, text controls, store flags, ...) is dropped: strict
// OpenAI-compatible upstreams reject unknown or Responses-only fields.
var extraChatPassthroughFields = []string{
	"frequency_penalty",
	"logit_bias",
	"logprobs",
	"metadata",
	"n",
	"parallel_tool_calls",
	"presence_penalty",
	"response_format",
	"seed",
	"service_tier",
	"stop",
	"stream_options",
	"top_logprobs",
	"user",
}

// ResponsesToChat converts a Responses create request to a Chat Completions
// request following the CC Switch Codex adapter semantics.
func ResponsesToChat(input []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(input, &src); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	out, err := RequestValueToChat(src)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func RequestValueToChat(src map[string]any) (map[string]any, error) {
	toolContext := NewToolContextFromRequestBody(src)
	out := map[string]any{}

	model, hasModel := src["model"]
	if !hasModel {
		return nil, errors.New("responses request requires model")
	}
	out["model"] = model
	modelName := stringOrEmpty(model)

	var messages []any
	if instructions, ok := src["instructions"]; ok {
		if text := instructionText(instructions); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	input, hasInput := src["input"]
	if !hasInput {
		return nil, errors.New("responses request requires input")
	}
	pipeline := &requestPipeline{toolContext: toolContext, lastAssistantIndex: -1}
	pipeline.appendInput(input)
	messages = append(messages, pipeline.messages...)
	messages = collapseSystemMessagesToHead(messages)
	out["messages"] = messages

	if maxOutput, ok := src["max_output_tokens"]; ok {
		if isOpenAIOSeries(modelName) {
			out["max_completion_tokens"] = maxOutput
		} else {
			out["max_tokens"] = maxOutput
		}
	}
	// Defensive passthrough: the Responses API does not define these fields,
	// but non-Codex clients may already send Chat-style limit fields.
	if maxTokens, ok := src["max_tokens"]; ok {
		out["max_tokens"] = maxTokens
	}
	if maxCompletion, ok := src["max_completion_tokens"]; ok {
		out["max_completion_tokens"] = maxCompletion
	}
	for _, key := range []string{"temperature", "top_p", "stream"} {
		if value, ok := src[key]; ok {
			out[key] = value
		}
	}
	if effort, ok := reasoningEffortRequested(src); ok && supportsReasoningEffort(modelName) {
		out["reasoning_effort"] = effort
	}

	if tools := toolContext.ChatTools(); len(tools) > 0 {
		out["tools"] = tools
	}
	if toolChoice, ok := src["tool_choice"]; ok {
		out["tool_choice"] = toolContext.toolChoiceToChat(toolChoice)
	}
	for _, key := range extraChatPassthroughFields {
		if value, ok := src[key]; ok {
			out[key] = value
		}
	}

	// Strict OpenAI-compatible upstreams (vLLM, enterprise gateways) reject
	// requests carrying tool_choice or parallel_tool_calls without a
	// non-empty tools array. Drop both when tools ended up absent or empty
	// after conversion to avoid 503/400 from such providers.
	tools, hasTools := out["tools"].([]any)
	if !hasTools || len(tools) == 0 {
		delete(out, "tool_choice")
		delete(out, "parallel_tool_calls")
	}
	injectOpenAIStreamIncludeUsage(out)
	return out, nil
}

func reasoningEffortRequested(src map[string]any) (string, bool) {
	reasoning, ok := src["reasoning"].(map[string]any)
	if !ok {
		return "", false
	}
	effort, ok := reasoning["effort"].(string)
	if !ok || strings.TrimSpace(effort) == "" {
		return "", false
	}
	return effort, true
}

// isOpenAIOSeries detects OpenAI o-series model names (o1, o3, o4-mini, ...).
func isOpenAIOSeries(model string) bool {
	if len(model) < 2 || model[0] != 'o' {
		return false
	}
	return model[1] >= '0' && model[1] <= '9'
}

// supportsReasoningEffort detects models whose Chat Completions API accepts a
// reasoning_effort field: OpenAI o-series, GPT-5+ and xAI Grok Build models.
func supportsReasoningEffort(model string) bool {
	normalized := strings.ToLower(model)
	if isOpenAIOSeries(normalized) {
		return true
	}
	if rest, ok := strings.CutPrefix(normalized, "gpt-"); ok {
		if len(rest) > 0 && rest[0] >= '5' && rest[0] <= '9' {
			return true
		}
	}
	return normalized == "grok-4.5" ||
		strings.HasPrefix(normalized, "grok-4.5-") ||
		strings.HasPrefix(normalized, "grok-build-")
}

// injectOpenAIStreamIncludeUsage makes sure streaming requests ask for usage:
// OpenAI-compatible upstreams omit usage chunks unless include_usage is set,
// which would silently zero token accounting for the converted stream.
func injectOpenAIStreamIncludeUsage(body map[string]any) {
	stream, _ := body["stream"].(bool)
	if !stream {
		return
	}
	if options, ok := body["stream_options"].(map[string]any); ok {
		options["include_usage"] = true
		return
	}
	body["stream_options"] = map[string]any{"include_usage": true}
}

// collapseSystemMessagesToHead merges all string-content system messages into
// a single head message. MiniMax strictly requires role=system to appear only
// in the first message (error 2013); the reordering is lossless for lenient
// upstreams such as OpenAI or DeepSeek.
func collapseSystemMessagesToHead(messages []any) []any {
	var systemChunks []string
	rest := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if ok {
			if role, _ := message["role"].(string); role == "system" {
				if text, ok := message["content"].(string); ok {
					if strings.TrimSpace(text) != "" {
						systemChunks = append(systemChunks, text)
					}
					continue
				}
			}
		}
		rest = append(rest, raw)
	}
	out := make([]any, 0, len(rest)+1)
	if len(systemChunks) > 0 {
		out = append(out, map[string]any{"role": "system", "content": strings.Join(systemChunks, "\n\n")})
	}
	return append(out, rest...)
}

func instructionText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
			}
			if text, ok := raw.(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Input item pipeline: Responses items -> Chat messages
// ---------------------------------------------------------------------------

type requestPipeline struct {
	messages         []any
	pendingToolCalls []any
	pendingReasoning string
	// lastAssistantIndex is the index of the most recent assistant message,
	// or -1. It is the back-attachment target for trailing reasoning.
	lastAssistantIndex int
	toolContext        *ToolContext
}

func (p *requestPipeline) appendInput(input any) {
	switch typed := input.(type) {
	case string:
		p.messages = append(p.messages, map[string]any{"role": "user", "content": typed})
	case []any:
		for _, raw := range typed {
			if item, ok := raw.(map[string]any); ok {
				p.appendItem(item)
				continue
			}
			// Non-object items are inert: they close a pending tool-call
			// batch, mirroring the catch-all arm of the Rust pipeline.
			p.flushPendingToolCalls()
		}
	case map[string]any:
		p.appendItem(typed)
	}
	p.flushPendingToolCalls()
	p.attachPendingReasoningToPreviousAssistant()
	p.backfillToolCallReasoningPlaceholders()
}

func (p *requestPipeline) appendItem(item map[string]any) {
	itemType, _ := item["type"].(string)
	switch itemType {
	case "function_call":
		if reasoning, ok := extractReasoningFieldText(item); ok {
			appendUniquePendingReasoning(&p.pendingReasoning, reasoning)
		}
		p.pendingToolCalls = append(p.pendingToolCalls, p.responsesFunctionCallToChatToolCall(item))
	case "custom_tool_call":
		if reasoning, ok := extractReasoningFieldText(item); ok {
			appendUniquePendingReasoning(&p.pendingReasoning, reasoning)
		}
		p.pendingToolCalls = append(p.pendingToolCalls, responsesCustomToolCallToChatToolCall(item))
	case "tool_search_call":
		if reasoning, ok := extractReasoningFieldText(item); ok {
			appendUniquePendingReasoning(&p.pendingReasoning, reasoning)
		}
		p.pendingToolCalls = append(p.pendingToolCalls, responsesToolSearchCallToChatToolCall(item))
	case "function_call_output":
		p.flushPendingToolCalls()
		callID, _ := item["call_id"].(string)
		p.messages = append(p.messages, map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      canonicalToolOutputContent(item["output"]),
		})
	case "custom_tool_call_output", "tool_search_output":
		p.flushPendingToolCalls()
		callID, _ := item["call_id"].(string)
		p.messages = append(p.messages, map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      canonicalJSONString(item),
		})
	case "reasoning":
		// Reasoning always enters the pending buffer first and is attached
		// forward to the following message or function-call batch. Attaching
		// it directly to the previous assistant here would splice a new
		// turn's thinking into an old message.
		if reasoning, ok := extractReasoningSummaryText(item); ok {
			appendPendingReasoning(&p.pendingReasoning, reasoning)
		}
	case "input_text", "input_image", "input_file", "input_audio":
		p.flushPendingToolCalls()
		role, _ := item["role"].(string)
		chatRole := responsesRoleToChatRole(role)
		message := map[string]any{
			"role":    chatRole,
			"content": responsesContentToChatContent([]any{item}),
		}
		if chatRole == "assistant" {
			p.attachPendingReasoningToAssistant(message)
		} else {
			// A turn boundary (user message) must not let pending reasoning
			// leak into a later assistant message; attach it backwards.
			p.attachPendingReasoningToPreviousAssistant()
		}
		p.updateLastAssistantIndex(message)
		p.messages = append(p.messages, message)
	default:
		_, hasRole := item["role"]
		_, hasContent := item["content"]
		if hasRole || hasContent {
			p.flushPendingToolCalls()
			message := p.responsesMessageItemToChatMessage(item)
			p.updateLastAssistantIndex(message)
			p.messages = append(p.messages, message)
			return
		}
		// Inert item: closes a pending tool-call batch (legacy ordering).
		p.flushPendingToolCalls()
	}
}

func (p *requestPipeline) responsesMessageItemToChatMessage(item map[string]any) map[string]any {
	role, _ := item["role"].(string)
	chatRole := responsesRoleToChatRole(role)
	var content any
	if value, exists := item["content"]; exists {
		content = responsesContentToChatContent(value)
	}
	message := map[string]any{"role": chatRole, "content": content}
	if chatRole == "assistant" {
		if reasoning, ok := extractReasoningFieldText(item); ok {
			appendPendingReasoning(&p.pendingReasoning, reasoning)
		}
		p.attachPendingReasoningToAssistant(message)
	} else {
		p.attachPendingReasoningToPreviousAssistant()
	}
	return message
}

func (p *requestPipeline) responsesFunctionCallToChatToolCall(item map[string]any) map[string]any {
	callID, _ := firstPresentString(item, "call_id", "id")
	name, _ := item["name"].(string)
	namespace, _ := item["namespace"].(string)
	chatName := p.toolContext.ChatNameForResponseFunction(name, namespace)
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      chatName,
			"arguments": canonicalizeToolArguments(item["arguments"]),
		},
	}
}

func responsesCustomToolCallToChatToolCall(item map[string]any) map[string]any {
	callID, _ := firstPresentString(item, "call_id", "id")
	name, _ := item["name"].(string)
	input, hasInput := item["input"]
	if !hasInput {
		input = ""
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": canonicalJSONString(map[string]any{customToolInputField: input}),
		},
	}
}

func responsesToolSearchCallToChatToolCall(item map[string]any) map[string]any {
	callID, _ := firstPresentString(item, "call_id", "id")
	arguments := "{}"
	if raw, exists := item["arguments"]; exists {
		arguments = canonicalJSONString(raw)
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      toolSearchProxyName,
			"arguments": arguments,
		},
	}
}

// canonicalToolOutputContent keeps tool results valid for Chat adapters that
// only accept string content: JSON strings are canonicalized (cache-stable),
// structured values are encoded as canonical JSON.
func canonicalToolOutputContent(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return canonicalizeJSONStringIfParseable(typed)
	default:
		return canonicalJSONString(typed)
	}
}

// flushPendingToolCalls emits accumulated tool calls as an assistant message.
// One Responses turn can carry commentary text directly followed by function
// calls; keeping them on one Chat assistant message stops a standalone
// text-only assistant turn from teaching the model to stop before the tool
// call.
func (p *requestPipeline) flushPendingToolCalls() {
	if len(p.pendingToolCalls) == 0 {
		return
	}
	if p.mergePendingToolCallsIntoAdjacentAssistant() {
		p.lastAssistantIndex = len(p.messages) - 1
		return
	}
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": p.pendingToolCalls}
	p.pendingToolCalls = nil
	p.attachPendingReasoningToAssistant(message)
	p.lastAssistantIndex = len(p.messages)
	p.messages = append(p.messages, message)
}

func (p *requestPipeline) mergePendingToolCallsIntoAdjacentAssistant() bool {
	if len(p.messages) == 0 {
		return false
	}
	message, ok := p.messages[len(p.messages)-1].(map[string]any)
	if !ok {
		return false
	}
	if role, _ := message["role"].(string); role != "assistant" {
		return false
	}
	if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
		return false
	}
	message["tool_calls"] = p.pendingToolCalls
	p.pendingToolCalls = nil
	attachPendingReasoningToAssistantUnique(message, &p.pendingReasoning)
	return true
}

func (p *requestPipeline) attachPendingReasoningToAssistant(message map[string]any) {
	if p.pendingReasoning == "" {
		return
	}
	reasoning := p.pendingReasoning
	p.pendingReasoning = ""
	appendReasoningContent(message, reasoning)
}

func attachPendingReasoningToAssistantUnique(message map[string]any, pending *string) {
	if *pending == "" {
		return
	}
	reasoning := strings.TrimSpace(*pending)
	*pending = ""
	if reasoning == "" {
		return
	}
	existing, _ := message["reasoning_content"].(string)
	existingSegments := splitReasoningSegments(existing)
	missing := make([]string, 0)
	for _, segment := range splitReasoningSegments(reasoning) {
		if !containsString(existingSegments, segment) {
			missing = append(missing, segment)
		}
	}
	if len(missing) == 0 {
		return
	}
	if len(existingSegments) == 0 {
		message["reasoning_content"] = strings.Join(missing, "\n\n")
		return
	}
	message["reasoning_content"] = strings.Join(existingSegments, "\n\n") + "\n\n" + strings.Join(missing, "\n\n")
}

func splitReasoningSegments(text string) []string {
	segments := strings.Split(text, "\n\n")
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		if trimmed := strings.TrimSpace(segment); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func appendPendingReasoning(pending *string, reasoning string) {
	trimmed := strings.TrimSpace(reasoning)
	if trimmed == "" {
		return
	}
	if *pending != "" {
		*pending += "\n\n" + trimmed
		return
	}
	*pending = trimmed
}

func appendUniquePendingReasoning(pending *string, reasoning string) {
	trimmed := strings.TrimSpace(reasoning)
	if trimmed == "" {
		return
	}
	if strings.Contains(*pending, trimmed) {
		return
	}
	if *pending != "" {
		*pending += "\n\n" + trimmed
		return
	}
	*pending = trimmed
}

// attachPendingReasoningToPreviousAssistant back-attaches still-pending
// reasoning to the previous assistant message. It only runs at genuine tail
// or turn-boundary points so reasoning can neither leak across a user turn
// nor be silently dropped.
func (p *requestPipeline) attachPendingReasoningToPreviousAssistant() {
	if p.pendingReasoning == "" {
		return
	}
	reasoning := p.pendingReasoning
	p.pendingReasoning = ""
	if p.lastAssistantIndex < 0 || p.lastAssistantIndex >= len(p.messages) {
		return
	}
	message, ok := p.messages[p.lastAssistantIndex].(map[string]any)
	if !ok {
		return
	}
	if role, _ := message["role"].(string); role != "assistant" {
		return
	}
	appendReasoningContent(message, reasoning)
}

// backfillToolCallReasoningPlaceholders ensures every assistant tool-call
// message carries non-empty reasoning_content. kimi/Moonshot and DeepSeek
// thinking models reject assistant tool-call messages without it
// ("reasoning_content is missing in assistant tool call message") when the
// original reasoning could not be recovered across turns. It must run as the
// final pipeline step: real reasoning may still arrive as a trailing
// reasoning item back-attached above.
func (p *requestPipeline) backfillToolCallReasoningPlaceholders() {
	for _, raw := range p.messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		calls, ok := message["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		if text, _ := message["reasoning_content"].(string); strings.TrimSpace(text) != "" {
			continue
		}
		message["reasoning_content"] = "tool call"
	}
}

func (p *requestPipeline) updateLastAssistantIndex(message map[string]any) {
	switch role, _ := message["role"].(string); role {
	case "assistant":
		p.lastAssistantIndex = len(p.messages)
	case "tool":
	default:
		p.lastAssistantIndex = -1
	}
}

func responsesRoleToChatRole(role string) string {
	switch role {
	case "system", "developer":
		return "system"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool"
	default:
		return "user"
	}
}

// responsesContentToChatContent maps Responses content parts to Chat parts.
// Unknown parts are skipped; text-only content collapses to a plain string.
func responsesContentToChatContent(content any) any {
	switch typed := content.(type) {
	case nil, string:
		return content
	case []any:
		parts := typed
		chatParts := make([]any, 0, len(parts))
		hasNonTextPart := false
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch partType, _ := part["type"].(string); partType {
			case "input_text", "output_text", "text":
				if text, ok := part["text"].(string); ok && text != "" {
					chatParts = append(chatParts, map[string]any{"type": "text", "text": text})
				}
			case "refusal":
				if text, ok := part["refusal"].(string); ok && text != "" {
					chatParts = append(chatParts, map[string]any{"type": "text", "text": text})
				}
			case "input_image":
				imageURL, exists := part["image_url"]
				if !exists {
					continue
				}
				switch imageValue := imageURL.(type) {
				case map[string]any:
					imageURL = imageValue
				case string:
					imageURL = map[string]any{"url": imageValue}
				default:
					imageURL = map[string]any{"url": ""}
				}
				chatParts = append(chatParts, map[string]any{"type": "image_url", "image_url": imageURL})
				hasNonTextPart = true
			case "input_file":
				file, ok := chatFileFromInputFile(part)
				if !ok {
					continue
				}
				chatParts = append(chatParts, map[string]any{"type": "file", "file": file})
				hasNonTextPart = true
			case "input_audio":
				inputAudio, exists := part["input_audio"]
				if !exists {
					continue
				}
				chatParts = append(chatParts, map[string]any{"type": "input_audio", "input_audio": inputAudio})
				hasNonTextPart = true
			}
		}
		if !hasNonTextPart {
			texts := make([]string, 0, len(chatParts))
			for _, raw := range chatParts {
				part := raw.(map[string]any)
				if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			}
			return strings.Join(texts, "\n")
		}
		return chatParts
	default:
		return content
	}
}

// chatFileFromInputFile converts a Responses input_file part to the Chat file
// part. Only file_id/file_data references are representable; URL-only parts
// are skipped.
func chatFileFromInputFile(part map[string]any) (any, bool) {
	_, hasFileID := part["file_id"]
	_, hasFileData := part["file_data"]
	if !hasFileID && !hasFileData {
		return nil, false
	}
	file := map[string]any{}
	for _, key := range []string{"file_id", "file_data", "filename"} {
		if value, exists := part[key]; exists {
			file[key] = value
		}
	}
	return file, true
}

// ---------------------------------------------------------------------------
// Response conversion: Chat Completions -> Responses (non-streaming)
// ---------------------------------------------------------------------------

// ChatResponseToResponses converts one non-streaming Chat response into a
// Responses response without tool restoration context.
func ChatResponseToResponses(input []byte) ([]byte, error) {
	return ChatResponseToResponsesWithContext(input, nil)
}

// ChatResponseToResponsesWithContext converts one non-streaming Chat response
// into a Responses response, restoring namespaced/custom/tool-search items
// using the tool context built from the original Responses request.
func ChatResponseToResponsesWithContext(input []byte, toolContext *ToolContext) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(input, &src); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	out, err := chatResponseValueToResponsesWithContext(src, toolContext)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// ChatResponseValueToResponses converts an already decoded Chat response.
func ChatResponseValueToResponses(src map[string]any) (map[string]any, error) {
	return chatResponseValueToResponsesWithContext(src, nil)
}

func chatResponseValueToResponsesWithContext(src map[string]any, toolContext *ToolContext) (map[string]any, error) {
	choices, ok := src["choices"].([]any)
	if !ok {
		return nil, errors.New("no choices in chat response")
	}
	if len(choices) == 0 {
		return nil, errors.New("empty choices in chat response")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, errors.New("no message in chat choice")
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return nil, errors.New("no message in chat choice")
	}

	responseID := responseIDFromChatID(stringOrEmpty(src["id"]))
	model := stringOrEmpty(src["model"])
	createdAt := jsonUint64(src["created"])
	finishReason := stringOrEmpty(choice["finish_reason"])

	reasoning := chatReasoningText(message)
	output := make([]any, 0, 3)
	if reasoning != "" {
		output = append(output, map[string]any{
			"id":   "rs_" + responseID,
			"type": "reasoning",
			"summary": []any{map[string]any{
				"type": "summary_text", "text": reasoning,
			}},
		})
	}
	if messageItem := chatMessageToResponseOutputItem(message, responseID); messageItem != nil {
		output = append(output, messageItem)
	}
	toolItems, dropped := chatToolCallsToResponseOutputItems(message, reasoning, toolContext)

	// When every tool call was dropped for a missing name and nothing else
	// remains, the client would see a "completed" turn with no tool call and
	// silently end its agent loop. Report the failure honestly instead. Only
	// applies to turns that should have completed: finish_reason=length is a
	// truncation, not malformed upstream data.
	if responseStatusFromFinishReason(finishReason) == "completed" && dropped > 0 && len(toolItems) == 0 {
		return nil, fmt.Errorf(
			"upstream returned %d tool call(s) without a function name, leaving no usable tool call in this turn",
			dropped)
	}
	output = append(output, toolItems...)

	status := responseStatusFromFinishReason(finishReason)
	response := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     status,
		"model":      model,
		"output":     output,
		"usage":      chatUsageToResponsesUsage(src["usage"]),
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": responseIncompleteReason(finishReason)}
	}
	return response, nil
}

func chatReasoningText(message map[string]any) string {
	if reasoning, ok := extractReasoningFieldText(message); ok {
		return reasoning
	}
	if content, ok := message["content"].(string); ok {
		if reasoning, _, ok := splitLeadingThinkBlock(content); ok && reasoning != "" {
			return reasoning
		}
	}
	return ""
}

func chatMessageToResponseOutputItem(message map[string]any, responseID string) map[string]any {
	content := make([]any, 0, 2)
	switch typed := message["content"].(type) {
	case string:
		if _, answer, ok := splitLeadingThinkBlock(typed); ok {
			typed = answer
		}
		if typed != "" {
			content = append(content, map[string]any{"type": "output_text", "text": typed, "annotations": []any{}})
		}
	case []any:
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch partType, _ := part["type"].(string); partType {
			case "text", "output_text":
				if text, ok := part["text"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
				}
			case "refusal":
				if text, ok := part["refusal"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "refusal", "refusal": text})
				}
			}
		}
	}
	if refusal, ok := message["refusal"].(string); ok && refusal != "" {
		content = append(content, map[string]any{"type": "refusal", "refusal": refusal})
	}
	if len(content) == 0 {
		return nil
	}
	return map[string]any{
		"id":      responseID + "_msg",
		"type":    "message",
		"status":  "completed",
		"role":    "assistant",
		"content": content,
	}
}

func chatToolCallsToResponseOutputItems(message map[string]any, reasoning string, toolContext *ToolContext) (items []any, dropped int) {
	calls, ok := message["tool_calls"].([]any)
	if ok {
		for index, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				dropped++
				continue
			}
			function, _ := call["function"].(map[string]any)
			name, _ := function["name"].(string)
			// Whitespace-only names map to no published tool and are treated
			// exactly like empty ones.
			if strings.TrimSpace(name) == "" {
				dropped++
				continue
			}
			items = append(items, chatToolCallToResponseItem(call, index, reasoning, toolContext))
		}
		return items, dropped
	}
	if functionCall, ok := message["function_call"].(map[string]any); ok {
		item, ok := chatLegacyFunctionCallToResponseItem(functionCall, reasoning, toolContext)
		if !ok {
			return nil, 1
		}
		return []any{item}, 0
	}
	return nil, 0
}

func chatToolCallToResponseItem(toolCall map[string]any, index int, reasoning string, toolContext *ToolContext) map[string]any {
	callID := stringOrEmpty(toolCall["id"])
	if callID == "" {
		callID = fmt.Sprintf("call_%d", index)
	}
	function, _ := toolCall["function"].(map[string]any)
	name, _ := function["name"].(string)
	arguments := canonicalizeToolArguments(function["arguments"])
	itemID := responseToolCallItemIDFromChatName(callID, name, toolContext)
	return responseToolCallItemFromChatName(itemID, "completed", callID, name, arguments, reasoning, toolContext)
}

func chatLegacyFunctionCallToResponseItem(functionCall map[string]any, reasoning string, toolContext *ToolContext) (map[string]any, bool) {
	callID := stringOrEmpty(functionCall["id"])
	if callID == "" {
		callID = "call_0"
	}
	name, _ := functionCall["name"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, false
	}
	arguments := canonicalizeToolArguments(functionCall["arguments"])
	itemID := responseToolCallItemIDFromChatName(callID, name, toolContext)
	return responseToolCallItemFromChatName(itemID, "completed", callID, name, arguments, reasoning, toolContext), true
}

func responseToolCallItemIDFromChatName(callID, chatName string, toolContext *ToolContext) string {
	if toolContext.IsCustomToolChatName(chatName) {
		return "ctc_" + callID
	}
	return "fc_" + callID
}

// responseToolCallItemFromChatName builds the Responses output item for a
// Chat tool call, restoring the original Responses tool vocabulary: tool
// search calls become tool_search_call items, custom tools become
// custom_tool_call items with their raw input, and namespaced functions get
// their namespace field back.
func responseToolCallItemFromChatName(itemID, status, callID, chatName, arguments, reasoning string, toolContext *ToolContext) map[string]any {
	spec, hasSpec := toolContext.LookupChatName(chatName)
	var item map[string]any
	switch {
	case hasSpec && spec.kind == toolKindToolSearch:
		item = map[string]any{
			"type":      "tool_search_call",
			"call_id":   callID,
			"status":    status,
			"execution": "client",
			"arguments": parseToolArgumentsObject(arguments),
		}
	case hasSpec && spec.kind == toolKindCustom:
		item = map[string]any{
			"id":      itemID,
			"type":    "custom_tool_call",
			"status":  status,
			"call_id": callID,
			"name":    spec.name,
			"input":   customToolInputFromChatArguments(arguments),
		}
	default:
		name := chatName
		if hasSpec {
			name = spec.name
		}
		item = map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"status":    status,
			"call_id":   callID,
			"name":      name,
			"arguments": arguments,
		}
		if hasSpec && spec.namespace != "" {
			item["namespace"] = spec.namespace
		}
	}
	attachReasoningContentField(item, reasoning)
	return item
}

// attachReasoningContentField attaches reasoning_content to a Responses
// output item when the reasoning text is non-empty.
func attachReasoningContentField(item map[string]any, reasoning string) {
	trimmed := strings.TrimSpace(reasoning)
	if trimmed == "" {
		return
	}
	item["reasoning_content"] = trimmed
}

// parseToolArgumentsObject parses tool-search arguments into an object.
// Non-object payloads are wrapped under query so the client receives a valid
// arguments object.
func parseToolArgumentsObject(arguments string) any {
	if strings.TrimSpace(arguments) == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return map[string]any{"query": arguments}
	}
	if object, ok := parsed.(map[string]any); ok {
		return object
	}
	return map[string]any{"query": arguments}
}

// customToolInputFromChatArguments extracts the raw freeform input from the
// wrapped {"input": ...} arguments payload.
func customToolInputFromChatArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return arguments
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return arguments
	}
	if input, ok := object[customToolInputField].(string); ok {
		return input
	}
	return arguments
}

func responseIDFromChatID(id string) string {
	if id == "" {
		id = "ccswitch"
	}
	if strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

// responseIncompleteReason maps a Chat finish_reason onto the Responses API's
// incomplete_details.reason, returning "" when the turn is not incomplete.
//
// Chat has two truncating finishes and Responses has a reason for each: length
// is max_output_tokens, content_filter is content_filter. Reporting only the
// former left a filtered turn marked "completed", so a client branching on
// status treated a cut-off turn as a finished one.
func responseIncompleteReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	default:
		return ""
	}
}

func responseStatusFromFinishReason(finishReason string) string {
	if responseIncompleteReason(finishReason) != "" {
		return "incomplete"
	}
	return "completed"
}

// ---------------------------------------------------------------------------
// Usage mapping (CC Switch chat_usage_to_responses_usage)
// ---------------------------------------------------------------------------

// chatUsageToResponsesUsage maps a Chat usage object to the Responses usage
// vocabulary. Cache hit counters fall back through the documented aliases:
// cache_read_input_tokens, prompt_tokens_details/input_tokens_details
// cached_tokens, and DeepSeek's prompt_cache_hit_tokens. A missing usage
// object synthesizes an all-zero usage so clients always see the field.
func chatUsageToResponsesUsage(usage any) map[string]any {
	zero := map[string]any{
		"input_tokens":          0,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens":         0,
		"total_tokens":          0,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	source, ok := usage.(map[string]any)
	if !ok {
		return zero
	}

	inputTokens := firstUsageUint(source, "prompt_tokens", "input_tokens")
	outputTokens := firstUsageUint(source, "completion_tokens", "output_tokens")
	totalTokens, hasTotal := jsonUint64Opt(source["total_tokens"])
	if !hasTotal {
		totalTokens = inputTokens + outputTokens
	}

	result := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  totalTokens,
	}

	directCacheRead, hasDirectCacheRead := jsonUint64Opt(source["cache_read_input_tokens"])
	var cached uint64
	if hasDirectCacheRead {
		cached = directCacheRead
	} else if value, ok := usageDetailUint(source, "prompt_tokens_details", "cached_tokens"); ok {
		cached = value
	} else if value, ok := usageDetailUint(source, "input_tokens_details", "cached_tokens"); ok {
		cached = value
	} else {
		cached = jsonUint64(source["prompt_cache_hit_tokens"])
	}
	cacheWrite := cacheWriteTokens(source)
	if cached > 0 || cacheWrite > 0 {
		result["input_tokens_details"] = map[string]any{
			"cached_tokens":      cached,
			"cache_write_tokens": cacheWrite,
		}
	} else {
		result["input_tokens_details"] = map[string]any{"cached_tokens": 0}
	}

	if details, ok := source["completion_tokens_details"].(map[string]any); ok {
		outputDetails := make(map[string]any, len(details)+1)
		for key, value := range details {
			outputDetails[key] = value
		}
		if _, exists := outputDetails["reasoning_tokens"]; !exists {
			outputDetails["reasoning_tokens"] = 0
		}
		result["output_tokens_details"] = outputDetails
	} else {
		result["output_tokens_details"] = map[string]any{"reasoning_tokens": 0}
	}

	if hasDirectCacheRead {
		result["cache_read_input_tokens"] = directCacheRead
	}
	if cacheWrite > 0 {
		result["cache_creation_input_tokens"] = cacheWrite
	}
	return result
}

func firstUsageUint(source map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		if value, exists := source[key]; exists {
			return jsonUint64(value)
		}
	}
	return 0
}

func usageDetailUint(source map[string]any, container, field string) (uint64, bool) {
	details, ok := source[container].(map[string]any)
	if !ok {
		return 0, false
	}
	return jsonUint64Opt(details[field])
}

func cacheWriteTokens(source map[string]any) uint64 {
	if value, ok := usageDetailUint(source, "prompt_tokens_details", "cache_write_tokens"); ok {
		return value
	}
	if value, ok := usageDetailUint(source, "input_tokens_details", "cache_write_tokens"); ok {
		return value
	}
	if value, ok := jsonUint64Opt(source["cache_creation_input_tokens"]); ok {
		return value
	}
	return 0
}

// ---------------------------------------------------------------------------
// Error normalization (CC Switch chat_error_to_response_error)
// ---------------------------------------------------------------------------

// ChatErrorToResponseError normalizes an upstream Chat error body into the
// OpenAI Responses error shape {"error":{message,type,code,param}}. It
// accepts standard OpenAI bodies, MiniMax-style base_resp bodies, and plain
// text or empty bodies.
func ChatErrorToResponseError(body []byte) []byte {
	data, err := json.Marshal(chatErrorValueToResponseError(body))
	if err != nil {
		return []byte(`{"error":{"message":"Upstream error","type":"upstream_error","code":null,"param":null}}`)
	}
	return data
}

func chatErrorValueToResponseError(body []byte) map[string]any {
	buildError := func(message, errorType string, code, param any) map[string]any {
		if errorType == "" {
			errorType = "upstream_error"
		}
		return map[string]any{"error": map[string]any{
			"message": message, "type": errorType, "code": code, "param": param,
		}}
	}
	emptyBody := func() map[string]any {
		return buildError("Upstream returned an empty error response", "upstream_error", nil, nil)
	}

	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return emptyBody()
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		// Plain text body: the message is the raw text.
		return buildError(trimmed, "upstream_error", nil, nil)
	}

	var source any = parsed
	if parsedMap, ok := parsed.(map[string]any); ok {
		if nested, ok := parsedMap["error"]; ok && nested != nil {
			source = nested
		}
	}
	switch typed := source.(type) {
	case string:
		return buildError(typed, "upstream_error", nil, nil)
	case map[string]any:
		sourceMap := typed
		message := ""
		for _, key := range []string{"message", "detail", "status_msg"} {
			if text, ok := sourceMap[key].(string); ok && text != "" {
				message = text
				break
			}
		}
		code := any(nil)
		if value, exists := sourceMap["code"]; exists {
			code = value
		}
		if baseResp, ok := sourceMap["base_resp"].(map[string]any); ok {
			if message == "" {
				message, _ = baseResp["status_msg"].(string)
			}
			if code == nil {
				if value, exists := baseResp["status_code"]; exists {
					code = value
				}
			}
		}
		if message == "" {
			// No field carried text: serialize the body so users can still
			// see what the upstream returned.
			message = canonicalJSONString(sourceMap)
		}
		errorType := ""
		if value, ok := sourceMap["type"].(string); ok {
			errorType = value
		}
		var param any
		if value, exists := sourceMap["param"]; exists {
			param = value
		}
		return buildError(message, errorType, code, param)
	default:
		return buildError(canonicalJSONString(parsed), "upstream_error", nil, nil)
	}
}
