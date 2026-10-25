package codec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	if source != target && request.N != nil && *request.N > 1 && (request.Stream || options.Stream != nil && *options.Stream) {
		return nil, diagnostics, conversionErrorf("n_unsupported", "cross-dialect streaming supports one candidate per request")
	}
	if err := rejectUnrepresentableCandidates(source, target, request); err != nil {
		return nil, diagnostics, err
	}
	encoded, encodeDiagnostics, err := EncodeRequest(request, target, options)
	diagnostics = append(diagnostics, encodeDiagnostics...)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

// rejectUnrepresentableCandidates fails a multi-candidate request when the
// target cannot return them all. Chat and Gemini both produce several
// candidates, so a conversion between them keeps n. A target that returns one
// answer would otherwise make the upstream generate candidates that are
// discarded after the fact.
func rejectUnrepresentableCandidates(source, target Format, request *Request) error {
	if request == nil || request.N == nil || *request.N <= 1 {
		return nil
	}
	if multiCandidateTarget(target) && multiCandidateSource(source) {
		return nil
	}
	if !multiCandidateTarget(target) {
		return conversionErrorf("n_unsupported",
			"n=%d asks for multiple candidates, but %s returns exactly one; send one request per candidate instead",
			*request.N, target)
	}
	return nil
}

// multiCandidateTarget reports whether a dialect can return several answers.
func multiCandidateTarget(format Format) bool {
	return format == OpenAIChat || format == Gemini
}

// multiCandidateSource reports whether a dialect's canonical decode can carry
// several answers. Only these sources reach a multi-candidate target intact.
func multiCandidateSource(format Format) bool {
	return format == OpenAIChat || format == Gemini
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
	return ConvertResponseWithRequest(source, target, body, nil)
}

// ConvertResponseWithRequest is ConvertResponse with the canonical request the
// response answers. A Responses target uses it to restore tool namespaces and
// to echo the request's tool policy.
func ConvertResponseWithRequest(source, target Format, body []byte, request *Request) ([]byte, []Diagnostic, error) {
	if source != target && multiCandidateSource(source) && multiCandidateTarget(target) {
		object, err := decodeObject(body)
		if err != nil {
			return nil, nil, err
		}
		sourceKey, targetKey := "choices", "candidates"
		if source == Gemini {
			sourceKey, targetKey = targetKey, sourceKey
		}
		if choices := arrayAt(object, sourceKey); len(choices) > 1 {
			var output map[string]any
			var convertedChoices []any
			var diagnostics []Diagnostic
			for index, choice := range choices {
				object[sourceKey] = []any{choice}
				input, err := marshalJSON(object)
				if err != nil {
					return nil, diagnostics, err
				}
				converted, notes, err := ConvertResponseWithRequest(source, target, input, request)
				diagnostics = append(diagnostics, notes...)
				if err != nil {
					return nil, diagnostics, err
				}
				result, err := decodeObject(converted)
				if err != nil {
					return nil, diagnostics, err
				}
				if output == nil {
					output = result
				}
				for _, raw := range arrayAt(result, targetKey) {
					item, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					item["index"] = index
					if original, ok := choice.(map[string]any); ok {
						if originalIndex, ok := intAt(original, "index"); ok {
							item["index"] = originalIndex
						}
					}
					convertedChoices = append(convertedChoices, item)
				}
			}
			output[targetKey] = convertedChoices
			converted, err := marshalJSON(output)
			return converted, diagnostics, err
		}
	}
	response, diagnostics, err := DecodeResponse(source, body)
	if err != nil {
		return nil, diagnostics, err
	}
	if source == OpenAIResponses && target == GeminiInteractions {
		diagnostics, err = restoreGatewayToolExecutions(body, response, diagnostics)
		if err != nil {
			return nil, diagnostics, err
		}
	}
	encoded, encodeDiagnostics, err := EncodeResponseWithRequest(response, target, request)
	diagnostics = append(diagnostics, encodeDiagnostics...)
	if err != nil {
		return nil, diagnostics, err
	}
	return encoded, diagnostics, nil
}

// hostedSearchKeyLiterals lists the JSON key fragments whose presence can make
// StripHostedSearchTools rewrite a body. A body containing none of them cannot
// be rewritten, so the expensive generic decode can be skipped. The check is
// deliberately conservative: a fragment that only appears inside a string value
// still routes the body through the decoder, which is correct (it only costs a
// decode). The reverse — skipping a body that does need a rewrite — is not.
var hostedSearchKeyLiterals = map[Format][]string{
	OpenAIChat:      {"web_search_options", "web_search"},
	OpenAIResponses: {"web_search"},
	ClaudeMessages:  {"web_search"},
	Gemini:          {"googleSearch", "google_search", "googleSearchRetrieval", "google_search_retrieval"},
}

// bodyMayCarryHostedSearchTools reports whether the body could contain a hosted
// search tool. It never decides that a body is clean when it might not be: a
// JSON body may spell any character with a \u escape, so a body that contains a
// backslash escape at all is always treated as suspicious.
func bodyMayCarryHostedSearchTools(body []byte, format Format) bool {
	if bytes.Contains(body, []byte("\\")) {
		return true
	}
	for _, literal := range hostedSearchKeyLiterals[format] {
		if bytes.Contains(body, []byte(literal)) {
			return true
		}
	}
	return false
}

// validateObjectBody reproduces the errors decodeObject raises for a body that
// is not a JSON object, without building the decoded map. A body the pre-scan
// cleared must fail exactly the way the decoding path failed, so a caller that
// relies on the rejection (a 400 for a malformed request) still sees it.
func validateObjectBody(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var probe json.RawMessage
	if err := decoder.Decode(&probe); err != nil {
		return err
	}
	if string(bytes.TrimSpace(probe)) == "null" {
		return errors.New("body is not a JSON object")
	}
	if probe[0] != '{' {
		return &json.UnmarshalTypeError{Value: jsonValueKind(probe), Type: reflect.TypeOf(map[string]any{})}
	}
	return nil
}

// jsonValueKind names the JSON kind of a raw value for an UnmarshalTypeError,
// matching what encoding/json reports when decoding into a map.
func jsonValueKind(probe json.RawMessage) string {
	trimmed := bytes.TrimSpace(probe)
	if len(trimmed) == 0 {
		return "invalid"
	}
	switch trimmed[0] {
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case '{':
		return "object"
	}
	return "number"
}

// StripHostedSearchTools removes provider-native hosted search tools, which no
// function-calling upstream can execute. It also removes the matching search
// option fields. changed reports whether the body was rewritten.
func StripHostedSearchTools(body []byte, format Format) (out []byte, changed bool, err error) {
	if !bodyMayCarryHostedSearchTools(body, format) {
		// Nothing to strip, but the decoding path still rejected a body that is
		// not a JSON object, so that rejection is reproduced here.
		if err := validateObjectBody(body); err != nil {
			return nil, false, fmt.Errorf("decode %s request: %w", format, err)
		}
		return body, false, nil
	}
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
