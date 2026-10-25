package codec

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// DecodeResponse reads a wire response into the canonical representation.
func DecodeResponse(format Format, body []byte) (*Response, []Diagnostic, error) {
	var diagnostics []Diagnostic
	var response *Response
	var err error
	switch format {
	case OpenAIChat:
		response, err = decodeChatResponse(body, &diagnostics)
	case OpenAIResponses:
		response, err = decodeResponsesResponse(body, &diagnostics)
	case ClaudeMessages:
		response, err = decodeClaudeResponse(body, &diagnostics)
	case Gemini:
		response, err = decodeGeminiResponse(body, &diagnostics)
	case GeminiInteractions:
		response, err = decodeInteractionsResponse(body, &diagnostics)
	default:
		return nil, nil, conversionErrorf("unsupported_format", "unsupported response dialect %q", format)
	}
	if err != nil {
		return nil, diagnostics, err
	}
	return response, diagnostics, nil
}

// EncodeResponse renders the canonical response in the target dialect.
func EncodeResponse(response *Response, target Format) ([]byte, []Diagnostic, error) {
	return EncodeResponseWithRequest(response, target, nil)
}

// EncodeResponseWithRequest is EncodeResponse with the canonical request the
// response answers, which supplies the tool state a Responses target needs to
// restore namespaces and echo the request's tool policy.
func EncodeResponseWithRequest(response *Response, target Format, request *Request) ([]byte, []Diagnostic, error) {
	if response == nil {
		return nil, nil, conversionErrorf("invalid_response", "response is nil")
	}
	var diagnostics []Diagnostic
	diagnosePartSignatures(response.Parts, target, &diagnostics)
	var out map[string]any
	switch target {
	case OpenAIChat:
		out = encodeChatResponse(response, &diagnostics)
	case OpenAIResponses:
		out = encodeResponsesResponse(response, request, &diagnostics)
	case ClaudeMessages:
		out = encodeClaudeResponse(response, &diagnostics)
	case Gemini:
		out = encodeGeminiResponse(response, &diagnostics)
	case GeminiInteractions:
		out = encodeInteractionsResponse(response, &diagnostics)
	default:
		return nil, diagnostics, conversionErrorf("unsupported_format", "unsupported response dialect %q", target)
	}
	encoded, err := marshalJSON(out)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

// ---------------------------------------------------------------------------
// Finish-reason mapping

func finishFromChat(reason string) string {
	switch reason {
	case "stop":
		return FinishStop
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishToolUse
	case "content_filter":
		return FinishContentFilter
	}
	return FinishOther
}

func finishToChat(reason string) string {
	switch reason {
	case FinishLength:
		return "length"
	case FinishToolUse:
		return "tool_calls"
	case FinishContentFilter:
		return "content_filter"
	}
	return "stop"
}

func finishFromClaude(reason string) string {
	switch reason {
	case "end_turn":
		return FinishStop
	case "max_tokens":
		return FinishLength
	case "tool_use":
		return FinishToolUse
	case "stop_sequence":
		return FinishStopSequence
	case "refusal":
		return FinishContentFilter
	case "pause_turn":
		return FinishPause
	}
	return FinishOther
}

func finishToClaude(reason string) string {
	switch reason {
	case FinishLength:
		return "max_tokens"
	case FinishToolUse:
		return "tool_use"
	case FinishStopSequence:
		return "stop_sequence"
	case FinishContentFilter:
		return "refusal"
	case FinishPause:
		return "pause_turn"
	}
	return "end_turn"
}

func finishFromGemini(reason string, hasToolCall bool) string {
	switch reason {
	case "STOP":
		if hasToolCall {
			return FinishToolUse
		}
		return FinishStop
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return FinishContentFilter
	}
	return FinishOther
}

func finishToGemini(reason string) string {
	switch reason {
	case FinishLength:
		return "MAX_TOKENS"
	case FinishContentFilter:
		return "SAFETY"
	}
	return "STOP"
}

func finishFromResponsesStatus(status, incompleteReason string, hasToolCall bool) string {
	switch status {
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return FinishLength
		case "content_filter":
			return FinishContentFilter
		}
		return FinishOther
	case "failed", "cancelled":
		return FinishOther
	}
	if hasToolCall {
		return FinishToolUse
	}
	return FinishStop
}

