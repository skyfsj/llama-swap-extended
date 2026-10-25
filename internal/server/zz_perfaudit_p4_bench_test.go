package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkStreamScan_1MiBStream measures the single walk that now feeds all
// three stream consumers, against the three separate walks the recorder used to
// perform. The body is a realistic 1 MiB chat stream.
func BenchmarkStreamScan_1MiBStream(b *testing.B) {
	base := time.Unix(1_700_000_000, 0).UTC()
	body := buildBenchmarkStream(1 << 20)
	writes := buildWriteLog(body, base)

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scan := scanStreamBody(body, writes, base)
		_, _, _ = scan.contentTimes()
		entry, err := scan.usageEntry("m", base)
		if err != nil {
			b.Fatal(err)
		}
		_ = scan.speedTimeline(base, entry.Tokens.OutputTokens)
	}
}

// BenchmarkStreamScan_ReferencePerConsumer is the same body through the three
// per-consumer implementations, so the two can be compared directly.
func BenchmarkStreamScan_ReferencePerConsumer(b *testing.B) {
	base := time.Unix(1_700_000_000, 0).UTC()
	body := buildBenchmarkStream(1 << 20)
	writes := buildWriteLog(body, base)

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = referenceStreamingContentTimes(body, writes)
		entry, err := referenceProcessStreamingResponse("m", base, body)
		if err != nil {
			b.Fatal(err)
		}
		_ = referenceStreamingSpeedTimeline(body, writes, base, entry.Tokens.OutputTokens)
	}
}

// buildBenchmarkStream renders a chat stream of roughly the requested size.
func buildBenchmarkStream(size int) []byte {
	var b strings.Builder
	written := 0
	for i := 0; written < size; i++ {
		frame := fmt.Sprintf("data: {\"id\":\"c%d\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token %d \"}}]}\n\n", i, i)
		b.WriteString(frame)
		written += len(frame)
	}
	b.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":128,\"completion_tokens\":512}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}
