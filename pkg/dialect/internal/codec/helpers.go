package codec

import (
	"encoding/json"

	"github.com/mostlygeek/llama-swap/pkg/dialect/internal/core"
	jsonx "github.com/mostlygeek/llama-swap/pkg/dialect/internal/jsonx"
)

// Thin aliases keep the codec reading without package qualifiers.

func decodeObject(body []byte) (map[string]any, error)     { return jsonx.DecodeObject(body) }
func objectAt(m map[string]any, key string) map[string]any { return jsonx.ObjectAt(m, key) }
func arrayAt(m map[string]any, key string) []any           { return jsonx.ArrayAt(m, key) }
func stringAt(m map[string]any, key string) string         { return jsonx.StringAt(m, key) }
func boolAt(m map[string]any, key string) (bool, bool)     { return jsonx.BoolAt(m, key) }
func boolValue(m map[string]any, key string) bool          { return jsonx.BoolValue(m, key) }
func intAt(m map[string]any, key string) (int, bool)       { return jsonx.IntAt(m, key) }
func int64At(m map[string]any, key string) (int64, bool)   { return jsonx.Int64At(m, key) }
func floatPtr(m map[string]any, key string) *float64       { return jsonx.FloatPtr(m, key) }
func intPtr(m map[string]any, key string) *int             { return jsonx.IntPtr(m, key) }
func int64Ptr(m map[string]any, key string) *int64         { return jsonx.Int64Ptr(m, key) }
func marshalJSON(value any) ([]byte, error)                { return jsonx.MarshalJSON(value) }
func rawObject(value any) json.RawMessage                  { return jsonx.RawObject(value) }
func rawJSONOrEmpty(raw json.RawMessage) json.RawMessage   { return jsonx.RawJSONOrEmpty(raw) }
func dataURL(mediaType, base64Data string) string          { return jsonx.DataURL(mediaType, base64Data) }
func parseDataURL(url string) (string, string, bool)       { return jsonx.ParseDataURL(url) }
func statusText(status int) string                         { return jsonx.StatusText(status) }

func conversionErrorf(code, format string, args ...any) error {
	return core.ConversionErrorf(code, format, args...)
}

func parseArguments(text string, diagnostics *[]Diagnostic, path string) json.RawMessage {
	return core.ParseArguments(text, diagnostics, path)
}

func argumentsText(raw json.RawMessage) string { return jsonx.ArgumentsText(raw) }

func stringList(m map[string]any, key string, diagnostics *[]Diagnostic) []string {
	return core.StringList(m, key, diagnostics)
}

func appendDiagnostic(target *[]Diagnostic, code, message, path string) {
	core.AppendDiagnostic(target, code, message, path)
}
