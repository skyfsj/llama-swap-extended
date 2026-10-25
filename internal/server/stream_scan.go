package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/tidwall/gjson"
)

// streamScan is one captured SSE body walked once, holding what the three
// stream consumers need: the usage/timings counters, the content-bearing frame
// offsets with their write timestamps, and the per-frame text volume the speed
// curve is built from.
//
// It exists so record() does not scan the same body three times. Each consumer
// keeps its own view of the bytes — the usage consumers read joined frames, the
// timing consumers read lines — so the values are the same as when each scanned
// the body on its own; only the number of walks changes.
type streamScan struct {
	// usage accumulation, as processStreamingResponse collects it.
	inputTokens  int64
	outputTokens int64
	// cachedTokens starts unknown, which is what the per-consumer version's
	// local did; the first reported value replaces it.
	cachedTokens int64
	hasAny       bool
	usage        gjson.Result
	timings      gjson.Result
	metrics      gjson.Result

	// contentTimes holds the timestamps of the first and last content-bearing
	// frames, as streamingContentTimes derives them.
	firstContent time.Time
	lastContent  time.Time

	// speedSamples holds the (time, text volume) pairs streamingSpeedTimeline
	// downsamples, in body order.
	speedSamples []speedSample
	// finalUsage is the last usage.completion_tokens the stream reported, which
	// anchors the curve's endpoint.
	finalUsage float64
}

type speedSample struct {
	at    time.Time
	runes int
}

// scanStreamBody walks body once and feeds the three consumers. writes and start
// are the response write log and the request start time the timing consumers map
// offsets through.
func scanStreamBody(body []byte, writes []responseBodyWrite, start time.Time) *streamScan {
	scan := &streamScan{cachedTokens: -1}
	// writeAt maps a body offset to its write timestamp. A frame whose offset
	// fell inside the compacted middle has no exact timestamp; its true time
	// lies between the previous record's time and the hole boundary. For the
	// first content frame the lower bound is used (earliest plausible), for the
	// last the upper bound, so the measured window can only come out wider —
	// never narrower — than reality. An inflated window slightly understates a
	// rate; a collapsed one produced absurd tokens/sec values.
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

	onLine := func(line sseLine) {
		if !gjson.ValidBytes(line.payload) {
			return
		}
		parsed := gjson.ParseBytes(line.payload)
		if bytes.Equal(line.payload, []byte("[DONE]")) {
			return
		}

		// The speed curve's per-frame text volume and its terminal anchor.
		if usage := parsed.Get("usage.completion_tokens"); usage.Exists() && usage.Type == gjson.Number {
			scan.finalUsage = usage.Float()
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
		if runes > 0 {
			at := writeTimeAt(writes, line.offset, len(scan.speedSamples) == 0)
			if !at.IsZero() {
				scan.speedSamples = append(scan.speedSamples, speedSample{at: at, runes: runes})
			}
		}

		// The content-time window, which counts only frames carrying visible
		// text — a role-only or whitespace-only frame must not start the decode
		// clock.
		if !streamingEventHasText(parsed) {
			return
		}
		// The first content frame takes the earliest plausible bound inside a
		// compaction hole; later frames (including the last) take the latest.
		at := writeAt(line.offset, !scan.firstContent.IsZero())
		if at.IsZero() {
			return
		}
		if scan.firstContent.IsZero() {
			scan.firstContent = at
		}
		scan.lastContent = at
	}

	onFrame := func(data []byte) {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			return
		}
		scan.applyFrame(gjson.ParseBytes(data))
	}

	scanSSEBody(body, onLine, onFrame)
	return scan
}

// applyFrame folds one parsed SSE event into the usage and timing telemetry. It
// is called with the result the line consumers already parsed when the line is a
// whole frame of its own, so a one-line-per-frame stream is parsed once.
func (s *streamScan) applyFrame(parsed gjson.Result) {
	for _, path := range usagePaths {
		u := parsed.Get(path)
		if !u.Exists() {
			continue
		}
		i, o, c, ok := extractUsageTokens(u)
		if !ok {
			continue
		}
		s.hasAny = true
		if i >= 0 {
			s.inputTokens = i
		}
		if o >= 0 {
			s.outputTokens = o
		}
		if c >= 0 {
			s.cachedTokens = c
		}
		// Remember the last usage block so buildMetrics can read the rates
		// a backend embeds there (TabbyAPI, oMLX).
		s.usage = u
	}
	if t := parsed.Get("timings"); t.Exists() {
		s.timings = t
		s.hasAny = true
	}
	if m := parsed.Get("metrics"); m.Exists() {
		s.metrics = m
		s.hasAny = true
	}
}

// contentTimes reports the timestamps of the first and last content-bearing
// frames, and whether the stream carried any at all.
func (s *streamScan) contentTimes() (time.Time, time.Time, bool) {
	if s.firstContent.IsZero() || s.lastContent.Before(s.firstContent) {
		return time.Time{}, time.Time{}, false
	}
	return s.firstContent, s.lastContent, true
}

// usageEntry builds the activity entry from the usage and timing telemetry the
// scan collected, exactly as processStreamingResponse did from its own pass.
func (s *streamScan) usageEntry(modelID string, start time.Time) (ActivityLogEntry, error) {
	if !s.hasAny {
		return ActivityLogEntry{}, fmt.Errorf("no valid JSON data found in stream")
	}
	return buildMetrics(modelID, start, s.inputTokens, s.outputTokens, s.cachedTokens, s.usage, s.timings, s.metrics), nil
}

// speedTimeline renders the per-request decode curve from the scan's samples,
// anchored to the streamed token count when it reported one.
func (s *streamScan) speedTimeline(start time.Time, outputTokens int) string {
	if len(s.speedSamples) == 0 {
		return ""
	}
	return renderSpeedTimeline(s.speedSamples, s.finalUsage, outputTokens, start)
}

// renderSpeedTimeline is the rendering half of streamingSpeedTimeline, kept
// separate so the sampling and the drawing can be tested independently.
func renderSpeedTimeline(samples []speedSample, finalUsage float64, outputTokens int, start time.Time) string {
	if len(samples) == 0 {
		return ""
	}
	const runesPerToken = 3.5
	points := make([][2]float64, 0, len(samples))
	cumulative := 0.0
	for _, sample := range samples {
		cumulative += float64(sample.runes) / runesPerToken
		points = append(points, [2]float64{float64(sample.at.Sub(start).Milliseconds()), cumulative})
	}

	// Anchor the endpoint to the authoritative token count: the streamed
	// final usage frame when present, otherwise the recorded output tokens.
	anchor := finalUsage
	if anchor <= 0 {
		anchor = float64(outputTokens)
	}
	if anchor > 0 && len(points) > 0 {
		if last := points[len(points)-1]; last[1] > 0 {
			scale := anchor / last[1]
			for index := range points {
				// Two decimals keep the stored JSON free of float noise like
				// 1.0000000000000002 without losing curve resolution.
				points[index][1] = math.Round(points[index][1]*scale*100) / 100
			}
			points[len(points)-1][1] = anchor
		}
	}

	// Downsample evenly, always keeping the first and last points.
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
