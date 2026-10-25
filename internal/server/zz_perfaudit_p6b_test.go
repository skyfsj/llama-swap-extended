package server

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// logmon level aliases, so the test reads like the logmon API.
const (
	logmonDebugLevel = logmon.LevelDebug
	logmonInfoLevel  = logmon.LevelInfo
	logmonWarnLevel  = logmon.LevelWarn
	logmonErrorLevel = logmon.LevelError
)

// countingStringer counts how many times fmt rendered it, which is what the
// level gating decides.
type countingStringer struct{ calls int }

func (c *countingStringer) String() string {
	c.calls++
	return "x"
}

// newTestLogMonitor returns a monitor that writes into its own history only.
func newTestLogMonitor() *logmon.Monitor {
	return logmon.NewWriter(io.Discard)
}

func testExtensionsConfig() config.ExtensionsConfig {
	cfg := config.ExtensionsConfig{}
	cfg.LogMaxDiskMiB = 8
	cfg.LogRetainDays = 7
	return cfg
}

// TestExtensionLogRotation_AppendReusesOneHandle is the performance property:
// appending many records must open the file once, not once per record. It is
// checked by watching the file's descriptor count through the cached map.
func TestExtensionLogRotation_AppendReusesOneHandle(t *testing.T) {
	dir := t.TempDir()
	rotation := newExtensionLogRotation(dir, testExtensionsConfig())
	defer rotation.close()

	for range 50 {
		rotation.append("ext-a", "line\n")
	}
	rotation.mu.Lock()
	handles := len(rotation.files)
	rotation.mu.Unlock()
	if handles != 1 {
		t.Fatalf("cached %d handles after 50 appends, want 1", handles)
	}

	data, err := os.ReadFile(filepath.Join(dir, "extensions", "ext-a.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Count(string(data), "line\n") != 50 {
		t.Fatalf("log has %d lines, want 50", strings.Count(string(data), "line\n"))
	}
}

// TestExtensionLogRotation_ConcurrentAppendsKeepEveryRecord checks that the
// shared handle keeps every record when several extensions write at once.
func TestExtensionLogRotation_ConcurrentAppendsKeepEveryRecord(t *testing.T) {
	dir := t.TempDir()
	rotation := newExtensionLogRotation(dir, testExtensionsConfig())
	defer rotation.close()

	const extensions = 4
	const perExtension = 25
	var wg sync.WaitGroup
	for e := range extensions {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			id := string(rune('a' + index))
			for range perExtension {
				rotation.append(id, "record\n")
			}
		}(e)
	}
	wg.Wait()

	for e := range extensions {
		id := string(rune('a' + e))
		data, err := os.ReadFile(filepath.Join(dir, "extensions", id+".log"))
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got := strings.Count(string(data), "record\n"); got != perExtension {
			t.Fatalf("%s has %d records, want %d", id, got, perExtension)
		}
	}
}

// TestExtensionLogRotation_PruneReopensTheFile covers the stale-handle case:
// a file removed by prune must be reopened (and recreated) by the next append,
// and the cached descriptor for it must not be reused.
func TestExtensionLogRotation_PruneReopensTheFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testExtensionsConfig()
	rotation := newExtensionLogRotation(dir, cfg)
	defer rotation.close()

	rotation.append("ext-a", "first\n")
	target := filepath.Join(dir, "extensions", "ext-a.log")
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove log: %v", err)
	}
	rotation.append("ext-a", "second\n")

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(data) != "second\n" {
		t.Fatalf("log = %q, want only the record written after the removal", string(data))
	}
}

