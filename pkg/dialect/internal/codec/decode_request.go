package codec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeRequest reads a wire request into the canonical representation.
func DecodeRequest(format Format, body []byte) (*Request, []Diagnostic, error) {
	var diagnostics []Diagnostic
	var request *Request
	var err error
	switch format {
	case OpenAIChat:
		request, err = decodeChatRequest(body, &diagnostics)
	case OpenAIResponses:
		request, err = decodeResponsesRequest(body, &diagnostics)
	case ClaudeMessages:
		request, err = decodeClaudeRequest(body, &diagnostics)
	case Gemini:
		request, err = decodeGeminiRequest(body, &diagnostics)
	case GeminiInteractions:
		request, err = decodeInteractionsRequest(body, &diagnostics)
	default:
		return nil, nil, conversionErrorf("unsupported_format", "unsupported request dialect %q", format)
	}
	if err != nil {
		return nil, diagnostics, err
	}
	return request, diagnostics, nil
}

// ---------------------------------------------------------------------------
// Shared helpers

func normalizeRole(role string) Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return RoleSystem
	case "assistant", "model":
		return RoleAssistant
	case "tool", "function":
		return RoleTool
	default:
		return RoleUser
	}
}

// appendAssistantToolCall adds a tool call to the trailing assistant message,
// creating one when the previous turn is not an assistant message.
func appendAssistantToolCall(messages []Message, part Part) []Message {
	if len(messages) > 0 && messages[len(messages)-1].Role == RoleAssistant {
		messages[len(messages)-1].Parts = append(messages[len(messages)-1].Parts, part)
		return messages
	}
	return append(messages, Message{Role: RoleAssistant, Parts: []Part{part}})
}

// toolResultPart builds a canonical tool result.
func toolResultPart(id, name string, content []Part, isError bool) Part {
	return Part{Kind: PartToolResult, ToolCallID: id, ToolName: name, Content: content, IsError: isError}
}

// textParts flattens text-only content.
func textParts(content any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		var parts []Part
		for index, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				appendDiagnostic(diagnostics, "invalid_content_part",
					"content part is not an object and was dropped", fmt.Sprintf("%s[%d]", path, index))
				continue
			}
			kind := stringAt(part, "type")
			switch kind {
			case "text", "input_text", "output_text", "summary_text", "reasoning_text", "":
				text := stringAt(part, "text")
				if text != "" {
					parts = append(parts, Part{Kind: PartText, Text: text})
				}
			default:
				appendDiagnostic(diagnostics, "content_part_dropped",
					fmt.Sprintf("content part type %q is not text", kind), fmt.Sprintf("%s[%d]", path, index))
			}
		}
		return parts
	}
	appendDiagnostic(diagnostics, "invalid_content", "content is neither a string nor an array", path)
	return nil
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions

func decodeChatRequest(body []byte, diagnostics *[]Diagnostic) (*Request, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model:     stringAt(object, "model"),
		Stream:    boolValue(object, "stream"),
		Extra:     map[string]json.RawMessage{},
		MaxTokens: intPtr(object, "max_completion_tokens"),
	}
	legacyCalls := 0
	legacyCallIDs := map[string][]string{}
	if request.MaxTokens == nil {
		request.MaxTokens = intPtr(object, "max_tokens")
	}
	request.Temperature = floatPtr(object, "temperature")
	request.TopP = floatPtr(object, "top_p")
	request.N = intPtr(object, "n")
	request.Seed = int64Ptr(object, "seed")
	request.PresencePenalty = floatPtr(object, "presence_penalty")
	request.FrequencyPenalty = floatPtr(object, "frequency_penalty")
	request.Stop = stringList(object, "stop", diagnostics)
	request.ParallelTools = boolPtr(object, "parallel_tool_calls")
	request.User = stringAt(object, "user")
	request.Metadata = stringMap(object, "metadata")
	request.ResponseFormat = chatResponseFormat(object, diagnostics)
	request.Reasoning = chatReasoning(object, diagnostics)

	for _, key := range []string{
		"logit_bias", "logprobs", "top_logprobs", "prompt_cache_key", "prompt_cache_retention",
		"store", "service_tier", "truncation", "safety_identifier",
	} {
		if raw, ok := object[key]; ok {
			request.Extra[key] = rawObject(raw)
		}
	}

	for index, raw := range arrayAt(object, "messages") {
		message, ok := raw.(map[string]any)
		if !ok {
			appendDiagnostic(diagnostics, "invalid_message",
				"message is not an object and was dropped", fmt.Sprintf("messages[%d]", index))
			continue
		}
		role := normalizeRole(stringAt(message, "role"))
		parts := decodeChatContent(message["content"], diagnostics, fmt.Sprintf("messages[%d].content", index))
		// Assistant tool calls.
		for callIndex, rawCall := range arrayAt(message, "tool_calls") {
			call, ok := rawCall.(map[string]any)
			if !ok {
				appendDiagnostic(diagnostics, "invalid_tool_call",
					"tool call is not an object and was dropped",
					fmt.Sprintf("messages[%d].tool_calls[%d]", index, callIndex))
				continue
			}
			function := objectAt(call, "function")
			parts = append(parts, Part{
				Kind:          PartToolCall,
				ToolCallID:    stringAt(call, "id"),
				ToolName:      stringAt(function, "name"),
				ArgumentsText: stringAt(function, "arguments"),
				Arguments: parseArguments(stringAt(function, "arguments"), diagnostics,
					fmt.Sprintf("messages[%d].tool_calls[%d].function.arguments", index, callIndex)),
			})
		}
		// Legacy single function call. The legacy wire format carries no id,
		// so one is synthesized and remembered so the matching role:function
		// result can be joined by name below.
		if function := objectAt(message, "function_call"); function != nil {
			callID := stringAt(function, "id")
			if callID == "" {
				callID = stringAt(message, "function_call_id")
			}
			if callID == "" {
				legacyCalls++
				callID = fmt.Sprintf("call_legacy_%d", legacyCalls)
			}
			legacyCallIDs[stringAt(function, "name")] = append(legacyCallIDs[stringAt(function, "name")], callID)
			parts = append(parts, Part{
				Kind:          PartToolCall,
				ToolCallID:    callID,
				ToolName:      stringAt(function, "name"),
				ArgumentsText: stringAt(function, "arguments"),
				Arguments: parseArguments(stringAt(function, "arguments"), diagnostics,
					fmt.Sprintf("messages[%d].function_call.arguments", index)),
			})
		}
		if reasoning := stringAt(message, "reasoning_content"); reasoning != "" {
			// Reasoning precedes the visible answer in the canonical order.
			parts = append([]Part{{Kind: PartReasoning, Text: reasoning}}, parts...)
		}
		if refusal := stringAt(message, "refusal"); refusal != "" {
			parts = append(parts, Part{Kind: PartRefusal, Text: refusal})
		}
		if role == RoleTool {
			// A Chat tool message names its call; rebuild it as one canonical
			// result so media and text survive conversion.
			callID := stringAt(message, "tool_call_id")
			name := stringAt(message, "name")
			if callID == "" && name != "" {
				// Legacy role:function results have no id: pair the oldest
				// unclaimed call with that name.
				if pending := legacyCallIDs[name]; len(pending) > 0 {
					callID = pending[0]
					legacyCallIDs[name] = pending[1:]
				}
			}
			if callID == "" {
				// An orphan result has no call to answer. Anthropic rejects an
				// empty tool_use_id, so name the problem and drop the turn.
				appendDiagnostic(diagnostics, "orphan_tool_result",
					"a tool result names no matching call and was dropped",
					fmt.Sprintf("messages[%d]", index))
				continue
			}
			parts = []Part{toolResultPart(
				callID, name,
				decodeChatContent(message["content"], diagnostics, fmt.Sprintf("messages[%d].content", index)),
				false,
			)}
		}
		if role == RoleSystem {
			// A leading system turn is the system prompt. A later one sits
			// between user turns and must keep its position, so it stays a
			// RoleSystem message for the encoders to place in order.
			if len(request.Messages) == 0 {
				request.System = append(request.System, parts...)
				continue
			}
			request.Messages = append(request.Messages, Message{Role: RoleSystem, Parts: parts})
			continue
		}
		if len(parts) == 0 && role != RoleAssistant {
			// A tool message with no content is still a valid empty result.
			if role == RoleTool {
				parts = []Part{toolResultPart(stringAt(message, "tool_call_id"), "", nil, false)}
			} else {
				continue
			}
		}
		request.Messages = append(request.Messages, Message{
			Role: role, Parts: parts, Name: stringAt(message, "name"),
		})
	}

	request.Tools = decodeChatTools(arrayAt(object, "tools"), diagnostics)
	request.ToolChoice = decodeChatToolChoice(object["tool_choice"], diagnostics)
	return request, nil
}

