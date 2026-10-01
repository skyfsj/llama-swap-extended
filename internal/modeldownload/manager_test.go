package modeldownload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func nilDownloadContext() context.Context { return nil }

func TestModelDownload_QueueRetriesAndResumes(t *testing.T) {
	content := []byte("resumable model payload")
	hash := sha256.Sum256(content)
	root := t.TempDir()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var ranges []string
	var resolveAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/acme/demo":
			_ = json.NewEncoder(w).Encode(hfModelInfo{
				SHA:      "abc123",
				Siblings: []hfFileEntry{{RFilename: "model.gguf", Size: int64(len(content)), LFS: &hfLFSMeta{Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])}}},
			})
		case r.URL.Path == "/acme/demo/resolve/main/model.gguf":
			mu.Lock()
			ranges = append(ranges, r.Header.Get("Range"))
			resolveAttempts++
			attempt := resolveAttempts
			mu.Unlock()
			start := 0
			if r.Header.Get("Range") != "" {
				rangeValue := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "-")
				parsed, parseErr := strconv.Atoi(rangeValue)
				if parseErr != nil {
					http.Error(w, "invalid range", http.StatusBadRequest)
					return
				}
				start = parsed
			}
			if attempt == 1 {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				_, _ = w.Write(content[:5])
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(content)-start))
			if start > 0 {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(content[start:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	progressEvents := make(chan swaputil.BackendProgressEvent, 32)
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:      &enabled,
			Workers:      1,
			MaxRetries:   2,
			RetryBackoff: time.Millisecond,
			HFBaseURL:    server.URL,
		},
		Progress: func(event swaputil.BackendProgressEvent) {
			select {
			case progressEvents <- event:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	task, duplicate, err := queue.Enqueue(ctx, Request{RepoID: "acme/demo", SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate || task.Status != store.ModelDownloadQueued {
		t.Fatalf("initial task = %+v, duplicate=%v", task, duplicate)
	}
	duplicateTask, duplicate, err := queue.Enqueue(ctx, Request{RepoID: "acme/demo", SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || duplicateTask.ID != task.ID {
		t.Fatalf("duplicate task = %+v, duplicate=%v", duplicateTask, duplicate)
	}

	deadline := time.Now().Add(5 * time.Second)
	var completed store.ModelDownloadTask
	for time.Now().Before(deadline) {
		completed, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != store.ModelDownloadCompleted {
		t.Fatalf("task did not complete: %+v", completed)
	}
	got, err := os.ReadFile(filepath.Join(root, "acme", "demo", "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("downloaded content = %q, want %q", got, content)
	}
	phases := make(map[string]bool)
	var lastDownloading swaputil.BackendProgressEvent
	for {
		select {
		case event := <-progressEvents:
			if event.Model != "acme/demo" {
				t.Fatalf("progress model = %q, want acme/demo", event.Model)
			}
			phases[event.Phase] = true
			if event.Phase == "downloading" {
				lastDownloading = event
			}
		default:
			goto drained
		}
	}
drained:
	for _, phase := range []string{"checking", "downloading", "completed"} {
		if !phases[phase] {
			t.Fatalf("progress phases = %#v, missing %q", phases, phase)
		}
	}
	if lastDownloading.Completed != int64(len(content)) || lastDownloading.Total != int64(len(content)) {
		t.Fatalf("download byte progress = %d/%d, want %d/%d", lastDownloading.Completed, lastDownloading.Total, len(content), len(content))
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	if len(gotRanges) < 2 || gotRanges[0] != "" || gotRanges[1] != "bytes=5-" {
		t.Fatalf("request ranges = %v, want initial request and resumed bytes=5-", gotRanges)
	}
}

func TestModelDownload_DownloadsRepositoryFilesConcurrently(t *testing.T) {
	payloads := map[string][]byte{
		"config.json":     []byte(`{"model_type":"demo"}`),
		"model-00001.bin": []byte("first model shard"),
		"model-00002.bin": []byte("second model shard"),
		"tokenizer.json":  []byte(`{"version":"1.0"}`),
	}
	siblings := make([]hfFileEntry, 0, len(payloads))
	totalBytes := int64(0)
	for filename, payload := range payloads {
		siblings = append(siblings, hfFileEntry{RFilename: filename, Size: int64(len(payload))})
		totalBytes += int64(len(payload))
	}
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].RFilename < siblings[j].RFilename })

	started := make(chan string, len(payloads))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDownloads := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseDownloads()
	var requestMu sync.Mutex
	activeRequests := 0
	peakRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/acme/parallel":
			_ = json.NewEncoder(w).Encode(hfModelInfo{SHA: "parallel123", Siblings: siblings})
		case strings.HasPrefix(r.URL.Path, "/acme/parallel/resolve/main/"):
			filename := strings.TrimPrefix(r.URL.Path, "/acme/parallel/resolve/main/")
			payload, ok := payloads[filename]
			if !ok {
				http.NotFound(w, r)
				return
			}
			requestMu.Lock()
			activeRequests++
			if activeRequests > peakRequests {
				peakRequests = activeRequests
			}
			requestMu.Unlock()
			started <- filename
			<-release
			requestMu.Lock()
			activeRequests--
			requestMu.Unlock()
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:      &enabled,
			Workers:      1,
			FileWorkers:  3,
			MaxRetries:   1,
			RetryBackoff: time.Millisecond,
			HFBaseURL:    server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	task, duplicate, err := queue.Enqueue(ctx, Request{RepoID: "acme/parallel", SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first parallel download was treated as a duplicate")
	}

	seen := make(map[string]bool)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(seen) < 3 {
		select {
		case filename := <-started:
			seen[filename] = true
		case <-timer.C:
			releaseDownloads()
			t.Fatalf("only %d file requests started concurrently: %v", len(seen), seen)
		}
	}

	current, found, err := st.GetModelDownload(ctx, task.ID)
	if err != nil || !found {
		releaseDownloads()
		t.Fatalf("active task = %+v, found=%v, err=%v", current, found, err)
	}
	if strings.Count(current.CurrentFile, " · ") < 2 {
		releaseDownloads()
		t.Fatalf("current files = %q, want at least three concurrent files", current.CurrentFile)
	}
	releaseDownloads()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current.Status != store.ModelDownloadCompleted {
		t.Fatalf("parallel task did not complete: %+v", current)
	}
	if current.CompletedFiles != len(payloads) || current.DownloadedBytes != totalBytes {
		t.Fatalf("parallel progress = %d files, %d bytes; want %d files, %d bytes", current.CompletedFiles, current.DownloadedBytes, len(payloads), totalBytes)
	}
	requestMu.Lock()
	peak := peakRequests
	requestMu.Unlock()
	if peak < 3 {
		t.Fatalf("peak concurrent file requests = %d, want at least 3", peak)
	}
	for filename, want := range payloads {
		got, err := os.ReadFile(filepath.Join(root, "acme", "parallel", filename))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("downloaded %s = %q, want %q", filename, got, want)
		}
	}
}

func TestModelDownload_RetriesTransientTaskFailure(t *testing.T) {
	content := []byte("automatic retry payload")
	root := t.TempDir()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	metadataRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/acme/retry":
			mu.Lock()
			metadataRequests++
			attempt := metadataRequests
			mu.Unlock()
			if attempt <= 2 {
				http.Error(w, "temporary provider outage", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(hfModelInfo{SHA: "retry123", Siblings: []hfFileEntry{{RFilename: "model.gguf", Size: int64(len(content))}}})
		case "/acme/retry/resolve/main/model.gguf":
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:        &enabled,
			Workers:        1,
			MaxRetries:     1,
			MaxTaskRetries: 1,
			RetryBackoff:   10 * time.Millisecond,
			HFBaseURL:      server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	task, _, err := queue.Enqueue(ctx, Request{RepoID: "acme/retry", SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	seenRetrying := false
	var current store.ModelDownloadTask
	for time.Now().Before(deadline) {
		current, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == store.ModelDownloadRetrying {
			seenRetrying = true
			if current.NextRetryAt.IsZero() || current.Error == "" {
				t.Fatalf("retrying task = %+v, want retry time and visible error", current)
			}
		}
		if current.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !seenRetrying || current.Status != store.ModelDownloadCompleted || current.Attempts != 2 {
		t.Fatalf("automatic retry result = %+v, saw retry=%v", current, seenRetrying)
	}
	if got, err := os.ReadFile(filepath.Join(root, "acme", "retry", "model.gguf")); err != nil || string(got) != string(content) {
		t.Fatalf("retried model file = %q, err=%v", got, err)
	}
}

func TestModelDownload_ChunksLargeRangeFileConcurrently(t *testing.T) {
	content := bytes.Repeat([]byte("chunked-model-data\n"), 220000)
	hash := sha256.Sum256(content)
	root := t.TempDir()
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan int, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var requestMu sync.Mutex
	activeRequests := 0
	peakRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/acme/chunked":
			_ = json.NewEncoder(w).Encode(hfModelInfo{SHA: "chunked123", Siblings: []hfFileEntry{{RFilename: "model.gguf", Size: int64(len(content)), LFS: &hfLFSMeta{Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])}}}})
		case r.URL.Path == "/acme/chunked/resolve/main/model.gguf":
			rangeValue := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
			bounds := strings.SplitN(rangeValue, "-", 2)
			if len(bounds) != 2 {
				http.Error(w, "missing range", http.StatusBadRequest)
				return
			}
			start, startErr := strconv.ParseInt(bounds[0], 10, 64)
			end, endErr := strconv.ParseInt(bounds[1], 10, 64)
			if startErr != nil || endErr != nil || start < 0 || end < start || end >= int64(len(content)) {
				http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if start > 0 {
				requestMu.Lock()
				activeRequests++
				if activeRequests > peakRequests {
					peakRequests = activeRequests
				}
				requestMu.Unlock()
				started <- int(start)
				<-release
				requestMu.Lock()
				activeRequests--
				requestMu.Unlock()
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:           &enabled,
			Workers:           1,
			FileWorkers:       1,
			ChunkWorkers:      3,
			ChunkSizeMiB:      1,
			ChunkThresholdMiB: 1,
			MaxRetries:        1,
			RetryBackoff:      time.Millisecond,
			HFBaseURL:         server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	task, _, err := queue.Enqueue(ctx, Request{RepoID: "acme/chunked", SourceID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for len(seen) < 3 {
		select {
		case start := <-started:
			seen[start] = true
		case <-timer.C:
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("only %d chunks started concurrently: %v", len(seen), seen)
		}
	}
	releaseOnce.Do(func() { close(release) })

	deadline := time.Now().Add(5 * time.Second)
	var current store.ModelDownloadTask
	for time.Now().Before(deadline) {
		current, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current.Status != store.ModelDownloadCompleted {
		t.Fatalf("chunked task did not complete: %+v", current)
	}
	requestMu.Lock()
	peak := peakRequests
	requestMu.Unlock()
	if peak < 3 {
		t.Fatalf("parallel chunk requests = %d, want at least 3", peak)
	}
	if got, err := os.ReadFile(filepath.Join(root, "acme", "chunked", "model.gguf")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("chunked model = %d bytes, err=%v", len(got), err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme", "chunked", "model.gguf.part.chunks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("chunk manifest remains after completion: %v", err)
	}
}

func TestModelDownload_ControlMethodsAcceptNilContext(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := New(Config{Store: st, Sources: sources, Settings: config.ModelDownloadsConfig{HFBaseURL: "http://127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.List(nilDownloadContext(), 10, 0); err != nil {
		t.Fatalf("nil-context list: %v", err)
	}
	if _, found, err := queue.Get(nilDownloadContext(), "missing"); err != nil || found {
		t.Fatalf("nil-context get = found %v err %v", found, err)
	}
	if err := queue.Cancel(nilDownloadContext(), "missing"); err == nil {
		t.Fatal("cancel of missing task unexpectedly succeeded")
	}
	if err := queue.Retry(nilDownloadContext(), "missing"); err == nil {
		t.Fatal("retry of missing task unexpectedly succeeded")
	}
}

func TestModelDownload_SafeJoinRejectsSymlinkedParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(parent, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(filepath.Join(linkedRoot, "hub"), "models--acme--demo/blobs/model"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("safeJoin accepted symlinked parent: %v", err)
	}
}

func TestModelDownload_HFCacheLayout(t *testing.T) {
	content := []byte("hf cache model payload")
	hash := sha256.Sum256(content)
	cacheRoot := filepath.Join(t.TempDir(), "hub")
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"hf": {Type: config.ModelFileSourceHFCache, Path: cacheRoot},
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/acme/demo":
			_ = json.NewEncoder(w).Encode(hfModelInfo{
				SHA: "commit123",
				Siblings: []hfFileEntry{{
					RFilename: "model.gguf",
					Size:      int64(len(content)),
					LFS:       &hfLFSMeta{Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:])},
				}},
			})
		case "/acme/demo/resolve/main/model.gguf":
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:      &enabled,
			Workers:      1,
			MaxRetries:   0,
			RetryBackoff: time.Millisecond,
			HFBaseURL:    server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	task, _, err := queue.Enqueue(ctx, Request{RepoID: "acme/demo", SourceID: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var completed store.ModelDownloadTask
	for time.Now().Before(deadline) {
		completed, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != store.ModelDownloadCompleted {
		t.Fatalf("HF cache task did not complete: %+v", completed)
	}

	repoRoot := filepath.Join(cacheRoot, "models--acme--demo")
	blob := filepath.Join(repoRoot, "blobs", hex.EncodeToString(hash[:]))
	snapshot := filepath.Join(repoRoot, "snapshots", "commit123", "model.gguf")
	if got, err := os.ReadFile(blob); err != nil || string(got) != string(content) {
		t.Fatalf("HF blob = %q, err=%v, want %q", got, err, content)
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != string(content) {
		t.Fatalf("HF snapshot = %q, err=%v, want %q", got, err, content)
	}
	if got, err := os.ReadFile(filepath.Join(repoRoot, "refs", "main")); err != nil || string(got) != "commit123\n" {
		t.Fatalf("HF ref = %q, err=%v, want commit123", got, err)
	}
}

func TestModelDownload_ModelScopeCacheLayout(t *testing.T) {
	content := []byte("modelscope cache model payload")
	hash := sha256.Sum256(content)
	cacheRoot := filepath.Join(t.TempDir(), "modelscope")
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"modelscope": {Type: config.ModelFileSourceMSCache, Path: cacheRoot},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MODELSCOPE_TOKEN", "environment-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer configured-secret" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/models/acme/demo/repo/files":
			if r.URL.Query().Get("Revision") != "master" || r.URL.Query().Get("Recursive") != "true" {
				t.Errorf("metadata query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Code": 200, "Success": true,
				"Data": map[string]any{"Files": []map[string]any{
					{"Path": "model.safetensors", "Revision": "commit456", "Sha256": hex.EncodeToString(hash[:]), "Size": len(content), "Type": "blob"},
				}},
			})
		case "/api/v1/models/acme/demo/repo":
			if r.URL.Query().Get("Revision") != "master" || r.URL.Query().Get("FilePath") != "model.safetensors" {
				t.Errorf("download query = %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	enabled := true
	queue, err := New(Config{
		Store:   st,
		Sources: sources,
		Settings: config.ModelDownloadsConfig{
			Enabled:            &enabled,
			Workers:            1,
			MaxRetries:         0,
			RetryBackoff:       time.Millisecond,
			ModelScopeBaseURL:  server.URL,
			ModelScopeTokenEnv: "TEST_MODELSCOPE_TOKEN",
			ModelScopeToken:    "configured-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	task, duplicate, err := queue.Enqueue(ctx, Request{Provider: ProviderModelScope, RepoID: "acme/demo"})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate || task.Provider != ProviderModelScope || task.Revision != "master" || task.SourceID != "modelscope" {
		t.Fatalf("queued task = %+v, duplicate=%v", task, duplicate)
	}

	deadline := time.Now().Add(5 * time.Second)
	var completed store.ModelDownloadTask
	for time.Now().Before(deadline) {
		completed, _, err = st.GetModelDownload(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status == store.ModelDownloadCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != store.ModelDownloadCompleted {
		t.Fatalf("ModelScope cache task did not complete: %+v", completed)
	}

	target := filepath.Join(cacheRoot, "models", "acme--demo", "snapshots", "commit456", "model.safetensors")
	if got, err := os.ReadFile(target); err != nil || string(got) != string(content) {
		t.Fatalf("ModelScope snapshot = %q, err=%v, want %q", got, err, content)
	}
	catalog, err := sources.List(ctx, modelmanager.ListOptions{SourceID: "modelscope", Limit: 20})
	if err != nil || len(catalog.Data) != 1 || catalog.Data[0].Repository != "acme/demo" || catalog.Data[0].Revision != "commit456" {
		t.Fatalf("ModelScope catalog = %+v, err=%v", catalog, err)
	}
}

func TestModelDownload_SelectsIncludeAndExclude(t *testing.T) {
	entries := []hfFileEntry{
		{RFilename: "model.gguf", Size: 1},
		{RFilename: "tokenizer.json", Size: 1},
		{RFilename: "docs/readme.md", Size: 1},
	}
	selected, err := selectSiblings(entries, []string{"*.gguf", "*.json"}, []string{"tokenizer.*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].RFilename != "model.gguf" {
		t.Fatalf("selected = %+v", selected)
	}
}

func TestModelDownload_RejectsUnsafeRepositoryPaths(t *testing.T) {
	for _, value := range []string{"/model.gguf", "../model.gguf", "models/../../model.gguf", "models/../model.gguf", "models//model.gguf", "models/./model.gguf", "models\\model.gguf", "models/\u200bmodel.gguf", "models/\u202emodel.gguf", "models/\u00a0model.gguf", "models/model\x00.gguf"} {
		if _, err := safeRepositoryPath(value); err == nil {
			t.Errorf("safeRepositoryPath(%q) accepted unsafe path", value)
		}
	}
	if _, err := normalizeRequest(Request{RepoID: "acme/demo", Include: []string{"["}}); err == nil || !strings.Contains(err.Error(), "invalid glob") {
		t.Fatalf("normalizeRequest invalid glob error = %v", err)
	}
}

func TestModelDownload_RejectsUnsafeRequestMetadata(t *testing.T) {
	for _, value := range []string{"acme/\u200bmodel", "acme/\u202emodel", "acme/\u00a0model", "acme/model name"} {
		if _, err := normalizeRequest(Request{RepoID: value}); err == nil {
			t.Errorf("normalizeRequest(%q) accepted unsafe repo id", value)
		}
	}
	for _, value := range []string{"rev\u200b", "rev\u202e", "rev\u00a0", "rev name"} {
		if _, err := normalizeRequest(Request{RepoID: "acme/model", Revision: value}); err == nil {
			t.Errorf("normalizeRequest revision %q accepted unsafe value", value)
		}
	}
	for _, value := range []string{"\u200b*.gguf", "\u202e*.gguf", "\u00a0*.gguf"} {
		if _, err := normalizeRequest(Request{RepoID: "acme/model", Include: []string{value}}); err == nil {
			t.Errorf("normalizeRequest pattern %q accepted unsafe value", value)
		}
	}
	if _, err := normalizeRequest(Request{RepoID: "acme/model", Include: []string{"model file*.gguf"}}); err != nil {
		t.Fatalf("normalizeRequest rejected a legal ASCII-space glob: %v", err)
	}
}

func TestModelDownload_SaturatingProgressTotals(t *testing.T) {
	if got := saturatingDownloadBytes(0, 10); got != 10 {
		t.Fatalf("initial total = %d, want 10", got)
	}
	if got := saturatingDownloadBytes(math.MaxInt64-1, 10); got != math.MaxInt64 {
		t.Fatalf("overflowing total = %d, want MaxInt64", got)
	}
	if got := saturatingDownloadBytes(-1, 10); got != 10 {
		t.Fatalf("negative total = %d, want 10", got)
	}
	if got := saturatingDownloadBytes(5, -10); got != 5 {
		t.Fatalf("negative increment changed total to %d", got)
	}
}

func TestModelDownload_RejectsMetadataBaseURL(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sources, err := modelmanager.New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: t.TempDir()},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://metadata/",
		"https://metadata.google.internal/",
		"https://instance-data/",
		"http://169.254.169.254/",
		"http://169.254.170.2/",
		"http://100.100.100.200/",
		"http://[fd00:ec2::254]/",
		"http://2852039166/",
	} {
		_, err := New(Config{
			Store:   st,
			Sources: sources,
			Settings: config.ModelDownloadsConfig{
				HFBaseURL: raw,
			},
		})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "metadata") {
			t.Errorf("New(HFBaseURL=%q) error = %v, want metadata rejection", raw, err)
		}
	}
}

func TestModelDownload_RedirectGuardRejectsPrivateDestinations(t *testing.T) {
	base, err := url.Parse("https://huggingface.co")
	if err != nil {
		t.Fatal(err)
	}
	client := guardedDownloadClient(&http.Client{}, base.String())
	for _, raw := range []string{
		"http://127.0.0.1/internal",
		"http://2130706433/internal",
		"http://0x7f000001/internal",
		"http://192.168.1.10/internal",
		"http://[::1]/internal",
		"http://metadata.google.internal/compute",
	} {
		destination, parseErr := url.Parse(raw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if err := client.CheckRedirect(&http.Request{URL: destination}, nil); err == nil {
			t.Fatalf("redirect to %s was accepted", raw)
		}
	}
	public, err := url.Parse("https://cdn.example.test/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(&http.Request{URL: public}, nil); err != nil {
		t.Fatalf("public CDN redirect rejected: %v", err)
	}
}

func TestModelDownload_RedirectGuardAllowsConfiguredMirrorHostOnly(t *testing.T) {
	base := "http://127.0.0.1:8080"
	client := guardedDownloadClient(nil, base)
	sameHost, err := url.Parse("http://127.0.0.1:9000/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(&http.Request{URL: sameHost}, nil); err != nil {
		t.Fatalf("same configured mirror host rejected: %v", err)
	}
	otherPrivate, err := url.Parse("http://127.0.0.2/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(&http.Request{URL: otherPrivate}, nil); err == nil {
		t.Fatal("redirect to another private host was accepted")
	}
}

func TestModelDownload_RedirectGuardRejectsHTTPSDowngrade(t *testing.T) {
	base := "https://huggingface.co"
	client := guardedDownloadClient(nil, base)
	destination, err := url.Parse("http://huggingface.co/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(&http.Request{URL: destination}, nil); err == nil {
		t.Fatal("HTTPS to HTTP redirect was accepted")
	}
	secure, err := url.Parse("https://cdn.example.test/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(&http.Request{URL: secure}, nil); err != nil {
		t.Fatalf("HTTPS public CDN redirect rejected: %v", err)
	}
	plainBase := guardedDownloadClient(nil, "http://mirror.example.test")
	upgrade, err := url.Parse("https://cdn.example.test/model.gguf")
	if err != nil {
		t.Fatal(err)
	}
	if err := plainBase.CheckRedirect(&http.Request{URL: upgrade}, nil); err != nil {
		t.Fatalf("HTTP to HTTPS redirect rejected: %v", err)
	}
}

func TestModelDownload_NumericIPv4Parser(t *testing.T) {
	for _, test := range []struct {
		host string
		want net.IP
	}{
		{host: "2130706433", want: net.IPv4(127, 0, 0, 1)},
		{host: "0x7f000001", want: net.IPv4(127, 0, 0, 1)},
		{host: "127.1", want: net.IPv4(127, 0, 0, 1)},
		{host: "08.0.0.1", want: net.IPv4(8, 0, 0, 1)},
	} {
		got, ok := numericDownloadIPv4(test.host)
		if !ok || !got.Equal(test.want) {
			t.Fatalf("numericDownloadIPv4(%q) = %v, %v; want %v, true", test.host, got, ok, test.want)
		}
	}
}

func TestModelDownload_NumericIPv4AmbiguousSpellingsFailClosed(t *testing.T) {
	for _, host := range []string{
		"0127.0.0.1",          // octal 87.0.0.1 or decimal 127.0.0.1
		"0177.0.0.1",          // octal 127.0.0.1 or decimal 177.0.0.1
		"0251.0376.0251.0376", // octal 169.254.169.254 or decimal interpretation
	} {
		if !restrictedDownloadHost(host) {
			t.Errorf("ambiguous private numeric host %q was accepted", host)
		}
	}
	for _, host := range []string{"[::1%25lo0]", "[fe80::1%25eth0]", "[::1%]"} {
		if !restrictedDownloadHost(host) {
			t.Errorf("zoned private IPv6 host %q was accepted", host)
		}
	}
	if restrictedDownloadHost("08.0.0.1") {
		t.Fatal("public decimal numeric host was treated as restricted")
	}
}
