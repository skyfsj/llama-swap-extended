package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type nativeCheckpointKey struct{}
type nativeCheckpointCapture struct{ data *nativeCheckpointData }
type nativeCheckpointData struct {
	State         conversation.State  `json:"state"`
	Native        conversation.Native `json:"native"`
	Version       int64               `json:"version"`
	Configuration string              `json:"configuration"`
	ResponseID    string              `json:"response_id"`
}

func (s *Server) nativeGenerationConfiguration() string {
	cfg := s.currentConfig()
	data, _ := json.Marshal([]any{cfg.Models, cfg.Providers})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (t *conversationTurn) captureNativeCheckpoint(ctx context.Context, capture *nativeCheckpointCapture, model string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.latest) != 1 {
		return errors.New("native background generation has an ambiguous upstream source")
	}
	data := &nativeCheckpointData{State: t.state, Version: t.record.Version, Configuration: t.server.nativeGenerationConfiguration()}
	current := t.current
	if t.request != nil {
		current = t.request
	}
	data.State.Append(current, nil)
	for _, native := range t.latest {
		sidecar := conversation.State{}
		sidecar.Remember(native.Scope, native.Version, native.Request, native.Response)
		if err := t.server.compactConversationMedia(ctx, &sidecar); err != nil {
			return err
		}
		data.Native = sidecar.Native[native.Scope.Key()]
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(data.Native.Response, &response) != nil || json.Unmarshal(response["id"], &data.ResponseID) != nil || data.ResponseID == "" {
		return errors.New("native background response omitted its identity")
	}
	if err := t.server.compactConversationMedia(ctx, &data.State); err != nil {
		return err
	}
	encoded, err := json.Marshal(data)
	if err != nil || len(encoded) > store.MaxConversationBytes {
		return errors.New("native background checkpoint exceeds capacity")
	}
	capture.data = data
	return nil
}

func (s *Server) nativeGenerationRequest(ctx context.Context, g store.ConversationGeneration, data *nativeCheckpointData, cancel bool) ([]byte, int, error) {
	if data.Configuration != s.nativeGenerationConfiguration() {
		return nil, 409, errors.New("native generation configuration changed")
	}
	gw := s.currentGateway()
	if gw == nil {
		return nil, 503, errors.New("native generation gateway is unavailable")
	}
	path := "/v1/responses/" + url.PathEscape(data.ResponseID)
	if data.Native.Scope.Format == dialect.GeminiInteractions {
		path = "/v1beta/interactions/" + url.PathEscape(data.ResponseID)
	}
	method := http.MethodGet
	if cancel {
		method = http.MethodPost
		path += "/cancel"
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://gateway.internal"+path, nil)
	if err != nil {
		return nil, 500, err
	}
	r.Header.Set("Accept", "application/json")
	capture := &backgroundResponseWriter{header: make(http.Header)}
	if err := gw.ForwardScope(capture, r, data.Native.Scope, gateway.Session{ID: g.ID, Model: g.Model}); err != nil {
		return nil, 409, err
	}
	body, err := decompressBody(capture.body, capture.header.Get("Content-Encoding"))
	if err != nil || capture.truncated || len(body) > store.MaxConversationBytes {
		return nil, 502, errors.New("invalid native background response")
	}
	if capture.status >= 400 {
		return body, capture.status, errors.New("native background retrieval failed")
	}
	return body, capture.status, nil
}

// nativePollInterval is how often a provider-owned job is re-read. The
// interval is deliberately modest: every poll is a real upstream request on
// the job's own account.
const nativePollInterval = time.Second

// nativeCancelGrace bounds how long a cancelled job may keep answering
// "queued"/"in_progress". Cancellation is asynchronous upstream, so a single
// poll is not evidence that it was ignored.
const nativeCancelGrace = 30 * time.Second

func (s *Server) watchNativeGeneration(r *http.Request, g store.ConversationGeneration, job *conversationBackgroundJob, data *nativeCheckpointData, initial []byte) {
	g.Native, _ = json.Marshal(data)
	g.Result = initial
	g.HTTPStatus = 200
	persist, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	err := s.store.UpdateConversationGeneration(persist, g, "in_progress")
	cancel()
	if err != nil {
		s.failNativeGeneration(r.Context(), g, err)
		return
	}
	var cancelDeadline time.Time
	for {
		ctx := r.Context()
		if ctx.Err() != nil {
			// Shutdown or the job deadline: the provider may still be working,
			// so the outcome here is unknown rather than failed.
			s.interruptNativeGeneration(g, "the gateway stopped watching before the provider finished")
			return
		}
		cancelled := job.cancelRequested.Load()
		// The cancellation poll gets its own budget: the watch context belongs
		// to the job and stays alive until the provider confirms.
		pollCtx, stopPoll := ctx, context.CancelFunc(func() {})
		if cancelled {
			pollCtx, stopPoll = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if cancelDeadline.IsZero() {
				cancelDeadline = time.Now().Add(nativeCancelGrace)
			}
		}
		body, status, err := s.nativeGenerationRequest(pollCtx, g, data, cancelled)
		stopPoll()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				s.interruptNativeGeneration(g, "the gateway stopped watching before the provider finished")
				return
			case gateway.IsTransientScopeError(err):
				// A cooling credential, a saturated one, a rate limit or a 5xx
				// says nothing about the job: keep watching it.
			default:
				s.failNativeGeneration(ctx, g, err)
				return
			}
		} else {
			response, decodeErr := decodeProtocolResponse(data.Native.Scope.Format, body)
			if decodeErr != nil || response.ID != data.ResponseID {
				s.failNativeGeneration(ctx, g, errors.New("native background response identity changed"))
				return
			}
			g.Result = body
			g.HTTPStatus = status
			switch response.Status {
			case "queued", "in_progress", "running":
			case "cancelled", "failed":
				g.State = response.Status
				g.Native = nil
				write, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = s.store.UpdateConversationGeneration(write, g, "in_progress")
				done()
				return
			case "completed", "incomplete":
				if err := s.commitNativeGeneration(ctx, g, data, response, body); err != nil {
					s.failNativeGeneration(ctx, g, err)
				}
				return
			default:
				s.failNativeGeneration(ctx, g, errors.New("unsupported native background lifecycle state"))
				return
			}
		}
		if cancelled && !cancelDeadline.IsZero() && time.Now().After(cancelDeadline) {
			s.failNativeGeneration(ctx, g, errors.New("native provider did not acknowledge cancellation"))
			return
		}
		timer := time.NewTimer(nativePollInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			s.interruptNativeGeneration(g, "the gateway stopped watching before the provider finished")
			return
		}
	}
}

