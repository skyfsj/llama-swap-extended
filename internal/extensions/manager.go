package extensions

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/mostlygeek/llama-swap/internal/netproxy"
	"github.com/shirou/gopsutil/v4/process"
	"gopkg.in/yaml.v3"
)

type Compiled struct {
	NativeHook     func(context.Context, string, Context, json.RawMessage) (json.RawMessage, []string, error)
	NativeApproval bool
	Manifest       Manifest
	Bundle         string
	ETag           string
	Tools          []Tool
	// ToolHandlers lists the tools whose calls the worker dispatches straight
	// to their SDK-bound handler instead of the onToolCall hook.
	ToolHandlers []string
	// Directories lists the extension's empty on-disk directories.
	Directories []string
	Hooks       []string
	Settings    []SettingField
	// LogSink receives structured log records the script emits live. It is set
	// by the server (per-extension monitor) and nil in tests.
	LogSink func(logLine)
	// network is the daemon's outbound proxy policy, passed to the worker
	// subprocess so its ctx.http.fetch calls honor it.
	network netproxy.Settings
	// runtime selects the executor for Invoke: zero value = goja worker
	// (production); RuntimeNode is set only for debug sessions.
	runtime   runtimeKind
	poolSlots chan struct{}
	statusMu  sync.RWMutex
	lastError string
	// room caches HookInputRoom's measurement of the request envelope; the
	// bundle and manifest do not change for a compiled extension.
	roomOnce sync.Once
	room     int
}

// runtimeKind selects the hook executor.
type runtimeKind int

const (
	RuntimeGoja runtimeKind = iota
	RuntimeNode
)

// WithRuntime returns a shallow copy using the given executor. Debug sessions
// flip to RuntimeNode; production compiled extensions keep the zero value.
// The delegate shares the original Compiled's pool, locks and logs — only
// Invoke dispatches elsewhere.
func (c *Compiled) WithRuntime(kind runtimeKind) *Compiled {
	if c == nil || c.runtime == kind {
		return c
	}
	return &Compiled{
		Manifest:       c.Manifest,
		NativeHook:     c.NativeHook,
		NativeApproval: c.NativeApproval,
		Bundle:         c.Bundle,
		ETag:           c.ETag,
		Tools:          c.Tools,
		ToolHandlers:   c.ToolHandlers,
		Directories:    c.Directories,
		Hooks:          c.Hooks,
		Settings:       c.Settings,
		LogSink:        c.LogSink,
		network:        c.network,
		runtime:        kind,
		poolSlots:      c.poolSlots,
		lastError:      c.lastError,
	}
}

// Invoke dispatches one hook to the configured executor.
func (c *Compiled) Invoke(ctx context.Context, hook string, meta Context, input json.RawMessage) (json.RawMessage, []string, error) {
	if c.NativeHook != nil {
		return c.NativeHook(ctx, hook, meta, input)
	}
	room := c.HookInputRoom()
	if err := CheckHookInputFor(c.Manifest.ID, hook, room, meta, input); err != nil {
		c.setLastError(err)
		return nil, nil, err
	}
	if c.runtime == RuntimeNode {
		return c.InvokeWithNode(ctx, hook, meta, input, DebugNodePath(), DebugExecutorScript())
	}
	response, err := c.run(ctx, workerRequest{Bundle: c.Bundle, Manifest: c.Manifest, Settings: c.Settings, Hook: hook, Context: meta, Input: input}, c.hookBudget())
	c.setLastError(err)
	return response.Output, response.Logs, err
}

