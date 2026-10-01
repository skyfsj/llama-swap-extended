package server

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// extensionLogRegistry owns one logmon monitor per extension id. Monitors are
// created lazily and survive extension reloads (the monitor outlives any one
// Compiled instance); monitors for deleted extensions are dropped on prune.
type extensionLogRegistry struct {
	mu       sync.Mutex
	monitors map[string]*logmon.Monitor
}

func newExtensionLogRegistry() *extensionLogRegistry {
	return &extensionLogRegistry{monitors: map[string]*logmon.Monitor{}}
}

// monitorFor returns (creating if needed) the monitor for an extension.
func (r *extensionLogRegistry) monitorFor(id string) *logmon.Monitor {
	r.mu.Lock()
	defer r.mu.Unlock()
	if monitor, ok := r.monitors[id]; ok {
		return monitor
	}
	monitor := logmon.NewWriter(io.Discard)
	r.monitors[id] = monitor
	return monitor
}

// prune drops monitors whose extension no longer exists.
func (r *extensionLogRegistry) prune(existing map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, monitor := range r.monitors {
		if !existing[id] {
			monitor.Close()
			delete(r.monitors, id)
		}
	}
}

// pruneExisting drops monitors whose extension no longer exists, based on the
// manager's current id list. Called after reload and delete so a 1 MiB buffer
// + broadcast goroutine per removed extension cannot accumulate.
func (s *Server) pruneExistingLogMonitors() {
	if s.extensionLogs == nil {
		return
	}
	existing := map[string]bool{}
	for _, item := range s.currentExtensions().List() {
		existing[item.Manifest.ID] = true
	}
	s.extensionLogs.prune(existing)
}

func formatExtensionLog(record extensions.LogRecord) string {
	ts := record.Timestamp.Format("2006-01-02 15:04:05.000")
	// Multi-line messages (exception stacks) have continuation lines escaped
	// with a leading space so a crafted message cannot forge the framing.
	message := strings.ReplaceAll(strings.ReplaceAll(record.Message, "\r\n", "\n"), "\n", "\n  ")
	return "[" + ts + "] [" + strings.ToUpper(record.Level) + "] " + message + "\n"
}

// extensionLogDir resolves the on-disk log directory for extension logs.
func extensionLogDir(base string) string {
	return filepath.Join(base, "extensions")
}

// startPruner runs the retention prune on an interval until stop is closed.
// It is started with the registry so the rolling store cannot grow unbounded.
func (r *extensionLogRotation) startPruner(stop <-chan struct{}, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.prune()
			case <-stop:
				return
			}
		}
	}()
}

// extensionLogRotation holds the rolling per-extension log files. Structure
// mirrors logarchive: bounded count of files, each capped, atomic writes.
type extensionLogRotation struct {
	mu         sync.Mutex
	dir        string
	maxDiskMiB int64
	retainDays int
}

func newExtensionLogRotation(dir string, cfg config.ExtensionsConfig) *extensionLogRotation {
	return &extensionLogRotation{
		dir:        extensionLogDir(dir),
		maxDiskMiB: int64(cfg.EffectiveLogMaxDiskMiB()),
		retainDays: cfg.EffectiveLogRetainDays(),
	}
}

// append writes one record to the extension's rolling log file. Level
// filtering happens at the call site (the middleware decides what persists).
func (r *extensionLogRotation) append(extensionID string, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return
	}
	target := filepath.Join(r.dir, extensionID+".log")
	file, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(line)
}

// prune enforces the retention policy: delete files older than retainDays and
// trim total size under maxDiskMiB (oldest first).
func (r *extensionLogRotation) prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -r.retainDays)
	type entryInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	var infos []entryInfo
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(r.dir, entry.Name()))
			continue
		}
		infos = append(infos, entryInfo{filepath.Join(r.dir, entry.Name()), info.ModTime(), info.Size()})
	}
	limit := r.maxDiskMiB << 20
	if total <= limit {
		return
	}
	// Oldest first until under the cap.
	sort.Slice(infos, func(i, j int) bool { return infos[i].modTime.Before(infos[j].modTime) })
	for _, info := range infos {
		if total <= limit {
			break
		}
		_ = os.Remove(info.path)
		total -= info.size
	}
}

// defaultExtensionLogDir is where extension logs land when no store path is
// configured (keyless/embedded runs).
const defaultExtensionLogDir = "logs/extensions"

// logLevelPersisted reports whether a level is written to the rolling store.
// The live SSE stream is unaffected by this filter; default persists
// info/warn/error (debug stays live-only) until operators change it.
func logLevelPersisted(level string) bool {
	switch strings.ToLower(level) {
	case "info", "warn", "error":
		return true
	default:
		return false
	}
}