func decodeChatContent(content any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		var parts []Part
		for index, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				appendDiagnostic(diagnostics, "invalid_content_part",
					"content part is not an object and was dropped", fmt.Sprintf("%s[%d]", path, index))
				continue
			}
			partPath := fmt.Sprintf("%s[%d]", path, index)
			switch stringAt(part, "type") {
			case "text", "input_text", "output_text":
				if text := stringAt(part, "text"); text != "" {
					parts = append(parts, Part{Kind: PartText, Text: text})
				}
			case "refusal":
				if text := stringAt(part, "refusal"); text != "" {
					parts = append(parts, Part{Kind: PartRefusal, Text: text})
				}
			case "image_url":
				parts = append(parts, decodeImageURL(part["image_url"], diagnostics, partPath)...)
			case "input_image":
				parts = append(parts, decodeImageURL(part["image_url"], diagnostics, partPath)...)
			case "input_file", "file":
				parts = append(parts, decodeFilePart(part, diagnostics, partPath)...)
			default:
				appendDiagnostic(diagnostics, "content_part_dropped",
					fmt.Sprintf("content part type %q cannot be represented", stringAt(part, "type")), partPath)
			}
		}
		return parts
	}
	appendDiagnostic(diagnostics, "invalid_content", "content is neither a string nor an array", path)
	return nil
}

func decodeImageURL(value any, diagnostics *[]Diagnostic, path string) []Part {
	var url string
	switch typed := value.(type) {
	case string:
		url = typed
	case map[string]any:
		url = stringAt(typed, "url")
	}
	if url == "" {
		appendDiagnostic(diagnostics, "invalid_image", "image part has no url", path)
		return nil
	}
	if mediaType, data, ok := parseDataURL(url); ok {
		return []Part{{Kind: PartImage, MediaType: mediaType, Base64: data}}
	}
	if strings.HasPrefix(url, "data:") {
		// A percent-encoded/plain data URL cannot be forwarded as a URL source:
		// upstreams reject data: URLs. Drop it with the real reason instead of
		// passing an unusable URL through.
		appendDiagnostic(diagnostics, "non_base64_data_url",
			"data: URLs must use base64 encoding to be converted", path)
		return nil
	}
	return []Part{{Kind: PartImage, URL: url}}
}

func decodeFilePart(part map[string]any, diagnostics *[]Diagnostic, path string) []Part {
	file := objectAt(part, "file")
	if file == nil {
		file = part
	}
	if data := stringAt(file, "file_data"); data != "" {
		if mediaType, base64Data, ok := parseDataURL(data); ok {
			return []Part{{Kind: PartDocument, MediaType: mediaType, Base64: base64Data}}
		}
		return []Part{{Kind: PartDocument, Text: data}}
	}
	if fileID := stringAt(file, "file_id"); fileID != "" {
		appendDiagnostic(diagnostics, "file_id_dropped",
			"file_id cannot be resolved by the conversion layer", path)
		return nil
	}
	for _, key := range []string{"url", "file_url"} {
		if url := stringAt(file, key); url != "" {
			return []Part{{Kind: PartDocument, URL: url}}
		}
	}
	appendDiagnostic(diagnostics, "invalid_file", "file part has no data, id or url", path)
	return nil
}