// HookInputRoom reports how many bytes of hook input this extension can carry,
// given the bundle that has to travel on the same protocol line. The caller that
// builds an input uses it to decide whether the payload fits; the limit is a
// property of the extension's size as much as of the protocol.
func (c *Compiled) HookInputRoom() int {
	// Measure the real envelope, not an estimate of it. The bundle is re-sent on
	// every call, its JSON escaping can grow it (every "<", ">" and "&" becomes a
	// six-byte sequence), and the manifest's config travels too. Adding up field
	// lengths under-counted all of that, so an input the guard accepted could
	// still overflow the line and kill the worker - which on a request hook is
	// the 502 the skip path exists to prevent.
	//
	// The empty-input request is marshalled once and cached: the bundle and the
	// config do not change for a compiled extension.
	c.roomOnce.Do(func() {
		probe := workerRequest{
			Bundle:   c.Bundle,
			Manifest: c.Manifest,
			Settings: c.Settings,
			Hook:     "onToolCall",
			Input:    json.RawMessage(`{}`),
		}
		encoded, err := json.Marshal(probe)
		if err != nil {
			c.room = 0
			return
		}
		// The input is one field of that envelope; what is left over is the room
		// it has, less the braces and quoting the field itself needs.
		room := MaxHookInputBytes - (len(encoded) - len(`"input":{}`))
		if room < 0 {
			room = 0
		}
		c.room = room
	})
	return c.room
}

// checkHookInput refuses a call whose input cannot cross the worker protocol.
// The whole request is one line with an 8 MiB ceiling, so a payload that does
// not fit has to be named as such: without this the worker dies mid-line and the
// failure reads as "ended without a response", which tells a caller nothing.
//
// The room depends on the bundle, which shares the line, so the check measures
// the extension's own budget rather than the raw limit. A preset whose bundle is
// large therefore reaches the limit sooner - and gets told the real number
// instead of a generic protocol error.
// CheckHookInputFor reports whether one hook call fits, given the context that
// travels with it. The context is not cached: it carries the caller's model
// snapshot, which is per request.
func CheckHookInputFor(extension, hook string, room int, meta Context, input json.RawMessage) error {
	if encoded, err := json.Marshal(meta); err == nil {
		room -= len(encoded)
	}
	return checkHookInput(extension, hook, room, input)
}

func checkHookInput(extension, hook string, room int, input json.RawMessage) error {
	// room <= 0 means the extension's own envelope already fills the line, so
	// nothing fits. Treating it as "no limit" (the first version did) let a
	// bundle in the reserve window accept every input and then die on the wire.
	if room <= 0 {
		return fmt.Errorf("%w: %s %s has no room left on the %s protocol line after its own bundle; the extension is too large to run a hook that takes an input",
			ErrHookInputTooLarge, extension, hook, humanMebi(maxWorkerLine))
	}
	if len(input) <= room {
		return nil
	}
	if room == MaxHookInputBytes {
		// The context-free message, for callers that do not know the bundle.
		return fmt.Errorf("%w: %s %s carries %s, over the %s one hook may receive",
			ErrHookInputTooLarge, extension, hook, humanMebi(len(input)), humanMebi(MaxHookInputBytes))
	}
	return fmt.Errorf("%w: %s %s carries %s, over the %s available beside this extension's own code (the protocol line is %s, shared with the bundle); a base64 image is the usual cause, so downscale or crop it first",
		ErrHookInputTooLarge, extension, hook, humanMebi(len(input)), humanMebi(room), humanMebi(MaxHookInputBytes))
}

// ErrHookInputTooLarge marks a hook that could not run because its payload does
// not fit on the worker's protocol line. It is a capacity limit rather than a
// fault in the extension, so a caller decides whether to skip the hook or to
// report the failure - a request hook on a conversation that has grown past the
// limit must not fail the user's request.
var ErrHookInputTooLarge = errors.New("hook payload too large")

// humanMebi renders a byte count for a size-limit message.
func humanMebi(bytes int) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

func (c *Compiled) setLastError(err error) {
	if c == nil || err == nil {
		return
	}
	c.statusMu.Lock()
	c.lastError = err.Error()
	c.statusMu.Unlock()
}

