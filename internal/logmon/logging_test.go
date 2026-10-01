package logmon

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogMonitor(t *testing.T) {
	logMonitor := NewWriter(io.Discard)

	var wg sync.WaitGroup

	client1Messages := make([]byte, 0)
	client2Messages := make([]byte, 0)

	defer logMonitor.OnLogData(func(data []byte) {
		client1Messages = append(client1Messages, data...)
		wg.Done()
	})()

	defer logMonitor.OnLogData(func(data []byte) {
		client2Messages = append(client2Messages, data...)
		wg.Done()
	})()

	wg.Add(6) // 2 x 3 writes

	logMonitor.Write([]byte("1"))
	logMonitor.Write([]byte("2"))
	logMonitor.Write([]byte("3"))

	wg.Wait()

	expectedHistory := "123"
	history := string(logMonitor.GetHistory())

	if history != expectedHistory {
		t.Errorf("Expected history: %s, got: %s", expectedHistory, history)
	}

	c1Data := string(client1Messages)
	if c1Data != expectedHistory {
		t.Errorf("Client1 expected %s, got: %s", expectedHistory, c1Data)
	}

	c2Data := string(client2Messages)
	if c2Data != expectedHistory {
		t.Errorf("Client2 expected %s, got: %s", expectedHistory, c2Data)
	}
}

func TestWrite_ImmutableBuffer(t *testing.T) {
	lm := NewWriter(io.Discard)

	msg := []byte("Hello, World!")
	lenmsg := len(msg)

	n, err := lm.Write(msg)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != lenmsg {
		t.Errorf("Expected %d bytes written but got %d", lenmsg, n)
	}

	msg[0] = 'B'

	history := lm.GetHistory()

	expected := []byte("Hello, World!")
	if !bytes.Equal(history, expected) {
		t.Errorf("Expected history to be %q, got %q", expected, history)
	}
}

func TestWrite_LogTimeFormat(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.timeFormat = time.RFC3339

	lm.Info("Hello, World!")

	history := lm.GetHistory()

	timestamp := ""
	fields := strings.Fields(string(history))
	if len(fields) > 0 {
		timestamp = fields[0]
	} else {
		t.Fatalf("Cannot extract string from history")
	}

	_, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		t.Fatalf("Cannot find timestamp: %v", err)
	}
}

func TestCircularBuffer_WrapAround(t *testing.T) {
	cb := newCircularBuffer(10)

	cb.Write([]byte("hello"))
	if got := string(cb.GetHistory()); got != "hello" {
		t.Errorf("Expected 'hello', got %q", got)
	}

	cb.Write([]byte("world"))
	if got := string(cb.GetHistory()); got != "helloworld" {
		t.Errorf("Expected 'helloworld', got %q", got)
	}

	cb.Write([]byte("12345"))
	if got := string(cb.GetHistory()); got != "world12345" {
		t.Errorf("Expected 'world12345', got %q", got)
	}

	cb.Write([]byte("abcdefghijklmnop"))
	if got := string(cb.GetHistory()); got != "ghijklmnop" {
		t.Errorf("Expected 'ghijklmnop', got %q", got)
	}
}

func TestCircularBuffer_BoundaryConditions(t *testing.T) {
	cb := newCircularBuffer(10)
	if got := cb.GetHistory(); got != nil {
		t.Errorf("Expected nil for empty buffer, got %q", got)
	}

	cb.Write([]byte("1234567890"))
	if got := string(cb.GetHistory()); got != "1234567890" {
		t.Errorf("Expected '1234567890', got %q", got)
	}

	cb = newCircularBuffer(10)
	cb.Write([]byte("12345"))
	cb.Write([]byte("67890"))
	if got := string(cb.GetHistory()); got != "1234567890" {
		t.Errorf("Expected '1234567890', got %q", got)
	}
}

func TestLogMonitor_LazyInit(t *testing.T) {
	lm := NewWriter(io.Discard)

	if lm.buffer != nil {
		t.Error("Expected buffer to be nil before first write")
	}

	if got := lm.GetHistory(); got != nil {
		t.Errorf("Expected nil history before first write, got %q", got)
	}

	lm.Write([]byte("test"))

	if lm.buffer == nil {
		t.Error("Expected buffer to be initialized after write")
	}

	if got := string(lm.GetHistory()); got != "test" {
		t.Errorf("Expected 'test', got %q", got)
	}
}

func TestLogMonitor_Clear(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Write([]byte("hello"))
	if got := string(lm.GetHistory()); got != "hello" {
		t.Errorf("Expected 'hello', got %q", got)
	}

	lm.Clear()

	if lm.buffer != nil {
		t.Error("Expected buffer to be nil after Clear")
	}

	if got := lm.GetHistory(); got != nil {
		t.Errorf("Expected nil history after Clear, got %q", got)
	}
}

func TestLogMonitor_ClearAndReuse(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Write([]byte("first"))
	lm.Clear()
	lm.Write([]byte("second"))

	if got := string(lm.GetHistory()); got != "second" {
		t.Errorf("Expected 'second' after clear and reuse, got %q", got)
	}
}