// ---------------------------------------------------------------------------
// Usage

func usageFromChat(object map[string]any) Usage {
	usage := Usage{
		InputTokens:  intValue(object, "prompt_tokens"),
		OutputTokens: intValue(object, "completion_tokens"),
		TotalTokens:  intValue(object, "total_tokens"),
	}
	if details := objectAt(object, "prompt_tokens_details"); details != nil {
		usage.CachedTokens = intValue(details, "cached_tokens")
	}
	if details := objectAt(object, "completion_tokens_details"); details != nil {
		usage.ReasoningTokens = intValue(details, "reasoning_tokens")
	}
	usage.Normalize()
	return usage
}

func usageToChat(usage Usage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      usage.TotalTokens,
	}
	if usage.CachedTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CachedTokens}
	}
	if usage.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
	}
	return out
}

func usageFromClaude(object map[string]any) Usage {
	cacheRead := intValue(object, "cache_read_input_tokens")
	cacheCreation := intValue(object, "cache_creation_input_tokens")
	input := intValue(object, "input_tokens")
	usage := Usage{
		InputTokens:         input + cacheRead + cacheCreation,
		OutputTokens:        intValue(object, "output_tokens"),
		CachedTokens:        cacheRead,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
	}
	usage.Normalize()
	return usage
}

func usageToClaude(usage Usage) map[string]any {
	cacheRead := usage.CacheReadTokens
	if cacheRead == 0 {
		cacheRead = usage.CachedTokens
	}
	nonCached := usage.InputTokens - cacheRead - usage.CacheCreationTokens
	if nonCached < 0 {
		nonCached = 0
	}
	out := map[string]any{
		"input_tokens":  nonCached,
		"output_tokens": usage.OutputTokens,
	}
	if cacheRead > 0 {
		out["cache_read_input_tokens"] = cacheRead
	}
	if usage.CacheCreationTokens > 0 {
		out["cache_creation_input_tokens"] = usage.CacheCreationTokens
	}
	return out
}

// Gemini counts thinking tokens separately from visible candidate tokens,
// while every other dialect includes reasoning inside its output count. The
// canonical Usage follows the majority convention (OutputTokens includes
// ReasoningTokens) and the Gemini boundary converts.
func usageFromGemini(object map[string]any) Usage {
	candidates := intValue(object, "candidatesTokenCount")
	thoughts := intValue(object, "thoughtsTokenCount")
	usage := Usage{
		InputTokens:     intValue(object, "promptTokenCount"),
		OutputTokens:    candidates + thoughts,
		TotalTokens:     intValue(object, "totalTokenCount"),
		CachedTokens:    intValue(object, "cachedContentTokenCount"),
		ReasoningTokens: thoughts,
	}
	usage.Normalize()
	return usage
}

func usageToGemini(usage Usage) map[string]any {
	// Gemini reports the visible answer and the thinking separately, so the
	// reasoning portion is subtracted from the canonical output count.
	candidates := usage.OutputTokens - usage.ReasoningTokens
	if candidates < 0 {
		candidates = 0
	}
	out := map[string]any{
		"promptTokenCount":     usage.InputTokens,
		"candidatesTokenCount": candidates,
		"totalTokenCount":      usage.TotalTokens,
	}
	if usage.CachedTokens > 0 {
		out["cachedContentTokenCount"] = usage.CachedTokens
	}
	if usage.ReasoningTokens > 0 {
		out["thoughtsTokenCount"] = usage.ReasoningTokens
	}
	return out
}

func usageFromResponses(object map[string]any) Usage {
	usage := Usage{
		InputTokens:  intValue(object, "input_tokens"),
		OutputTokens: intValue(object, "output_tokens"),
		TotalTokens:  intValue(object, "total_tokens"),
	}
	if details := objectAt(object, "input_tokens_details"); details != nil {
		usage.CachedTokens = intValue(details, "cached_tokens")
	}
	if details := objectAt(object, "output_tokens_details"); details != nil {
		usage.ReasoningTokens = intValue(details, "reasoning_tokens")
	}
	usage.Normalize()
	return usage
}