func (c *Compiled) LastError() string {
	if c == nil {
		return ""
	}
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.lastError
}

type Manager struct {
	opMu        sync.RWMutex
	mu          sync.RWMutex
	directory   string
	items       map[string]*Compiled
	errors      map[string]string
	network     netproxy.Settings
	watchCancel context.CancelFunc
}

// Network reports the proxy policy extension fetches use.
func (m *Manager) Network() netproxy.Settings {
	if m == nil {
		return netproxy.Settings{}
	}
	return m.network
}

func NewManager(directory string) (*Manager, error) {
	return NewManagerWithNetwork(directory, netproxy.Settings{})
}

// NewManagerWithNetwork opens the manager with the daemon's outbound proxy
// policy: extension fetches go through it (they used to ignore the proxy
// entirely, including the environment).
func NewManagerWithNetwork(directory string, network netproxy.Settings) (*Manager, error) {
	m := &Manager{directory: directory, items: map[string]*Compiled{}, errors: map[string]string{}, network: network}
	if err := m.Reload(context.Background()); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Directory() string { return m.directory }

func (m *Manager) StartWatch(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	m.watchCancel = cancel
	m.mu.Unlock()
	go func() {
		previous := m.sourceStamp()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current := m.sourceStamp()
				if current != previous {
					_ = m.Reload(ctx)
					previous = current
				}
			}
		}
	}()
}

func (m *Manager) Close() {
	m.mu.Lock()
	cancel := m.watchCancel
	m.watchCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) sourceStamp() string {
	return treeStamp(m.directory)
}

func (m *Manager) Compiled(id string) (*Compiled, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.items[id]
	return item, ok
}

// CompileDraft compiles an unsaved definition in memory (npm-less: a draft
// with dependencies still needs a lock file on disk, so dependency resolution
// stays a save-time concern). Nothing touches the extension directory.
func (m *Manager) CompileDraft(ctx context.Context, manifest Manifest, files map[string]string) (*Compiled, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := validateFileMap(files); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp("", "llama-swap-extension-draft-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.yaml"), manifestBytes, 0o600); err != nil {
		return nil, err
	}
	if err := writeTree(stage, files); err != nil {
		return nil, err
	}
	compiled, err := compileDirectory(ctx, stage)
	if err != nil {
		return nil, err
	}
	compiled.Manifest = manifest
	compiled.network = m.network
	return compiled, nil
}

func (m *Manager) Reload(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	entries, err := os.ReadDir(m.directory)
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Lock()
		m.items = map[string]*Compiled{}
		m.errors = map[string]string{}
		m.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	next := map[string]*Compiled{}
	failed := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		root := filepath.Join(m.directory, entry.Name())
		etag, etagErr := directoryETag(root)
		m.mu.RLock()
		old := m.items[entry.Name()]
		m.mu.RUnlock()
		if etagErr == nil && old != nil && old.ETag == etag {
			next[entry.Name()] = old
			continue
		}
		compiled, err := compileDirectory(ctx, root)
		if err != nil {
			failed[entry.Name()] = err.Error()
			continue
		}
		if compiled.Manifest.ID != entry.Name() {
			failed[entry.Name()] = "manifest id does not match directory name"
			continue
		}
		compiled.network = m.network
		next[entry.Name()] = compiled
	}
	m.mu.Lock()
	for id := range failed {
		if old := m.items[id]; old != nil {
			next[id] = old
		}
	}
	m.items, m.errors = next, failed
	m.mu.Unlock()
	return nil
}

func (m *Manager) Match(ctx Context) []*Compiled {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := sortedIDs(m.items)
	result := make([]*Compiled, 0, len(ids))
	for _, id := range ids {
		item := m.items[id]
		if item.Manifest.Enabled && item.Manifest.Matches(ctx) {
			result = append(result, item)
		}
	}
	return result
}

