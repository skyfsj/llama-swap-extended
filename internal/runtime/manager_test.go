package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type testProvider struct{ failHealth bool }

type progressProvider struct{}

type versionHealthProvider struct{ failVersion string }

type typedNilProvider struct{}

type identityMismatchProvider struct{}

type checkPersistenceFailureProvider struct{ statePath string }

type stageFailureProvider struct {
	journalPath string
	mutate      bool
}

type blockingStageProvider struct {
	started chan struct{}
	release chan struct{}
}

type updateTestProvider struct {
	testProvider
	candidate string
}

func (p updateTestProvider) CheckForUpdate(_ context.Context, _ Manifest, desired Spec, _ UpdatePolicy) (Spec, bool, error) {
	desired.Version = p.candidate
	return desired, true, nil
}

func (p *blockingStageProvider) Stage(_ context.Context, spec Spec, destination string) (Manifest, error) {
	close(p.started)
	<-p.release
	return (testProvider{}).Stage(context.Background(), spec, destination)
}

func (*blockingStageProvider) Verify(context.Context, Manifest, string) error { return nil }

func (*blockingStageProvider) Health(context.Context, Manifest, string) error { return nil }

func TestRuntime_ManagerListDoesNotWaitForProviderStage(t *testing.T) {
	provider := &blockingStageProvider{started: make(chan struct{}), release: make(chan struct{})}
	manager, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"blocking": provider})
	if err != nil {
		t.Fatal(err)
	}

	stageDone := make(chan error, 1)
	go func() {
		_, stageErr := manager.Stage(context.Background(), Spec{Name: "vllm", Kind: "blocking", Version: "1", Source: "fixture"})
		stageDone <- stageErr
	}()
	<-provider.started

	listDone := make(chan []Status, 1)
	go func() { listDone <- manager.List() }()
	select {
	case statuses := <-listDone:
		if len(statuses) != 1 || statuses[0].Name != "vllm" || statuses[0].State != StateBuilding {
			t.Fatalf("statuses=%+v, want vllm in BUILDING state", statuses)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runtime list waited for provider stage to finish")
	}

	close(provider.release)
	if err := <-stageDone; err != nil {
		t.Fatalf("stage failed: %v", err)
	}
}

func TestRuntime_RewriteVLLMEntrypointShebangsAfterStageRename(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "versions", ".staging-123")
	newDir := filepath.Join(root, "versions", "0.0.1")
	binDir := filepath.Join(newDir, ".venv", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(binDir, "vllm")
	original := "#!" + filepath.Join(oldDir, ".venv", "bin", "python") + "\nprint('ok')\n"
	if err := os.WriteFile(entrypoint, []byte(original), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := rewriteVenvEntrypointShebangs(newDir, oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(entrypoint)
	if err != nil {
		t.Fatal(err)
	}
	want := "#!" + filepath.Join(newDir, ".venv", "bin", "python") + "\nprint('ok')\n"
	if string(content) != want {
		t.Fatalf("rewritten entrypoint=%q want %q", content, want)
	}
	if info, err := os.Stat(entrypoint); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("entrypoint mode=%v err=%v, want 0750", info.Mode().Perm(), err)
	}
}

func (*typedNilProvider) Stage(context.Context, Spec, string) (Manifest, error) {
	return Manifest{}, nil
}
func (*typedNilProvider) Verify(context.Context, Manifest, string) error { return nil }
func (*typedNilProvider) Health(context.Context, Manifest, string) error { return nil }

func TestRuntime_ProviderValidationRejectsTypedNilAndUnsafeNames(t *testing.T) {
	if !isNilProvider((*typedNilProvider)(nil)) {
		t.Fatal("typed-nil provider was not detected")
	}
	if isNilProvider(&typedNilProvider{}) {
		t.Fatal("non-nil provider was classified as nil")
	}
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{
		"test": (*typedNilProvider)(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []Spec{
		{Name: "runtime name", Kind: "test", Version: "1.0"},
		{Name: "runtime", Kind: "test", Version: "1 0"},
		{Name: "runtime\u00a0name", Kind: "test", Version: "1.0"},
		{Name: "runtime\u200bname", Kind: "test", Version: "1.0"},
		{Name: "runtime", Kind: "test", Version: "1\u2003.0"},
		{Name: "runtime", Kind: "test", Version: strings.Repeat("x", 256)},
	} {
		if _, err := m.Stage(context.Background(), spec); err == nil {
			t.Fatalf("unsafe runtime spec unexpectedly succeeded: %+v", spec)
		}
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "runtime", Kind: "test", Version: "1.0"}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("typed-nil provider stage error = %v, want not registered", err)
	}
}

func TestRuntime_NilManagerOptionalMethodsAreSafe(t *testing.T) {
	var manager *Manager
	manager.SetIdleProbe(func() bool { return true })
	manager.SetBuildWhileBusy(true)
	if err := manager.SetProvider("test", testProvider{}); err == nil {
		t.Fatal("nil manager provider replacement unexpectedly succeeded")
	}
	if err := manager.Unconfigure("runtime"); err == nil {
		t.Fatal("nil manager unconfigure unexpectedly succeeded")
	}
	if got := manager.Root(); got != "" {
		t.Fatalf("nil manager root = %q, want empty", got)
	}
	if got := manager.List(); got != nil {
		t.Fatalf("nil manager list = %#v, want nil", got)
	}
	if status, ok := manager.Get("runtime"); ok || status != (Status{}) {
		t.Fatalf("nil manager get = %+v, found=%v", status, ok)
	}
	if _, err := manager.Check(context.Background(), "runtime"); err == nil {
		t.Fatal("nil manager check unexpectedly succeeded")
	}
	if _, ok, err := manager.Detail("runtime"); ok || err == nil {
		t.Fatalf("nil manager detail = found=%v err=%v", ok, err)
	}
	if err := manager.Register("runtime", "test"); err == nil {
		t.Fatal("nil manager register unexpectedly succeeded")
	}
	if _, err := manager.Stage(context.Background(), Spec{Name: "runtime", Kind: "test", Version: "1"}); err == nil {
		t.Fatal("nil manager stage unexpectedly succeeded")
	}
	if err := manager.Activate(context.Background(), "runtime", "1"); err == nil {
		t.Fatal("nil manager activate unexpectedly succeeded")
	}
	if err := manager.Rollback(context.Background(), "runtime"); err == nil {
		t.Fatal("nil manager rollback unexpectedly succeeded")
	}
	if err := manager.Pin("runtime", "1"); err == nil {
		t.Fatal("nil manager pin unexpectedly succeeded")
	}
	if err := manager.Unpin("runtime"); err == nil {
		t.Fatal("nil manager unpin unexpectedly succeeded")
	}
	if err := manager.RemoveVersion("runtime", "1"); err == nil {
		t.Fatal("nil manager remove unexpectedly succeeded")
	}
}

func TestRuntime_ManagerCallbackSettersAreConcurrentSafe(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 256; i++ {
			m.SetIdleProbe(func() bool { return true })
			m.SetBuildWhileBusy(i%2 == 0)
			m.SetProgress(func(swaputil.BackendProgressEvent) {})
		}
	}()
	for i := 0; i < 256; i++ {
		m.emitProgress("runtime", "checking", float64(i)/256, "callback race", nil)
		_ = m.idleNow()
		_ = m.buildWhileBusyAllowed()
	}
	<-done
}

func (p testProvider) Stage(_ context.Context, spec Spec, destination string) (Manifest, error) {
	if err := os.WriteFile(filepath.Join(destination, "runtime.bin"), []byte(spec.Version), 0o750); err != nil {
		return Manifest{}, err
	}
	return Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}
func (testProvider) Verify(_ context.Context, _ Manifest, directory string) error {
	_, err := os.Stat(filepath.Join(directory, "runtime.bin"))
	return err
}
func (p testProvider) Health(_ context.Context, _ Manifest, _ string) error {
	if p.failHealth {
		return os.ErrInvalid
	}
	return nil
}

func (p checkPersistenceFailureProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	return (testProvider{}).Stage(ctx, spec, destination)
}

func (checkPersistenceFailureProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	return (testProvider{}).Verify(ctx, manifest, directory)
}

func (p checkPersistenceFailureProvider) Health(context.Context, Manifest, string) error {
	if err := os.Remove(p.statePath); err != nil {
		return err
	}
	return os.Mkdir(p.statePath, 0o750)
}

func (p stageFailureProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	if _, err := (testProvider{}).Stage(ctx, spec, destination); err != nil {
		return Manifest{}, err
	}
	if p.mutate {
		if err := os.Remove(p.journalPath); err != nil {
			return Manifest{}, err
		}
		if err := os.Mkdir(p.journalPath, 0o750); err != nil {
			return Manifest{}, err
		}
	}
	return Manifest{}, errors.New("provider stage failed")
}

func (p stageFailureProvider) Verify(context.Context, Manifest, string) error { return nil }

func (p stageFailureProvider) Health(context.Context, Manifest, string) error { return nil }

func (identityMismatchProvider) Stage(_ context.Context, spec Spec, _ string) (Manifest, error) {
	return Manifest{Name: spec.Name, Version: spec.Version, Kind: "other", Source: spec.Source}, nil
}

func (identityMismatchProvider) Verify(context.Context, Manifest, string) error { return nil }

func (identityMismatchProvider) Health(context.Context, Manifest, string) error { return nil }

func (progressProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	reportRuntimeProgressBytes(ctx, "download-chunk", 0.31, 31, 100, "downloaded fixture chunk")
	if err := os.WriteFile(filepath.Join(destination, "runtime.bin"), []byte(spec.Version), 0o750); err != nil {
		return Manifest{}, err
	}
	reportRuntimeProgress(ctx, "build-step", 0.67, "compiled fixture")
	return Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}

func (progressProvider) Verify(_ context.Context, _ Manifest, directory string) error {
	_, err := os.Stat(filepath.Join(directory, "runtime.bin"))
	return err
}

func (progressProvider) Health(context.Context, Manifest, string) error { return nil }

func (p versionHealthProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	return (testProvider{}).Stage(ctx, spec, destination)
}

func (versionHealthProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	return (testProvider{}).Verify(ctx, manifest, directory)
}

func (p versionHealthProvider) Health(_ context.Context, manifest Manifest, _ string) error {
	if manifest.Version == p.failVersion {
		return fmt.Errorf("version %s is unhealthy", manifest.Version)
	}
	return nil
}

// cancelingProvider deliberately ignores cancellation and then cancels the
// caller after completing a provider phase. Manager-level post-phase checks
// must still prevent a canceled operation from committing pointers or a
// staged version.
type cancelingProvider struct {
	cancel context.CancelFunc
	phase  string
}

func (p cancelingProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	manifest, err := (testProvider{}).Stage(ctx, spec, destination)
	if err == nil && p.phase == "stage" && p.cancel != nil {
		p.cancel()
	}
	return manifest, err
}

func (p cancelingProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	if p.phase == "verify" && p.cancel != nil {
		p.cancel()
	}
	return (testProvider{}).Verify(ctx, manifest, directory)
}

func (p cancelingProvider) Health(ctx context.Context, manifest Manifest, directory string) error {
	return (testProvider{}).Health(ctx, manifest, directory)
}

func TestRuntime_ManagerStagesActivatesAndReloadsState(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	progressEvents := make(chan swaputil.BackendProgressEvent, 32)
	m.SetProgress(func(event swaputil.BackendProgressEvent) {
		select {
		case progressEvents <- event:
		default:
		}
	})
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1.0.0", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "vllm", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	status, ok := m.Get("vllm")
	if !ok || status.State != StateActive || status.Current != "1.0.0" || status.Source != "fixture" {
		t.Fatalf("status = %+v, found=%v", status, ok)
	}
	if target, err := os.Readlink(filepath.Join(root, "runtimes", "vllm", "current")); err != nil || target != filepath.Join("versions", "1.0.0") {
		t.Fatalf("current link = %q, err=%v", target, err)
	}
	phases := make(map[string]bool)
	for {
		select {
		case event := <-progressEvents:
			if event.Runtime != "vllm" {
				t.Fatalf("progress runtime = %q, want vllm", event.Runtime)
			}
			phases[event.Phase] = true
		default:
			goto drained
		}
	}
drained:
	for _, phase := range []string{"staging", "downloading", "building", "verifying", "staged", "activating", "health_check", "active"} {
		if !phases[phase] {
			t.Fatalf("progress phases = %#v, missing %q", phases, phase)
		}
	}
	reloaded, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	status, ok = reloaded.Get("vllm")
	if !ok || status.Current != "1.0.0" || status.State != StateActive {
		t.Fatalf("reloaded status = %+v, found=%v", status, ok)
	}
}

func TestRuntime_ManagerForwardsProviderProgress(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"progress": progressProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan swaputil.BackendProgressEvent, 32)
	m.SetProgress(func(event swaputil.BackendProgressEvent) { events <- event })
	if _, err := m.Stage(context.Background(), Spec{Name: "runtime", Kind: "progress", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]swaputil.BackendProgressEvent{}
	for {
		select {
		case event := <-events:
			seen[event.Phase] = event
		default:
			if _, ok := seen["download-chunk"]; !ok {
				t.Fatalf("provider progress was not forwarded: %#v", seen)
			}
			if _, ok := seen["build-step"]; !ok {
				t.Fatalf("provider build progress was not forwarded: %#v", seen)
			}
			if seen["download-chunk"].Progress != 0.31 || seen["build-step"].Progress != 0.67 {
				t.Fatalf("provider progress values=%#v", seen)
			}
			if seen["download-chunk"].Completed != 31 || seen["download-chunk"].Total != 100 {
				t.Fatalf("provider byte progress=%+v", seen["download-chunk"])
			}
			return
		}
	}
}

func TestRuntime_ManagerOperationSinkMirrorsDurableTransitions(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	var operations []Operation
	m.SetOperationSink(func(operation Operation) {
		operations = append(operations, operation)
	})
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1.0", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if len(operations) < 5 {
		t.Fatalf("operation sink received %d transitions, want staging phases", len(operations))
	}
	first := operations[0]
	last := operations[len(operations)-1]
	if first.ID == "" || first.Action != "stage" || first.State != StateStaging {
		t.Fatalf("first operation=%+v", first)
	}
	if last.ID != first.ID || last.Action != "stage" || last.State != StateStaged || last.Version != "1.0" {
		t.Fatalf("last operation=%+v, want final state for same operation", last)
	}
}

