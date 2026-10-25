package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_ResponseHistoryRestartAndOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withIdentity(context.Background(), auth.Identity{ID: "owner"})
	r := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	persistResponseAffinity(r, st, "m", "vllm", []byte(`{"model":"m","instructions":"OLD_INSTRUCTIONS","input":"FIRST_USER","store":true}`), []byte(`{"id":"resp_first","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"FIRST_ANSWER"}]}]}`), 200)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	body := []byte(`{"model":"m","instructions":"NEW_INSTRUCTIONS","input":"SECOND_USER","previous_response_id":"resp_first"}`)
	out, err := convertProtocolRequest(ctx, protocolrelaykit.NewMeta("m", false), protocolrelaykit.OpenAIResponses, protocolrelaykit.OpenAIChat, body, "m", st)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "OLD_INSTRUCTIONS") || !strings.Contains(text, "NEW_INSTRUCTIONS") {
		t.Fatalf("instruction policy: %s", text)
	}
	first, answer, second := strings.Index(text, "FIRST_USER"), strings.Index(text, "FIRST_ANSWER"), strings.Index(text, "SECOND_USER")
	if first < 0 || answer <= first || second <= answer {
		t.Fatalf("history order after restart: %s", text)
	}
	other := withIdentity(context.Background(), auth.Identity{ID: "other"})
	if _, err := convertProtocolRequest(other, protocolrelaykit.NewMeta("m", false), protocolrelaykit.OpenAIResponses, protocolrelaykit.OpenAIChat, body, "m", st); err == nil {
		t.Fatal("foreign key resumed a stored conversation")
	}
	r2 := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	persistResponseAffinity(r2, st, "m", "vllm", []byte(`{"model":"m","input":"PRIVATE_USER","store":false}`), []byte(`{"id":"resp_private","status":"completed","output":[]}`), 200)
	if _, found, err := st.GetResponseAffinity(ctx, "resp_private", time.Now()); err != nil || found {
		t.Fatalf("store:false persisted history: found=%t err=%v", found, err)
	}
}

// A conversation long enough to exceed the stored-row budget must not cost the
// client the response itself: the history is trimmed to what fits, and the
// follow-up still resumes with the most recent turns.
func TestServer_ResponseHistoryTrimmedInsteadOfDropped(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := withIdentity(context.Background(), auth.Identity{ID: "owner"})
	// Each message is large enough that only the last few of them fit under
	// the row budget together with the response.
	message := strings.Repeat("h", 900*1024)
	var input strings.Builder
	input.WriteString(`[`)
	for index := 0; index < 6; index++ {
		if index > 0 {
			input.WriteString(",")
		}
		input.WriteString(`{"role":"user","content":"` + message + `-` + string(rune('A'+index)) + `"}`)
	}
	input.WriteString(`]`)
	request := []byte(`{"model":"m","input":` + input.String() + `,"store":true}`)
	r := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	persistResponseAffinity(r, st, "m", "vllm", request, []byte(`{"id":"resp_long","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ANSWER"}]}]}`), 200)

	affinity, found, err := st.GetResponseAffinity(ctx, "resp_long", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("an oversized history dropped the response row entirely")
	}
	if len(affinity.Request) == 0 {
		t.Fatal("no history survived the trim")
	}
	if len(affinity.Request)+len(affinity.Response) > maxAffinityResponseBytes {
		t.Fatalf("stored row exceeds the budget: history=%d response=%d", len(affinity.Request), len(affinity.Response))
	}
	// The newest turn must be the one that survives, because that is the part
	// a follow-up needs to reproduce.
	if !strings.Contains(string(affinity.Request), "-F") {
		t.Fatalf("the oldest turns were kept instead of the newest: %.80s", affinity.Request)
	}
	if strings.Contains(string(affinity.Request), "-A") {
		t.Fatalf("an oldest turn survived the trim: %.80s", affinity.Request)
	}
}

func TestServer_ResponseHistoryTrimDropsEverythingWhenOneMessageExceedsBudget(t *testing.T) {
	history := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 2048) + `"}]}`)
	if got := trimAffinityHistory(history, "resp_x", 64); got != nil {
		t.Fatalf("a single unrepresentable message should leave no history, got %d bytes", len(got))
	}
	if got := trimAffinityHistory(history, "resp_x", 1<<20); len(got) == 0 {
		t.Fatal("a history that fits was dropped")
	}
}
