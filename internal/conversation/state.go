package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type Scope struct {
	Provider   string         `json:"provider"`
	Adapter    string         `json:"adapter"`
	Model      string         `json:"model"`
	Credential string         `json:"credential,omitempty"`
	Format     dialect.Format `json:"format"`
}

func (s Scope) Key() string {
	sum := sha256.Sum256([]byte(s.Provider + "\x00" + s.Adapter + "\x00" + s.Model + "\x00" + s.Credential + "\x00" + string(s.Format)))
	return hex.EncodeToString(sum[:])
}

type Native struct {
	Scope    Scope           `json:"scope"`
	Version  int64           `json:"version"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response"`
}
type State struct {
	Messages      []dialect.Message `json:"messages"`
	Native        map[string]Native `json:"native,omitempty"`
	Summary       string            `json:"summary,omitempty"`
	Parent        string            `json:"parent,omitempty"`
	ParentVersion int64             `json:"parent_version,omitempty"`
}

func Decode(data []byte) (State, error) {
	var s State
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s); err != nil {
			return s, err
		}
	}
	if s.Native == nil {
		s.Native = map[string]Native{}
	}
	return s, nil
}
func (s State) Encode() ([]byte, error) { return json.Marshal(s) }

func (s State) Prepare(scope Scope, version int64, current *dialect.Request, actual []byte) ([]byte, []dialect.Diagnostic, error) {
	return s.prepare(scope, version, current, actual, true)
}

func (s State) Replay(scope Scope, version int64, current *dialect.Request, actual []byte) ([]byte, []dialect.Diagnostic, error) {
	return s.prepare(scope, version, current, actual, false)
}

func (s State) prepare(scope Scope, version int64, current *dialect.Request, actual []byte, resume bool) ([]byte, []dialect.Diagnostic, error) {
	if current == nil {
		return nil, nil, errors.New("missing conversation request")
	}
	copy := *current
	copy.Messages = append([]dialect.Message(nil), s.Messages...)
	copy.System = nil
	if s.Summary != "" {
		copy.System = []dialect.Part{{Kind: dialect.PartText, Text: "Conversation summary:\n" + s.Summary}}
	}
	encoded, notes, err := dialect.EncodeRequest(&copy, scope.Format, dialect.ConvertOptions{Model: scope.Model})
	if err != nil {
		return nil, notes, err
	}
	var target, portable map[string]json.RawMessage
	if json.Unmarshal(actual, &target) != nil || json.Unmarshal(encoded, &portable) != nil {
		return nil, notes, errors.New("conversation request is not a JSON object")
	}
	key := inputKey(scope.Format)
	fresh := target[key]
	native, ok := s.Native[scope.Key()]
	if ok && native.Version == version && len(native.Response) > 0 {
		if err := resumeNative(target, fresh, scope, native, resume); err != nil {
			return nil, notes, err
		}
	} else {
		prefix, err := nativeInput(scope.Format, portable[key])
		if err != nil {
			return nil, notes, err
		}
		next, err := nativeInput(scope.Format, fresh)
		if err != nil {
			return nil, notes, err
		}
		target[key], err = json.Marshal(append(prefix, next...))
		if err != nil {
			return nil, notes, err
		}
		delete(target, "previous_response_id")
		delete(target, "previous_interaction_id")
		if s.Summary != "" {
			mergeSummary(target, portable, scope.Format)
		}
	}
	body, err := json.Marshal(target)
	return body, notes, err
}