func TestRuntime_ManagerStageStopsWhenJournalCannotBeWritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the journal path is manager-owned but cannot be opened for
	// append. Stage must fail before invoking the provider rather than silently
	// proceeding with an operation whose state transitions are not durable.
	journalPath := filepath.Join(root, "demo", "operations.jsonl")
	if err := os.MkdirAll(journalPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "staging transition") {
		t.Fatalf("Stage error = %v, want durable journal failure", err)
	}
	if _, ok := m.Get("demo"); ok {
		t.Fatal("failed Stage left an in-memory status")
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "1", "runtime.bin")); !os.IsNotExist(err) {
		t.Fatalf("failed Stage invoked provider or retained candidate: %v", err)
	}
}

func TestRuntime_ManagerStageFailurePropagatesJournalError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	journalPath := filepath.Join(root, "demo", "operations.jsonl")
	m, err := NewManager(root, map[string]Provider{"test": stageFailureProvider{journalPath: journalPath, mutate: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err == nil || !strings.Contains(err.Error(), "record runtime staging failure") {
		t.Fatalf("Stage error = %v, want provider and journal persistence errors", err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateDegraded || !strings.Contains(status.LastError, "provider stage failed") {
		t.Fatalf("status = %+v, found=%v, want durable degraded provider failure", status, ok)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "1")); !os.IsNotExist(err) {
		t.Fatalf("failed provider stage committed candidate: %v", err)
	}
}

func TestRuntime_ManagerActivateStopsWhenJournalCannotBeWritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	before, ok := m.Get("demo")
	if !ok || before.State != StateStaged {
		t.Fatalf("staged status = %+v, found=%v", before, ok)
	}
	journalPath := filepath.Join(root, "demo", "operations.jsonl")
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "activation transition") {
		t.Fatalf("Activate error = %v, want durable journal failure", err)
	}
	if got, ok := m.Get("demo"); !ok || got != before {
		t.Fatalf("failed Activate changed status: before=%+v after=%+v found=%v", before, got, ok)
	}
	if _, err := os.Lstat(filepath.Join(root, "demo", "current")); !os.IsNotExist(err) {
		t.Fatalf("failed Activate changed current pointer: %v", err)
	}
}

func TestRuntime_ManagerRollbackStopsWhenJournalCannotBeWritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	before, ok := m.Get("demo")
	if !ok || before.State != StateActive || before.Current != "2" || before.Previous != "1" {
		t.Fatalf("active status = %+v, found=%v", before, ok)
	}

	journalPath := filepath.Join(root, "demo", "operations.jsonl")
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journalPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := m.Rollback(context.Background(), "demo"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "rollback transition") {
		t.Fatalf("Rollback error = %v, want durable journal failure", err)
	}
	if got, ok := m.Get("demo"); !ok || got != before {
		t.Fatalf("failed Rollback changed status: before=%+v after=%+v found=%v", before, got, ok)
	}
	current, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || current != filepath.Join("versions", "2") {
		t.Fatalf("failed Rollback changed current pointer: %q, err=%v", current, err)
	}
}

func TestRuntime_ReadOperationsIgnoresTruncatedFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	valid, err := json.Marshal(Operation{ID: "op-1", Name: "demo", Action: "stage", State: StateStaged})
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(valid, '\n'), []byte(`{"id":"op-2","name":"demo","action":"activate"`)...)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	operations, err := readOperations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != "op-1" {
		t.Fatalf("operations=%+v, want only the complete record", operations)
	}
}

