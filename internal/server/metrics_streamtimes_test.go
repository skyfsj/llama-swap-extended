package server

import (
	"testing"
	"time"
)

// A stream whose first 600 writes carry no text and whose content frames all
// land in the tail segment used to collapse the measured content window to a
// couple of milliseconds once the write log compacted, reporting hundreds of
// thousands of tokens/sec. The hole-aware bounds must keep the window wide.
func TestStreamingContentTimes_HoleDoesNotCollapseWindow(t *testing.T) {
	var body []byte
	var writes []responseBodyWrite
	start := time.Now()
	addWrite := func(payload string) {
		body = append(body, payload...)
		writes = append(writes, responseBodyWrite{end: len(body), at: start.Add(time.Duration(len(writes)) * 100 * time.Millisecond)})
	}
	for i := 0; i < 600; i++ {
		addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	}
	for i := 0; i < 1000; i++ {
		addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	// Compact the same way the copier does.
	kept := make([]responseBodyWrite, 0, responseWriteHeadRecords+responseWriteTailRecords)
	kept = append(kept, writes[:responseWriteHeadRecords]...)
	for i := range kept {
		kept[i].hole = false
	}
	tail := append([]responseBodyWrite(nil), writes[len(writes)-responseWriteTailRecords:]...)
	tail[0].hole = true
	kept = append(kept, tail...)
	writes = kept

	first, last, ok := streamingContentTimes(body, writes)
	if !ok {
		t.Fatal("no content times found")
	}
	span := last.Sub(first)
	// Content starts at write #600 (60s) and ends at write #1599 (~159.9s).
	// The first content frame falls in the hole; the lower bound is the last
	// head record (51.2s), so the span must be at least ~100s wide.
	if span < 95*time.Second {
		t.Fatalf("content window collapsed: %v (expect >= 100s)", span)
	}
	if span > 110*time.Second {
		t.Fatalf("content window implausibly wide: %v", span)
	}
}

// Without compaction the bounds are exact.
func TestStreamingContentTimes_ExactWithoutCompaction(t *testing.T) {
	var body []byte
	var writes []responseBodyWrite
	start := time.Now()
	addWrite := func(payload string) {
		body = append(body, payload...)
		writes = append(writes, responseBodyWrite{end: len(body), at: start.Add(time.Duration(len(writes)) * 10 * time.Millisecond)})
	}
	addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
	addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n")
	addWrite("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"}}]}\n\n")

	first, last, ok := streamingContentTimes(body, writes)
	if !ok {
		t.Fatal("no content times found")
	}
	if got := first.Sub(start); got != 10*time.Millisecond {
		t.Fatalf("first content at %v, want 10ms", got)
	}
	if got := last.Sub(start); got != 30*time.Millisecond {
		t.Fatalf("last content at %v, want 30ms", got)
	}
}
