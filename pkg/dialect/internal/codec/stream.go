package codec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StreamConverter converts one upstream SSE stream into the client dialect.
// Feed receives one decoded SSE data payload at a time; Finish flushes the
// terminal frames. It is not safe for concurrent use.
type StreamConverter struct {
	source  Format
	target  Format
	options StreamOptions
	encoder streamEncoder

	decoder func(data []byte) ([]Event, error)

	// accumulated canonical state, used by encoders that must render a whole
	// response object at the end (Responses) and for diagnostics.
	response    Response
	diagnostics []Diagnostic
	finished    bool
	started     bool
	usage       Usage
	// toolCounter numbers Gemini function calls, which carry no id/index.
	toolCounter int
	// toolIndices tracks Chat tool calls that have not been closed yet.
	toolIndices map[int]struct{}
}

// NewStreamConverter builds a converter for one stream.
func NewStreamConverter(source, target Format, options StreamOptions) (*StreamConverter, error) {
	if source == target {
		return nil, conversionErrorf("same_dialect", "stream conversion requires two different dialects")
	}
	converter := &StreamConverter{
		source: source, target: target, options: options,
		response: Response{ID: options.ID, Model: options.Model, Created: options.Created},
	}
	if converter.response.Created == 0 {
		converter.response.Created = time.Now().Unix()
	}
	switch source {
	case OpenAIChat:
		converter.decoder = converter.decodeChatEvent
	case OpenAIResponses:
		converter.decoder = converter.decodeResponsesEvent
	case ClaudeMessages:
		converter.decoder = converter.decodeClaudeEvent
	case Gemini:
		converter.decoder = converter.decodeGeminiEvent
	case GeminiInteractions:
		converter.decoder = converter.decodeInteractionsEvent
	default:
		return nil, conversionErrorf("unsupported_format", "unsupported stream source %q", source)
	}
	switch target {
	case OpenAIChat:
		converter.encoder = &chatStreamEncoder{options: options}
	case OpenAIResponses:
		converter.encoder = &responsesStreamEncoder{options: options}
	case ClaudeMessages:
		converter.encoder = &claudeStreamEncoder{
			options: options, estimateInput: options.EstimateInputTokens,
		}
	case Gemini:
		converter.encoder = &geminiStreamEncoder{}
	case GeminiInteractions:
		converter.encoder = &interactionsStreamEncoder{options: options}
	default:
		return nil, conversionErrorf("unsupported_format", "unsupported stream target %q", target)
	}
	return converter, nil
}

// Feed converts one upstream SSE data payload into zero or more client frames.
func (c *StreamConverter) Feed(data []byte) ([]Frame, error) {
	if c.finished {
		return nil, conversionErrorf("stream_finished", "stream already finished")
	}
	events, err := c.decoder(data)
	if err != nil {
		return nil, err
	}
	var frames []Frame
	for _, event := range events {
		if err := c.apply(event); err != nil {
			return frames, err
		}
		emitted, err := c.encoder.Encode(event)
		if err != nil {
			return frames, err
		}
		frames = append(frames, emitted...)
	}
	return frames, nil
}

// Finish flushes the terminal frames for the client.
func (c *StreamConverter) Finish() ([]Frame, error) {
	if c.finished {
		return nil, nil
	}
	c.finished = true
	if !c.started {
		if err := c.apply(Event{Kind: EventStart, ID: c.options.ID, Model: c.options.Model, Created: c.options.Created}); err != nil {
			return nil, err
		}
		if frames, err := c.encoder.Encode(Event{Kind: EventStart, ID: c.options.ID, Model: c.options.Model, Created: c.options.Created}); err != nil {
			return nil, err
		} else if len(frames) > 0 {
			// Keep the frames emitted below in order: start first.
			pending := frames
			frames, err = c.encoder.Finish()
			if err != nil {
				return pending, err
			}
			return append(pending, frames...), nil
		}
	}
	return c.encoder.Finish()
}

// Diagnostics reports conversion notes gathered so far.
func (c *StreamConverter) Diagnostics() []Diagnostic {
	return append(append([]Diagnostic(nil), c.diagnostics...), c.encoder.Diagnostics()...)
}

func (c *StreamConverter) diagnose(code, message, path string) {
	appendDiagnostic(&c.diagnostics, code, message, path)
}

// apply records an event into the accumulated response.
func (c *StreamConverter) apply(event Event) error {
	switch event.Kind {
	case EventStart:
		c.started = true
		if event.ID != "" {
			c.response.ID = event.ID
		}
		if event.Model != "" {
			c.response.Model = event.Model
		}
		if event.Created != 0 {
			c.response.Created = event.Created
		}
		if event.Usage != nil {
			c.usage.Add(*event.Usage)
			c.response.Usage.Add(*event.Usage)
		}
	case EventTextDelta:
		c.appendPart(Part{Kind: PartText, Text: event.Text})
	case EventReasoningDelta:
		c.appendPart(Part{Kind: PartReasoning, Text: event.Text})
	case EventRefusalDelta:
		c.appendPart(Part{Kind: PartRefusal, Text: event.Text})
	case EventToolCallStart:
		c.response.Parts = append(c.response.Parts, Part{
			Kind: PartToolCall, ToolCallID: event.ToolCallID, ToolName: event.ToolName,
			ThoughtSignature: event.ThoughtSignature,
		})
	case EventToolCallDelta:
		for index := len(c.response.Parts) - 1; index >= 0; index-- {
			if c.response.Parts[index].Kind == PartToolCall {
				c.response.Parts[index].ArgumentsText += event.ArgumentsDelta
				break
			}
		}
	case EventUsage:
		if event.Usage != nil {
			c.usage.Add(*event.Usage)
			c.response.Usage.Add(*event.Usage)
		}
	case EventFinish:
		c.response.FinishReason = event.FinishReason
		c.response.StopSequence = event.StopSequence
	case EventError:
		c.response.Status = "failed"
		c.response.Error = event.Error
		c.response.FinishReason = FinishOther
	}
	return nil
}

func (c *StreamConverter) appendPart(part Part) {
	if len(c.response.Parts) > 0 {
		last := &c.response.Parts[len(c.response.Parts)-1]
		if last.Kind == part.Kind {
			last.Text += part.Text
			if part.ThoughtSignature != "" {
				last.ThoughtSignature = part.ThoughtSignature
			}
			return
		}
	}
	c.response.Parts = append(c.response.Parts, part)
}

// ---------------------------------------------------------------------------
// Source decoders

