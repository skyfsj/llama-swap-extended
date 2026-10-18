package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorker_ResponseIsNeverLostToProcessExit is a regression for a race that
// showed up only under load: the worker writes its terminal response and exits,
// and the parent's process-exit path could win the select before the pump
// delivered that line. The old fallback accepted a response only if it carried
// an error, so a *successful* hook was reported as "extension worker ended
// without a response" - a finished result thrown away.
//
// The seam below makes the race deterministic rather than timing-dependent: it
// holds an already-read response on the channel while the process exits, which is
// precisely the state the fallback got wrong. Without the seam this test passed
// ten times in a row on the pre-fix code.
func TestWorker_ResponseIsNeverLostToProcessExit(t *testing.T) {
	// Hold every response back until the process has exited, then release it.
	delay := func(source <-chan workerResponse) <-chan workerResponse {
		held := make(chan workerResponse, 1)
		go func() {
			value, ok := <-source
			if !ok {
				close(held)
				return
			}
			time.Sleep(400 * time.Millisecond)
			held <- value
			close(held)
		}()
		return held
	}
	root := t.TempDir()
	dir := filepath.Join(root, "exiting")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A hook that returns immediately and a hook that works hard first: both
	// exit right after writing the response.
	source := `export default {
  async onRequest(ctx, request) {
    if (request.spin) {
      let total = 0;
      for (let index = 0; index < 200000; index += 1) {
        total += index % 7;
      }
      request.spun = total > 0;
    }
    request.handled = true;
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: exiting\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("exiting")
	if !ok {
		t.Fatalf("no compile: %+v", m.List())
	}
	// Installed only now: compilation itself runs a hook, and holding that
	// response back would make the extension fail to compile rather than test
	// the drain.
	responseDelayHook.Store(&delay)
	t.Cleanup(func() { responseDelayHook.Store(nil) })
	for round := 0; round < 12; round++ {
		input := json.RawMessage(`{"model":"m"}`)
		if round%2 == 1 {
			input = json.RawMessage(`{"model":"m","spin":true}`)
		}
		out, _, err := item.Invoke(context.Background(), "onRequest", Context{}, input)
		if err != nil {
			t.Fatalf("round %d lost the response: %v", round, err)
		}
		var result map[string]any
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatalf("round %d output %q: %v", round, string(out), err)
		}
		if result["handled"] != true {
			t.Fatalf("round %d result = %+v", round, result)
		}
	}
}

// TestWorker_OutputAboveTheProtocolLineIsReportedAsASizeLimit pins the failure a
// hook meets when its result exceeds the line the worker writes: the pump's
// scanner stops mid-token while the worker is blocked writing, so waiting for the
// process ends at the hook's deadline. The caller has to see a size error instead
// of a timeout it cannot act on.
func TestWorker_OutputAboveTheProtocolLineIsReportedAsASizeLimit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hugeout")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: hugeout\nenabled: true\ntimeout: 5s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { async onRequest(ctx, request) { request.blob = "x".repeat(9 * 1024 * 1024); return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("hugeout")
	if !ok {
		t.Fatal("no compile")
	}
	started := time.Now()
	_, _, err = item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a 9 MiB result was accepted")
	}
	if !errors.Is(err, ErrHookOutputTooLarge) {
		t.Fatalf("error = %v, want ErrHookOutputTooLarge", err)
	}
	if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "context") {
		t.Fatalf("an oversized result was reported as a timeout: %v", err)
	}
	// It has to come back well before the 5s budget, not at the deadline.
	if elapsed > 3*time.Second {
		t.Fatalf("the size limit took %s to report; it should be immediate", elapsed)
	}
}

// TestWorker_OversizedOutputWinsOverAShortDeadline pins the interaction the
// race fix is about: a hook with a tight budget that returns a result past the
// protocol line has to be reported as a size limit, not as a deadline. The
// diagnosis is written by the pump goroutine and read by the run loop, so the
// pair is also what -race looks at.
func TestWorker_OversizedOutputWinsOverAShortDeadline(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tightout")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: tightout\nenabled: true\ntimeout: 5s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { async onRequest(ctx, request) { request.blob = "x".repeat(9 * 1024 * 1024); return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := m.Compiled("tightout")
	if !ok {
		t.Fatal("no compile")
	}
	for round := 0; round < 3; round++ {
		// The budget has to be long enough for the worker to write past the line
		// (that is what triggers the diagnosis) but the assertion is that the
		// answer arrives well before the budget ends: a size limit is not a
		// timeout waiting to happen. A budget that is too tight would have the
		// deadline kill the worker mid-write, which is a different (and
		// legitimate) outcome.
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _, err := item.Invoke(ctx, "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
		cancel()
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("a 9 MiB result was accepted")
		}
		if !errors.Is(err, ErrHookOutputTooLarge) {
			t.Fatalf("round %d: error = %v after %s, want the size limit", round, err, elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("round %d: the size limit took %s; it should not wait for the budget", round, elapsed)
		}
	}
}
