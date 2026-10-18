package codec

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The Gemini Interactions dialect (POST /v1beta/interactions) is Google's
// stateful primitive: a request carries a model (or agent) plus input that may
// be a string, Content, an array of Content, or an array of Steps, and the
// response is an Interaction whose steps[] transcript holds the model output,
// thoughts and tool calls.
//
// Canonical mapping:
//   - input Content[] / string        -> Messages (user turn)
//   - input Step[]                    -> Messages (user_input, model_output,
//     function_call, function_result, thought)
//   - system_instruction              -> Request.System
//   - tools[].function                -> Tool
//   - generation_config               -> temperature/top_p/max_output_tokens/
//     stop_sequences/seed/tool_choice/thinking_level
//   - response_format (+ mime type)   -> ResponseFormat
//   - steps[].model_output.content    -> Response.Parts
//   - steps[].function_call           -> PartToolCall (id/name/arguments)
//   - steps[].thought                 -> PartReasoning (+ signature)
//   - usage                           -> Usage (totals + thought/tool tokens)

// decodeInteractionsRequest reads an Interactions request into canonical form.
func decodeInteractionsRequest(body []byte, diagnostics *[]Diagnostic) (*Request, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	request := &Request{
		Model:  firstStringAt(object, "model", "agent"),
		Stream: boolValue(object, "stream"),
		Extra:  map[string]json.RawMessage{},
	}
	if request.Model == "" {
		// `model` may arrive as an object with a `name` field on some SDK paths.
		if model := objectAt(object, "model"); model != nil {
			request.Model = stringAt(model, "name")
		}
	}
	if instruction := stringAt(object, "system_instruction"); instruction != "" {
		request.System = []Part{{Kind: PartText, Text: instruction}}
	}
	if config := objectAt(object, "generation_config"); config != nil {
		request.Temperature = floatPtr(config, "temperature")
		request.TopP = floatPtr(config, "top_p")
		request.MaxTokens = intPtr(config, "max_output_tokens")
		request.Seed = int64Ptr(config, "seed")
		if values := arrayAt(config, "stop_sequences"); values != nil {
			for index, item := range values {
				if text, ok := item.(string); ok {
					request.Stop = append(request.Stop, text)
				} else {
					appendDiagnostic(diagnostics, "invalid_stop_entry",
						fmt.Sprintf("stop_sequences entry %d is not a string", index),
						fmt.Sprintf("generation_config.stop_sequences[%d]", index))
				}
			}
		}
		request.Reasoning = interactionsReasoning(config)
		request.ToolChoice = decodeInteractionsToolChoice(config["tool_choice"], diagnostics)
	}
	request.ResponseFormat = decodeInteractionsResponseFormat(object, diagnostics)
	request.Tools = decodeInteractionsTools(arrayAt(object, "tools"), diagnostics)

	if raw := object["input"]; raw != nil {
		messages, system, err := decodeInteractionsInput(raw, diagnostics)
		if err != nil {
			return nil, err
		}
		request.Messages = messages
		if len(system) > 0 {
			request.System = append(request.System, system...)
		}
	}

	// Stateful and cache fields the converter cannot resolve are preserved so a
	// Gemini target can forward them and any other target can diagnose them.
	for _, key := range []string{"previous_interaction_id", "cached_content", "store", "background", "labels", "service_tier", "response_mime_type", "response_modalities"} {
		if raw, ok := object[key]; ok {
			request.Extra[key] = rawObject(raw)
		}
	}
	if len(request.Extra) == 0 {
		request.Extra = nil
	}
	return request, nil
}

