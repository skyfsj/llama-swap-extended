package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/realtime"
)

type realtimeTestHijacker struct {
	conn     net.Conn
	rw       *bufio.ReadWriter
	header   http.Header
	hijacked chan struct{}
}

func (w *realtimeTestHijacker) Header() http.Header { return w.header }

func (w *realtimeTestHijacker) Write(data []byte) (int, error) { return w.rw.Write(data) }

func (w *realtimeTestHijacker) WriteHeader(_ int) {}

func (w *realtimeTestHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	select {
	case <-w.hijacked:
	default:
		close(w.hijacked)
	}
	return w.conn, w.rw, nil
}

func TestWriteHijackedErrorEscapesJSONControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	rw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	writeHijackedError(rw, http.StatusBadRequest, "bad\ninput\t\"quoted\"")
	parts := strings.SplitN(buf.String(), "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("response missing header separator: %q", buf.String())
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(parts[1]), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v (%q)", err, parts[1])
	}
	if payload["error"] != "bad\ninput\t\"quoted\"" {
		t.Fatalf("error payload = %q", payload["error"])
	}
}

func TestRealtimeHijackedWriterFinishFlushesHeaderOnlyResponse(t *testing.T) {
	var buf bytes.Buffer
	rw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
	writer := &realtimeHijackedWriter{header: make(http.Header), rw: rw, status: http.StatusNoContent}
	writer.finish()
	if !strings.HasPrefix(buf.String(), "HTTP/1.1 204 No Content\r\n") {
		t.Fatalf("header-only response = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Content-Length: 0\r\n") {
		t.Fatalf("header-only response missing content length: %q", buf.String())
	}
	if !writer.written {
		t.Fatal("finish did not mark response written")
	}
}

func TestRealtimeMiddlewareResolvesBodyFrameForNonHijackingWriter(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	var gotModel string
	handler := CreateRealtimeMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel = r.URL.Query().Get("model")
		w.WriteHeader(http.StatusNoContent)
	}))
	payload := `{"type":"session.update","session":{"model":"m"}}`
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, byte(0x80 | len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range []byte(payload) {
		frame = append(frame, b^mask[i%4])
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/realtime", strings.NewReader(string(frame)))
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if gotModel != "m" || w.Code != http.StatusNoContent {
		t.Fatalf("model=%q status=%d body=%s", gotModel, w.Code, w.Body.String())
	}
	if body, _ := io.ReadAll(r.Body); string(body) != string(frame) {
		t.Fatalf("first frame was not restored")
	}
}

func TestRealtimeMiddlewareRejectsOversizedFallbackFrame(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	called := false
	handler := CreateRealtimeMiddleware(cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	body := bytes.Repeat([]byte{'x'}, realtime.MaxFramePayload+33)
	r := httptest.NewRequest(http.MethodGet, "/v1/realtime", bytes.NewReader(body))
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if called {
		t.Fatal("oversized realtime fallback reached dispatcher")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%q, want 413", w.Code, w.Body.String())
	}
}

func TestRealtimeMiddlewareEnforcesInferenceScopeBeforeDispatch(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	called := false
	handler := CreateRealtimeMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	payload := `{"type":"session.update","session":{"model":"m"}}`
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, byte(0x80 | len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for i, b := range []byte(payload) {
		frame = append(frame, b^mask[i%4])
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/realtime", strings.NewReader(string(frame)))
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r = r.WithContext(withIdentity(r.Context(), auth.Identity{ID: "logs-only", Scopes: map[string]struct{}{auth.ScopeLogs: {}}}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if called {
		t.Fatal("realtime request reached dispatcher without inference scope")
	}
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "missing scope") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRealtimeMiddlewareCancelsBeforeFirstFrame(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	called := make(chan struct{}, 1)
	handler := CreateRealtimeMiddleware(cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called <- struct{}{}
	}))
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	writer := &realtimeTestHijacker{
		conn:     serverConn,
		rw:       bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)),
		header:   make(http.Header),
		hijacked: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil).WithContext(ctx)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, r)
		close(done)
	}()
	select {
	case <-writer.hijacked:
	case <-time.After(time.Second):
		t.Fatal("realtime middleware did not hijack connection")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("realtime middleware did not stop after request cancellation")
	}
	select {
	case <-called:
		t.Fatal("cancelled realtime request reached the dispatcher")
	default:
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("hijacked connection remained readable after cancellation")
	}
}

