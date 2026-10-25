package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func protocolStreamObjects(body []byte) []map[string]any {
	pending := bytes.NewBuffer(body)
	var objects []map[string]any
	for pending.Len() > 0 {
		event, ok := takeSSEEvent(pending)
		if !ok {
			event = append([]byte(nil), pending.Bytes()...)
			pending.Reset()
		}
		data, ok := sseData(event)
		if !ok || data == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) == nil && obj != nil {
			objects = append(objects, obj)
		}
	}
	return objects
}

func nativeStreamResponse(format dialect.Format, body []byte) ([]byte, error) {
	objects := protocolStreamObjects(body)
	if format == dialect.OpenAIChat {
		result := map[string]any{"object": "chat.completion", "status": "completed"}
		choices := map[int]map[string]any{}
		tools := map[int]map[int]map[string]any{}
		for _, obj := range objects {
			for key, value := range obj {
				if key != "choices" {
					result[key] = value
				}
			}
			for _, raw := range anySlice(obj["choices"]) {
				choice, _ := raw.(map[string]any)
				index := jsonInt(choice["index"])
				target := choices[index]
				if target == nil {
					target = map[string]any{"index": index, "message": map[string]any{"role": "assistant"}}
					choices[index] = target
				}
				for key, value := range choice {
					if key != "delta" && key != "message" {
						target[key] = value
					}
				}
				message := target["message"].(map[string]any)
				if complete, ok := choice["message"].(map[string]any); ok {
					target["message"] = complete
					message = complete
				}
				delta, _ := choice["delta"].(map[string]any)
				for key, value := range delta {
					switch key {
					case "content", "refusal", "reasoning_content", "reasoning":
						if text, ok := value.(string); ok {
							message[key] = jsonString(message[key]) + text
						} else if value != nil {
							result["_native_replay_unavailable"] = true
						}
					case "role":
						// MiMo repeats "role": null on every fragment after
						// the first. A null must not erase the role the
						// opening fragment carried: the replayed assistant
						// turn would reach the upstream without one and be
						// rejected ("messages[1] role is not supported").
						if value != nil {
							message[key] = value
						}
					case "tool_calls":
						if tools[index] == nil {
							tools[index] = map[int]map[string]any{}
						}
						for _, rawCall := range anySlice(value) {
							call, _ := rawCall.(map[string]any)
							callIndex := jsonInt(call["index"])
							acc := tools[index][callIndex]
							if acc == nil {
								acc = map[string]any{"type": "function", "function": map[string]any{}}
								tools[index][callIndex] = acc
							}
							for field, item := range call {
								if field == "index" {
									continue
								}
								if field == "function" {
									fn, _ := item.(map[string]any)
									dest := acc["function"].(map[string]any)
									for field, value := range fn {
										if field == "arguments" {
											dest[field] = jsonString(dest[field]) + jsonString(value)
										} else if value != nil {
											// Some upstreams repeat every field on each
											// tool-call fragment with null for the ones
											// that do not change (Xiaomi MiMo sends
											// "name": null): a null must not erase the
											// value the first fragment carried, or the
											// rebuilt call loses its identity and the
											// continuation can no longer match it.
											dest[field] = value
										}
									}
								} else if item != nil {
									acc[field] = item
								}
							}
						}
					case "audio":
						audio, _ := value.(map[string]any)
						saved, _ := message["audio"].(map[string]any)
						if saved == nil {
							saved = map[string]any{}
							message["audio"] = saved
						}
						for field, value := range audio {
							if field == "data" || field == "transcript" {
								saved[field] = jsonString(saved[field]) + jsonString(value)
							} else {
								saved[field] = value
							}
						}
					default:
						result["_native_replay_unavailable"] = true
					}
				}
			}
		}
		indices := []int{}
		for index := range choices {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		items := []any{}
		for _, index := range indices {
			choice := choices[index]
			message := choice["message"].(map[string]any)
			toolIndices := []int{}
			for callIndex := range tools[index] {
				toolIndices = append(toolIndices, callIndex)
			}
			sort.Ints(toolIndices)
			calls := []any{}
			for _, callIndex := range toolIndices {
				calls = append(calls, tools[index][callIndex])
			}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
			items = append(items, choice)
		}
		result["choices"] = items
		if result["_native_replay_unavailable"] == true {
			result["_raw_stream"] = objects
		}
		return json.Marshal(result)
	}
	if format == dialect.OpenAIResponses {
		return json.Marshal(responseFromSSE(body, ""))
	}
	if format == dialect.ClaudeMessages {
		result := map[string]any{}
		blocks := map[int]map[string]any{}
		arguments := map[int]*strings.Builder{}
		for _, obj := range objects {
			index := jsonInt(obj["index"])
			switch obj["type"] {
			case "message_start":
				if value, ok := obj["message"].(map[string]any); ok {
					for k, v := range value {
						result[k] = v
					}
				}
			case "content_block_start":
				if block, ok := obj["content_block"].(map[string]any); ok {
					blocks[index] = block
				}
			case "content_block_delta":
				block := blocks[index]
				delta, _ := obj["delta"].(map[string]any)
				if block == nil || delta == nil {
					continue
				}
				switch delta["type"] {
				case "text_delta":
					block["text"] = jsonString(block["text"]) + jsonString(delta["text"])
				case "thinking_delta":
					block["thinking"] = jsonString(block["thinking"]) + jsonString(delta["thinking"])
				case "signature_delta":
					block["signature"] = jsonString(block["signature"]) + jsonString(delta["signature"])
				case "compaction_delta":
					block["content"] = jsonString(block["content"]) + jsonString(delta["content"])
				case "citations_delta":
					citations, _ := block["citations"].([]any)
					block["citations"] = append(citations, delta["citation"])
				case "input_json_delta":
					if arguments[index] == nil {
						arguments[index] = &strings.Builder{}
					}
					arguments[index].WriteString(jsonString(delta["partial_json"]))
				default:
					result["_native_replay_unavailable"] = true
				}
			case "message_delta":
				if delta, ok := obj["delta"].(map[string]any); ok {
					for k, v := range delta {
						result[k] = v
					}
				}
				if usage, ok := obj["usage"].(map[string]any); ok {
					merged, _ := result["usage"].(map[string]any)
					if merged == nil {
						merged = map[string]any{}
					}
					for k, v := range usage {
						merged[k] = v
					}
					result["usage"] = merged
				}
			}
		}
		indices := []int{}
		for index := range blocks {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		content := []any{}
		for _, index := range indices {
			block := blocks[index]
			if b := arguments[index]; b != nil {
				var input any
				if json.Unmarshal([]byte(b.String()), &input) != nil {
					return nil, errors.New("invalid native tool arguments")
				}
				block["input"] = input
			}
			content = append(content, block)
		}
		result["content"] = content
		if result["_native_replay_unavailable"] == true {
			result["_raw_stream"] = objects
		}
		return json.Marshal(result)
	}
	if format == dialect.Gemini {
		result := map[string]any{}
		candidates := map[int]map[string]any{}
		for _, obj := range objects {
			for k, v := range obj {
				if k != "candidates" {
					result[k] = v
				}
			}
			items, _ := obj["candidates"].([]any)
			for _, raw := range items {
				candidate, _ := raw.(map[string]any)
				if candidate == nil {
					continue
				}
				index := jsonInt(candidate["index"])
				target := candidates[index]
				if target == nil {
					target = map[string]any{"index": index, "content": map[string]any{"role": "model", "parts": []any{}}}
					candidates[index] = target
				}
				for k, v := range candidate {
					if k != "content" {
						target[k] = v
					}
				}
				content, _ := candidate["content"].(map[string]any)
				if content != nil {
					existing := target["content"].(map[string]any)
					old, _ := existing["parts"].([]any)
					next, _ := content["parts"].([]any)
					existing["parts"] = append(old, next...)
				}
			}
		}
		indices := []int{}
		for index := range candidates {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		items := []any{}
		for _, index := range indices {
			items = append(items, candidates[index])
		}
		result["candidates"] = items
		return json.Marshal(result)
	}
	if format == dialect.GeminiInteractions {
		var result map[string]any
		steps := map[int]map[string]any{}
		args := map[int]*strings.Builder{}
		for _, obj := range objects {
			if value, ok := obj["interaction"].(map[string]any); ok {
				result = value
			}
			index := jsonInt(obj["index"])
			switch obj["event_type"] {
			case "step.start":
				if step, ok := obj["step"].(map[string]any); ok {
					steps[index] = step
				}
			case "step.delta":
				step := steps[index]
				delta, _ := obj["delta"].(map[string]any)
				if step == nil || delta == nil {
					continue
				}
				switch delta["type"] {
				case "arguments_delta":
					if args[index] == nil {
						args[index] = &strings.Builder{}
					}
					args[index].WriteString(jsonString(delta["arguments"]))
				case "thought_signature":
					step["signature"] = jsonString(step["signature"]) + jsonString(delta["signature"])
				case "text":
					content, _ := step["content"].([]any)
					step["content"] = append(content, map[string]any{"type": "text", "text": delta["text"]})
				case "thought_summary":
					summary, _ := step["summary"].([]any)
					summary = append(summary, delta["content"])
					step["summary"] = summary
				}
			}
		}
		if result == nil {
			return nil, errors.New("missing native interaction lifecycle")
		}
		if existing, ok := result["steps"].([]any); !ok || len(existing) == 0 {
			indices := []int{}
			for index := range steps {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			items := []any{}
			for _, index := range indices {
				step := steps[index]
				if b := args[index]; b != nil {
					var value any
					if json.Unmarshal([]byte(b.String()), &value) != nil {
						return nil, errors.New("invalid native interaction arguments")
					}
					step["arguments"] = value
				}
				items = append(items, step)
			}
			result["steps"] = items
		}
		return json.Marshal(result)
	}
	r, err := decodeProtocolResponse(format, body)
	if err != nil {
		return nil, err
	}
	encoded, _, err := dialect.EncodeResponse(r, format)
	return encoded, err
}

func jsonInt(value any) int       { number, _ := value.(float64); return int(number) }
func jsonString(value any) string { text, _ := value.(string); return text }