func (c *StreamConverter) decodeChatEvent(data []byte) ([]Event, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	events := []Event{{
		Kind: EventStart, ID: stringAt(object, "id"), Model: stringAt(object, "model"),
		Created: intValue64(object, "created"),
	}}
	for _, raw := range arrayAt(object, "choices") {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if index, ok := intAt(choice, "index"); ok && index > 0 {
			c.diagnose("extra_choice_dropped",
				"the target dialect cannot represent a second stream choice", "choices")
			continue
		}
		delta := objectAt(choice, "delta")
		if delta == nil {
			delta = map[string]any{}
		}
		if reasoning := firstString(delta, "reasoning_content", "reasoning"); reasoning != "" {
			events = append(events, Event{Kind: EventReasoningDelta, Text: reasoning})
		}
		if content := stringAt(delta, "content"); content != "" {
			events = append(events, Event{Kind: EventTextDelta, Text: content})
		}
		if refusal := stringAt(delta, "refusal"); refusal != "" {
			events = append(events, Event{Kind: EventRefusalDelta, Text: refusal})
		}
		for _, rawCall := range arrayAt(delta, "tool_calls") {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			index, _ := intAt(call, "index")
			function := objectAt(call, "function")
			name := stringAt(function, "name")
			arguments := stringAt(function, "arguments")
			if c.toolIndices == nil {
				c.toolIndices = map[int]struct{}{}
			}
			_, alreadyOpen := c.toolIndices[index]
			if !alreadyOpen && (name != "" || stringAt(call, "id") != "") {
				events = append(events, Event{
					Kind: EventToolCallStart, Index: index,
					ToolCallID: stringAt(call, "id"), ToolName: name,
				})
			}
			c.toolIndices[index] = struct{}{}
			if arguments != "" {
				events = append(events, Event{Kind: EventToolCallDelta, Index: index, ArgumentsDelta: arguments})
			}
		}
		if finish := stringAt(choice, "finish_reason"); finish != "" {
			events = append(events, Event{Kind: EventFinish, FinishReason: finishFromChat(finish)})
		}
	}
	if usage := objectAt(object, "usage"); usage != nil {
		u := usageFromChat(usage)
		events = append(events, Event{Kind: EventUsage, Usage: &u})
	}
	// A finish frame closes every tool call that is still open, so a target
	// encoder never leaves a block unterminated.
	for _, event := range events {
		if event.Kind == EventFinish {
			for index := range c.toolIndices {
				events = append(events, Event{Kind: EventToolCallEnd, Index: index})
			}
			c.toolIndices = nil
			break
		}
	}
	return events, nil
}

