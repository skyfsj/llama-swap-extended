package codec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EncodeRequest renders the canonical request in the target dialect.
func EncodeRequest(request *Request, target Format, options ConvertOptions) ([]byte, []Diagnostic, error) {
	if request == nil {
		return nil, nil, conversionErrorf("invalid_request", "request is nil")
	}
	var diagnostics []Diagnostic
	var out map[string]any
	var err error
	switch target {
	case OpenAIChat:
		out, err = encodeChatRequest(request, options, &diagnostics)
	case OpenAIResponses:
		out, err = encodeResponsesRequest(request, options, &diagnostics)
	case ClaudeMessages:
		out, err = encodeClaudeRequest(request, options, &diagnostics)
	case Gemini:
		out, err = encodeGeminiRequest(request, options, &diagnostics)
	case GeminiInteractions:
		out, err = encodeInteractionsRequest(request, options, &diagnostics)
	default:
		return nil, diagnostics, conversionErrorf("unsupported_format", "unsupported request dialect %q", target)
	}
	if err != nil {
		return nil, diagnostics, err
	}
	encoded, err := marshalJSON(out)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

func targetModel(request *Request, options ConvertOptions) string {
	if options.Model != "" {
		return options.Model
	}
	return request.Model
}

// systemText flattens system parts into text, reporting non-text parts.
func systemText(parts []Part, diagnostics *[]Diagnostic, dialect string) string {
	var builder strings.Builder
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if builder.Len() > 0 {
				builder.WriteString("\n")
			}
			builder.WriteString(part.Text)
		default:
			appendDiagnostic(diagnostics, "system_part_dropped",
				fmt.Sprintf("%s system content cannot carry a %s part", dialect, part.Kind), "system")
		}
	}
	return builder.String()
}

// diagnoseSignatureLoss reports a Gemini thought signature that the target
// dialect cannot carry. Dropping it silently would break a later Gemini turn
// that replays this conversation.
func diagnoseSignatureLoss(part Part, dialect string, diagnostics *[]Diagnostic) {
	if part.ThoughtSignature == "" {
		return
	}
	appendDiagnostic(diagnostics, "thought_signature_dropped",
		"Gemini thought signature cannot be represented in "+dialect+"; a replay of this turn on Gemini may be rejected",
		"contents")
}

func applyTemperature(object map[string]any, request *Request) {
	if request.Temperature != nil {
		object["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		object["top_p"] = *request.TopP
	}
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions

func encodeChatRequest(request *Request, options ConvertOptions, diagnostics *[]Diagnostic) (map[string]any, error) {
	model := targetModel(request, options)
	if strings.TrimSpace(model) == "" {
		return nil, conversionErrorf("missing_model", "a target model name is required")
	}
	stream := request.Stream
	if options.Stream != nil {
		stream = *options.Stream
	}
	if request.CacheBreakpoints > 0 {
		appendDiagnostic(diagnostics, "cache_control_dropped",
			fmt.Sprintf("%d Anthropic cache_control breakpoint(s) cannot be represented; caching falls back to the upstream's automatic policy", request.CacheBreakpoints),
			"cache_control")
	}
	out := map[string]any{
		"model":    model,
		"messages": encodeChatMessages(request, diagnostics),
	}
	if stream {
		out["stream"] = true
		// Converted streams must ask for usage: every other dialect reports it
		// unconditionally, and OpenAI-compatible upstreams omit it otherwise.
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if request.MaxTokens != nil {
		out["max_tokens"] = *request.MaxTokens
	}
	applyTemperature(out, request)
	if request.TopK != nil {
		appendDiagnostic(diagnostics, "top_k_dropped", "OpenAI Chat has no top_k parameter", "top_k")
	}
	if request.N != nil {
		out["n"] = *request.N
	}
	if request.Seed != nil {
		out["seed"] = *request.Seed
	}
	if request.PresencePenalty != nil {
		out["presence_penalty"] = *request.PresencePenalty
	}
	if request.FrequencyPenalty != nil {
		out["frequency_penalty"] = *request.FrequencyPenalty
	}
	if len(request.Stop) > 0 {
		stop := request.Stop
		if len(stop) > 4 {
			appendDiagnostic(diagnostics, "stop_sequences_truncated",
				"OpenAI Chat accepts at most 4 stop sequences", "stop")
			stop = stop[:4]
		}
		out["stop"] = stop
	}
	if request.User != "" {
		out["user"] = request.User
	}
	if request.Metadata != nil {
		out["metadata"] = request.Metadata
	}
	if len(request.Tools) > 0 {
		out["tools"] = encodeChatTools(request.Tools)
	}
	if choice := encodeChatToolChoice(request.ToolChoice); choice != nil {
		out["tool_choice"] = choice
	}
	if request.ParallelTools != nil {
		out["parallel_tool_calls"] = *request.ParallelTools
	}
	if request.ToolChoice != nil && request.ToolChoice.DisableParallel != nil && *request.ToolChoice.DisableParallel {
		out["parallel_tool_calls"] = false
	}
	if request.ToolChoice != nil && request.ToolChoice.DisableParallel != nil && *request.ToolChoice.DisableParallel && request.Tools == nil {
		appendDiagnostic(diagnostics, "disable_parallel_without_tools",
			"disable_parallel_tool_use was requested with no tools", "tool_choice")
	}
	if format := encodeChatResponseFormat(request.ResponseFormat, diagnostics); format != nil {
		out["response_format"] = format
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		out["reasoning_effort"] = request.Reasoning.Effort
	}
	for key, raw := range request.Extra {
		switch key {
		case "logit_bias", "logprobs", "top_logprobs":
			var value any
			if json.Unmarshal(raw, &value) == nil {
				out[key] = value
			}
		case "cachedContent":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Gemini cachedContent names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "previous_interaction_id":
			appendDiagnostic(diagnostics, "field_dropped",
				"previous_interaction_id names stateful Gemini context this target cannot resolve; the referenced turns are not included",
				key)
		case "cached_content":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Interactions cached_content names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "background":
			appendDiagnostic(diagnostics, "field_dropped",
				"background interactions are not supported by this target", key)
		case "labels", "response_modalities":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of this target's request", key), key)
		case "prompt_cache_retention":
			appendDiagnostic(diagnostics, "field_dropped",
				"prompt_cache_retention is not part of the OpenAI Chat Completions request", key)
		case "store", "service_tier", "prompt_cache_key", "truncation", "safety_identifier":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the OpenAI Chat Completions request", key), key)
		}
	}
	return out, nil
}

func encodeChatMessages(request *Request, diagnostics *[]Diagnostic) []any {
	messages := make([]any, 0, len(request.Messages)+1)
	if system := systemText(request.System, diagnostics, "OpenAI Chat"); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			if text := systemText(message.Parts, diagnostics, "OpenAI Chat"); text != "" {
				messages = append(messages, map[string]any{"role": "system", "content": text})
			}
		case RoleTool:
			messages = append(messages, encodeChatToolMessages(message, diagnostics)...)
		case RoleAssistant:
			messages = append(messages, encodeChatAssistantMessage(message, diagnostics))
		default:
			// Anthropic (and Gemini) mix tool results into user turns; Chat
			// represents each result as its own role:"tool" message that must
			// precede the user content.
			var userParts []Part
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					userParts = append(userParts, part)
					continue
				}
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": part.ToolCallID,
					"content": encodeChatContent(part.Content, diagnostics, "tool content"),
				})
			}
			if len(userParts) > 0 {
				content := encodeChatContent(userParts, diagnostics, "user")
				if content != nil {
					messages = append(messages, map[string]any{"role": "user", "content": content})
				}
			}
		}
	}
	return messages
}

