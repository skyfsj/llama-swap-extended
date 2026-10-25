package codec

func ConvertResponseToStream(source, target Format, body []byte, options StreamOptions) ([]Frame, []Diagnostic, error) {
	response, diagnostics, err := DecodeResponse(source, body)
	if err != nil {
		return nil, diagnostics, err
	}
	if source == OpenAIResponses && target == GeminiInteractions {
		diagnostics, err = restoreGatewayToolExecutions(body, response, diagnostics)
		if err != nil {
			return nil, diagnostics, err
		}
	}
	diagnosePartSignatures(response.Parts, target, &diagnostics)
	streamSource := source
	if streamSource == target {
		streamSource = OpenAIChat
		if streamSource == target {
			streamSource = OpenAIResponses
		}
	}
	converter, err := NewStreamConverter(streamSource, target, options)
	if err != nil {
		return nil, diagnostics, err
	}
	usage := response.Usage
	events := []Event{{Kind: EventStart, ID: response.ID, Model: response.Model, Created: response.Created, Usage: &usage}}
	for index, part := range response.Parts {
		event := Event{Text: part.Text, ThoughtSignature: part.ThoughtSignature}
		switch part.Kind {
		case PartText:
			event.Kind = EventTextDelta
		case PartReasoning:
			event.Kind = EventReasoningDelta
		case PartRefusal:
			event.Kind = EventRefusalDelta
		case PartToolCall:
			events = append(events,
				Event{Kind: EventToolCallStart, Index: index, ToolCallID: part.ToolCallID, ToolName: part.ToolName, ToolNamespace: part.ToolNamespace, ToolKind: part.ToolKind, ThoughtSignature: part.ThoughtSignature},
				Event{Kind: EventToolCallDelta, Index: index, ArgumentsDelta: argumentsText(part.Arguments)},
				Event{Kind: EventToolCallEnd, Index: index},
			)
			continue
		case PartToolResult:
			if target == GeminiInteractions {
				events = append(events, Event{Kind: EventToolExecution, Parts: []Part{part}})
			} else {
				appendDiagnostic(&diagnostics, "stream_part_dropped", "the target cannot stream an executed tool result", string(part.Kind))
			}
			continue
		default:
			appendDiagnostic(&diagnostics, "stream_part_dropped", "the response part has no streaming representation", string(part.Kind))
			continue
		}
		events = append(events, event)
	}
	events = append(events, Event{Kind: EventUsage, Usage: &usage})
	if responseIsFailed(response) {
		events = append(events, Event{Kind: EventError, Error: response.Error})
	} else {
		events = append(events, Event{Kind: EventFinish, FinishReason: response.FinishReason, StopSequence: response.StopSequence})
	}
	var frames []Frame
	for _, event := range events {
		emitted, err := converter.encoder.Encode(event)
		if err != nil {
			return frames, diagnostics, err
		}
		frames = append(frames, emitted...)
	}
	tail, err := converter.encoder.Finish()
	frames = append(frames, tail...)
	diagnostics = append(diagnostics, converter.Diagnostics()...)
	return frames, diagnostics, err
}