func (c *StreamConverter) decodeResponsesEvent(data []byte) ([]Event, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	typeName := stringAt(object, "type")
	switch typeName {
	case "response.created", "response.in_progress":
		response := objectAt(object, "response")
		if response == nil {
			return nil, nil
		}
		return []Event{{
			Kind: EventStart, ID: stringAt(response, "id"), Model: stringAt(response, "model"),
			Created: intValue64(response, "created_at"),
		}}, nil
	case "response.output_text.delta":
		return []Event{{Kind: EventTextDelta, Text: stringAt(object, "delta")}}, nil
	case "response.refusal.delta":
		return []Event{{Kind: EventRefusalDelta, Text: stringAt(object, "delta")}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []Event{{Kind: EventReasoningDelta, Text: stringAt(object, "delta")}}, nil
	case "response.output_item.added":
		item := objectAt(object, "item")
		outputIndex, _ := intAt(object, "output_index")
		if item == nil {
			return nil, nil
		}
		switch stringAt(item, "type") {
		case "function_call":
			return []Event{{
				Kind: EventToolCallStart, Index: outputIndex,
				ToolCallID: stringAt(item, "call_id"), ToolName: stringAt(item, "name"),
			}}, nil
		case "reasoning":
			// Reasoning content arrives through its own delta events.
			return nil, nil
		}
		return nil, nil
	case "response.function_call_arguments.delta":
		outputIndex, _ := intAt(object, "output_index")
		return []Event{{Kind: EventToolCallDelta, Index: outputIndex, ArgumentsDelta: stringAt(object, "delta")}}, nil
	case "response.function_call_arguments.done":
		outputIndex, _ := intAt(object, "output_index")
		return []Event{{Kind: EventToolCallEnd, Index: outputIndex}}, nil
	case "response.output_item.done":
		item := objectAt(object, "item")
		if item != nil && stringAt(item, "type") == "function_call" {
			outputIndex, _ := intAt(object, "output_index")
			return []Event{{Kind: EventToolCallEnd, Index: outputIndex}}, nil
		}
		return nil, nil
	case "response.completed", "response.incomplete", "response.failed":
		response := objectAt(object, "response")
		if response == nil {
			return nil, nil
		}
		var events []Event
		if usage := objectAt(response, "usage"); usage != nil {
			u := usageFromResponses(usage)
			events = append(events, Event{Kind: EventUsage, Usage: &u})
		}
		switch typeName {
		case "response.failed":
			errorObject := objectAt(response, "error")
			if errorObject == nil {
				errorObject = map[string]any{}
			}
			events = append(events, Event{Kind: EventError, Error: &StreamError{
				Type: stringAt(errorObject, "type"), Code: stringAt(errorObject, "code"),
				Message: stringAt(errorObject, "message"),
			}})
		default:
			incomplete := objectAt(response, "incomplete_details")
			reason := ""
			if incomplete != nil {
				reason = stringAt(incomplete, "reason")
			}
			hasToolCall := false
			for _, part := range c.response.Parts {
				if part.Kind == PartToolCall {
					hasToolCall = true
					break
				}
			}
			events = append(events, Event{
				Kind:         EventFinish,
				FinishReason: finishFromResponsesStatus(stringAt(response, "status"), reason, hasToolCall),
			})
		}
		return events, nil
	case "error":
		return []Event{{Kind: EventError, Error: &StreamError{
			Type: stringAt(object, "type"), Code: stringAt(object, "code"), Message: stringAt(object, "message"),
		}}}, nil
	case "response.output_text.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.annotation.added", "response.reasoning_summary_text.done",
		"response.reasoning_text.done", "response.reasoning_summary_part.added", "response.refusal.done",
		"response.queued":
		return nil, nil
	}
	return nil, nil
}

func (c *StreamConverter) decodeClaudeEvent(data []byte) ([]Event, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	switch typeName := stringAt(object, "type"); typeName {
	case "message_start":
		message := objectAt(object, "message")
		if message == nil {
			return nil, nil
		}
		var usage Usage
		if rawUsage := objectAt(message, "usage"); rawUsage != nil {
			usage = usageFromClaude(rawUsage)
		}
		return []Event{{
			Kind: EventStart, ID: stringAt(message, "id"), Model: stringAt(message, "model"),
			Usage: &usage,
		}}, nil
	case "content_block_start":
		index, _ := intAt(object, "index")
		block := objectAt(object, "content_block")
		if block == nil {
			return nil, nil
		}
		if stringAt(block, "type") == "tool_use" {
			return []Event{{
				Kind: EventToolCallStart, Index: index,
				ToolCallID: stringAt(block, "id"), ToolName: stringAt(block, "name"),
			}}, nil
		}
		return nil, nil
	case "content_block_delta":
		index, _ := intAt(object, "index")
		delta := objectAt(object, "delta")
		if delta == nil {
			return nil, nil
		}
		switch stringAt(delta, "type") {
		case "text_delta":
			return []Event{{Kind: EventTextDelta, Text: stringAt(delta, "text")}}, nil
		case "thinking_delta":
			return []Event{{Kind: EventReasoningDelta, Text: stringAt(delta, "thinking")}}, nil
		case "input_json_delta":
			return []Event{{Kind: EventToolCallDelta, Index: index, ArgumentsDelta: stringAt(delta, "partial_json")}}, nil
		case "signature_delta":
			c.diagnose("thinking_signature_dropped",
				"Anthropic thinking signatures do not survive cross-dialect streams", "signature_delta")
			return nil, nil
		case "citations_delta":
			c.diagnose("citation_dropped", "Anthropic citations do not survive cross-dialect streams", "citations_delta")
			return nil, nil
		}
		return nil, nil
	case "content_block_stop":
		index, _ := intAt(object, "index")
		return []Event{{Kind: EventToolCallEnd, Index: index}}, nil
	case "message_delta":
		delta := objectAt(object, "delta")
		events := []Event{}
		if rawUsage := objectAt(object, "usage"); rawUsage != nil {
			usage := usageFromClaude(rawUsage)
			events = append(events, Event{Kind: EventUsage, Usage: &usage})
		}
		if delta != nil {
			if reason := stringAt(delta, "stop_reason"); reason != "" {
				events = append(events, Event{
					Kind: EventFinish, FinishReason: finishFromClaude(reason), StopSequence: stringAt(delta, "stop_sequence"),
				})
			}
		}
		return events, nil
	case "message_stop":
		return nil, nil
	case "ping":
		return []Event{{Kind: EventPing}}, nil
	case "error":
		errorObject := objectAt(object, "error")
		if errorObject == nil {
			errorObject = map[string]any{}
		}
		return []Event{{Kind: EventError, Error: &StreamError{
			Type: stringAt(errorObject, "type"), Message: stringAt(errorObject, "message"),
		}}}, nil
	}
	return nil, nil
}

func (c *StreamConverter) decodeGeminiEvent(data []byte) ([]Event, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if errorObject := objectAt(object, "error"); errorObject != nil {
		streamError := &StreamError{
			Status:  stringAt(errorObject, "status"),
			Message: stringAt(errorObject, "message"),
		}
		if code := formatErrorCode(errorObject["code"]); code != "" {
			streamError.Code = code
		}
		return []Event{{Kind: EventError, Error: streamError}}, nil
	}
	if feedback := objectAt(object, "promptFeedback"); feedback != nil {
		if blocked := stringAt(feedback, "blockReason"); blocked != "" {
			return []Event{{Kind: EventFinish, FinishReason: FinishContentFilter}}, nil
		}
	}
	events := []Event{{Kind: EventStart, Model: stringAt(object, "modelVersion")}}
	candidates := arrayAt(object, "candidates")
	if len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			content := objectAt(candidate, "content")
			for _, rawPart := range arrayAt(content, "parts") {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				inline := objectAt(part, "inlineData")
				if inline == nil {
					inline = objectAt(part, "inline_data")
				}
				if inline != nil {
					c.diagnose("stream_media_dropped",
						"inline media inside a stream cannot be represented as a delta", "inlineData")
					continue
				}
				fileData := objectAt(part, "fileData")
				if fileData == nil {
					fileData = objectAt(part, "file_data")
				}
				if fileData != nil {
					c.diagnose("stream_media_dropped",
						"file media inside a stream cannot be represented as a delta", "fileData")
					continue
				}
				if call := objectAt(part, "functionCall"); call != nil {
					name := stringAt(call, "name")
					args := string(mustMarshal(call["args"]))
					index := c.toolCounter
					c.toolCounter++
					events = append(events,
						Event{Kind: EventToolCallStart, Index: index, ToolCallID: fmt.Sprintf("call_gemini_%d", index+1), ToolName: name,
							ThoughtSignature: geminiThoughtSignature(part)},
						Event{Kind: EventToolCallDelta, Index: index, ArgumentsDelta: args},
						Event{Kind: EventToolCallEnd, Index: index},
					)
					continue
				}
				text := stringAt(part, "text")
				if text == "" {
					continue
				}
				if thought, ok := boolAt(part, "thought"); ok && thought {
					events = append(events, Event{
						Kind: EventReasoningDelta, Text: text, ThoughtSignature: geminiThoughtSignature(part),
					})
				} else {
					events = append(events, Event{Kind: EventTextDelta, Text: text})
				}
			}
			if reason := stringAt(candidate, "finishReason"); reason != "" {
				// The current frame's functionCall events have not been applied
				// to c.response yet, so include them in the detection.
				hasToolCall := false
				for _, event := range events {
					if event.Kind == EventToolCallStart {
						hasToolCall = true
						break
					}
				}
				if !hasToolCall {
					for _, part := range c.response.Parts {
						if part.Kind == PartToolCall {
							hasToolCall = true
							break
						}
					}
				}
				events = append(events, Event{Kind: EventFinish, FinishReason: finishFromGemini(reason, hasToolCall)})
			}
		}
	}
	if usage := objectAt(object, "usageMetadata"); usage != nil {
		u := usageFromGemini(usage)
		events = append(events, Event{Kind: EventUsage, Usage: &u})
	}
	return events, nil
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAt(object, key); value != "" {
			return value
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Encoder interface

// streamEncoder renders canonical events in one client dialect.
//
// Encode is called once per canonical event, in order, and returns the frames
// to write to the client (zero or more; a delta that the dialect cannot express
// yields none plus a diagnostic). It must not write the terminal frame: Finish
// emits it, so a trailing usage frame (which OpenAI sends after the finish
// chunk) is still reflected in the client's final message. Finish is called
// exactly once, also for a stream that ended early. Diagnostics reports the
// conversion notes gathered so far.
type streamEncoder interface {
	Encode(event Event) ([]Frame, error)
	Finish() ([]Frame, error)
	Diagnostics() []Diagnostic
}

func frame(event string, payload any) (Frame, error) {
	data, err := marshalJSON(payload)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Event: event, Data: data}, nil
}

// ---------------------------------------------------------------------------
// Chat encoder

type chatStreamEncoder struct {
	options     StreamOptions
	started     bool
	id          string
	model       string
	created     int64
	usage       Usage
	finish      bool
	done        bool
	roleSent    bool
	diagnostics []Diagnostic
}