func decodeChatTools(raw []any, diagnostics *[]Diagnostic) []Tool {
	var tools []Tool
	for index, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typeName := stringAt(tool, "type"); typeName != "" && typeName != "function" {
			appendDiagnostic(diagnostics, "hosted_tool_dropped",
				fmt.Sprintf("hosted tool type %q cannot be represented as a function", typeName),
				fmt.Sprintf("tools[%d]", index))
			continue
		}
		function := objectAt(tool, "function")
		if function == nil {
			function = tool
		}
		name := stringAt(function, "name")
		if name == "" {
			appendDiagnostic(diagnostics, "invalid_tool", "tool has no name and was dropped", fmt.Sprintf("tools[%d]", index))
			continue
		}
		entry := Tool{
			Name:        name,
			Description: stringAt(function, "description"),
			Parameters:  rawObject(function["parameters"]),
		}
		if strict, ok := boolAt(function, "strict"); ok {
			entry.Strict = &strict
		}
		tools = append(tools, entry)
	}
	return tools
}

func decodeChatToolChoice(value any, diagnostics *[]Diagnostic) *ToolChoice {
	switch typed := value.(type) {
	case string:
		switch typed {
		case "auto", "none", "required":
			return &ToolChoice{Mode: ToolChoiceMode(typed)}
		}
	case map[string]any:
		typeName := stringAt(typed, "type")
		if typeName == "function" {
			name := stringAt(objectAt(typed, "function"), "name")
			if name == "" {
				name = stringAt(typed, "name")
			}
			if name != "" {
				return &ToolChoice{Mode: ToolChoiceTool, Name: name}
			}
			return &ToolChoice{Mode: ToolChoiceRequired}
		}
		if typeName == "allowed_tools" {
			// Responses-style policy seen on Chat-compatible providers.
			mode := stringAt(typed, "mode")
			if tools := arrayAt(typed, "tools"); len(tools) == 1 {
				if tool, ok := tools[0].(map[string]any); ok {
					name := stringAt(tool, "name")
					if name == "" {
						name = stringAt(objectAt(tool, "function"), "name")
					}
					if name != "" {
						return &ToolChoice{Mode: ToolChoiceTool, Name: name}
					}
				}
			}
			if mode == "required" {
				return &ToolChoice{Mode: ToolChoiceRequired}
			}
			return &ToolChoice{Mode: ToolChoiceAuto}
		}
	}
	if value != nil {
		appendDiagnostic(diagnostics, "tool_choice_dropped", "tool_choice has an unsupported shape", "tool_choice")
	}
	return nil
}

func chatResponseFormat(object map[string]any, diagnostics *[]Diagnostic) *ResponseFormat {
	raw := objectAt(object, "response_format")
	if raw == nil {
		return nil
	}
	kind := FormatKind(stringAt(raw, "type"))
	format := &ResponseFormat{Kind: kind}
	if kind == FormatJSONSchema {
		schema := objectAt(raw, "json_schema")
		if schema == nil {
			schema = raw
		}
		format.Name = stringAt(schema, "name")
		format.Description = stringAt(schema, "description")
		format.Schema = rawObject(schema["schema"])
		if strict, ok := boolAt(schema, "strict"); ok {
			format.Strict = &strict
		}
		if len(format.Schema) == 0 {
			appendDiagnostic(diagnostics, "response_format_schema_missing",
				"json_schema response_format has no schema", "response_format.json_schema.schema")
		}
	}
	return format
}

func chatReasoning(object map[string]any, diagnostics *[]Diagnostic) *ReasoningConfig {
	effort := stringAt(object, "reasoning_effort")
	if effort == "" {
		effort = stringAt(objectAt(object, "reasoning"), "effort")
	}
	if effort == "" {
		return nil
	}
	return &ReasoningConfig{Effort: effort}
}

func boolPtr(m map[string]any, key string) *bool {
	if value, ok := boolAt(m, key); ok {
		return &value
	}
	return nil
}

func stringMap(m map[string]any, key string) map[string]string {
	raw := objectAt(m, key)
	if raw == nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		if text, ok := value.(string); ok {
			out[name] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Anthropic Messages

func decodeClaudeRequest(body []byte, diagnostics *[]Diagnostic) (*Request, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model:       stringAt(object, "model"),
		Stream:      boolValue(object, "stream"),
		MaxTokens:   intPtr(object, "max_tokens"),
		Temperature: floatPtr(object, "temperature"),
		TopP:        floatPtr(object, "top_p"),
		TopK:        intPtr(object, "top_k"),
		Stop:        stringList(object, "stop_sequences", diagnostics),
		Extra:       map[string]json.RawMessage{},
	}
	request.System = textParts(object["system"], diagnostics, "system")
	// cache_control breakpoints are Anthropic-only. Counting them here lets a
	// non-Anthropic target report the loss instead of dropping it silently.
	request.CacheBreakpoints = countCacheBreakpoints(object)
	request.Tools = decodeClaudeTools(arrayAt(object, "tools"), diagnostics)
	request.ToolChoice = decodeClaudeToolChoice(object["tool_choice"], diagnostics)
	request.Reasoning = claudeReasoning(object, diagnostics)
	request.ResponseFormat = claudeResponseFormat(object, diagnostics)
	if metadata := objectAt(object, "metadata"); metadata != nil {
		if user := stringAt(metadata, "user_id"); user != "" {
			request.User = user
			request.Metadata = map[string]string{"user_id": user}
		}
	}

	for index, raw := range arrayAt(object, "messages") {
		message, ok := raw.(map[string]any)
		if !ok {
			appendDiagnostic(diagnostics, "invalid_message", "message is not an object and was dropped", fmt.Sprintf("messages[%d]", index))
			continue
		}
		role := normalizeRole(stringAt(message, "role"))
		parts := decodeClaudeContent(message["content"], diagnostics, fmt.Sprintf("messages[%d].content", index))
		if len(parts) == 0 {
			continue
		}
		request.Messages = append(request.Messages, Message{Role: role, Parts: parts})
	}
	return request, nil
}

func decodeClaudeContent(content any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		var parts []Part
		for index, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			blockPath := fmt.Sprintf("%s[%d]", path, index)
			switch stringAt(block, "type") {
			case "text":
				if text := stringAt(block, "text"); text != "" {
					parts = append(parts, Part{Kind: PartText, Text: text})
				}
			case "image":
				parts = append(parts, decodeClaudeMedia(block, PartImage, diagnostics, blockPath)...)
			case "document":
				parts = append(parts, decodeClaudeMedia(block, PartDocument, diagnostics, blockPath)...)
			case "tool_use":
				parts = append(parts, Part{
					Kind:          PartToolCall,
					ToolCallID:    stringAt(block, "id"),
					ToolName:      stringAt(block, "name"),
					Arguments:     parseArguments(string(mustMarshal(block["input"])), diagnostics, blockPath+".input"),
					ArgumentsText: string(mustMarshal(block["input"])),
				})
			case "tool_result":
				contentParts := decodeClaudeContent(block["content"], diagnostics, blockPath+".content")
				parts = append(parts, toolResultPart(
					stringAt(block, "tool_use_id"), "", contentParts, boolValue(block, "is_error")))
			case "thinking":
				parts = append(parts, Part{
					Kind: PartReasoning, Text: stringAt(block, "thinking"), Signature: stringAt(block, "signature"),
				})
			case "redacted_thinking":
				parts = append(parts, Part{Kind: PartReasoning, Redacted: true, Data: stringAt(block, "data")})
			case "server_tool_use", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "mcp_tool_use", "mcp_tool_result":
				appendDiagnostic(diagnostics, "server_tool_result_dropped",
					fmt.Sprintf("Anthropic server tool block %q cannot be represented", stringAt(block, "type")), blockPath)
			default:
				appendDiagnostic(diagnostics, "content_block_dropped",
					fmt.Sprintf("content block type %q cannot be represented", stringAt(block, "type")), blockPath)
			}
		}
		return parts
	}
	return nil
}

