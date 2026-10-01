package logmon

import (
	"bytes"
	"strings"
	"testing"
)

func TestLinePrefixWriter(t *testing.T) {
	t.Run("prefixes every complete line", func(t *testing.T) {
		var next bytes.Buffer
		w := NewLinePrefixWriter("[m1] ", &next)
		w.Write([]byte("line one\nline two\n"))
		if got := next.String(); got != "[m1] line one\n[m1] line two\n" {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("carries incomplete lines across writes", func(t *testing.T) {
		var next bytes.Buffer
		w := NewLinePrefixWriter("[m1] ", &next)
		w.Write([]byte("half "))
		w.Write([]byte("line\nnext\n"))
		if got := next.String(); got != "[m1] half line\n[m1] next\n" {
			t.Fatalf("output = %q", got)
		}
		if len(w.carry) != 0 {
			t.Fatalf("carry = %q, want empty", w.carry)
		}
	})

	t.Run("flushes an oversized fragment as its own line", func(t *testing.T) {
		var next bytes.Buffer
		w := NewLinePrefixWriter("[m1] ", &next)
		big := strings.Repeat("x", linePrefixCarryLimit+1)
		w.Write([]byte(big))
		if !strings.HasSuffix(next.String(), big+"\n") {
			t.Fatalf("oversized fragment not flushed: %d bytes", next.Len())
		}
		if len(w.carry) != 0 {
			t.Fatalf("carry = %d bytes, want empty", len(w.carry))
		}
	})

	t.Run("keeps the drain alive when the next writer fails", func(t *testing.T) {
		w := NewLinePrefixWriter("[m1] ", failingWriter{})
		if n, err := w.Write([]byte("line\n")); err != nil || n != 5 {
			t.Fatalf("write = %d, %v; want 5, nil (drain must not break)", n, err)
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = errorString("write failed")

type errorString string

func (e errorString) Error() string { return string(e) }