func mergeSummary(target, summary map[string]json.RawMessage, format dialect.Format) {
	switch format {
	case dialect.OpenAIResponses, dialect.GeminiInteractions:
		key := "instructions"
		if format == dialect.GeminiInteractions {
			key = "system_instruction"
		}
		var a, b string
		_ = json.Unmarshal(summary[key], &a)
		_ = json.Unmarshal(target[key], &b)
		target[key], _ = json.Marshal(a + "\n\n" + b)
	case dialect.ClaudeMessages:
		var prefix string
		_ = json.Unmarshal(summary["system"], &prefix)
		var blocks []any
		if json.Unmarshal(summary["system"], &blocks) != nil {
			blocks = []any{map[string]any{"type": "text", "text": prefix}}
		}
		var original []any
		if json.Unmarshal(target["system"], &original) != nil {
			var text string
			_ = json.Unmarshal(target["system"], &text)
			if text != "" {
				original = []any{map[string]any{"type": "text", "text": text}}
			}
		}
		target["system"], _ = json.Marshal(append(blocks, original...))
	case dialect.Gemini:
		var a, b map[string]json.RawMessage
		_ = json.Unmarshal(summary["systemInstruction"], &a)
		_ = json.Unmarshal(target["systemInstruction"], &b)
		var left, right []json.RawMessage
		_ = json.Unmarshal(a["parts"], &left)
		_ = json.Unmarshal(b["parts"], &right)
		target["systemInstruction"], _ = json.Marshal(map[string]any{"parts": append(left, right...)})
	}
}

func inputKey(format dialect.Format) string {
	switch format {
	case dialect.OpenAIResponses, dialect.GeminiInteractions:
		return "input"
	case dialect.Gemini:
		return "contents"
	default:
		return "messages"
	}
}

func resumeNative(target map[string]json.RawMessage, fresh json.RawMessage, scope Scope, native Native, resume bool) error {
	var previous, response map[string]json.RawMessage
	if json.Unmarshal(native.Request, &previous) != nil || json.Unmarshal(native.Response, &response) != nil {
		return errors.New("invalid native conversation state")
	}
	if string(response["_native_replay_unavailable"]) == "true" {
		return errors.New("the native stream contains an unknown state delta and cannot be replayed safely")
	}
	key := inputKey(scope.Format)
	stored := string(previous["store"]) != "false"
	var id string
	_ = json.Unmarshal(response["id"], &id)
	if resume && stored && id != "" && (scope.Format == dialect.OpenAIResponses || scope.Format == dialect.GeminiInteractions) {
		field := "previous_response_id"
		if scope.Format == dialect.GeminiInteractions {
			field = "previous_interaction_id"
		}
		if len(target["conversation"]) == 0 || string(target["conversation"]) == "null" {
			target[field], _ = json.Marshal(id)
			target[key] = fresh
		}
		return nil
	}
	var history, next, output []json.RawMessage
	history, err := nativeInput(scope.Format, previous[key])
	if err != nil {
		return fmt.Errorf("invalid native input history: %w", err)
	}
	next, err = nativeInput(scope.Format, fresh)
	if err != nil {
		return err
	}
	if scope.Format == dialect.OpenAIChat {
		for len(history) > 0 {
			var message map[string]any
			if json.Unmarshal(history[0], &message) != nil || message["role"] != "system" && message["role"] != "developer" {
				break
			}
			history = history[1:]
		}
		var prefix []json.RawMessage
		for len(next) > 0 {
			var message map[string]any
			if json.Unmarshal(next[0], &message) != nil || message["role"] != "system" && message["role"] != "developer" {
				break
			}
			prefix = append(prefix, next[0])
			next = next[1:]
		}
		history = append(prefix, history...)
	}
	switch scope.Format {
	case dialect.OpenAIResponses:
		_ = json.Unmarshal(response["output"], &output)
	case dialect.GeminiInteractions:
		_ = json.Unmarshal(response["steps"], &output)
	case dialect.ClaudeMessages:
		message, _ := json.Marshal(map[string]any{"role": "assistant", "content": response["content"]})
		output = []json.RawMessage{message}
	case dialect.Gemini:
		var candidates []map[string]json.RawMessage
		_ = json.Unmarshal(response["candidates"], &candidates)
		if len(candidates) > 0 {
			output = []json.RawMessage{candidates[0]["content"]}
		}
	case dialect.OpenAIChat:
		var choices []map[string]json.RawMessage
		_ = json.Unmarshal(response["choices"], &choices)
		if len(choices) > 0 {
			output = []json.RawMessage{choices[0]["message"]}
		}
		if len(output) > 0 {
			var message map[string]json.RawMessage
			if json.Unmarshal(output[0], &message) == nil {
				var audio map[string]json.RawMessage
				if json.Unmarshal(message["audio"], &audio) == nil && len(audio["id"]) > 0 {
					message["audio"], _ = json.Marshal(map[string]json.RawMessage{"id": audio["id"]})
					output[0], _ = json.Marshal(message)
				}
			}
		}
	}
	delete(target, "previous_response_id")
	delete(target, "previous_interaction_id")
	target[key], err = json.Marshal(append(append(history, output...), next...))
	return err
}

