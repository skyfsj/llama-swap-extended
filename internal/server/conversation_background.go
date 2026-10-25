package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

type conversationBackground struct {
	mu     sync.Mutex
	epoch  string
	closed bool
	jobs   map[string]*conversationBackgroundJob
}
type conversationBackgroundJob struct {
	cancel context.CancelFunc
	// abort interrupts the work this job is currently doing. For a gateway
	// execution that is the in-flight upstream request, so cancellation stops
	// it immediately. A provider-native job leaves it nil: its submit must be
	// allowed to finish and hand back the provider's job id, otherwise the
	// provider keeps running work this gateway can no longer reach.
	abort           context.CancelFunc
	done            chan struct{}
	cancelRequested atomic.Bool
}

func (s *Server) backgroundState() *conversationBackground {
	s.mcpRuntimeMu.Lock()
	defer s.mcpRuntimeMu.Unlock()
	if s.conversationBackground == nil {
		s.conversationBackground = &conversationBackground{epoch: protocolObjectID("run_"), jobs: map[string]*conversationBackgroundJob{}}
	}
	return s.conversationBackground
}

func (s *Server) closeConversationBackground() {
	b := s.backgroundState()
	b.mu.Lock()
	b.closed = true
	jobs := make([]*conversationBackgroundJob, 0, len(b.jobs))
	for _, job := range b.jobs {
		jobs = append(jobs, job)
		job.cancel()
	}
	b.mu.Unlock()
	for _, job := range jobs {
		<-job.done
	}
}