func (e *chatStreamEncoder) Encode(event Event) ([]Frame, error) {
	if e.done {
		return nil, nil
	}
	var frames []Frame
	if !e.started {
		e.started = true
		e.id = event.ID
		e.model = event.Model
		e.created = event.Created
	}
	if e.id == "" {
		e.id = e.options.ID
	}
	if e.id == "" {
		e.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if e.model == "" {
		e.model = e.options.Model
	}
	if e.created == 0 {
		e.created = e.options.Created
	}
	if e.created == 0 {
		e.created = time.Now().Unix()
	}
	switch event.Kind {
	case EventStart:
		// Anthropic reports input usage in message_start; without seeding it
		// here a Chat client only ever sees output tokens.
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		// The role chunk is emitted lazily by ensureRoleChunk so it always
		// precedes content, even when the upstream never sent a start event.
		return nil, nil
	case EventTextDelta:
		frames = append(frames, e.ensureRoleChunk()...)
		chunk := e.chunk(map[string]any{"content": event.Text})
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventReasoningDelta:
		frames = append(frames, e.ensureRoleChunk()...)
		chunk := e.chunk(map[string]any{"reasoning_content": event.Text})
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventRefusalDelta:
		frames = append(frames, e.ensureRoleChunk()...)
		chunk := e.chunk(map[string]any{"refusal": event.Text})
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventToolCallStart:
		if event.ThoughtSignature != "" {
			appendDiagnostic(&e.diagnostics, "thought_signature_dropped",
				"Gemini thought signature cannot be represented in OpenAI Chat", "tool_calls")
		}
		frames = append(frames, e.ensureRoleChunk()...)
		chunk := e.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": event.Index, "id": event.ToolCallID, "type": "function",
			"function": map[string]any{"name": event.ToolName, "arguments": ""},
		}}})
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventToolCallDelta:
		if event.ThoughtSignature != "" {
			appendDiagnostic(&e.diagnostics, "thought_signature_dropped",
				"Gemini thought signature cannot be represented in OpenAI Chat", "tool_calls")
		}
		frames = append(frames, e.ensureRoleChunk()...)
		chunk := e.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": event.Index, "function": map[string]any{"arguments": event.ArgumentsDelta},
		}}})
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventUsage:
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		return nil, nil
	case EventFinish:
		if e.finish {
			return nil, nil
		}
		if !e.roleSent {
			frames = append(frames, e.ensureRoleChunk()...)
		}
		e.finish = true
		// The terminal frames wait for Finish(): OpenAI emits the usage chunk
		// after the finish chunk, and dropping it would lose billing data.
		chunk := e.chunkWithFinish(finishToChat(event.FinishReason))
		f, err := frame("", chunk)
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventError:
		e.done = true
		f, err := frame("", map[string]any{"error": chatErrorEnvelope(event.Error)})
		if err != nil {
			return nil, err
		}
		return []Frame{f}, nil
	}
	return frames, nil
}

// ensureRoleChunk emits the assistant role chunk once, immediately before the
// first content-bearing chunk. Doing it lazily means an EventStart that only
// arrives at Finish() can never place the role after content.
func (e *chatStreamEncoder) ensureRoleChunk() []Frame {
	if e.roleSent {
		return nil
	}
	e.roleSent = true
	f, err := frame("", e.chunk(map[string]any{"role": "assistant"}))
	if err != nil {
		return nil
	}
	return []Frame{f}
}

func (e *chatStreamEncoder) chunk(delta map[string]any) map[string]any {
	return map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": nil}},
	}
}

func (e *chatStreamEncoder) chunkWithFinish(reason string) map[string]any {
	return map[string]any{
		"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "logprobs": nil, "finish_reason": reason}},
	}
}

func (e *chatStreamEncoder) terminalFrames() []Frame {
	var frames []Frame
	if e.options.IncludeUsage && e.usage != (Usage{}) {
		if f, err := frame("", map[string]any{
			"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
			"choices": []any{}, "usage": usageToChat(e.usage),
		}); err == nil {
			frames = append(frames, f)
		}
	}
	frames = append(frames, Frame{Data: []byte("[DONE]")})
	return frames
}

func (e *chatStreamEncoder) Finish() ([]Frame, error) {
	if e.done {
		return nil, nil
	}
	frames := []Frame{}
	if !e.started {
		e.started = true
		e.id = e.options.ID
		if e.id == "" {
			e.id = fmt.Sprintf("chatcmpl-%d", time.Now().Unix())
		}
		e.model = e.options.Model
		e.created = e.options.Created
		if e.created == 0 {
			e.created = time.Now().Unix()
		}
	}
	// Every Chat stream carries the assistant role chunk first, including a
	// usage-only or finish-only stream.
	frames = append(frames, e.ensureRoleChunk()...)
	if !e.finish {
		if f, err := frame("", e.chunkWithFinish("stop")); err == nil {
			frames = append(frames, f)
		}
	}
	frames = append(frames, e.terminalFrames()...)
	e.done = true
	return frames, nil
}

func (e *chatStreamEncoder) Diagnostics() []Diagnostic { return e.diagnostics }

func chatErrorEnvelope(streamError *StreamError) map[string]any {
	errorType := "upstream_error"
	message := "upstream stream failed"
	if streamError != nil {
		if streamError.Type != "" {
			errorType = streamError.Type
		}
		if streamError.Message != "" {
			message = streamError.Message
		}
	}
	envelope := map[string]any{"message": message, "type": errorType}
	// A code is optional in every dialect; only set it when there is one, so a
	// missing upstream code cannot render as the string "<nil>".
	if code := errorCode(streamError); code != "" {
		envelope["code"] = code
	}
	return envelope
}

// ---------------------------------------------------------------------------
// Anthropic encoder

type claudeStreamEncoder struct {
	options       StreamOptions
	estimateInput int
	started       bool
	finished      bool
	id            string
	model         string
	blockIndex    int
	currentBlock  string // text, thinking, tool_use
	usage         Usage
	toolBlocks    map[int]int
	finishReason  string
	stopSequence  string
	openToolBlock int
	diagnostics   []Diagnostic
}

func (e *claudeStreamEncoder) Encode(event Event) ([]Frame, error) {
	if event.Kind == EventStart {
		return e.encodeBody(event)
	}
	var prefix []Frame
	if !e.started {
		started, err := e.encodeBody(Event{Kind: EventStart, ID: e.options.ID, Model: e.options.Model})
		if err != nil {
			return nil, err
		}
		prefix = started
	}
	frames, err := e.encodeBody(event)
	if err != nil {
		return prefix, err
	}
	return append(prefix, frames...), nil
}

