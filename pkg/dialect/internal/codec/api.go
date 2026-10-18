package codec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConvertRequest decodes a request in source and renders it in target.
func ConvertRequest(source, target Format, body []byte, options ConvertOptions) ([]byte, []Diagnostic, error) {
	request, diagnostics, err := DecodeRequest(source, body)
	if err != nil {
		return nil, diagnostics, err
	}
	if request.Model == "" {
		request.Model = options.Model
	}
	encoded, encodeDiagnostics, err := EncodeRequest(request, target, options)
	diagnostics = append(diagnostics, encodeDiagnostics...)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

// ResponseFailed reports whether a decoded response body represents a failure.
func ResponseFailed(source Format, body []byte) bool {
	response, _, err := DecodeResponse(source, body)
	if err != nil {
		return false
	}
	return responseIsFailed(response)
}

// responseIsFailed is the single definition of "the upstream said it failed":
// an explicit failed status, or an error object that actually carries
// information. An empty error object on a completed response is not a failure.
func responseIsFailed(response *Response) bool {
	if response == nil {
		return false
	}
	if response.Status == "failed" {
		return true
	}
	if response.Error == nil {
		return false
	}
	return response.Error.Message != "" || response.Error.Type != "" || response.Error.Code != ""
}

// ConvertResponse decodes a whole response in source and renders it in target.
func ConvertResponse(source, target Format, body []byte) ([]byte, []Diagnostic, error) {
	response, diagnostics, err := DecodeResponse(source, body)
	if err != nil {
		return nil, diagnostics, err
	}
	encoded, encodeDiagnostics, err := EncodeResponse(response, target)
	diagnostics = append(diagnostics, encodeDiagnostics...)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

// StripHostedSearchTools removes provider-native hosted search tools, which no
// function-calling upstream can execute. It also removes the matching search
// option fields. changed reports whether the body was rewritten.
func StripHostedSearchTools(body []byte, format Format) (out []byte, changed bool, err error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, false, fmt.Errorf("decode %s request: %w", format, err)
	}
	changed = false
	if format == OpenAIChat {
		if _, exists := object["web_search_options"]; exists {
			delete(object, "web_search_options")
			changed = true
		}
	}
	if tools, ok := object["tools"].([]any); ok {
		filtered := make([]any, 0, len(tools))
		for _, raw := range tools {
			if format == Gemini {
				// A Gemini tool object can carry googleSearch beside
				// functionDeclarations: strip only the hosted keys.
				if tool, ok := raw.(map[string]any); ok {
					cleaned := map[string]any{}
					for key, value := range tool {
						if key == "googleSearch" || key == "google_search" ||
							key == "googleSearchRetrieval" || key == "google_search_retrieval" {
							changed = true
							continue
						}
						cleaned[key] = value
					}
					if len(cleaned) == 0 {
						continue
					}
					filtered = append(filtered, cleaned)
					continue
				}
			}
			if hostedSearchTool(format, raw) {
				changed = true
				continue
			}
			filtered = append(filtered, raw)
		}
		if changed {
			object["tools"] = filtered
		}
	}
	// Gemini also carries search grounding in its own field.
	if format == Gemini {
		for _, key := range []string{"googleSearch", "google_search", "googleSearchRetrieval", "google_search_retrieval"} {
			if _, exists := object[key]; exists {
				delete(object, key)
				changed = true
			}
		}
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := marshalJSON(object)
	if err != nil {
		return nil, false, fmt.Errorf("encode %s request: %w", format, err)
	}
	return encoded, true, nil
}

func hostedSearchTool(format Format, raw any) bool {
	tool, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	typeName := stringAt(tool, "type")
	switch format {
	case OpenAIChat, OpenAIResponses:
		return strings.HasPrefix(typeName, "web_search")
	case ClaudeMessages:
		return typeName == "web_search" || strings.HasPrefix(typeName, "web_search_")
	case Gemini:
		_, hasCamel := tool["googleSearch"]
		_, hasSnake := tool["google_search"]
		return hasCamel || hasSnake
	}
	return false
}

// ErrorEnvelope renders an upstream error body in the client dialect. The
// upstream payload is decoded leniently: a JSON error object, a bare message,
// or plain text all produce a usable client error.
func ErrorEnvelope(body []byte, target Format, status int) []byte {
	streamError := decodeErrorPayload(body, status)
	var envelope map[string]any
	switch target {
	case ClaudeMessages:
		envelope = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    anthropicErrorType(status),
				"message": streamError.Message,
			},
		}
	case Gemini:
		envelope = map[string]any{
			"error": map[string]any{
				"code": status, "message": streamError.Message,
				"status": geminiErrorStatus(status),
			},
		}
	case OpenAIResponses, OpenAIChat:
		errorType := openAIErrorType(status)
		code := openAIErrorCode(status, streamError)
		envelope = map[string]any{
			"error": map[string]any{
				"message": streamError.Message, "type": errorType,
				"code": code, "param": nil,
			},
		}
	default:
		envelope = map[string]any{
			"error": map[string]any{"message": streamError.Message, "type": "upstream_error"},
		}
	}
	encoded, err := marshalJSON(envelope)
	if err != nil {
		return []byte(`{"error":{"message":"upstream request failed"}}`)
	}
	return encoded
}

