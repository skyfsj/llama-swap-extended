package relaykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type Format = types.RelayFormat

const (
	OpenAIChat      = types.RelayFormatOpenAI
	OpenAIResponses = types.RelayFormatOpenAIResponses
	ClaudeMessages  = types.RelayFormatClaude
	Gemini          = types.RelayFormatGemini
)

type Route struct {
	Format Format
	Model  string
	Stream bool
	Action string
}

func RouteForRequest(method, path string) (Route, bool) {
	if strings.ToUpper(method) != "POST" {
		return Route{}, false
	}
	switch path {
	case "/v1/chat/completions", "/v/chat/completions":
		return Route{Format: OpenAIChat}, true
	case "/v1/responses", "/v/responses":
		return Route{Format: OpenAIResponses}, true
	case "/v1/messages", "/v/messages":
		return Route{Format: ClaudeMessages}, true
	}
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return Route{}, false
	}
	modelAction := strings.TrimPrefix(path, prefix)
	for _, action := range []struct {
		name   string
		stream bool
	}{{"streamGenerateContent", true}, {"generateContent", false}} {
		suffix := ":" + action.name
		if !strings.HasSuffix(modelAction, suffix) {
			continue
		}
		model := strings.TrimSuffix(modelAction, suffix)
		model = strings.Trim(model, "/")
		if model == "" {
			return Route{}, false
		}
		return Route{Format: Gemini, Model: model, Stream: action.stream, Action: action.name}, true
	}
	return Route{}, false
}

func RequestStreamed(route Route, body []byte) bool {
	if route.Format == Gemini {
		return route.Stream
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func TargetPath(format Format, model string, stream bool) (string, error) {
	switch format {
	case OpenAIChat:
		return "/v1/chat/completions", nil
	case OpenAIResponses:
		return "/v1/responses", nil
	case ClaudeMessages:
		return "/v1/messages", nil
	case Gemini:
		model = strings.Trim(model, "/")
		if model == "" {
			return "", errors.New("gemini target requires a model")
		}
		action := "generateContent"
		if stream {
			action = "streamGenerateContent"
		}
		return "/v1beta/models/" + model + ":" + action, nil
	default:
		return "", fmt.Errorf("unsupported relaykit format %q", format)
	}
}

func TargetPathForRequest(format Format, model string, stream bool, publicPath string) (string, error) {
	path, err := TargetPath(format, model, stream)
	if err != nil || format == Gemini || !strings.HasPrefix(publicPath, "/v/") {
		return path, err
	}
	return "/v" + strings.TrimPrefix(path, "/v1"), nil
}

func NewMeta(model string, stream bool) *convmeta.Values {
	return &convmeta.Values{
		OriginModelName:   model,
		UpstreamModelName: model,
		IsStream:          stream,
		Options: &convmeta.Options{
			Claude: convmeta.ClaudeOptions{
				DefaultMaxTokens: func(string) int { return 4096 },
			},
			WebSearch: func(convmeta.WebSearchCall) (*convmeta.WebSearchEncoding, error) {
				return &convmeta.WebSearchEncoding{Omitted: true}, nil
			},
		},
	}
}

func StripHostedSearchTools(body []byte, format Format) ([]byte, bool, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, fmt.Errorf("decode %s request: %w", format, err)
	}
	changed := false
	if format == OpenAIChat {
		if _, exists := request["web_search_options"]; exists {
			delete(request, "web_search_options")
			changed = true
		}
	}
	tools, ok := request["tools"].([]any)
	if ok {
		filtered := make([]any, 0, len(tools))
		for _, raw := range tools {
			if hostedSearchTool(format, raw) {
				changed = true
				continue
			}
			filtered = append(filtered, raw)
		}
		if changed {
			request["tools"] = filtered
		}
	}
	if !changed {
		return body, false, nil
	}
	encoded, err := json.Marshal(request)
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
	typeName, _ := tool["type"].(string)
	switch format {
	case OpenAIChat, OpenAIResponses:
		return strings.HasPrefix(typeName, "web_search")
	case ClaudeMessages:
		return typeName == "web_search" || strings.HasPrefix(typeName, "web_search_")
	case Gemini:
		_, hasCamel := tool["googleSearch"]
		_, hasSnake := tool["google_search"]
		return hasCamel || hasSnake
	default:
		return false
	}
}

func DecodeRequest(format Format, body []byte) (any, error) {
	var request any
	switch format {
	case OpenAIChat:
		request = &dto.GeneralOpenAIRequest{}
	case OpenAIResponses:
		request = &dto.OpenAIResponsesRequest{}
	case ClaudeMessages:
		request = &dto.ClaudeRequest{}
	case Gemini:
		request = &dto.GeminiChatRequest{}
	default:
		return nil, fmt.Errorf("unsupported relaykit request format %q", format)
	}
	if err := json.Unmarshal(body, request); err != nil {
		return nil, fmt.Errorf("decode %s request: %w", format, err)
	}
	return request, nil
}

func ConvertRequest(ctx context.Context, meta convmeta.Meta, source, target Format, body []byte, model string) (*relayconvert.RequestResult, error) {
	request, err := DecodeRequest(source, body)
	if err != nil {
		return nil, err
	}
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	case *dto.OpenAIResponsesRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	case *dto.ClaudeRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	}
	if source == Gemini && target == OpenAIResponses {
		chat, err := relayconvert.ConvertRequest(ctx, meta, OpenAIChat, request)
		if err != nil {
			return nil, err
		}
		chatRequest, ok := chat.Value.(*dto.GeneralOpenAIRequest)
		if !ok {
			return nil, fmt.Errorf("gemini-to-chat converter returned %T", chat.Value)
		}
		setRequestModel(chatRequest, model)
		result, err := relayconvert.ConvertRequest(ctx, meta, OpenAIResponses, chatRequest)
		if err != nil {
			return nil, err
		}
		result.From = Gemini
		setRequestModel(result.Value, model)
		result.Steps = append(append([]relayconvert.RequestStep(nil), chat.Steps...), result.Steps...)
		result.Diagnostics = append(append([]types.ConversionDiagnostic(nil), chat.Diagnostics...), result.Diagnostics...)
		return result, nil
	}
	result, err := relayconvert.ConvertRequest(ctx, meta, target, request)
	if err != nil {
		return nil, err
	}
	setRequestModel(result.Value, model)
	return result, nil
}

