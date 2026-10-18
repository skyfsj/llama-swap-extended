package codec

import (
	"fmt"
	"time"
)

// Interactions streaming: the upstream emits interaction.created,
// step.start / step.delta / step.stop and interaction.completed (plus error).
// Deltas carry a typed payload (text, arguments, thought_summary,
// thought_signature, function_result, image, audio, video, hosted-tool kinds).
//
// Canonical mapping:
//   - interaction.created              -> EventStart
//   - step.start(model_output)         -> nothing (the deltas carry the content)
//   - step.delta(text)                 -> EventTextDelta
//   - step.delta(thought_summary)      -> EventReasoningDelta
//   - step.delta(thought_signature)    -> attaches the signature to the open
//     reasoning or tool-call event
//   - step.start(function_call)        -> EventToolCallStart
//   - step.delta(arguments)            -> EventToolCallDelta
//   - step.stop(function_call)         -> EventToolCallEnd
//   - step.stop(usage)                 -> EventUsage
//   - interaction.completed            -> EventUsage + EventFinish
//   - error                            -> EventError

// decodeInteractionsEvent reads one upstream SSE payload.
func (c *StreamConverter) decodeInteractionsEvent(data []byte) ([]Event, error) {
	object, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	eventType := firstStringAt(object, "event_type", "type")
	switch eventType {
	case "interaction.status_update":
		// A progress notification with no cross-dialect event; the terminal
		// frames carry the status a client needs.
		return nil, nil
	case "interaction.created":
		interaction := objectAt(object, "interaction")
		if interaction == nil {
			interaction = object
		}
		return []Event{{
			Kind: EventStart, ID: stringAt(interaction, "id"), Model: stringAt(interaction, "model"),
			Created: interactionsCreatedAt(interaction),
		}}, nil
	case "step.start":
		step := objectAt(object, "step")
		if step == nil {
			return nil, nil
		}
		index, _ := intAt(object, "index")
		switch stringAt(step, "type") {
		case "function_call":
			return []Event{{
				Kind: EventToolCallStart, Index: index,
				ToolCallID: stringAt(step, "id"), ToolName: stringAt(step, "name"),
			}}, nil
		case "thought":
			if signature := stringAt(step, "signature"); signature != "" {
				return []Event{{Kind: EventReasoningDelta, ThoughtSignature: signature}}, nil
			}
			return nil, nil
		default:
			return nil, nil
		}
	case "step.delta":
		index, _ := intAt(object, "index")
		delta := objectAt(object, "delta")
		if delta == nil {
			return nil, nil
		}
		return c.interactionsDeltaEvents(delta, index, object), nil
	case "step.stop":
		index, _ := intAt(object, "index")
		events := []Event{{Kind: EventToolCallEnd, Index: index}}
		if usage := objectAt(object, "usage"); usage != nil {
			u := usageFromInteractions(usage)
			events = append(events, Event{Kind: EventUsage, Usage: &u})
		} else if usage := objectAt(object, "step_usage"); usage != nil {
			u := usageFromInteractions(usage)
			events = append(events, Event{Kind: EventUsage, Usage: &u})
		}
		return events, nil
	case "interaction.completed":
		interaction := objectAt(object, "interaction")
		if interaction == nil {
			interaction = object
		}
		var events []Event
		if usage := objectAt(interaction, "usage"); usage != nil {
			u := usageFromInteractions(usage)
			events = append(events, Event{Kind: EventUsage, Usage: &u})
		}
		status := stringAt(interaction, "status")
		if status == "budget_exceeded" {
			// Mirrors the non-stream path: a budget stop is a terminal failure,
			// not a completed answer a client would treat as final.
			events = append(events, Event{Kind: EventError, Error: &StreamError{
				Code: "budget_exceeded", Message: "the interaction exceeded its budget",
			}})
			return events, nil
		}
		if status == "failed" || status == "cancelled" {
			// A failed or cancelled interaction is a terminal error, not a
			// finished answer: FinishOther renders as "stop" for Chat clients.
			message := "the interaction " + status
			if array := arrayAt(interaction, "errors"); len(array) > 0 {
				if entry, ok := array[0].(map[string]any); ok {
					if text := stringAt(entry, "message"); text != "" {
						message = text
					}
				}
			}
			events = append(events, Event{Kind: EventError, Error: &StreamError{
				Code: status, Message: message,
			}})
			return events, nil
		}
		if status == "queued" || status == "in_progress" || status == "running" {
			// The non-stream path reports this as a pending error; a stream must
			// not present a bogus "completed" frame for a response that never ran.
			events = append(events, Event{Kind: EventError, Error: &StreamError{
				Code:    "interaction_pending",
				Message: fmt.Sprintf("the interaction is still %s", status),
			}})
			return events, nil
		}
		if status == "requires_action" {
			// requires_action means the model is waiting for its tool call to be
			// run. With a call present the finish reason says so; without one the
			// non-stream path reports an error, and a stream must not invent a
			// completed answer for a request that cannot proceed.
			hasToolCall := false
			for _, part := range c.response.Parts {
				if part.Kind == PartToolCall {
					hasToolCall = true
					break
				}
			}
			if !hasToolCall {
				events = append(events, Event{Kind: EventError, Error: &StreamError{
					Code:    "requires_action_without_call",
					Message: "the interaction requires an action but carries no function_call step",
				}})
				return events, nil
			}
		}
		reason := FinishStop
		switch status {
		case "incomplete":
			reason = FinishLength
		default:
			// A turn that ended on a tool call must say so: clients decide
			// whether to run the tools from the finish reason.
			for _, part := range c.response.Parts {
				if part.Kind == PartToolCall {
					reason = FinishToolUse
					break
				}
			}
		}
		events = append(events, Event{Kind: EventFinish, FinishReason: reason})
		return events, nil
	case "error":
		errorObject := objectAt(object, "error")
		if errorObject == nil {
			errorObject = object
		}
		streamError := &StreamError{Message: stringAt(errorObject, "message")}
		if code := formatErrorCode(errorObject["code"]); code != "" {
			streamError.Code = code
		}
		return []Event{{Kind: EventError, Error: streamError}}, nil
	}
	return nil, nil
}

