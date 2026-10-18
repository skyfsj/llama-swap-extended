package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StringList accepts a string or an array of strings. Non-string array entries
// are reported through diagnostics rather than panicking.
// StringList accepts a string or an array of strings. Non-string array entries
// are reported through diagnostics rather than panicking.
func StringList(m map[string]any, key string, diagnostics *[]Diagnostic) []string {
	switch value := m[key].(type) {
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))
		for index, item := range value {
			text, ok := item.(string)
			if !ok {
				AppendDiagnostic(diagnostics, "invalid_stop_entry",
					fmt.Sprintf("stop entry %d is not a string and was dropped", index),
					fmt.Sprintf("%s[%d]", key, index))
				continue
			}
			out = append(out, text)
		}
		return out
	}
	return nil
}

// parseArguments turns a tool-call arguments string into a JSON object. An
// invalid or empty string becomes {} so downstream dialects always receive an
// object (with a diagnostic when it was invalid).
// ParseArguments turns a tool-call arguments string into a JSON object, reporting
// an invalid payload through diagnostics instead of failing.
func ParseArguments(text string, diagnostics *[]Diagnostic, path string) json.RawMessage {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		AppendDiagnostic(diagnostics, "invalid_tool_arguments",
			"tool call arguments are not valid JSON; an empty object was used", path)
		return json.RawMessage(`{}`)
	}
	if !strings.HasPrefix(trimmed, "{") {
		// JSON scalars cannot represent named arguments.
		AppendDiagnostic(diagnostics, "tool_arguments_not_object",
			"tool call arguments are not a JSON object; an empty object was used", path)
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// AppendDiagnostic records one conversion note when the caller collects them.
func AppendDiagnostic(target *[]Diagnostic, code, message, path string) {
	if target == nil {
		return
	}
	*target = append(*target, Diagnostic{Code: code, Message: message, Path: path})
}