func (e *claudeStreamEncoder) encodeBody(event Event) ([]Frame, error) {
	var frames []Frame
	if event.Kind == EventStart {
		if !e.started {
			e.started = true
			e.id = event.ID
			if e.id == "" {
				e.id = e.options.ID
			}
			if e.id == "" {
				e.id = fmt.Sprintf("msg_%d", time.Now().Unix())
			}
			e.model = event.Model
			if e.model == "" {
				e.model = e.options.Model
			}
			if event.Usage != nil {
				e.usage.Add(*event.Usage)
			}
			input := e.usage.InputTokens
			if input == 0 {
				input = e.estimateInput
			}
			f, err := frame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": e.id, "type": "message", "role": "assistant", "model": e.model,
					"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]any{"input_tokens": input, "output_tokens": e.usage.OutputTokens},
				},
			})
			if err != nil {
				return nil, err
			}
			frames = append(frames, f)
		}
		return frames, nil
	}
	switch event.Kind {
	case EventTextDelta:
		if event.Text == "" {
			return nil, nil
		}
		if e.currentBlock != "text" {
			e.closeOpenToolBlocks(&frames)
			e.closeBlock(&frames)
			e.startBlock(&frames, "text", map[string]any{"type": "text", "text": ""})
		}
		f, err := frame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": event.Text},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	case EventReasoningDelta:
		if event.Text == "" {
			return nil, nil
		}
		if e.currentBlock != "thinking" {
			e.closeOpenToolBlocks(&frames)
			e.closeBlock(&frames)
			e.startBlock(&frames, "thinking", map[string]any{"type": "thinking", "thinking": ""})
		}
		f, err := frame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": event.Text},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	case EventRefusalDelta:
		if event.Text == "" {
			return nil, nil
		}
		if e.currentBlock != "text" {
			e.closeOpenToolBlocks(&frames)
			e.closeBlock(&frames)
			e.startBlock(&frames, "text", map[string]any{"type": "text", "text": ""})
		}
		f, err := frame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": event.Text},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	case EventToolCallStart:
		if e.toolBlocks == nil {
			e.toolBlocks = map[int]int{}
		}
		if _, exists := e.toolBlocks[event.Index]; exists {
			// Any provider can repeat a start event; opening a second block for
			// the same index would orphan the first one.
			return frames, nil
		}
		// Text/thinking must be closed before the first tool block, but tool
		// blocks stay open in parallel: Chat interleaves argument deltas by
		// index, and Anthropic addresses each block by its own index.
		if e.currentBlock != "tool_use" {
			e.closeBlock(&frames)
		}
		blockIndex := e.blockIndex
		e.startBlock(&frames, "tool_use", map[string]any{
			"type": "tool_use", "id": event.ToolCallID, "name": event.ToolName, "input": map[string]any{},
		})
		e.toolBlocks[event.Index] = blockIndex
		// startBlock advanced the shared index; the next tool block gets a new
		// one, but the current block stays open until EventToolCallEnd.
		e.blockIndex++
		e.currentBlock = "tool_use"
		e.openToolBlock = blockIndex
	case EventToolCallDelta:
		index, ok := e.toolBlocks[event.Index]
		if event.ArgumentsDelta == "" {
			return frames, nil
		}
		if !ok {
			// The block was already closed (or never opened); silently dropping
			// the fragment would hand the client a tool call with truncated
			// arguments.
			appendDiagnostic(&e.diagnostics, "tool_arguments_dropped",
				"tool argument fragment arrived for a closed block and was dropped", "tool_calls")
			return frames, nil
		}
		f, err := frame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": event.ArgumentsDelta},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	case EventToolCallEnd:
		if index, ok := e.toolBlocks[event.Index]; ok {
			e.closeToolBlock(&frames, index)
			delete(e.toolBlocks, event.Index)
		}
	case EventUsage:
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
	case EventFinish:
		// Deferred to Finish(): a trailing usage frame (OpenAI sends one after
		// the finish chunk) must be reflected in message_delta.usage.
		e.closeAllBlocks(&frames)
		e.finishReason = finishToClaude(event.FinishReason)
		e.stopSequence = event.StopSequence
	case EventError:
		f, err := frame("error", map[string]any{
			"type": "error", "error": map[string]any{
				"type":    claudeErrorType(event.Error),
				"message": errorMessage(event.Error),
			},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
		e.finished = true
	case EventPing:
		f, err := frame("ping", map[string]any{"type": "ping"})
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, nil
}

func (e *claudeStreamEncoder) startBlock(frames *[]Frame, kind string, block map[string]any) {
	f, err := frame("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIndex, "content_block": block,
	})
	if err == nil {
		*frames = append(*frames, f)
	}
	e.currentBlock = kind
}

func (e *claudeStreamEncoder) closeBlock(frames *[]Frame) {
	if e.currentBlock == "" {
		return
	}
	if e.currentBlock == "tool_use" {
		// A tool block may still be awaiting its own end event.
		return
	}
	f, err := frame("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": e.blockIndex,
	})
	if err == nil {
		*frames = append(*frames, f)
	}
	e.blockIndex++
	e.currentBlock = ""
}

// closeToolBlock closes one tool block by its own index, independent of the
// shared cursor, so parallel calls never stop the wrong block.
// closeOpenToolBlocks closes every still-open tool block in ascending index
// order. Anthropic requires each content_block_stop to follow its own block;
// closing them out of order (or interleaving a text block) is invalid.
func (e *claudeStreamEncoder) closeOpenToolBlocks(frames *[]Frame) {
	indices := make([]int, 0, len(e.toolBlocks))
	for _, index := range e.toolBlocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		e.closeToolBlock(frames, index)
	}
	e.toolBlocks = nil
	// currentBlock tracks the text/thinking block; a tool_use marker is
	// cleared by closeToolBlock itself. Do not clobber a text block here.
	if e.currentBlock == "tool_use" {
		e.currentBlock = ""
	}
}

func (e *claudeStreamEncoder) closeToolBlock(frames *[]Frame, index int) {
	f, err := frame("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
	if err == nil {
		*frames = append(*frames, f)
	}
	if e.openToolBlock == index {
		e.currentBlock = ""
		e.openToolBlock = -1
	}
}

// closeAllBlocks closes text/thinking and any tool blocks still open.
func (e *claudeStreamEncoder) closeAllBlocks(frames *[]Frame) {
	// Tool blocks must close even when currentBlock was cleared by the last
	// EventToolCallEnd: another parallel block can still be open.
	if len(e.toolBlocks) > 0 {
		e.closeOpenToolBlocks(frames)
	}
	if e.currentBlock != "" {
		e.closeBlock(frames)
	}
	e.toolBlocks = nil
	e.currentBlock = ""
}

func (e *claudeStreamEncoder) Finish() ([]Frame, error) {
	if e.finished {
		return nil, nil
	}
	var frames []Frame
	if !e.started {
		startFrames, err := e.Encode(Event{Kind: EventStart, ID: e.options.ID, Model: e.options.Model})
		if err != nil {
			return nil, err
		}
		frames = append(frames, startFrames...)
	}
	e.closeAllBlocks(&frames)
	reason := e.finishReason
	if reason == "" {
		reason = "end_turn"
	}
	var stopSequence any
	if e.stopSequence != "" {
		stopSequence = e.stopSequence
	}
	// The final usage is cumulative and may repeat the input/cache counts:
	// Anthropic's own web-search stream carries them here, and multi-round
	// callers (server-tool loops) aggregate this event.
	f, err := frame("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": reason, "stop_sequence": stopSequence,
		},
		"usage": usageToClaude(e.usage),
	})
	if err != nil {
		return frames, err
	}
	frames = append(frames, f)
	if stop, err := frame("message_stop", map[string]any{"type": "message_stop"}); err == nil {
		frames = append(frames, stop)
	}
	e.finished = true
	return frames, nil
}