// decodeErrorPayload extracts a message and code from the many error shapes
// upstreams produce.
func decodeErrorPayload(body []byte, status int) *StreamError {
	streamError := &StreamError{Message: httpStatusText(status)}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return streamError
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		streamError.Message = trimmed
		return streamError
	}
	object, ok := value.(map[string]any)
	if !ok {
		streamError.Message = trimmed
		return streamError
	}
	if nested := objectAt(object, "error"); nested != nil {
		if text := firstNonEmptyString(nested, "message", "msg", "detail"); text != "" {
			streamError.Message = text
		}
		streamError.Type = firstNonEmptyString(nested, "type", "status")
		streamError.Code = firstNonEmptyString(nested, "code", "status")
		if streamError.Code == "" {
			if number, ok := intAt(nested, "code"); ok {
				streamError.Code = fmt.Sprintf("%d", number)
			}
		}
		return streamError
	}
	if text := firstNonEmptyString(object, "message", "msg", "detail", "error"); text != "" {
		streamError.Message = text
	}
	if base := objectAt(object, "base_resp"); base != nil {
		if text := stringAt(base, "status_msg"); text != "" {
			streamError.Message = text
		}
	}
	return streamError
}

func firstNonEmptyString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAt(object, key); value != "" {
			return value
		}
	}
	return ""
}

// anthropicErrorType maps an HTTP status onto Anthropic's error taxonomy.
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529, 503:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// openAIErrorType maps an HTTP status onto OpenAI's error taxonomy.
func openAIErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 500:
		return "server_error"
	case 503:
		return "service_unavailable"
	}
	if status >= 500 {
		return "server_error"
	}
	return "invalid_request_error"
}

// openAIErrorCode preserves an upstream machine code when it has one; the
// HTTP status is the fallback.
func openAIErrorCode(status int, streamError *StreamError) string {
	if streamError != nil && streamError.Code != "" && !looksLikeHTTPStatus(streamError.Code) {
		return streamError.Code
	}
	switch status {
	case 401:
		return "invalid_api_key"
	case 404:
		return "model_not_found"
	case 429:
		return "rate_limit_exceeded"
	}
	return ""
}

func looksLikeHTTPStatus(code string) bool {
	switch code {
	case "400", "401", "403", "404", "413", "429", "500", "502", "503", "504", "529":
		return true
	}
	return false
}

// geminiErrorStatus maps an HTTP status onto google.rpc.Code names.
func geminiErrorStatus(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 409:
		return "ABORTED"
	case 413:
		return "INVALID_ARGUMENT"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 499:
		return "CANCELLED"
	case 500:
		return "INTERNAL"
	case 501:
		return "UNIMPLEMENTED"
	case 503:
		return "UNAVAILABLE"
	case 504:
		return "DEADLINE_EXCEEDED"
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}

func httpStatusText(status int) string {
	if text := statusText(status); text != "" {
		return text
	}
	return fmt.Sprintf("upstream request failed with status %d", status)
}