func TestRuntime_MetadataReadersAndJournalRejectSymlinkedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register("demo", "test"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo")
	outside := t.TempDir()
	for _, name := range []string{"state.json", "manifest.json", "operations.jsonl"} {
		if err := os.WriteFile(filepath.Join(outside, name), []byte(`{"id":"outside"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	statePath := filepath.Join(dir, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "state.json"), statePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := m.readState("demo"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("readState error=%v, want symlink rejection", err)
	}
	manifestPath := filepath.Join(outside, "manifest.json")
	if _, err := readManifest(manifestPath); err != nil {
		t.Fatalf("readManifest outside regular file: %v", err)
	}
	manifestLink := filepath.Join(dir, "manifest.json")
	if err := os.Symlink(manifestPath, manifestLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readManifest(manifestLink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("readManifest error=%v, want symlink rejection", err)
	}
	operationsPath := filepath.Join(dir, "operations.jsonl")
	if err := os.Symlink(filepath.Join(outside, "operations.jsonl"), operationsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readOperations(operationsPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("readOperations error=%v, want symlink rejection", err)
	}
	if err := m.appendOperation(Operation{ID: "op-1", Name: "demo", Action: "stage", State: StateStaged}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("appendOperation error=%v, want symlink rejection", err)
	}
}

func TestRuntime_ManifestMetadataIsBounded(t *testing.T) {
	directory := t.TempDir()
	over := bytes.Repeat([]byte("x"), int(maxRuntimeManifestBytes)+1)
	path := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(path, over, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized manifest read error = %v, want bounded rejection", err)
	}
	if err := writeManifest(directory, Manifest{Metadata: map[string]string{"payload": strings.Repeat("x", int(maxRuntimeManifestBytes))}}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized manifest write error = %v, want bounded rejection", err)
	}
}

func TestRuntime_StateMetadataIsBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "demo")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(maxRuntimeStateBytes)+1), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := m.readState("demo"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized state read error = %v, want bounded rejection", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	state := persistedState{Status: Status{Name: "demo", State: StateIdle}, Versions: map[string]Manifest{
		"1": {Name: "demo", Version: "1", Kind: "test", Source: "fixture", Metadata: map[string]string{"payload": strings.Repeat("x", int(maxRuntimeStateBytes))}},
	}}
	if err := m.writeState("demo", state); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized state write error = %v, want bounded rejection", err)
	}
}

func TestRuntime_StatusStateDoesNotMaskCorruptOrSymlinkedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register("demo", "test"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "demo", "state.json")
	if err := os.WriteFile(statePath, []byte("{not-json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Detail("demo"); err == nil || !strings.Contains(err.Error(), "decode runtime demo state") {
		t.Fatalf("Detail corrupt state error=%v, want decode error", err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(outside, []byte(`{"status":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, statePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := m.Detail("demo"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Detail symlinked state error=%v, want symlink rejection", err)
	}
}

func TestRuntime_ReadOperationsRejectsCorruptCompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	valid, err := json.Marshal(Operation{ID: "op-1", Name: "demo", Action: "stage", State: StateStaged})
	if err != nil {
		t.Fatal(err)
	}
	data := append(append(valid, '\n'), []byte("not-json\n")...)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readOperations(path); err == nil || !strings.Contains(err.Error(), "decode runtime operation journal") {
		t.Fatalf("readOperations error=%v, want complete-record corruption", err)
	}
}

func TestRuntime_ReadOperationsBoundsLargeJournalToRecentTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	var data []byte
	for i := 0; i < 110; i++ {
		op := Operation{
			ID:        fmt.Sprintf("op-%03d", i),
			Name:      "demo",
			Action:    "stage",
			State:     StateStaged,
			Error:     strings.Repeat("x", 80_000),
			Timestamp: time.Unix(int64(i), 0).UTC(),
		}
		line, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	operations, err := readOperations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 100 {
		t.Fatalf("read %d operations, want bounded recent 100", len(operations))
	}
	if operations[0].ID != "op-010" || operations[len(operations)-1].ID != "op-109" {
		t.Fatalf("tail operations=%q..%q, want op-010..op-109", operations[0].ID, operations[len(operations)-1].ID)
	}
}

func TestRuntime_ReadOperationsPreservesExactTailBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	const maxJournalBytes = 8 << 20
	prefix, err := json.Marshal(Operation{ID: "before-tail", Name: "demo", Action: "stage", State: StateStaged})
	if err != nil {
		t.Fatal(err)
	}
	prefix = append(prefix, '\n')
	firstTail, err := json.Marshal(Operation{ID: "tail-first", Name: "demo", Action: "stage", State: StateStaged})
	if err != nil {
		t.Fatal(err)
	}
	firstTail = append(firstTail, '\n')
	secondTail, err := json.Marshal(Operation{ID: "tail-second", Name: "demo", Action: "stage", State: StateStaged})
	if err != nil {
		t.Fatal(err)
	}
	secondTail = append(secondTail, '\n')
	tail := append(append(firstTail, secondTail...), bytes.Repeat([]byte{'\n'}, maxJournalBytes-len(firstTail)-len(secondTail))...)
	if len(tail) != maxJournalBytes {
		t.Fatalf("tail length=%d, want %d", len(tail), maxJournalBytes)
	}
	data := append(prefix, tail...)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	operations, err := readOperations(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 || operations[0].ID != "tail-first" || operations[1].ID != "tail-second" {
		t.Fatalf("operations=%+v, want both records at exact tail boundary", operations)
	}
}

func TestRuntime_ManagerSeedIfMissingPersistsAndNeverOverwritesCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	source := filepath.Join(t.TempDir(), "bundled-v1")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	spec := Spec{Name: "bundled", Kind: "test", Version: "image-1", SourceType: "bundled", Source: source}
	manifest, seeded, err := m.SeedIfMissing(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !seeded || manifest.Version != "image-1" {
		t.Fatalf("seed result manifest=%+v seeded=%v", manifest, seeded)
	}
	currentPath := filepath.Join(root, "bundled", "current")
	if target, err := os.Readlink(currentPath); err != nil || target != filepath.Join("versions", "image-1") {
		t.Fatalf("current pointer=%q err=%v", target, err)
	}
	installed, err := os.ReadFile(filepath.Join(root, "bundled", "versions", "image-1", "runtime.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "v1" {
		t.Fatalf("installed runtime=%q", installed)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("v2"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest, seeded, err = m.SeedIfMissing(context.Background(), Spec{Name: "bundled", Kind: "test", Version: "image-2", SourceType: "bundled", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if seeded || manifest.Version != "image-1" {
		t.Fatalf("second seed result manifest=%+v seeded=%v", manifest, seeded)
	}
	installed, err = os.ReadFile(filepath.Join(root, "bundled", "versions", "image-1", "runtime.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "v1" {
		t.Fatalf("persistent runtime was overwritten: %q", installed)
	}
	reloaded, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, seeded, err = reloaded.SeedIfMissing(context.Background(), spec)
	if err != nil || seeded || manifest.Version != "image-1" {
		t.Fatalf("reloaded seed result manifest=%+v seeded=%v err=%v", manifest, seeded, err)
	}
}

func TestRuntime_ManagerSeedIfMissingNormalizesBundledLlamaBinaryDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	source := filepath.Join(t.TempDir(), "image-app")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "llama-server"), []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{
		"llamacpp": ProviderMux{Native: LlamaCPPProvider{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, seeded, err := m.SeedIfMissing(context.Background(), Spec{
		Name: "llamacpp", Kind: "llamacpp", Mode: RuntimeModeNative,
		Version: "bundled", SourceType: "bundled", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seeded || manifest.Version != "bundled" || manifest.Kind != "llamacpp" {
		t.Fatalf("seed result manifest=%+v seeded=%v", manifest, seeded)
	}
	for _, relative := range []string{"bin/llama-server", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(root, "llamacpp", "versions", "bundled", relative)); err != nil {
			t.Fatalf("bundled runtime missing %s: %v", relative, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "llamacpp", "versions", "bundled", "build")); err != nil {
		t.Fatalf("bundled runtime build directory missing: %v", err)
	}
	if status, ok := m.Get("llamacpp"); !ok || status.Current != "bundled" || status.State != StateActive {
		t.Fatalf("bundled runtime status=%+v found=%v", status, ok)
	}
}

func TestRuntime_ManagerSeedIfMissingRejectsUnsafeCurrentAndOverlappingSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "bundled")
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "missing"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "bundled")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SeedIfMissing(context.Background(), Spec{Name: "bundled", Kind: "test", Version: "1", SourceType: "bundled", Source: source}); err == nil {
		t.Fatal("dangling current pointer was silently replaced")
	}
	if target, readErr := os.Readlink(filepath.Join(dir, "current")); readErr != nil || target != filepath.Join("versions", "missing") {
		t.Fatalf("unsafe current pointer changed target=%q err=%v", target, readErr)
	}
	if _, _, err := m.SeedIfMissing(context.Background(), Spec{Name: "other", Kind: "test", Version: "1", SourceType: "bundled", Source: dir}); err == nil {
		t.Fatal("overlapping source path was accepted")
	}
}

func TestRuntime_ManagerRejectsSymlinkedRuntimeDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escaped")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "escaped", Kind: "test", Version: "1", Source: "fixture"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked runtime directory was accepted: %v", err)
	}
	if err := m.Register("escaped", "test"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Register accepted symlinked runtime directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "versions")); !os.IsNotExist(err) {
		t.Fatalf("symlinked runtime target was modified, stat error=%v", err)
	}
}

func TestRuntime_NewManagerRejectsSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real-root")
	if err := os.MkdirAll(realRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(parent, "runtime-root")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(linkRoot, map[string]Provider{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked runtime root was accepted: %v", err)
	}
}

func TestRuntime_NewManagerRejectsSymlinkedParentComponents(t *testing.T) {
	if stdruntime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	if err := os.MkdirAll(realParent, 0o750); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(filepath.Join(linkedParent, "runtimes"), map[string]Provider{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("runtime root with symlinked parent was accepted: %v", err)
	}
}

func TestRuntime_ManagerSeedIfMissingHonorsCancellation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "bundled")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.SeedIfMissing(ctx, Spec{Name: "bundled", Kind: "test", Version: "1", SourceType: "bundled", Source: source}); !errors.Is(err, context.Canceled) {
		t.Fatalf("seed cancellation error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.Root(), "bundled", "current")); !os.IsNotExist(err) {
		t.Fatalf("canceled seed created current pointer: %v", err)
	}
}

func TestRuntime_ManagerSeedStopsWhenJournalCannotBeWritten(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	source := filepath.Join(t.TempDir(), "bundled")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "bundled", "operations.jsonl")
	if err := os.MkdirAll(journalPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, seeded, err := m.SeedIfMissing(context.Background(), Spec{Name: "bundled", Kind: "test", Version: "image-1", SourceType: "bundled", Source: source}); err == nil || seeded || !strings.Contains(strings.ToLower(err.Error()), "seed transition") {
		t.Fatalf("SeedIfMissing result seeded=%v err=%v, want durable journal failure", seeded, err)
	}
	if _, ok := m.Get("bundled"); ok {
		t.Fatal("failed seed left an in-memory status")
	}
	if _, err := os.Lstat(filepath.Join(root, "bundled", "current")); !os.IsNotExist(err) {
		t.Fatalf("failed seed created current pointer: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "bundled", "versions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed seed invoked provider or retained candidates: %v", entries)
	}
}

func TestRuntime_ManagerSeedIfMissingFailureLeavesDegradedRetryableState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	source := filepath.Join(t.TempDir(), "bundled")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "runtime.bin"), []byte("v1"), 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{"test": testProvider{failHealth: true}})
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Name: "bundled", Kind: "test", Version: "image-1", SourceType: "bundled", Source: source}
	if _, seeded, err := m.SeedIfMissing(context.Background(), spec); err == nil || seeded {
		t.Fatalf("seed result seeded=%v err=%v, want degraded failure", seeded, err)
	}
	status, ok := m.Get("bundled")
	if !ok || status.State != StateDegraded || status.LastError == "" {
		t.Fatalf("status=%+v found=%v, want degraded error", status, ok)
	}
	if _, err := os.Lstat(filepath.Join(root, "bundled", "current")); !os.IsNotExist(err) {
		t.Fatalf("failed seed created current pointer: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "bundled", "versions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed seed left version entries: %v", entries)
	}
	detail, found, err := m.Detail("bundled")
	if err != nil || !found || len(detail.Operations) == 0 {
		t.Fatalf("detail=%+v found=%v err=%v", detail, found, err)
	}
	last := detail.Operations[len(detail.Operations)-1]
	if last.State != StateDegraded || last.Error == "" {
		t.Fatalf("last operation=%+v, want degraded error", last)
	}
}

func TestRuntime_ManagerDoesNotActivateWhileBusy(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "llama", Kind: "test", Version: "a", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return false })
	if err := m.Activate(context.Background(), "llama", "a"); err == nil {
		t.Fatal("expected busy activation error")
	}
	status, _ := m.Get("llama")
	if status.State != StateWaitingForIdle {
		t.Fatalf("state = %s, want %s", status.State, StateWaitingForIdle)
	}
}

func TestRuntime_ManagerUsesPerRuntimeIdleProbe(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "lmcache", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	m.SetRuntimeIdleProbe("lmcache", func() bool { return false })
	if err := m.Activate(context.Background(), "lmcache", "1"); err == nil || !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("LMCache activation error = %v, want ErrRuntimeBusy", err)
	}
	if status, _ := m.Get("lmcache"); status.State != StateWaitingForIdle {
		t.Fatalf("LMCache status = %+v, want WAITING_FOR_IDLE", status)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "vllm", "1"); err != nil {
		t.Fatalf("generic runtime activation unexpectedly used LMCache probe: %v", err)
	}
}

func TestRuntime_ManagerCheckForUpdatePersistsCandidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": updateTestProvider{candidate: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Source: "fixture"}, UpdatePolicy{Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	candidate, available, err := m.CheckForUpdate(context.Background(), "demo")
	if err != nil || !available || candidate.Version != "2" {
		t.Fatalf("candidate=%+v available=%v err=%v", candidate, available, err)
	}
	status, ok := m.Get("demo")
	if !ok || status.Available != "2" || status.LastCheck.IsZero() || status.State != StateUpdateAvailable {
		t.Fatalf("persisted check status=%+v found=%v", status, ok)
	}
	reloaded, err := NewManager(root, map[string]Provider{"test": updateTestProvider{candidate: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	status, ok = reloaded.Get("demo")
	if !ok || status.Available != "2" || status.LastCheck.IsZero() {
		t.Fatalf("reloaded check status=%+v found=%v", status, ok)
	}
}

func TestRuntime_ManagerActivateBusyPropagatesPersistenceError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "busy"), 0o750); err != nil {
		t.Fatal(err)
	}
	// A directory at state.json makes the durable status write fail while the
	// in-memory state still transitions to WAITING_FOR_IDLE.
	if err := os.Mkdir(filepath.Join(root, "busy", "state.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return false })
	err = m.Activate(context.Background(), "busy", "1")
	if err == nil || !strings.Contains(err.Error(), "runtime busy is busy") || !strings.Contains(strings.ToLower(err.Error()), "persist waiting-for-idle") {
		t.Fatalf("Activate error = %v, want busy and persistence errors", err)
	}
	status, ok := m.Get("busy")
	if !ok || status.State != StateWaitingForIdle {
		t.Fatalf("status = %+v, found=%v, want in-memory waiting state", status, ok)
	}
}

func TestRuntime_ManagerDoesNotStageWhileBusyByDefault(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return false })
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1", Source: "fixture"}); err == nil {
		t.Fatal("expected busy staging error")
	}
	status, _ := m.Get("vllm")
	if status.State != StateWaitingForIdle {
		t.Fatalf("state = %s, want %s", status.State, StateWaitingForIdle)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "vllm", "versions", ".staging-")); !os.IsNotExist(err) {
		t.Fatalf("busy staging created a staging directory, stat err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(m.Root(), "vllm", "versions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("busy staging created version entries: %+v", entries)
	}
}