func (e *claudeStreamEncoder) Diagnostics() []Diagnostic { return e.diagnostics }

func claudeErrorType(streamError *StreamError) string {
	if streamError != nil && streamError.Type != "" {
		switch streamError.Type {
		case "invalid_request_error", "authentication_error", "permission_error", "not_found_error",
			"request_too_large", "rate_limit_error", "api_error", "overloaded_error":
			return streamError.Type
		}
	}
	return "api_error"
}

func errorMessage(streamError *StreamError) string {
	if streamError == nil || streamError.Message == "" {
		return "upstream stream failed"
	}
	return streamError.Message
}

// ---------------------------------------------------------------------------
// Gemini encoder

type geminiStreamEncoder struct {
	finished       bool
	finishEmitted  bool
	model          string
	usage          Usage
	toolOrder      []int
	toolNames      map[int]string
	toolSignatures map[int]string
	toolArgs       map[int]*strings.Builder
	diagnostics    []Diagnostic
}

func (e *geminiStreamEncoder) Encode(event Event) ([]Frame, error) {
	if e.toolNames == nil {
		e.toolNames = map[int]string{}
		e.toolSignatures = map[int]string{}
		e.toolArgs = map[int]*strings.Builder{}
	}
	switch event.Kind {
	case EventStart:
		if event.Model != "" {
			e.model = event.Model
		}
		// Anthropic reports input usage in message_start; without seeding it
		// here the Gemini usageMetadata only ever counts output tokens.
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		return nil, nil
	case EventTextDelta:
		if event.Text == "" {
			return nil, nil
		}
		return geminiChunk(map[string]any{"text": event.Text}), nil
	case EventReasoningDelta:
		if event.Text == "" {
			return nil, nil
		}
		return geminiChunk(map[string]any{"text": event.Text, "thought": true}), nil
	case EventRefusalDelta:
		if event.Text == "" {
			return nil, nil
		}
		return geminiChunk(map[string]any{"text": event.Text}), nil
	case EventToolCallStart:
		if _, exists := e.toolNames[event.Index]; !exists {
			e.toolOrder = append(e.toolOrder, event.Index)
		}
		e.toolNames[event.Index] = event.ToolName
		if event.ThoughtSignature != "" {
			e.toolSignatures[event.Index] = event.ThoughtSignature
		}
		return nil, nil
	case EventToolCallDelta:
		if event.ThoughtSignature != "" {
			// The signature can arrive on its own delta (Interactions sends it
			// as a separate step.delta); keep it for the functionCall part.
			e.toolSignatures[event.Index] = event.ThoughtSignature
		}
		if event.ArgumentsDelta == "" {
			return nil, nil
		}
		builder, ok := e.toolArgs[event.Index]
		if !ok {
			builder = &strings.Builder{}
			e.toolArgs[event.Index] = builder
		}
		builder.WriteString(event.ArgumentsDelta)
		return nil, nil
	case EventToolCallEnd:
		return e.flushTool(event.Index), nil
	case EventUsage:
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		// Usage that trails the finish frame (Gemini often sends it in the
		// same chunk) still needs its own frame.
		if e.finishEmitted {
			if f, err := frame("", map[string]any{"usageMetadata": usageToGemini(e.usage)}); err == nil {
				return []Frame{f}, nil
			}
		}
		return nil, nil
	case EventFinish:
		if e.finishEmitted {
			return nil, nil
		}
		var frames []Frame
		for _, index := range e.toolOrder {
			if e.toolNames[index] != "" {
				frames = append(frames, e.flushTool(index)...)
			}
		}
		if f, err := frame("", map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{}},
				"finishReason": finishToGemini(event.FinishReason), "index": 0,
			}},
		}); err == nil {
			frames = append(frames, f)
		}
		if e.usage != (Usage{}) {
			if f, err := frame("", map[string]any{"usageMetadata": usageToGemini(e.usage)}); err == nil {
				frames = append(frames, f)
			}
		}
		e.finishEmitted = true
		return frames, nil
	case EventError:
		e.finished = true
		status := ""
		code := 0
		if event.Error != nil {
			status = event.Error.Status
			if event.Error.Code != "" {
				fmt.Sscanf(event.Error.Code, "%d", &code)
			}
		}
		f, err := frame("", map[string]any{"error": map[string]any{
			"code": code, "message": errorMessage(event.Error), "status": status,
		}})
		if err != nil {
			return nil, err
		}
		return []Frame{f}, nil
	}
	return nil, nil
}

func geminiChunk(part map[string]any) []Frame {
	f, err := frame("", map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{part}},
			"index":   0,
		}},
	})
	if err != nil {
		return nil
	}
	return []Frame{f}
}

func (e *geminiStreamEncoder) flushTool(index int) []Frame {
	name := e.toolNames[index]
	if name == "" {
		return nil
	}
	signature := e.toolSignatures[index]
	delete(e.toolSignatures, index)
	delete(e.toolNames, index)
	raw := ""
	if builder, ok := e.toolArgs[index]; ok {
		raw = builder.String()
	}
	delete(e.toolArgs, index)
	var args any = map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if json.Unmarshal([]byte(raw), &args) != nil {
			appendDiagnostic(&e.diagnostics, "invalid_tool_arguments",
				"streamed tool arguments are not valid JSON; an empty object was used", "functionCall")
			args = map[string]any{}
		}
	}
	part := map[string]any{"functionCall": map[string]any{"name": name, "args": args}}
	// Gemini 3 rejects a replayed call whose thought signature was dropped.
	if signature != "" {
		part["thoughtSignature"] = signature
	}
	return geminiChunk(part)
}

func (e *geminiStreamEncoder) Finish() ([]Frame, error) {
	if e.finished {
		return nil, nil
	}
	e.finished = true
	if e.finishEmitted {
		// Only a stream that never saw a finish reason gets a synthesized one;
		// emitting a second frame would overwrite MAX_TOKENS/SAFETY with STOP.
		return nil, nil
	}
	return e.Encode(Event{Kind: EventFinish, FinishReason: FinishStop})
}

func (e *geminiStreamEncoder) Diagnostics() []Diagnostic { return e.diagnostics }

// ---------------------------------------------------------------------------
// Responses encoder