func encodeChatToolMessages(message Message, diagnostics *[]Diagnostic) []any {
	var messages []any
	for _, part := range message.Parts {
		if part.Kind != PartToolResult {
			appendDiagnostic(diagnostics, "message_part_dropped",
				fmt.Sprintf("a %s part cannot live in a tool message", part.Kind), "messages")
			continue
		}
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": part.ToolCallID,
			"content":      encodeChatContent(part.Content, diagnostics, "tool content"),
		})
	}
	return messages
}

func encodeChatAssistantMessage(message Message, diagnostics *[]Diagnostic) map[string]any {
	output := map[string]any{"role": "assistant"}
	var content []any
	var reasoning strings.Builder
	var refusal string
	var toolCalls []any
	for _, part := range message.Parts {
		switch part.Kind {
		case PartText:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case PartRefusal:
			refusal = part.Text
		case PartReasoning:
			if reasoning.Len() > 0 {
				reasoning.WriteString("\n")
			}
			reasoning.WriteString(part.Text)
		case PartToolCall:
			diagnoseSignatureLoss(part, "OpenAI Chat", diagnostics)
			toolCalls = append(toolCalls, map[string]any{
				"id":   part.ToolCallID,
				"type": "function",
				"function": map[string]any{
					"name":      part.ToolName,
					"arguments": argumentsText(part.Arguments),
				},
			})
		case PartImage, PartDocument, PartAudio, PartVideo:
			appendDiagnostic(diagnostics, "assistant_media_dropped",
				"assistant messages cannot carry media in Chat Completions", "messages")
		}
	}
	output["content"] = singleTextOrParts(content)
	if reasoning.Len() > 0 {
		output["reasoning_content"] = reasoning.String()
	}
	if refusal != "" {
		output["refusal"] = refusal
	}
	if len(toolCalls) > 0 {
		output["tool_calls"] = toolCalls
	}
	return output
}

// singleTextOrParts renders a content array as a plain string when it holds a
// single text part, matching the common wire shape.
func singleTextOrParts(parts []any) any {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok {
			if part["type"] == "text" {
				return part["text"]
			}
		}
	}
	return parts
}

func encodeChatContent(parts []Part, diagnostics *[]Diagnostic, context string) any {
	var content []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			}
		case PartRefusal:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case PartReasoning:
			appendDiagnostic(diagnostics, "reasoning_dropped",
				"reasoning content cannot be represented in this message", context)
		case PartImage:
			url := part.URL
			if part.Base64 != "" {
				url = dataURL(part.MediaType, part.Base64)
			}
			if url == "" {
				appendDiagnostic(diagnostics, "invalid_media",
					"image part has no data or url", context)
				continue
			}
			content = append(content, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url},
			})
		case PartDocument:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": part.Text})
				continue
			}
			appendDiagnostic(diagnostics, "document_dropped",
				"Chat Completions has no document content part", context)
		case PartAudio, PartVideo:
			// Chat Completions has no audio/video part; say so instead of
			// emitting an unusable media block.
			appendDiagnostic(diagnostics, string(part.Kind)+"_dropped",
				fmt.Sprintf("Chat Completions has no %s content part", part.Kind), context)
		case PartToolResult:
			appendDiagnostic(diagnostics, "tool_result_dropped",
				"a tool result outside a tool message cannot be represented", context)
		}
	}
	return singleTextOrParts(content)
}

