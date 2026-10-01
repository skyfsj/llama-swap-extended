package swaputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorStartMarker(t *testing.T) {
	unmarked := httptest.NewRequest(http.MethodGet, "/", nil)
	if IsOperatorStart(unmarked) {
		t.Error("IsOperatorStart on a plain request = true, want false")
	}

	marked := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(WithOperatorStart(context.Background()))
	if !IsOperatorStart(marked) {
		t.Error("IsOperatorStart on a marked request = false, want true")
	}
	if IsOperatorStart(nil) {
		t.Error("IsOperatorStart(nil) = true, want false")
	}
	// Wrapping the context again must not lose the marker: the request passes
	// through several WithContext calls on its way to the router.
	markedCtx := WithOperatorStart(context.Background())
	rewrapped := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(markedCtx)
	rewrapped = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(rewrapped.Context())
	if !IsOperatorStart(rewrapped) {
		t.Error("marker was lost when the context was rewrapped")
	}
}