// interactionsDeltaEvents turns one typed delta into canonical events.
func (c *StreamConverter) interactionsDeltaEvents(delta map[string]any, index int, envelope map[string]any) []Event {
	deltaType := firstStringAt(delta, "type", "delta_type")
	switch deltaType {
	case "text":
		return []Event{{Kind: EventTextDelta, Text: stringAt(delta, "text")}}
	case "thought_summary":
		content := objectAt(delta, "content")
		if content == nil {
			content = delta
		}
		return []Event{{Kind: EventReasoningDelta, Text: stringAt(content, "text")}}
	case "thought_signature":
		signature := stringAt(delta, "signature")
		if signature == "" {
			return nil
		}
		// A signature with no text attaches to whatever block is open: the
		// reasoning summary or the tool call it belongs to.
		if pending := c.openToolIndex(); pending >= 0 {
			return []Event{{Kind: EventToolCallDelta, Index: pending, ThoughtSignature: signature}}
		}
		return []Event{{Kind: EventReasoningDelta, ThoughtSignature: signature}}
	case "arguments_delta", "arguments":
		// The SDK's union maps the streamed tool arguments to
		// "arguments_delta"; "arguments" is accepted for compatibility with
		// servers that reuse the step field name.
		arguments := stringAt(delta, "arguments")
		if arguments == "" {
			return nil
		}
		return []Event{{Kind: EventToolCallDelta, Index: index, ArgumentsDelta: arguments}}
	case "function_result":
		return []Event{{Kind: EventToolCallEnd, Index: index}}
	case "image", "audio", "video":
		appendDiagnostic(&c.diagnostics, "stream_media_dropped",
			fmt.Sprintf("an Interactions %s delta cannot be represented as a text delta", deltaType), deltaType)
		return nil
	case "text_annotation_delta":
		// Text annotations have no cross-dialect event.
		return nil
	case "google_search_call", "google_search_result", "file_search_call", "file_search_result",
		"code_execution_call", "code_execution_result", "url_context_call", "url_context_result",
		"mcp_server_tool_call", "mcp_server_tool_result", "retrieval_call", "retrieval_result",
		"google_maps_call", "google_maps_result", "processing_call", "processing_result":
		appendDiagnostic(&c.diagnostics, "hosted_step_dropped",
			fmt.Sprintf("provider-hosted delta %q has no cross-dialect representation", deltaType), deltaType)
		return nil
	case "":
		// Some frames inline the payload without a discriminator: infer it.
		if text := stringAt(delta, "text"); text != "" {
			return []Event{{Kind: EventTextDelta, Text: text}}
		}
		return nil
	default:
		appendDiagnostic(&c.diagnostics, "unknown_delta_dropped",
			fmt.Sprintf("unknown delta type %q was dropped", deltaType), deltaType)
		return nil
	}
}