func encodeChatTools(tools []Tool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		function := map[string]any{
			"name":       tool.Name,
			"parameters": rawJSONOrEmpty(tool.Parameters),
		}
		if tool.Description != "" {
			function["description"] = tool.Description
		}
		if tool.Strict != nil {
			function["strict"] = *tool.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

func encodeChatToolChoice(choice *ToolChoice) any {
	if choice == nil {
		return nil
	}
	switch choice.Mode {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		// Chat has no per-choice disable_parallel flag; the request-level
		// parallel_tool_calls switch is the equivalent (see encodeChatRequest).
		return string(choice.Mode)
	case ToolChoiceTool:
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		}
	}
	return nil
}

func encodeChatResponseFormat(format *ResponseFormat, diagnostics *[]Diagnostic) any {
	if format == nil {
		return nil
	}
	switch format.Kind {
	case FormatText:
		return map[string]any{"type": "text"}
	case FormatJSONObject:
		return map[string]any{"type": "json_object"}
	case FormatJSONSchema:
		schema := map[string]any{
			"name":   format.Name,
			"schema": rawJSONOrEmpty(format.Schema),
		}
		if schema["name"] == "" {
			schema["name"] = "response"
		}
		if format.Description != "" {
			schema["description"] = format.Description
		}
		if format.Strict != nil {
			schema["strict"] = *format.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": schema}
	}
	appendDiagnostic(diagnostics, "response_format_dropped",
		fmt.Sprintf("response format kind %q is unknown", format.Kind), "response_format")
	return nil
}

// ---------------------------------------------------------------------------
// Anthropic Messages

func encodeClaudeRequest(request *Request, options ConvertOptions, diagnostics *[]Diagnostic) (map[string]any, error) {
	model := targetModel(request, options)
	if strings.TrimSpace(model) == "" {
		return nil, conversionErrorf("missing_model", "a target model name is required")
	}
	if request.N != nil && *request.N > 1 {
		return nil, conversionErrorf("n_unsupported",
			"Anthropic Messages returns exactly one message; n=%d cannot be converted", *request.N)
	}
	maxTokens := 0
	if request.MaxTokens != nil {
		maxTokens = *request.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 4096
		appendDiagnostic(diagnostics, "max_tokens_defaulted",
			"Anthropic Messages requires max_tokens; 4096 was used", "max_tokens")
	}
	out := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   encodeClaudeMessages(request, diagnostics),
	}
	if system := systemText(request.System, diagnostics, "Anthropic Messages"); system != "" {
		out["system"] = system
	}
	stream := request.Stream
	if options.Stream != nil {
		stream = *options.Stream
	}
	if stream {
		out["stream"] = true
	}
	applyTemperature(out, request)
	if request.TopK != nil {
		out["top_k"] = *request.TopK
	}
	if len(request.Stop) > 0 {
		stop := request.Stop
		if len(stop) > 4 {
			appendDiagnostic(diagnostics, "stop_sequences_truncated",
				"Anthropic Messages accepts at most 4 stop sequences", "stop_sequences")
			stop = stop[:4]
		}
		out["stop_sequences"] = stop
	}
	if len(request.Tools) > 0 {
		out["tools"] = encodeClaudeTools(request.Tools)
	}
	disableParallel := request.ParallelTools != nil && !*request.ParallelTools
	if request.ToolChoice != nil && request.ToolChoice.DisableParallel != nil && *request.ToolChoice.DisableParallel {
		disableParallel = true
	}
	if choice := encodeClaudeToolChoice(request.ToolChoice, disableParallel); choice != nil {
		out["tool_choice"] = choice
	} else if disableParallel {
		// Anthropic applies the switch through tool_choice; synthesize the
		// auto variant the caller implied.
		out["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": true}
	}
	if request.Reasoning != nil {
		thinking := map[string]any{}
		switch {
		case request.Reasoning.Enabled != nil && !*request.Reasoning.Enabled:
			thinking["type"] = "disabled"
		case request.Reasoning.BudgetTokens != nil:
			thinking["type"] = "enabled"
			thinking["budget_tokens"] = *request.Reasoning.BudgetTokens
		default:
			thinking = nil
		}
		if thinking != nil {
			out["thinking"] = thinking
		}
	}
	outputConfig := map[string]any{}
	if format := encodeClaudeResponseFormat(request.ResponseFormat, diagnostics); format != nil {
		outputConfig["format"] = format
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		outputConfig["effort"] = request.Reasoning.Effort
	}
	if len(outputConfig) > 0 {
		out["output_config"] = outputConfig
	}
	if request.Metadata != nil {
		out["metadata"] = map[string]any{"user_id": request.Metadata["user_id"]}
	} else if request.User != "" {
		out["metadata"] = map[string]any{"user_id": request.User}
	}
	if request.Seed != nil {
		appendDiagnostic(diagnostics, "seed_dropped", "Anthropic Messages has no seed parameter", "seed")
	}
	if request.PresencePenalty != nil || request.FrequencyPenalty != nil {
		appendDiagnostic(diagnostics, "penalty_dropped",
			"Anthropic Messages has no presence/frequency penalty parameters", "penalty")
	}
	for key := range request.Extra {
		switch key {
		case "metadata":
			// Already represented through metadata.user_id.
		case "cachedContent":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Gemini cachedContent names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "previous_interaction_id":
			appendDiagnostic(diagnostics, "field_dropped",
				"previous_interaction_id names stateful Gemini context this target cannot resolve; the referenced turns are not included",
				key)
		case "cached_content":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Interactions cached_content names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "background":
			appendDiagnostic(diagnostics, "field_dropped",
				"background interactions are not supported by this target", key)
		case "labels", "response_modalities":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of this target's request", key), key)
		case "prompt_cache_retention":
			appendDiagnostic(diagnostics, "field_dropped",
				"prompt_cache_retention is not part of the Anthropic Messages request", key)
		case "prompt_cache_key":
			// Anthropic caches by explicit cache_control breakpoints; the OpenAI
			// cache key has no equivalent, and the client's breakpoints are what
			// actually drive caching here.
			appendDiagnostic(diagnostics, "prompt_cache_key_ignored",
				"Anthropic caches by cache_control breakpoints, not prompt_cache_key", key)
		default:
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the Anthropic Messages request", key), key)
		}
	}
	return out, nil
}

func encodeClaudeMessages(request *Request, diagnostics *[]Diagnostic) []any {
	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			// Callers fold system content; a mid-conversation system turn is
			// appended as user text rather than dropped.
			if text := systemText(message.Parts, diagnostics, "Anthropic Messages"); text != "" {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
		case RoleTool:
			blocks := make([]any, 0, len(message.Parts))
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					continue
				}
				content := encodeClaudeToolResultContent(part.Content, diagnostics)
				block := map[string]any{
					"type":        "tool_result",
					"tool_use_id": part.ToolCallID,
					"content":     content,
				}
				if part.IsError {
					block["is_error"] = true
				}
				blocks = append(blocks, block)
			}
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": blocks})
			}
		case RoleAssistant:
			blocks := encodeClaudeAssistantBlocks(message.Parts, diagnostics)
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			}
		default:
			blocks := encodeClaudeUserBlocks(message.Parts, diagnostics)
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": blocks})
			}
		}
	}
	return messages
}