func NormalizeInput(format dialect.Format, raw json.RawMessage) ([]json.RawMessage, error) {
	return nativeInput(format, raw)
}
func nativeInput(format dialect.Format, raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return items, nil
	}
	if json.Unmarshal(raw, &items) == nil {
		if format == dialect.GeminiInteractions && len(items) > 0 {
			var first map[string]any
			if json.Unmarshal(items[0], &first) == nil {
				switch first["type"] {
				case "text", "image", "audio", "video", "document", "file":
					wrapped, err := json.Marshal(map[string]any{"type": "user_input", "content": items})
					return []json.RawMessage{wrapped}, err
				}
			}
		}
		return items, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		var item any = map[string]any{"role": "user", "content": text}
		if format == dialect.GeminiInteractions {
			item = map[string]any{"type": "user_input", "content": []any{map[string]any{"type": "text", "text": text}}}
		}
		b, err := json.Marshal(item)
		return []json.RawMessage{b}, err
	}
	if format == dialect.GeminiInteractions {
		var value map[string]any
		if json.Unmarshal(raw, &value) == nil {
			b, err := json.Marshal(map[string]any{"type": "user_input", "content": []any{value}})
			return []json.RawMessage{b}, err
		}
	}
	return nil, errors.New("native conversation input must be an array")
}

func portableMessages(messages []dialect.Message) []dialect.Message {
	var result []dialect.Message
	for _, message := range messages {
		copy := message
		copy.Parts = portableParts(message.Parts)
		if len(copy.Parts) > 0 {
			result = append(result, copy)
		}
	}
	return result
}
func portableParts(parts []dialect.Part) []dialect.Part {
	var result []dialect.Part
	for _, part := range parts {
		if part.Redacted {
			continue
		}
		part.Signature = ""
		part.ThoughtSignature = ""
		part.Data = ""
		part.Content = portableParts(part.Content)
		if part.Kind == dialect.PartReasoning {
			if part.Text == "" {
				continue
			}
			part.Kind = dialect.PartText
			part.Text = "Reasoning summary:\n" + part.Text
		}
		result = append(result, part)
	}
	return result
}

func (s *State) Append(current *dialect.Request, response *dialect.Response) {
	s.Messages = append(s.Messages, portableMessages(current.Messages)...)
	if response != nil && len(response.Parts) > 0 {
		s.Messages = append(s.Messages, portableMessages([]dialect.Message{{Role: dialect.RoleAssistant, Parts: response.Parts}})...)
	}
}
func (s *State) Remember(scope Scope, version int64, request, response []byte) {
	if s.Native == nil {
		s.Native = map[string]Native{}
	}
	var req, resp map[string]json.RawMessage
	if json.Unmarshal(request, &req) != nil || json.Unmarshal(response, &resp) != nil {
		return
	}
	input := map[string]json.RawMessage{inputKey(scope.Format): req[inputKey(scope.Format)]}
	if raw, ok := req["store"]; ok {
		input["store"] = raw
	}
	output := map[string]json.RawMessage{}
	for _, key := range []string{"id", "model", "modelVersion", "status", "output", "content", "choices", "candidates", "steps", "usage", "usageMetadata", "created", "created_at", "error", "errors", "stop_reason", "stop_sequence", "_native_replay_unavailable", "_raw_stream"} {
		if raw, ok := resp[key]; ok {
			output[key] = raw
		}
	}
	request, _ = json.Marshal(input)
	response, _ = json.Marshal(output)
	s.Native[scope.Key()] = Native{Scope: scope, Version: version, Request: request, Response: response}
}