func decodeClaudeMedia(block map[string]any, kind PartKind, diagnostics *[]Diagnostic, path string) []Part {
	source := objectAt(block, "source")
	if source == nil {
		source = objectAt(block, "file")
	}
	if source == nil {
		appendDiagnostic(diagnostics, "invalid_media", "media block has no source", path)
		return nil
	}
	switch stringAt(source, "type") {
	case "base64":
		return []Part{{
			Kind: kind, MediaType: stringAt(source, "media_type"), Base64: stringAt(source, "data"),
		}}
	case "url":
		return []Part{{Kind: kind, URL: stringAt(source, "url")}}
	case "text":
		// Plain-text documents degrade to text.
		return []Part{{Kind: PartText, Text: stringAt(source, "data")}}
	default:
		appendDiagnostic(diagnostics, "media_source_dropped",
			fmt.Sprintf("media source type %q cannot be represented", stringAt(source, "type")), path)
		return nil
	}
}

func decodeClaudeTools(raw []any, diagnostics *[]Diagnostic) []Tool {
	var tools []Tool
	for index, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Anthropic hosted tools carry a versioned type instead of a schema.
		if typeName := stringAt(tool, "type"); typeName != "" && typeName != "custom" {
			appendDiagnostic(diagnostics, "hosted_tool_dropped",
				fmt.Sprintf("Anthropic hosted tool %q cannot be represented as a function", typeName),
				fmt.Sprintf("tools[%d]", index))
			continue
		}
		name := stringAt(tool, "name")
		if name == "" {
			continue
		}
		entry := Tool{
			Name:        name,
			Description: stringAt(tool, "description"),
			Parameters:  rawObject(tool["input_schema"]),
		}
		if strict, ok := boolAt(tool, "strict"); ok {
			entry.Strict = &strict
		}
		tools = append(tools, entry)
	}
	return tools
}

func decodeClaudeToolChoice(value any, diagnostics *[]Diagnostic) *ToolChoice {
	object, ok := value.(map[string]any)
	if !ok {
		if value != nil {
			appendDiagnostic(diagnostics, "tool_choice_dropped", "tool_choice is not an object", "tool_choice")
		}
		return nil
	}
	choice := &ToolChoice{}
	switch stringAt(object, "type") {
	case "auto":
		choice.Mode = ToolChoiceAuto
	case "none":
		choice.Mode = ToolChoiceNone
	case "any":
		choice.Mode = ToolChoiceRequired
	case "tool":
		choice.Mode = ToolChoiceTool
		choice.Name = stringAt(object, "name")
		if choice.Name == "" {
			choice.Mode = ToolChoiceRequired
		}
	default:
		appendDiagnostic(diagnostics, "tool_choice_dropped",
			fmt.Sprintf("tool_choice type %q is unknown", stringAt(object, "type")), "tool_choice")
		return nil
	}
	if disable, ok := boolAt(object, "disable_parallel_tool_use"); ok {
		choice.DisableParallel = &disable
	}
	return choice
}

func claudeReasoning(object map[string]any, diagnostics *[]Diagnostic) *ReasoningConfig {
	reasoning := &ReasoningConfig{}
	thinking := objectAt(object, "thinking")
	if thinking != nil {
		switch stringAt(thinking, "type") {
		case "enabled":
			enabled := true
			reasoning.Enabled = &enabled
			reasoning.BudgetTokens = intPtr(thinking, "budget_tokens")
		case "disabled":
			enabled := false
			reasoning.Enabled = &enabled
		}
	}
	if output := objectAt(object, "output_config"); output != nil {
		reasoning.Effort = stringAt(output, "effort")
	}
	if reasoning.BudgetTokens == nil && reasoning.Effort == "" && reasoning.Enabled == nil {
		return nil
	}
	return reasoning
}