func encodeClaudeAssistantBlocks(parts []Part, diagnostics *[]Diagnostic) []any {
	var blocks []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case PartReasoning:
			diagnoseSignatureLoss(part, "Anthropic Messages", diagnostics)
			if part.Redacted {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": part.Data})
				continue
			}
			block := map[string]any{"type": "thinking", "thinking": part.Text}
			if part.Signature != "" {
				block["signature"] = part.Signature
			}
			blocks = append(blocks, block)
		case PartToolCall:
			diagnoseSignatureLoss(part, "Anthropic Messages", diagnostics)
			input := part.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			var decoded any
			if json.Unmarshal(input, &decoded) != nil {
				decoded = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": part.ToolCallID, "name": part.ToolName, "input": decoded,
			})
		case PartRefusal:
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		}
	}
	return blocks
}

func encodeClaudeUserBlocks(parts []Part, diagnostics *[]Diagnostic) []any {
	var toolResults []any
	var blocks []any
	for _, part := range parts {
		switch part.Kind {
		case PartToolResult:
			content := encodeClaudeToolResultContent(part.Content, diagnostics)
			block := map[string]any{
				"type": "tool_result", "tool_use_id": part.ToolCallID, "content": content,
			}
			if part.IsError {
				block["is_error"] = true
			}
			// Anthropic requires tool_result blocks before other content.
			toolResults = append(toolResults, block)
		case PartText:
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case PartImage:
			blocks = append(blocks, encodeClaudeMedia(part, PartImage, diagnostics)...)
		case PartDocument:
			blocks = append(blocks, encodeClaudeMedia(part, PartDocument, diagnostics)...)
		case PartAudio, PartVideo:
			blocks = append(blocks, encodeClaudeMedia(part, part.Kind, diagnostics)...)
		case PartReasoning:
			appendDiagnostic(diagnostics, "reasoning_dropped",
				"reasoning content is only valid in assistant messages", "messages")
		}
	}
	return append(toolResults, blocks...)
}

func encodeClaudeToolResultContent(parts []Part, diagnostics *[]Diagnostic) any {
	if len(parts) == 0 {
		return ""
	}
	allText := true
	for _, part := range parts {
		if part.Kind != PartText {
			allText = false
			break
		}
	}
	if allText {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			texts = append(texts, part.Text)
		}
		return strings.Join(texts, "\n")
	}
	var blocks []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		case PartImage:
			blocks = append(blocks, encodeClaudeMedia(part, PartImage, diagnostics)...)
		default:
			appendDiagnostic(diagnostics, "tool_result_part_dropped",
				fmt.Sprintf("a %s part cannot live in a tool result", part.Kind), "tool_result")
		}
	}
	return blocks
}

func encodeClaudeMedia(part Part, kind PartKind, diagnostics *[]Diagnostic) []any {
	switch kind {
	case PartAudio, PartVideo:
		// Anthropic's document block accepts PDFs and text, not audio/video;
		// sending media_type: audio/* produced an upstream 400 with no hint of
		// where it came from.
		appendDiagnostic(diagnostics, string(kind)+"_dropped",
			fmt.Sprintf("Anthropic Messages has no %s content block", kind), "messages")
		return nil
	}
	blockType := "image"
	if kind == PartDocument {
		blockType = "document"
	}
	if part.Base64 != "" {
		return []any{map[string]any{
			"type": blockType,
			"source": map[string]any{
				"type": "base64", "media_type": mediaTypeOrDefault(part.MediaType), "data": part.Base64,
			},
		}}
	}
	if part.URL != "" {
		return []any{map[string]any{
			"type":   blockType,
			"source": map[string]any{"type": "url", "url": part.URL},
		}}
	}
	appendDiagnostic(diagnostics, "invalid_media", "media part has no data or url", blockType)
	return nil
}

func mediaTypeOrDefault(mediaType string) string {
	if mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

func encodeClaudeTools(tools []Tool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		entry := map[string]any{
			"name":         tool.Name,
			"input_schema": rawJSONOrEmpty(tool.Parameters),
		}
		if tool.Description != "" {
			entry["description"] = tool.Description
		}
		if tool.Strict != nil {
			entry["strict"] = *tool.Strict
		}
		out = append(out, entry)
	}
	return out
}

func encodeClaudeToolChoice(choice *ToolChoice, disableParallel bool) any {
	if choice == nil {
		return nil
	}
	object := map[string]any{}
	switch choice.Mode {
	case ToolChoiceAuto:
		object["type"] = "auto"
	case ToolChoiceNone:
		object["type"] = "none"
	case ToolChoiceRequired:
		object["type"] = "any"
	case ToolChoiceTool:
		object["type"] = "tool"
		object["name"] = choice.Name
	default:
		return nil
	}
	if disableParallel || (choice.DisableParallel != nil && *choice.DisableParallel) {
		object["disable_parallel_tool_use"] = true
	}
	return object
}

