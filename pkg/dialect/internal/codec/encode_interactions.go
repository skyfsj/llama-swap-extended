package codec

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// encodeInteractionsRequest renders the canonical request in the Interactions
// dialect. This is used when the upstream speaks Interactions and the client
// speaks something else.
func encodeInteractionsRequest(request *Request, options ConvertOptions, diagnostics *[]Diagnostic) (map[string]any, error) {
	model := targetModel(request, options)
	if strings.TrimSpace(model) == "" {
		return nil, conversionErrorf("missing_model", "a target model name is required")
	}
	stream := request.Stream
	if options.Stream != nil {
		stream = *options.Stream
	}
	out := map[string]any{
		"model": model,
		"input": encodeInteractionsInput(request, diagnostics),
	}
	if stream {
		out["stream"] = true
	}
	if system := systemText(request.System, diagnostics, "Interactions"); system != "" {
		out["system_instruction"] = system
	}
	if request.CacheBreakpoints > 0 {
		appendDiagnostic(diagnostics, "cache_control_dropped",
			fmt.Sprintf("%d Anthropic cache_control breakpoint(s) cannot be represented; use cached_content or the upstream's automatic policy instead", request.CacheBreakpoints),
			"cache_control")
	}
	generation := map[string]any{}
	if request.MaxTokens != nil {
		generation["max_output_tokens"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		generation["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		generation["top_p"] = *request.TopP
	}
	if request.Seed != nil {
		generation["seed"] = *request.Seed
	}
	if len(request.Stop) > 0 {
		stop := request.Stop
		if len(stop) > 5 {
			appendDiagnostic(diagnostics, "stop_sequences_truncated",
				"Interactions accepts at most 5 stop sequences", "generation_config.stop_sequences")
			stop = stop[:5]
		}
		generation["stop_sequences"] = stop
	}
	if request.Reasoning != nil {
		// thinking_level accepts exactly minimal|low|medium|high.
		switch strings.ToLower(request.Reasoning.Effort) {
		case "minimal", "none":
			generation["thinking_level"] = "minimal"
		case "low":
			generation["thinking_level"] = "low"
		case "medium":
			generation["thinking_level"] = "medium"
		case "high", "xhigh", "max":
			generation["thinking_level"] = "high"
		}
		if request.Reasoning.Summary != "" {
			generation["thinking_summaries"] = request.Reasoning.Summary
		}
	}
	if choice := encodeInteractionsToolChoice(request.ToolChoice); choice != nil {
		generation["tool_choice"] = choice
	}
	if request.TopK != nil {
		appendDiagnostic(diagnostics, "top_k_dropped", "Interactions has no top_k parameter", "top_k")
	}
	if request.PresencePenalty != nil || request.FrequencyPenalty != nil {
		appendDiagnostic(diagnostics, "penalty_dropped",
			"Interactions has no presence/frequency penalty parameters", "penalty")
	}
	if len(generation) > 0 {
		out["generation_config"] = generation
	}
	if len(request.Tools) > 0 {
		out["tools"] = encodeInteractionsTools(request.Tools)
	}
	if format := encodeInteractionsResponseFormat(request.ResponseFormat, diagnostics); format != nil {
		out["response_format"] = format
	}
	if request.N != nil && *request.N > 1 {
		appendDiagnostic(diagnostics, "n_ignored", "Interactions returns one interaction per request", "n")
	}
	// Stateful/cache fields pass through when they came from the same dialect.
	for key, raw := range request.Extra {
		switch key {
		case "previous_interaction_id", "cached_content", "store", "background", "labels",
			"service_tier", "response_modalities":
			var value any
			if json.Unmarshal(raw, &value) == nil {
				out[key] = value
			}
		case "prompt_cache_key", "prompt_cache_retention":
			// Interactions caches through cached_content, not an OpenAI key.
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the Interactions request", key), key)
		default:
			appendDiagnostic(diagnostics, "field_dropped",
				fmt.Sprintf("%s is not part of the Interactions request", key), key)
		}
	}
	return out, nil
}

// encodeInteractionsInput renders the canonical conversation as Interactions
// Steps: the model's own turns become model_output/function_call/thought steps
// and the tool results become function_result steps, which is what a stateful
// replay expects.
func encodeInteractionsInput(request *Request, diagnostics *[]Diagnostic) []any {
	input := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			text := systemText(message.Parts, diagnostics, "Interactions")
			if text != "" {
				input = append(input, map[string]any{
					"type":    "user_input",
					"content": []any{map[string]any{"type": "text", "text": text}},
				})
			}
		case RoleUser:
			// Anthropic and Gemini put tool results inside user turns. An
			// Interactions transcript needs each of them as its own
			// function_result step, before the remaining user content.
			var userParts []Part
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					userParts = append(userParts, part)
					continue
				}
				step := map[string]any{
					"type": "function_result", "call_id": part.ToolCallID,
					"result": interactionsFunctionResult(part.Content),
				}
				if part.ToolName != "" {
					step["name"] = part.ToolName
				}
				if part.IsError {
					step["is_error"] = true
				}
				input = append(input, step)
			}
			content := encodeInteractionsContent(userParts, diagnostics)
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "user_input", "content": content})
			}
		case RoleAssistant:
			input = append(input, encodeInteractionsAssistantSteps(message.Parts, diagnostics)...)
		case RoleTool:
			for _, part := range message.Parts {
				if part.Kind != PartToolResult {
					continue
				}
				step := map[string]any{
					"type": "function_result", "call_id": part.ToolCallID,
					"result": interactionsFunctionResult(part.Content),
				}
				if part.ToolName != "" {
					step["name"] = part.ToolName
				}
				if part.IsError {
					step["is_error"] = true
				}
				input = append(input, step)
			}
		}
	}
	return input
}