// decodeInteractionsInput handles every documented input shape.
func decodeInteractionsInput(input any, diagnostics *[]Diagnostic) ([]Message, []Part, error) {
	switch value := input.(type) {
	case nil:
		return nil, nil, nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, nil, nil
		}
		return []Message{{Role: RoleUser, Parts: []Part{{Kind: PartText, Text: value}}}}, nil, nil
	case map[string]any:
		// A single Content object.
		parts := decodeInteractionsContent(value, diagnostics, "input")
		if len(parts) == 0 {
			return nil, nil, nil
		}
		return []Message{{Role: RoleUser, Parts: parts}}, nil, nil
	case []any:
		var messages []Message
		var system []Part
		// A Content[] array is ONE user turn, so consecutive Content items
		// accumulate into a single message; a Step flushes it.
		var pending []Part
		flush := func() {
			if len(pending) == 0 {
				return
			}
			messages = append(messages, Message{Role: RoleUser, Parts: pending})
			pending = nil
		}
		for index, raw := range value {
			item, ok := raw.(map[string]any)
			if !ok {
				appendDiagnostic(diagnostics, "invalid_input_item",
					"input item is not an object and was dropped", fmt.Sprintf("input[%d]", index))
				continue
			}
			itemPath := fmt.Sprintf("input[%d]", index)
			// Content and Step both carry a `type`, so only the documented step
			// types are routed to the step decoder; every other type is Content
			// (a Content[] input array used to be dropped entirely).
			if stepType := stringAt(item, "type"); stepType != "" && isInteractionsStepType(stepType) {
				flush()
				stepMessages, stepSystem, err := decodeInteractionsStep(stepType, item, diagnostics, itemPath)
				if err != nil {
					return nil, nil, err
				}
				messages = append(messages, stepMessages...)
				system = append(system, stepSystem...)
				continue
			}
			pending = append(pending, decodeInteractionsContent(item, diagnostics, itemPath)...)
		}
		flush()
		return messages, system, nil
	}
	return nil, nil, conversionErrorf("invalid_input",
		"input must be a string, a Content object, or an array of Content/Step items")
}

// interactionsStepTypes is the documented set of input Step discriminators.
var interactionsStepTypes = map[string]bool{
	"user_input": true, "model_output": true, "thought": true,
	"function_call": true, "function_result": true,
	"google_search_call": true, "google_search_result": true,
	"file_search_call": true, "file_search_result": true,
	"code_execution_call": true, "code_execution_result": true,
	"url_context_call": true, "url_context_result": true,
	"mcp_server_tool_call": true, "mcp_server_tool_result": true,
	"retrieval_call": true, "retrieval_result": true,
	"google_maps_call": true, "google_maps_result": true,
	"processing_call": true, "processing_result": true,
	"model_output_error": true, "error": true,
}

func isInteractionsStepType(name string) bool {
	return interactionsStepTypes[name]
}

// decodeInteractionsStep converts one input Step into canonical messages.
func decodeInteractionsStep(stepType string, item map[string]any, diagnostics *[]Diagnostic, path string) ([]Message, []Part, error) {
	switch stepType {
	case "user_input":
		parts := decodeInteractionsContentList(arrayAt(item, "content"), diagnostics, path+".content")
		if len(parts) == 0 {
			return nil, nil, nil
		}
		return []Message{{Role: RoleUser, Parts: parts}}, nil, nil
	case "model_output":
		parts := decodeInteractionsContentList(arrayAt(item, "content"), diagnostics, path+".content")
		if len(parts) == 0 {
			return nil, nil, nil
		}
		return []Message{{Role: RoleAssistant, Parts: parts}}, nil, nil
	case "thought":
		var parts []Part
		for _, raw := range arrayAt(item, "summary") {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := stringAt(entry, "text"); text != "" {
				parts = append(parts, Part{Kind: PartReasoning, Text: text, ThoughtSignature: stringAt(item, "signature")})
			}
		}
		if len(parts) == 0 && stringAt(item, "signature") != "" {
			parts = append(parts, Part{Kind: PartReasoning, ThoughtSignature: stringAt(item, "signature")})
		}
		if len(parts) == 0 {
			return nil, nil, nil
		}
		return []Message{{Role: RoleAssistant, Parts: parts}}, nil, nil
	case "function_call":
		callID := stringAt(item, "id")
		name := stringAt(item, "name")
		if name == "" {
			appendDiagnostic(diagnostics, "invalid_tool_call",
				"function_call step has no name and was dropped", path)
			return nil, nil, nil
		}
		arguments := rawObject(item["arguments"])
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		part := Part{
			Kind: PartToolCall, ToolCallID: callID, ToolName: name,
			Arguments: arguments, ArgumentsText: string(arguments),
		}
		return []Message{{Role: RoleAssistant, Parts: []Part{part}}}, nil, nil
	case "function_result":
		callID := stringAt(item, "call_id")
		name := stringAt(item, "name")
		content := decodeInteractionsFunctionResult(item["result"], diagnostics, path+".result")
		if callID == "" {
			appendDiagnostic(diagnostics, "orphan_tool_result",
				"function_result step has no call_id; the result cannot be paired", path)
			return nil, nil, nil
		}
		isError := boolValue(item, "is_error")
		return []Message{{Role: RoleTool, Parts: []Part{
			toolResultPart(callID, name, content, isError),
		}}}, nil, nil
	case "google_search_call", "file_search_call", "code_execution_call", "url_context_call",
		"mcp_server_tool_call", "retrieval_call", "google_maps_call", "processing_call":
		appendDiagnostic(diagnostics, "hosted_step_dropped",
			fmt.Sprintf("provider-hosted step %q cannot be replayed to a non-Gemini target", stepType), path)
		return nil, nil, nil
	case "google_search_result", "file_search_result", "code_execution_result", "url_context_result",
		"mcp_server_tool_result", "retrieval_result", "google_maps_result", "processing_result":
		appendDiagnostic(diagnostics, "hosted_step_dropped",
			fmt.Sprintf("provider-hosted step %q cannot be replayed to a non-Gemini target", stepType), path)
		return nil, nil, nil
	case "model_output_error", "error":
		appendDiagnostic(diagnostics, "error_step_dropped",
			"an error step in the input cannot be replayed as conversation content", path)
		return nil, nil, nil
	default:
		appendDiagnostic(diagnostics, "unknown_step_dropped",
			fmt.Sprintf("unknown step type %q was dropped", stepType), path)
		return nil, nil, nil
	}
}