// TestLogMonitor_DropsWhenSubscriberBlocked verifies that a stalled subscriber
// can never block Write (the upstream process's stdout drain) and that dropped
// data is reported in-stream with a marker once delivery resumes. See #875.
func TestLogMonitor_DropsWhenSubscriberBlocked(t *testing.T) {
	lm := NewWriter(io.Discard)

	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var received [][]byte

	cancel := lm.OnLogData(func(data []byte) {
		// Block the first delivery, stalling the broadcaster goroutine so the
		// hand-off channel and event queue fill and subsequent writes drop.
		once.Do(func() { <-release })
		mu.Lock()
		received = append(received, append([]byte(nil), data...))
		mu.Unlock()
	})
	defer cancel()

	// Flood well past the hand-off channel (1024) + event queue (1000)
	// capacity. None of these writes may block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4000; i++ {
			lm.Write([]byte("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked while subscriber was stalled")
	}

	close(release)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, d := range received {
			if strings.Contains(string(d), "bytes dropped") {
				found = true
				break
			}
		}
		mu.Unlock()
		if found {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected a 'bytes dropped' marker after resuming delivery")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestLogMonitor_Close(t *testing.T) {
	lm := NewWriter(io.Discard)

	received := make(chan []byte, 16)
	cancel := lm.OnLogData(func(data []byte) {
		received <- data
	})
	defer cancel()

	lm.Write([]byte("before close"))
	select {
	case data := <-received:
		if string(data) != "before close" {
			t.Fatalf("received %q before close", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the pre-close write")
	}

	lm.Close()
	if !lm.IsClosed() {
		t.Fatal("IsClosed() = false after Close")
	}
	// Idempotent: a second Close must not panic.
	lm.Close()

	lm.Write([]byte("after close"))
	select {
	case data := <-received:
		t.Fatalf("closed monitor broadcast %q", data)
	case <-time.After(100 * time.Millisecond):
	}

	// History (including post-close writes) stays readable for late callers
	// such as crash archives and reconnecting clients.
	history := string(lm.GetHistory())
	if !strings.Contains(history, "before close") || !strings.Contains(history, "after close") {
		t.Fatalf("history after close = %q, want both pre- and post-close writes", history)
	}
}

func TestLogMonitor_SeedHistory(t *testing.T) {
	lm := NewWriter(io.Discard)
	lm.SeedHistory([]byte("inherited tail"))

	if got := string(lm.GetHistory()); got != "inherited tail" {
		t.Fatalf("GetHistory after seed = %q", got)
	}

	received := make(chan []byte, 16)
	cancel := lm.OnLogData(func(data []byte) {
		received <- data
	})
	defer cancel()

	// Live writes still broadcast; the seeded history must not be replayed
	// to subscribers nor forwarded through the stdout sink a second time.
	lm.Write([]byte("live line"))
	select {
	case data := <-received:
		if string(data) != "live line" {
			t.Fatalf("subscriber received %q, want only the live write", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the live write")
	}
	if got := string(lm.GetHistory()); !strings.Contains(got, "inherited tail") || !strings.Contains(got, "live line") {
		t.Fatalf("history = %q, want seed followed by live write", got)
	}
}

func BenchmarkLogMonitorWrite(b *testing.B) {
	smallMsg := []byte("small message\n")
	mediumMsg := []byte(strings.Repeat("medium message content ", 10) + "\n")
	largeMsg := []byte(strings.Repeat("large message content for benchmarking ", 100) + "\n")

	b.Run("SmallWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(smallMsg)
		}
	})

	b.Run("MediumWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(mediumMsg)
		}
	})

	b.Run("LargeWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(largeMsg)
		}
	})

	b.Run("WithSubscribers", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		for i := 0; i < 5; i++ {
			lm.OnLogData(func(data []byte) {})
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(mediumMsg)
		}
	})

	b.Run("GetHistory", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		for i := 0; i < 1000; i++ {
			lm.Write(mediumMsg)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.GetHistory()
		}
	})
}

// TestLogMonitor_StartsFreshLineAfterUnterminatedWrite covers the display bug an
// operator hit when a model died mid-line: a progress write left the buffer
// without a trailing newline, and the lifecycle diagnostic was appended to the
// end of that partial line instead of starting its own.
func TestLogMonitor_StartsFreshLineAfterUnterminatedWrite(t *testing.T) {
	lm := NewWriter(io.Discard)

	// An upstream that stops mid-line, exactly like a killed warmup progress
	// line: no terminating newline.
	lm.Write([]byte("SM70 long-prefill op reached tokens="))
	lm.Error("process exited before becoming ready: exited with code 120")

	history := string(lm.GetHistory())
	lines := strings.Split(strings.TrimRight(history, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("history has %d lines, want 2 (partial line + diagnostic):\n%q", len(lines), history)
	}
	if !strings.HasSuffix(lines[0], "tokens=") {
		t.Fatalf("first line = %q, want the unterminated upstream fragment", lines[0])
	}
	if !strings.Contains(lines[1], "process exited before becoming ready") {
		t.Fatalf("second line = %q, want the diagnostic on its own line", lines[1])
	}
}

// TestLogMonitor_NoBlankLineAfterTerminatedWrite keeps the other side: when the
// upstream did end its line, the diagnostic must not introduce a stray blank
// line.
func TestLogMonitor_NoBlankLineAfterTerminatedWrite(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Write([]byte("normal line\n"))
	lm.Error("process exited unexpectedly")

	history := string(lm.GetHistory())
	if strings.Contains(history, "\n\n") {
		t.Fatalf("history contains a blank line:\n%q", history)
	}
	if !strings.Contains(history, "normal line\n") {
		t.Fatalf("history lost its first line: %q", history)
	}
}

// TestLogMonitor_FirstMessageDoesNotLeadWithNewline pins the empty-buffer case:
// a monitor that has written nothing yet must not open with a newline.
func TestLogMonitor_FirstMessageDoesNotLeadWithNewline(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Error("first message")

	history := string(lm.GetHistory())
	if strings.HasPrefix(history, "\n") {
		t.Fatalf("history starts with a newline: %q", history)
	}
}
