package process

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestProcessCommand_ParseLoadingProgress(t *testing.T) {
	f := func(v float64) *float64 { return &v }

	tests := []struct {
		name   string
		body   string
		want   LoadingProgress
		wantOK bool
	}{
		{"progress and message", `{"progress":0.25,"message":"loading tensors"}`, LoadingProgress{Progress: f(0.25), Message: "loading tensors"}, true},
		{"progress only", `{"progress":0}`, LoadingProgress{Progress: f(0)}, true},
		{"message only", `{"message":"  warming up  "}`, LoadingProgress{Message: "warming up"}, true},
		{"progress clamped high", `{"progress":7}`, LoadingProgress{Progress: f(1)}, true},
		{"progress clamped low", `{"progress":-1}`, LoadingProgress{Progress: f(0)}, true},
		{"kyojin loading body", `{"status":"loading","source":"kyojin","message":"loading weights, 42% (stage 2 of 4)","progress":0.42,"stage":"weights","stage_index":2,"stage_count":4,"stage_progress":0.7,"elapsed_s":12.5,"eta_s":null,"stage_eta_s":null,"progress_basis":"stages"}`, LoadingProgress{Progress: f(0.42), Message: "loading weights, 42% (stage 2 of 4)"}, true},
		{"llama-server loading body", `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`, LoadingProgress{}, false},
		{"empty message", `{"message":""}`, LoadingProgress{}, false},
		{"not json", `Service Unavailable`, LoadingProgress{}, false},
		{"empty body", ``, LoadingProgress{}, false},
		{"wrong type", `{"progress":"half"}`, LoadingProgress{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLoadingProgress([]byte(tt.body))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !got.equal(&tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProcessCommand_ParseLoadingProgressTrimsMessage(t *testing.T) {
	// Multi-byte characters check the cut lands on a character boundary.
	long := strings.Repeat("é", maxLoadingMessageLen+10)
	got, ok := parseLoadingProgress([]byte(fmt.Sprintf(`{"message":%q}`, long)))
	if !ok {
		t.Fatal("expected a progress report")
	}
	if n := len([]rune(got.Message)); n != maxLoadingMessageLen {
		t.Errorf("message is %d characters, want %d", n, maxLoadingMessageLen)
	}
	if got.Message != strings.Repeat("é", maxLoadingMessageLen) {
		t.Error("message was not cut on a character boundary")
	}
}

// progressUpstream serves a health check that answers 503 with body(n) for
// the first `reports` polls, n counting from 1, and then becomes ready.
func progressUpstream(t *testing.T, reports int64, body func(n int64) string) *httptest.Server {
	t.Helper()
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		if n > reports {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, body(n))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProcessCommand_LoadingProgressExtendsTimeout checks that a load which
// keeps reporting progress is not cut off by the health check timeout, and that
// each report is published on the backend progress stream.
func TestProcessCommand_LoadingProgressExtendsTimeout(t *testing.T) {
	skipIfNoSimpleResponder(t)

	// Three reports one second apart take longer than the timeout below.
	mock := progressUpstream(t, 3, func(n int64) string {
		return fmt.Sprintf(`{"progress":%g,"message":"step %d"}`, float64(n)/4, n)
	})
	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           cmd,
		Proxy:         mock.URL,
		CheckEndpoint: "/health",
	})

	events := make(chan swaputil.BackendProgressEvent, 16)
	cancel := event.On(func(e swaputil.BackendProgressEvent) {
		if e.Model == p.id && e.Phase == "loading" {
			events <- e
		}
	})
	defer cancel()

	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	if err := p.EnsureReady(ctx, 1500*time.Millisecond); err != nil {
		t.Fatalf("EnsureReady: %v", err)
	}
	defer p.Stop(testStopTimeout)

	select {
	case e := <-events:
		if e.Progress != 0.25 || e.Message != "step 1" {
			t.Errorf("first event = %+v, want progress 0.25 and message \"step 1\"", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no loading progress event emitted")
	}
}

// TestProcessCommand_LoadingProgressAbortsOnStop covers the interaction the
// progress extension must not break: once the start budget has expired with
// progress already reported, an operator Stop still has to end the load
// promptly. The budget's Done channel is closed by then, so only a separate
// abort signal can reach the health loop.
//
// The upstream matters here. It reports progress for a few polls and then
// blocks the probe, so the loop is parked inside the request when Stop
// arrives: nothing but healthCtx cancellation can end it. With a responsive
// upstream the loop would notice through its one-second tick and the bug would
// stay hidden.
func TestProcessCommand_LoadingProgressAbortsOnStop(t *testing.T) {
	skipIfNoSimpleResponder(t)

	var polls atomic.Int64
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := polls.Add(1); n <= 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"progress":%g,"message":"loading"}`, float64(n)/10)
			return
		}
		// Park the probe until the client gives up; only a cancelled health
		// context can unblock it.
		<-r.Context().Done()
	}))
	defer mock.Close()

	cmd, _ := simpleResponderCmd(t, "-silent")
	p := newProcessCommand(t, config.ModelConfig{
		Cmd:           cmd,
		Proxy:         mock.URL,
		CheckEndpoint: "/health",
		// Far longer than the test's patience: the health deadline must not be
		// what ends this start.
		HealthCheckTimeout: 60,
		// No response-header timeout, so the blocked probe stays blocked.
		Timeouts: config.TimeoutsConfig{ResponseHeader: 0},
	})
	t.Cleanup(func() { _ = p.Stop(testStopTimeout) })

	startErr := make(chan error, 1)
	go func() { startErr <- p.EnsureReady(context.Background(), 4*time.Second) }()

	sawProgress := make(chan struct{})
	cancelWatch := event.On(func(e swaputil.BackendProgressEvent) {
		if e.Model == p.id && e.Phase == "loading" && e.Progress > 0 {
			select {
			case <-sawProgress:
			default:
				close(sawProgress)
			}
		}
	})
	defer cancelWatch()

	select {
	case <-sawProgress:
	case <-time.After(8 * time.Second):
		t.Fatal("no loading progress was published")
	}
	// Let the start budget expire, then let the loop advance past the reports
	// and park itself inside the blocked probe.
	time.Sleep(5 * time.Second)
	if got := p.State(); got != StateStarting {
		t.Fatalf("state = %s, want starting", got)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop(testStopTimeout) }()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Generous: testStopTimeout already bounds Stop at 2s; a loaded CI
		// runner (taskkill is slow to spawn) needs margin above that, not a
		// three-second ceiling that fires while teardown is still legal.
		t.Fatal("Stop did not return while a progress-extended load was starting")
	}

	select {
	case err := <-startErr:
		if !errors.Is(err, ErrStartAborted) {
			t.Fatalf("EnsureReady error = %v, want ErrStartAborted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EnsureReady did not return after Stop")
	}

	waitForState(t, p, StateStopped)
}

// TestProcessCommand_LoadingProgressStalls checks that only a change in the
// reported progress value holds the start open past the health check timeout.
func TestProcessCommand_LoadingProgressStalls(t *testing.T) {
	skipIfNoSimpleResponder(t)

	tests := []struct {
		name    string
		body    func(n int64) string
		wantErr string
	}{
		{
			name:    "same report",
			body:    func(int64) string { return `{"progress":0.1,"message":"step 1"}` },
			wantErr: "without loading progress",
		},
		{
			// kyojin's message carries an ETA that keeps changing, and grows,
			// while a stage is stuck.
			name:    "message changes but progress does not",
			body:    func(n int64) string { return fmt.Sprintf(`{"progress":0.1,"message":"about %d s left"}`, 10+n) },
			wantErr: "without loading progress",
		},
		{
			name:    "message without progress",
			body:    func(n int64) string { return fmt.Sprintf(`{"message":"step %d"}`, n) },
			wantErr: "health check timed out after 1.5s",
		}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := progressUpstream(t, 1000, tt.body)
			cmd, _ := simpleResponderCmd(t, "-silent")
			p := newProcessCommand(t, config.ModelConfig{
				Cmd:           cmd,
				Proxy:         mock.URL,
				CheckEndpoint: "/health",
			})

			// The upstream never becomes ready, so if the health check timeout
			// did not fire, EnsureReady would end with this context's deadline
			// error instead. The fork appends ": aborted" to keep ErrStartAborted
			// in the chain, so the deadline text is matched as a substring.
			ctx, cancelCtx := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelCtx()
			err := p.EnsureReady(ctx, 1500*time.Millisecond)
			if err == nil || !errors.Is(err, ErrStartAborted) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("EnsureReady error = %v, want ErrStartAborted containing %q", err, tt.wantErr)
			}
		})
	}
}