func (m *Manager) List() []Definition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := sortedIDs(m.items)
	invalidIDs := make([]string, 0)
	for id := range m.errors {
		if m.items[id] == nil {
			invalidIDs = append(invalidIDs, id)
		}
	}
	sort.Strings(invalidIDs)
	ids = append(ids, invalidIDs...)
	result := make([]Definition, 0, len(ids))
	for _, id := range ids {
		item := m.items[id]
		if item == nil {
			result = append(result, Definition{Manifest: Manifest{ID: id}, Status: "invalid", LastError: m.errors[id]})
			continue
		}
		status := "ready"
		if !item.Manifest.Enabled {
			status = "disabled"
		}
		if m.errors[id] != "" {
			status = "stale"
		}
		lastError := m.errors[id]
		if lastError == "" {
			lastError = item.LastError()
			if lastError != "" {
				status = "error"
			}
		}
		result = append(result, Definition{Manifest: item.Manifest, Settings: item.Settings, ETag: item.ETag, Status: status, LastError: lastError})
	}
	return result
}

func (m *Manager) Get(id string) (Definition, error) {
	m.opMu.RLock()
	defer m.opMu.RUnlock()
	return m.get(id)
}

func (m *Manager) get(id string) (Definition, error) {
	if !safeID(id) {
		return Definition{}, os.ErrNotExist
	}
	root := filepath.Join(m.directory, id)
	info, err := os.Lstat(root)
	if err != nil {
		return Definition{}, err
	}
	if !info.IsDir() {
		return Definition{}, os.ErrPermission
	}
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.yaml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Definition{}, err
	}
	files, err := readTree(root)
	if err != nil {
		return Definition{}, err
	}
	delete(files, "manifest.yaml")
	var d Definition
	decodeErr := yaml.Unmarshal(manifest, &d.Manifest)
	if decodeErr != nil || d.Manifest.ID == "" {
		d.Manifest.ID = id
	}
	d.Files = files
	if directories, dirErr := listEmptyDirectories(root); dirErr == nil {
		d.Directories = directories
	}
	d.ETag = treeETag(manifest, files)
	m.mu.RLock()
	d.LastError = m.errors[id]
	if d.LastError == "" && decodeErr != nil {
		d.LastError = decodeErr.Error()
	}
	stale := d.LastError != ""
	if d.LastError == "" {
		if item := m.items[id]; item != nil {
			d.LastError = item.LastError()
		}
	}
	if item := m.items[id]; item != nil {
		// Settings are read back from the compiled bundle, not the manifest,
		// because only the compiled form has them.
		d.Settings = item.Settings
	}
	m.mu.RUnlock()
	d.Status = "ready"
	if decodeErr != nil {
		d.Status = "invalid"
	} else if d.LastError != "" {
		if stale {
			d.Status = "stale"
		} else {
			d.Status = "error"
		}
	}
	return d, nil
}