func claudeResponseFormat(object map[string]any, diagnostics *[]Diagnostic) *ResponseFormat {
	output := objectAt(object, "output_config")
	if output == nil {
		return nil
	}
	format := objectAt(output, "format")
	if format == nil {
		return nil
	}
	if stringAt(format, "type") != "json_schema" {
		appendDiagnostic(diagnostics, "response_format_dropped",
			"Anthropic output_config.format only supports json_schema", "output_config.format.type")
		return nil
	}
	return &ResponseFormat{
		Kind:   FormatJSONSchema,
		Schema: rawObject(format["schema"]),
	}
}

// ---------------------------------------------------------------------------
// Gemini generateContent

func decodeGeminiRequest(body []byte, diagnostics *[]Diagnostic) (*Request, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model: stringAt(object, "model"),
		Extra: map[string]json.RawMessage{},
	}
	// Gemini carries the model in the URL; callers set it from the route.
	if request.Model == "" {
		request.Model = stringAt(objectAt(object, "model"), "name")
	}
	// cachedContent references a server-side prefix cache that only Gemini can
	// honour; the converter cannot resolve it, so a target that is not Gemini
	// must not pretend the prefix is present.
	if cached := stringAt(object, "cachedContent"); cached != "" {
		request.Extra["cachedContent"] = rawObject(cached)
	} else if cached := stringAt(object, "cached_content"); cached != "" {
		request.Extra["cachedContent"] = rawObject(cached)
	}
	request.System = decodeGeminiSystem(object["systemInstruction"], diagnostics)
	if config := objectAt(object, "generationConfig"); config != nil {
		request.Temperature = floatPtr(config, "temperature")
		request.TopP = floatPtr(config, "topP")
		request.TopK = intPtr(config, "topK")
		request.MaxTokens = intPtr(config, "maxOutputTokens")
		request.N = intPtr(config, "candidateCount")
		request.Seed = int64Ptr(config, "seed")
		request.PresencePenalty = floatPtr(config, "presencePenalty")
		request.FrequencyPenalty = floatPtr(config, "frequencyPenalty")
		if values := arrayAt(config, "stopSequences"); values != nil {
			for index, item := range values {
				if text, ok := item.(string); ok {
					request.Stop = append(request.Stop, text)
				} else {
					appendDiagnostic(diagnostics, "invalid_stop_entry",
						fmt.Sprintf("stopSequences entry %d is not a string", index),
						fmt.Sprintf("generationConfig.stopSequences[%d]", index))
				}
			}
		}
		request.ResponseFormat = geminiResponseFormat(config, diagnostics)
		request.Reasoning = geminiReasoning(config)
	}
	request.Tools = decodeGeminiTools(arrayAt(object, "tools"), diagnostics)
	request.ToolChoice = decodeGeminiToolChoice(objectAt(object, "toolConfig"), diagnostics)

	calls := newGeminiCallTracker()
	for index, raw := range arrayAt(object, "contents") {
		content, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := RoleUser
		if stringAt(content, "role") == "model" {
			role = RoleAssistant
		}
		parts := decodeGeminiParts(arrayAt(content, "parts"), calls, diagnostics, fmt.Sprintf("contents[%d].parts", index))
		if len(parts) == 0 {
			continue
		}
		request.Messages = append(request.Messages, Message{Role: role, Parts: parts})
	}
	return request, nil
}

// geminiCallTracker hands out synthetic ids for Gemini function calls, which
// carry no id of their own. functionResponse parts are paired back by name.
type geminiCallTracker struct {
	sequence int
	pending  map[string][]string
	// orphans counts functionResponses with no matching call, so the decoder
	// can report them.
	orphans int
}

func newGeminiCallTracker() *geminiCallTracker {
	return &geminiCallTracker{pending: map[string][]string{}}
}

func (t *geminiCallTracker) callID(name string) string {
	t.sequence++
	id := fmt.Sprintf("call_gemini_%d", t.sequence)
	t.pending[name] = append(t.pending[name], id)
	return id
}

// resolve pairs a functionResponse with the oldest unclaimed call of that
// name. An orphan (no preceding call) still gets an id so the converted request
// never carries a tool result that no assistant call declares; the caller
// reports it.
func (t *geminiCallTracker) resolve(name string) string {
	ids := t.pending[name]
	if len(ids) == 0 {
		// An orphan reserves nothing: minting an id here left it pending, so a
		// later real call/result pair could be handed the dropped result's id.
		t.orphans++
		return ""
	}
	id := ids[0]
	t.pending[name] = ids[1:]
	return id
}

func decodeGeminiSystem(value any, diagnostics *[]Diagnostic) []Part {
	object, ok := value.(map[string]any)
	if !ok {
		if text, ok := value.(string); ok {
			return []Part{{Kind: PartText, Text: text}}
		}
		return nil
	}
	return decodeGeminiParts(arrayAt(object, "parts"), newGeminiCallTracker(), diagnostics, "systemInstruction.parts")
}