func TestRuntime_ManagerRejectsPathLikeVersion(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"../escape", `a\\b`, ".", "..", " v1", "v1 "} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err == nil {
			t.Fatalf("version %q should be rejected", version)
		}
	}
	if err := (Manifest{Name: "demo", Version: "../escape", Kind: "test", Source: "fixture"}).Validate(); err == nil {
		t.Fatal("manifest path-like version should be rejected")
	}
}

func TestRuntime_ManifestValidateRejectsUnsafeOrMissingExactRefs(t *testing.T) {
	base := Manifest{
		Name:    "demo",
		Version: "1",
		Kind:    "vllm",
		Source:  "https://example.test/vllm.git",
		Metadata: map[string]string{
			"sourceType": "git",
		},
	}
	for _, ref := range []string{"", "-c", "refs/heads/../main", "refs/heads/main@{1}"} {
		manifest := base
		manifest.Ref = ref
		if err := manifest.Validate(); err == nil {
			t.Fatalf("manifest ref %q unexpectedly passed validation", ref)
		}
	}
	valid := base
	valid.Ref = "refs/tags/v0.8.5"
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid exact ref rejected: %v", err)
	}
	legacy := base
	legacy.Version = "git-" + strings.Repeat("5", 40)
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy immutable version rejected: %v", err)
	}
	legacy.Version = "legacy-git-runtime"
	legacy.Commit = strings.Repeat("6", 40)
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy immutable commit rejected: %v", err)
	}
}

func TestRuntime_ManagerCanStageWhileBusyWhenExplicitlyEnabled(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return false })
	m.SetBuildWhileBusy(true)
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntime_ManagerRollsBackOnHealthFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "llama", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "llama", version); err != nil {
			t.Fatal(err)
		}
	}
	status, _ := m.Get("llama")
	if status.Previous != "1" {
		t.Fatalf("previous = %q, want 1", status.Previous)
	}
}

func TestRuntime_ManagerHealthFailureRestoresServingCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	// Make only the candidate's health check fail. The old version remains the
	// serving current and should not be replaced by an older rollback target.
	m.providers["test"] = testProvider{failHealth: true}
	if err := m.Activate(context.Background(), "demo", "2"); err == nil {
		t.Fatal("expected candidate health failure")
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateActive || status.Current != "1" {
		t.Fatalf("status after failed activation = %+v, found=%v", status, ok)
	}
	if status.Previous != "" {
		t.Fatalf("previous pointer after failed activation = %q, want empty", status.Previous)
	}
	target, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || target != filepath.Join("versions", "1") {
		t.Fatalf("current pointer after failed activation = %q, err=%v", target, err)
	}
}

func TestRuntime_ManagerActivationCancellationRestoresServingCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.providers["test"] = cancelingProvider{cancel: cancel, phase: "verify"}
	if err := m.Activate(ctx, "demo", "2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("activation error = %v, want context.Canceled", err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateActive || status.Current != "1" {
		t.Fatalf("status after canceled activation = %+v, found=%v", status, ok)
	}
	target, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || target != filepath.Join("versions", "1") {
		t.Fatalf("current pointer after canceled activation = %q, err=%v", target, err)
	}
}

func TestRuntime_ManagerRollbackCancellationRestoresServingCurrent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.providers["test"] = cancelingProvider{cancel: cancel, phase: "verify"}
	if err := m.Rollback(ctx, "demo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("rollback error = %v, want context.Canceled", err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateActive || status.Current != "2" || status.Previous != "1" {
		t.Fatalf("status after canceled rollback = %+v, found=%v", status, ok)
	}
	target, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || target != filepath.Join("versions", "2") {
		t.Fatalf("current pointer after canceled rollback = %q, err=%v", target, err)
	}
}

