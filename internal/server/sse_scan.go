package server

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// sseLine is one line of a captured SSE body, presented to the consumers that
// read a stream line by line.
type sseLine struct {
	// payload is the trimmed JSON payload of a "data:" line, or nil when the
	// line is not a data line.
	payload []byte
	// offset is the offset just past this line, which is what the write-log
	// mapping needs to find the line's timestamp.
	offset int
}

// scanSSEBody walks a captured SSE body once and feeds both views the metrics
// consumers need:
//
//   - onLine sees every "data:" line on its own, which is how the content-time
//     and speed-timeline consumers read a stream (they never join lines);
//   - onFrame sees each joined SSE event, which is how the usage consumers read
//     it (a provider may split one JSON object over several data lines).
//
// Doing both in one walk replaces the five separate body scans the recorder
// used to perform, while keeping each consumer's own view of the bytes: the
// consumers keep their parsing rules, so the recorded values do not change.
func scanSSEBody(body []byte, onLine func(sseLine), onFrame func(data []byte)) {
	if len(body) == 0 {
		return
	}
	var eventData bytes.Buffer
	flush := func() {
		if eventData.Len() == 0 {
			return
		}
		if onFrame != nil {
			onFrame(eventData.Bytes())
		}
		eventData.Reset()
	}

	for offset := 0; offset < len(body); {
		lineStart := offset
		newline := bytes.IndexByte(body[offset:], '\n')
		if newline < 0 {
			offset = len(body)
		} else {
			offset += newline + 1
		}
		line := bytes.TrimSuffix(body[lineStart:offset], []byte{'\r'})
		trimmed := bytes.TrimSpace(line)

		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			// A frame ends at a blank line, or at a data line that already
			// completes a JSON object (providers that omit the blank line).
			if eventData.Len() > 0 && gjson.ValidBytes(bytes.TrimSpace(eventData.Bytes())) {
				flush()
			}
			if bytes.Equal(payload, []byte("[DONE]")) {
				flush()
				continue
			}
			// The line is buffered whether or not it completes a JSON object on
			// its own, so a following data line joins it into a multi-line frame
			// and the frame consumers see the event exactly once, through the
			// flush.
			if eventData.Len() > 0 {
				eventData.WriteByte('\n')
			}
			eventData.Write(payload)
			if onLine != nil {
				onLine(sseLine{payload: payload, offset: offset})
			}
			continue
		}

		if len(trimmed) == 0 {
			flush()
		}
		// Any other line (an SSE comment or event name) contributes to neither
		// view and does not end a frame: a provider may emit "event: x" between
		// the data lines of one object.
	}
	flush()
}
