package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// extensionHost executes host functions requested by extension hooks during
// one inference request. The request is kept so capabilities (forwarding) can
// inherit the caller's identity; every call is validated here, never in the
// worker.
type extensionHost struct {
	server  *Server
	request *http.Request
	// depth is how many forward hops this request already spent.
	depth int
}

func newExtensionHost(s *Server, r *http.Request) *extensionHost {
	depth, _ := r.Context().Value(extensionForwardDepthKey{}).(int)
	return &extensionHost{server: s, request: r, depth: depth}
}

func (h *extensionHost) Exec(ctx context.Context, fn string, args json.RawMessage) (any, error) {
	started := time.Now()
	value, err := h.exec(ctx, fn, args)
	recordHostCall(ctx, fn, started, err)
	return value, err
}

func (h *extensionHost) exec(ctx context.Context, fn string, args json.RawMessage) (any, error) {
	switch {
	case fn == "":
		return nil, fmt.Errorf("host function is required")
	case fn == "kv.get" || fn == "kv.set" || fn == "kv.delete" || fn == "kv.keys":
		return h.execKV(ctx, fn, args)
	case fn == "models.forward":
		return h.execForward(ctx, args)
	case fn == "session.usage":
		return h.execSessionUsage(ctx)
	case fn == "http.fetch":
		return h.execHTTPFetch(ctx, args)
	case fn == "files.read":
		return h.execFileRead(ctx, args)
	case fn == "files.write":
		return h.execFileWrite(ctx, args)
	default:
		return nil, fmt.Errorf("host function %s is not supported", fn)
	}
}

// execHTTPFetch serves a Node executor's ctx.http.fetch through the same
// allowlisted fetch the goja worker performs in-process.
func (h *extensionHost) execHTTPFetch(ctx context.Context, args json.RawMessage) (any, error) {
	var call struct {
		URL       string         `json:"url"`
		Options   map[string]any `json:"options"`
		Extension string         `json:"extension"`
	}
	if len(args) == 0 || json.Unmarshal(args, &call) != nil {
		return nil, fmt.Errorf("host call arguments are invalid")
	}
	item, ok := h.server.currentExtensions().Compiled(call.Extension)
	if !ok {
		return nil, errors.New("http.fetch is unavailable without a resolved extension")
	}
	body, status, err := extensions.FetchAllowed(item.Manifest.Permissions.NetworkHosts, call.URL, call.Options)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": status, "ok": status >= 200 && status < 300, "body": string(body)}, nil
}

// execFileRead / execFileWrite serve the Node executor's ctx.files with the
// same readRoots/writeRoots gating as the goja worker's in-process I/O.
func (h *extensionHost) execFileRead(ctx context.Context, args json.RawMessage) (any, error) {
	var call struct {
		Path      string `json:"path"`
		Extension string `json:"extension"`
	}
	if len(args) == 0 || json.Unmarshal(args, &call) != nil {
		return nil, fmt.Errorf("host call arguments are invalid")
	}
	item, ok := h.server.currentExtensions().Compiled(call.Extension)
	if !ok {
		return nil, errors.New("files are unavailable without a resolved extension")
	}
	file, err := extensions.AllowedPath(item.Manifest.Permissions.ReadRoots, call.Path, false)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

func (h *extensionHost) execFileWrite(ctx context.Context, args json.RawMessage) (any, error) {
	var call struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Extension string `json:"extension"`
	}
	if len(args) == 0 || json.Unmarshal(args, &call) != nil {
		return nil, fmt.Errorf("host call arguments are invalid")
	}
	item, ok := h.server.currentExtensions().Compiled(call.Extension)
	if !ok {
		return nil, errors.New("files are unavailable without a resolved extension")
	}
	if len(call.Content) > 10<<20 {
		return nil, errors.New("file exceeds 10 MiB")
	}
	file, err := extensions.AllowedPath(item.Manifest.Permissions.WriteRoots, call.Path, true)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte(call.Content), 0o600); err != nil {
		return nil, err
	}
	return true, nil
}

// ===== ctx.models.forward =====

// extensionForwardDepthKey counts how many forward hops a request already
// spent, so nested forwards are bounded by extensions.maxForwardDepth.
type extensionForwardDepthKey struct{}

// extensionSkipKey marks a request that must skip extension matching: the
// forward's inner call runs the pipeline without hooks.
type extensionSkipKey struct{}