func usageToResponses(usage Usage) map[string]any {
	return map[string]any{
		"input_tokens":          usage.InputTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CachedTokens},
		"output_tokens":         usage.OutputTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
		"total_tokens":          usage.TotalTokens,
	}
}

// chatErrorResponse reads an OpenAI-compatible error envelope. Compatible
// upstreams answer HTTP 200 with {"error": {...}}; treating that as an empty
// successful answer hides the failure from the client entirely.
func chatErrorResponse(object map[string]any) *Response {
	errorObject := objectAt(object, "error")
	if errorObject == nil {
		return nil
	}
	streamError := &StreamError{
		Type:    stringAt(errorObject, "type"),
		Code:    stringAt(errorObject, "code"),
		Message: stringAt(errorObject, "message"),
		Status:  stringAt(errorObject, "status"),
	}
	if streamError.Message == "" && streamError.Type == "" && streamError.Code == "" {
		// An empty error object is not a failure report.
		return nil
	}
	return &Response{Status: "failed", Error: streamError, FinishReason: FinishOther}
}

func intValue(object map[string]any, key string) int {
	if value, ok := intAt(object, key); ok {
		return value
	}
	return 0
}

// ---------------------------------------------------------------------------
// OpenAI Chat response

func decodeChatResponse(body []byte, diagnostics *[]Diagnostic) (*Response, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	if response := chatErrorResponse(object); response != nil {
		return response, nil
	}
	response := &Response{
		ID:      stringAt(object, "id"),
		Model:   stringAt(object, "model"),
		Created: intValue64(object, "created"),
	}
	choices := arrayAt(object, "choices")
	if len(choices) > 1 {
		appendDiagnostic(diagnostics, "extra_choices_dropped",
			"the canonical response carries one choice; additional choices were dropped", "choices")
	}
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message := objectAt(choice, "message")
			parts := decodeChatContent(message["content"], diagnostics, "choices[0].message.content")
			for index, rawCall := range arrayAt(message, "tool_calls") {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				function := objectAt(call, "function")
				parts = append(parts, Part{
					Kind: PartToolCall, ToolCallID: stringAt(call, "id"), ToolName: stringAt(function, "name"),
					ArgumentsText: stringAt(function, "arguments"),
					Arguments: parseArguments(stringAt(function, "arguments"), diagnostics,
						fmt.Sprintf("choices[0].message.tool_calls[%d].function.arguments", index)),
				})
			}
			if reasoning := stringAt(message, "reasoning_content"); reasoning != "" {
				parts = append([]Part{{Kind: PartReasoning, Text: reasoning}}, parts...)
			}
			if refusal := stringAt(message, "refusal"); refusal != "" {
				parts = append(parts, Part{Kind: PartRefusal, Text: refusal})
			}
			response.Parts = parts
			response.FinishReason = finishFromChat(stringAt(choice, "finish_reason"))
		}
	}
	if usage := objectAt(object, "usage"); usage != nil {
		response.Usage = usageFromChat(usage)
	}
	response.Status = "completed"
	return response, nil
}

func encodeChatResponse(response *Response, diagnostics *[]Diagnostic) map[string]any {
	if responseIsFailed(response) {
		envelope := map[string]any{
			"type": "server_error", "message": responseErrorMessage(response),
		}
		if code := errorCode(response.Error); code != "" {
			envelope["code"] = code
		}
		return map[string]any{"error": envelope}
	}
	id := response.ID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", response.Created)
	}
	message := map[string]any{"role": "assistant"}
	var texts []string
	var reasoning []string
	var toolCalls []any
	var refusal string
	for _, part := range response.Parts {
		switch part.Kind {
		case PartText:
			texts = append(texts, part.Text)
		case PartRefusal:
			refusal = part.Text
		case PartReasoning:
			reasoning = append(reasoning, part.Text)
		case PartToolCall:
			toolCalls = append(toolCalls, map[string]any{
				"id": part.ToolCallID, "type": "function",
				"function": map[string]any{"name": part.ToolName, "arguments": argumentsText(part.Arguments)},
			})
		case PartImage, PartDocument, PartAudio, PartVideo:
			appendDiagnostic(diagnostics, "assistant_media_dropped",
				"assistant media has no Chat Completions representation", "choices")
		}
	}
	content := strings.Join(texts, "")
	if content != "" || (len(toolCalls) == 0 && refusal == "") {
		message["content"] = content
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if len(reasoning) > 0 {
		message["reasoning_content"] = strings.Join(reasoning, "\n")
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": response.Created,
		"model":   response.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "logprobs": nil,
			"finish_reason": finishToChat(response.FinishReason),
		}},
	}
	if response.Usage != (Usage{}) {
		out["usage"] = usageToChat(response.Usage)
	}
	return out
}