func encodeClaudeResponseFormat(format *ResponseFormat, diagnostics *[]Diagnostic) any {
	if format == nil {
		return nil
	}
	switch format.Kind {
	case FormatText:
		return nil
	case FormatJSONObject:
		appendDiagnostic(diagnostics, "response_format_approximated",
			"Anthropic has no json_object mode; a permissive object schema was used",
			"output_config.format")
		return map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}
	case FormatJSONSchema:
		return map[string]any{"type": "json_schema", "schema": rawJSONOrEmpty(format.Schema)}
	}
	appendDiagnostic(diagnostics, "response_format_dropped",
		fmt.Sprintf("response format kind %q is unknown", format.Kind), "output_config.format")
	return nil
}

// ---------------------------------------------------------------------------
// Gemini generateContent

func encodeGeminiRequest(request *Request, options ConvertOptions, diagnostics *[]Diagnostic) (map[string]any, error) {
	if request.CacheBreakpoints > 0 {
		appendDiagnostic(diagnostics, "cache_control_dropped",
			fmt.Sprintf("%d Anthropic cache_control breakpoint(s) cannot be represented; caching falls back to the upstream's automatic policy", request.CacheBreakpoints),
			"cache_control")
	}
	out := map[string]any{
		"contents": encodeGeminiContents(request, diagnostics),
	}
	if system := systemText(request.System, diagnostics, "Gemini"); system != "" {
		out["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": system}},
		}
	}
	generation := map[string]any{}
	if request.MaxTokens != nil {
		generation["maxOutputTokens"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		generation["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		generation["topP"] = *request.TopP
	}
	if request.TopK != nil {
		generation["topK"] = *request.TopK
	}
	if request.N != nil {
		generation["candidateCount"] = *request.N
	}
	if request.Seed != nil {
		generation["seed"] = *request.Seed
	}
	if request.PresencePenalty != nil {
		generation["presencePenalty"] = *request.PresencePenalty
	}
	if request.FrequencyPenalty != nil {
		generation["frequencyPenalty"] = *request.FrequencyPenalty
	}
	if len(request.Stop) > 0 {
		stop := request.Stop
		if len(stop) > 5 {
			appendDiagnostic(diagnostics, "stop_sequences_truncated",
				"Gemini accepts at most 5 stop sequences", "generationConfig.stopSequences")
			stop = stop[:5]
		}
		generation["stopSequences"] = stop
	}
	if request.ResponseFormat != nil {
		switch request.ResponseFormat.Kind {
		case FormatJSONObject:
			generation["responseMimeType"] = "application/json"
		case FormatJSONSchema:
			generation["responseMimeType"] = "application/json"
			generation["responseSchema"] = sanitizeGeminiSchema(request.ResponseFormat.Schema, diagnostics, "responseSchema")
		case FormatText:
		}
	}
	if request.Reasoning != nil {
		thinking := map[string]any{}
		switch {
		case request.Reasoning.BudgetTokens != nil:
			thinking["thinkingBudget"] = *request.Reasoning.BudgetTokens
		case request.Reasoning.Effort != "":
			budget, ok := geminiThinkingBudget(request.Reasoning.Effort)
			if ok {
				thinking["thinkingBudget"] = budget
				appendDiagnostic(diagnostics, "reasoning_effort_mapped",
					fmt.Sprintf("reasoning effort %q was mapped to thinkingBudget %d", request.Reasoning.Effort, budget),
					"generationConfig.thinkingConfig")
			}
		case request.Reasoning.Enabled != nil && !*request.Reasoning.Enabled:
			thinking["thinkingBudget"] = 0
		}
		if len(thinking) > 0 {
			generation["thinkingConfig"] = thinking
		}
	}
	if len(generation) > 0 {
		out["generationConfig"] = generation
	}
	if len(request.Tools) > 0 {
		declarations := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			declaration := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				declaration["description"] = tool.Description
			}
			if len(tool.Parameters) > 0 {
				declaration["parameters"] = sanitizeGeminiSchema(tool.Parameters, diagnostics, "tools."+tool.Name+".parameters")
			}
			declarations = append(declarations, declaration)
		}
		out["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	if config := encodeGeminiToolChoice(request.ToolChoice); config != nil {
		out["toolConfig"] = config
	}
	if request.ParallelTools != nil || (request.ToolChoice != nil && request.ToolChoice.DisableParallel != nil && *request.ToolChoice.DisableParallel) {
		appendDiagnostic(diagnostics, "parallel_tool_calls_ignored",
			"Gemini has no parallel_tool_calls switch", "parallel_tool_calls")
	}
	for key := range request.Extra {
		switch key {
		case "cachedContent":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Gemini cachedContent names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "previous_interaction_id":
			appendDiagnostic(diagnostics, "field_dropped",
				"previous_interaction_id names stateful Gemini context this target cannot resolve; the referenced turns are not included",
				key)
		case "cached_content":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Interactions cached_content names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "background":
			appendDiagnostic(diagnostics, "field_dropped",
				"background interactions are not supported by this target", key)
		case "labels", "response_modalities":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of this target's request", key), key)
		case "prompt_cache_retention":
			appendDiagnostic(diagnostics, "field_dropped",
				"prompt_cache_retention is not part of the Gemini generateContent request", key)
		case "logit_bias", "logprobs", "top_logprobs", "store", "service_tier", "prompt_cache_key", "truncation", "safety_identifier":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the Gemini generateContent request", key), key)
		}
	}
	return out, nil
}

func geminiThinkingBudget(effort string) (int, bool) {
	switch strings.ToLower(effort) {
	case "minimal", "none":
		return 0, true
	case "low":
		return 1024, true
	case "medium":
		return 8192, true
	case "high", "xhigh", "max":
		return 24576, true
	}
	return 0, false
}