func (h *extensionHost) execForward(ctx context.Context, args json.RawMessage) (any, error) {
	if h.server.modelChainHandler == nil {
		return nil, errors.New("model forwarding is unavailable")
	}
	max := h.server.currentConfig().Extensions.EffectiveMaxForwardDepth()
	if h.depth >= max {
		return nil, fmt.Errorf("forward depth limit %d reached", max)
	}
	var call struct {
		Model  string         `json:"model"`
		Window string         `json:"window"`
		Fields map[string]any `json:"request"`
	}
	if err := json.Unmarshal(args, &call); err != nil {
		return nil, fmt.Errorf("host call arguments are invalid: %s", err.Error())
	}
	if call.Fields == nil {
		return nil, errors.New("forward requires a request object")
	}
	model := strings.TrimSpace(call.Model)
	identity := identityFromContext(h.request.Context())
	cfg := h.server.currentConfig()
	if !modelAllowedForIdentity(cfg, identity, model) {
		return nil, fmt.Errorf("model %s is not available to this caller", model)
	}
	// Only "messages"-style bodies are forwarded: a chat.completions shape is
	// what the extension saw, and stream is always off for a buffered reply.
	payload := map[string]any{"model": model, "stream": false}
	for name, value := range call.Fields {
		payload[name] = value
	}
	if _, ok := payload["messages"]; !ok {
		return nil, errors.New("forward requires a messages array")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, extensionSkipKey{}, true)
	ctx = context.WithValue(ctx, extensionForwardDepthKey{}, h.depth+1)
	inner, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	inner.Header.Set("Content-Type", "application/json")
	// The inner chain re-authenticates, so the caller's credentials and a
	// traceable request id travel with it.
	inner.Header.Set("Authorization", h.request.Header.Get("Authorization"))
	if cookie := h.request.Header.Get("Cookie"); cookie != "" {
		inner.Header.Set("Cookie", cookie)
	}
	inner.Header.Set("X-Request-ID", forwardRequestID(h.request.Header.Get("X-Request-ID"), h.depth))
	recorder := newCappedRecorder(maxForwardResponseBytes)
	h.server.modelChainHandler.ServeHTTP(recorder, inner)
	if recorder.overflow {
		return nil, errors.New("forward response exceeds size limit")
	}
	var body any
	if len(recorder.body.Bytes()) > 0 {
		if err := json.Unmarshal(recorder.body.Bytes(), &body); err != nil {
			body = recorder.body.String()
		}
	}
	return map[string]any{"status": recorder.status, "body": body}, nil
}

const (
	// maxForwardResponseBytes caps one inner response; the script gets JSON
	// parsed from it, so a streaming-sized body has no place to go.
	maxForwardResponseBytes = 8 << 20
)

func forwardRequestID(outer string, depth int) string {
	base := strings.TrimSpace(outer)
	if base == "" {
		base = "ext"
	}
	if len(base) > 96 {
		base = base[:96]
	}
	return fmt.Sprintf("%s-fwd%d", base, depth+1)
}

// cappedRecorder buffers one handler write with a hard byte cap.
type cappedRecorder struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
	limit    int
}

func newCappedRecorder(limit int) *cappedRecorder {
	return &cappedRecorder{header: http.Header{}, status: 200, limit: limit}
}

func (r *cappedRecorder) Header() http.Header        { return r.header }
func (r *cappedRecorder) WriteHeader(statusCode int) { r.status = statusCode }
func (r *cappedRecorder) Write(data []byte) (int, error) {
	if r.body.Len()+len(data) > r.limit {
		r.overflow = true
		return 0, errors.New("forward response exceeds size limit")
	}
	return r.body.Write(data)
}

// ===== ctx.session.usage =====

func (h *extensionHost) execSessionUsage(ctx context.Context) (any, error) {
	session := extensionSession(h.request, auth.Identity{})
	requests, inputTokens, outputTokens, err := h.server.store.ExtensionSessionUsage(ctx, session.KeyID, session.ID, time.Now().Add(-sessionUsageWindow).Unix())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"requests":     requests,
		"inputTokens":  inputTokens,
		"outputTokens": outputTokens,
		"window":       "24h",
	}, nil
}

const sessionUsageWindow = 24 * time.Hour

// ===== ctx.kv =====

type kvArgs struct {
	Key        string `json:"key"`
	Scope      string `json:"scope"`      // "global" (default) | "session" | "key"
	Prefix     string `json:"prefix"`     // keys()
	TTLSeconds int64  `json:"ttlSeconds"` // set() override of the default TTL
	Value      any    `json:"value"`      // set()
	Extension  string `json:"extension"`  // worker-stamped identity of the call
}

