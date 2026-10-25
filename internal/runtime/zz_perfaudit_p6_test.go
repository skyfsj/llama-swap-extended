package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// referenceReadOperations is the forward reader the tail window replaced. It
// parses every record of the (bounded) journal and keeps the last 100, so a
// journal that fits inside the window must produce the same result.
func referenceReadOperations(t *testing.T, path string) []Operation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var operations []Operation
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var operation Operation
		if err := json.Unmarshal([]byte(trimmed), &operation); err != nil {
			continue
		}
		operations = append(operations, operation)
	}
	if len(operations) > 100 {
		operations = operations[len(operations)-100:]
	}
	return operations
}

func writeJournal(t *testing.T, dir string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "operations.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	return path
}

func operationLine(index int) string {
	return fmt.Sprintf(`{"action":"stage","state":"active","version":"v%d"}`, index)
}

// TestReadOperations_TailMatchesForward pins the tail window against the
// forward reader: for every journal shape that fits in the window the last 100
// records must come back in file order, exactly as before.
func TestReadOperations_TailMatchesForward(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
	}{
		{"empty", nil},
		{"single", []string{operationLine(1)}},
		{"no trailing newline", []string{operationLine(1), operationLine(2)}},
		{"trailing newline", []string{operationLine(1) + "\n"}},
		{"blank lines", []string{"", operationLine(1), "", operationLine(2), ""}},
		{"exactly 100", lines(1, 100)},
		{"101 keeps the last 100", lines(1, 101)},
		{"500 keeps the last 100", lines(1, 500)},
		{"large records", []string{
			fmt.Sprintf(`{"action":"stage","state":"active","version":"%s"}`, strings.Repeat("x", 4096)),
			operationLine(2),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeJournal(t, dir, tc.lines)
			got, err := readOperations(path)
			if err != nil {
				t.Fatalf("readOperations: %v", err)
			}
			want := referenceReadOperations(t, path)
			if len(got) != len(want) {
				t.Fatalf("got %d operations, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("operation %d = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}
}

func lines(from, to int) []string {
	out := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, operationLine(i))
	}
	return out
}

// TestReadOperations_TailWindowIsBounded is the performance property: a journal
// far larger than the window must not be parsed in full, yet the returned
// records must still be the journal's last ones.
func TestReadOperations_TailWindowIsBounded(t *testing.T) {
	dir := t.TempDir()
	// Well past the 8 MiB window: each record is ~60 bytes, so this needs
	// hundreds of thousands of records to cross the threshold.
	var builder strings.Builder
	const total = 200_000
	for i := 1; i <= total; i++ {
		builder.WriteString(operationLine(i))
		builder.WriteString("\n")
	}
	path := filepath.Join(dir, "operations.jsonl")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() <= 8<<20 {
		t.Fatalf("journal is %d bytes, want more than the 8 MiB window", info.Size())
	}

	got, err := readOperations(path)
	if err != nil {
		t.Fatalf("readOperations: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("got %d operations, want 100", len(got))
	}
	for index, operation := range got {
		want := fmt.Sprintf("v%d", total-100+index+1)
		if operation.Version != want {
			t.Fatalf("operation %d version = %q, want %q", index, operation.Version, want)
		}
	}
}

// TestReadOperations_MalformedTerminatedLineIsCorruption keeps the error
// contract: a record that is not valid JSON and is followed by another record is
// reported as corruption, while an unterminated final record is ignored.
func TestReadOperations_MalformedTerminatedLineIsCorruption(t *testing.T) {
	t.Run("terminated", func(t *testing.T) {
		dir := t.TempDir()
		path := writeJournal(t, dir, []string{"{not json}", operationLine(2)})
		if _, err := readOperations(path); err == nil {
			t.Fatal("a malformed terminated record must be reported as corruption")
		}
	})
	t.Run("unterminated final", func(t *testing.T) {
		dir := t.TempDir()
		path := writeJournal(t, dir, []string{operationLine(1), "{not json"})
		got, err := readOperations(path)
		if err != nil {
			t.Fatalf("an unterminated final record is incomplete, not corrupt: %v", err)
		}
		if len(got) != 1 || got[0].Version != "v1" {
			t.Fatalf("operations = %+v, want only v1", got)
		}
	})
}

// TestReadOperations_MissingFile keeps the "no journal yet" contract.
func TestReadOperations_MissingFile(t *testing.T) {
	got, err := readOperations(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("readOperations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d operations, want 0", len(got))
	}
}

// TestReadOperations_BlockAlignedWindow covers the window arithmetic: the
// window starts mid-record, so its first partial line must be dropped and the
// records after it kept in order.
func TestReadOperations_BlockAlignedWindow(t *testing.T) {
	dir := t.TempDir()
	// Enough records to span several 64 KiB blocks, with a record deliberately
	// straddling the window boundary at the end.
	all := lines(1, 5000)
	path := writeJournal(t, dir, all)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() <= 8<<20 {
		t.Skip("journal is smaller than the window, so the window path is not used")
	}
	got, err := readOperations(path)
	if err != nil {
		t.Fatalf("readOperations: %v", err)
	}
	want := referenceReadOperations(t, path)
	if len(got) != len(want) {
		t.Fatalf("got %d operations, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("operation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