func decodeGeminiParts(raw []any, calls *geminiCallTracker, diagnostics *[]Diagnostic, path string) []Part {
	var parts []Part
	for index, item := range raw {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		partPath := fmt.Sprintf("%s[%d]", path, index)
		switch {
		case part["text"] != nil:
			text := stringAt(part, "text")
			kind := PartText
			if thought, ok := boolAt(part, "thought"); ok && thought {
				kind = PartReasoning
			}
			if text != "" {
				parts = append(parts, Part{
					Kind: kind, Text: text, ThoughtSignature: geminiThoughtSignature(part),
				})
			}
		case part["inlineData"] != nil || part["inline_data"] != nil:
			inline := objectAt(part, "inlineData")
			if inline == nil {
				inline = objectAt(part, "inline_data")
			}
			mediaType := stringAt(inline, "mimeType")
			if mediaType == "" {
				appendDiagnostic(diagnostics, "media_type_missing",
					"inline media has no mimeType; application/octet-stream was assumed", partPath)
			}
			parts = append(parts, Part{
				Kind: mediaKind(mediaType), MediaType: mediaType, Base64: stringAt(inline, "data"),
			})
		case part["fileData"] != nil || part["file_data"] != nil:
			file := objectAt(part, "fileData")
			if file == nil {
				file = objectAt(part, "file_data")
			}
			mediaType := stringAt(file, "mimeType")
			if mediaType == "" {
				appendDiagnostic(diagnostics, "media_type_missing",
					"file media has no mimeType; application/octet-stream was assumed", partPath)
			}
			parts = append(parts, Part{
				Kind: mediaKind(mediaType), MediaType: mediaType, URL: stringAt(file, "fileUri"),
			})
		case part["functionCall"] != nil || part["function_call"] != nil:
			call := objectAt(part, "functionCall")
			if call == nil {
				call = objectAt(part, "function_call")
			}
			name := stringAt(call, "name")
			arguments := rawObject(call["args"])
			parts = append(parts, Part{
				Kind: PartToolCall, ToolCallID: calls.callID(name), ToolName: name,
				Arguments: arguments, ArgumentsText: string(arguments),
				ThoughtSignature: geminiThoughtSignature(part),
			})
		case part["functionResponse"] != nil || part["function_response"] != nil:
			response := objectAt(part, "functionResponse")
			if response == nil {
				response = objectAt(part, "function_response")
			}
			name := stringAt(response, "name")
			payload := rawObject(response["response"])
			text := string(payload)
			if inner, ok := response["response"].(map[string]any); ok {
				// functionResponse.response is an object; a single "result"
				// field is the common shape.
				if result, exists := inner["result"]; exists {
					if resultText, ok := result.(string); ok {
						text = resultText
					} else {
						text = string(mustMarshal(result))
					}
				}
			}
			before := calls.orphans
			callID := calls.resolve(name)
			if calls.orphans > before {
				// Anthropic rejects a tool_result whose tool_use_id has no
				// preceding tool_use, so a synthetic id does not make the turn
				// valid: the orphan is dropped and reported instead.
				appendDiagnostic(diagnostics, "orphan_tool_result",
					"a Gemini functionResponse has no matching functionCall and was dropped", partPath)
				continue
			}
			parts = append(parts, toolResultPart(callID, name,
				[]Part{{Kind: PartText, Text: text}}, false))
		default:
			appendDiagnostic(diagnostics, "gemini_part_dropped",
				"Gemini part cannot be represented (executable code, code result or unknown field)", partPath)
		}
	}
	return parts
}

func decodeGeminiTools(raw []any, diagnostics *[]Diagnostic) []Tool {
	var tools []Tool
	for index, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		declarations := arrayAt(tool, "functionDeclarations")
		if declarations == nil {
			declarations = arrayAt(tool, "function_declarations")
		}
		for _, rawDecl := range declarations {
			declaration, ok := rawDecl.(map[string]any)
			if !ok {
				continue
			}
			name := stringAt(declaration, "name")
			if name == "" {
				continue
			}
			tools = append(tools, Tool{
				Name:        name,
				Description: stringAt(declaration, "description"),
				Parameters:  rawObject(declaration["parameters"]),
			})
		}
		if len(declarations) == 0 {
			for _, hosted := range []string{"googleSearch", "google_search", "googleSearchRetrieval", "codeExecution"} {
				if _, exists := tool[hosted]; exists {
					appendDiagnostic(diagnostics, "hosted_tool_dropped",
						fmt.Sprintf("Gemini hosted tool %q cannot be represented as a function", hosted),
						fmt.Sprintf("tools[%d]", index))
				}
			}
		}
	}
	return tools
}

func decodeGeminiToolChoice(config map[string]any, diagnostics *[]Diagnostic) *ToolChoice {
	if config == nil {
		return nil
	}
	calling := objectAt(config, "functionCallingConfig")
	if calling == nil {
		calling = objectAt(config, "function_calling_config")
	}
	if calling == nil {
		return nil
	}
	choice := &ToolChoice{}
	switch strings.ToUpper(stringAt(calling, "mode")) {
	case "AUTO":
		choice.Mode = ToolChoiceAuto
	case "NONE":
		choice.Mode = ToolChoiceNone
	case "ANY":
		choice.Mode = ToolChoiceRequired
		if names := arrayAt(calling, "allowedFunctionNames"); len(names) == 1 {
			if name, ok := names[0].(string); ok && name != "" {
				choice.Mode = ToolChoiceTool
				choice.Name = name
			}
		}
	default:
		return nil
	}
	return choice
}

func geminiReasoning(config map[string]any) *ReasoningConfig {
	thinking := objectAt(config, "thinkingConfig")
	if thinking == nil {
		thinking = objectAt(config, "thinking_config")
	}
	if thinking == nil {
		return nil
	}
	reasoning := &ReasoningConfig{}
	if budget, ok := intAt(thinking, "thinkingBudget"); ok {
		reasoning.BudgetTokens = &budget
		enabled := budget != 0
		reasoning.Enabled = &enabled
	}
	return reasoning
}

func geminiResponseFormat(config map[string]any, diagnostics *[]Diagnostic) *ResponseFormat {
	mimeType := stringAt(config, "responseMimeType")
	schema := rawObject(config["responseSchema"])
	if mimeType == "" && len(schema) == 0 {
		return nil
	}
	switch mimeType {
	case "application/json", "":
		if len(schema) > 0 {
			return &ResponseFormat{Kind: FormatJSONSchema, Schema: schema}
		}
		return &ResponseFormat{Kind: FormatJSONObject}
	case "text/plain":
		return nil
	default:
		appendDiagnostic(diagnostics, "response_format_dropped",
			fmt.Sprintf("Gemini responseMimeType %q has no equivalent", mimeType), "generationConfig.responseMimeType")
		return nil
	}
}

// mustMarshal encodes a decoded JSON value, returning {} on failure.
// countCacheBreakpoints counts Anthropic cache_control markers anywhere in the
// request body (system, messages, tools).
func countCacheBreakpoints(object map[string]any) int {
	count := 0
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if _, ok := typed["cache_control"]; ok {
				count++
			}
			for _, nested := range typed {
				walk(nested)
			}
		case []any:
			for _, nested := range typed {
				walk(nested)
			}
		}
	}
	walk(object["system"])
	walk(object["messages"])
	walk(object["tools"])
	return count
}