// decodeInteractionsContentList reads a Content[] array.
func decodeInteractionsContentList(list []any, diagnostics *[]Diagnostic, path string) []Part {
	var parts []Part
	for index, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts = append(parts, decodeInteractionsContent(entry, diagnostics, fmt.Sprintf("%s[%d]", path, index))...)
	}
	return parts
}

// decodeInteractionsContent reads one Content object (text, image, audio, video,
// document, file).
func decodeInteractionsContent(content map[string]any, diagnostics *[]Diagnostic, path string) []Part {
	contentType := stringAt(content, "type")
	switch contentType {
	case "", "text":
		if text := stringAt(content, "text"); text != "" {
			return []Part{{Kind: PartText, Text: text}}
		}
		return nil
	case "image":
		return []Part{decodeInteractionsMedia(content, PartImage)}
	case "audio":
		return []Part{decodeInteractionsMedia(content, PartAudio)}
	case "video":
		return []Part{decodeInteractionsMedia(content, PartVideo)}
	case "document":
		return []Part{decodeInteractionsMedia(content, PartDocument)}
	case "file":
		// A file reference by id/uri: the proxy has no file store, so it is
		// reported rather than silently dropped.
		if uri := firstStringAt(content, "uri", "file_uri", "file_url"); uri != "" {
			return []Part{{Kind: PartDocument, URL: uri, MediaType: stringAt(content, "mime_type")}}
		}
		if id := firstStringAt(content, "file_id", "id"); id != "" {
			appendDiagnostic(diagnostics, "file_id_dropped",
				"an Interactions file id cannot be resolved by the conversion layer", path)
			return nil
		}
		appendDiagnostic(diagnostics, "invalid_file", "file content has no uri or id", path)
		return nil
	default:
		appendDiagnostic(diagnostics, "content_part_dropped",
			fmt.Sprintf("content type %q cannot be represented", contentType), path)
		return nil
	}
}

// decodeInteractionsMedia reads inline base64 or a URI from a media Content.
func decodeInteractionsMedia(content map[string]any, kind PartKind) Part {
	mediaType := firstStringAt(content, "mime_type", "mimeType")
	if data := stringAt(content, "data"); data != "" {
		if parsedType, base64Data, ok := parseDataURL(data); ok {
			if mediaType == "" {
				mediaType = parsedType
			}
			return Part{Kind: kind, MediaType: mediaType, Base64: base64Data}
		}
		return Part{Kind: kind, MediaType: mediaType, Base64: data}
	}
	return Part{Kind: kind, MediaType: mediaType, URL: firstStringAt(content, "uri", "url")}
}

