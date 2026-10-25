package server

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// The reference implementations below are the per-consumer versions the single
// walk replaced: each one scanned the whole body on its own. They exist so the
// merged scan can be proven equivalent on real bodies, not just on the fixtures
// the unit tests happen to carry.

func referenceStreamingContentTimes(body []byte, writes []responseBodyWrite) (time.Time, time.Time, bool) {
	var first, last time.Time
	writeAt := func(offset int, upper bool) time.Time {
		for index, write := range writes {
			if offset > write.end {
				continue
			}
			if write.hole && index > 0 {
				if upper {
					return write.at
				}
				return writes[index-1].at
			}
			return write.at
		}
		return time.Time{}
	}
	for offset := 0; offset < len(body); {
		lineStart := offset
		newline := bytes.IndexByte(body[offset:], '\n')
		if newline < 0 {
			offset = len(body)
		} else {
			offset += newline + 1
		}
		line := bytes.TrimSpace(body[lineStart:offset])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(payload) {
			continue
		}
		parsed := gjson.ParseBytes(payload)
		if !streamingEventHasText(parsed) {
			continue
		}
		at := writeAt(offset, !first.IsZero())
		if at.IsZero() {
			continue
		}
		if first.IsZero() {
			first = at
		}
		last = at
	}
	if first.IsZero() || last.Before(first) {
		return time.Time{}, time.Time{}, false
	}
	return first, last, true
}

func referenceProcessStreamingResponse(modelID string, start time.Time, body []byte) (ActivityLogEntry, error) {
	var (
		inputTokens, outputTokens int64
		cachedTokens              int64 = -1
		hasAny                    bool
		usage                     gjson.Result
		timings                   gjson.Result
		responseMetrics           gjson.Result
	)
	consume := func(data []byte) {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			return
		}
		parsed := gjson.ParseBytes(data)
		for _, path := range usagePaths {
			u := parsed.Get(path)
			if !u.Exists() {
				continue
			}
			i, o, c, ok := extractUsageTokens(u)
			if !ok {
				continue
			}
			hasAny = true
			if i >= 0 {
				inputTokens = i
			}
			if o >= 0 {
				outputTokens = o
			}
			if c >= 0 {
				cachedTokens = c
			}
			usage = u
		}
		if t := parsed.Get("timings"); t.Exists() {
			timings = t
			hasAny = true
		}
		if m := parsed.Get("metrics"); m.Exists() {
			responseMetrics = m
			hasAny = true
		}
	}
	var eventData bytes.Buffer
	flush := func() {
		if eventData.Len() == 0 {
			return
		}
		consume(eventData.Bytes())
		eventData.Reset()
	}
	for offset := 0; offset < len(body); {
		nl := bytes.IndexByte(body[offset:], '\n')
		var line []byte
		if nl == -1 {
			line = body[offset:]
			offset = len(body)
		} else {
			line = body[offset : offset+nl]
			offset += nl + 1
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			flush()
			continue
		}
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if eventData.Len() > 0 && gjson.ValidBytes(bytes.TrimSpace(eventData.Bytes())) {
			flush()
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			flush()
			continue
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.Write(payload)
	}
	flush()
	if !hasAny {
		return ActivityLogEntry{}, errNoValidJSONData
	}
	return buildMetrics(modelID, start, inputTokens, outputTokens, cachedTokens, usage, timings, responseMetrics), nil
}

var errNoValidJSONData = &noValidJSONDataError{}

type noValidJSONDataError struct{}

func (e *noValidJSONDataError) Error() string { return "no valid JSON data found in stream" }

