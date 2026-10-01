//go:build !windows

// vllm-wrapper drives a linux vLLM serve process (sleep/wake control via the
// vLLM HTTP API and unix signals); it has no windows build.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	pathpkg "path"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// sleepLevel represents the vLLM sleep level.
type sleepLevel int

const (
	sleepLevel1 sleepLevel = 1
	sleepLevel2 sleepLevel = 2
)

// vllmControlClient bounds lifecycle calls made by the wrapper. A hung
// management endpoint must not block SIGTERM shutdown or leave a cmdStop
// process waiting forever; inference proxy requests use their own transport
// and are intentionally not constrained by this timeout.
var vllmControlClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectVLLMRedirect}

// Control and readiness requests must stay on the configured vLLM endpoint.
// Following a redirect here could turn a health check or lifecycle operation
// into an SSRF primitive, especially when the wrapper is exposed on a host
// network. A redirect is an explicit backend failure instead.
func rejectVLLMRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// vllmWrapper serves as a cmd/cmdStop wrapper for vLLM with sleep mode.
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <command> [args]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  serve    Start as a forward proxy (for cmd)\n")
		fmt.Fprintf(os.Stderr, "  sleep    Put vLLM to sleep (for cmdStop)\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "sleep":
		sleepCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// serveCmd implements the serve subcommand.
func serveCmd(args []string) {
	var (
		vllmURL     string
		listenAddr  string
		sleepLevel  int
		healthPath  string
		waitTimeout time.Duration
		journalUnit string
	)
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&vllmURL, "vllm-url", "", "Base URL of vLLM server (e.g., http://127.0.0.1:8000)")
	fs.StringVar(&listenAddr, "listen", "", "Address to listen on (e.g., :$PORT)")
	fs.IntVar(&sleepLevel, "sleep-level", 1, "Sleep level to use when sleeping (default 1)")
	fs.StringVar(&healthPath, "health-path", "/health", "Health check path (default /health)")
	fs.DurationVar(&waitTimeout, "wait-timeout", 120*time.Second, "Timeout waiting for daemon to become healthy")
	fs.StringVar(&journalUnit, "journal-unit", "", "User systemd unit whose logs should be forwarded to stdout")
	fs.Parse(args)
	startArgs := fs.Args()
	if err := validateSleepLevel(sleepLevel); err != nil {
		log.Fatal(err)
	}

	if vllmURL == "" {
		log.Fatalf("--vllm-url is required")
	}
	if listenAddr == "" {
		log.Fatalf("--listen is required")
	}
	if len(startArgs) == 0 {
		log.Fatalf("a daemon command after -- is required")
	}
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		log.Fatal(err)
	}
	if err := validateVLLMHealthPath(healthPath); err != nil {
		log.Fatal(err)
	}

	// Ensure vLLM URL does not have trailing slash or accidental outer
	// whitespace before it is handed to the reverse proxy.
	vllmURL = strings.TrimRight(strings.TrimSpace(vllmURL), "/")

	journalCtx, stopJournal := context.WithCancel(context.Background())
	defer stopJournal()
	if journalUnit != "" {
		if err := startJournalForwarder(journalCtx, journalUnit); err != nil {
			log.Printf("Warning: failed to forward logs from %s: %v", journalUnit, err)
		}
	}

	// Step 1: Ensure the daemon is running and awake.
	// First, check if we can reach the daemon (liveness).
	if err := checkHealthy(vllmURL, healthPath); err != nil {
		// Not reachable, try to wake up (in case it's asleep but we couldn't reach? Actually, if not reachable, waking won't work)
		log.Printf("vLLM daemon not reachable (%v), attempting to wake up", err)
		if err := wakeUpVLLM(vllmURL); err != nil {
			// Wake up failed (e.g., connection refused), assume daemon not running, try to start it.
			log.Printf("Wake up failed: %v, attempting to start daemon", err)
			if err := startDaemon(startArgs, vllmURL, healthPath, waitTimeout); err != nil {
				log.Fatalf("Failed to start daemon: %v", err)
			}
		} else {
			// Wake up succeeded, now wait for healthy (liveness).
			log.Printf("Wake up sent, waiting for healthy state")
			if err := waitForHealthyWithPath(vllmURL, healthPath, waitTimeout); err != nil {
				log.Fatalf("vLLM health check failed after wake up: %v", err)
			}
		}
	} else {
		// Reachable (liveness ok). Now wake up to ensure it's not asleep.
		log.Printf("vLLM daemon is reachable at %s%s, attempting to wake up if asleep", vllmURL, healthPath)
		if err := wakeUpVLLM(vllmURL); err != nil {
			// Log the error but continue because wake is idempotent and we might be already awake.
			log.Printf("Warning: wake up failed (but continuing): %v", err)
		}
		// After waking, we wait for the daemon to become healthy again (liveness) to ensure it's ready.
		log.Printf("Waiting for vLLM to be healthy after wake up")
		if err := waitForHealthyWithPath(vllmURL, healthPath, 10*time.Second); err != nil {
			log.Fatalf("vLLM health check failed after wake up: %v", err)
		}
	}

	// Step 2: Set up reverse proxy from listenAddr to vllmURL.
	proxyURL, err := url.Parse(vllmURL)
	if err != nil {
		log.Fatalf("Invalid vLLM URL %q: %v", vllmURL, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(proxyURL)

	// Create a custom transport to set timeouts.
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}
	proxy.Transport = transport

	// Modify response to disable buffering for streaming.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}

	// Create HTTP server.
	srv := &http.Server{
		Addr: listenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUnsafeVLLMPath(r.URL.Path) {
				// The wrapper listener may be reachable by an ordinary inference
				// client. Never expose vLLM's process/weight/profile control plane
				// through that listener; the wrapper's own sleep command talks to
				// the configured backend URL directly.
				http.Error(w, "vLLM management endpoint is not available through the inference proxy", http.StatusForbidden)
				return
			}
			proxy.ServeHTTP(w, r)
		}),
	}

	// Start server in a goroutine.
	go func() {
		log.Printf("Starting vllm-wrapper serve on %s -> %s", listenAddr, vllmURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	sig := <-c
	signal.Stop(c)

	if sig == syscall.SIGTERM {
		log.Printf("SIGTERM received, putting vLLM to sleep (level %d)", sleepLevel)
		if err := sleepVLLM(vllmURL, sleepLevel); err != nil {
			log.Printf("Warning: failed to put vLLM to sleep: %v", err)
		} else {
			log.Printf("Successfully put vLLM to sleep (level %d)", sleepLevel)
		}
	}

	log.Println("Shutting down vllm-wrapper serve...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
	log.Println("Server stopped")
}

// sleepCmd implements the sleep subcommand.
func sleepCmd(args []string) {
	var (
		vllmURL    string
		sleepLevel int
		stopPID    int
	)
	fs := flag.NewFlagSet("sleep", flag.ExitOnError)
	fs.StringVar(&vllmURL, "vllm-url", "", "Base URL of vLLM server (e.g., http://127.0.0.1:8000)")
	fs.IntVar(&sleepLevel, "sleep-level", 1, "Sleep level to use (default 1)")
	fs.IntVar(&stopPID, "stop-pid", 0, "PID of the serve proxy to terminate after vLLM successfully enters sleep mode")
	fs.Parse(args)
	if err := validateSleepLevel(sleepLevel); err != nil {
		log.Fatal(err)
	}

	if vllmURL == "" {
		log.Fatalf("--vllm-url is required")
	}
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		log.Fatal(err)
	}
	vllmURL = strings.TrimRight(strings.TrimSpace(vllmURL), "/")

	// Current vLLM exposes sleep level as a query parameter. Keep the request
	// body empty so this also works with strict FastAPI validation.
	resp, err := postSleep(vllmURL, sleepLevel)
	if err != nil {
		log.Fatalf("Failed to send sleep request: %v", err)
	}
	defer resp.Body.Close()

	if !isSuccessfulControlStatus(resp.StatusCode) {
		log.Fatalf("vLLM sleep request failed with status %d: %v", resp.StatusCode, resp.Status)
	}

	log.Printf("Successfully put vLLM to sleep (level %d)", sleepLevel)

	if stopPID > 0 {
		if err := syscall.Kill(stopPID, syscall.SIGTERM); err != nil {
			log.Fatalf("Failed to stop serve proxy process %d: %v", stopPID, err)
		}
		log.Printf("Sent SIGTERM to serve proxy process %d", stopPID)
	}
}

// sleepVLLM sends a POST to /sleep to put the vLLM daemon to sleep.
func sleepVLLM(vllmURL string, sleepLevel int) error {
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		return err
	}
	resp, err := postSleep(vllmURL, sleepLevel)
	if err != nil {
		return fmt.Errorf("failed to send sleep request: %w", err)
	}
	defer resp.Body.Close()

	if !isSuccessfulControlStatus(resp.StatusCode) {
		return fmt.Errorf("vLLM sleep request failed with status %d: %s", resp.StatusCode, resp.Status)
	}

	return nil
}