// decodeInteractionsFunctionResult renders a function_result payload as parts.
func decodeInteractionsFunctionResult(result any, diagnostics *[]Diagnostic, path string) []Part {
	switch value := result.(type) {
	case nil:
		return nil
	case string:
		if value == "" {
			return nil
		}
		return []Part{{Kind: PartText, Text: value}}
	case []any:
		// A subcontent list is the schema's multi-part result form.
		return decodeInteractionsContentList(value, diagnostics, path)
	case map[string]any:
		// A structured result (often {"result": ...} or subcontents[]).
		if subcontents := arrayAt(value, "subcontents"); subcontents != nil {
			return decodeInteractionsContentList(subcontents, diagnostics, path+".subcontents")
		}
		return []Part{{Kind: PartText, Text: string(mustMarshal(value))}}
	default:
		return []Part{{Kind: PartText, Text: string(mustMarshal(value))}}
	}
}

func decodeInteractionsTools(raw []any, diagnostics *[]Diagnostic) []Tool {
	var tools []Tool
	for index, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// A callable tool is a flat object: type:"function" with name,
		// description and parameters at the top level. Everything else is a
		// provider-hosted kind (google_search, file_search, code_execution, ...).
		// A nested {"function": {...}} is also accepted for compatibility with
		// clients that model it that way.
		function := tool
		if nested := objectAt(tool, "function"); nested != nil {
			function = nested
		}
		if kind := stringAt(tool, "type"); kind != "" && kind != "function" {
			appendDiagnostic(diagnostics, "hosted_tool_dropped",
				fmt.Sprintf("Interactions hosted tool %q cannot be represented as a function", kind),
				fmt.Sprintf("tools[%d]", index))
			continue
		}
		name := stringAt(function, "name")
		if name == "" {
			appendDiagnostic(diagnostics, "invalid_tool", "tool has no name and was dropped", fmt.Sprintf("tools[%d]", index))
			continue
		}
		tools = append(tools, Tool{
			Name:        name,
			Description: stringAt(function, "description"),
			Parameters:  rawObject(function["parameters"]),
		})
	}
	return tools
}

func decodeInteractionsToolChoice(raw any, diagnostics *[]Diagnostic) *ToolChoice {
	switch value := raw.(type) {
	case nil:
		return nil
	case string:
		switch value {
		case "auto", "none", "required":
			return &ToolChoice{Mode: ToolChoiceMode(value)}
		case "any":
			return &ToolChoice{Mode: ToolChoiceRequired}
		}
	case map[string]any:
		// ToolChoiceConfig: {"allowed_tools": {"mode": "auto"|"any"|"none",
		// "tools": [names]}}. A flat {type, function_name} shape is accepted for
		// compatibility with clients that model it that way.
		if allowed := objectAt(value, "allowed_tools"); allowed != nil {
			mode := strings.ToLower(stringAt(allowed, "mode"))
			names := arrayAt(allowed, "tools")
			// The mode decides first: a single allowed name only narrows an
			// "any" policy, it does not override "none" or "validated".
			switch mode {
			case "auto":
				return &ToolChoice{Mode: ToolChoiceAuto}
			case "none":
				return &ToolChoice{Mode: ToolChoiceNone}
			case "any", "required":
				if len(names) == 1 {
					if name, ok := names[0].(string); ok && name != "" {
						return &ToolChoice{Mode: ToolChoiceTool, Name: name}
					}
				}
				if len(names) > 1 {
					appendDiagnostic(diagnostics, "tool_choice_approximated",
						fmt.Sprintf("%d allowed tools were collapsed to a required tool call", len(names)),
						"generation_config.tool_choice")
				}
				return &ToolChoice{Mode: ToolChoiceRequired}
			case "validated":
				// Validated mode asks the provider to check the arguments; the
				// closest portable policy is an unrestricted tool call.
				appendDiagnostic(diagnostics, "tool_choice_approximated",
					"validated tool choice was approximated as a required tool call",
					"generation_config.tool_choice")
				if len(names) == 1 {
					if name, ok := names[0].(string); ok && name != "" {
						return &ToolChoice{Mode: ToolChoiceTool, Name: name}
					}
				}
				return &ToolChoice{Mode: ToolChoiceRequired}
			}
			if len(names) == 1 {
				if name, ok := names[0].(string); ok && name != "" {
					return &ToolChoice{Mode: ToolChoiceTool, Name: name}
				}
			}
			if len(names) > 1 {
				appendDiagnostic(diagnostics, "tool_choice_approximated",
					fmt.Sprintf("%d allowed tools were collapsed to a required tool call", len(names)),
					"generation_config.tool_choice")
				return &ToolChoice{Mode: ToolChoiceRequired}
			}
			if mode != "" {
				appendDiagnostic(diagnostics, "tool_choice_dropped",
					fmt.Sprintf("tool_choice mode %q has no equivalent", mode), "generation_config.tool_choice")
			}
			return nil
		}
		typeName := firstStringAt(value, "type", "mode")
		switch typeName {
		case "auto":
			return &ToolChoice{Mode: ToolChoiceAuto}
		case "none":
			return &ToolChoice{Mode: ToolChoiceNone}
		case "required", "any":
			return &ToolChoice{Mode: ToolChoiceRequired}
		case "function":
			if name := firstStringAt(value, "function_name", "name"); name != "" {
				return &ToolChoice{Mode: ToolChoiceTool, Name: name}
			}
			return &ToolChoice{Mode: ToolChoiceRequired}
		}
	}
	if raw != nil {
		appendDiagnostic(diagnostics, "tool_choice_dropped", "tool_choice has an unsupported shape", "generation_config.tool_choice")
	}
	return nil
}