func encodeInteractionsAssistantSteps(parts []Part, diagnostics *[]Diagnostic) []any {
	var steps []any
	var content []any
	flushContent := func() {
		if len(content) == 0 {
			return
		}
		steps = append(steps, map[string]any{"type": "model_output", "content": content})
		content = nil
	}
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			}
		case PartRefusal:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case PartReasoning:
			flushContent()
			step := map[string]any{"type": "thought"}
			if part.Text != "" {
				step["summary"] = []any{map[string]any{"type": "text", "text": part.Text}}
			}
			if part.ThoughtSignature != "" {
				step["signature"] = part.ThoughtSignature
			}
			steps = append(steps, step)
		case PartToolCall:
			flushContent()
			arguments := part.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			var decoded any
			if json.Unmarshal(arguments, &decoded) != nil {
				decoded = map[string]any{}
			}
			step := map[string]any{
				"type": "function_call", "name": part.ToolName, "arguments": decoded,
			}
			if part.ToolCallID != "" {
				step["id"] = part.ToolCallID
			}
			if part.ThoughtSignature != "" {
				step["signature"] = part.ThoughtSignature
			}
			steps = append(steps, step)
		case PartImage, PartDocument, PartAudio, PartVideo:
			media := encodeInteractionsMedia(part)
			if media != nil {
				content = append(content, media)
			}
		}
	}
	flushContent()
	return steps
}

func encodeInteractionsContent(parts []Part, diagnostics *[]Diagnostic) []any {
	content := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			}
		case PartImage, PartDocument, PartAudio, PartVideo:
			if media := encodeInteractionsMedia(part); media != nil {
				content = append(content, media)
			}
		case PartToolResult:
			appendDiagnostic(diagnostics, "tool_result_dropped",
				"a tool result must be emitted as a function_result step", "input")
		case PartReasoning:
			appendDiagnostic(diagnostics, "reasoning_dropped",
				"reasoning belongs in a thought step of an assistant turn", "input")
		}
	}
	return content
}

func encodeInteractionsMedia(part Part) map[string]any {
	kind := string(part.Kind)
	entry := map[string]any{"type": kind}
	if part.MediaType != "" {
		entry["mime_type"] = part.MediaType
	}
	if part.Base64 != "" {
		entry["data"] = part.Base64
		return entry
	}
	if part.URL != "" {
		entry["uri"] = part.URL
		return entry
	}
	return nil
}