func (m *Manager) Save(ctx context.Context, d Definition, ifMatch string) (Definition, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := d.Manifest.Validate(); err != nil {
		return Definition{}, err
	}
	files := stripManifest(d.Files)
	if err := validateFileMap(files); err != nil {
		return Definition{}, err
	}
	directories, err := validateDirectoryEntries(files, d.Directories)
	if err != nil {
		return Definition{}, err
	}
	d.Directories = directories
	root := filepath.Join(m.directory, d.Manifest.ID)
	old, oldErr := m.get(d.Manifest.ID)
	if oldErr == nil && old.ETag != ifMatch {
		return Definition{}, errors.New("etag conflict")
	}
	if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
		return Definition{}, oldErr
	}
	if oldErr != nil && ifMatch != "" {
		return Definition{}, errors.New("etag conflict")
	}
	if err := os.MkdirAll(m.directory, 0o700); err != nil {
		return Definition{}, err
	}
	stage, err := os.MkdirTemp(m.directory, ".stage-")
	if err != nil {
		return Definition{}, err
	}
	defer os.RemoveAll(stage)
	manifest, err := yaml.Marshal(d.Manifest)
	if err != nil {
		return Definition{}, err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.yaml"), manifest, 0o600); err != nil {
		return Definition{}, err
	}
	if err := writeTree(stage, files); err != nil {
		return Definition{}, err
	}
	// Empty directories are real on-disk folders so a script can rely on them
	// (create/write inside) across reloads.
	for _, directory := range directories {
		if err := os.MkdirAll(filepath.Join(stage, filepath.FromSlash(directory)), 0o700); err != nil {
			return Definition{}, err
		}
	}
	compiled, err := compileDirectory(ctx, stage)
	if err != nil {
		return Definition{}, err
	}
	if diagnostics := validateSettingsValues(compiled.Settings, compiled.Manifest.Config); len(diagnostics) > 0 {
		return Definition{}, settingsValidationError(diagnostics)
	}
	keep := map[string]struct{}{}
	for name := range files {
		cleaned, cleanErr := normalizeExtensionPath(name)
		if cleanErr != nil {
			return Definition{}, cleanErr
		}
		keep[cleaned] = struct{}{}
	}
	// Declared empty directories survive the post-rename prune.
	for _, directory := range directories {
		keep[directory] = struct{}{}
	}
	if oldErr == nil {
		backup := filepath.Join(m.directory, ".previous-"+d.Manifest.ID)
		_ = os.RemoveAll(backup)
		if err := os.Rename(root, backup); err != nil {
			return Definition{}, err
		}
		if err := os.Rename(stage, root); err != nil {
			_ = os.Rename(backup, root)
			return Definition{}, err
		}
		if err := removeFilesNotIn(root, keep, directories); err != nil {
			return Definition{}, err
		}
		_ = os.RemoveAll(backup)
	} else if err := os.Rename(stage, root); err != nil {
		return Definition{}, err
	}
	m.mu.Lock()
	m.items[d.Manifest.ID] = compiled
	delete(m.errors, d.Manifest.ID)
	m.mu.Unlock()
	return m.get(d.Manifest.ID)
}

func (m *Manager) Delete(id, ifMatch string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	old, err := m.get(id)
	if err != nil {
		return err
	}
	if old.ETag != ifMatch {
		return errors.New("etag conflict")
	}
	if err := os.RemoveAll(filepath.Join(m.directory, id)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.items, id)
	delete(m.errors, id)
	m.mu.Unlock()
	return nil
}

func directoryETag(root string) (string, error) {
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		return "", err
	}
	files, err := readTree(root)
	if err != nil {
		return "", err
	}
	delete(files, "manifest.yaml")
	return treeETag(manifest, files), nil
}

func safeID(id string) bool { return (Manifest{ID: id}).Validate() == nil }