func (h *extensionHost) execKV(ctx context.Context, fn string, args json.RawMessage) (any, error) {
	var call kvArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &call); err != nil {
			return nil, fmt.Errorf("host call arguments are invalid: %s", err.Error())
		}
	}
	if fn != "kv.keys" && !validKVKey(call.Key) {
		return nil, errors.New("kv key must be 1-128 characters without newline")
	}
	item, ok := h.server.currentExtensions().Compiled(call.Extension)
	if !ok {
		return nil, errors.New("kv is unavailable without a resolved extension")
	}
	persistent := item.Manifest.Permissions.Storage == extensions.StoragePersistent
	if item.Manifest.Permissions.Storage == extensions.StorageNone {
		return nil, errors.New("storage permission is disabled for this extension")
	}
	scope := h.kvScope(call)
	switch fn {
	case "kv.get":
		if persistent {
			record, found, err := h.server.store.GetExtensionKV(ctx, item.Manifest.ID, scope, call.Key)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, nil
			}
			var value any
			if err := json.Unmarshal(record.Value, &value); err != nil {
				return nil, errors.New("stored value is invalid")
			}
			return value, nil
		}
		return h.server.ephemeralExtensionKV().get(scopeKey(item.Manifest.ID, scope, call.Key)), nil
	case "kv.set":
		return h.kvSet(ctx, item.Manifest, persistent, scope, call)
	case "kv.delete":
		if persistent {
			return nil, h.server.store.DeleteExtensionKV(ctx, item.Manifest.ID, scope, call.Key)
		}
		h.server.ephemeralExtensionKV().delete(scopeKey(item.Manifest.ID, scope, call.Key))
		return true, nil
	case "kv.keys":
		if !validKVKey(call.Prefix) {
			return nil, errors.New("kv prefix must be 1-128 characters without newline")
		}
		if persistent {
			return h.server.store.ListExtensionKVPrefix(ctx, item.Manifest.ID, scope, call.Prefix)
		}
		return h.server.ephemeralExtensionKV().keys(scopeKey(item.Manifest.ID, scope, call.Prefix)), nil
	}
	return nil, fmt.Errorf("host function %s is not supported", fn)
}

// extensionID reads the worker-written identity of the call: the bridge stamps
// every host call with the manifest id, so a script cannot address another
// extension's storage. The id arrives inside the args payload.

func (h *extensionHost) kvSet(ctx context.Context, manifest extensions.Manifest, persistent bool, scope string, call kvArgs) (any, error) {
	value, err := json.Marshal(call.Value)
	if err != nil {
		return nil, fmt.Errorf("kv value is invalid: %s", err.Error())
	}
	quota := h.server.currentConfig().Extensions.EffectiveStorageMaxValueBytes()
	if len(value) > quota {
		return nil, fmt.Errorf("kv value exceeds %d bytes", quota)
	}
	maxKeys := h.server.currentConfig().Extensions.EffectiveStorageMaxKeys()
	extensionID := manifest.ID
	if persistent {
		count, err := h.server.store.CountExtensionKVs(ctx, extensionID)
		if err != nil {
			return nil, err
		}
		existing, _, err := h.server.store.GetExtensionKV(ctx, extensionID, scope, call.Key)
		if err != nil {
			return nil, err
		}
		if len(existing.Value) == 0 && count >= maxKeys {
			return nil, fmt.Errorf("kv key limit %d reached", maxKeys)
		}
		var expires *time.Time
		if call.TTLSeconds > 0 {
			moment := time.Now().Add(time.Duration(call.TTLSeconds) * time.Second)
			expires = &moment
		}
		return nil, h.server.store.SaveExtensionKV(ctx, extensionID, scope, call.Key, value, expires)
	}
	store := h.server.ephemeralExtensionKV()
	if !store.retain(extensionID, maxKeys) {
		return nil, fmt.Errorf("kv key limit %d reached", maxKeys)
	}
	ttl := time.Duration(0)
	if call.TTLSeconds > 0 {
		ttl = time.Duration(call.TTLSeconds) * time.Second
	} else {
		ttl = time.Duration(h.server.currentConfig().Extensions.EffectiveStorageEphemeralTTL()) * time.Minute
	}
	store.set(scopeKey(extensionID, scope, call.Key), value, ttl)
	return true, nil
}

// kvScope maps the script-level scope ("global"|"session"|"key") to a stored
// namespace. Callers without a session or key id get the shared "global" scope
// so keyless behavior stays usable.
func (h *extensionHost) kvScope(call kvArgs) string {
	switch call.Scope {
	case "session", "key":
		session := extensionSession(h.request, auth.Identity{})
		if call.Scope == "session" {
			if session.ID == "" && session.KeyID == "anonymous" {
				return "global"
			}
			return "session:" + scopeSanitize(session.ID)
		}
		if session.KeyID == "" || session.KeyID == "anonymous" {
			return "global"
		}
		return "key:" + scopeSanitize(session.KeyID)
	default:
		return "global"
	}
}