func interactionsFunctionResult(parts []Part) any {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && parts[0].Kind == PartText {
		return parts[0].Text
	}
	// FunctionResultStepResult is a union of a bare subcontent list or a string;
	// the list is not wrapped in an object.
	subcontents := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			subcontents = append(subcontents, map[string]any{"type": "text", "text": part.Text})
		default:
			if media := encodeInteractionsMedia(part); media != nil {
				subcontents = append(subcontents, media)
			}
		}
	}
	return subcontents
}

// encodeInteractionsTools renders callable tools in the Interactions shape: a
// flat object whose discriminator is type:"function", with name/description/
// parameters at the top level (the SDK rejects a nested {"function": {...}}).
func encodeInteractionsTools(tools []Tool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		entry := map[string]any{"type": "function", "name": tool.Name}
		if tool.Description != "" {
			entry["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 {
			entry["parameters"] = rawJSONOrEmpty(tool.Parameters)
		}
		out = append(out, entry)
	}
	return out
}

// encodeInteractionsToolChoice renders the Interactions ToolChoiceConfig:
// {"allowed_tools": {"mode": "auto"|"any"|"none", "tools": [names]}}.
func encodeInteractionsToolChoice(choice *ToolChoice) any {
	if choice == nil {
		return nil
	}
	allowed := map[string]any{}
	switch choice.Mode {
	case ToolChoiceAuto:
		allowed["mode"] = "auto"
	case ToolChoiceNone:
		allowed["mode"] = "none"
	case ToolChoiceRequired:
		allowed["mode"] = "any"
	case ToolChoiceTool:
		allowed["mode"] = "any"
		allowed["tools"] = []string{choice.Name}
	default:
		return nil
	}
	return map[string]any{"allowed_tools": allowed}
}

// encodeInteractionsResponseFormat renders a TextResponseFormat: the JSON
// constraint lives in mime_type plus an optional schema, not in a json_schema
// type discriminator.
func encodeInteractionsResponseFormat(format *ResponseFormat, diagnostics *[]Diagnostic) map[string]any {
	if format == nil {
		return nil
	}
	switch format.Kind {
	case FormatText:
		return map[string]any{"type": "text", "mime_type": "text/plain"}
	case FormatJSONObject:
		return map[string]any{"type": "text", "mime_type": "application/json"}
	case FormatJSONSchema:
		entry := map[string]any{
			"type": "text", "mime_type": "application/json",
			"schema": rawJSONOrEmpty(format.Schema),
		}
		return entry
	}
	appendDiagnostic(diagnostics, "response_format_dropped",
		fmt.Sprintf("response format kind %q is unknown", format.Kind), "response_format")
	return nil
}

// ---------------------------------------------------------------------------
// Responses