func (s *Server) handleConversationGenerate(w http.ResponseWriter, r *http.Request) {
	c, err := s.conversationForRequest(r)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	var input struct {
		Format    dialect.Format  `json:"format"`
		Request   json.RawMessage `json:"request"`
		Execution string          `json:"execution"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, store.MaxConversationBytes+1))
	if err != nil || len(body) > store.MaxConversationBytes || json.Unmarshal(body, &input) != nil {
		conversationError(w, r, errors.New("invalid background generation"))
		return
	}
	var raw map[string]any
	if json.Unmarshal(input.Request, &raw) != nil || raw == nil {
		conversationError(w, r, errors.New("request object is required"))
		return
	}
	model, _ := raw["model"].(string)
	if model == "" {
		model = c.Model
		raw["model"] = model
	}
	if model == "" || !identityFromContext(r.Context()).HasModel(model) {
		swaputil.SendResponse(w, r, 403, "model is not allowed")
		return
	}
	if stored, ok := raw["store"].(bool); ok && !stored {
		conversationError(w, r, errors.New("background checkpoints require persistent storage"))
		return
	}
	if approvalRequestHasCredentials(raw) {
		conversationError(w, r, errors.New("configure MCP credentials through environment references"))
		return
	}
	path := ""
	switch input.Format {
	case dialect.OpenAIChat:
		path = "/v1/chat/completions"
	case dialect.OpenAIResponses:
		path = "/v1/responses"
	case dialect.ClaudeMessages:
		path = "/v1/messages"
	case dialect.Gemini:
		path = fmt.Sprintf("/v1beta/models/%s:generateContent", model)
	case dialect.GeminiInteractions:
		path = "/v1beta/interactions"
	default:
		conversationError(w, r, errors.New("unsupported background format"))
		return
	}
	if _, _, err := dialect.DecodeRequest(input.Format, input.Request); err != nil {
		conversationError(w, r, err)
		return
	}
	raw["stream"] = false
	native := input.Execution == "native"
	if input.Execution != "" && input.Execution != "gateway" && !native {
		conversationError(w, r, errors.New("unsupported background execution mode"))
		return
	}
	if native {
		if input.Format != dialect.OpenAIResponses && input.Format != dialect.GeminiInteractions {
			conversationError(w, r, errors.New("native background generation requires Responses or Interactions"))
			return
		}
		raw["background"] = true
	} else {
		delete(raw, "background")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	b := s.backgroundState()
	b.mu.Lock()
	if b.closed || len(b.jobs) >= 32 {
		b.mu.Unlock()
		swaputil.SendResponse(w, r, 503, "background generation capacity unavailable")
		return
	}
	g := store.ConversationGeneration{ID: protocolObjectID("ggen_"), Owner: c.Owner, ConversationID: c.ID, Model: model, Epoch: b.epoch, Format: string(input.Format), State: "queued"}
	if err := s.store.CreateConversationGeneration(r.Context(), g); err != nil {
		b.mu.Unlock()
		conversationError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	job := &conversationBackgroundJob{cancel: cancel, done: make(chan struct{})}
	if !native {
		job.abort = cancel
	}
	b.jobs[g.ID] = job
	b.mu.Unlock()
	copy := r.Clone(ctx)
	var nativeCapture *nativeCheckpointCapture
	if native {
		nativeCapture = &nativeCheckpointCapture{}
		copy = copy.WithContext(context.WithValue(copy.Context(), nativeCheckpointKey{}, nativeCapture))
	}
	copy.Method = http.MethodPost
	copy.URL.Path = path
	copy.URL.RawPath = ""
	copy.URL.RawQuery = ""
	copy.Header.Set("Content-Type", "application/json")
	copy.Header.Set(conversationHeader, c.ID)
	copy.Header.Set(conversationVersionHeader, strconv.FormatInt(c.Version, 10))
	if err := setProtocolRequestBody(copy, encoded); err != nil {
		cancel()
		close(job.done)
		b.mu.Lock()
		delete(b.jobs, g.ID)
		b.mu.Unlock()
		conversationError(w, r, err)
		return
	}
	go func() {
		defer close(job.done)
		defer cancel()
		defer func() { b.mu.Lock(); delete(b.jobs, g.ID); b.mu.Unlock() }()
		stop := func() bool { return false }
		if s.shutdownCtx != nil {
			stop = context.AfterFunc(s.shutdownCtx, cancel)
		}
		defer stop()
		g.State = "in_progress"
		startCtx, startCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := s.store.UpdateConversationGeneration(startCtx, g, "queued")
		startCancel()
		if err != nil {
			return
		}
		capture := &backgroundResponseWriter{header: make(http.Header)}
		s.ServeHTTP(capture, copy)
		if nativeCapture != nil && nativeCapture.data != nil && capture.status < 400 && !capture.truncated {
			s.watchNativeGeneration(copy, g, job, nativeCapture.data, capture.body)
			return
		}
		g.HTTPStatus = capture.status
		g.Result = capture.body
		if len(g.Result) > 0 && !json.Valid(g.Result) {
			g.Result, _ = json.Marshal(map[string]any{"error": map[string]any{"message": string(g.Result)}})
		}
		g.State = "failed"
		if capture.header.Get("X-Llama-Swap-Conversation-Status") == "completed" && !capture.truncated {
			g.State = "completed"
		} else if ctx.Err() != nil {
			g.State = "cancelled"
		}
		if capture.truncated {
			g.Result = []byte(`{"error":{"type":"background_result_too_large"}}`)
		}
		persist, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer finishCancel()
		if err := s.store.UpdateConversationGeneration(persist, g, "in_progress"); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("background generation %s outcome persistence failed: %v", g.ID, err)
		}
	}()
	stored, _ := s.store.GetConversationGeneration(r.Context(), g.ID, g.Owner)
	w.Header().Set("Location", "/api/conversation-generations/"+g.ID)
	protocolJSON(w, 202, stored)
}

func (s *Server) handleConversationGeneration(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.GetConversationGeneration(r.Context(), r.PathValue("generation_id"), identityIDFromContext(r.Context()))
	if err != nil || !identityFromContext(r.Context()).HasModel(g.Model) {
		swaputil.SendResponse(w, r, 404, "background generation not found")
		return
	}
	b := s.backgroundState()
	b.mu.Lock()
	job := b.jobs[g.ID]
	if r.Method == http.MethodPost && job != nil {
		job.cancelRequested.Store(true)
		if job.abort != nil {
			job.abort()
		}
	}
	b.mu.Unlock()
	if len(g.Native) > 0 && g.State == "in_progress" && job == nil {
		if err := s.resumeNativeGeneration(r, &g); err != nil {
			// Another gateway process owns the record: leave it alone. Any
			// other failure is this record's own (the saved scope no longer
			// exists, the configuration moved on), so it must not be retried
			// on every poll.
			if !errors.Is(err, store.ErrConversationConflict) {
				s.failNativeGeneration(r.Context(), g, err)
			}
			conversationError(w, r, err)
			return
		}
		protocolJSON(w, 200, g)
		return
	}
	if g.Epoch != b.epoch && (g.State == "queued" || g.State == "in_progress") {
		previous := g.State
		g.State = "interrupted"
		g.Result = []byte(`{"error":{"type":"runtime_restarted","message":"Generation outcome is unknown; inspect the conversation before retrying"}}`)
		if err := s.store.UpdateConversationGeneration(r.Context(), g, previous); err != nil {
			conversationError(w, r, err)
			return
		}
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteConversationGeneration(r.Context(), g.ID, g.Owner); err != nil {
			conversationError(w, r, err)
			return
		}
		protocolJSON(w, 200, map[string]any{"id": g.ID, "deleted": true})
		return
	}
	protocolJSON(w, 200, struct {
		store.ConversationGeneration
		Response json.RawMessage `json:"response,omitempty"`
	}{g, g.Result})
}

type backgroundResponseWriter struct {
	header    http.Header
	status    int
	body      []byte
	truncated bool
}

func (w *backgroundResponseWriter) Header() http.Header { return w.header }
func (w *backgroundResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *backgroundResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if len(w.body)+len(data) <= store.MaxConversationBytes {
		w.body = append(w.body, data...)
	} else {
		w.truncated = true
	}
	return len(data), nil
}
func (w *backgroundResponseWriter) Flush() {}
