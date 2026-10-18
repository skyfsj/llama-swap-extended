package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DecodeObject decodes a JSON object preserving number precision.
func DecodeObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("body is not a JSON object")
	}
	return object, nil
}

func ObjectAt(m map[string]any, key string) map[string]any {
	value, _ := m[key].(map[string]any)
	return value
}

func ArrayAt(m map[string]any, key string) []any {
	value, _ := m[key].([]any)
	return value
}

func StringAt(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func BoolAt(m map[string]any, key string) (bool, bool) {
	value, ok := m[key].(bool)
	return value, ok
}

func BoolValue(m map[string]any, key string) bool {
	value, _ := m[key].(bool)
	return value
}

func NumberAt(m map[string]any, key string) (float64, bool) {
	switch value := m[key].(type) {
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	}
	return 0, false
}

func IntAt(m map[string]any, key string) (int, bool) {
	value, ok := m[key].(json.Number)
	if ok {
		if parsed, err := value.Int64(); err == nil {
			return int(parsed), true
		}
		if parsed, err := value.Float64(); err == nil {
			return int(parsed), true
		}
	}
	if number, ok := m[key].(float64); ok {
		return int(number), true
	}
	return 0, false
}

func Int64At(m map[string]any, key string) (int64, bool) {
	if value, ok := m[key].(json.Number); ok {
		if parsed, err := value.Int64(); err == nil {
			return parsed, true
		}
		if parsed, err := value.Float64(); err == nil {
			return int64(parsed), true
		}
	}
	if number, ok := m[key].(float64); ok {
		return int64(number), true
	}
	return 0, false
}

// FloatPtr returns a pointer to the value when the key exists and is numeric.
func FloatPtr(m map[string]any, key string) *float64 {
	if value, ok := NumberAt(m, key); ok {
		return &value
	}
	return nil
}

func IntPtr(m map[string]any, key string) *int {
	if value, ok := IntAt(m, key); ok {
		return &value
	}
	return nil
}

func Int64Ptr(m map[string]any, key string) *int64 {
	if value, ok := Int64At(m, key); ok {
		return &value
	}
	return nil
}

// MarshalJSON encodes without HTML escaping so tool schemas and text survive
// byte-for-byte (and smaller).
func MarshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// RawObject converts any JSON-encodable value into RawMessage.
func RawObject(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	if raw, ok := value.(json.RawMessage); ok {
		return raw
	}
	if text, ok := value.(string); ok {
		trimmed := strings.TrimSpace(text)
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
		encoded, _ := MarshalJSON(text)
		return encoded
	}
	encoded, err := MarshalJSON(value)
	if err != nil {
		return nil
	}
	return encoded
}

// RawJSONOrEmpty returns {} for a nil/empty schema.
func RawJSONOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// DataURL builds a data: URL for inline media.
func DataURL(mediaType, base64Data string) string {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + base64Data
}

// ParseDataURL splits a data: URL into media type and base64 payload.
func ParseDataURL(url string) (mediaType, base64Data string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := url[len("data:"):comma]
	payload := url[comma+1:]
	mediaType = "application/octet-stream"
	base64Encoded := false
	for _, item := range strings.Split(meta, ";") {
		switch {
		case strings.EqualFold(item, "base64"):
			base64Encoded = true
		case strings.Contains(item, "/"):
			mediaType = item
		}
	}
	if !base64Encoded {
		return "", "", false
	}
	return mediaType, payload, true
}

// argumentsText renders tool-call arguments as the string form used by Chat
// and Responses.
func ArgumentsText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	return string(raw)
}

// StatusText is net/http's status text, behind a helper so callers do not need
// a direct net/http dependency in their signatures.
func StatusText(status int) string {
	return http.StatusText(status)
}
