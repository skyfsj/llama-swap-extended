package pricing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestSyncStoresExactCatalogPricesAndUsesETag(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"provider":{"models":{"m":{"cost":{"input":1,"output":2,"cache_read":0.1}}}}}`))
	}))
	defer srv.Close()
	s := &Syncer{Store: st, URL: srv.URL}
	if result, err := s.Sync(context.Background()); err != nil || result.Count != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	prices, err := st.FindPrices(context.Background(), "provider", "m")
	if err != nil || len(prices) != 1 || prices[0].Input != 1 {
		t.Fatalf("prices=%+v err=%v", prices, err)
	}
}

func TestSyncFailureKeepsLastSuccessfulPricingSnapshot(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("ETag", `"good"`)
			_, _ = w.Write([]byte(`{"provider":{"models":{"m":{"cost":{"input":1}}}}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"provider":`))
	}))
	defer srv.Close()
	s := &Syncer{Store: st, URL: srv.URL}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("malformed catalog unexpectedly succeeded")
	}
	prices, err := st.FindPrices(context.Background(), "provider", "m")
	if err != nil || len(prices) != 1 || prices[0].Input != 1 {
		t.Fatalf("last successful snapshot lost: prices=%+v err=%v", prices, err)
	}
	meta, err := st.GetPricingMeta(context.Background())
	if err != nil || meta.ETag != `"good"` {
		t.Fatalf("last successful metadata lost: meta=%+v err=%v", meta, err)
	}
}

func TestSyncNullCatalogKeepsLastSuccessfulPricingSnapshot(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("ETag", `"good"`)
			_, _ = w.Write([]byte(`{"provider":{"models":{"m":{"cost":{"input":1}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`null`))
	}))
	defer srv.Close()
	s := &Syncer{Store: st, URL: srv.URL}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "catalog must be an object") {
		t.Fatalf("null catalog error = %v", err)
	}
	prices, err := st.FindPrices(context.Background(), "provider", "m")
	if err != nil || len(prices) != 1 || prices[0].Input != 1 {
		t.Fatalf("last successful snapshot lost after null catalog: prices=%+v err=%v", prices, err)
	}
	meta, err := st.GetPricingMeta(context.Background())
	if err != nil || meta.ETag != `"good"` {
		t.Fatalf("last successful metadata lost after null catalog: meta=%+v err=%v", meta, err)
	}
}

func TestSyncConcurrentCallsSerializeETagAndSnapshot(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var active atomic.Int32
	var maxActive atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maxActive.Load()
			if current <= observed || maxActive.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"provider":{"models":{"m":{"cost":{"input":1}}}}}`))
	}))
	defer srv.Close()
	s := &Syncer{Store: st, URL: srv.URL}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, syncErr := s.Sync(context.Background())
			errs <- syncErr
		}()
	}
	wg.Wait()
	close(errs)
	for syncErr := range errs {
		if syncErr != nil {
			t.Fatalf("concurrent sync failed: %v", syncErr)
		}
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("pricing sync requests overlapped: max active=%d", got)
	}
	prices, err := st.FindPrices(context.Background(), "provider", "m")
	if err != nil || len(prices) != 1 || prices[0].Input != 1 {
		t.Fatalf("concurrent sync lost snapshot: prices=%+v err=%v", prices, err)
	}
}

func TestSyncHonorsDeadlineWhenCatalogStalls(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	requestStarted := make(chan struct{})
	var startedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(requestStarted) })
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = (&Syncer{Store: st, URL: srv.URL}).Sync(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled sync error = %v, want context deadline exceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("stalled sync ignored deadline: elapsed=%s", elapsed)
	}
	select {
	case <-requestStarted:
	default:
		t.Fatal("pricing sync did not issue its catalog request")
	}
}

func TestSyncNilReceiverReturnsError(t *testing.T) {
	var syncer *Syncer
	if _, err := syncer.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "syncer is required") {
		t.Fatalf("nil syncer error = %v", err)
	}
}

func TestSyncRejectsRedirectsAndOversizedCatalogs(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	if _, err := (&Syncer{Store: st, URL: source.URL}).Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect response error = %v", err)
	}
	if redirected {
		t.Fatal("pricing sync followed a redirect")
	}

	client := &http.Client{Transport: roundTripPricingFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&repeatingPricingReader{remaining: maxPricingCatalogBytes + 1}),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	if _, err := (&Syncer{Store: st, URL: "https://models.dev/api.json", Client: client}).Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "catalog exceeds") {
		t.Fatalf("oversized catalog error = %v", err)
	}
}

func TestSyncSkipsUnsafeCatalogIdentities(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"good provider": {"models": {"good model": {"cost": {"input": 1}}}},
			"bad\u200bprovider": {"models": {"model": {"cost": {"input": 2}}}},
			"another": {"models": {"bad\u00a0model": {"cost": {"input": 3}}, " padded ": {"cost": {"input": 4}}}}
		}`))
	}))
	defer srv.Close()
	if _, err := (&Syncer{Store: st, URL: srv.URL}).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	prices, err := st.FindPrices(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 1 || prices[0].Provider != "good provider" || prices[0].Model != "good model" {
		t.Fatalf("unsafe catalog identities were persisted: %+v", prices)
	}
}

func TestSafePricingIdentityBoundsAndWhitespace(t *testing.T) {
	for _, value := range []string{"provider", "model name", "claude-3.5"} {
		if !safePricingIdentity(value) {
			t.Fatalf("safe pricing identity rejected %q", value)
		}
	}
	for _, value := range []string{"", " padded ", "model\u00a0name", "model\u200bname", "model\nname", strings.Repeat("x", maxPricingIdentityBytes+1)} {
		if safePricingIdentity(value) {
			t.Fatalf("unsafe pricing identity accepted %q", value)
		}
	}
}

type roundTripPricingFunc func(*http.Request) (*http.Response, error)

func (f roundTripPricingFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type repeatingPricingReader struct {
	remaining int64
}

func (r *repeatingPricingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= n
	return int(n), nil
}
