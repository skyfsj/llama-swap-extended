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
	if response == nil {
		return nil, nil, conversionErrorf("invalid_response", "response is nil")
	}
	var diagnostics []Diagnostic
	var out map[string]any
	switch target {
	case OpenAIChat:
		out = encodeChatResponse(response, &diagnostics)
	case OpenAIResponses:
		out = encodeResponsesResponse(response, &diagnostics)
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

func usageFromGemini(object map[string]any) Usage {
	usage := Usage{
		InputTokens:     intValue(object, "promptTokenCount"),
		OutputTokens:    intValue(object, "candidatesTokenCount"),
		TotalTokens:     intValue(object, "totalTokenCount"),
		CachedTokens:    intValue(object, "cachedContentTokenCount"),
		ReasoningTokens: intValue(object, "thoughtsTokenCount"),
	}
	usage.Normalize()
	return usage
}

func usageToGemini(usage Usage) map[string]any {
	out := map[string]any{
		"promptTokenCount":     usage.InputTokens,
		"candidatesTokenCount": usage.OutputTokens,
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
			if part.Text != "" {
				out = append(out, map[string]any{"text": part.Text})
			}
		case PartReasoning:
			if part.Text != "" {
				out = append(out, map[string]any{"text": part.Text, "thought": true})
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
			out = append(out, map[string]any{
				"functionCall": map[string]any{"name": part.ToolName, "args": decoded},
			})
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
				ArgumentsText: stringAt(item, "arguments"),
				Arguments:     parseArguments(stringAt(item, "arguments"), diagnostics, itemPath+".arguments"),
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

func encodeResponsesResponse(response *Response, diagnostics *[]Diagnostic) map[string]any {
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
	output := encodeResponsesOutputItems(response.Parts, diagnostics)
	out := map[string]any{
		"id": id, "object": "response", "created_at": response.Created,
		"status": status, "incomplete_details": incomplete,
		"model": response.Model, "output": output,
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"text":       map[string]any{"format": map[string]any{"type": "text"}},
		"truncation": "disabled",
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

func encodeResponsesOutputItems(parts []Part, diagnostics *[]Diagnostic) []any {
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
			items = append(items, map[string]any{
				"id": fmt.Sprintf("fc_%d", len(items)), "type": "function_call", "status": "completed",
				"call_id": part.ToolCallID, "name": part.ToolName, "arguments": argumentsText(part.Arguments),
			})
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