// geminiThoughtSignature reads Google's thought signature from a part. The
// REST API spells it camelCase; snake_case is accepted for compatible servers.
func geminiThoughtSignature(part map[string]any) string {
	if value := stringAt(part, "thoughtSignature"); value != "" {
		return value
	}
	return stringAt(part, "thought_signature")
}

// mediaKind classifies inline media by MIME type. Audio and video get their own
// kinds: reporting them as documents produced invalid Anthropic document blocks
// (media_type: audio/*) with no diagnostic.
func mediaKind(mediaType string) PartKind {
	lowered := strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.HasPrefix(lowered, "image/"):
		return PartImage
	case strings.HasPrefix(lowered, "audio/"):
		return PartAudio
	case strings.HasPrefix(lowered, "video/"):
		return PartVideo
	}
	return PartDocument
}

func mustMarshal(value any) []byte {
	encoded, err := marshalJSON(value)
	if err != nil {
		return []byte(`{}`)
	}
	return encoded
}

// ---------------------------------------------------------------------------
// OpenAI Responses

func decodeResponsesRequest(body []byte, diagnostics *[]Diagnostic) (*Request, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model:       stringAt(object, "model"),
		Stream:      boolValue(object, "stream"),
		MaxTokens:   intPtr(object, "max_output_tokens"),
		Temperature: floatPtr(object, "temperature"),
		TopP:        floatPtr(object, "top_p"),
		Extra:       map[string]json.RawMessage{},
	}
	request.System = textParts(object["instructions"], diagnostics, "instructions")
	request.Tools = decodeResponsesTools(arrayAt(object, "tools"), diagnostics)
	request.ToolChoice = decodeResponsesToolChoice(object["tool_choice"], diagnostics)
	request.ParallelTools = boolPtr(object, "parallel_tool_calls")
	request.Metadata = stringMap(object, "metadata")
	request.User = stringAt(object, "user")
	request.ResponseFormat = responsesResponseFormat(object, diagnostics)
	request.Reasoning = responsesReasoning(object)

	for _, key := range []string{"store", "prompt_cache_key", "prompt_cache_retention", "truncation", "service_tier", "safety_identifier", "top_logprobs"} {
		if raw, ok := object[key]; ok {
			request.Extra[key] = rawObject(raw)
		}
	}

	messages, err := decodeResponsesInput(object["input"], diagnostics)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message.Role == RoleSystem && len(request.Messages) == 0 {
			// Only leading developer/system items are the system prompt; a
			// later one sits between user turns and must keep its position.
			request.System = append(request.System, message.Parts...)
			continue
		}
		request.Messages = append(request.Messages, message)
	}
	return request, nil
}

func decodeResponsesInput(input any, diagnostics *[]Diagnostic) ([]Message, error) {
	switch value := input.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []Message{{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: value}}}}, nil
	case []any:
		var messages []Message
		for index, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				appendDiagnostic(diagnostics, "invalid_input_item",
					"input item is not an object and was dropped", fmt.Sprintf("input[%d]", index))
				continue
			}
			itemPath := fmt.Sprintf("input[%d]", index)
			itemType := stringAt(item, "type")
			switch itemType {
			case "", "message":
				role := normalizeRole(stringAt(item, "role"))
				parts := decodeResponsesContent(item["content"], diagnostics, itemPath+".content")
				if len(parts) == 0 {
					continue
				}
				// System/developer items are folded into Request.System by the
				// caller; keep them as system messages for now so ordering is
				// visible to that pass.
				messages = append(messages, Message{Role: role, Parts: parts})
			case "function_call":
				call := Part{
					Kind:          PartToolCall,
					ToolCallID:    stringAt(item, "call_id"),
					ToolName:      stringAt(item, "name"),
					ArgumentsText: stringAt(item, "arguments"),
					Arguments:     parseArguments(stringAt(item, "arguments"), diagnostics, itemPath+".arguments"),
				}
				messages = appendAssistantToolCall(messages, call)
			case "custom_tool_call":
				input, _ := item["input"].(string)
				encoded := mustMarshal(map[string]any{"input": input})
				appendDiagnostic(diagnostics, "custom_tool_approximated",
					"custom tool call was approximated as a JSON function call", itemPath)
				messages = appendAssistantToolCall(messages, Part{
					Kind: PartToolCall, ToolCallID: stringAt(item, "call_id"),
					ToolName: stringAt(item, "name"), Arguments: encoded, ArgumentsText: string(encoded),
				})
			case "function_call_output", "custom_tool_call_output":
				content := decodeResponsesToolOutput(item["output"], diagnostics, itemPath+".output")
				messages = append(messages, Message{
					Role:  RoleTool,
					Parts: []Part{toolResultPart(stringAt(item, "call_id"), "", content, false)},
				})
			case "reasoning":
				parts := decodeResponsesReasoning(item, diagnostics, itemPath)
				if len(parts) == 0 {
					continue
				}
				messages = append(messages, Message{Role: RoleAssistant, Parts: parts})
			case "item_reference":
				appendDiagnostic(diagnostics, "item_reference_dropped",
					"item_reference cannot be resolved without the Responses store", itemPath)
			case "web_search_call", "file_search_call", "computer_call", "computer_call_output",
				"local_shell_call", "local_shell_call_output", "image_generation_call", "mcp_call", "mcp_list_tools", "mcp_approval_request":
				appendDiagnostic(diagnostics, "hosted_item_dropped",
					fmt.Sprintf("hosted Responses item %q cannot be represented", itemType), itemPath)
			default:
				if role := stringAt(item, "role"); role != "" {
					parts := decodeResponsesContent(item["content"], diagnostics, itemPath+".content")
					if len(parts) > 0 {
						messages = append(messages, Message{Role: normalizeRole(role), Parts: parts})
					}
					continue
				}
				appendDiagnostic(diagnostics, "input_item_dropped",
					fmt.Sprintf("input item type %q cannot be represented", itemType), itemPath)
			}
		}
		return messages, nil
	}
	return nil, conversionErrorf("invalid_input", "input must be a string or an array of items")
}

