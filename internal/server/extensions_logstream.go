package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// handleAPIExtensionLogStream tails one extension's log monitor. The wire
// format is the same plain chunked stream the model logs use: history first
// (unless ?no-history), then live records until the client disconnects.
func (s *Server) handleAPIExtensionLogStream(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	id := r.PathValue("id")
	if _, err := m.Get(id); err != nil {
		extensionAPIError(w, r, err)
		return
	}
	if s.extensionLogs == nil {
		s.extensionLogs = newExtensionLogRegistry()
	}
	monitor := s.extensionLogs.monitorFor(id)

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	if r.URL.Query().Get("no-history") == "" {
		if history := monitor.GetHistory(); len(history) > 0 {
			_, _ = w.Write(history)
			flusher.Flush()
		}
	}

	sendChan := make(chan []byte, extensionLogStreamBuffer)
	subscribe := monitor.OnLogData(func(data []byte) {
		select {
		case sendChan <- data:
		default: // slow client: drop rather than block the writer
		}
	})
	defer subscribe()

	stopped := r.Context().Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case data, ok := <-sendChan:
			if !ok {
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// Keepalive keeps proxies from closing idle streams.
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-stopped:
			return
		}
	}
}

// handleAPIExtensionLogs returns the extension's rolling log file contents
// (the persisted store), newest tail first.
func (s *Server) handleAPIExtensionLogs(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	id := r.PathValue("id")
	if _, err := m.Get(id); err != nil {
		extensionAPIError(w, r, err)
		return
	}
	cfg := s.currentConfig()
	logRoot := extensionLogRoot(&cfg)
	data, err := os.ReadFile(filepath.Join(extensionLogDir(logRoot), id+".log"))
	truncated := false
	if err != nil {
		data = []byte("")
	}
	// Tail limit: the newest maxExtensionLogBytes bytes.
	if len(data) > maxExtensionLogBytes {
		data = data[len(data)-maxExtensionLogBytes:]
		truncated = true
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if truncated {
		w.Header().Set("X-Log-Truncated", "true")
	}
	_, _ = w.Write(data)
}

const extensionLogStreamBuffer = 256

// maxExtensionLogBytes caps one history read (512 KiB tail, like logTail.ts).
const maxExtensionLogBytes = 512 << 10

// extensionLogRoot resolves the directory the rotation writes into.
func extensionLogRoot(cfg *config.Config) string {
	if cfg.Store != nil && strings.TrimSpace(cfg.Store.Path) != "" {
		return filepath.Join(filepath.Dir(cfg.Store.Path), "extensions-logs")
	}
	return defaultExtensionLogDir
}