// treeETag hashes the manifest plus every managed file in a stable order, so a
// change to any module of a multi-file extension changes its ETag.
func treeETag(manifest []byte, files map[string]string) string {
	h := sha256.New()
	_, _ = h.Write(manifest)
	_, _ = h.Write([]byte{0})
	for _, name := range sortedTreePaths(files) {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte(files[name]))
		_, _ = h.Write([]byte{0})
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

//go:embed sdk/dist/extension.js
var sdkBundle string

// sdkModulePath materializes the bundled SDK to a temp file so esbuild's Alias
// option can resolve `@llama-swap/extension` to it. The file lives for the
// process lifetime; the content is fixed at build time.
func sdkModulePath() string {
	sdkOnce.Do(func() {
		file, err := os.CreateTemp("", "llama-swap-sdk-*.js")
		if err != nil {
			sdkPathErr = err
			return
		}
		if _, err := file.WriteString(sdkBundle); err != nil {
			file.Close()
			sdkPathErr = err
			return
		}
		if err := file.Close(); err != nil {
			sdkPathErr = err
			return
		}
		sdkPath = file.Name()
	})
	if sdkPathErr != nil {
		return ""
	}
	return sdkPath
}

var (
	sdkOnce    sync.Once
	sdkPath    string
	sdkPathErr error
)

func compileDirectory(ctx context.Context, root string) (*Compiled, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.yaml"))
	if err != nil {
		return nil, err
	}
	files, err := readTree(root)
	if err != nil {
		return nil, err
	}
	// The manifest is hashed separately, so it must not also appear in the
	// tree: Reload compares this ETag against directoryETag to skip unchanged
	// extensions, and a mismatch here would recompile every directory every
	// two seconds.
	delete(files, ManifestFile)
	var manifest Manifest
	if err := yaml.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	pkg, lock := files["package.json"], files["package-lock.json"]
	working := root
	if len(pkg) > 0 {
		// A package.json with no dependencies has nothing to lock, so a fresh
		// scaffold installs without the user having to run npm by hand.
		if !hasDependencies([]byte(pkg)) {
			lock = ""
		} else if len(lock) == 0 {
			return nil, fmt.Errorf("extension %s: package-lock.json is required", manifest.ID)
		}
		stage, err := os.MkdirTemp("", "llama-swap-extension-deps-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		if err := writeTree(stage, files); err != nil {
			return nil, err
		}
		if err := runExtensionNPM(ctx, stage); err != nil {
			return nil, fmt.Errorf("extension %s: %w", manifest.ID, err)
		}
		working = stage
	}
	result := api.Build(api.BuildOptions{
		EntryPoints:   []string{filepath.Join(working, EntryFile)},
		AbsWorkingDir: working,
		Bundle:        true, Write: false,
		Platform: api.PlatformNeutral, Format: api.FormatIIFE,
		GlobalName: "__llamaSwapExtension",
		LogLevel:   api.LogLevelSilent,
		// `import { defineTool } from "@llama-swap/extension"` resolves to the
		// bundled SDK instead of npm: the SDK is part of the runtime contract.
		Alias: map[string]string{"@llama-swap/extension": sdkModulePath()},
	})
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("extension %s: bundle: %s", manifest.ID, bundleErrorText(result.Errors))
	}
	if len(result.OutputFiles) != 1 {
		return nil, fmt.Errorf("extension %s: no JavaScript output", manifest.ID)
	}
	// Empty directories are part of the definition: they appear in the UI tree
	// and must survive reloads, so they factor into the ETag.
	emptyDirectories, err := listEmptyDirectories(working)
	if err != nil {
		return nil, err
	}
	compiled := &Compiled{
		Manifest: manifest, Bundle: string(result.OutputFiles[0].Contents),
		ETag: treeETag(manifestBytes, files), Directories: emptyDirectories,
		// A lone compile (the editor's check endpoint, tests) has no daemon
		// policy to inherit; Reload stamps the configured one below.
		network: netproxy.FromEnv(),
	}
	if len(compiled.Bundle) > 4<<20 {
		return nil, fmt.Errorf("extension %s: bundle exceeds 4 MiB", manifest.ID)
	}
	if err := compiled.Inspect(ctx); err != nil {
		return nil, fmt.Errorf("extension %s: %w", manifest.ID, err)
	}
	if err := validateCompiledTools(compiled); err != nil {
		return nil, fmt.Errorf("extension %s: %w", manifest.ID, err)
	}
	return compiled, nil
}

// hasDependencies reports whether a package.json declares anything to install.
// A scaffolded package.json declares none, so it installs without a lock file.
func hasDependencies(packageJSON []byte) bool {
	var parsed struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		Optional        map[string]string `json:"optionalDependencies"`
		Peer            map[string]string `json:"peerDependencies"`
	}
	if err := json.Unmarshal(packageJSON, &parsed); err != nil {
		// Unparseable is not "no dependencies": let npm report it.
		return true
	}
	return len(parsed.Dependencies)+len(parsed.DevDependencies)+len(parsed.Optional)+len(parsed.Peer) > 0
}