func referenceStreamingSpeedTimeline(body []byte, writes []responseBodyWrite, start time.Time, outputTokens int) string {
	if len(body) == 0 || len(writes) == 0 {
		return ""
	}
	type sample struct {
		at    time.Time
		runes int
	}
	var samples []sample
	finalUsage := -1.0
	for offset := 0; offset < len(body); {
		lineStart := offset
		newline := bytes.IndexByte(body[offset:], '\n')
		if newline < 0 {
			offset = len(body)
		} else {
			offset += newline + 1
		}
		line := bytes.TrimSpace(body[lineStart:offset])
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !gjson.ValidBytes(payload) {
			continue
		}
		parsed := gjson.ParseBytes(payload)
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if usage := parsed.Get("usage.completion_tokens"); usage.Exists() && usage.Type == gjson.Number {
			finalUsage = usage.Float()
		}
		runes := 0
		for _, path := range streamingTextPaths {
			result := parsed.Get(path)
			if !result.Exists() {
				continue
			}
			if result.Type == gjson.String {
				runes += len([]rune(result.String()))
			} else {
				result.ForEach(func(_, child gjson.Result) bool {
					if child.Type == gjson.String {
						runes += len([]rune(child.String()))
					}
					return true
				})
			}
		}
		if runes == 0 {
			continue
		}
		at := writeTimeAt(writes, offset, len(samples) == 0)
		if at.IsZero() {
			continue
		}
		samples = append(samples, sample{at: at, runes: runes})
	}
	if len(samples) == 0 {
		return ""
	}
	const runesPerToken = 3.5
	points := make([][2]float64, 0, len(samples))
	cumulative := 0.0
	for _, s := range samples {
		cumulative += float64(s.runes) / runesPerToken
		points = append(points, [2]float64{float64(s.at.Sub(start).Milliseconds()), cumulative})
	}
	anchor := finalUsage
	if anchor <= 0 {
		anchor = float64(outputTokens)
	}
	if anchor > 0 && len(points) > 0 {
		if last := points[len(points)-1]; last[1] > 0 {
			scale := anchor / last[1]
			for index := range points {
				points[index][1] = roundTimelineValue(points[index][1] * scale)
			}
			points[len(points)-1][1] = anchor
		}
	}
	if len(points) > maxSpeedTimelinePoints {
		reduced := make([][2]float64, 0, maxSpeedTimelinePoints)
		last := len(points) - 1
		for index := 0; index < maxSpeedTimelinePoints; index++ {
			position := index * last / (maxSpeedTimelinePoints - 1)
			reduced = append(reduced, points[position])
		}
		points = reduced
	}
	encoded, err := json.Marshal(points)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func roundTimelineValue(value float64) float64 {
	scaled := value * 100
	if scaled >= 0 {
		return float64(int64(scaled+0.5)) / 100
	}
	return float64(int64(scaled-0.5)) / 100
}

// TestStreamScan_CompactedWriteLogHoleBounds is the hole case: once the write
// log compacts its middle, a frame landing in the gap has no exact timestamp and
// both timing consumers fall back to the neighbouring records. The merged scan
// must reproduce those bounds exactly, so a body long enough to trigger
// compaction is built here.
func TestStreamScan_CompactedWriteLogHoleBounds(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()

	// Enough frames that the write log compacts: one record per line, and the
	// compaction trigger is 1536 records.
	var frames []string
	for i := range 4000 {
		frames = append(frames, "data: "+`{"choices":[{"delta":{"content":"tok"}}]}`+"\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")
		_ = i
	}
	body := []byte(joinFrames(frames))
	writes := buildWriteLog(body, base)
	if len(writes) < responseWriteCompactAt {
		t.Fatalf("write log has %d records, want at least %d to force compaction", len(writes), responseWriteCompactAt)
	}
	// Compact the way the recorder does.
	copier := &responseBodyCopier{body: bytes.NewBuffer(body), writes: writes}
	copier.compactWriteRecords()
	if !hasHole(copier.writes) {
		t.Fatal("the write log has no compaction hole after compaction")
	}
	writes = copier.writes

	scan := scanStreamBody(body, writes, base)
	gotFirst, gotLast, gotHas := scan.contentTimes()
	wantFirst, wantLast, wantHas := referenceStreamingContentTimes(body, writes)
	if gotHas != wantHas {
		t.Fatalf("hasContent = %v, want %v", gotHas, wantHas)
	}
	if gotHas && (!gotFirst.Equal(wantFirst) || !gotLast.Equal(wantLast)) {
		t.Fatalf("content window = %v..%v, want %v..%v", gotFirst, gotLast, wantFirst, wantLast)
	}
	if !gotFirst.Equal(base) {
		t.Fatalf("first content time = %v, want the earliest bound %v", gotFirst, base)
	}
}

func hasHole(writes []responseBodyWrite) bool {
	for _, write := range writes {
		if write.hole {
			return true
		}
	}
	return false
}

// TestStreamScan_MatchesPerConsumerReference is the equivalence proof for the
// single walk: for every body the merged scan must produce the same content
// window, the same usage entry and the same speed curve the three per-consumer
// implementations produced when each scanned the body on its own.
func TestStreamScan_MatchesPerConsumerReference(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name   string
		frames []string
	}{
		{"empty", nil},
		{"no data lines", []string{"event: ping\n\n"}},
		{"one text frame", []string{`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"}},
		{
			name: "chat stream with usage",
			frames: []string{
				`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"Hello "}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"world"}}]}` + "\n\n",
				`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2}}` + "\n\n",
				"data: [DONE]\n\n",
			},
		},
		{
			name: "multiline data field",
			frames: []string{
				"data: {\"choices\":[{\"delta\":\n" + "data: {\"content\":\"split\"}}]}\n\n",
				`data: {"choices":[{"delta":{"content":" tail"}}]}` + "\n\n",
			},
		},
		{
			name: "no blank line between frames",
			frames: []string{
				`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n",
				`data: {"choices":[{"delta":{"content":"b"}}]}` + "\n",
			},
		},
		{
			name: "timings and metrics",
			frames: []string{
				`data: {"choices":[{"delta":{"content":"x"}}],"timings":{"prompt_per_second":12.5,"predicted_per_second":40}}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"y"}}],"metrics":{"tokens_per_second":41}}` + "\n\n",
			},
		},
		{
			name: "responses dialect deltas",
			frames: []string{
				`data: {"type":"response.output_text.delta","delta":"Hello "}` + "\n\n",
				`data: {"type":"response.output_text.delta","delta":"there"}` + "\n\n",
			},
		},
		{
			name: "reasoning then answer",
			frames: []string{
				`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"answer"}}]}` + "\n\n",
			},
		},
		{
			name: "malformed payload ignored",
			frames: []string{
				`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n",
				"data: {not json\n\n",
				`data: {"choices":[{"delta":{"content":"!"}}]}` + "\n\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(joinFrames(tc.frames))
			writes := buildWriteLog(body, base)

			scan := scanStreamBody(body, writes, base)
			gotFirst, gotLast, gotHas := scan.contentTimes()
			wantFirst, wantLast, wantHas := referenceStreamingContentTimes(body, writes)
			if gotHas != wantHas {
				t.Fatalf("hasContent = %v, want %v", gotHas, wantHas)
			}
			if gotHas && (!gotFirst.Equal(wantFirst) || !gotLast.Equal(wantLast)) {
				t.Fatalf("content window = %v..%v, want %v..%v", gotFirst, gotLast, wantFirst, wantLast)
			}

			gotEntry, gotErr := scan.usageEntry("m", base)
			wantEntry, wantErr := referenceProcessStreamingResponse("m", base, body)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("usage error = %v, want %v", gotErr, wantErr)
			}
			if gotErr == nil {
				// DurationMs is wall clock, so it is normalized away.
				gotEntry.DurationMs, wantEntry.DurationMs = 0, 0
				if !reflectEqualEntry(gotEntry, wantEntry) {
					t.Fatalf("usage entry mismatch\n got: %+v\nwant: %+v", gotEntry, wantEntry)
				}
			}

			// The curve is only built when the usage pass succeeded, which is
			// what record() does.
			if wantErr == nil {
				gotCurve := scan.speedTimeline(base, wantEntry.Tokens.OutputTokens)
				wantCurve := referenceStreamingSpeedTimeline(body, writes, base, wantEntry.Tokens.OutputTokens)
				if gotCurve != wantCurve {
					t.Fatalf("speed curve mismatch\n got: %s\nwant: %s", gotCurve, wantCurve)
				}
			}
		})
	}
}

func joinFrames(frames []string) string {
	var b bytes.Buffer
	for _, frame := range frames {
		b.WriteString(frame)
	}
	return b.String()
}

// buildWriteLog splits the body into per-line write records so the timing
// consumers have a realistic log to map offsets through.
func buildWriteLog(body []byte, base time.Time) []responseBodyWrite {
	var writes []responseBodyWrite
	offset := 0
	for offset < len(body) {
		newline := bytes.IndexByte(body[offset:], '\n')
		if newline < 0 {
			writes = append(writes, responseBodyWrite{end: len(body), at: base.Add(time.Duration(len(writes)) * time.Millisecond)})
			break
		}
		offset += newline + 1
		writes = append(writes, responseBodyWrite{end: offset, at: base.Add(time.Duration(len(writes)) * time.Millisecond)})
	}
	return writes
}

func reflectEqualEntry(got, want ActivityLogEntry) bool {
	got.Timestamp, want.Timestamp = time.Time{}, time.Time{}
	return fmtEntry(got) == fmtEntry(want)
}

func fmtEntry(entry ActivityLogEntry) string {
	data, err := json.Marshal(entry)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

// TestSseFrames_SplitsStreamsAndDocuments pins the frame splitter both
// telemetry consumers share: a single JSON document yields itself, and a stream
// yields its joined events.
func TestSseFrames_SplitsStreamsAndDocuments(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty", "", 0},
		{"single document", `{"usage":{"input_tokens":1}}`, 1},
		{"single document with newline", "{\"usage\":{}}\n", 1},
		{"two events", "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n", 2},
		{"done is dropped", "data: {\"a\":1}\n\ndata: [DONE]\n\n", 1},
		{"no blank line", "data: {\"a\":1}\ndata: {\"b\":2}\n", 2},
		{"comments ignored", ": keep-alive\n\ndata: {\"a\":1}\n\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sseFrames([]byte(tc.body))
			if len(got) != tc.want {
				t.Fatalf("got %d frames (%s), want %d", len(got), got, tc.want)
			}
			for _, frame := range got {
				if !json.Valid(frame) {
					t.Fatalf("frame is not valid JSON: %s", frame)
				}
			}
		})
	}
}

// TestStreamScan_HoleBoundIsUsedForLaterFrames pins the hole bound itself: an
// offset inside the compacted middle resolves to the previous record for the
// first content frame and to the hole record for later ones. Both timing
// consumers share this rule, so the merged scan inherits it.
func TestStreamScan_HoleBoundIsUsedForLaterFrames(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	// head: [0,100) at base, [100,200) at base+1s; hole record starts at 200;
	// tail: [200,300) at base+9s.
	writes := []responseBodyWrite{
		{end: 100, at: base},
		{end: 200, at: base.Add(time.Second)},
		{end: 300, at: base.Add(9 * time.Second), hole: true},
		{end: 400, at: base.Add(10 * time.Second)},
	}
	// A frame whose offset is inside the hole: the lower bound is the previous
	// record, the upper bound is the hole record.
	holeOffset := 250
	if got := writeTimeAt(writes, holeOffset, true); !got.Equal(base.Add(time.Second)) {
		t.Fatalf("first frame in hole = %v, want the lower bound %v", got, base.Add(time.Second))
	}
	if got := writeTimeAt(writes, holeOffset, false); !got.Equal(base.Add(9 * time.Second)) {
		t.Fatalf("later frame in hole = %v, want the upper bound %v", got, base.Add(9*time.Second))
	}
	// An offset inside a real record resolves to that record either way.
	if got := writeTimeAt(writes, 50, true); !got.Equal(base) {
		t.Fatalf("offset in the first record = %v, want %v", got, base)
	}
	if got := writeTimeAt(writes, 350, false); !got.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("offset in the tail record = %v, want %v", got, base.Add(10*time.Second))
	}
	if got := writeTimeAt(writes, 150, false); !got.Equal(base.Add(time.Second)) {
		t.Fatalf("offset in the second record = %v, want %v", got, base.Add(time.Second))
	}
}