type responsesStreamEncoder struct {
	options  StreamOptions
	started  bool
	finished bool
	sequence int
	id       string
	model    string
	created  int64
	usage    Usage
	// One assistant turn is at most one message item, one reasoning item and
	// any number of function-call items. Text and reasoning stream live; tool
	// calls are collected and emitted at Finish so the message stays first.
	messageIndex   int
	reasoningIndex int
	nextIndex      int
	messageItem    map[string]any
	messageText    strings.Builder
	refusalMode    bool
	reasoningItem  map[string]any
	reasoningText  strings.Builder
	toolItems      []map[string]any
	// toolPositions records the output_index allocated to each toolItems
	// entry: tool calls interleave with text and reasoning, so their
	// positions are not contiguous and cannot be derived from a base.
	toolPositions []int
	toolByIndex   map[int]int
	diagnostics   []Diagnostic
	finishReason  string
}

func (e *responsesStreamEncoder) Encode(event Event) ([]Frame, error) {
	if e.toolByIndex == nil {
		e.toolByIndex = map[int]int{}
	}
	var frames []Frame
	if !e.started && event.Kind != EventStart {
		started, err := e.Encode(Event{Kind: EventStart, ID: e.options.ID, Model: e.options.Model, Created: e.options.Created})
		if err != nil {
			return nil, err
		}
		frames = append(frames, started...)
	}
	switch event.Kind {
	case EventStart:
		if e.started {
			return nil, nil
		}
		e.started = true
		e.id = normalizeResponsesID(event.ID)
		if e.id == "" {
			e.id = normalizeResponsesID(e.options.ID)
		}
		if e.id == "" {
			e.id = newResponseID(time.Now().Unix())
		}
		e.model = event.Model
		if e.model == "" {
			e.model = e.options.Model
		}
		e.created = event.Created
		if e.created == 0 {
			e.created = e.options.Created
		}
		if e.created == 0 {
			e.created = time.Now().Unix()
		}
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		f, err := e.eventFrame("response.created", map[string]any{
			"type": "response.created", "sequence_number": e.nextSequence(),
			"response": e.responseObject("in_progress", []any{}, nil),
		})
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventTextDelta, EventRefusalDelta:
		if event.Text == "" {
			return frames, nil
		}
		isRefusal := event.Kind == EventRefusalDelta
		if e.messageItem == nil {
			e.messageIndex = e.nextIndex
			e.nextIndex++
			item := map[string]any{
				"id": fmt.Sprintf("msg_%d", e.messageIndex), "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			}
			e.messageItem = item
			e.refusalMode = isRefusal
			added, err := e.eventFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSequence(),
				"output_index": e.messageIndex, "item": item,
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, added)
			partType := "output_text"
			if isRefusal {
				partType = "refusal"
			}
			part := map[string]any{"type": partType}
			if partType == "output_text" {
				part["text"] = ""
				part["annotations"] = []any{}
			} else {
				part["refusal"] = ""
			}
			addedPart, err := e.eventFrame("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "sequence_number": e.nextSequence(),
				"item_id": item["id"], "output_index": e.messageIndex, "content_index": 0, "part": part,
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, addedPart)
		}
		e.messageText.WriteString(event.Text)
		eventName := "response.output_text.delta"
		payload := map[string]any{
			"type": eventName, "sequence_number": e.nextSequence(),
			"item_id": e.messageItem["id"], "output_index": e.messageIndex, "content_index": 0,
			"delta": event.Text, "logprobs": []any{},
		}
		if isRefusal {
			eventName = "response.refusal.delta"
			payload = map[string]any{
				"type": eventName, "sequence_number": e.nextSequence(),
				"item_id": e.messageItem["id"], "output_index": e.messageIndex, "content_index": 0,
				"delta": event.Text,
			}
		}
		deltaFrame, err := e.eventFrame(eventName, payload)
		if err != nil {
			return frames, err
		}
		return append(frames, deltaFrame), nil
	case EventReasoningDelta:
		if event.Text == "" {
			return frames, nil
		}
		if e.reasoningItem == nil {
			e.reasoningIndex = e.nextIndex
			e.nextIndex++
			item := map[string]any{
				"id": fmt.Sprintf("rs_%d", e.reasoningIndex), "type": "reasoning", "status": "in_progress",
				"summary": []any{},
			}
			e.reasoningItem = item
			added, err := e.eventFrame("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSequence(),
				"output_index": e.reasoningIndex, "item": item,
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, added)
		}
		e.reasoningText.WriteString(event.Text)
		deltaFrame, err := e.eventFrame("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": e.nextSequence(),
			"item_id": e.reasoningItem["id"], "output_index": e.reasoningIndex,
			"summary_index": 0, "delta": event.Text,
		})
		if err != nil {
			return frames, err
		}
		return append(frames, deltaFrame), nil
	case EventToolCallStart:
		if slot, exists := e.toolByIndex[event.Index]; exists {
			// A start after a delta (or a repeated start) adopts the item the
			// delta created rather than opening a second one for the index.
			if event.ToolCallID != "" {
				e.toolItems[slot]["call_id"] = event.ToolCallID
			}
			if event.ToolName != "" {
				e.toolItems[slot]["name"] = event.ToolName
			}
			return frames, nil
		}
		slot := len(e.toolItems)
		outputIndex := e.nextIndex
		e.nextIndex++
		item := map[string]any{
			"id": fmt.Sprintf("fc_%d", outputIndex), "type": "function_call", "status": "in_progress",
			"call_id": event.ToolCallID, "name": event.ToolName, "arguments": "",
		}
		e.toolByIndex[event.Index] = slot
		e.toolItems = append(e.toolItems, item)
		e.toolPositions = append(e.toolPositions, outputIndex)
		return frames, nil
	case EventToolCallDelta:
		index, ok := e.toolByIndex[event.Index]
		if !ok {
			// A delta without a start is still a valid call fragment: allocate
			// it from the shared counter so a later message cannot reuse the
			// same output_index.
			outputIndex := e.nextIndex
			e.nextIndex++
			index = len(e.toolItems)
			e.toolByIndex[event.Index] = index
			e.toolItems = append(e.toolItems, map[string]any{
				"id": fmt.Sprintf("fc_%d", outputIndex), "type": "function_call", "status": "in_progress",
				"call_id": "", "name": "", "arguments": "",
			})
			e.toolPositions = append(e.toolPositions, outputIndex)
		}
		current, _ := e.toolItems[index]["arguments"].(string)
		e.toolItems[index]["arguments"] = current + event.ArgumentsDelta
		return frames, nil
	case EventToolCallEnd:
		return frames, nil
	case EventUsage:
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		return frames, nil
	case EventFinish:
		e.finishReason = event.FinishReason
		return frames, nil
	case EventError:
		// Deferred like Finish so the failure can be reported once, with the
		// full output accumulated so far.
		e.finished = true
		e.finishReason = FinishOther
		errorPayload := map[string]any{
			"type": "error", "message": errorMessage(event.Error), "param": nil,
		}
		if code := errorCode(event.Error); code != "" {
			errorPayload["code"] = code
		}
		f, err := e.eventFrame("error", errorPayload)
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
		failed, err := e.eventFrame("response.failed", map[string]any{
			"type": "response.failed", "sequence_number": e.nextSequence(),
			"response": e.responseObjectWith("failed", e.outputItems(), nil, map[string]any{
				"code": errorCode(event.Error), "message": errorMessage(event.Error),
			}),
		})
		if err != nil {
			return frames, err
		}
		return append(frames, failed), nil
	}
	return frames, nil
}