func encodeGeminiContents(request *Request, diagnostics *[]Diagnostic) []any {
	// One map for the whole conversation: rebuilding it per message made the
	// conversion O(n^2) for long chats.
	callNames := toolCallNames(request)
	var contents []any
	for _, message := range request.Messages {
		role := "user"
		switch message.Role {
		case RoleAssistant:
			role = "model"
		case RoleSystem:
			// System content belongs in systemInstruction; preserve a
			// mid-conversation system turn as user text.
			if text := systemText(message.Parts, diagnostics, "Gemini"); text != "" {
				contents = appendGeminiContent(contents, role, []any{map[string]any{"text": text}})
			}
			continue
		}
		parts := encodeGeminiParts(message.Parts, callNames, diagnostics)
		if len(parts) > 0 {
			contents = appendGeminiContent(contents, role, parts)
		}
	}
	return contents
}

// appendGeminiContent merges consecutive same-role turns, which Gemini accepts
// but some compatible servers reject.
func appendGeminiContent(contents []any, role string, parts []any) []any {
	if len(contents) > 0 {
		if last, ok := contents[len(contents)-1].(map[string]any); ok && last["role"] == role {
			existing, _ := last["parts"].([]any)
			last["parts"] = append(existing, parts...)
			return contents
		}
	}
	return append(contents, map[string]any{"role": role, "parts": parts})
}

func encodeGeminiParts(parts []Part, callNames map[string]string, diagnostics *[]Diagnostic) []any {
	var out []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				out = append(out, map[string]any{"text": part.Text})
			}
		case PartReasoning:
			block := map[string]any{"thought": true}
			if part.Text != "" {
				block["text"] = part.Text
			}
			if part.ThoughtSignature != "" {
				block["thoughtSignature"] = part.ThoughtSignature
			}
			if len(block) > 1 {
				out = append(out, block)
			}
		case PartImage, PartDocument, PartAudio, PartVideo:
			if part.Base64 != "" {
				out = append(out, map[string]any{
					"inlineData": map[string]any{
						"mimeType": mediaTypeOrDefault(part.MediaType), "data": part.Base64,
					},
				})
				continue
			}
			if part.URL != "" {
				if isGeminiFileURI(part.URL) {
					out = append(out, map[string]any{
						"fileData": map[string]any{
							"mimeType": mediaTypeOrDefault(part.MediaType), "fileUri": part.URL,
						},
					})
					continue
				}
				appendDiagnostic(diagnostics, "media_url_dropped",
					"Gemini cannot fetch an arbitrary media URL; upload the file or inline it", "contents")
				continue
			}
			appendDiagnostic(diagnostics, "invalid_media", "media part has no data or url", "contents")
		case PartToolCall:
			args := part.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			var decoded any
			if json.Unmarshal(args, &decoded) != nil {
				decoded = map[string]any{}
			}
			block := map[string]any{
				"functionCall": map[string]any{"name": part.ToolName, "args": decoded},
			}
			// Gemini 3 rejects a replayed function call whose thought
			// signature was dropped.
			if part.ThoughtSignature != "" {
				block["thoughtSignature"] = part.ThoughtSignature
			}
			out = append(out, block)
		case PartToolResult:
			name := part.ToolName
			if name == "" {
				name = callNames[part.ToolCallID]
			}
			if name == "" {
				appendDiagnostic(diagnostics, "tool_result_name_missing",
					"Gemini functionResponse requires the function name", "contents")
				continue
			}
			out = append(out, map[string]any{
				"functionResponse": map[string]any{
					"name": name, "response": geminiToolResponse(part),
				},
			})
		case PartRefusal:
			if part.Text != "" {
				out = append(out, map[string]any{"text": part.Text})
			}
		}
	}
	return out
}

func geminiToolResponse(part Part) map[string]any {
	if len(part.Content) == 0 {
		return map[string]any{"result": ""}
	}
	if len(part.Content) == 1 && part.Content[0].Kind == PartText {
		return map[string]any{"result": part.Content[0].Text}
	}
	var items []any
	for _, nested := range part.Content {
		if nested.Kind == PartText {
			items = append(items, nested.Text)
		}
	}
	if len(items) == 1 {
		return map[string]any{"result": items[0]}
	}
	return map[string]any{"result": items}
}

func toolCallNames(request *Request) map[string]string {
	names := map[string]string{}
	for _, message := range request.Messages {
		for _, part := range message.Parts {
			if part.Kind == PartToolCall && part.ToolCallID != "" {
				names[part.ToolCallID] = part.ToolName
			}
		}
	}
	return names
}

func isGeminiFileURI(url string) bool {
	return strings.HasPrefix(url, "gs://") || strings.Contains(url, "generativelanguage.googleapis.com/")
}

func encodeGeminiToolChoice(choice *ToolChoice) any {
	if choice == nil {
		return nil
	}
	config := map[string]any{}
	switch choice.Mode {
	case ToolChoiceAuto:
		config["mode"] = "AUTO"
	case ToolChoiceNone:
		config["mode"] = "NONE"
	case ToolChoiceRequired:
		config["mode"] = "ANY"
	case ToolChoiceTool:
		config["mode"] = "ANY"
		config["allowedFunctionNames"] = []string{choice.Name}
	default:
		return nil
	}
	return map[string]any{"functionCallingConfig": config}
}

// geminiSchemaKeys is the OpenAPI-subset vocabulary Gemini accepts for
// function parameters and response schemas. Unknown JSON Schema keywords make
// the upstream reject the request, so they are stripped with a diagnostic.
var geminiSchemaKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "maxItems": true, "minItems": true,
	"properties": true, "required": true, "items": true, "minProperties": true,
	"maxProperties": true, "minLength": true, "maxLength": true, "pattern": true,
	"example": true, "anyOf": true, "propertyOrdering": true, "default": true,
	"minimum": true, "maximum": true,
}

func sanitizeGeminiSchema(raw json.RawMessage, diagnostics *[]Diagnostic, path string) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		appendDiagnostic(diagnostics, "schema_invalid", "schema is not a JSON object", path)
		return map[string]any{}
	}
	return sanitizeGeminiSchemaMap(schema, diagnostics, path)
}