// ---------------------------------------------------------------------------
// Anthropic response

func decodeClaudeResponse(body []byte, diagnostics *[]Diagnostic) (*Response, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	response := &Response{
		ID:           stringAt(object, "id"),
		Model:        stringAt(object, "model"),
		StopSequence: stringAt(object, "stop_sequence"),
		Parts:        decodeClaudeContent(object["content"], diagnostics, "content"),
		Status:       "completed",
	}
	response.FinishReason = finishFromClaude(stringAt(object, "stop_reason"))
	if usage := objectAt(object, "usage"); usage != nil {
		response.Usage = usageFromClaude(usage)
	}
	return response, nil
}

func encodeClaudeResponse(response *Response, diagnostics *[]Diagnostic) map[string]any {
	if responseIsFailed(response) {
		return map[string]any{"type": "error", "error": map[string]any{
			"type": "api_error", "message": responseErrorMessage(response),
		}}
	}
	id := response.ID
	if id == "" {
		id = fmt.Sprintf("msg_%d", response.Created)
	}
	blocks := encodeClaudeAssistantBlocks(response.Parts, diagnostics)
	if blocks == nil {
		blocks = []any{}
	}
	out := map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": response.Model,
		"content": blocks, "stop_reason": finishToClaude(response.FinishReason),
		"stop_sequence": nilIfEmpty(response.StopSequence),
		"usage":         usageToClaude(response.Usage),
	}
	return out
}

// ---------------------------------------------------------------------------
// Gemini response

func decodeGeminiResponse(body []byte, diagnostics *[]Diagnostic) (*Response, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	response := &Response{
		Model:  stringAt(object, "modelVersion"),
		Status: "completed",
	}
	candidates := arrayAt(object, "candidates")
	if len(candidates) > 1 {
		appendDiagnostic(diagnostics, "extra_candidates_dropped",
			"the canonical response carries one candidate; additional candidates were dropped", "candidates")
	}
	hasToolCall := false
	if len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			content := objectAt(candidate, "content")
			response.Parts = decodeGeminiParts(arrayAt(content, "parts"), newGeminiCallTracker(), diagnostics, "candidates[0].content.parts")
			for _, part := range response.Parts {
				if part.Kind == PartToolCall {
					hasToolCall = true
				}
			}
			response.FinishReason = finishFromGemini(stringAt(candidate, "finishReason"), hasToolCall)
		}
	}
	if feedback := objectAt(object, "promptFeedback"); feedback != nil {
		if blocked := stringAt(feedback, "blockReason"); blocked != "" {
			response.FinishReason = FinishContentFilter
			appendDiagnostic(diagnostics, "prompt_blocked",
				fmt.Sprintf("Gemini blocked the prompt (%s)", blocked), "promptFeedback.blockReason")
		}
	}
	if usage := objectAt(object, "usageMetadata"); usage != nil {
		response.Usage = usageFromGemini(usage)
	}
	return response, nil
}