// completedItem renders one accumulated item with its final content, looked up
// by identity so a positional assumption cannot substitute a sibling.
func (e *responsesStreamEncoder) completedItem(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	targetID, _ := source["id"].(string)
	for _, raw := range e.outputItems() {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := item["id"].(string); id == targetID {
			return item
		}
	}
	return nil
}

// outputItems renders the accumulated items ordered by the output_index each
// item announced, so output[i] always matches the index the client was told.
func (e *responsesStreamEncoder) outputItems() []any {
	type indexedItem struct {
		index int
		value any
	}
	ordered := make([]indexedItem, 0, 2+len(e.toolItems))
	if e.reasoningItem != nil {
		item := cloneMap(e.reasoningItem)
		if text := e.reasoningText.String(); text != "" {
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
		}
		item["status"] = "completed"
		ordered = append(ordered, indexedItem{index: e.reasoningIndex, value: item})
	}
	if e.messageItem != nil {
		item := cloneMap(e.messageItem)
		text := e.messageText.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if e.refusalMode {
			part = map[string]any{"type": "refusal", "refusal": text}
		}
		item["content"] = []any{part}
		item["status"] = "completed"
		ordered = append(ordered, indexedItem{index: e.messageIndex, value: item})
	}
	for offset, tool := range e.toolItems {
		item := cloneMap(tool)
		item["status"] = "completed"
		outputIndex := e.nextIndex
		if offset < len(e.toolPositions) {
			outputIndex = e.toolPositions[offset]
		}
		ordered = append(ordered, indexedItem{index: outputIndex, value: item})
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	items := make([]any, len(ordered))
	for position, entry := range ordered {
		items[position] = entry.value
	}
	return items
}

func cloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (e *responsesStreamEncoder) eventFrame(name string, payload map[string]any) (Frame, error) {
	return frame(name, payload)
}

func (e *responsesStreamEncoder) nextSequence() int {
	e.sequence++
	return e.sequence
}

func (e *responsesStreamEncoder) responseObject(status string, output []any, incomplete any) map[string]any {
	return e.responseObjectWith(status, output, incomplete)
}

func (e *responsesStreamEncoder) responseObjectWith(status string, output []any, incomplete any, streamError ...map[string]any) map[string]any {
	object := map[string]any{
		"id": e.id, "object": "response", "created_at": e.created, "status": status,
		"error": nil, "incomplete_details": incomplete, "instructions": nil,
		"model": e.model, "output": output, "parallel_tool_calls": true,
		"previous_response_id": nil, "store": false,
		"text":        map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": "auto", "tools": []any{}, "truncation": "disabled", "metadata": map[string]any{},
	}
	if e.usage != (Usage{}) {
		object["usage"] = usageToResponses(e.usage)
	}
	if len(streamError) > 0 && streamError[0] != nil {
		object["error"] = streamError[0]
	}
	return object
}

func errorCode(streamError *StreamError) string {
	if streamError == nil {
		return ""
	}
	if streamError.Code != "" {
		return streamError.Code
	}
	return streamError.Type
}

func (e *responsesStreamEncoder) Finish() ([]Frame, error) {
	if e.finished {
		return nil, nil
	}
	var frames []Frame
	if !e.started {
		started, err := e.Encode(Event{Kind: EventStart, ID: e.options.ID, Model: e.options.Model, Created: e.options.Created})
		if err != nil {
			return nil, err
		}
		frames = append(frames, started...)
	}
	// Close the streaming items: reasoning, message, then tool calls.
	if e.reasoningItem != nil {
		text := e.reasoningText.String()
		if done, err := e.eventFrame("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "sequence_number": e.nextSequence(),
			"item_id": e.reasoningItem["id"], "output_index": e.reasoningIndex, "summary_index": 0, "text": text,
		}); err == nil {
			frames = append(frames, done)
		}
		item := e.completedItem(e.reasoningItem)
		if done, err := e.eventFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.nextSequence(),
			"output_index": e.reasoningIndex, "item": item,
		}); err == nil {
			frames = append(frames, done)
		}
	}
	if e.messageItem != nil {
		text := e.messageText.String()
		eventName := "response.output_text.done"
		payload := map[string]any{
			"type": eventName, "sequence_number": e.nextSequence(),
			"item_id": e.messageItem["id"], "output_index": e.messageIndex,
			"content_index": 0, "text": text,
		}
		if e.refusalMode {
			eventName = "response.refusal.done"
			payload = map[string]any{
				"type": eventName, "sequence_number": e.nextSequence(),
				"item_id": e.messageItem["id"], "output_index": e.messageIndex,
				"content_index": 0, "refusal": text,
			}
		}
		if done, err := e.eventFrame(eventName, payload); err == nil {
			frames = append(frames, done)
		}
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if e.refusalMode {
			part = map[string]any{"type": "refusal", "refusal": text}
		}
		if done, err := e.eventFrame("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": e.nextSequence(),
			"item_id": e.messageItem["id"], "output_index": e.messageIndex,
			"content_index": 0, "part": part,
		}); err == nil {
			frames = append(frames, done)
		}
	}
	items := e.outputItems()
	for index, tool := range e.toolItems {
		item := cloneMap(tool)
		item["status"] = "completed"
		// Announce the index allocated when the call first appeared, not a
		// positional guess: tool calls interleave with text and reasoning.
		outputIndex := e.nextIndex
		if index < len(e.toolPositions) {
			outputIndex = e.toolPositions[index]
		}
		if added, err := e.eventFrame("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": e.nextSequence(),
			"output_index": outputIndex, "item": item,
		}); err == nil {
			frames = append(frames, added)
		}
		if done, err := e.eventFrame("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.nextSequence(),
			"output_index": outputIndex, "item": item,
		}); err == nil {
			frames = append(frames, done)
		}
	}
	status := "completed"
	var incomplete any
	switch e.finishReason {
	case FinishLength:
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case FinishContentFilter:
		status = "incomplete"
		incomplete = map[string]any{"reason": "content_filter"}
	}
	eventName := "response.completed"
	if status == "incomplete" {
		eventName = "response.incomplete"
	}
	f, err := e.eventFrame(eventName, map[string]any{
		"type": eventName, "sequence_number": e.nextSequence(),
		"response": e.responseObjectWith(status, items, incomplete),
	})
	if err != nil {
		return frames, err
	}
	frames = append(frames, f)
	e.finished = true
	return frames, nil
}

func (e *responsesStreamEncoder) Diagnostics() []Diagnostic { return e.diagnostics }
