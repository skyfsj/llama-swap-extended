package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ScopeError reports a request that could not be forwarded on a pinned route
// scope. A caller polling a provider-owned job separates the two cases: a
// transient failure (a credential cooling down or busy, a transport error, a
// rate limit) may succeed on the next poll and must not abandon the job, while
// a permanent one (the provider, adapter, model or credential left the
// configuration) never will.
type ScopeError struct {
	Permanent bool
	Status    int
	Message   string
}

func (e *ScopeError) Error() string { return e.Message }

// IsTransientScopeError reports whether err is a forward failure worth
// retrying.
func IsTransientScopeError(err error) bool {
	var scopeErr *ScopeError
	return errors.As(err, &scopeErr) && !scopeErr.Permanent
}

// ForwardScope forwards one request on the exact route and credential a
// previous request used. Provider-owned background jobs can only be read or
// cancelled by the account that started them, so this must never re-enter the
// pool scheduler: a rotated provider or credential would answer 404 or, worse,
// a different account's job.
func (g *Gateway) ForwardScope(w http.ResponseWriter, r *http.Request, scope conversation.Scope, session Session) error {
	var selected *Route
	var credential *Credential
	g.mu.RLock()
	for _, route := range g.routes {
		model := route.RemoteName
		if model == "" {
			model = route.ModelID
		}
		candidate := routeScope(route, model)
		if candidate.Provider != scope.Provider || candidate.Adapter != scope.Adapter ||
			candidate.Model != scope.Model || candidate.Format != scope.Format {
			continue
		}
		for _, key := range route.Members {
			if key.ID == scope.Credential {
				selected, credential = route, key
				break
			}
		}
		if credential != nil {
			break
		}
	}
	g.mu.RUnlock()
	if selected == nil || credential == nil {
		return &ScopeError{Permanent: true, Message: "native generation source configuration or credential changed"}
	}
	if !credential.available(time.Now()) {
		return &ScopeError{Message: "native generation source credential is cooling down"}
	}
	release, ok := credential.acquire()
	if !ok {
		return &ScopeError{Message: "native generation source credential is busy"}
	}
	defer release()
	result := g.attempt(w, r, selected, credential, session, AttemptOptions{})
	switch result.outcome {
	case outcomeCommit:
		return nil
	case outcomeStop:
		if err := r.Context().Err(); err != nil {
			return err
		}
		return &ScopeError{Permanent: true, Status: result.status, Message: "native generation request stopped: " + result.cause}
	}
	status := result.status
	if status < 400 {
		status = http.StatusBadGateway
	}
	snippet := result.snippet
	if snippet == "" {
		snippet = result.cause
	}
	swaputil.SendResponse(w, r, status, snippet)
	return &ScopeError{
		Permanent: !retryableScopeStatus(status),
		Status:    status,
		Message:   fmt.Sprintf("native generation request failed (%d): %s", status, snippet),
	}
}

// retryableScopeStatus reports whether an upstream status can change on its
// own. Authentication and not-found answers will not: the pinned source is
// gone or no longer authorized.
func retryableScopeStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}
