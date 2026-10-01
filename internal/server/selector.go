package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type selectorContextKey struct{}

func withSelectorContext(r *http.Request, selectorID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), selectorContextKey{}, selectorID))
}

func selectorFromContext(ctx context.Context) string {
	selectorID, _ := ctx.Value(selectorContextKey{}).(string)
	return selectorID
}

type spilloverTarget struct {
	target  string
	modelID string
	local   bool
}

type selectorSpilloverState struct {
	mu        sync.Mutex
	spillover int
	targets   []spilloverTarget
	inflight  map[string]int
	rr        uint64
}

type selectorSpilloverTracker struct {
	states map[string]*selectorSpilloverState
}

func newSelectorSpilloverTracker(cfg config.Config) *selectorSpilloverTracker {
	tracker := &selectorSpilloverTracker{states: make(map[string]*selectorSpilloverState)}
	for selectorID, selector := range cfg.Selectors {
		if selector.Strategy != config.SelectorStrategySpillover {
			continue
		}
		state := &selectorSpilloverState{
			spillover: selector.Settings.Spillover,
			targets:   make([]spilloverTarget, 0, len(selector.Targets)),
			inflight:  make(map[string]int, len(selector.Targets)),
		}
		for _, target := range selector.Targets {
			modelID, local := cfg.RealModelName(target)
			if !local {
				peerID, peerModelID, _ := cfg.ResolvePeerModel(target)
				modelID = config.PeerModelFQN(peerID, peerModelID)
			}
			state.targets = append(state.targets, spilloverTarget{
				target:  target,
				modelID: modelID,
				local:   local,
			})
		}
		tracker.states[selectorID] = state
	}
	return tracker
}

func (t *selectorSpilloverTracker) release(selectorID, target string) {
	if t == nil {
		return
	}
	state := t.states[selectorID]
	if state == nil {
		return
	}
	state.mu.Lock()
	for _, candidate := range state.targets {
		if candidate.target == target && state.inflight[candidate.modelID] > 0 {
			state.inflight[candidate.modelID]--
			break
		}
	}
	state.mu.Unlock()
}

// CreateSelectorMiddleware resolves selector model IDs after profile rewrites
// and before the normal request context, filters, routing, and metrics pipeline.
func CreateSelectorMiddleware(s *Server) chain.Middleware {
	cfg := s.currentConfig()
	spillovers := newSelectorSpilloverTracker(cfg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(cfg.Selectors) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			model, err := swaputil.ExtractModel(r)
			if err != nil || model == "" {
				next.ServeHTTP(w, r)
				return
			}
			selector, found := cfg.Selectors[model]
			if !found {
				next.ServeHTTP(w, r)
				return
			}
			if !selectorAllowedForIdentity(cfg, identityFromContext(r.Context()), selector) {
				swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for every selector target")
				return
			}
			if pending, ok := r.Context().Value(extensionPendingKey{}).(extensionPendingContext); ok {
				for _, target := range selector.Targets {
					resolved, _ := cfg.RealModelName(target)
					if resolved != pending.record.ModelID {
						continue
					}
					updated, err := swaputil.ReplaceRequestModel(r, model, target)
					if err != nil {
						sendModelRewriteError(w, r, err)
						return
					}
					next.ServeHTTP(w, withSelectorContext(updated, model))
					return
				}
				swaputil.SendResponse(w, r, http.StatusConflict, "extension continuation target is no longer in selector")
				return
			}

			// Failover must receive the untouched request. Rewriting the model
			// before the retry loop would make every subsequent attempt see the
			// first concrete target and silently route all retries to it.
			if selector.Strategy == config.SelectorStrategyFailover {
				target, err := strategyFailover(selector, cfg, s.local.RunningModels())
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusServiceUnavailable, err.Error())
					return
				}
				if swaputil.IsWebSocketUpgrade(r) {
					// A WebSocket handshake commits the transport before an upstream
					// status can be classified. Buffering it for a retry would strip
					// Hijacker from the writer and can corrupt the 101/frame stream;
					// route the selected target once and let the bridge own the
					// connection lifecycle.
					updated, replaceErr := swaputil.ReplaceRequestModel(r, model, target)
					if replaceErr != nil {
						sendModelRewriteError(w, r, replaceErr)
						return
					}
					next.ServeHTTP(w, withSelectorContext(updated, model))
					return
				}
				s.serveFailover(next, w, r, model, selector)
				return
			}

			var target string
			switch selector.Strategy {
			case config.SelectorStrategyPin:
				target, err = strategyPin(selector)
			case config.SelectorStrategyWarm:
				target, err = strategyWarm(cfg, selector, s.local.RunningModels())
			case config.SelectorStrategySpillover:
				target, err = strategySpillover(model, spillovers, s.local.RunningModels())
			default:
				err = fmt.Errorf("unknown selector strategy %q", selector.Strategy)
			}
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusServiceUnavailable, err.Error())
				return
			}

			updated, err := swaputil.ReplaceRequestModel(r, model, target)
			if err != nil {
				if selector.Strategy == config.SelectorStrategySpillover {
					spillovers.release(model, target)
				}
				sendModelRewriteError(w, r, err)
				return
			}

			s.proxylog.Debugf("selector: id=%s target=%s", model, target)

			if selector.Strategy == config.SelectorStrategySpillover {
				modelConfig, _, local := cfg.FindConfig(target)
				if local && modelConfig.Compat.IgnoreWebsockets && swaputil.IsWebSocketUpgrade(updated) {
					// strategySpillover reserves while choosing. Release immediately
					// so a long-lived ignored websocket does not affect later choices.
					spillovers.release(model, target)
				} else {
					defer spillovers.release(model, target)
				}
			}
			next.ServeHTTP(w, withSelectorContext(updated, model))
		})
	}
}