// TestExtensionLogRotation_PruneClosesRemovedHandles makes sure prune itself
// drops the descriptor of a file it deletes, so the map cannot grow with every
// extension that ever logged.
func TestExtensionLogRotation_PruneClosesRemovedHandles(t *testing.T) {
	dir := t.TempDir()
	cfg := testExtensionsConfig()
	rotation := newExtensionLogRotation(dir, cfg)
	defer rotation.close()

	rotation.append("ext-a", "x\n")
	rotation.append("ext-b", "y\n")
	rotation.mu.Lock()
	handles := len(rotation.files)
	rotation.mu.Unlock()
	if handles != 2 {
		t.Fatalf("cached %d handles, want 2", handles)
	}

	// Backdate both files past the retention window and prune.
	cutoff := time.Now().AddDate(0, 0, -cfg.LogRetainDays-1)
	for _, name := range []string{"ext-a.log", "ext-b.log"} {
		path := filepath.Join(dir, "extensions", name)
		if err := os.Chtimes(path, cutoff, cutoff); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	rotation.prune()

	rotation.mu.Lock()
	handles = len(rotation.files)
	rotation.mu.Unlock()
	if handles != 0 {
		t.Fatalf("cached %d handles after prune, want 0", handles)
	}
	if _, err := os.Stat(filepath.Join(dir, "extensions", "ext-a.log")); !os.IsNotExist(err) {
		t.Fatalf("the pruned log still exists: %v", err)
	}
}

// TestExtensionLogRegistry_RotationIsSharedAndRebuilt pins the registry-level
// caching: the same configuration returns one rotation, and a changed log root
// rebuilds it (dropping the old handles) rather than writing to a stale path.
func TestExtensionLogRegistry_RotationIsSharedAndRebuilt(t *testing.T) {
	registry := newExtensionLogRegistry()
	defer registry.close()

	cfg := testExtensionsConfig()
	first := registry.rotationFor("/tmp/logs-one", cfg)
	if first == nil {
		t.Fatal("rotationFor returned nil")
	}
	if again := registry.rotationFor("/tmp/logs-one", cfg); again != first {
		t.Fatal("the same configuration must return the same rotation")
	}

	second := registry.rotationFor("/tmp/logs-two", cfg)
	if second == first {
		t.Fatal("a changed log root must rebuild the rotation")
	}
	if filepath.Base(second.dir) != "extensions" || filepath.Dir(second.dir) != "/tmp/logs-two" {
		t.Fatalf("rotation dir = %q, want the new root", second.dir)
	}
}

// TestLogMonitor_FormattedVariantsSkipDisabledLevels is the level-gating
// property: a disabled level must not format its message, which is what the
// caller's arguments observe.
func TestLogMonitor_FormattedVariantsSkipDisabledLevels(t *testing.T) {
	monitor := newTestLogMonitor()
	monitor.SetLogLevel(logmonWarnLevel)

	if monitor.Enabled(logmonDebugLevel) || monitor.Enabled(logmonInfoLevel) {
		t.Fatal("debug and info must be disabled at warn level")
	}
	if !monitor.Enabled(logmonWarnLevel) || !monitor.Enabled(logmonErrorLevel) {
		t.Fatal("warn and error must be enabled at warn level")
	}

	// The argument is a Stringer, which fmt only renders when the message is
	// actually built: a disabled level must leave it untouched. (A plain
	// function call would be evaluated by Go before the method is entered, so
	// it cannot observe the gating.)
	probe := &countingStringer{}
	monitor.Debugf("debug %s", probe)
	monitor.Infof("info %s", probe)
	if probe.calls != 0 {
		t.Fatalf("disabled levels rendered their argument %d times, want 0", probe.calls)
	}
	monitor.Warnf("warn %s", probe)
	monitor.Errorf("error %s", probe)
	if probe.calls != 2 {
		t.Fatalf("enabled levels rendered their argument %d times, want 2", probe.calls)
	}

	// The enabled output must be byte-identical to the unformatted path.
	plain := newTestLogMonitor()
	plain.SetLogLevel(logmonWarnLevel)
	formatted := newTestLogMonitor()
	formatted.SetLogLevel(logmonWarnLevel)
	plain.Warn("warn hello")
	formatted.Warnf("warn %s", "hello")
	if string(plain.GetHistory()) != string(formatted.GetHistory()) {
		t.Fatalf("formatted output differs:\n%q\n%q", plain.GetHistory(), formatted.GetHistory())
	}
}
