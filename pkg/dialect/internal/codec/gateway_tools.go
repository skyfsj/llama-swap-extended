package codec

import "fmt"

func gatewayToolExecution(item map[string]any, diagnostics *[]Diagnostic, path string) ([]Part, bool) {
	if stringAt(item, "type") != "mcp_call" || stringAt(item, "server_label") != "llama-swap" || stringAt(item, "status") != "completed" {
		return nil, false
	}
	id, name := stringAt(item, "id"), stringAt(item, "name")
	if id == "" || name == "" {
		return nil, false
	}
	arguments := stringAt(item, "arguments")
	if arguments == "" {
		arguments = "{}"
	}
	output := item["output"]
	failed := item["error"] != nil
	if failed {
		output = item["error"]
	}
	return []Part{{Kind: PartToolCall, ToolCallID: id, ToolName: name, Arguments: parseArguments(arguments, diagnostics, path+".arguments"), ArgumentsText: arguments}, toolResultPart(id, name, decodeResponsesToolOutput(output, diagnostics, path+".output"), failed)}, true
}

func restoreGatewayToolExecutions(body []byte, response *Response, diagnostics []Diagnostic) ([]Diagnostic, error) {
	object, err := decodeObject(body)
	if err != nil {
		return diagnostics, err
	}
	var parts []Part
	restored := map[string]bool{}
	for index, raw := range arrayAt(object, "output") {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := fmt.Sprintf("output[%d]", index)
		if execution, ok := gatewayToolExecution(item, &diagnostics, path); ok {
			parts = append(parts, execution...)
			restored[path] = true
			continue
		}
		single, err := marshalJSON(map[string]any{"output": []any{item}})
		if err != nil {
			return diagnostics, err
		}
		decoded, _, err := DecodeResponse(OpenAIResponses, single)
		if err != nil {
			return diagnostics, err
		}
		parts = append(parts, decoded.Parts...)
	}
	if len(restored) == 0 {
		return diagnostics, nil
	}
	response.Parts = parts
	filtered := diagnostics[:0]
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "output_item_dropped" && restored[diagnostic.Path] {
			continue
		}
		filtered = append(filtered, diagnostic)
	}
	return filtered, nil
}