func sanitizeGeminiSchemaMap(schema map[string]any, diagnostics *[]Diagnostic, path string) map[string]any {
	out := make(map[string]any, len(schema))
	for key, value := range schema {
		switch key {
		case "properties":
			properties, ok := value.(map[string]any)
			if !ok {
				continue
			}
			clean := make(map[string]any, len(properties))
			for name, rawProperty := range properties {
				if property, ok := rawProperty.(map[string]any); ok {
					clean[name] = sanitizeGeminiSchemaMap(property, diagnostics, path+".properties."+name)
				}
			}
			out[key] = clean
		case "items":
			if item, ok := value.(map[string]any); ok {
				out[key] = sanitizeGeminiSchemaMap(item, diagnostics, path+".items")
			}
		case "anyOf":
			list, ok := value.([]any)
			if !ok {
				continue
			}
			clean := make([]any, 0, len(list))
			for index, entry := range list {
				if item, ok := entry.(map[string]any); ok {
					clean = append(clean, sanitizeGeminiSchemaMap(item, diagnostics, fmt.Sprintf("%s.anyOf[%d]", path, index)))
				}
			}
			out[key] = clean
		default:
			if !geminiSchemaKeys[key] {
				appendDiagnostic(diagnostics, "schema_keyword_dropped",
					fmt.Sprintf("JSON Schema keyword %q is not supported by Gemini", key), path+"."+key)
				continue
			}
			out[key] = value
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenAI Responses

func encodeResponsesRequest(request *Request, options ConvertOptions, diagnostics *[]Diagnostic) (map[string]any, error) {
	model := targetModel(request, options)
	if strings.TrimSpace(model) == "" {
		return nil, conversionErrorf("missing_model", "a target model name is required")
	}
	if request.N != nil && *request.N > 1 {
		return nil, conversionErrorf("n_unsupported",
			"OpenAI Responses returns exactly one response; n=%d cannot be converted", *request.N)
	}
	if request.CacheBreakpoints > 0 {
		appendDiagnostic(diagnostics, "cache_control_dropped",
			fmt.Sprintf("%d Anthropic cache_control breakpoint(s) cannot be represented; caching falls back to the upstream's automatic policy", request.CacheBreakpoints),
			"cache_control")
	}
	stream := request.Stream
	if options.Stream != nil {
		stream = *options.Stream
	}
	out := map[string]any{
		"model": model,
		"input": encodeResponsesInput(request, diagnostics),
	}
	if stream {
		out["stream"] = true
	}
	if system := systemText(request.System, diagnostics, "OpenAI Responses"); system != "" {
		out["instructions"] = system
	}
	if request.MaxTokens != nil {
		out["max_output_tokens"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		out["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		out["top_p"] = *request.TopP
	}
	if len(request.Tools) > 0 {
		out["tools"] = encodeResponsesTools(request.Tools)
	}
	if choice := encodeResponsesToolChoice(request.ToolChoice); choice != nil {
		out["tool_choice"] = choice
	}
	if request.ParallelTools != nil {
		out["parallel_tool_calls"] = *request.ParallelTools
	}
	if request.ToolChoice != nil && request.ToolChoice.DisableParallel != nil && *request.ToolChoice.DisableParallel {
		out["parallel_tool_calls"] = false
	}
	if format := encodeResponsesResponseFormat(request.ResponseFormat); format != nil {
		out["text"] = map[string]any{"format": format}
	}
	if request.Reasoning != nil {
		reasoning := map[string]any{}
		if request.Reasoning.Effort != "" {
			reasoning["effort"] = request.Reasoning.Effort
		}
		if request.Reasoning.Summary != "" {
			reasoning["summary"] = request.Reasoning.Summary
		}
		if len(reasoning) > 0 {
			out["reasoning"] = reasoning
		}
	}
	if request.Metadata != nil {
		out["metadata"] = request.Metadata
	}
	if request.User != "" {
		out["user"] = request.User
	}
	if request.TopK != nil {
		appendDiagnostic(diagnostics, "top_k_dropped", "OpenAI Responses has no top_k parameter", "top_k")
	}
	if request.PresencePenalty != nil || request.FrequencyPenalty != nil {
		appendDiagnostic(diagnostics, "penalty_dropped",
			"OpenAI Responses has no presence/frequency penalty parameters", "penalty")
	}
	if request.Seed != nil {
		appendDiagnostic(diagnostics, "seed_dropped", "OpenAI Responses has no seed parameter", "seed")
	}
	if len(request.Stop) > 0 {
		appendDiagnostic(diagnostics, "stop_dropped", "OpenAI Responses has no stop parameter", "stop")
	}
	for key, raw := range request.Extra {
		switch key {
		case "store", "prompt_cache_key", "prompt_cache_retention", "truncation", "service_tier", "safety_identifier", "top_logprobs":
			var value any
			if json.Unmarshal(raw, &value) == nil {
				out[key] = value
			}
		case "previous_interaction_id":
			appendDiagnostic(diagnostics, "field_dropped",
				"previous_interaction_id names stateful Gemini context this target cannot resolve; the referenced turns are not included",
				key)
		case "cached_content":
			appendDiagnostic(diagnostics, "cached_content_dropped",
				"Interactions cached_content names a server-side cache this target cannot resolve; the cached prefix is not included",
				key)
		case "background":
			appendDiagnostic(diagnostics, "field_dropped",
				"background interactions are not supported by this target", key)
		case "labels", "response_modalities":
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of this target's request", key), key)
		default:
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the OpenAI Responses request", key), key)
		}
	}
	return out, nil
}

func encodeResponsesInput(request *Request, diagnostics *[]Diagnostic) []any {
	var input []any
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			text := systemText(message.Parts, diagnostics, "OpenAI Responses")
			if text != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "developer",
					"content": []any{map[string]any{"type": "input_text", "text": text}},
				})
			}
		case RoleTool:
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					continue
				}
				input = append(input, map[string]any{
					"type": "function_call_output", "call_id": part.ToolCallID,
					"output": encodeResponsesToolOutput(part.Content, diagnostics),
				})
			}
		case RoleAssistant:
			input = append(input, encodeResponsesAssistantItems(message.Parts, diagnostics)...)
		default:
			// Anthropic and Gemini put tool results inside user turns. A
			// Responses transcript needs each of them as its own
			// function_call_output item, before the remaining user content.
			var userParts []Part
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					userParts = append(userParts, part)
					continue
				}
				input = append(input, map[string]any{
					"type": "function_call_output", "call_id": part.ToolCallID,
					"output": encodeResponsesToolOutput(part.Content, diagnostics),
				})
			}
			if len(userParts) > 0 {
				content := encodeResponsesUserContent(userParts, diagnostics)
				if len(content) > 0 {
					input = append(input, map[string]any{
						"type": "message", "role": "user", "content": content,
					})
				}
			}
		}
	}
	return input
}

