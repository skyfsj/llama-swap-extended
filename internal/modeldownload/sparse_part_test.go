package modeldownload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/store"
)

// TestModelDownload_SparsePartWithoutChecksumIsNotFinalized guards the 416
// resume path: a full-size .part that is nothing but holes (a chunk download
// truncated the file, then lost its manifest) must not be accepted on size
// alone when the provider advertises no checksum.
func TestModelDownload_SparsePartWithoutChecksumIsNotFinalized(t *testing.T) {
	root := t.TempDir()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const size = 4 << 20
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Range") != "" {
			// A resumed request for a "complete" part that the server also
			// considers complete: the classic 416 reply.
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(make([]byte, size))
	}))
	defer server.Close()

	final := filepath.Join(root, "model.gguf")
	part := final + ".part"
	file, err := os.Create(part)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate creates the sparse, all-zero file the chunk path leaves behind.
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{store: st}
	provider := downloadProvider{client: server.Client()}
	if _, err := manager.downloadFile(context.Background(), provider, server.URL, final, size, "", nil); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if requests < 2 {
		t.Fatalf("sparse part was accepted without a restart (requests=%d)", requests)
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("final size = %d, want %d", info.Size(), size)
	}
}