// isSuccessfulControlStatus accepts the full successful HTTP class. vLLM
// releases and reverse proxies have used both 200 and 204 for lifecycle
// commands; treating a successful 2xx response uniformly keeps sleep/wake
// idempotent without accepting redirects or other non-success responses.
func isSuccessfulControlStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

func postSleep(vllmURL string, level int) (*http.Response, error) {
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		return nil, err
	}
	if err := validateSleepLevel(level); err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(vllmURL, "/") + "/sleep?level=" + url.QueryEscape(fmt.Sprintf("%d", level))
	request, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return vllmControlClient.Do(request)
}

func validateSleepLevel(level int) error {
	if level < int(sleepLevel1) || level > int(sleepLevel2) {
		return fmt.Errorf("sleep level must be 1 or 2")
	}
	return nil
}

func isUnsafeVLLMPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	// Request paths can contain escaped path segments, repeated separators, or
	// dot segments. Normalize all of them before comparing the denylist so a
	// public inference listener cannot reach a management endpoint through a
	// spelling variant. Matching is case-insensitive as a defensive measure for
	// upstream routers that normalize endpoint names differently.
	// Path may have passed through more than one proxy before reaching this
	// listener. Decode a small bounded number of layers so a double-escaped
	// management segment cannot bypass the denylist, while avoiding an
	// attacker-controlled unbounded loop. Treat malformed escapes as unsafe;
	// an inference request can be retried with a valid URL, whereas forwarding
	// an ambiguous path to a management-capable upstream is not safe.
	for i := 0; i < 3; i++ {
		decoded, err := url.PathUnescape(path)
		if err != nil {
			return true
		}
		if decoded == path {
			break
		}
		path = decoded
	}
	// Backslashes are not path separators on the Linux wrapper itself, but
	// treating them as separators here closes the same normalization gap when
	// a request traverses a Windows-aware proxy before arriving upstream.
	path = strings.ReplaceAll(path, `\`, "/")
	path = pathpkg.Clean("/" + strings.TrimPrefix(path, "/"))
	normalized := strings.ToLower(path)
	for _, blocked := range []string{
		"/sleep",
		"/wake_up",
		"/is_sleeping",
		"/pause",
		"/resume",
		"/is_paused",
		"/abort_requests",
		"/collective_rpc",
		"/load_lora_adapter",
		"/unload_lora_adapter",
		"/remove_lora_adapter",
		"/update_lora_adapter",
		"/start_profile",
		"/stop_profile",
		"/reset_prefix_cache",
		"/reset_mm_cache",
		"/reset_encoder_cache",
		"/reload_weights",
		"/init_weight_transfer_engine",
		"/start_weight_update",
		"/update_weights",
		"/finish_weight_update",
		"/update_weight_version",
		"/weight_info",
		"/get_world_size",
		"/scale_elastic_ep",
		"/is_scaling_elastic_ep",
		"/server_info",
		"/tokenizer_info",
		"/shutdown",
	} {
		blocked = strings.ToLower(blocked)
		for _, candidate := range []string{normalized, stripVLLMVersionPrefix(normalized)} {
			if candidate == blocked || strings.HasPrefix(candidate, blocked+"/") {
				return true
			}
		}
	}
	return false
}

// stripVLLMVersionPrefix returns the unversioned spelling used by vLLM's
// development endpoints. Some releases expose a management endpoint at both
// /foo and /v1/foo (notably the LoRA helpers), so the public wrapper must
// apply the same denylist to either form.
func stripVLLMVersionPrefix(path string) string {
	if strings.HasPrefix(path, "/v1/") {
		return path[len("/v1"):]
	}
	return path
}

// wakeUpVLLM sends a POST to /wake_up to wake the vLLM daemon.
func wakeUpVLLM(vllmURL string) error {
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		return err
	}
	// The wake_up endpoint may not require a body; we'll send a POST with empty body.
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(vllmURL, "/")+"/wake_up", strings.NewReader(""))
	if err != nil {
		return fmt.Errorf("failed to create /wake_up request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := vllmControlClient.Do(request)
	if err != nil {
		return fmt.Errorf("failed to POST /wake_up: %w", err)
	}
	defer resp.Body.Close()

	if !isSuccessfulControlStatus(resp.StatusCode) {
		// vLLM and compatible reverse proxies may return any successful 2xx
		// status for an idempotent lifecycle operation.
		return fmt.Errorf("/wake_up returned unexpected status %d: %s", resp.StatusCode, resp.Status)
	}
	return nil
}

// waitForHealthyWithPath polls the vLLM daemon's health endpoint at the given path.
func waitForHealthyWithPath(vllmURL string, healthPath string, timeout time.Duration) error {
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		return err
	}
	if err := validateVLLMHealthPath(healthPath); err != nil {
		return err
	}
	if timeout <= 0 {
		// A non-positive timeout must never be interpreted as "already
		// healthy". Callers use this helper as the readiness gate before
		// exposing the proxy, so an empty polling window is an explicit
		// deadline failure.
		return context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	healthURL := joinVLLMPath(vllmURL, healthPath)
	for time.Now().Before(deadline) {
		// Create a request with context.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		// Readiness is part of the control plane just like sleep/wake. Use the
		// redirect-disabled client so a 3xx response cannot move polling to an
		// unrelated host after the configured endpoint has been checked.
		resp, err := vllmControlClient.Do(req)
		if err != nil {
			// If context canceled, break.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Wait a bit before retrying, but never sleep past the caller's
			// deadline. This keeps short health timeouts genuinely bounded.
			if !waitForHealthRetry(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		resp.Body.Close()
		if !waitForHealthRetry(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// joinVLLMPath accepts the path forms used by config files and avoids making
// readiness depend on whether an operator included a trailing slash in the
// base URL or a leading slash in the health path.
func joinVLLMPath(vllmURL, healthPath string) string {
	base := strings.TrimRight(strings.TrimSpace(vllmURL), "/")
	path := strings.TrimSpace(healthPath)
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func waitForHealthRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// checkHealthy sends a GET request to the health path and returns nil if the response status is 200 OK.
func checkHealthy(vllmURL string, healthPath string) error {
	if err := validateVLLMBaseURL(vllmURL); err != nil {
		return err
	}
	if err := validateVLLMHealthPath(healthPath); err != nil {
		return err
	}
	resp, err := vllmControlClient.Get(joinVLLMPath(vllmURL, healthPath))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// validateVLLMBaseURL keeps the wrapper's control and inference targets
// explicit. Credentials, query strings and fragments are not meaningful for a
// backend base URL and can accidentally leak into reverse-proxy requests or
// control endpoints. Callers may still use a path-bearing base URL for a
// reverse proxy mounted below a prefix.
func validateVLLMBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return fmt.Errorf("vLLM URL must be a valid HTTP(S) URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("vLLM URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if err := validateVLLMPathValue(u.EscapedPath(), "vLLM URL path"); err != nil {
		return err
	}
	return nil
}

func validateVLLMHealthPath(raw string) error {
	if err := validateVLLMPathValue(raw, "vLLM health path"); err != nil {
		return err
	}
	return nil
}

// validateVLLMPathValue validates a path before it is concatenated with a
// configured backend URL. URL parsing decodes only one layer and different
// reverse proxies normalize dot segments at different points, so perform a
// bounded decode and reject ambiguous path data before constructing requests.
func validateVLLMPathValue(raw, label string) error {
	if raw != strings.TrimSpace(raw) {
		return fmt.Errorf("%s contains invisible or whitespace data", label)
	}
	value := raw
	if value == "" {
		return nil
	}
	for i := 0; i < 3; i++ {
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return fmt.Errorf("%s contains malformed escaping", label)
		}
		if decoded == value {
			break
		}
		value = decoded
	}
	if strings.ContainsAny(value, "?#\\\x00\r\n") {
		return fmt.Errorf("%s must be a path without query, fragment, backslash, or control data", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return fmt.Errorf("%s contains invisible or whitespace data", label)
		}
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s must not contain dot path segments", label)
		}
	}
	return nil
}

// startJournalForwarder forwards new log entries from a user systemd unit to stdout.
func startJournalForwarder(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(
		ctx,
		"journalctl",
		"--user-unit="+unit,
		"--follow",
		"--lines=0",
		"--output=cat",
		"--no-pager",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start journalctl for %s: %w", unit, err)
	}

	go func() {
		err := cmd.Wait()
		if ctx.Err() == nil && err != nil {
			log.Printf("Journal forwarding for %s stopped: %v", unit, err)
		}
	}()

	return nil
}

// startDaemon executes the start command and waits for the vLLM daemon to become healthy.
func startDaemon(startArgs []string, vllmURL string, healthPath string, waitTimeout time.Duration) error {
	// A failed readiness probe must still give a short-lived helper enough time
	// to finish its exec/argv handling before it is killed.  Under the race
	// detector the whole repository is tested concurrently and process
	// scheduling can be delayed well beyond the normal sub-second path; keep the
	// grace bounded, but generous enough that diagnostics are not lost merely
	// because the host is busy.
	const processExitGrace = 10 * time.Second
	cmd := exec.Command(startArgs[0], startArgs[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon command: %w", err)
	}
	// Always reap the child. Besides avoiding a zombie when a daemon exits
	// during startup, observing its exit before killing it prevents a very
	// short-lived command from being terminated before it has run its argv
	// handling (which made diagnostics and tests race the exec boundary).
	processDone := make(chan error, 1)
	go func() { processDone <- cmd.Wait() }()
	// Wait for healthy state.
	log.Printf("Started daemon with PID %d, waiting for healthy state", cmd.Process.Pid)
	err := waitForHealthyWithPath(vllmURL, healthPath, waitTimeout)
	if err != nil {
		// If the command already exited, let its side effects and exit status be
		// observed before returning. Otherwise kill it and wait for reaping. The
		// bounded grace keeps a hung child from extending the health timeout while
		// avoiding a race with very short-lived helpers that have not reached their
		// argv handling yet.
		select {
		case <-processDone:
		case <-time.After(processExitGrace):
			_ = cmd.Process.Kill()
			<-processDone
		}
		return fmt.Errorf("daemon did not become healthy: %w", err)
	}
	// Daemon is healthy, we don't wait for the command to exit (it should keep running).
	// We'll let it run; the wrapper will not kill it on exit.
	return nil
}