// openToolIndex reports the index of the tool call currently being streamed, or
// -1 when none is open.
func (c *StreamConverter) openToolIndex() int {
	for index := len(c.response.Parts) - 1; index >= 0; index-- {
		if c.response.Parts[index].Kind == PartToolCall {
			return index
		}
	}
	return -1
}

func interactionsCreatedAt(object map[string]any) int64 {
	if created := stringAt(object, "created"); created != "" {
		if moment, err := time.Parse(time.RFC3339, created); err == nil {
			return moment.Unix()
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Interactions encoder

type interactionsStreamEncoder struct {
	options StreamOptions
	started bool
	id      string
	model   string
	created int64
	usage   Usage

	stepIndex    int
	openStep     string // model_output, thought, function_call
	openStepID   int
	finishReason string
	// failed records a terminal error so the closing frame reports a failed
	// status instead of "completed".
	failed         bool
	failureCode    string
	failureMessage string
	diagnostics    []Diagnostic
	finished       bool
}

func (e *interactionsStreamEncoder) Encode(event Event) ([]Frame, error) {

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
			if event.Usage != nil {
				e.usage.Add(*event.Usage)
			}
			return nil, nil
		}
		e.started = true
		e.id = event.ID
		if e.id == "" {
			e.id = e.options.ID
		}
		if e.id == "" {
			e.id = fmt.Sprintf("interaction_%d", time.Now().Unix())
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
		f, err := e.event("interaction.created", map[string]any{
			"interaction": e.interactionObject("in_progress", nil),
		})
		if err != nil {
			return nil, err
		}
		return append(frames, f), nil
	case EventTextDelta:
		if event.Text == "" {
			return frames, nil
		}
		frames = append(frames, e.ensureStep("model_output", map[string]any{"type": "model_output", "content": []any{}})...)
		delta, err := e.delta(map[string]any{"type": "text", "text": event.Text})
		if err != nil {
			return frames, err
		}
		return append(frames, delta...), nil
	case EventRefusalDelta:
		if event.Text == "" {
			return frames, nil
		}
		frames = append(frames, e.ensureStep("model_output", map[string]any{"type": "model_output", "content": []any{}})...)
		delta, err := e.delta(map[string]any{"type": "text", "text": event.Text})
		if err != nil {
			return frames, err
		}
		return append(frames, delta...), nil
	case EventReasoningDelta:
		if event.Text == "" && event.ThoughtSignature == "" {
			return frames, nil
		}
		if event.Text != "" {
			frames = append(frames, e.ensureStep("thought", map[string]any{"type": "thought", "summary": []any{}})...)
			delta, err := e.delta(map[string]any{
				"type": "thought_summary", "content": map[string]any{"type": "text", "text": event.Text},
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, delta...)
		}
		if event.ThoughtSignature != "" {
			frames = append(frames, e.ensureStep("thought", map[string]any{"type": "thought", "summary": []any{}})...)
			signature, err := e.delta(map[string]any{
				"type": "thought_signature", "signature": event.ThoughtSignature,
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, signature...)
		}
		return frames, nil
	case EventToolCallStart:
		frames = append(frames, e.closeStep()...)
		step := map[string]any{"type": "function_call", "name": event.ToolName, "arguments": map[string]any{}}
		if event.ToolCallID != "" {
			step["id"] = event.ToolCallID
		}
		frames = append(frames, e.ensureStep("function_call", step)...)
		if event.ThoughtSignature != "" {
			signature, err := e.delta(map[string]any{
				"type": "thought_signature", "signature": event.ThoughtSignature,
			})
			if err != nil {
				return frames, err
			}
			frames = append(frames, signature...)
		}
		return frames, nil
	case EventToolCallDelta:
		if event.ThoughtSignature != "" && event.ArgumentsDelta == "" {
			signature, err := e.delta(map[string]any{
				"type": "thought_signature", "signature": event.ThoughtSignature,
			})
			if err != nil {
				return frames, err
			}
			return append(frames, signature...), nil
		}
		if event.ArgumentsDelta == "" {
			return frames, nil
		}
		delta, err := e.delta(map[string]any{"type": "arguments_delta", "arguments": event.ArgumentsDelta})
		if err != nil {
			return frames, err
		}
		return append(frames, delta...), nil
	case EventToolCallEnd:
		return append(frames, e.closeStep()...), nil
	case EventUsage:
		if event.Usage != nil {
			e.usage.Add(*event.Usage)
		}
		return frames, nil
	case EventFinish:
		e.finishReason = event.FinishReason
		frames = append(frames, e.closeStep()...)
		return frames, nil
	case EventError:
		e.failed = true
		e.failureCode = errorCode(event.Error)
		e.failureMessage = errorMessage(event.Error)
		frames = append(frames, e.closeStep()...)
		f, err := e.event("error", map[string]any{
			"error": map[string]any{"code": e.failureCode, "message": e.failureMessage},
		})
		if err != nil {
			return frames, err
		}
		return append(frames, f), nil
	}
	return frames, nil
}

func (e *interactionsStreamEncoder) event(eventType string, payload map[string]any) (Frame, error) {
	payload["event_type"] = eventType
	return frame(eventType, payload)
}

// ensureStep opens a step of the requested kind when a different one is open.
func (e *interactionsStreamEncoder) ensureStep(kind string, step map[string]any) []Frame {
	if e.openStep == kind {
		return nil
	}
	var frames []Frame
	frames = append(frames, e.closeStep()...)
	e.openStep = kind
	e.openStepID = e.stepIndex
	f, err := e.event("step.start", map[string]any{
		"index": e.openStepID, "step": step,
	})
	if err == nil {
		frames = append(frames, f)
	}
	return frames
}

func (e *interactionsStreamEncoder) closeStep() []Frame {
	if e.openStep == "" {
		return nil
	}
	payload := map[string]any{"index": e.openStepID}
	f, err := e.event("step.stop", payload)
	e.openStep = ""
	e.stepIndex++
	if err != nil {
		return nil
	}
	return []Frame{f}
}

func (e *interactionsStreamEncoder) delta(delta map[string]any) ([]Frame, error) {
	f, err := e.event("step.delta", map[string]any{
		"index": e.openStepID, "delta": delta,
	})
	if err != nil {
		return nil, err
	}
	return []Frame{f}, nil
}

func (e *interactionsStreamEncoder) interactionObject(status string, steps []any) map[string]any {
	created := time.Unix(e.created, 0).UTC().Format(time.RFC3339)
	object := map[string]any{
		"id": e.id, "object": "interaction", "status": status,
		"created": created, "updated": created, "model": e.model,
	}
	if steps != nil {
		object["steps"] = steps
	}
	if e.usage != (Usage{}) {
		object["usage"] = usageToInteractions(e.usage)
	}
	return object
}

func (e *interactionsStreamEncoder) Finish() ([]Frame, error) {
	if e.finished {
		return nil, nil
	}
	e.finished = true
	var frames []Frame
	if !e.started {
		started, err := e.Encode(Event{Kind: EventStart, ID: e.options.ID, Model: e.options.Model, Created: e.options.Created})
		if err != nil {
			return nil, err
		}
		frames = append(frames, started...)
	}
	frames = append(frames, e.closeStep()...)
	// A stream that ended on a length, content filter or error must not report
	// "completed": clients read the status to decide whether to retry.
	status := "completed"
	switch {
	case e.failed:
		status = "failed"
	case e.finishReason == FinishLength || e.finishReason == FinishContentFilter:
		status = "incomplete"
	case e.finishReason == FinishOther:
		status = "incomplete"
	}
	f, err := e.event("interaction.completed", map[string]any{
		"interaction": e.interactionObject(status, []any{}),
	})
	if err != nil {
		return frames, err
	}
	return append(frames, f), nil
}

func (e *interactionsStreamEncoder) Diagnostics() []Diagnostic { return e.diagnostics }