func strategyFailover(selector config.SelectorConfig, cfg config.Config, running map[string]process.ProcessState) (string, error) {
	for _, target := range selector.Targets {
		if _, found := cfg.ResolveBaseModel(target); !found {
			continue
		}
		modelID, local := cfg.RealModelName(target)
		if !local {
			return target, nil
		}
		if state, ok := running[modelID]; !ok || state != process.StateStopping && state != process.StateShutdown {
			return target, nil
		}
	}
	return "", fmt.Errorf("failover selector has no available targets")
}

func (s *Server) serveFailover(next http.Handler, w http.ResponseWriter, r *http.Request, requested string, selector config.SelectorConfig) {
	var original []byte
	readBody := r.Body != nil && r.Method != http.MethodGet
	if r.Body != nil && r.Method != http.MethodGet {
		var readErr error
		original, readErr = io.ReadAll(io.LimitReader(r.Body, backendTransformBodyLimit+1))
		_ = r.Body.Close()
		if readErr != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read failover request body")
			return
		}
		if len(original) > backendTransformBodyLimit {
			swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("failover request body exceeds %d bytes", backendTransformBodyLimit))
			return
		}
	}
	for i, target := range selector.Targets {
		if err := r.Context().Err(); err != nil {
			return
		}
		attempt := r.Clone(r.Context())
		if readBody {
			attempt.Body = io.NopCloser(bytes.NewReader(original))
			attempt.ContentLength = int64(len(original))
		}
		updated, err := swaputil.ReplaceRequestModel(attempt, requested, target)
		if err != nil {
			sendModelRewriteError(w, r, err)
			return
		}
		buffer := newBufferedResponseWriter()
		next.ServeHTTP(buffer, withSelectorContext(updated, requested))
		if buffer.overflow {
			swaputil.SendResponse(w, r, http.StatusBadGateway, fmt.Sprintf("failover response exceeds %d bytes", backendTransformBodyLimit))
			return
		}
		status := buffer.status
		if status == 0 {
			status = http.StatusOK
		}
		// The response is kept private until the attempt finishes. This makes a
		// retry safe even when an upstream wrote an error body, while still
		// ensuring that no bytes from a partial SSE stream have reached the
		// client. Once the final attempt is selected, copy headers and body once.
		// A streamed error may contain a partial event sequence that cannot be
		// safely replayed against another backend. Keep the buffered response and
		// return it as-is instead of issuing a second request. Ordinary empty
		// retryable responses remain eligible for failover.
		retryable := retryableFailoverStatus(status)
		if retryable && strings.Contains(strings.ToLower(buffer.Header().Get("Content-Type")), "text/event-stream") && buffer.body.Len() > 0 {
			retryable = false
		}
		if !retryable || i == len(selector.Targets)-1 {
			copyHeaders(w.Header(), buffer.Header())
			w.WriteHeader(status)
			_, _ = w.Write(buffer.body.Bytes())
			return
		}
	}
}

func retryableFailoverStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func strategyPin(selector config.SelectorConfig) (string, error) {
	if len(selector.Targets) == 0 {
		return "", fmt.Errorf("selector has no targets")
	}
	return selector.Targets[0], nil
}

func strategyWarm(cfg config.Config, selector config.SelectorConfig, running map[string]process.ProcessState) (string, error) {
	if len(selector.Targets) == 0 {
		return "", fmt.Errorf("selector has no targets")
	}

	for _, target := range selector.Targets {
		modelID, _ := cfg.RealModelName(target)
		if running[modelID] == process.StateReady {
			return target, nil
		}
	}
	for _, target := range selector.Targets {
		modelID, _ := cfg.RealModelName(target)
		if running[modelID] == process.StateStarting {
			return target, nil
		}
	}
	return selector.Targets[0], nil
}

func strategySpillover(selectorID string, tracker *selectorSpilloverTracker, running map[string]process.ProcessState) (string, error) {
	if tracker == nil || tracker.states[selectorID] == nil {
		return "", fmt.Errorf("spillover selector %q is not configured", selectorID)
	}
	state := tracker.states[selectorID]
	state.mu.Lock()
	defer state.mu.Unlock()

	active := make([]spilloverTarget, 0, len(state.targets))
	cold := make([]spilloverTarget, 0, len(state.targets))
	for _, target := range state.targets {
		if !target.local {
			if state.inflight[target.modelID] > 0 {
				active = append(active, target)
			} else {
				cold = append(cold, target)
			}
			continue
		}

		processState, runningNow := running[target.modelID]
		switch {
		case processState == process.StateStopping || processState == process.StateShutdown:
			continue
		case processState == process.StateReady || processState == process.StateStarting:
			active = append(active, target)
		case state.inflight[target.modelID] > 0:
			active = append(active, target)
		case !runningNow || processState == process.StateStopped:
			cold = append(cold, target)
		}
	}

	if len(active) == 0 {
		if len(cold) == 0 {
			return "", fmt.Errorf("selector %q has no available spillover targets", selectorID)
		}
		return state.reserve(cold[0]), nil
	}

	minimum := state.minimum(active)
	if minimum < state.spillover {
		return state.reserveLeastBusy(active), nil
	}
	if len(cold) > 0 {
		return state.reserve(cold[0]), nil
	}
	return state.reserveLeastBusy(active), nil
}

func (s *selectorSpilloverState) reserve(target spilloverTarget) string {
	s.inflight[target.modelID]++
	return target.target
}

func (s *selectorSpilloverState) reserveLeastBusy(targets []spilloverTarget) string {
	minimum := s.minimum(targets)
	tied := make([]spilloverTarget, 0, len(targets))
	for _, target := range targets {
		if s.inflight[target.modelID] == minimum {
			tied = append(tied, target)
		}
	}
	target := tied[s.rr%uint64(len(tied))]
	s.rr++
	return s.reserve(target)
}

func (s *selectorSpilloverState) minimum(targets []spilloverTarget) int {
	minimum := -1
	for _, target := range targets {
		if count := s.inflight[target.modelID]; minimum < 0 || count < minimum {
			minimum = count
		}
	}
	return minimum
}