func decodeInteractionsResponseFormat(object map[string]any, diagnostics *[]Diagnostic) *ResponseFormat {
	mimeType := stringAt(object, "response_mime_type")
	raw := object["response_format"]
	if raw == nil && mimeType == "" {
		return nil
	}
	format := &ResponseFormat{}
	if entry, ok := raw.(map[string]any); ok {
		// TextResponseFormat: type:"text" plus mime_type and an optional schema.
		// A json_schema/json_object discriminator is accepted for compatibility.
		switch stringAt(entry, "type") {
		case "text", "":
			entryMime := stringAt(entry, "mime_type")
			switch {
			case len(rawObject(entry["schema"])) > 0:
				format.Kind = FormatJSONSchema
				format.Schema = rawObject(entry["schema"])
				format.Name = stringAt(entry, "name")
			case entryMime == "application/json":
				format.Kind = FormatJSONObject
			default:
				format.Kind = FormatText
			}
		case "json_object":
			format.Kind = FormatJSONObject
		case "json_schema":
			format.Kind = FormatJSONSchema
			format.Name = stringAt(entry, "name")
			format.Description = stringAt(entry, "description")
			format.Schema = rawObject(entry["schema"])
		default:
			appendDiagnostic(diagnostics, "response_format_dropped",
				fmt.Sprintf("response_format type %q is unknown", stringAt(entry, "type")), "response_format")
		}
	}
	if format.Kind == "" {
		switch mimeType {
		case "application/json":
			format.Kind = FormatJSONObject
		case "", "text/plain":
			return nil
		default:
			appendDiagnostic(diagnostics, "response_format_dropped",
				fmt.Sprintf("response_mime_type %q has no equivalent", mimeType), "response_mime_type")
			return nil
		}
	}
	return format
}

func interactionsReasoning(config map[string]any) *ReasoningConfig {
	level := stringAt(config, "thinking_level")
	summaries := stringAt(config, "thinking_summaries")
	if level == "" && summaries == "" {
		return nil
	}
	reasoning := &ReasoningConfig{}
	switch strings.ToLower(level) {
	case "minimal", "low", "medium", "high":
		// Preserve the vendor's own vocabulary: the encoders map it, and
		// collapsing "minimal" into "low" lost a documented level.
		reasoning.Effort = strings.ToLower(level)
	case "default":
		reasoning.Effort = "medium"
	case "max":
		reasoning.Effort = "high"
	}
	if summaries != "" {
		reasoning.Summary = summaries
	}
	if reasoning.Effort == "" && reasoning.Summary == "" {
		return nil
	}
	return reasoning
}

func firstStringAt(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAt(object, key); value != "" {
			return value
		}
	}
	return ""
}
