package codec

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// referenceStripHostedSearchTools is the pre-optimization implementation of
// StripHostedSearchTools: it always decodes the whole body. The optimized
// version must return byte-identical output for every input.
func referenceStripHostedSearchTools(body []byte, format Format) (out []byte, changed bool, err error) {
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

// TestStripHostedSearchTools_MatchesReference pins the pre-scan against the
// always-decode implementation: for every body and format the optimized path
// must return the same bytes, the same changed flag and the same error.
func TestStripHostedSearchTools_MatchesReference(t *testing.T) {
	formats := []Format{OpenAIChat, OpenAIResponses, ClaudeMessages, Gemini}
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"m","web_search_options":{},"messages":[]}`,
		`{"model":"m","tools":[{"type":"web_search"}]}`,
		`{"model":"m","tools":[{"type":"web_search_20250305"}]}`,
		`{"model":"m","tools":[{"type":"function","function":{"name":"f"}},{"type":"web_search"}]}`,
		`{"model":"m","tools":[{"type":"web_search"}],"messages":[{"role":"user","content":"web_search mentioned in text"}]}`,
		`{"model":"m","tools":[{"googleSearch":{}}]}`,
		`{"model":"m","tools":[{"google_search":{}}]}`,
		`{"model":"m","tools":[{"googleSearchRetrieval":{}}]}`,
		`{"model":"m","tools":[{"google_search_retrieval":{}}]}`,
		`{"model":"m","googleSearch":{},"messages":[]}`,
		`{"model":"m","google_search":{}}`,
		`{"model":"m","tools":[{"googleSearch":{},"functionDeclarations":[{"name":"f"}]}]}`,
		// The key spelled with a \u escape: the pre-scan must not skip it.
		`{"model":"m","\u0077eb_search_options":{},"messages":[]}`,
		`{"model":"m","tools":[{"type":"\u0077eb_search"}]}`,
		// A key that only appears inside a string value.
		`{"model":"m","messages":[{"role":"user","content":"tell me about googleSearch"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"back\\slash and google_search"}]}`,
		// Malformed and non-object bodies: both paths must fail identically.
		`not json`,
		`[]`,
		`"a string"`,
		``,
		`{"model":"m","tools":"not an array"}`,
		`{"model":"m","tools":[{"type":123}]}`,
		`{"model":"m","tools":[null]}`,
	}
	for _, format := range formats {
		for _, body := range bodies {
			t.Run(string(format)+" "+body, func(t *testing.T) {
				wantOut, wantChanged, wantErr := referenceStripHostedSearchTools([]byte(body), format)
				gotOut, gotChanged, gotErr := StripHostedSearchTools([]byte(body), format)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("error mismatch: reference=%v optimized=%v", wantErr, gotErr)
				}
				if wantErr != nil {
					if wantErr.Error() != gotErr.Error() {
						t.Fatalf("error text mismatch:\nreference=%v\noptimized=%v", wantErr, gotErr)
					}
					return
				}
				if wantChanged != gotChanged {
					t.Fatalf("changed mismatch: reference=%v optimized=%v", wantChanged, gotChanged)
				}
				if !bytes.Equal(wantOut, gotOut) {
					t.Fatalf("output mismatch\nreference=%s\noptimized=%s", wantOut, gotOut)
				}
			})
		}
	}
}

// TestBodyMayCarryHostedSearchTools_NeverMissesARewrite is the safety property
// of the pre-scan: whenever the reference implementation rewrites a body, the
// pre-scan must have said "maybe". It is checked over generated combinations of
// keys so a literal missing from the table is caught.
func TestBodyMayCarryHostedSearchTools_NeverMissesARewrite(t *testing.T) {
	formats := []Format{OpenAIChat, OpenAIResponses, ClaudeMessages, Gemini}
	keyNames := []string{
		"web_search_options", "web_search", "web_search_20250305",
		"googleSearch", "google_search", "googleSearchRetrieval", "google_search_retrieval",
		"function", "name", "messages", "model", "tools",
		// The same keys spelled with a \u escape for their first letter, which
		// is what the pre-scan has to route back to the decoder.
		"\u0077eb_search_options", "\u0077eb_search",
		"\u0067oogleSearch", "\u0067oogle_search",
	}
	for _, format := range formats {
		for _, key := range keyNames {
			for _, value := range []string{`{}`, `"x"`, `[{"type":"web_search"}]`, `[{"googleSearch":{}}]`, `[{"type":"function","function":{"name":"f"}}]`} {
				body := `{"model":"m","` + key + `":` + value + `}`
				_, changed, err := referenceStripHostedSearchTools([]byte(body), format)
				if err != nil {
					continue
				}
				if !changed {
					continue
				}
				if !bodyMayCarryHostedSearchTools([]byte(body), format) {
					t.Fatalf("pre-scan skipped a body %s rewrites for %s: %s", format, body, body)
				}
			}
		}
	}
}

// TestBodyMayCarryHostedSearchTools_SkipsTheCommonCase guards the performance
// win itself: an ordinary request must not be routed into the decoder.
func TestBodyMayCarryHostedSearchTools_SkipsTheCommonCase(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"write a haiku about rain"}],"temperature":0.7}`)
	if bodyMayCarryHostedSearchTools(body, OpenAIChat) {
		t.Fatal("a plain chat request must not look like it carries a hosted search tool")
	}
	out, changed, err := StripHostedSearchTools(body, OpenAIChat)
	if err != nil || changed {
		t.Fatalf("plain request: changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out, body) {
		t.Fatal("a plain request must be returned unchanged")
	}
	if strings.Contains(string(out), "\\u") {
		t.Fatal("unexpected escaping in the passthrough body")
	}
}

// TestValidateObjectBody_MatchesDecodeObjectErrors pins the rejection texts of
// the pre-scan path against the decoder it stands in for: a body that is not a
// JSON object must fail with the same message on both paths, because callers
// surface that message to the client.
func TestValidateObjectBody_MatchesDecodeObjectErrors(t *testing.T) {
	bodies := []string{
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`[]`,
		`"a string"`,
		`true`,
		`false`,
		`123`,
		`1.5`,
		`null`,
		``,
		`not json`,
		`{"unterminated":`,
		`  {"model":"m"}  `,
		"{\n}",
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			_, wantErr := decodeObject([]byte(body))
			gotErr := validateObjectBody([]byte(body))
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("presence mismatch: decoder=%v prescan=%v", wantErr, gotErr)
			}
			if wantErr == nil {
				return
			}
			if wantErr.Error() != gotErr.Error() {
				t.Fatalf("text mismatch\ndecoder=%v\nprescan=%v", wantErr, gotErr)
			}
		})
	}
}