// decodeInteractionsResponse reads an Interaction into canonical form.
func decodeInteractionsResponse(body []byte, diagnostics *[]Diagnostic) (*Response, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	response := &Response{
		ID:     stringAt(object, "id"),
		Model:  stringAt(object, "model"),
		Status: stringAt(object, "status"),
	}
	if response.Status == "" {
		response.Status = "completed"
	}
	if created := stringAt(object, "created"); created != "" {
		if moment, err := time.Parse(time.RFC3339, created); err == nil {
			response.Created = moment.Unix()
		}
	}
	if errors := arrayAt(object, "errors"); len(errors) > 0 {
		if entry, ok := errors[0].(map[string]any); ok {
			streamError := &StreamError{Message: stringAt(entry, "message")}
			if code := formatErrorCode(entry["code"]); code != "" {
				streamError.Code = code
			}
			response.Error = streamError
		}
	}
	hasToolCall := false
	for index, raw := range arrayAt(object, "steps") {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		stepPath := fmt.Sprintf("steps[%d]", index)
		switch stringAt(step, "type") {
		case "model_output":
			response.Parts = append(response.Parts, decodeInteractionsContentList(arrayAt(step, "content"), diagnostics, stepPath+".content")...)
		case "thought":
			for _, rawSummary := range arrayAt(step, "summary") {
				entry, ok := rawSummary.(map[string]any)
				if !ok {
					continue
				}
				if text := stringAt(entry, "text"); text != "" {
					response.Parts = append(response.Parts, Part{
						Kind: PartReasoning, Text: text, ThoughtSignature: stringAt(step, "signature"),
					})
				}
			}
		case "function_call":
			hasToolCall = true
			arguments := rawObject(step["arguments"])
			response.Parts = append(response.Parts, Part{
				Kind: PartToolCall, ToolCallID: stringAt(step, "id"), ToolName: stringAt(step, "name"),
				Arguments: arguments, ArgumentsText: string(arguments),
				ThoughtSignature: stringAt(step, "signature"),
			})
		case "function_result":
			// A result the provider executed itself (server-side tool).
			content := decodeInteractionsFunctionResult(step["result"], diagnostics, stepPath+".result")
			response.Parts = append(response.Parts, toolResultPart(
				stringAt(step, "call_id"), stringAt(step, "name"), content, boolValue(step, "is_error")))
		case "google_search_call", "file_search_call", "code_execution_call", "url_context_call",
			"mcp_server_tool_call", "retrieval_call", "google_maps_call", "processing_call",
			"google_search_result", "file_search_result", "code_execution_result", "url_context_result",
			"mcp_server_tool_result", "retrieval_result", "google_maps_result", "processing_result":
			appendDiagnostic(diagnostics, "hosted_step_dropped",
				fmt.Sprintf("provider-hosted step %q has no cross-dialect representation", stringAt(step, "type")), stepPath)
		default:
			appendDiagnostic(diagnostics, "unknown_step_dropped",
				fmt.Sprintf("unknown output step %q was dropped", stringAt(step, "type")), stepPath)
		}
	}
	if usage := objectAt(object, "usage"); usage != nil {
		response.Usage = usageFromInteractions(usage)
	}
	if response.Error != nil {
		response.Status = "failed"
		response.FinishReason = FinishOther
		return response, nil
	}
	switch response.Status {
	case "completed", "requires_action":
		// requires_action means the model is waiting for the client to run the
		// tool call it just emitted; reporting "stop" made clients answer
		// without ever executing it.
		if hasToolCall {
			response.FinishReason = FinishToolUse
		} else if response.Status == "requires_action" {
			response.FinishReason = FinishOther
			response.Status = "failed"
			response.Error = &StreamError{
				Code:    "requires_action_without_call",
				Message: "the interaction requires an action but carries no function_call step",
			}
		} else {
			response.FinishReason = FinishStop
		}
	case "incomplete":
		response.FinishReason = FinishLength
	case "budget_exceeded":
		// The provider stopped because a budget ran out; that is a terminal
		// failure, not a length-truncated answer.
		response.FinishReason = FinishOther
		response.Status = "failed"
		response.Error = &StreamError{
			Code: "budget_exceeded", Message: "the interaction exceeded its budget",
		}
	case "cancelled":
		response.FinishReason = FinishOther
		response.Status = "failed"
		response.Error = &StreamError{Code: "cancelled", Message: "the interaction was cancelled"}
	case "failed":
		response.FinishReason = FinishOther
	case "queued", "in_progress", "running":
		// A response that has not finished cannot be presented as a completed
		// answer.
		pending := response.Status
		response.FinishReason = FinishOther
		response.Status = "failed"
		response.Error = &StreamError{
			Code:    "interaction_pending",
			Message: fmt.Sprintf("the interaction is still %s", pending),
		}
	default:
		response.FinishReason = FinishStop
	}
	return response, nil
}

func usageFromInteractions(object map[string]any) Usage {
	usage := Usage{
		InputTokens:     intValue(object, "total_input_tokens"),
		OutputTokens:    intValue(object, "total_output_tokens"),
		TotalTokens:     intValue(object, "total_tokens"),
		CachedTokens:    intValue(object, "total_cached_tokens"),
		ReasoningTokens: intValue(object, "total_thought_tokens"),
		ToolUseTokens:   intValue(object, "total_tool_use_tokens"),
	}
	usage.Normalize()
	return usage
}

