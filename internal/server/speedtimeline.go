package server

import (
	"bytes"
	"encoding/json"
	"math"
	"time"

	"github.com/tidwall/gjson"
)

// maxSpeedTimelinePoints bounds the per-request speed curve stored with the
// activity row. 120 samples at ~15 bytes each keep the row small while the
// curve still shows acceleration, stalls and cache-hit cliffs.
const maxSpeedTimelinePoints = 120

// streamingSpeedTimeline samples the streamed response into a bounded
// [msSinceStart, cumulativeTokens] curve for the per-request detail view.
//
// Per-frame token counts are usually unavailable mid-stream (vLLM only
// guarantees a final usage frame), so the curve accumulates visible text
// runes as a proportional estimate and is then scaled so its last point
// equals the authoritative output token count — the shape carries the
// acceleration and stalls, the endpoint carries the truth. Frame times use
// the same conservative hole bounds as StreamingContentTimes: the first
// point takes the earliest plausible time, later points the latest, so the
// curve can only stretch — never fold back on itself.
func streamingSpeedTimeline(body []byte, writes []responseBodyWrite, start time.Time, outputTokens int) string {
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

// writeTimeAt maps a body offset to its write timestamp with the same hole
// semantics as streamingContentTimes: offsets inside the compacted middle use
// the lower bound (earliest) for the first content frame and the upper bound
// for later ones, so timestamps stay monotonic across the hole.
func writeTimeAt(writes []responseBodyWrite, offset int, first bool) time.Time {
	for index, write := range writes {
		if offset > write.end {
			continue
		}
		if write.hole && index > 0 {
			if first {
				return writes[index-1].at
			}
			return write.at
		}
		return write.at
	}
	return time.Time{}
}