func encodeGeminiResponse(response *Response, diagnostics *[]Diagnostic) map[string]any {
	if responseIsFailed(response) {
		return map[string]any{"error": map[string]any{
			"code": 500, "status": "INTERNAL", "message": responseErrorMessage(response),
		}}
	}
	parts := encodeGeminiOutputParts(response.Parts, diagnostics)
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":       map[string]any{"role": "model", "parts": parts},
			"finishReason":  finishToGemini(response.FinishReason),
			"index":         0,
			"safetyRatings": []any{},
		}},
	}
	if response.Model != "" {
		out["modelVersion"] = response.Model
	}
	if response.Usage != (Usage{}) {
		out["usageMetadata"] = usageToGemini(response.Usage)
	}
	return out
}

func encodeGeminiOutputParts(parts []Part, diagnostics *[]Diagnostic) []any {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" || part.ThoughtSignature != "" {
				entry := map[string]any{"text": part.Text}
				if part.ThoughtSignature != "" {
					entry["thoughtSignature"] = part.ThoughtSignature
				}
				out = append(out, entry)
			}
		case PartReasoning:
			if part.Text != "" || part.ThoughtSignature != "" {
				entry := map[string]any{"text": part.Text, "thought": true}
				// Gemini 3 requires its opaque signature to come back verbatim
				// on the next turn; dropping it on the response path made the
				// follow-up request invalid.
				if part.ThoughtSignature != "" {
					entry["thoughtSignature"] = part.ThoughtSignature
				}
				out = append(out, entry)
			}
		case PartToolCall:
			args := part.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			var decoded any
			if json.Unmarshal(args, &decoded) != nil {
				decoded = map[string]any{}
			}
			entry := map[string]any{
				"functionCall": map[string]any{"name": part.ToolName, "args": decoded},
			}
			if part.ToolCallID != "" {
				entry["functionCall"].(map[string]any)["id"] = part.ToolCallID
			}
			if part.ThoughtSignature != "" {
				entry["thoughtSignature"] = part.ThoughtSignature
			}
			out = append(out, entry)
		case PartRefusal:
			if part.Text != "" {
				out = append(out, map[string]any{"text": part.Text})
			}
		case PartImage, PartDocument, PartAudio, PartVideo:
			appendDiagnostic(diagnostics, "assistant_media_dropped",
				"assistant media has no Gemini candidate representation", "candidates")
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenAI Responses

func decodeResponsesResponse(body []byte, diagnostics *[]Diagnostic) (*Response, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	response := &Response{
		ID:       stringAt(object, "id"),
		Model:    stringAt(object, "model"),
		Created:  intValue64(object, "created_at"),
		Metadata: stringMap(object, "metadata"),
		Status:   stringAt(object, "status"),
	}
	if response.Status == "" {
		response.Status = "completed"
	}
	if errorObject := objectAt(object, "error"); errorObject != nil {
		response.Error = &StreamError{
			Type:    stringAt(errorObject, "type"),
			Code:    stringAt(errorObject, "code"),
			Message: stringAt(errorObject, "message"),
		}
	}
	hasToolCall := false
	for index, raw := range arrayAt(object, "output") {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemPath := fmt.Sprintf("output[%d]", index)
		switch stringAt(item, "type") {
		case "message":
			response.Parts = append(response.Parts, decodeResponsesContent(item["content"], diagnostics, itemPath+".content")...)
		case "function_call":
			hasToolCall = true
			response.Parts = append(response.Parts, Part{
				Kind: PartToolCall, ToolCallID: stringAt(item, "call_id"), ToolName: stringAt(item, "name"),
				ToolNamespace: stringAt(item, "namespace"),
				ArgumentsText: stringAt(item, "arguments"),
				Arguments:     parseArguments(stringAt(item, "arguments"), diagnostics, itemPath+".arguments"),
			})
		case "custom_tool_call":
			hasToolCall = true
			input, _ := item["input"].(string)
			arguments := marshalCustomToolInput(input)
			response.Parts = append(response.Parts, Part{
				Kind: PartToolCall, ToolCallID: stringAt(item, "call_id"), ToolName: stringAt(item, "name"),
				ToolNamespace: stringAt(item, "namespace"), ToolKind: ToolKindCustom,
				Arguments: arguments, ArgumentsText: string(arguments),
			})
		case "reasoning":
			response.Parts = append(response.Parts, decodeResponsesReasoning(item, diagnostics, itemPath)...)
		case "refusal":
			if text := stringAt(item, "refusal"); text != "" {
				response.Parts = append(response.Parts, Part{Kind: PartRefusal, Text: text})
			}
		default:
			appendDiagnostic(diagnostics, "output_item_dropped",
				fmt.Sprintf("output item type %q cannot be represented", stringAt(item, "type")), itemPath)
		}
	}
	incomplete := objectAt(object, "incomplete_details")
	incompleteReason := ""
	if incomplete != nil {
		incompleteReason = stringAt(incomplete, "reason")
	}
	response.FinishReason = finishFromResponsesStatus(response.Status, incompleteReason, hasToolCall)
	if usage := objectAt(object, "usage"); usage != nil {
		response.Usage = usageFromResponses(usage)
	}
	return response, nil
}