func TestRuntime_ManagerStageCancellationDoesNotCommitVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	m.providers["test"] = cancelingProvider{cancel: cancel, phase: "stage"}
	if _, err := m.Stage(ctx, Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("stage error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "1")); !os.IsNotExist(err) {
		t.Fatalf("canceled stage committed version, stat err=%v", err)
	}
}

func TestRuntime_ManagerStageExistingVersionLeavesDegradedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	versionDir := filepath.Join(root, "demo", "versions", "1")
	if err := os.MkdirAll(versionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "runtime.bin"), []byte("existing"), 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("duplicate stage error=%v, want already-installed failure", err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateDegraded || status.LastError == "" {
		t.Fatalf("status=%+v found=%v, want degraded duplicate-stage state", status, ok)
	}
	detail, found, err := m.Detail("demo")
	if err != nil || !found || len(detail.Operations) == 0 {
		t.Fatalf("detail=%+v found=%v err=%v", detail, found, err)
	}
	last := detail.Operations[len(detail.Operations)-1]
	if last.State != StateDegraded || !strings.Contains(last.Error, "already installed") {
		t.Fatalf("last operation=%+v, want degraded duplicate-stage operation", last)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "1")); err != nil {
		t.Fatalf("pre-existing version was removed: %v", err)
	}
}

type countingTestProvider struct{ calls int }

func (p *countingTestProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	p.calls++
	return (testProvider{}).Stage(ctx, spec, destination)
}

func (p *countingTestProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	return (testProvider{}).Verify(ctx, manifest, directory)
}

func (p *countingTestProvider) Health(context.Context, Manifest, string) error { return nil }

// Staging a version that is already installed (directory plus ledger entry)
// must be an idempotent no-op: the provider is not re-invoked, the recorded
// manifest comes back unchanged, and the version directory is untouched.
// This is what makes a version switch back to a previously installed version
// a pointer activate instead of a doomed re-build.
func TestRuntime_ManagerStageInstalledVersionIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	provider := &countingTestProvider{}
	m, err := NewManager(root, map[string]Provider{"test": provider})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	spec := Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}

	first, err := m.Stage(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls after first stage=%d, want 1", provider.calls)
	}
	second, err := m.Stage(context.Background(), spec)
	if err != nil {
		t.Fatalf("staging an installed version must succeed idempotently: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider re-invoked for installed version: calls=%d", provider.calls)
	}
	if second.Name != first.Name || second.Version != first.Version || second.Kind != first.Kind || second.Fingerprint != first.Fingerprint {
		t.Fatalf("second stage manifest=%+v, want the recorded manifest %+v", second, first)
	}
	if content, err := os.ReadFile(filepath.Join(root, "demo", "versions", "1", "runtime.bin")); err != nil || string(content) != "1" {
		t.Fatalf("installed version content=%q err=%v, want the original", content, err)
	}
}