func (s *Server) commitNativeGeneration(ctx context.Context, g store.ConversationGeneration, data *nativeCheckpointData, response *dialect.Response, body []byte) error {
	state := data.State
	state.Append(&dialect.Request{}, response)
	state.Remember(data.Native.Scope, data.Version+1, data.Native.Request, body)
	if err := s.compactConversationMedia(ctx, &state); err != nil {
		return err
	}
	encoded, err := state.Encode()
	if err != nil {
		return err
	}
	token := protocolObjectID("native_lease_")
	write, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err := s.store.LeaseConversation(write, g.ConversationID, g.Owner, data.Version, token, time.Now().Add(time.Minute)); err != nil {
		return err
	}
	defer s.store.ReleaseConversation(context.WithoutCancel(ctx), g.ConversationID, g.Owner, token)
	return s.store.CommitNativeGeneration(write, g, store.Conversation{ID: g.ConversationID, Owner: g.Owner, Model: g.Model, Version: data.Version, State: encoded}, token)
}

func (s *Server) failNativeGeneration(ctx context.Context, g store.ConversationGeneration, cause error) {
	var response any
	if json.Valid(g.Result) {
		_ = json.Unmarshal(g.Result, &response)
	}
	g.State = "failed"
	g.Result, _ = json.Marshal(map[string]any{"error": map[string]any{"type": "native_checkpoint_failed", "message": cause.Error()}, "native_response": response})
	write, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_ = s.store.UpdateConversationGeneration(write, g, "in_progress")
}

// interruptNativeGeneration records that the gateway stopped watching while
// the provider-owned job was still running. The job keeps its own lifecycle
// upstream; its outcome is unknown here, so the record says so instead of
// pretending the generation failed or completed.
func (s *Server) interruptNativeGeneration(g store.ConversationGeneration, cause string) {
	g.State = "interrupted"
	g.Result, _ = json.Marshal(map[string]any{"error": map[string]any{
		"type":    "native_generation_interrupted",
		"message": cause + "; the provider may still finish it, but this checkpoint was not updated",
	}})
	write, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = s.store.UpdateConversationGeneration(write, g, "in_progress")
}

func (s *Server) resumeNativeGeneration(r *http.Request, g *store.ConversationGeneration) error {
	var data nativeCheckpointData
	if json.Unmarshal(g.Native, &data) != nil {
		return errors.New("invalid saved native generation")
	}
	if data.Configuration != s.nativeGenerationConfiguration() {
		return errors.New("native generation configuration changed")
	}
	b := s.backgroundState()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.jobs) >= 32 {
		return errors.New("native generation capacity unavailable")
	}
	if b.jobs[g.ID] != nil {
		return nil
	}
	if err := s.store.ClaimNativeGeneration(r.Context(), g.ID, g.Owner, g.Epoch, b.epoch); err != nil {
		return err
	}
	g.Epoch = b.epoch
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	job := &conversationBackgroundJob{cancel: cancel, done: make(chan struct{})}
	// A cancel poll re-attaches to the same provider job; the watch context
	// itself stays alive so the cancellation request can reach the provider.
	if r.Method == http.MethodPost {
		job.cancelRequested.Store(true)
	}
	b.jobs[g.ID] = job
	copy := r.Clone(ctx)
	saved := *g
	go func() {
		defer close(job.done)
		defer cancel()
		defer func() { b.mu.Lock(); delete(b.jobs, saved.ID); b.mu.Unlock() }()
		stop := func() bool { return false }
		if s.shutdownCtx != nil {
			stop = context.AfterFunc(s.shutdownCtx, cancel)
		}
		defer stop()
		s.watchNativeGeneration(copy, saved, job, &data, bytes.Clone(saved.Result))
	}()
	return nil
}