func encodeResponsesAssistantItems(parts []Part, diagnostics *[]Diagnostic) []any {
	var items []any
	var content []any
	// Flush visible content as one message item before any later reasoning or
	// tool item so the transcript keeps its original order.
	flushContent := func() {
		if len(content) == 0 {
			return
		}
		items = append(items, map[string]any{
			"type": "message", "role": "assistant", "content": content,
		})
		content = nil
	}
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "output_text", "text": part.Text, "annotations": []any{}})
			}
		case PartRefusal:
			content = append(content, map[string]any{"type": "refusal", "refusal": part.Text})
		case PartReasoning:
			flushContent()
			summary := []any{}
			if part.Text != "" {
				summary = append(summary, map[string]any{"type": "summary_text", "text": part.Text})
			}
			items = append(items, map[string]any{
				"type": "reasoning", "summary": summary,
			})
		case PartToolCall:
			diagnoseSignatureLoss(part, "OpenAI Responses", diagnostics)
			flushContent()
			items = append(items, map[string]any{
				"type": "function_call", "call_id": part.ToolCallID,
				"name": part.ToolName, "arguments": argumentsText(part.Arguments),
			})
		case PartImage, PartDocument, PartAudio, PartVideo:
			appendDiagnostic(diagnostics, "assistant_media_dropped",
				"assistant output in Responses cannot carry input media", "input")
		}
	}
	flushContent()
	return items
}

func encodeResponsesUserContent(parts []Part, diagnostics *[]Diagnostic) []any {
	var content []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "input_text", "text": part.Text})
			}
		case PartImage:
			url := part.URL
			if part.Base64 != "" {
				url = dataURL(part.MediaType, part.Base64)
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": url})
		case PartDocument:
			url := part.URL
			if part.Base64 != "" {
				url = dataURL(part.MediaType, part.Base64)
			}
			if url != "" {
				content = append(content, map[string]any{"type": "input_file", "file_url": url})
			} else {
				appendDiagnostic(diagnostics, "document_dropped",
					"document has no data or url", "input")
			}
		case PartToolResult:
			appendDiagnostic(diagnostics, "tool_result_dropped",
				"tool results must be emitted as function_call_output items", "input")
		case PartAudio:
			// Responses carries audio as input_audio with its own format field,
			// which the canonical part does not model yet.
			appendDiagnostic(diagnostics, "audio_dropped",
				"Responses input_audio requires a format field the conversion does not model", "input")
		case PartVideo:
			appendDiagnostic(diagnostics, "video_dropped",
				"OpenAI Responses has no video content part", "input")
		case PartReasoning:
			appendDiagnostic(diagnostics, "reasoning_dropped",
				"reasoning is only valid in assistant turns", "input")
		}
	}
	return content
}

func encodeResponsesToolOutput(parts []Part, diagnostics *[]Diagnostic) any {
	if len(parts) == 0 {
		return ""
	}
	allText := true
	for _, part := range parts {
		if part.Kind != PartText {
			allText = false
			break
		}
	}
	if allText {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			texts = append(texts, part.Text)
		}
		return strings.Join(texts, "\n")
	}
	var content []any
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			content = append(content, map[string]any{"type": "input_text", "text": part.Text})
		case PartImage:
			url := part.URL
			if part.Base64 != "" {
				url = dataURL(part.MediaType, part.Base64)
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": url})
		default:
			appendDiagnostic(diagnostics, "tool_result_part_dropped",
				fmt.Sprintf("a %s part cannot live in a tool output", part.Kind), "function_call_output")
		}
	}
	return content
}

func encodeResponsesTools(tools []Tool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		entry := map[string]any{
			"type": "function", "name": tool.Name,
			"parameters": rawJSONOrEmpty(tool.Parameters),
		}
		if tool.Description != "" {
			entry["description"] = tool.Description
		}
		if tool.Strict != nil {
			entry["strict"] = *tool.Strict
		}
		out = append(out, entry)
	}
	return out
}

func encodeResponsesToolChoice(choice *ToolChoice) any {
	if choice == nil {
		return nil
	}
	switch choice.Mode {
	case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		return string(choice.Mode)
	case ToolChoiceTool:
		return map[string]any{"type": "function", "name": choice.Name}
	}
	return nil
}

func encodeResponsesResponseFormat(format *ResponseFormat) any {
	if format == nil {
		return nil
	}
	switch format.Kind {
	case FormatText:
		return map[string]any{"type": "text"}
	case FormatJSONObject:
		return map[string]any{"type": "json_object"}
	case FormatJSONSchema:
		entry := map[string]any{"type": "json_schema", "schema": rawJSONOrEmpty(format.Schema)}
		if format.Name != "" {
			entry["name"] = format.Name
		} else {
			// Responses requires a name for json_schema formats.
			entry["name"] = "response"
		}
		if format.Description != "" {
			entry["description"] = format.Description
		}
		if format.Strict != nil {
			entry["strict"] = *format.Strict
		}
		return entry
	}
	return nil
}