func decodeResponsesContent(content any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := content.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		var parts []Part
		for index, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			switch stringAt(item, "type") {
			case "input_text", "output_text", "text":
				if text := stringAt(item, "text"); text != "" {
					parts = append(parts, Part{Kind: PartText, Text: text})
				}
			case "refusal":
				if text := stringAt(item, "refusal"); text != "" {
					parts = append(parts, Part{Kind: PartRefusal, Text: text})
				}
			case "input_image":
				parts = append(parts, decodeImageURL(item["image_url"], diagnostics, itemPath)...)
			case "input_file":
				parts = append(parts, decodeFilePart(item, diagnostics, itemPath)...)
			case "output_audio":
				appendDiagnostic(diagnostics, "audio_dropped", "audio content cannot be represented", itemPath)
			default:
				appendDiagnostic(diagnostics, "content_part_dropped",
					fmt.Sprintf("content part type %q cannot be represented", stringAt(item, "type")), itemPath)
			}
		}
		return parts
	}
	return nil
}

func decodeResponsesToolOutput(output any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := output.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		return decodeResponsesContent(value, diagnostics, path)
	default:
		if output != nil {
			return []Part{{Kind: PartText, Text: string(mustMarshal(output))}}
		}
		return nil
	}
}

func decodeResponsesReasoning(item map[string]any, diagnostics *[]Diagnostic, path string) []Part {
	var parts []Part
	for _, raw := range arrayAt(item, "summary") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text := stringAt(entry, "text"); text != "" {
			parts = append(parts, Part{Kind: PartReasoning, Text: text})
		}
	}
	for _, raw := range arrayAt(item, "content") {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text := stringAt(entry, "text"); text != "" {
			parts = append(parts, Part{Kind: PartReasoning, Text: text})
		}
	}
	if len(parts) == 0 {
		// A reasoning item with only encrypted content cannot be represented;
		// it is intentionally dropped rather than becoming an empty message.
		if encrypted := stringAt(item, "encrypted_content"); encrypted != "" {
			appendDiagnostic(diagnostics, "reasoning_encrypted_dropped",
				"encrypted reasoning content has no equivalent in the target dialect", path)
		}
	}
	return parts
}

func decodeResponsesTools(raw []any, diagnostics *[]Diagnostic) []Tool {
	var tools []Tool
	for index, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName := stringAt(tool, "type")
		if typeName != "" && typeName != "function" {
			appendDiagnostic(diagnostics, "hosted_tool_dropped",
				fmt.Sprintf("Responses hosted tool %q cannot be represented as a function", typeName),
				fmt.Sprintf("tools[%d]", index))
			continue
		}
		name := stringAt(tool, "name")
		if name == "" {
			// Some providers nest the function object.
			function := objectAt(tool, "function")
			name = stringAt(function, "name")
			if name == "" {
				continue
			}
			tools = append(tools, Tool{
				Name:        name,
				Description: stringAt(function, "description"),
				Parameters:  rawObject(function["parameters"]),
				Strict:      boolPtr(function, "strict"),
			})
			continue
		}
		tools = append(tools, Tool{
			Name:        name,
			Description: stringAt(tool, "description"),
			Parameters:  rawObject(tool["parameters"]),
			Strict:      boolPtr(tool, "strict"),
		})
	}
	return tools
}

func decodeResponsesToolChoice(value any, diagnostics *[]Diagnostic) *ToolChoice {
	switch typed := value.(type) {
	case string:
		switch typed {
		case "auto", "none", "required":
			return &ToolChoice{Mode: ToolChoiceMode(typed)}
		}
	case map[string]any:
		switch stringAt(typed, "type") {
		case "function":
			name := stringAt(typed, "name")
			if name == "" {
				name = stringAt(objectAt(typed, "function"), "name")
			}
			if name != "" {
				return &ToolChoice{Mode: ToolChoiceTool, Name: name}
			}
			return &ToolChoice{Mode: ToolChoiceRequired}
		case "allowed_tools":
			mode := stringAt(typed, "mode")
			tools := arrayAt(typed, "tools")
			if len(tools) == 1 {
				if tool, ok := tools[0].(map[string]any); ok {
					name := stringAt(tool, "name")
					if name == "" {
						name = stringAt(objectAt(tool, "function"), "name")
					}
					if name != "" {
						return &ToolChoice{Mode: ToolChoiceTool, Name: name}
					}
				}
			}
			if mode == "required" {
				return &ToolChoice{Mode: ToolChoiceRequired}
			}
			return &ToolChoice{Mode: ToolChoiceAuto}
		case "none":
			return &ToolChoice{Mode: ToolChoiceNone}
		}
	}
	if value != nil {
		appendDiagnostic(diagnostics, "tool_choice_dropped", "tool_choice has an unsupported shape", "tool_choice")
	}
	return nil
}

func responsesResponseFormat(object map[string]any, diagnostics *[]Diagnostic) *ResponseFormat {
	text := objectAt(object, "text")
	if text == nil {
		return nil
	}
	format := objectAt(text, "format")
	if format == nil {
		return nil
	}
	result := &ResponseFormat{
		Kind:        FormatKind(stringAt(format, "type")),
		Name:        stringAt(format, "name"),
		Description: stringAt(format, "description"),
		Schema:      rawObject(format["schema"]),
		Strict:      boolPtr(format, "strict"),
	}
	if result.Kind == "" {
		return nil
	}
	if result.Kind == FormatJSONSchema && len(result.Schema) == 0 {
		appendDiagnostic(diagnostics, "response_format_schema_missing",
			"json_schema text.format has no schema", "text.format.schema")
	}
	return result
}

func responsesReasoning(object map[string]any) *ReasoningConfig {
	reasoning := objectAt(object, "reasoning")
	if reasoning == nil {
		return nil
	}
	config := &ReasoningConfig{
		Effort:  stringAt(reasoning, "effort"),
		Summary: stringAt(reasoning, "summary"),
	}
	if config.Effort == "" && config.Summary == "" {
		return nil
	}
	return config
}