// scopeSanitize keeps scope ids free of delimiter injection so one caller
// cannot address another namespace's row.
func scopeSanitize(id string) string {
	return strings.NewReplacer(":", "_", "/", "_", "\x00", "_").Replace(id)
}

func scopeKey(extensionID, scope, key string) string {
	return extensionID + "\x00" + scope + "\x00" + key
}

func validKVKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, char := range key {
		if char == '\n' || char == '\r' || char == '\x00' {
			return false
		}
	}
	return true
}

// ephemeralExtensionKV returns the process-local KV instance. Sliding TTLs are
// applied per access, and a sweep runs opportunistically on writes so expired
// rows cannot outlive their TTL by more than one sweep interval.
type ephemeralKV struct {
	mu      sync.Mutex
	entries map[string]ephemeralKVEntry
	lastGC  time.Time
}

func (k *ephemeralKV) sweep(now time.Time) {
	for key, entry := range k.entries {
		if !entry.expires.IsZero() && !entry.expires.After(now) {
			delete(k.entries, key)
		}
	}
}

func (k *ephemeralKV) get(composite string) any {
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.entries[composite]
	if !ok {
		return nil
	}
	if !entry.expires.IsZero() && !entry.expires.After(time.Now()) {
		delete(k.entries, composite)
		return nil
	}
	var value any
	if err := json.Unmarshal(entry.value, &value); err != nil {
		return nil
	}
	return value
}

func (k *ephemeralKV) delete(composite string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, composite)
}

func (k *ephemeralKV) set(composite string, value []byte, ttl time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Since(k.lastGC) > 5*time.Minute {
		k.sweep(time.Now())
		k.lastGC = time.Now()
	}
	entry := ephemeralKVEntry{value: value}
	if ttl > 0 {
		entry.expires = time.Now().Add(ttl)
	}
	k.entries[composite] = entry
}

// retain reports whether a new key still fits under the per-extension limit.
func (k *ephemeralKV) retain(extensionID string, maxKeys int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	count := 0
	for key := range k.entries {
		if strings.HasPrefix(key, extensionID+"\x00") {
			count++
		}
	}
	return count < maxKeys
}

func (k *ephemeralKV) keys(prefix string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	var keys []string
	for key, entry := range k.entries {
		if !entry.expires.IsZero() && !entry.expires.After(now) {
			continue
		}
		compositeKey := key
		if strings.HasPrefix(compositeKey, prefix) {
			keys = append(keys, strings.TrimPrefix(compositeKey, prefix))
		}
	}
	return keys
}

// ===== test mode =====

// dryRunHost answers host calls the same way ctx.http.fetch answers in test
// mode: a clear rejection instead of silent behavior differences.
type dryRunHost struct{}

var dryRunFeatureByPrefix = []struct {
	prefix  string
	feature string
}{
	{"kv.", "storage"},
	{"models.", "model forwarding and model listing context"},
	{"session.", "session usage"},
}

func (dryRunHost) Exec(_ context.Context, fn string, _ json.RawMessage) (any, error) {
	for _, entry := range dryRunFeatureByPrefix {
		if len(fn) > len(entry.prefix) && fn[:len(entry.prefix)] == entry.prefix {
			return nil, fmt.Errorf("%s is disabled in extension test mode", entry.feature)
		}
	}
	return nil, fmt.Errorf("host function %s is disabled in extension test mode", fn)
}

// recordHostCall attaches one host call to the debug chat capture when present.
func recordHostCall(ctx context.Context, fn string, started time.Time, execErr error) {
	capture := debugCaptureFrom(ctx)
	if capture == nil {
		return
	}
	step := map[string]any{"kind": "hostcall", "fn": fn, "durationMs": float64(time.Since(started)) / float64(time.Millisecond)}
	if execErr != nil {
		step["error"] = execErr.Error()
	}
	capture.record(step)
}

// withExtensionHost attaches the per-request host-call executor to the request
// context so every hook invocation on this request can reach it.
func withExtensionHost(s *Server, r *http.Request) *http.Request {
	return r.WithContext(extensions.WithHostCaller(r.Context(), newExtensionHost(s, r)))
}

// ephemeralExtensionKV returns the process-local KV instance shared by every
// extension; composite keys (extension+scope+key) keep the entries apart.
func (s *Server) ephemeralExtensionKV() *ephemeralKV {
	s.extensionKVOnce.Do(func() { s.extensionKV = &ephemeralKV{entries: map[string]ephemeralKVEntry{}} })
	return s.extensionKV
}

// ephemeralKVEntry is one value with its sliding expiry; a zero expiry means
// the entry never expires on its own.
type ephemeralKVEntry struct {
	value   []byte
	expires time.Time
}