func encodeResponsesResponse(response *Response, request *Request, diagnostics *[]Diagnostic) map[string]any {
	id := normalizeResponsesID(response.ID)
	if id == "" {
		id = newResponseID(response.Created)
	}
	status := "completed"
	var incomplete any
	switch response.FinishReason {
	case FinishLength:
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case FinishContentFilter:
		status = "incomplete"
		incomplete = map[string]any{"reason": "content_filter"}
	}
	if responseIsFailed(response) {
		status = "failed"
	}
	toolState := response.ToolState
	if toolState == nil && request != nil {
		toolState = request.ToolState
	}
	output := encodeResponsesOutputItems(response.Parts, id, toolState, diagnostics)
	out := map[string]any{
		"id": id, "object": "response", "created_at": response.Created,
		"status": status, "incomplete_details": incomplete,
		"model": response.Model, "output": output,
		"text":       map[string]any{"format": map[string]any{"type": "text"}},
		"truncation": "disabled",
	}
	// Echo the request's tool policy rather than inventing one: a client that
	// reads these fields back would otherwise see a policy it never sent.
	out["parallel_tool_calls"] = true
	out["tool_choice"] = "auto"
	out["tools"] = []any{}
	if request != nil {
		if request.ParallelTools != nil {
			out["parallel_tool_calls"] = *request.ParallelTools
		}
		if choice := encodeResponsesToolChoice(request.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
		if len(request.Tools) > 0 {
			out["tools"] = encodeResponsesTools(request.Tools)
		}
	}
	if responseIsFailed(response) {
		errorObject := map[string]any{"code": "", "message": "", "type": ""}
		if response.Error != nil {
			errorObject = map[string]any{
				"code": response.Error.Code, "message": response.Error.Message, "type": response.Error.Type,
			}
		}
		out["error"] = errorObject
	} else {
		out["error"] = nil
	}
	if response.Usage != (Usage{}) {
		out["usage"] = usageToResponses(response.Usage)
	}
	if response.Metadata != nil {
		out["metadata"] = response.Metadata
	}
	return out
}

// synthesizedCallID mints a stable call id for a tool call that arrived
// without one. callIDs lets the id be reused when the same function was
// rendered before (stream and whole-response paths share the rule).
func synthesizedCallID(responseID string, index int, callIDs map[string]struct{}) string {
	candidate := fmt.Sprintf("%s_call_%d", responseID, index)
	for suffix := 1; ; suffix++ {
		if _, taken := callIDs[candidate]; !taken {
			if callIDs != nil {
				callIDs[candidate] = struct{}{}
			}
			return candidate
		}
		candidate = fmt.Sprintf("%s_call_%d_%d", responseID, index, suffix)
	}
}

// marshalCustomToolInput wraps a freeform tool input in the single "input"
// argument a function-calling target receives, so the canonical part is a
// valid JSON argument object.
func marshalCustomToolInput(input string) json.RawMessage {
	encoded, err := marshalJSON(map[string]any{"input": input})
	if err != nil {
		return json.RawMessage(`{"input":""}`)
	}
	return encoded
}

// customToolInputFromArguments unwraps a custom tool input from the arguments
// a Chat function call carries. Arguments of any other shape are returned
// unchanged so the model's output is never dropped.
func customToolInputFromArguments(arguments string) string {
	var value map[string]any
	if err := json.Unmarshal([]byte(arguments), &value); err != nil {
		return arguments
	}
	input, ok := value["input"]
	if !ok {
		return arguments
	}
	if text, ok := input.(string); ok {
		return text
	}
	encoded, err := marshalJSON(input)
	if err != nil {
		return arguments
	}
	return string(encoded)
}

func encodeResponsesOutputItems(parts []Part, responseID string, state *ToolState, diagnostics *[]Diagnostic) []any {
	var items []any
	var content []any
	flush := func() {
		if len(content) == 0 {
			return
		}
		items = append(items, map[string]any{
			"id": fmt.Sprintf("msg_%d", len(items)), "type": "message", "status": "completed",
			"role": "assistant", "content": content,
		})
		content = nil
	}
	for _, part := range parts {
		switch part.Kind {
		case PartText:
			if part.Text != "" {
				content = append(content, map[string]any{
					"type": "output_text", "text": part.Text, "annotations": []any{},
				})
			}
		case PartRefusal:
			content = append(content, map[string]any{"type": "refusal", "refusal": part.Text})
		case PartReasoning:
			flush()
			summary := []any{}
			if part.Text != "" {
				summary = append(summary, map[string]any{"type": "summary_text", "text": part.Text})
			}
			items = append(items, map[string]any{
				"id": fmt.Sprintf("rs_%d", len(items)), "type": "reasoning", "status": "completed",
				"summary": summary,
			})
		case PartToolCall:
			flush()
			itemID := fmt.Sprintf("fc_%d", len(items))
			namespace, name, kind := part.ToolNamespace, part.ToolName, part.ToolKind
			if binding, ok := state.Binding(part.ToolName); ok {
				// The binding is authoritative: it is what the request
				// declared, and the call name is the flattened upstream name.
				namespace, name, kind = binding.Namespace, strings.TrimPrefix(part.ToolName, binding.Namespace+"__"), binding.Kind
				if binding.Namespace == "" || binding.Namespace == defaultToolNamespace {
					name = part.ToolName
				}
			}
			callID := part.ToolCallID
			if callID == "" {
				callID = fmt.Sprintf("%s_call_%d", responseID, len(items))
				appendDiagnostic(diagnostics, "tool_call_id_synthesized",
					"the upstream tool call carried no id; a stable response-scoped id was synthesized", "output")
			}
			entry := map[string]any{
				"id": itemID, "status": "completed", "call_id": callID, "name": name,
			}
			if namespace != "" {
				entry["namespace"] = namespace
			}
			if kind == ToolKindCustom {
				entry["type"] = "custom_tool_call"
				entry["input"] = customToolInputFromArguments(argumentsText(part.Arguments))
			} else {
				entry["type"] = "function_call"
				entry["arguments"] = argumentsText(part.Arguments)
			}
			items = append(items, entry)
		case PartImage, PartDocument, PartAudio, PartVideo:
			appendDiagnostic(diagnostics, "assistant_media_dropped",
				"assistant media has no Responses output representation", "output")
		}
	}
	flush()
	if items == nil {
		items = []any{}
	}
	return items
}

// normalizeResponsesID gives a Responses object an id with the documented
// "resp_" prefix. A source dialect's id (chatcmpl-1, msg_1, ...) is not a
// Responses id, and clients key their follow-up requests on the prefix.
func normalizeResponsesID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

// newResponseID mints a Responses id when the upstream supplied none. The
// random suffix matters: a plain per-second id collides for two responses
// finishing in the same second, and the store keys stored responses by id.
func newResponseID(created int64) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// A timestamp-only id is still better than no id at all.
		return fmt.Sprintf("resp_%d", created)
	}
	return fmt.Sprintf("resp_%d_%s", created, hex.EncodeToString(suffix[:]))
}

func responseErrorMessage(response *Response) string {
	if response.Error != nil && response.Error.Message != "" {
		return response.Error.Message
	}
	return "upstream response failed"
}

func intValue64(object map[string]any, key string) int64 {
	if value, ok := int64At(object, key); ok {
		return value
	}
	return 0
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