func TestRuntime_ManagerStageCleansCommittedCandidateWhenStateReadFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	dir := filepath.Join(root, "runtime", "versions")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "runtime", "state.json")
	outside := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(outside, []byte(`{"status":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, statePath); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Stage(context.Background(), Spec{Name: "runtime", Kind: "test", Version: "1.0", Source: "fixture"}); err == nil {
		t.Fatal("stage unexpectedly succeeded with a symlinked state file")
	}
	if _, err := os.Stat(filepath.Join(root, "runtime", "versions", "1.0")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed candidate remains after state failure: err=%v", err)
	}
	status, ok := m.Get("runtime")
	if !ok || status.State != StateDegraded {
		t.Fatalf("status=%+v found=%v, want degraded", status, ok)
	}
}

func TestRuntime_ManagerRollbackVerifiesCandidateBeforeSwitching(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	// The provider now rejects every health check, including the rollback
	// candidate. Rollback must leave version 2 serving and preserve version 1
	// as the candidate instead of switching first and discovering the failure
	// afterwards.
	m.providers["test"] = testProvider{failHealth: true}
	if err := m.Rollback(context.Background(), "demo"); err == nil {
		t.Fatal("expected rollback health failure")
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateActive || status.Current != "2" || status.Previous != "1" {
		t.Fatalf("status after rejected rollback = %+v, found=%v", status, ok)
	}
	target, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || target != filepath.Join("versions", "2") {
		t.Fatalf("current pointer after rejected rollback = %q, err=%v", target, err)
	}
}

func TestRuntime_ManagerRollbackSwapsPreviousCandidate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Rollback(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateActive || status.Current != "1" || status.Previous != "2" {
		t.Fatalf("status after rollback = %+v, found=%v", status, ok)
	}
	current, err := os.Readlink(filepath.Join(root, "demo", "current"))
	if err != nil || current != filepath.Join("versions", "1") {
		t.Fatalf("current pointer after rollback = %q, err=%v", current, err)
	}
	previous, err := os.Readlink(filepath.Join(root, "demo", "previous"))
	if err != nil || previous != filepath.Join("versions", "2") {
		t.Fatalf("previous pointer after rollback = %q, err=%v", previous, err)
	}
}

func TestRuntime_ManagerActivationFailureRemovesNewCurrentOnFirstActivation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(dir, "versions", "1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	// activationFailure re-reads the durable state under the lock; mirror the
	// first-activation crash window by persisting an ACTIVATING status.
	if err := m.writeState("demo", persistedState{Status: Status{Name: "demo", State: StateActivating, Current: "1"}, Versions: map[string]Manifest{
		"1": {Name: "demo", Version: "1", Kind: "test", Source: "fixture"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := m.activationFailure("demo", "", "", errors.New("state write failed")); err == nil {
		t.Fatal("expected activation failure")
	}
	if _, err := os.Lstat(filepath.Join(dir, "current")); !os.IsNotExist(err) {
		t.Fatalf("new current pointer survived first-activation rollback: %v", err)
	}
	status, ok := m.Get("demo")
	if !ok || status.State != StateDegraded {
		t.Fatalf("status after first-activation rollback = %+v, found=%v", status, ok)
	}
}

func TestRuntime_ManagerRecoversInterruptedStateAndRemovesDanglingPointers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	name := "vllm"
	dir := filepath.Join(root, name)
	versionDir := filepath.Join(dir, "versions", "1.0")
	if err := os.MkdirAll(versionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "runtime.bin"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1.0"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	state := persistedState{Status: Status{Name: name, Kind: "test", State: StateBuilding, Current: "1.0"}, Versions: map[string]Manifest{
		"1.0": {Name: name, Version: "1.0", Kind: "test", Source: "fixture"},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	status, ok := m.Get(name)
	if !ok || status.State != StateActive || status.Current != "1.0" {
		t.Fatalf("recovered status = %+v, found=%v", status, ok)
	}

	dangling := "bad"
	if err := os.Remove(filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", dangling), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root, map[string]Provider{"test": testProvider{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "current")); !os.IsNotExist(err) {
		t.Fatalf("dangling current pointer was retained: %v", err)
	}
}

func TestRuntime_ManagerRejectsExternalPointer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	name := "llama"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	state := persistedState{Status: Status{Name: name, State: StateActive, Current: "outside"}, Versions: map[string]Manifest{}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root, map[string]Provider{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "current")); !os.IsNotExist(err) {
		t.Fatalf("external current pointer was retained: %v", err)
	}
}

func TestRuntime_ManagerRejectsSymlinkedManagerLockAndRollsBackRegister(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "manager.lock")
	if err := os.WriteFile(outside, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".manager.lock")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.Register("demo", "test"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("Register error = %v, want symlink refusal", err)
	}
	if _, ok := m.Get("demo"); ok {
		t.Fatal("failed Register left a status in memory")
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("failed Register wrote state.json: %v", err)
	}
}

func TestRuntime_ManagerPinAndUnpinRollbackOnPersistenceFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	previous, ok := m.Get("demo")
	if !ok {
		t.Fatal("staged runtime status is missing")
	}
	outside := filepath.Join(t.TempDir(), "manager.lock")
	if err := os.WriteFile(outside, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".manager.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".manager.lock")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.Pin("demo", "1"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("Pin error = %v, want symlink refusal", err)
	}
	if got, _ := m.Get("demo"); got != previous {
		t.Fatalf("failed Pin changed in-memory status: before=%+v after=%+v", previous, got)
	}
	if err := m.Unpin("demo"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("Unpin error = %v, want symlink refusal", err)
	}
	if got, _ := m.Get("demo"); got != previous {
		t.Fatalf("failed Unpin changed in-memory status: before=%+v after=%+v", previous, got)
	}
}

func TestRuntime_ManagerCheckPersistenceFailureRestoresStatus(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	provider := testProvider{}
	m, err := NewManager(root, map[string]Provider{"test": provider})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	before, ok := m.Get("demo")
	if !ok || before.State != StateActive {
		t.Fatalf("active status before check = %+v, found=%v", before, ok)
	}
	statePath := filepath.Join(root, "demo", "state.json")
	m.providers["test"] = checkPersistenceFailureProvider{statePath: statePath}
	status, err := m.Check(context.Background(), "demo")
	if err == nil {
		t.Fatal("Check unexpectedly succeeded despite state persistence failure")
	}
	if got, ok := m.Get("demo"); !ok || got != before {
		t.Fatalf("failed Check changed in-memory status: before=%+v after=%+v found=%v", before, got, ok)
	}
	if status.State != StateActive {
		t.Fatalf("returned status = %+v, want the last durable active status", status)
	}
}

func TestRuntime_AutoUpdateReportsCheckPersistenceFailure(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test"}, UpdatePolicy{CheckEvery: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "manager.lock")
	if err := os.WriteFile(outside, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".manager.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".manager.lock")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("AutoUpdateOnce error = %v, want symlink persistence failure", err)
	}
	if status, ok := m.Get("demo"); !ok || status.State != StateIdle {
		t.Fatalf("failed automatic status write changed in-memory state: status=%+v found=%v", status, ok)
	}
}

func TestRuntime_ManagerRejectsVersionDirectorySymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	dir := filepath.Join(root, "llama")
	if err := os.MkdirAll(filepath.Join(dir, "versions"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "versions", "1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("versions", "1"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	state := persistedState{Status: Status{Name: "llama", State: StateActive, Current: "1"}, Versions: map[string]Manifest{
		"1": {Name: "llama", Version: "1", Kind: "test", Source: "fixture"},
	}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(root, map[string]Provider{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "current")); !os.IsNotExist(err) {
		t.Fatalf("pointer to symlinked version was retained: %v", err)
	}
}

func TestRuntime_ManagerRecoversValidPointerWhenStateFileWasNotRenamed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "demo", "state.json")); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	status, ok := reloaded.Get("demo")
	if !ok || status.State != StateActive || status.Current != "1" {
		t.Fatalf("recovered status = %+v, found=%v", status, ok)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "state.json")); err != nil {
		t.Fatalf("recovered state was not persisted: %v", err)
	}
}

func TestRuntime_ManagerAutomaticUpdateWaitsForIdleWindow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2", Source: "fixture"}, UpdatePolicy{MinIdle: 30 * time.Minute, CheckEvery: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Get("demo")
	if status.State != StateWaitingForIdle || status.Available != "2" {
		t.Fatalf("initial automatic status = %+v", status)
	}
	clock = clock.Add(29 * time.Minute)
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Current != "1" || status.State != StateWaitingForIdle {
		t.Fatalf("pre-idle-window status = %+v", status)
	}
	clock = clock.Add(2 * time.Minute)
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Current != "2" || status.State != StateActive || status.Available != "" {
		t.Fatalf("activated status = %+v", status)
	}
}

func TestRuntime_ManagerAutomaticUpdateHonorsPinnedVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Pin("demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2", Source: "fixture"}, UpdatePolicy{
		CheckEvery:          time.Nanosecond,
		CheckEverySet:       true,
		MinIdle:             0,
		MinIdleSet:          true,
		ActivateOnlyIdle:    true,
		ActivateOnlyIdleSet: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, ok := m.Get("demo")
	if !ok {
		t.Fatal("pinned runtime status disappeared")
	}
	if status.Current != "1" || status.Pinned != "1" {
		t.Fatalf("pinned runtime changed active version: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "2")); !os.IsNotExist(err) {
		t.Fatalf("pinned runtime staged an update, stat err=%v", err)
	}
}

func TestRuntime_ManagerPinningNonCurrentVersionFreezesAutomaticReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Pin("demo", "2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "3", Source: "fixture"}, UpdatePolicy{
		CheckEvery:          time.Nanosecond,
		CheckEverySet:       true,
		MinIdle:             0,
		MinIdleSet:          true,
		ActivateOnlyIdle:    true,
		ActivateOnlyIdleSet: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, ok := m.Get("demo")
	if !ok {
		t.Fatal("pinned runtime status disappeared")
	}
	if status.Current != "1" || status.Pinned != "2" {
		t.Fatalf("pinning a non-current version changed serving state: %+v", status)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "3")); !os.IsNotExist(err) {
		t.Fatalf("automatic update staged a replacement despite non-current pin, stat err=%v", err)
	}
}

func TestRuntime_ManagerActivationHooksTrackPointerSwitch(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	var before, after []string
	m.SetActivationHooks(
		func(_ context.Context, name, from, to string) error {
			before = append(before, name+":"+from+">"+to)
			return nil
		},
		func(_ context.Context, name, from, to string) error {
			after = append(after, name+":"+from+">"+to)
			return nil
		},
	)
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := strings.Join(before, ","), "demo:>1,demo:1>2"; got != want {
		t.Fatalf("before hooks=%q want %q", got, want)
	}
	if got, want := strings.Join(after, ","), "demo:>1,demo:1>2"; got != want {
		t.Fatalf("after hooks=%q want %q", got, want)
	}
}

func TestRuntime_ManagerActivationHookFailureRestoresProcessBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	var after []string
	m.SetActivationHooks(nil, func(_ context.Context, name, from, to string) error {
		after = append(after, name+":"+from+">"+to)
		if to == "2" {
			return errors.New("replacement process failed")
		}
		return nil
	})
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if version == "1" {
			if err := m.Activate(context.Background(), "demo", version); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := m.Activate(context.Background(), "demo", version); err == nil || !strings.Contains(err.Error(), "replacement process failed") {
			t.Fatalf("failed activation error=%v", err)
		}
	}
	status, ok := m.Get("demo")
	if !ok || status.Current != "1" {
		t.Fatalf("restored status=%+v found=%v", status, ok)
	}
	if got, want := strings.Join(after, ","), "demo:>1,demo:1>2,demo:2>1"; got != want {
		t.Fatalf("after compensation hooks=%q want %q", got, want)
	}
}

func TestRuntime_ManagerConfigureRejectsInvalidDefinitionBeforePersistence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		spec    Spec
		policy  UpdatePolicy
		wantErr string
	}{
		{name: "bad version", spec: Spec{Name: "demo", Kind: "test", Version: "../2"}, wantErr: "version"},
		{name: "kind padding", spec: Spec{Name: "demo", Kind: "test ", Version: "2"}, wantErr: "kind"},
		{name: "kind invisible", spec: Spec{Name: "demo", Kind: "te\u200bst", Version: "2"}, wantErr: "kind"},
		{name: "bad policy", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Policy: "sometimes"}, wantErr: "policy"},
		{name: "policy padding", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Policy: " automatic"}, wantErr: "policy"},
		{name: "policy invisible", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Policy: "auto\u200bmatic"}, wantErr: "policy"},
		{name: "bad channel", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Channel: "nightly"}, wantErr: "channel"},
		{name: "channel padding", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Channel: "stable "}, wantErr: "channel"},
		{name: "channel invisible", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{Channel: "pre\u200brelease"}, wantErr: "channel"},
		{name: "negative idle", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{MinIdle: -time.Second}, wantErr: "idle"},
		{name: "negative keep", spec: Spec{Name: "demo", Kind: "test", Version: "2"}, policy: UpdatePolicy{KeepVersions: -1}, wantErr: "keepVersions"},
		{name: "git ref option", spec: Spec{Name: "demo", Kind: "vllm", Version: "2", SourceType: "git", Ref: "-c"}, wantErr: "must not begin"},
		{name: "git ref missing", spec: Spec{Name: "demo", Kind: "llamacpp", Version: "2", SourceType: "commit"}, wantErr: "requires an exact ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Configure("demo", tc.spec, tc.policy); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErr)) {
				t.Fatalf("Configure error = %v, want substring %q", err, tc.wantErr)
			}
			if _, ok := m.Definition("demo"); ok {
				t.Fatal("invalid definition was persisted in memory")
			}
			if _, err := os.Stat(filepath.Join(root, "demo", "state.json")); !os.IsNotExist(err) {
				t.Fatalf("invalid definition wrote state.json: %v", err)
			}
		})
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2"}, UpdatePolicy{}); err != nil {
		t.Fatalf("valid Configure failed: %v", err)
	}
	if definition, ok := m.Definition("demo"); !ok || definition.Spec.Version != "2" {
		t.Fatalf("valid definition = %+v, found=%v", definition, ok)
	}
}

func TestRuntime_ManagerSetProviderReplacesProviderForFutureOperations(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{
		"test": testProvider{failHealth: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetProvider("test", testProvider{}); err != nil {
		t.Fatalf("SetProvider failed: %v", err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatalf("Activate used the replaced unhealthy provider: %v", err)
	}
	if err := m.SetProvider("bad kind", testProvider{}); err == nil {
		t.Fatal("SetProvider accepted an unsafe provider kind")
	}
	if err := m.SetProvider("test", (*typedNilProvider)(nil)); err == nil {
		t.Fatal("SetProvider accepted a typed-nil provider")
	}
}

func TestRuntime_ManagerUnconfigurePreservesInstalledRuntimeState(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}, UpdatePolicy{Policy: "automatic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unconfigure("demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("demo"); ok {
		t.Fatal("Unconfigure left the runtime in the automatic update set")
	}
	detail, ok, err := m.Detail("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || detail.Status.Current != "1" {
		t.Fatalf("runtime status after Unconfigure = %+v, found=%v", detail.Status, ok)
	}
	if _, ok := detail.Versions["1"]; !ok {
		t.Fatalf("installed versions after Unconfigure = %#v", detail.Versions)
	}
}

func TestRuntime_ManagerConfigureDoesNotMutateMapsWhenStateWriteFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(outside, []byte(`{"status":{}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "state.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "1"}, UpdatePolicy{}); err == nil {
		t.Fatal("Configure unexpectedly succeeded with a symlinked state file")
	}
	if _, ok := m.Definition("demo"); ok {
		t.Fatal("failed Configure left a definition in memory")
	}
	if _, ok := m.Get("demo"); ok {
		t.Fatal("failed Configure left a status in memory")
	}
}

func TestRuntime_ManagerRejectsProviderManifestIdentityMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": identityMismatchProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err == nil || !strings.Contains(err.Error(), "manifest kind") {
		t.Fatalf("manifest identity mismatch error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "demo", "versions", "1")); !os.IsNotExist(err) {
		t.Fatalf("mismatched manifest left committed version: %v", err)
	}
}