// runExtensionNPM installs a staged tree's dependencies. Install scripts stay
// disabled: whatever an extension needs at build time is the author's to run,
// and an install script could do anything at all.
func runExtensionNPM(ctx context.Context, stage string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "ci", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", "--prefix", stage)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + stage, "npm_config_ignore_scripts=true", "npm_config_audit=false"}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "NPM_CONFIG_REGISTRY"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	stdout := &boundedBuffer{limit: 128 << 10}
	stderr := &boundedBuffer{limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("npm ci: %w", err)
	}
	cleanup := attachExtensionCgroup(cmd.Process.Pid, 1024)
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid, _ := process.NewProcess(int32(cmd.Process.Pid))
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("npm ci failed: %w", err)
			}
			return nil
		case <-ticker.C:
			if pid == nil {
				continue
			}
			if memory, err := pid.MemoryInfo(); err == nil && memory.RSS > 1<<30 {
				_ = cmd.Process.Kill()
				<-done
				return errors.New("npm ci memory limit exceeded")
			}
			if times, err := pid.Times(); err == nil && (times.User+times.System) > 90 {
				_ = cmd.Process.Kill()
				<-done
				return errors.New("npm ci CPU limit exceeded")
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return ctx.Err()
		}
	}
}

// bundleErrorText renders esbuild errors with their position. With several
// modules in one extension the message is the only place the author learns
// which file is broken.
func bundleErrorText(messages []api.Message) string {
	primary := messages[0]
	text := primary.Text
	if primary.Location != nil {
		text = fmt.Sprintf("%s:%d:%d %s", primary.Location.File, primary.Location.Line, primary.Location.Column, text)
	}
	if len(messages) > 1 {
		return fmt.Sprintf("%s (and %d more)", text, len(messages)-1)
	}
	return text
}

// ToolNames lists the names of the tools this extension declares, sorted.
func (c *Compiled) ToolNames() []string {
	names := make([]string, 0, len(c.Tools))
	for _, tool := range c.Tools {
		var function struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool.Function, &function); err != nil || function.Name == "" {
			continue
		}
		names = append(names, function.Name)
	}
	sort.Strings(names)
	return names
}

func validateCompiledTools(c *Compiled) error {
	names := map[string]bool{}
	for _, tool := range c.Tools {
		if tool.Type != "function" {
			return errors.New("only function tools are supported")
		}
		if tool.Execution != "" && tool.Execution != "client" && tool.Execution != "server" {
			return errors.New("tool execution must be client or server")
		}
		if tool.Execution == "server" {
			handler := false
			for _, hook := range c.Hooks {
				if hook == "onToolCall" {
					handler = true
					break
				}
			}
			for _, bound := range c.ToolHandlers {
				var function struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(tool.Function, &function); err == nil && function.Name == bound {
					handler = true
					break
				}
			}
			if !handler {
				return errors.New("server tool requires onToolCall")
			}
		}
		var function struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tool.Function, &function); err != nil || function.Name == "" {
			return errors.New("tool function must have a name")
		}
		if names[function.Name] {
			return fmt.Errorf("duplicate tool %q", function.Name)
		}
		names[function.Name] = true
	}
	return nil
}

// SetLogSink installs the live log sink once; later calls are no-ops so a
// request goroutine cannot replace a sink another goroutine already installed.
func (c *Compiled) SetLogSink(sink func(logLine)) {
	if c == nil {
		return
	}
	c.statusMu.Lock()
	if c.LogSink == nil {
		c.LogSink = sink
	}
	c.statusMu.Unlock()
}

// logSink returns the installed sink under the same lock that guards its
// installation; reading the field directly raced with SetLogSink.
func (c *Compiled) logSink() func(logLine) {
	if c == nil {
		return nil
	}
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.LogSink
}