// formatErrorCode renders an error code without the "<nil>" a bare %v of a
// missing field would produce.
func formatErrorCode(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return fmt.Sprintf("%d", int64(typed))
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func usageToInteractions(usage Usage) map[string]any {
	out := map[string]any{
		"total_input_tokens":  usage.InputTokens,
		"total_output_tokens": usage.OutputTokens,
		"total_tokens":        usage.TotalTokens,
	}
	if usage.CachedTokens > 0 {
		out["total_cached_tokens"] = usage.CachedTokens
	}
	if usage.ReasoningTokens > 0 {
		out["total_thought_tokens"] = usage.ReasoningTokens
	}
	if usage.ToolUseTokens > 0 {
		out["total_tool_use_tokens"] = usage.ToolUseTokens
	}
	return out
}

// encodeInteractionsResponse renders the canonical response as an Interaction.
func encodeInteractionsResponse(response *Response, diagnostics *[]Diagnostic) map[string]any {
	id := response.ID
	if id == "" {
		id = fmt.Sprintf("interaction_%d", response.Created)
	}
	created := time.Unix(response.Created, 0).UTC().Format(time.RFC3339)
	status := "completed"
	switch response.FinishReason {
	case FinishLength, FinishContentFilter:
		status = "incomplete"
	case FinishOther:
		if response.Error != nil {
			status = "failed"
		}
	}
	out := map[string]any{
		"id": id, "object": "interaction", "status": status,
		"created": created, "updated": created,
		"model": response.Model,
		"steps": encodeInteractionsOutputSteps(response.Parts, diagnostics),
	}
	if response.Error != nil {
		entry := map[string]any{"message": responseErrorMessage(response)}
		if code := errorCode(response.Error); code != "" {
			entry["code"] = code
		}
		out["errors"] = []any{entry}
	}
	if response.Usage != (Usage{}) {
		out["usage"] = usageToInteractions(response.Usage)
	}
	return out
}

// encodeInteractionsOutputSteps renders assistant parts as output Steps,
// preserving thought signatures (Interactions rejects a replay that lost one).
func encodeInteractionsOutputSteps(parts []Part, diagnostics *[]Diagnostic) []any {
	steps := make([]any, 0, len(parts))
	var content []any
	flushContent := func() {
		if len(content) == 0 {
			return
		}
		steps = append(steps, map[string]any{"type": "model_output", "content": content})
		content = nil
	}
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			}
		case PartRefusal:
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case PartReasoning:
			flushContent()
			step := map[string]any{"type": "thought"}
			if part.Text != "" {
				step["summary"] = []any{map[string]any{"type": "text", "text": part.Text}}
			}
			if part.ThoughtSignature != "" {
				step["signature"] = part.ThoughtSignature
			}
			steps = append(steps, step)
		case PartToolCall:
			flushContent()
			arguments := part.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			var decoded any
			if json.Unmarshal(arguments, &decoded) != nil {
				decoded = map[string]any{}
			}
			step := map[string]any{"type": "function_call", "name": part.ToolName, "arguments": decoded}
			if part.ToolCallID != "" {
				step["id"] = part.ToolCallID
			}
			steps = append(steps, step)
		case PartToolResult:
			// A tool the gateway executed on the model's behalf.
			flushContent()
			step := map[string]any{
				"type": "function_result", "call_id": part.ToolCallID,
				"result": interactionsFunctionResult(part.Content),
			}
			if part.ToolName != "" {
				step["name"] = part.ToolName
			}
			if part.IsError {
				step["is_error"] = true
			}
			steps = append(steps, step)
		case PartImage, PartDocument, PartAudio, PartVideo:
			if media := encodeInteractionsMedia(part); media != nil {
				content = append(content, media)
			}
		}
	}
	flushContent()
	return steps
}
