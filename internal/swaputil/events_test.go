package swaputil

import (
	"math"
	"strings"
	"testing"
)

func TestBackendProgressEventNormalizeBoundsAndClamps(t *testing.T) {
	event, ok := (BackendProgressEvent{
		Model:     "  model-1  ",
		Phase:     strings.Repeat("p", 100),
		Progress:  math.Inf(1),
		Completed: 50,
		Total:     40,
		Message:   strings.Repeat("消息", 2000),
		Error:     "  failed  ",
	}).Normalize()
	if !ok {
		t.Fatal("addressable progress event was dropped")
	}
	if event.Model != "model-1" || len([]rune(event.Phase)) != 64 {
		t.Fatalf("normalized identity/phase = model=%q phase length=%d", event.Model, len([]rune(event.Phase)))
	}
	if event.Progress != 0 {
		t.Fatalf("non-finite progress = %v, want 0", event.Progress)
	}
	if len(event.Message) > 2048 || event.Message != strings.TrimSpace(event.Message) {
		t.Fatalf("message was not bounded: bytes=%d", len(event.Message))
	}
	if event.Error != "failed" {
		t.Fatalf("error = %q, want trimmed value", event.Error)
	}
	if event.Completed != 40 || event.Total != 40 {
		t.Fatalf("byte counters = %d/%d, want clamped 40/40", event.Completed, event.Total)
	}

	event, ok = (BackendProgressEvent{Runtime: "runtime", Progress: 3, Phase: ""}).Normalize()
	if !ok || event.Phase != "unknown" || event.Progress != 1 {
		t.Fatalf("default/clamped event = %+v, ok=%v", event, ok)
	}
}

func TestBackendProgressEventNormalizePreservesUnknownTotal(t *testing.T) {
	event, ok := (BackendProgressEvent{Model: "model", Phase: "downloading", Completed: 12, Total: -1}).Normalize()
	if !ok {
		t.Fatal("addressable event was dropped")
	}
	if event.Completed != 12 || event.Total != -1 {
		t.Fatalf("unknown total counters = %d/%d, want 12/-1", event.Completed, event.Total)
	}
	malformed, ok := (BackendProgressEvent{Model: "model", Phase: "downloading", Completed: -3, Total: -9}).Normalize()
	if !ok || malformed.Completed != 0 || malformed.Total != -1 {
		t.Fatalf("malformed counters = %+v, want 0/-1", malformed)
	}
}

func TestBackendProgressEventNormalizeDropsUnaddressable(t *testing.T) {
	if _, ok := (BackendProgressEvent{Phase: "loading", Progress: 0.5}).Normalize(); ok {
		t.Fatal("progress without model or runtime was accepted")
	}
}

func TestBackendProgressEventNormalizeRejectsInvisibleFields(t *testing.T) {
	if _, ok := (BackendProgressEvent{Model: "model\u200b", Phase: "loading"}).Normalize(); ok {
		t.Fatal("model containing a zero-width format character was accepted")
	}
	event, ok := (BackendProgressEvent{Model: "model", Phase: "\tloading", Message: "bad\nmessage", Error: "bad\u202Eerror"}).Normalize()
	if !ok {
		t.Fatal("addressable event was dropped because only diagnostics were invalid")
	}
	if event.Phase != "unknown" || event.Message != "" || event.Error != "" {
		t.Fatalf("invisible fields were retained: %+v", event)
	}
}

func TestBackendProgressEventNormalizePreservesBoundedCommandOutput(t *testing.T) {
	event, ok := (BackendProgressEvent{
		Runtime:      "vllm",
		OperationID:  "op-123",
		Phase:        "log",
		OutputStream: "stderr",
		Output:       "compile\nwarning: slow\x00\n",
	}).Normalize()
	if !ok {
		t.Fatal("runtime output event was dropped")
	}
	if event.Output != "compile\nwarning: slow\n" || event.OutputStream != "stderr" || event.OperationID != "op-123" {
		t.Fatalf("normalized command output = %+v", event)
	}
	large := strings.Repeat("x", 40<<10)
	event, ok = (BackendProgressEvent{Runtime: "vllm", Output: large}).Normalize()
	if !ok || len(event.Output) > 32<<10 {
		t.Fatalf("command output was not bounded: ok=%v bytes=%d", ok, len(event.Output))
	}
}

// truncateProgressString used to re-encode the whole remaining string per
// dropped rune: trimming a 256KB tail to 32KB re-encoded gigabytes and
// stalled the calling handler for minutes. The large input below completes
// instantly with the linear implementation.
func TestBackendProgressEventTruncateLargeTail(t *testing.T) {
	value := strings.Repeat("a", 100<<10)
	got := truncateProgressString(value, 32<<10)
	if len(got) != 32<<10 {
		t.Fatalf("truncated length = %d, want %d", len(got), 32<<10)
	}
	if got != value[:32<<10] {
		t.Fatal("truncation did not keep the head")
	}

	// Rune-aware: a multibyte tail is dropped whole, never split.
	multi := strings.Repeat("é", 4096) // 2 bytes per rune
	got = truncateProgressString(multi, 8192)
	if len(got) != 8192 {
		t.Fatalf("multibyte truncated length = %d, want 8192", len(got))
	}
}