func setRequestModel(request any, model string) {
	switch typed := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	case *dto.OpenAIResponsesRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	case *dto.ClaudeRequest:
		if typed.Model == "" {
			typed.Model = model
		}
	}
}

func DecodeResponse(format Format, body []byte) (any, error) {
	var response any
	switch format {
	case OpenAIChat:
		response = &dto.OpenAITextResponse{}
	case OpenAIResponses:
		response = &dto.OpenAIResponsesResponse{}
	case ClaudeMessages:
		response = &dto.ClaudeResponse{}
	case Gemini:
		response = &dto.GeminiChatResponse{}
	default:
		return nil, fmt.Errorf("unsupported relaykit response format %q", format)
	}
	if err := json.Unmarshal(body, response); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", format, err)
	}
	return response, nil
}

func ConvertResponse(ctx context.Context, meta convmeta.Meta, source, target Format, body []byte) (*relayconvert.ResponseResult, error) {
	response, err := DecodeResponse(source, body)
	if err != nil {
		return nil, err
	}
	result, err := relayconvert.ConvertResponse(ctx, meta, target, response)
	if err != nil {
		return nil, err
	}
	if target == OpenAIResponses && source != OpenAIResponses {
		if converted, ok := result.Value.(*dto.OpenAIResponsesResponse); ok && converted.ID != "" && !strings.HasPrefix(converted.ID, "resp_") {
			converted.ID = "resp_" + converted.ID
		}
	}
	return result, nil
}

func DecodeStreamResponse(format Format, body []byte) (any, error) {
	var response any
	switch format {
	case OpenAIChat:
		response = &dto.ChatCompletionsStreamResponse{}
	case OpenAIResponses:
		response = &dto.ResponsesStreamResponse{}
	case ClaudeMessages:
		response = &dto.ClaudeResponse{}
	case Gemini:
		response = &dto.GeminiChatResponse{}
	default:
		return nil, fmt.Errorf("unsupported relaykit stream format %q", format)
	}
	if err := json.Unmarshal(body, response); err != nil {
		return nil, fmt.Errorf("decode %s stream event: %w", format, err)
	}
	return response, nil
}

func EncodeSSE(format Format, response any) ([]byte, error) {
	if format == OpenAIResponses {
		switch event := response.(type) {
		case relayconvert.ChatToResponsesStreamEvent:
			return encodeTypedSSE(event.Type, event.Payload)
		case *relayconvert.ChatToResponsesStreamEvent:
			if event != nil {
				return encodeTypedSSE(event.Type, event.Payload)
			}
		}
	}
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode %s stream event: %w", format, err)
	}
	if format == OpenAIChat || format == Gemini {
		return []byte("data: " + string(data) + "\n\n"), nil
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil || event.Type == "" {
		return nil, fmt.Errorf("%s stream event has no type", format)
	}
	return encodeTypedSSE(event.Type, json.RawMessage(data))
}

func encodeTypedSSE(event string, response any) ([]byte, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode typed stream event: %w", err)
	}
	if strings.ContainsAny(event, "\r\n") || event == "" {
		return nil, fmt.Errorf("invalid stream event type %q", event)
	}
	return []byte("event: " + event + "\ndata: " + string(data) + "\n\n"), nil
}

func DoneSSE(format Format) []byte {
	if format == OpenAIChat {
		return []byte("data: [DONE]\n\n")
	}
	return nil
}

func StreamResponseID(format Format, response any) string {
	var id string
	switch typed := response.(type) {
	case *dto.ChatCompletionsStreamResponse:
		id = typed.Id
	case *dto.ClaudeResponse:
		id = typed.Id
		if id == "" && typed.Message != nil {
			id = typed.Message.Id
		}
	case *dto.OpenAIResponsesResponse:
		id = typed.ID
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "resp_") {
		return id
	}
	return "resp_" + id
}

func Marshal(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode converted request: %w", err)
	}
	return data, nil
}