func TestRuntime_ManagerAutomaticUpdatePublishesWaitingAndActiveProgress(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2", Source: "fixture"}, UpdatePolicy{MinIdle: time.Hour, CheckEvery: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	idle := true
	m.SetIdleProbe(func() bool { return idle })
	events := make(chan swaputil.BackendProgressEvent, 64)
	m.SetProgress(func(event swaputil.BackendProgressEvent) {
		select {
		case events <- event:
		default:
		}
	})
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	seenWaiting := false
	for {
		select {
		case event := <-events:
			seenWaiting = seenWaiting || event.Phase == "waiting_for_idle"
		default:
			goto waitingDone
		}
	}
waitingDone:
	if !seenWaiting {
		t.Fatal("automatic update did not publish waiting_for_idle")
	}
	// Reusing the default clock would require sleeping for the one-hour policy;
	// use a short policy on a fresh manager pass after the waiting event has
	// established the idle timestamp.
	definition, ok := m.Definition("demo")
	if !ok {
		t.Fatal("runtime definition disappeared")
	}
	definition.Policy.MinIdle = 0
	definition.Policy.MinIdleSet = true
	if err := m.Configure("demo", definition.Spec, definition.Policy); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	seenActive := false
	for {
		select {
		case event := <-events:
			seenActive = seenActive || event.Phase == "active" && event.Progress == 1
		default:
			goto activeDone
		}
	}
activeDone:
	if !seenActive {
		t.Fatal("automatic update did not publish active progress")
	}
}

func TestRuntime_ManagerAutomaticUpdateHonorsBusyGateAndPrunesVersions(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "3", Source: "fixture"}, UpdatePolicy{MinIdle: time.Millisecond, KeepVersions: 2, KeepVersionsSet: true, CheckEvery: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	busy := false
	m.SetIdleProbe(func() bool { return !busy })
	for _, version := range []string{"1", "2"} {
		if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: version, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Activate(context.Background(), "demo", version); err != nil {
			t.Fatal(err)
		}
	}
	busy = true
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Get("demo")
	if status.Current != "2" || status.State != StateWaitingForIdle {
		t.Fatalf("busy status = %+v", status)
	}
	busy = false
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Current != "2" || status.State != StateWaitingForIdle {
		t.Fatalf("before idle window status = %+v", status)
	}
	time.Sleep(2 * time.Millisecond)
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Current != "3" || status.State != StateActive {
		t.Fatalf("post-idle status = %+v", status)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "demo", "versions", "1")); !os.IsNotExist(err) {
		t.Fatalf("old version was not pruned, stat err=%v", err)
	}
}

func TestRuntime_ManagerAutomaticUpdateCanStageWhileBusyButDefersActivation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	busy := true
	m.SetIdleProbe(func() bool { return !busy })
	m.SetBuildWhileBusy(true)
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2", Source: "fixture"}, UpdatePolicy{CheckEvery: time.Nanosecond, MinIdle: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Get("demo")
	if status.Current != "1" || status.Staged != "2" || status.Available != "2" || status.State != StateWaitingForIdle {
		t.Fatalf("busy staged status = %+v", status)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Staged != "2" {
		t.Fatalf("repeated busy pass lost staged candidate: %+v", status)
	}

	busy = false
	definition, ok := m.Definition("demo")
	if !ok {
		t.Fatal("runtime definition disappeared")
	}
	definition.Policy.MinIdle = 0
	definition.Policy.MinIdleSet = true
	if err := m.Configure("demo", definition.Spec, definition.Policy); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ = m.Get("demo")
	if status.Current != "2" || status.Staged != "" || status.State != StateActive {
		t.Fatalf("idle activation status = %+v", status)
	}
}

func TestRuntime_ManagerAutomaticUpdateActivationFailurePreservesCandidateManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": versionHealthProvider{failVersion: "2"}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if err := m.Configure("demo", Spec{Name: "demo", Kind: "test", Version: "2", Source: "fixture"}, UpdatePolicy{
		CheckEvery:          time.Nanosecond,
		MinIdle:             0,
		MinIdleSet:          true,
		ActivateOnlyIdle:    true,
		ActivateOnlyIdleSet: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), "demo", "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdateOnce(context.Background()); err == nil {
		t.Fatal("automatic update unexpectedly activated unhealthy candidate")
	}
	detail, ok, err := m.Detail("demo")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("runtime detail disappeared after failed activation")
	}
	if detail.Status.Current != "1" || detail.Status.Staged != "2" || detail.Status.State != StateActive {
		t.Fatalf("status after failed automatic activation = %+v", detail.Status)
	}
	if _, ok := detail.Versions["2"]; !ok {
		t.Fatalf("staged candidate manifest was lost after activation failure: %#v", detail.Versions)
	}
}

func TestRuntime_ManagerVersionMutationsRejectPathLikeReferences(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"../1", "versions/1", `a\\b`, "."} {
		if err := m.Pin("demo", version); err == nil {
			t.Fatalf("Pin(%q) unexpectedly succeeded", version)
		}
		if err := m.RemoveVersion("demo", version); err == nil {
			t.Fatalf("RemoveVersion(%q) unexpectedly succeeded", version)
		}
		if err := m.Activate(context.Background(), "demo", version); err == nil {
			t.Fatalf("Activate(%q) unexpectedly succeeded", version)
		}
	}
	if err := m.Pin("demo", "1"); err != nil {
		t.Fatalf("Pin valid version: %v", err)
	}
	if status, ok := m.Get("demo"); !ok || status.Pinned != "1" {
		t.Fatalf("pinned status = %+v, found=%v", status, ok)
	}
}