func TestRealtimeMiddlewareReplaysFirstFrameThroughHijackedBridge(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	var mu sync.Mutex
	var gotModel string
	server := httptest.NewServer(CreateRealtimeMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotModel = r.URL.Query().Get("model")
		mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unavailable", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		first, _, err := realtime.ReadFrame(rw.Reader)
		if err != nil || string(first.Payload) == "" {
			return
		}
		second, wire, err := realtime.ReadFrame(rw.Reader)
		if err != nil || second.Opcode != 1 {
			return
		}
		_, _ = rw.Write(wire)
		_ = rw.Flush()
	})))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", serverURL.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	firstPayload := `{"type":"session.update","session":{"model":"m"}}`
	firstWire := maskedRealtimeFrame(firstPayload, [4]byte{1, 2, 3, 4})
	secondPayload := `{"type":"input.text","text":"hello"}`
	secondWire := maskedRealtimeFrame(secondPayload, [4]byte{5, 6, 7, 8})
	request := "GET /v1/realtime HTTP/1.1\r\n" +
		"Host: " + serverURL.Host + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := conn.Write(append([]byte(request), append(firstWire, secondWire...)...)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade status=%q err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	echoFrame, _, err := realtime.ReadFrame(reader)
	if err != nil || string(echoFrame.Payload) != secondPayload {
		t.Fatalf("echo frame=%+v err=%v", echoFrame, err)
	}
	mu.Lock()
	model := gotModel
	mu.Unlock()
	if model != "m" {
		t.Fatalf("bridge model=%q, want m", model)
	}
}

func TestServer_RealtimeEndToEndThroughRoutes(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	gotModel := make(chan string, 1)
	bridgeErrors := make(chan error, 1)
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotModel <- r.URL.Query().Get("model"):
		default:
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			bridgeErrors <- io.ErrUnexpectedEOF
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			bridgeErrors <- err
			return
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			bridgeErrors <- err
			return
		}
		if err := rw.Flush(); err != nil {
			bridgeErrors <- err
			return
		}
		first, _, err := realtime.ReadFrame(rw.Reader)
		if err != nil {
			bridgeErrors <- err
			return
		}
		second, _, err := realtime.ReadFrame(rw.Reader)
		if err != nil {
			bridgeErrors <- err
			return
		}
		if first.Opcode != 1 || string(first.Payload) != `{"type":"session.update","session":{"model":"m"}}` {
			bridgeErrors <- errors.New("upstream did not receive the replayed session.update frame")
			return
		}
		if second.Opcode != 1 || string(second.Payload) != `{"type":"input.text","text":"hello"}` {
			bridgeErrors <- errors.New("upstream did not receive the buffered client frame")
			return
		}
		payload := second.Payload
		wire := append([]byte{0x81, byte(len(payload))}, payload...)
		if _, err := rw.Write(wire); err != nil {
			bridgeErrors <- err
			return
		}
		if err := rw.Flush(); err != nil {
			bridgeErrors <- err
		}
	}

	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	s.routes()
	upstream := httptest.NewServer(s)
	defer func() {
		upstream.Close()
		_ = s.Shutdown(time.Second)
	}()
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	firstPayload := `{"type":"session.update","session":{"model":"m"}}`
	secondPayload := `{"type":"input.text","text":"hello"}`
	request := "GET /v1/realtime HTTP/1.1\r\n" +
		"Host: " + parsed.Host + "\r\n" +
		"Connection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := conn.Write(append([]byte(request), append(maskedRealtimeFrame(firstPayload, [4]byte{1, 2, 3, 4}), maskedRealtimeFrame(secondPayload, [4]byte{5, 6, 7, 8})...)...)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade status=%q err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	echo, _, err := realtime.ReadFrame(reader)
	if err != nil || string(echo.Payload) != secondPayload {
		t.Fatalf("echo frame=%+v err=%v", echo, err)
	}
	select {
	case err := <-bridgeErrors:
		t.Fatalf("realtime bridge error: %v", err)
	case model := <-gotModel:
		if model != "m" {
			t.Fatalf("upstream model=%q, want m", model)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for realtime bridge completion")
	}
}

func maskedRealtimeFrame(payload string, mask [4]byte) []byte {
	data := []byte(payload)
	wire := []byte{0x81, byte(0x80 | len(data)), mask[0], mask[1], mask[2], mask[3]}
	for i, value := range data {
		wire = append(wire, value^mask[i%len(mask)])
	}
	return wire
}