func TestRuntime_ManagerControlUpdateQueueAppliesOnFlush(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	seed := ControlUpdate{Definitions: map[string]Definition{
		"gone": {Spec: Spec{Name: "gone", Kind: "test", Version: "1", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
	}}
	if err := m.QueueControlUpdate(seed); err != nil {
		t.Fatal(err)
	}
	if err := m.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("gone"); !ok {
		t.Fatal("seed definition was not applied")
	}

	buildWhileBusy := true
	update := ControlUpdate{
		Definitions: map[string]Definition{
			"b": {Spec: Spec{Name: "b", Kind: "test", Version: "2", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
			"a": {Spec: Spec{Name: "a", Kind: "test", Version: "1", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
		},
		Registered:     []string{"plain"},
		Removed:        []string{"gone"},
		Providers:      map[string]Provider{"test": testProvider{}},
		BuildWhileBusy: &buildWhileBusy,
	}
	if err := m.QueueControlUpdate(update); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("a"); ok {
		t.Fatal("queued update was applied before flush")
	}
	if err := m.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if def, ok := m.Definition("a"); !ok || def.Spec.Version != "1" || def.Policy.Policy != "manual" {
		t.Fatalf("definition a = %+v, found=%v", def, ok)
	}
	if def, ok := m.Definition("b"); !ok || def.Spec.Version != "2" {
		t.Fatalf("definition b = %+v, found=%v", def, ok)
	}
	if _, ok := m.Definition("gone"); ok {
		t.Fatal("removed runtime remained configured")
	}
	if _, ok := m.Get("gone"); !ok {
		t.Fatal("removed runtime lost its durable status")
	}
	if _, ok := m.Get("plain"); !ok {
		t.Fatal("registered-only runtime has no durable state")
	}
	if _, ok := m.Get("a"); !ok {
		t.Fatal("configured runtime has no durable state")
	}
	if !m.buildWhileBusyAllowed() {
		t.Fatal("buildWhileBusy flag was not applied")
	}

	// A newer update supersedes the queued one; the older is discarded.
	if err := m.QueueControlUpdate(ControlUpdate{Removed: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.QueueControlUpdate(ControlUpdate{Removed: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("a"); ok {
		t.Fatal("superseded update removed only part of the set")
	}
	if _, ok := m.Definition("b"); ok {
		t.Fatal("newest update was not applied")
	}
	if _, ok := m.Get("a"); !ok {
		t.Fatal("durable status must survive unconfigure")
	}
}

func TestRuntime_ManagerControlUpdateRejectsInvalidDefinitionBeforeQueueing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	// A definition missing its kind must reject the whole update before
	// anything is recorded, so the reload candidate fails closed.
	if err := m.QueueControlUpdate(ControlUpdate{Definitions: map[string]Definition{
		"bad":  {Spec: Spec{Name: "bad", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
		"good": {Spec: Spec{Name: "good", Kind: "test", Version: "1", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
	}}); err == nil {
		t.Fatal("invalid definition was accepted")
	}
	if err := m.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("bad"); ok {
		t.Fatal("invalid definition was queued")
	}
	if _, ok := m.Definition("good"); ok {
		t.Fatal("valid sibling definition was queued with the rejected update")
	}
	if err := m.QueueControlUpdate(ControlUpdate{Providers: map[string]Provider{"test": nil}}); err == nil {
		t.Fatal("nil provider was accepted")
	}
}

// Regression for the runtime-manager/config-apply deadlock: an in-flight
// activation holds m.mu while its switch-after hook waits for the
// configuration apply to finish. The configuration apply (QueueControlUpdate)
// must not wait on m.mu, or the pair deadlocks permanently.
func TestRuntime_ManagerConfigApplyDoesNotDeadlockWithActivation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtimes")
	m, err := NewManager(root, map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stage(context.Background(), Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	hookEntered := make(chan struct{})
	releaseHook := make(chan struct{})
	m.SetActivationHooks(nil, func(context.Context, string, string, string) error {
		close(hookEntered)
		<-releaseHook
		return nil
	})
	activateDone := make(chan error, 1)
	go func() {
		activateDone <- m.Activate(context.Background(), "demo", "1")
	}()
	<-hookEntered

	queued := make(chan error, 1)
	go func() {
		queued <- m.QueueControlUpdate(ControlUpdate{Definitions: map[string]Definition{
			"demo": {Spec: Spec{Name: "demo", Kind: "test", Version: "1", Source: "fixture"}, Policy: UpdatePolicy{Policy: "manual"}},
		}})
	}()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued config apply failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("config apply waited on the manager lock held by the in-flight activation")
	}

	close(releaseHook)
	if err := <-activateDone; err != nil {
		t.Fatalf("Activate = %v", err)
	}
	if err := m.FlushControlUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Definition("demo"); !ok {
		t.Fatal("queued definition was not applied after the activation released the lock")
	}
	if status, ok := m.Get("demo"); !ok || status.State != StateActive || status.Current != "1" {
		t.Fatalf("status = %+v, found=%v, want active on 1", status, ok)
	}
}

// TestRuntime_ManagerAdoptsOrphanVersionAfterCrash simulates the Stage crash
// window: the version directory and manifest are durable but the ledger entry
// never committed to state.json. load() must fold the orphan back into the
// ledger, or every future Stage of the same version redoes the whole build
// and then fails the commit guard with "already installed".
func TestRuntime_ManagerAdoptsOrphanVersionAfterCrash(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1.0.0", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}

	// Roll the ledger back to the crash window: directory durable, entry lost.
	state, err := m.statusState("vllm")
	if err != nil {
		t.Fatal(err)
	}
	state.Versions = map[string]Manifest{}
	if err := m.writeState("vllm", state); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	detail, found, err := reloaded.Detail("vllm")
	if err != nil || !found {
		t.Fatalf("detail after reload found=%v err=%v", found, err)
	}
	if _, ok := detail.Versions["1.0.0"]; !ok {
		t.Fatalf("orphan version was not adopted: %+v", detail.Versions)
	}
	// A re-stage of the adopted version must be idempotent, not a rebuild that
	// fails the commit guard.
	manifest, err := reloaded.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1.0.0", Source: "fixture"})
	if err != nil {
		t.Fatalf("re-stage of adopted orphan: %v", err)
	}
	if manifest.Version != "1.0.0" {
		t.Fatalf("re-stage manifest version = %q", manifest.Version)
	}
}

// TestRuntime_SaveAutoStatusPreservesConcurrentPin verifies the stale-snapshot
// protection: a check that started before an operator Pin must not clobber
// the pin when it persists its (older) status.
func TestRuntime_SaveAutoStatusPreservesConcurrentPin(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"test": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })
	if _, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "test", Version: "1.0.0", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	// base is the snapshot a check took BEFORE the operator pinned.
	base, ok := m.Get("vllm")
	if !ok {
		t.Fatal("staged runtime status missing")
	}

	// The operator pins while a check is in flight; the check then persists a
	// snapshot taken before the pin.
	if err := m.Pin("vllm", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	stale := base
	stale.Pinned = ""
	stale.UpdatedAt = m.now()
	if err := m.saveAutoStatus("vllm", stale); err != nil {
		t.Fatal(err)
	}

	got, ok := m.Get("vllm")
	if !ok || got.Pinned != "1.0.0" {
		t.Fatalf("pinned field after stale save = %+v (found=%v), want pin preserved", got, ok)
	}
}

// gateStageProvider blocks Stage until released, simulating a long download
// or compile.
type gateStageProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *gateStageProvider) Stage(_ context.Context, spec Spec, destination string) (Manifest, error) {
	select {
	case <-p.started:
	default:
		if p.started != nil {
			close(p.started)
		}
	}
	if p.release != nil {
		<-p.release
	}
	if err := os.WriteFile(filepath.Join(destination, "runtime.bin"), []byte(spec.Version), 0o750); err != nil {
		return Manifest{}, err
	}
	return Manifest{Name: spec.Name, Version: spec.Version, Kind: spec.Kind, Source: spec.Source}, nil
}

func (*gateStageProvider) Verify(context.Context, Manifest, string) error { return nil }
func (*gateStageProvider) Health(context.Context, Manifest, string) error { return nil }

// TestRuntime_ManagerStageDoesNotBlockOtherRuntimes verifies the per-runtime
// operation lock design: a slow build for one runtime must not block staging
// of another runtime, control-plane flushes, or status reads. Before the
// split, the manager-wide lock held across provider work serialized all of
// these behind the build.
func TestRuntime_ManagerStageDoesNotBlockOtherRuntimes(t *testing.T) {
	root := t.TempDir()
	slow := &gateStageProvider{started: make(chan struct{}), release: make(chan struct{})}
	m, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"slow": slow, "fast": testProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })

	slowDone := make(chan error, 1)
	go func() {
		_, err := m.Stage(context.Background(), Spec{Name: "slow", Kind: "slow", Version: "1", Source: "fixture"})
		slowDone <- err
	}()
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow stage never reached the provider")
	}

	fastDone := make(chan error, 1)
	go func() {
		_, err := m.Stage(context.Background(), Spec{Name: "fast", Kind: "fast", Version: "1", Source: "fixture"})
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast stage failed while slow runtime was building: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast stage blocked behind the slow runtime's build")
	}

	flushed := make(chan error, 1)
	go func() { flushed <- m.FlushControlUpdate() }()
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("FlushControlUpdate failed during a build: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FlushControlUpdate blocked during a provider build")
	}

	if status, ok := m.Get("slow"); !ok || status.State != StateBuilding {
		t.Fatalf("slow status = %+v (found=%v), want BUILDING visible during the build", status, ok)
	}

	close(slow.release)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow stage: %v", err)
	}
	if status, ok := m.Get("slow"); !ok || status.State != StateStaged {
		t.Fatalf("slow status after stage = %+v (found=%v), want STAGED", status, ok)
	}
}

// Two concurrent Stages of the SAME runtime must serialize on the op lock
// instead of racing the staging directory or ledger.
func TestRuntime_ManagerConcurrentStageSameRuntimeSerializes(t *testing.T) {
	root := t.TempDir()
	slow := &gateStageProvider{started: make(chan struct{}), release: make(chan struct{})}
	m, err := NewManager(filepath.Join(root, "runtimes"), map[string]Provider{"vllm": slow})
	if err != nil {
		t.Fatal(err)
	}
	m.SetIdleProbe(func() bool { return true })

	first := make(chan error, 1)
	go func() {
		_, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "1", Source: "fixture"})
		first <- err
	}()
	<-slow.started

	second := make(chan error, 1)
	go func() {
		_, err := m.Stage(context.Background(), Spec{Name: "vllm", Kind: "vllm", Version: "2", Source: "fixture"})
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("second stage did not serialize behind the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(slow.release)
	if err := <-first; err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtimes", "vllm", "versions", "2", "runtime.bin")); err != nil {
		t.Fatalf("second staged version missing: %v", err)
	}
}

func TestRuntime_ManagerRuntimeOperationLog(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, output := m.RuntimeOperationLog("vllm-dev"); output != "" {
		t.Fatalf("empty manager returned output %q", output)
	}

	m.emitRuntimeLog("vllm-dev", "op-1", "stdout", []byte("step one\n"))
	m.emitRuntimeLog("vllm-dev", "op-1", "stderr", []byte("warning\n"))
	operationID, output := m.RuntimeOperationLog("vllm-dev")
	if operationID != "op-1" {
		t.Fatalf("operationID = %q, want op-1", operationID)
	}
	if output != "step one\nwarning\n" {
		t.Fatalf("output = %q, want interleaved streams", output)
	}

	// A new operation replaces the capture so the panel reflects the latest
	// (or currently running) operation instead of accreting history forever.
	m.emitRuntimeLog("vllm-dev", "op-2", "stdout", []byte("step two\n"))
	operationID, output = m.RuntimeOperationLog("vllm-dev")
	if operationID != "op-2" || output != "step two\n" {
		t.Fatalf("after new operation: id=%q output=%q", operationID, output)
	}

	if _, output := m.RuntimeOperationLog("unknown"); output != "" {
		t.Fatalf("unknown runtime returned output %q", output)
	}
}

func TestRuntime_ManagerRuntimeOperationLogTrimsToTail(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "runtimes"), map[string]Provider{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	chunk := strings.Repeat("x", 64<<10)
	for i := 0; i < 5; i++ {
		m.emitRuntimeLog("vllm-dev", "op-1", "stdout", []byte(chunk))
	}
	m.emitRuntimeLog("vllm-dev", "op-1", "stdout", []byte("FINAL-TAIL"))
	_, output := m.RuntimeOperationLog("vllm-dev")
	if len(output) != runtimeOperationLogBufferLimit {
		t.Fatalf("captured %d bytes, want the %d byte cap", len(output), runtimeOperationLogBufferLimit)
	}
	// The kept bytes are the newest ones.
	if !strings.HasSuffix(output, "FINAL-TAIL") {
		t.Fatal("capture does not hold the tail of the output")
	}
}
