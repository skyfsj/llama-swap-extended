package runtime

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// tarTypeRegA is the historical NUL typeflag emitted by old tar writers.
// archive/tar exposes the same value as the deprecated TypeRegA constant; keep
// the byte literal local so archives remain compatible without carrying a
// deprecated API reference through static analysis.
const tarTypeRegA byte = 0

// maxRuntimeMetadataBytes bounds provider update indexes. These responses are
// expected to be small JSON documents; reading one extra byte lets us reject
// an oversized response instead of silently parsing a truncated prefix.
const maxRuntimeMetadataBytes int64 = 16 << 20

// maxVLLMProbeBytes bounds the fixed Python probe output.  The probe is
// expected to emit a handful of version strings; accepting a multi-megabyte
// stdout here would let a broken/compromised interpreter consume memory and
// would risk persisting arbitrary metadata into the runtime manifest.
const maxVLLMProbeBytes int = 64 << 10

// maxRuntimeSmokeCommandBytes bounds the optional post-stage verification
// command stored in a runtime manifest. Smoke commands are operator supplied
// and are executed as argv (never through a shell), so a bound here prevents a
// malformed manifest from turning every health check into an unbounded parse
// or allocation.
const maxRuntimeSmokeCommandBytes = 4096

// CommandRunner is deliberately argv-based. Runtime providers never pass a
// configured command through a shell, so spaces and metacharacters cannot
// turn a runtime update into arbitrary command execution.
type CommandRunner func(context.Context, string, string, ...string) ([]byte, error)

// EnvCommandRunner is the environment-aware sibling of CommandRunner. It is
// optional so embedders that already inject argv-only runners remain source
// compatible; built-in providers use it when a runtime declares build.env.
type EnvCommandRunner func(context.Context, string, map[string]string, string, ...string) ([]byte, error)

func defaultCommandRunner(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return commandOutputWithDiagnostics(ctx, cmd)
}

func defaultCommandRunnerWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		// Build a replacement environment instead of appending duplicate
		// KEY=value entries. POSIX leaves duplicate entries technically
		// representable, but the value returned by getenv is implementation
		// dependent; an operator supplied build override must deterministically
		// win over the inherited process environment.
		overrides := make(map[string]string, len(env))
		for key, value := range env {
			overrides[key] = value
		}
		cmd.Env = make([]string, 0, len(os.Environ())+len(overrides))
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if _, overridden := overrides[key]; overridden {
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			cmd.Env = append(cmd.Env, key+"="+env[key])
		}
	}
	return commandOutputWithDiagnostics(ctx, cmd)
}

// commandOutputCollector preserves the combined command output for error
// diagnostics while forwarding each stdout/stderr write to the runtime log
// stream. os/exec may write the two streams concurrently, hence the small
// mutex around the aggregate buffer.
type commandOutputBuffer struct {
	mu     sync.Mutex
	output bytes.Buffer
}

type commandOutputCollector struct {
	ctx    context.Context
	stream string
	buffer *commandOutputBuffer
}

func (w *commandOutputCollector) Write(data []byte) (int, error) {
	if w == nil || w.buffer == nil {
		return 0, errors.New("runtime command output buffer is nil")
	}
	w.buffer.mu.Lock()
	n, err := w.buffer.output.Write(data)
	w.buffer.mu.Unlock()
	if err != nil {
		return n, err
	}
	if n != len(data) {
		// bytes.Buffer never short-writes today, but this writer sits on a
		// child's stdout drain where a short count becomes io.Copy's
		// ErrShortWrite and kills the build command with SIGPIPE. Treat a
		// partial write as an explicit error rather than returning a count that
		// silently tears the pipe down.
		return n, io.ErrShortWrite
	}
	reportRuntimeLog(w.ctx, w.stream, data)
	return n, nil
}

func (w *commandOutputCollector) Bytes() []byte {
	if w == nil || w.buffer == nil {
		return nil
	}
	w.buffer.mu.Lock()
	defer w.buffer.mu.Unlock()
	return append([]byte(nil), w.buffer.output.Bytes()...)
}

// commandOutputWithDiagnostics keeps provider failures actionable. Build
// tools conventionally write compiler and linker errors to stderr; returning
// only exec.Cmd.Output's stdout turns those failures into an opaque exit code
// in the runtime manager UI. Keep the diagnostic bounded because it is carried
// through the manager's operation state and API response.
func commandOutputWithDiagnostics(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	buffer := &commandOutputBuffer{}
	stdout := &commandOutputCollector{ctx: ctx, stream: "stdout", buffer: buffer}
	stderr := &commandOutputCollector{ctx: ctx, stream: "stderr", buffer: buffer}
	// The collector publishes every write to the runtime log stream, and a
	// publisher can backpressure (see event.Broadcast). os/exec drains these
	// pipes on its own copier goroutine, so publishing inline would let a slow
	// log subscriber fill the pipe and kill a build with SIGPIPE. DrainWriter
	// moves the publishing off that path; the copies below flush it.
	stdoutDrain := logmon.NewDrainWriter(stdout)
	stderrDrain := logmon.NewDrainWriter(stderr)
	cmd.Stdout = stdoutDrain
	cmd.Stderr = stderrDrain
	err := cmd.Run()
	// Run has returned, so both copiers are done and every byte is either
	// delivered or queued; flush before reading the aggregate, or the
	// diagnostic could come up short.
	stdoutDrain.Close()
	stderrDrain.Close()
	output := stdout.Bytes()
	if err == nil {
		return output, nil
	}
	const maxDiagnosticBytes = 12 << 10
	diagnostic := strings.TrimSpace(string(output))
	if len(diagnostic) > maxDiagnosticBytes {
		diagnostic = diagnostic[len(diagnostic)-maxDiagnosticBytes:]
		diagnostic = "..." + diagnostic
	}
	if diagnostic == "" {
		return output, err
	}
	return output, fmt.Errorf("%w: %s", err, diagnostic)
}

// runRuntimeCommand adds a cancellation check before and after a provider
// command. CommandRunner is injectable for tests and remote builders, and a
// third-party runner may ignore context cancellation; checking at this
// boundary prevents later install/build steps from continuing after a caller
// has already canceled the operation.
func runRuntimeCommand(ctx context.Context, run CommandRunner, directory, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("runtime command runner is nil")
	}
	beginRuntimeCommandOutput(ctx)
	output, err := run(ctx, directory, name, args...)
	reportRuntimeCommandOutput(ctx, output)
	if err != nil {
		return output, err
	}
	if err := ctx.Err(); err != nil {
		return output, err
	}
	return output, nil
}

func runRuntimeCommandEnv(ctx context.Context, run CommandRunner, runEnv EnvCommandRunner, env map[string]string, directory, name string, args ...string) ([]byte, error) {
	if len(env) == 0 {
		return runRuntimeCommand(ctx, run, directory, name, args...)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runEnv != nil {
		beginRuntimeCommandOutput(ctx)
		output, err := runEnv(ctx, directory, cloneMetadata(env), name, args...)
		reportRuntimeCommandOutput(ctx, output)
		if err != nil {
			return output, err
		}
		if err := ctx.Err(); err != nil {
			return output, err
		}
		return output, nil
	}
	if run == nil {
		return defaultCommandRunnerWithEnv(ctx, directory, env, name, args...)
	}
	// An argv-only injected runner cannot transport environment overrides. Keep
	// its established behavior instead of rewriting the command through `env`
	// (which would alter argv and reintroduce portability surprises).
	return runRuntimeCommand(ctx, run, directory, name, args...)
}

// runStructuredBuildSteps executes configured build steps without invoking a
// shell. WorkDir is resolved against the staged runtime directory and the
// small set of documented absolute macros (SOURCE_DIR, BUILD_DIR, PYTHON and
// UV) is expanded before the command is launched.
func runStructuredBuildSteps(ctx context.Context, destination, defaultDir string, steps []BuildStep, baseEnv map[string]string, vars map[string]string, run CommandRunner, runEnv EnvCommandRunner) error {
	if len(steps) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for index, step := range steps {
		command := expandRuntimeMacro(step.Command, vars)
		if strings.TrimSpace(command) == "" || strings.TrimSpace(command) != command {
			return fmt.Errorf("build step %d command is empty or not normalized", index)
		}
		if err := validateRuntimeCommandToken(command); err != nil {
			return fmt.Errorf("build step %d: %w", index, err)
		}
		dir := defaultDir
		if workDir := strings.TrimSpace(step.WorkDir); workDir != "" {
			expanded := expandRuntimeMacro(workDir, vars)
			resolved, err := resolveRuntimeWorkDir(destination, expanded)
			if err != nil {
				return fmt.Errorf("build step %d workDir: %w", index, err)
			}
			dir = resolved
		}
		if err := rejectSymlinkPath(dir); err != nil {
			return fmt.Errorf("build step %d workDir: %w", index, err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			if err != nil {
				return fmt.Errorf("build step %d workDir: %w", index, err)
			}
			return fmt.Errorf("build step %d workDir is not a directory", index)
		}
		env := cloneMetadata(baseEnv)
		if env == nil && len(step.Env) > 0 {
			env = make(map[string]string, len(step.Env))
		}
		for key, value := range step.Env {
			env[key] = expandRuntimeMacro(value, vars)
		}
		args := make([]string, len(step.Args))
		for i, value := range step.Args {
			args[i] = expandRuntimeMacro(value, vars)
			if err := validateRuntimeCommandToken(args[i]); err != nil {
				return fmt.Errorf("build step %d arg %d: %w", index, i, err)
			}
		}
		progress := 0.5
		if len(steps) > 1 {
			progress = 0.5 + 0.35*float64(index)/float64(len(steps))
		}
		reportRuntimeProgress(ctx, "building", progress, fmt.Sprintf("running build step %d/%d", index+1, len(steps)))
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, env, dir, command, args...); err != nil {
			return fmt.Errorf("build step %d (%s): %w", index, command, err)
		}
	}
	return nil
}

func expandRuntimeMacro(value string, vars map[string]string) string {
	for key, replacement := range vars {
		value = strings.ReplaceAll(value, "${"+key+"}", replacement)
	}
	return value
}

func resolveRuntimeWorkDir(destination, value string) (string, error) {
	if value == "" {
		return destination, nil
	}
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("workDir contains NUL")
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	pathValue := value
	if !filepath.IsAbs(pathValue) {
		pathValue = filepath.Join(root, pathValue)
	}
	resolved, err := filepath.Abs(pathValue)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("workDir escapes runtime directory")
	}
	return resolved, nil
}

func validateRuntimeCommandToken(value string) error {
	if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
		return errors.New("command argument is too long or contains NUL")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("command argument contains control characters")
		}
	}
	return nil
}

func copyRuntimeArtifacts(ctx context.Context, destination string, artifacts []Artifact, vars map[string]string) error {
	for index, artifact := range artifacts {
		from := expandRuntimeMacro(artifact.From, vars)
		to := expandRuntimeMacro(artifact.To, vars)
		source, err := resolveRuntimeArtifactPath(destination, from)
		if err != nil {
			return fmt.Errorf("artifact %d source: %w", index, err)
		}
		target, err := resolveRuntimeArtifactPath(destination, to)
		if err != nil {
			return fmt.Errorf("artifact %d target: %w", index, err)
		}
		if err := rejectSymlinkPath(source); err != nil {
			return fmt.Errorf("artifact %d source: %w", index, err)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("artifact %d source: %w", index, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact %d source is a symlink", index)
		}
		if info.IsDir() {
			if err := rejectSymlinkPath(target); err != nil {
				return fmt.Errorf("artifact %d target: %w", index, err)
			}
			if existing, statErr := os.Lstat(target); statErr == nil {
				if existing.Mode()&os.ModeSymlink != 0 || !existing.IsDir() {
					return fmt.Errorf("artifact %d target is not a real directory", index)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("artifact %d target: %w", index, statErr)
			}
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			if err := copyTree(ctx, source, target); err != nil {
				return fmt.Errorf("artifact %d copy: %w", index, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact %d source is not a regular file", index)
		}
		if err := rejectSymlinkPath(filepath.Dir(target)); err != nil {
			return fmt.Errorf("artifact %d target: %w", index, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if existing, statErr := os.Lstat(target); statErr == nil {
			if existing.Mode()&os.ModeSymlink != 0 || !existing.Mode().IsRegular() {
				return fmt.Errorf("artifact %d target is not a regular file", index)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("artifact %d target: %w", index, statErr)
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		syncErr := output.Sync()
		closeOutErr := output.Close()
		closeInErr := input.Close()
		if err := errors.Join(copyErr, syncErr, closeOutErr, closeInErr); err != nil {
			return fmt.Errorf("artifact %d copy: %w", index, err)
		}
	}
	return nil
}

func resolveRuntimeArtifactPath(destination, value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
		return "", errors.New("artifact path is required")
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	pathValue := value
	if !filepath.IsAbs(pathValue) {
		pathValue = filepath.Join(root, pathValue)
	}
	pathValue, err = filepath.Abs(pathValue)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, pathValue)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact path escapes runtime directory")
	}
	return pathValue, nil
}

type VLLMProvider struct {
	UVPath   string
	Python   string
	IndexURL string
	// AllowPipFallback lets Stage fall back to `python -m venv` and
	// `python -m pip install` when uv is not found on the runtime
	// environment PATH. The fallback only applies when UVPath is unset, so
	// an explicitly configured uv binary still fails fast when missing.
	AllowPipFallback bool
	// UpdateURL overrides the PyPI JSON endpoint used by CheckForUpdate. It is
	// primarily useful for controlled mirrors and deterministic tests; the
	// default is the public vLLM package metadata endpoint.
	UpdateURL       string
	Extras          []string
	SourceAllowlist []string
	// Client and MaxDownloadBytes are injectable for air-gapped deployments
	// and deterministic tests. A nil client uses a bounded default timeout.
	Client           *http.Client
	MaxDownloadBytes int64
	Run              CommandRunner
	RunEnv           EnvCommandRunner
}

// CheckForUpdate discovers the newest vLLM package version for a PyPI-backed
// runtime. Explicit update.version values remain authoritative. A Git runtime
// with an explicit trackRef may resolve that ref even before its first stage;
// declaring both automatic policy and trackRef is the operator's opt-in to the
// metadata request.
func (p VLLMProvider) CheckForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(desired.Version) != "" {
		return desired, desiredDiff(current, desired), nil
	}
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferVLLMSourceType(desired.Source)
	}
	if sourceType == "git" && strings.TrimSpace(desired.Metadata["trackRef"]) != "" {
		return checkTrackedGitUpdate(ctx, p.Run, p.SourceAllowlist, current, desired)
	}
	if strings.TrimSpace(current.Version) == "" {
		return desired, false, nil
	}
	if sourceType != "pypi" {
		return desired, desiredDiff(current, desired), nil
	}
	endpoint := strings.TrimSpace(p.UpdateURL)
	if endpoint == "" {
		endpoint = "https://pypi.org/pypi/vllm/json"
	}
	if !validRuntimeURL(endpoint, p.SourceAllowlist) {
		return desired, false, fmt.Errorf("vLLM update URL is not allowed")
	}
	client := guardedRuntimeClient(p.Client, p.SourceAllowlist, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return desired, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return desired, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return desired, false, fmt.Errorf("vLLM update metadata returned HTTP %d", resp.StatusCode)
	}
	var metadata struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := decodeBoundedRuntimeJSON(resp.Body, maxRuntimeMetadataBytes, &metadata); err != nil {
		return desired, false, fmt.Errorf("decode vLLM update metadata: %w", err)
	}
	latest := strings.TrimSpace(metadata.Info.Version)
	if len(metadata.Releases) > 0 {
		for version := range metadata.Releases {
			if !isRuntimeVersionCandidate(version, policy.Channel) {
				continue
			}
			if latest == "" || compareRuntimeVersions(version, latest) > 0 {
				latest = version
			}
		}
	}
	if latest == "" || !isRuntimeVersionCandidate(latest, policy.Channel) {
		return desired, false, nil
	}
	// Automatic checks must never turn a stale or reordered package index into
	// a downgrade. PyPI's `info.version` is not guaranteed to be the highest
	// release when mirrors lag or metadata is cached, so compare it with the
	// active manifest before advertising an update.
	if current.Version != "" && compareRuntimeVersions(latest, current.Version) <= 0 {
		return desired, false, nil
	}
	candidate := desired
	candidate.Version = latest
	if candidate.Source == "" {
		candidate.Source = "pypi"
	}
	if candidate.SourceType == "" {
		candidate.SourceType = "pypi"
	}
	return candidate, desiredDiff(current, candidate), nil
}

func (p VLLMProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime.GOOS != "linux" {
		return Manifest{}, errors.New("vLLM managed runtimes are supported on Linux only")
	}
	if err := validateProviderStage(spec, "vllm"); err != nil {
		return Manifest{}, err
	}
	if err := prepareProviderDestination(destination); err != nil {
		return Manifest{}, fmt.Errorf("vLLM stage destination: %w", err)
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	runEnv := p.RunEnv
	if runEnv == nil && p.Run == nil {
		runEnv = defaultCommandRunnerWithEnv
	}
	uv := p.UVPath
	if uv == "" {
		uv = "uv"
	}
	uvAvailable := true
	if p.AllowPipFallback && p.UVPath == "" {
		if _, lookupErr := exec.LookPath(uv); lookupErr != nil {
			uvAvailable = false
		}
	}
	python := p.Python
	if configured := firstBuildArg(spec, "python"); configured != "" {
		python = configured
	}
	if python == "" {
		python = "python3"
	}
	driver := strings.ToLower(strings.TrimSpace(firstBuildArg(spec, "driver")))
	if driver == "" {
		driver = "uv"
	}
	if driver != "uv" && driver != "custom" {
		return Manifest{}, fmt.Errorf("vLLM build driver %q is unsupported", driver)
	}
	venv := filepath.Join(destination, ".venv")
	reportRuntimeProgress(ctx, "staging", 0.12, "creating vLLM virtualenv")
	if uvAvailable {
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, uv, "venv", "--python", python, venv); err != nil {
			return Manifest{}, fmt.Errorf("create vLLM virtualenv: %w", err)
		}
	} else {
		// uv is not present in the runtime environment: fall back to the
		// Python standard-library venv module so hosts without uv can still
		// stage vLLM.
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, python, "-m", "venv", venv); err != nil {
			return Manifest{}, fmt.Errorf("create vLLM virtualenv: uv is not installed and %q -m venv failed: %w", python, err)
		}
	}
	packageSpec := "vllm"
	if spec.Version != "" {
		packageSpec += "==" + spec.Version
	}
	source := strings.TrimSpace(spec.Source)
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType == "" {
		sourceType = inferVLLMSourceType(source)
	}
	if sourceType == "" {
		// Empty source means the default PyPI package, matching uv's normal
		// install behavior. Record that choice explicitly for manifests and
		// runtime diagnostics.
		sourceType = "pypi"
	}
	var sourceChecksum string
	var sourceRoot string
	switch sourceType {
	case "", "pypi", "bundled":
		// The default package is installed from the configured index. A
		// bundled runtime is represented by the same package spec; image seed
		// handling belongs to the manager, not the provider.
	case "git":
		if spec.Ref == "" {
			return Manifest{}, errors.New("vLLM git source requires an exact ref")
		}
		gitSource := strings.TrimPrefix(source, "git+")
		if !validGitSource(gitSource, p.SourceAllowlist) {
			return Manifest{}, errors.New("vLLM git source is not allowed")
		}
		packageSpec = "git+" + gitSource + "@" + spec.Ref
		if driver == "custom" {
			sourceRoot = filepath.Join(destination, "source")
			reportRuntimeProgress(ctx, "downloading", 0.2, "cloning vLLM source")
			if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, "git", "clone", "--no-checkout", gitSource, sourceRoot); err != nil {
				return Manifest{}, fmt.Errorf("clone vLLM source: %w", err)
			}
			if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, sourceRoot, "git", "checkout", "--detach", spec.Ref); err != nil {
				return Manifest{}, fmt.Errorf("checkout vLLM ref: %w", err)
			}
		}
	case "wheel", "release":
		if source == "" {
			return Manifest{}, errors.New("vLLM wheel source is required")
		}
		if localPath, ok := runtimeSourcePath(source); ok {
			if err := rejectSymlinkPath(localPath); err != nil {
				return Manifest{}, fmt.Errorf("vLLM source path: %w", err)
			}
			info, statErr := os.Stat(localPath)
			if statErr != nil {
				return Manifest{}, fmt.Errorf("vLLM source path: %w", statErr)
			}
			if !info.Mode().IsRegular() {
				return Manifest{}, errors.New("vLLM wheel source must be a regular file")
			}
			if !pathAllowed(localPath, p.SourceAllowlist) {
				return Manifest{}, errors.New("vLLM source path is not allowed")
			}
			var checksumErr error
			sourceChecksum, checksumErr = fileChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash vLLM source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("vLLM source checksum: %w", err)
			}
			packageSpec = localPath
		} else if validRuntimeURL(source, p.SourceAllowlist) {
			reportRuntimeProgress(ctx, "downloading", 0.2, "downloading vLLM wheel")
			artifact, checksum, downloadErr := downloadRuntimeArtifactWithProgress(ctx, p.Client, source, destination, p.MaxDownloadBytes, p.SourceAllowlist, func(completed, total int64) {
				progress := progressFraction(completed, total, 0.2, 0.4)
				reportRuntimeProgressBytes(ctx, "downloading", progress, completed, total, fmt.Sprintf("downloaded %s", formatProgressBytes(completed, total)))
			})
			if downloadErr != nil {
				return Manifest{}, fmt.Errorf("download vLLM wheel: %w", downloadErr)
			}
			sourceChecksum = checksum
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("vLLM source checksum: %w", err)
			}
			packageSpec = artifact
		} else {
			return Manifest{}, errors.New("vLLM source URL or path is not allowed")
		}
	case "local":
		localPath, ok := runtimeSourcePath(source)
		if !ok {
			return Manifest{}, errors.New("vLLM source path is not allowed")
		}
		if err := rejectSymlinkPath(localPath); err != nil {
			return Manifest{}, fmt.Errorf("vLLM source path: %w", err)
		}
		if !pathAllowed(localPath, p.SourceAllowlist) {
			return Manifest{}, errors.New("vLLM source path is not allowed")
		}
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			return Manifest{}, fmt.Errorf("vLLM source path: %w", statErr)
		}
		if info.IsDir() {
			var checksumErr error
			sourceChecksum, checksumErr = directoryChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash vLLM source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("vLLM source checksum: %w", err)
			}
			sourceRoot = filepath.Join(destination, "source")
			if err := os.MkdirAll(sourceRoot, 0o750); err != nil {
				return Manifest{}, fmt.Errorf("create vLLM source directory: %w", err)
			}
			if err := copyTree(ctx, localPath, sourceRoot); err != nil {
				return Manifest{}, fmt.Errorf("copy vLLM source: %w", err)
			}
			packageSpec = sourceRoot
		} else if info.Mode().IsRegular() {
			var checksumErr error
			sourceChecksum, checksumErr = fileChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash vLLM source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("vLLM source checksum: %w", err)
			}
			packageSpec = localPath
		} else {
			return Manifest{}, errors.New("vLLM local source must be a directory or regular wheel file")
		}
	default:
		return Manifest{}, fmt.Errorf("unsupported vLLM source type %q", spec.SourceType)
	}
	packageIdentity := packageSpec
	vars := map[string]string{
		"RUNTIME_DIR": destination,
		"SOURCE_DIR":  sourceRoot,
		"BUILD_DIR":   filepath.Join(destination, "build"),
		"PYTHON":      filepath.Join(venv, "bin", "python"),
		"UV":          uv,
	}
	indexURL := p.IndexURL
	if configured := firstBuildArg(spec, "indexURL"); configured != "" {
		indexURL = configured
	}
	extras := append([]string(nil), p.Extras...)
	if configured := buildArgs(spec, "extras"); len(configured) > 0 {
		extras = append(extras, configured...)
	} else if configured := strings.TrimSpace(spec.Build["extras"]); configured != "" {
		// Keep accepting the pre-structured representation for callers that
		// construct Spec values directly. New config paths use BuildArgs so an
		// argument containing spaces remains one argv element.
		extras = append(extras, strings.Fields(configured)...)
	}
	if driver == "custom" {
		if len(spec.BuildSteps) == 0 {
			return Manifest{}, errors.New("vLLM custom build requires at least one build step")
		}
		defaultDir := destination
		if sourceRoot != "" {
			defaultDir = sourceRoot
		}
		if configured := strings.TrimSpace(spec.Build["workDir"]); configured != "" {
			resolved, resolveErr := resolveRuntimeWorkDir(destination, expandRuntimeMacro(configured, vars))
			if resolveErr != nil {
				return Manifest{}, fmt.Errorf("vLLM custom workDir: %w", resolveErr)
			}
			defaultDir = resolved
		}
		if err := runStructuredBuildSteps(ctx, destination, defaultDir, spec.BuildSteps, spec.BuildEnv, vars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	} else if uvAvailable {
		args := []string{"pip", "install", "--python", vars["PYTHON"]}
		if indexURL != "" {
			if !validRuntimeURL(indexURL, p.SourceAllowlist) {
				return Manifest{}, errors.New("vLLM package index URL is not allowed")
			}
			args = append(args, "--index-url", indexURL)
		}
		if len(spec.InstallArgs) > 0 {
			args = append(args, spec.InstallArgs...)
		}
		if configured := buildArgs(spec, "args"); len(configured) > 0 {
			args = append(args, configured...)
		}
		args = append(args, packageSpec)
		for _, extra := range extras {
			if strings.TrimSpace(extra) != "" {
				args = append(args, extra)
			}
		}
		reportRuntimeProgress(ctx, "building", 0.45, "installing vLLM and dependencies")
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, uv, args...); err != nil {
			return Manifest{}, fmt.Errorf("install vLLM: %w", err)
		}
		if err := runStructuredBuildSteps(ctx, destination, destination, spec.BuildSteps, spec.BuildEnv, vars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	} else {
		// pip fallback (uv not available): invoke the venv interpreter
		// directly, which removes the need for uv's --python flag.
		args := []string{"-m", "pip", "install"}
		if indexURL != "" {
			if !validRuntimeURL(indexURL, p.SourceAllowlist) {
				return Manifest{}, errors.New("vLLM package index URL is not allowed")
			}
			args = append(args, "--index-url", indexURL)
		}
		if len(spec.InstallArgs) > 0 {
			args = append(args, spec.InstallArgs...)
		}
		if configured := buildArgs(spec, "args"); len(configured) > 0 {
			args = append(args, configured...)
		}
		args = append(args, packageSpec)
		for _, extra := range extras {
			if strings.TrimSpace(extra) != "" {
				args = append(args, extra)
			}
		}
		reportRuntimeProgress(ctx, "building", 0.45, "installing vLLM and dependencies (pip fallback)")
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, vars["PYTHON"], args...); err != nil {
			return Manifest{}, fmt.Errorf("install vLLM: %w", err)
		}
		if err := runStructuredBuildSteps(ctx, destination, destination, spec.BuildSteps, spec.BuildEnv, vars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	}
	if err := copyRuntimeArtifacts(ctx, destination, spec.Artifacts, vars); err != nil {
		return Manifest{}, err
	}
	metadata := cloneMetadata(spec.Metadata)
	metadata["mode"] = normalizeRuntimeMode(spec.Mode)
	metadata["sourceType"] = sourceType
	if len(spec.BuildEnv) > 0 {
		metadata["buildEnvKeys"] = strings.Join(sortedMapKeys(spec.BuildEnv), ",")
	}
	// Keep both the selected interpreter and the package/runtime versions in
	// the manifest. The probe is best-effort: an offline/air-gapped image may
	// not expose importlib metadata even though `uv pip install` succeeded, so
	// a probe failure must never turn a healthy staged environment into a
	// failed update. Explicit spec values remain the fallback and are always
	// deterministic.
	metadata["python"] = python
	metadata["pythonPath"] = python
	if spec.Version != "" {
		metadata["vllm"] = spec.Version
	}
	if uvAvailable {
		metadata["uv"] = uv
	} else {
		metadata["uv"] = "pip"
	}
	metadata["package"] = packageIdentity
	metadata["sourceType"] = sourceType
	metadata["source"] = source
	metadata["runtime"] = runtime.GOOS + "/" + runtime.GOARCH
	metadata["installedAt"] = time.Now().UTC().Format(time.RFC3339)
	if sourceChecksum != "" {
		metadata["sourceChecksum"] = sourceChecksum
	}
	if spec.Checksum != "" {
		metadata["checksum"] = normalizeChecksum(sourceChecksum)
		if metadata["checksum"] == "" {
			metadata["checksum"] = normalizeChecksum(spec.Checksum)
		}
	}
	if spec.Commit != "" {
		metadata["commit"] = spec.Commit
	}
	metadata["lockFingerprint"] = runtimeLockFingerprint(spec, packageIdentity, python, indexURL, extras, sourceChecksum)
	reportRuntimeProgress(ctx, "verifying", 0.72, "probing vLLM environment")
	if probed := probeVLLMEnvironment(ctx, run, destination, filepath.Join(venv, "bin", "python")); len(probed) > 0 {
		for key, value := range probed {
			metadata[key] = value
		}
	}
	if err := writeRuntimeMetadata(destination, metadata); err != nil {
		return Manifest{}, err
	}
	fingerprint := fingerprintMetadata(metadata)
	manifestSource := spec.Source
	if strings.TrimSpace(manifestSource) == "" {
		// A direct provider caller may omit Source for the default PyPI install;
		// keep the manifest valid and make the provenance explicit instead of
		// requiring every embedder to duplicate the config translator's fallback.
		manifestSource = sourceType
		if strings.TrimSpace(manifestSource) == "" {
			manifestSource = "pypi"
		}
	}
	manifest := Manifest{Name: spec.Name, Version: spec.Version, Kind: "vllm", Source: manifestSource, Ref: spec.Ref, Commit: spec.Commit, Checksum: spec.Checksum, Fingerprint: fingerprint, Metadata: metadata, InstalledAt: time.Now().UTC()}
	manifest.Python = python
	manifest.PythonVersion = strings.TrimSpace(metadata["pythonVersion"])
	manifest.VLLM = strings.TrimSpace(metadata["vllm"])
	if value := strings.TrimSpace(metadata["torch"]); value != "" {
		manifest.Torch = value
	}
	if value := strings.TrimSpace(metadata["cuda"]); value != "" {
		manifest.CUDA = value
	}
	if value := strings.TrimSpace(metadata["rocm"]); value != "" {
		manifest.ROCm = value
	}
	return manifest, nil
}

func (p VLLMProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if manifest.Kind != "" && manifest.Kind != "vllm" {
		return fmt.Errorf("manifest kind %q is not vllm", manifest.Kind)
	}
	venvPath := filepath.Join(directory, ".venv")
	if err := rejectSymlinkPath(venvPath); err != nil {
		return fmt.Errorf("vLLM virtualenv path: %w", err)
	}
	info, err := os.Lstat(venvPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("vLLM runtime has no .venv")
	}
	if err := verifyRuntimeMetadata(directory, manifest); err != nil {
		return err
	}
	return verifyVLLMEntrypoints(directory)
}

// verifyVLLMEntrypoints checks the two native launch forms accepted by
// CurrentLaunchBinding. Python virtual environments commonly make the Python
// executable itself a symlink to the base interpreter, so inspect the bin
// directory without following a directory link but allow a normal leaf
// symlink. The vllm console script is installed by the managed package and
// must be executable before an activation is considered healthy.
func verifyVLLMEntrypoints(directory string) error {
	binDir := filepath.Join(directory, ".venv", "bin")
	if err := rejectSymlinkPath(binDir); err != nil {
		return fmt.Errorf("vLLM virtualenv bin path: %w", err)
	}
	for _, name := range []string{"python", "vllm"} {
		entrypoint := filepath.Join(binDir, name)
		info, err := os.Stat(entrypoint)
		if err != nil {
			return fmt.Errorf("vLLM entrypoint %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("vLLM entrypoint %s is not a regular file", name)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("vLLM entrypoint %s is not executable", name)
		}
	}
	return nil
}

func (p VLLMProvider) Health(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Verify(ctx, manifest, directory); err != nil {
		return err
	}
	if err := validateRuntimeHealthPath(manifest.Metadata["verify.healthPath"]); err != nil {
		return err
	}
	return runRuntimeSmokeCommand(ctx, p.Run, directory, manifest.Metadata["verify.smokeCommand"])
}

// probeVLLMEnvironment collects version metadata from the environment that
// was just installed. It deliberately executes a fixed Python snippet (no
// configured shell text) and treats every error as best-effort. Providers
// backed by a fake/remote uv command, or images that omit torch import support,
// can therefore still stage successfully while retaining the source/spec
// metadata written by Stage.
func probeVLLMEnvironment(ctx context.Context, run CommandRunner, directory, python string) map[string]string {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil || strings.TrimSpace(directory) == "" || strings.TrimSpace(python) == "" {
		return nil
	}
	const script = `import importlib.metadata as m, json, platform
def version(name):
    try:
        return m.version(name)
    except Exception:
        return ""
result = {"pythonVersion": platform.python_version(), "vllm": version("vllm"), "torch": version("torch")}
try:
    import torch
    result["cuda"] = torch.version.cuda or ""
    result["rocm"] = getattr(torch.version, "hip", "") or ""
except Exception:
    pass
print(json.dumps(result, sort_keys=True))`
	output, err := runRuntimeCommand(ctx, run, directory, python, "-c", script)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	if len(output) > maxVLLMProbeBytes {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil
	}
	result := make(map[string]string)
	for _, key := range []string{"pythonVersion", "vllm", "torch", "cuda", "rocm"} {
		value, ok := raw[key].(string)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// runtimeLockFingerprint identifies the complete install input without
// storing a secret. Stable sorting of extras/build arguments makes the value
// independent of map iteration order while preserving argument boundaries.
func runtimeLockFingerprint(spec Spec, packageSpec, python, indexURL string, extras []string, sourceChecksum string) string {
	type lockInput struct {
		Kind           string              `json:"kind"`
		Mode           string              `json:"mode"`
		Version        string              `json:"version"`
		SourceType     string              `json:"sourceType"`
		Source         string              `json:"source"`
		Ref            string              `json:"ref"`
		Commit         string              `json:"commit"`
		Checksum       string              `json:"checksum"`
		SourceChecksum string              `json:"sourceChecksum"`
		Package        string              `json:"package"`
		Python         string              `json:"python"`
		IndexURL       string              `json:"indexURL"`
		Extras         []string            `json:"extras,omitempty"`
		Build          map[string]string   `json:"build,omitempty"`
		BuildArgs      map[string][]string `json:"buildArgs,omitempty"`
		BuildEnv       map[string]string   `json:"buildEnv,omitempty"`
		BuildSteps     []BuildStep         `json:"buildSteps,omitempty"`
		InstallArgs    []string            `json:"installArgs,omitempty"`
		Artifacts      []Artifact          `json:"artifacts,omitempty"`
		Container      *ContainerLaunch    `json:"container,omitempty"`
		Metadata       map[string]string   `json:"metadata,omitempty"`
	}
	extrasCopy := append([]string(nil), extras...)
	// Preserve the order of explicit extras: uv treats that order as part of
	// the command input, and changing it would make a lock fingerprint claim
	// equivalence for a different install request.
	value := lockInput{
		Kind: spec.Kind, Mode: normalizeRuntimeMode(spec.Mode), Version: spec.Version, SourceType: spec.SourceType,
		Source: spec.Source, Ref: spec.Ref, Commit: spec.Commit,
		Checksum: spec.Checksum, SourceChecksum: sourceChecksum,
		Package: packageSpec, Python: python, IndexURL: indexURL,
		Extras: extrasCopy, Build: cloneMetadata(spec.Build), BuildArgs: cloneBuildArgs(spec.BuildArgs),
		BuildEnv: cloneMetadata(spec.BuildEnv), BuildSteps: cloneBuildSteps(spec.BuildSteps),
		InstallArgs: append([]string(nil), spec.InstallArgs...), Artifacts: cloneArtifacts(spec.Artifacts), Container: cloneContainerLaunch(spec.Container), Metadata: cloneMetadata(spec.Metadata),
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneBuildArgs(input map[string][]string) map[string][]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string][]string, len(input))
	for key, values := range input {
		if strings.TrimSpace(key) == "" {
			continue
		}
		output[key] = append([]string(nil), values...)
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

type LlamaCPPProvider struct {
	CMake             string
	Compiler          string
	Backend           string
	CUDAArchitectures []string
	SourceAllowlist   []string
	// UpdateURL overrides the GitHub releases endpoint used by
	// CheckForUpdate. When it is empty, a github.com source URL is mapped to
	// the corresponding api.github.com repository releases endpoint.
	UpdateURL        string
	Client           *http.Client
	MaxDownloadBytes int64
	Run              CommandRunner
	RunEnv           EnvCommandRunner
}

// CheckForUpdate discovers a newer llama.cpp release for release/channel
// sources. Git/tag/commit sources remain exact-ref declarations and are never
// advanced implicitly. Explicit update.version values always win, while an
// unavailable release endpoint is treated as a failed check rather than a
// reason to change the active runtime.
func (p LlamaCPPProvider) CheckForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(desired.Version) != "" {
		return desired, desiredDiff(current, desired), nil
	}
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferLlamaSourceType(desired.Source)
	}
	if sourceType == "git" && strings.TrimSpace(desired.Metadata["trackRef"]) != "" {
		return checkTrackedGitUpdate(ctx, p.Run, p.SourceAllowlist, current, desired)
	}
	if strings.TrimSpace(current.Version) == "" {
		return desired, false, nil
	}
	if sourceType != "release" && sourceType != "channel" {
		return desired, desiredDiff(current, desired), nil
	}
	endpoint := strings.TrimSpace(p.UpdateURL)
	if endpoint == "" {
		endpoint = githubReleaseEndpoint(desired.Source)
	}
	if endpoint == "" {
		return desired, false, nil
	}
	if !validRuntimeURL(endpoint, p.SourceAllowlist) {
		return desired, false, fmt.Errorf("llama.cpp update URL is not allowed")
	}
	client := guardedRuntimeClient(p.Client, p.SourceAllowlist, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return desired, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return desired, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return desired, false, fmt.Errorf("llama.cpp release metadata returned HTTP %d", resp.StatusCode)
	}
	var releases []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
		Pre     bool   `json:"prerelease"`
	}
	if err := decodeBoundedRuntimeJSON(resp.Body, maxRuntimeMetadataBytes, &releases); err != nil {
		return desired, false, fmt.Errorf("decode llama.cpp release metadata: %w", err)
	}
	latest := ""
	for _, release := range releases {
		version := strings.TrimSpace(release.TagName)
		if release.Draft || version == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(policy.Channel), "prerelease") {
			// A prerelease channel may consume both prereleases and stable
			// releases; selecting the highest version keeps it monotonic when a
			// stable release supersedes an RC.
		} else if release.Pre || !isRuntimeVersionCandidate(version, "stable") {
			continue
		}
		if latest == "" || compareRuntimeVersions(version, latest) > 0 {
			latest = version
		}
	}
	if latest == "" || compareRuntimeVersions(latest, current.Version) <= 0 {
		return desired, false, nil
	}
	candidate := desired
	candidate.Version = latest
	if candidate.SourceType == "" {
		candidate.SourceType = sourceType
	}
	if candidate.Source == "" {
		candidate.Source = desired.Source
	}
	// A release/channel definition commonly points at a GitHub repository or
	// at the previous tag's source archive.  Advancing only the manifest
	// version would make the next Stage download the old (or a literal
	// `current`) archive again.  Derive the tag-specific source URL while
	// retaining custom asset names and the configured allowlist boundary.
	if source, ok := llamaReleaseSource(desired.Source, latest); ok {
		candidate.Source = source
	}
	return candidate, desiredDiff(current, candidate), nil
}

// checkTrackedGitUpdate resolves an operator-selected branch/tag ref to one
// immutable commit without cloning the repository. It is safe on the busy
// update path because ls-remote transfers only Git reference metadata; the
// later Stage operation remains responsible for downloading/building source
// after the manager's idle admission gate.
func checkTrackedGitUpdate(ctx context.Context, run CommandRunner, allowlist []string, current Manifest, desired Spec) (Spec, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trackRef := strings.TrimSpace(desired.Metadata["trackRef"])
	if err := validateRuntimeRef("runtime tracked git ref", trackRef, true); err != nil {
		return desired, false, err
	}
	source := strings.TrimPrefix(strings.TrimSpace(desired.Source), "git+")
	if !validGitSource(source, allowlist) {
		return desired, false, errors.New("tracked git source is not allowed")
	}
	if run == nil {
		run = defaultCommandRunner
	}
	queryRef := trackRef
	if !strings.HasPrefix(queryRef, "refs/") {
		queryRef = "refs/heads/" + queryRef
	}
	output, err := runRuntimeCommand(ctx, run, "", "git", "ls-remote", "--exit-code", source, queryRef)
	if err != nil && queryRef == trackRef {
		return desired, false, fmt.Errorf("resolve tracked git ref: %w", err)
	}
	if err != nil {
		// A concise ref such as `v1.2.0` is commonly a tag. Try the explicit
		// tag namespace after the branch lookup, while keeping every argv item
		// shell-free and validated.
		queryRef = "refs/tags/" + trackRef
		output, err = runRuntimeCommand(ctx, run, "", "git", "ls-remote", "--exit-code", source, queryRef)
		if err != nil {
			return desired, false, fmt.Errorf("resolve tracked git ref: %w", err)
		}
	}
	if len(output) > 1<<20 {
		return desired, false, errors.New("tracked git ref response exceeds 1 MiB")
	}
	commit, err := parseTrackedGitCommit(output, queryRef)
	if err != nil {
		return desired, false, err
	}
	candidate := desired
	candidate.Source = source
	candidate.Ref = commit
	candidate.Commit = commit
	candidate.Version = "git-" + commit
	candidate.Metadata = cloneMetadata(desired.Metadata)
	if candidate.Metadata == nil {
		candidate.Metadata = make(map[string]string)
	}
	candidate.Metadata["trackRef"] = trackRef
	candidate.Metadata["resolvedCommit"] = commit

	currentCommit := strings.ToLower(strings.TrimSpace(current.Commit))
	if currentCommit == "" && validGitCommit(strings.TrimSpace(current.Ref)) {
		currentCommit = strings.ToLower(strings.TrimSpace(current.Ref))
	}
	if currentCommit == "" {
		currentCommit, _ = gitCommitFromVersion(current.Version)
	}
	return candidate, currentCommit != commit, nil
}

func parseTrackedGitCommit(output []byte, trackRef string) (string, error) {
	var commit string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != trackRef {
			continue
		}
		value := strings.ToLower(fields[0])
		if !validGitCommit(value) {
			return "", errors.New("tracked git ref returned an invalid commit hash")
		}
		if commit != "" && commit != value {
			return "", errors.New("tracked git ref resolved to multiple commits")
		}
		commit = value
	}
	if commit == "" {
		return "", fmt.Errorf("tracked git ref %q was not found", trackRef)
	}
	return commit, nil
}

func validGitCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func githubReleaseEndpoint(source string) string {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" || strings.ContainsAny(owner+repo, "\\\x00") {
		return ""
	}
	return "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases"
}

// llamaReleaseSource resolves a GitHub source/archive declaration to the
// artifact for an exact release tag.  It intentionally handles only the
// canonical GitHub URL shapes we can rewrite without guessing an asset:
// repository roots, /archive/refs/tags/<tag>.tar.gz and
// /releases/download/<tag>/<asset>.  Non-GitHub URLs and unknown path shapes
// are returned unchanged so operators retain complete control over custom
// mirrors and release assets.
func llamaReleaseSource(source, version string) (string, bool) {
	source = strings.TrimSpace(source)
	rawVersion := version
	version = strings.TrimSpace(version)
	if source == "" || version == "" || rawVersion != version || !isRuntimeVersionCandidate(version, "prerelease") {
		return "", false
	}
	u, err := url.Parse(source)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	// A .git suffix is valid for repository URLs but not part of the GitHub
	// archive path. Keep the original repository spelling otherwise.
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	if parts[1] == "" {
		return "", false
	}
	escapedVersion := url.PathEscape(version)
	switch {
	case len(parts) == 2:
		parts = append(parts, "archive", "refs", "tags", escapedVersion+".tar.gz")
	case len(parts) >= 6 && parts[2] == "archive" && parts[3] == "refs" && parts[4] == "tags":
		parts = append(parts[:5], escapedVersion+archiveSuffix(parts[len(parts)-1]))
	case len(parts) >= 6 && parts[2] == "releases" && parts[3] == "download":
		// Preserve the configured asset name (for example a source bundle or a
		// platform-specific archive) and replace only its tag component.
		parts[4] = escapedVersion
	default:
		return "", false
	}
	u.Path = "/" + strings.Join(parts, "/")
	return u.String(), true
}

func archiveSuffix(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar", ".zip"} {
		if strings.HasSuffix(lower, suffix) {
			return name[len(name)-len(suffix):]
		}
	}
	// GitHub's source archives are tarballs by convention. A malformed or
	// extensionless path is still rewritten deterministically instead of
	// appending a second extension to an operator-supplied asset.
	return ""
}

func decodeBoundedRuntimeJSON(reader io.Reader, maxBytes int64, destination any) error {
	if reader == nil {
		return errors.New("runtime metadata body is nil")
	}
	if maxBytes <= 0 {
		return errors.New("runtime metadata limit must be positive")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read runtime metadata: %w", err)
	}
	if int64(len(payload)) > maxBytes {
		return fmt.Errorf("runtime metadata exceeds %d bytes", maxBytes)
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return err
	}
	return nil
}

func (p LlamaCPPProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime.GOOS != "linux" {
		return Manifest{}, errors.New("llama.cpp managed runtimes are supported on Linux only")
	}
	if err := validateProviderStage(spec, "llamacpp"); err != nil {
		return Manifest{}, err
	}
	if err := prepareProviderDestination(destination); err != nil {
		return Manifest{}, fmt.Errorf("llama.cpp stage destination: %w", err)
	}
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	runEnv := p.RunEnv
	if runEnv == nil && p.Run == nil {
		runEnv = defaultCommandRunnerWithEnv
	}
	source := strings.TrimSpace(spec.Source)
	if source == "" {
		return Manifest{}, errors.New("llama.cpp source is required")
	}
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType == "" {
		sourceType = inferLlamaSourceType(source)
	}
	if sourceType == "release" || sourceType == "channel" {
		if resolved, ok := llamaReleaseSource(source, spec.Version); ok {
			source = resolved
		}
	}
	localSource, localSourceOK := runtimeSourcePath(source)
	// An explicit Git source must go through git clone/checkout even when it is
	// expressed as a local file:// URL. Treating the bare repository directory
	// as a generic local tree would skip ref verification and would not test the
	// same immutable source path used by remote repositories.
	if sourceType == "git" || sourceType == "tag" || sourceType == "commit" {
		localSourceOK = false
	}
	var sourceChecksum string
	var sourceRoot string
	if localSourceOK {
		if err := rejectSymlinkPath(localSource); err != nil {
			return Manifest{}, fmt.Errorf("llama.cpp source path: %w", err)
		}
		if pathInfo, err := os.Stat(localSource); err == nil && pathInfo.IsDir() {
			if !pathAllowed(localSource, p.SourceAllowlist) {
				return Manifest{}, errors.New("llama.cpp source path is not allowed")
			}
			var checksumErr error
			sourceChecksum, checksumErr = directoryChecksum(localSource)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash llama.cpp source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("llama.cpp source checksum: %w", err)
			}
			sourceRoot = filepath.Join(destination, "source")
			if err := os.MkdirAll(sourceRoot, 0o750); err != nil {
				return Manifest{}, fmt.Errorf("create llama.cpp source directory: %w", err)
			}
			if err := copyTree(ctx, localSource, sourceRoot); err != nil {
				return Manifest{}, err
			}
		} else if sourceType == "local" || sourceType == "bundled" {
			return Manifest{}, fmt.Errorf("llama.cpp source path %q is not a directory", localSource)
		} else {
			localSourceOK = false
		}
	}
	if !localSourceOK {
		if sourceType == "release" || sourceType == "channel" {
			if !validRuntimeURL(source, p.SourceAllowlist) {
				return Manifest{}, errors.New("llama.cpp release source URL is not allowed")
			}
			reportRuntimeProgress(ctx, "downloading", 0.15, "downloading llama.cpp release")
			artifact, checksum, downloadErr := downloadRuntimeArtifactWithProgress(ctx, p.Client, source, destination, p.MaxDownloadBytes, p.SourceAllowlist, func(completed, total int64) {
				progress := progressFraction(completed, total, 0.15, 0.35)
				reportRuntimeProgressBytes(ctx, "downloading", progress, completed, total, fmt.Sprintf("downloaded %s", formatProgressBytes(completed, total)))
			})
			if downloadErr != nil {
				return Manifest{}, fmt.Errorf("download llama.cpp release: %w", downloadErr)
			}
			sourceChecksum = checksum
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("llama.cpp release checksum: %w", err)
			}
			sourceRoot = filepath.Join(destination, "source")
			reportRuntimeProgress(ctx, "staging", 0.4, "extracting llama.cpp release")
			if err := extractRuntimeArchive(ctx, artifact, sourceRoot, p.MaxDownloadBytes); err != nil {
				return Manifest{}, fmt.Errorf("extract llama.cpp release: %w", err)
			}
			sourceRoot = collapseArchiveRoot(sourceRoot)
		} else if sourceType != "git" && sourceType != "tag" && sourceType != "commit" {
			return Manifest{}, errors.New("llama.cpp requires a local directory or an allowlisted git source")
		}
		if sourceRoot == "" {
			gitSource := strings.TrimPrefix(source, "git+")
			if !validGitSource(gitSource, p.SourceAllowlist) || spec.Ref == "" {
				return Manifest{}, errors.New("llama.cpp requires an allowlisted git source and exact ref")
			}
			if _, err := runRuntimeCommand(ctx, run, destination, "git", "clone", "--no-checkout", gitSource, filepath.Join(destination, "source")); err != nil {
				return Manifest{}, fmt.Errorf("clone llama.cpp: %w", err)
			}
			if _, err := runRuntimeCommand(ctx, run, filepath.Join(destination, "source"), "git", "checkout", "--detach", spec.Ref); err != nil {
				return Manifest{}, fmt.Errorf("checkout llama.cpp ref: %w", err)
			}
			sourceRoot = filepath.Join(destination, "source")
		}
	}
	if sourceRoot == "" {
		return Manifest{}, errors.New("llama.cpp source is empty")
	}
	reportRuntimeProgress(ctx, "building", 0.48, "configuring llama.cpp")
	buildDir := filepath.Join(destination, "build")
	backendName := strings.TrimSpace(p.Backend)
	if configured := firstBuildArg(spec, "backend"); configured != "" {
		backendName = configured
	}
	compiler := strings.TrimSpace(p.Compiler)
	if configured := firstBuildArg(spec, "compiler"); configured != "" {
		compiler = configured
	}
	if err := rejectSymlinkPath(buildDir); err != nil {
		return Manifest{}, fmt.Errorf("llama.cpp build path: %w", err)
	}
	if err := os.MkdirAll(buildDir, 0o750); err != nil {
		return Manifest{}, fmt.Errorf("create llama.cpp build directory: %w", err)
	}
	buildVars := map[string]string{"RUNTIME_DIR": destination, "SOURCE_DIR": sourceRoot, "BUILD_DIR": buildDir}
	driver := strings.ToLower(strings.TrimSpace(spec.Build["driver"]))
	if configured := firstBuildArg(spec, "driver"); configured != "" {
		driver = strings.ToLower(configured)
	}
	if driver == "" {
		driver = "cmake"
	}
	architectures := append([]string(nil), p.CUDAArchitectures...)
	switch {
	case driver == "none":
		// Prebuilt release archives ship a ready-to-run binary tree; there
		// is nothing to compile and materialization copies the entrypoint
		// (plus its shared libraries) straight from the source layout.
	case driver == "custom" || len(spec.BuildSteps) > 0:
		if len(spec.BuildSteps) == 0 {
			return Manifest{}, errors.New("llama.cpp custom build requires at least one build step")
		}
		if err := runStructuredBuildSteps(ctx, destination, sourceRoot, spec.BuildSteps, spec.BuildEnv, buildVars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	case driver == "make":
		makeCommand := strings.TrimSpace(spec.Build["make"])
		if makeCommand == "" {
			makeCommand = "make"
		}
		makeArgs := buildArgs(spec, "make")
		if len(makeArgs) == 0 {
			makeArgs = buildArgs(spec, "args")
		}
		if len(makeArgs) == 0 && strings.TrimSpace(spec.Build["args"]) != "" {
			makeArgs = strings.Fields(spec.Build["args"])
		}
		if err := validateRuntimeCommandToken(makeCommand); err != nil {
			return Manifest{}, err
		}
		makeDir := sourceRoot
		if configured := strings.TrimSpace(spec.Build["workDir"]); configured != "" {
			resolved, resolveErr := resolveRuntimeWorkDir(destination, expandRuntimeMacro(configured, buildVars))
			if resolveErr != nil {
				return Manifest{}, fmt.Errorf("llama.cpp make workDir: %w", resolveErr)
			}
			makeDir = resolved
		}
		buildEnv := cloneMetadata(spec.BuildEnv)
		if compiler != "" {
			if _, exists := buildEnv["CC"]; !exists {
				buildEnv["CC"] = compiler
			}
		}
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, buildEnv, makeDir, makeCommand, makeArgs...); err != nil {
			return Manifest{}, fmt.Errorf("build llama.cpp with make: %w", err)
		}
	case driver == "cmake":
		cmake := p.CMake
		if cmake == "" {
			cmake = "cmake"
		}
		// CMAKE_BUILD_RPATH_USE_ORIGIN rewrites the build-tree RPATH to be
		// $ORIGIN-relative. CMake otherwise bakes absolute staging paths into
		// llama-server's RUNPATH, and the staging directory is renamed into
		// versions/<v>/ on success, leaving the binary unable to find its
		// own shared libraries after Stage/Activate.
		args := []string{"-S", sourceRoot, "-B", buildDir, "-DGGML_NATIVE=OFF", "-DCMAKE_BUILD_RPATH_USE_ORIGIN=ON"}
		if backendName != "" {
			args = append(args, "-DGGML_"+strings.ToUpper(backendName)+"=ON")
		}
		if configured := buildArgs(spec, "cudaArchitectures"); len(configured) > 0 {
			architectures = append([]string(nil), configured...)
		} else if configured := strings.TrimSpace(spec.Build["cudaArchitectures"]); configured != "" {
			architectures = strings.Split(configured, ",")
		}
		if len(architectures) > 0 {
			architectures = cleanBuildArgs(architectures)
			sort.Strings(architectures)
			if len(architectures) > 0 {
				args = append(args, "-DCMAKE_CUDA_ARCHITECTURES="+strings.Join(architectures, ";"))
			}
		}
		if configured := buildArgs(spec, "cmake"); len(configured) > 0 {
			args = append(args, configured...)
		} else if configured := strings.TrimSpace(spec.Build["cmake"]); configured != "" {
			args = append(args, strings.Fields(configured)...)
		}
		if configured := buildArgs(spec, "args"); len(configured) > 0 {
			args = append(args, configured...)
		} else if configured := strings.TrimSpace(spec.Build["args"]); configured != "" {
			args = append(args, strings.Fields(configured)...)
		}
		if compiler != "" {
			args = append(args, "-DCMAKE_C_COMPILER="+compiler)
		}
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, cmake, args...); err != nil {
			return Manifest{}, fmt.Errorf("configure llama.cpp: %w", err)
		}
		reportRuntimeProgress(ctx, "building", 0.68, "compiling llama.cpp")
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, cmake, "--build", buildDir, "--target", "llama-server"); err != nil {
			return Manifest{}, fmt.Errorf("build llama.cpp: %w", err)
		}
	default:
		return Manifest{}, fmt.Errorf("unsupported llama.cpp build driver %q", driver)
	}
	// The process launcher intentionally binds managed llama.cpp models to the
	// stable current/bin/llama-server path. Keep that executable outside the
	// build tree so callers never need to know which build driver or CMake
	// layout produced it. This also prevents a successful Stage/Activate from
	// becoming a delayed model-load failure because only build/bin exists.
	if err := materializeLlamaCPPEntrypoint(ctx, destination); err != nil {
		return Manifest{}, err
	}
	if err := copyRuntimeArtifacts(ctx, destination, spec.Artifacts, buildVars); err != nil {
		return Manifest{}, err
	}
	metadata := cloneMetadata(spec.Metadata)
	metadata["mode"] = normalizeRuntimeMode(spec.Mode)
	metadata["sourceType"] = sourceType
	metadata["driver"] = driver
	if len(spec.BuildEnv) > 0 {
		metadata["buildEnvKeys"] = strings.Join(sortedMapKeys(spec.BuildEnv), ",")
	}
	cmake := p.CMake
	if cmake == "" {
		cmake = "cmake"
	}
	metadata["cmake"] = cmake
	metadata["backend"] = backendName
	metadata["compiler"] = compiler
	metadata["runtime"] = runtime.GOOS + "/" + runtime.GOARCH
	metadata["builtAt"] = time.Now().UTC().Format(time.RFC3339)
	if sourceChecksum != "" {
		metadata["sourceChecksum"] = sourceChecksum
	}
	if len(architectures) > 0 {
		metadata["cudaArchitectures"] = strings.Join(architectures, ",")
	}
	if spec.Checksum != "" {
		metadata["checksum"] = normalizeChecksum(sourceChecksum)
		if metadata["checksum"] == "" {
			metadata["checksum"] = normalizeChecksum(spec.Checksum)
		}
	}
	if err := writeRuntimeMetadata(destination, metadata); err != nil {
		return Manifest{}, err
	}
	reportRuntimeProgress(ctx, "verifying", 0.86, "recording llama.cpp build metadata")
	manifest := Manifest{Name: spec.Name, Version: spec.Version, Kind: "llamacpp", Source: spec.Source, Ref: spec.Ref, Commit: spec.Commit, Checksum: spec.Checksum, Fingerprint: fingerprintMetadata(metadata), Metadata: metadata, InstalledAt: time.Now().UTC()}
	if value := strings.TrimSpace(metadata["compiler"]); value != "" {
		manifest.Metadata["compiler"] = value
	}
	return manifest, nil
}

// validateProviderStage keeps the provider boundary safe for embedders that
// call Provider.Stage directly instead of going through Manager.Stage. The
// manager already validates the runtime name/version and provider lookup, but
// a direct caller must not be able to make a vLLM provider emit a llama.cpp
// manifest (or vice versa), nor turn a malformed name/version into a path or
// map-key collision later in the manager.
func validateProviderStage(spec Spec, expectedKind string) error {
	mode := normalizeRuntimeMode(spec.Mode)
	if mode != RuntimeModeNative && mode != RuntimeModeContainer {
		return fmt.Errorf("%s provider runtime mode %q is unsupported", expectedKind, spec.Mode)
	}
	if kind := strings.TrimSpace(spec.Kind); kind != "" && (kind != spec.Kind || !strings.EqualFold(kind, expectedKind)) {
		return fmt.Errorf("%s provider cannot stage runtime kind %q", expectedKind, spec.Kind)
	}
	if spec.Name != "" {
		if err := validateName(spec.Name); err != nil {
			return err
		}
	}
	if spec.Version != "" {
		if err := validateVersion(spec.Version); err != nil {
			return err
		}
	}
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if rawSourceType := spec.SourceType; rawSourceType != "" {
		if strings.TrimSpace(rawSourceType) != rawSourceType {
			return fmt.Errorf("%s provider source type %q is not normalized", expectedKind, rawSourceType)
		}
		allowed := map[string]struct{}{}
		switch expectedKind {
		case "vllm":
			allowed = map[string]struct{}{"pypi": {}, "bundled": {}, "git": {}, "wheel": {}, "release": {}, "local": {}, "image": {}}
		case "llamacpp":
			allowed = map[string]struct{}{"bundled": {}, "release": {}, "channel": {}, "git": {}, "tag": {}, "commit": {}, "local": {}, "image": {}}
		case "lmcache":
			allowed = map[string]struct{}{"pypi": {}, "git": {}, "wheel": {}, "release": {}, "local": {}}
		}
		if _, ok := allowed[sourceType]; !ok {
			return fmt.Errorf("%s provider source type %q is unsupported", expectedKind, rawSourceType)
		}
	}
	if err := validateRuntimeSpecReferences(spec, expectedKind); err != nil {
		return err
	}
	if err := validateStructuredBuildInputs(spec, expectedKind); err != nil {
		return err
	}
	containerImage := strings.TrimSpace(spec.Source)
	if containerImage == "" {
		containerImage = strings.TrimSpace(spec.Metadata["image"])
	}
	if containerImage == "" && spec.Container != nil {
		containerImage = strings.TrimSpace(spec.Container.Image)
	}
	if mode == RuntimeModeContainer && sourceType != "image" && containerImage == "" {
		return fmt.Errorf("%s container runtime requires an image source", expectedKind)
	}
	if mode == RuntimeModeContainer && containerImage != "" {
		if err := validateContainerImage(containerImage, nil); err != nil {
			return fmt.Errorf("%s container image: %w", expectedKind, err)
		}
		if spec.Container != nil {
			if _, err := spec.Container.RunArgs(containerImage); err != nil {
				return fmt.Errorf("%s container launch: %w", expectedKind, err)
			}
		}
	}
	return nil
}

// validateStructuredBuildInputs mirrors the configuration-layer limits at the
// Provider.Stage boundary. Providers are public Go interfaces and can be
// called directly by embedders, so malformed argv/env/path values must not be
// able to bypass the shell-free build contract simply by skipping YAML load.
func validateStructuredBuildInputs(spec Spec, kind string) error {
	if len(spec.Build) > 256 {
		return fmt.Errorf("%s build has too many entries", kind)
	}
	for key, value := range spec.Build {
		if err := validateRuntimeMapKey(key, "build"); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if err := validateRuntimeCommandToken(value); err != nil {
			return fmt.Errorf("%s build.%s: %w", kind, key, err)
		}
	}
	if len(spec.InstallArgs) > 512 {
		return fmt.Errorf("%s installArgs has too many arguments", kind)
	}
	for index, value := range spec.InstallArgs {
		if err := validateRuntimeCommandToken(value); err != nil {
			return fmt.Errorf("%s installArgs[%d]: %w", kind, index, err)
		}
	}
	if len(spec.BuildArgs) > 256 {
		return fmt.Errorf("%s buildArgs has too many entries", kind)
	}
	for key, values := range spec.BuildArgs {
		if err := validateRuntimeMapKey(key, "buildArgs"); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if len(values) > 512 {
			return fmt.Errorf("%s buildArgs.%s has too many arguments", kind, key)
		}
		for index, value := range values {
			if err := validateRuntimeCommandToken(value); err != nil {
				return fmt.Errorf("%s buildArgs.%s[%d]: %w", kind, key, index, err)
			}
		}
	}
	if len(spec.BuildEnv) > 256 {
		return fmt.Errorf("%s buildEnv has too many entries", kind)
	}
	for key, value := range spec.BuildEnv {
		if !validRuntimeEnvName(key) {
			return fmt.Errorf("%s buildEnv has invalid environment key %q", kind, key)
		}
		if err := validateRuntimeCommandToken(value); err != nil {
			return fmt.Errorf("%s buildEnv.%s: %w", kind, key, err)
		}
	}
	if len(spec.BuildSteps) > 128 {
		return fmt.Errorf("%s buildSteps has too many entries", kind)
	}
	for index, step := range spec.BuildSteps {
		if strings.TrimSpace(step.Command) == "" || strings.TrimSpace(step.Command) != step.Command {
			return fmt.Errorf("%s buildSteps[%d].command is required and must be normalized", kind, index)
		}
		if err := validateRuntimeCommandToken(step.Command); err != nil {
			return fmt.Errorf("%s buildSteps[%d].command: %w", kind, index, err)
		}
		if step.WorkDir != "" {
			if err := validateRuntimeRelativeInput(step.WorkDir, fmt.Sprintf("%s buildSteps[%d].workDir", kind, index)); err != nil {
				return fmt.Errorf("%s buildSteps[%d].workDir: %w", kind, index, err)
			}
		}
		if len(step.Args) > 512 {
			return fmt.Errorf("%s buildSteps[%d].args has too many arguments", kind, index)
		}
		for argIndex, value := range step.Args {
			if err := validateRuntimeCommandToken(value); err != nil {
				return fmt.Errorf("%s buildSteps[%d].args[%d]: %w", kind, index, argIndex, err)
			}
		}
		if len(step.Env) > 256 {
			return fmt.Errorf("%s buildSteps[%d].env has too many entries", kind, index)
		}
		for key, value := range step.Env {
			if !validRuntimeEnvName(key) {
				return fmt.Errorf("%s buildSteps[%d].env has invalid environment key %q", kind, index, key)
			}
			if err := validateRuntimeCommandToken(value); err != nil {
				return fmt.Errorf("%s buildSteps[%d].env.%s: %w", kind, index, key, err)
			}
		}
	}
	if len(spec.Artifacts) > 128 {
		return fmt.Errorf("%s artifacts has too many entries", kind)
	}
	for index, artifact := range spec.Artifacts {
		for field, value := range map[string]string{"from": artifact.From, "to": artifact.To} {
			if err := validateRuntimeRelativeInput(value, fmt.Sprintf("%s artifacts[%d].%s", kind, index, field)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRuntimeMapKey(key, field string) error {
	if key == "" || len(key) > 256 || strings.TrimSpace(key) != key {
		return fmt.Errorf("%s key %q is not normalized", field, key)
	}
	for _, r := range key {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%s key contains control characters", field)
		}
	}
	return nil
}

func validRuntimeEnvName(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func validateRuntimeRelativeInput(value, field string) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') || filepath.IsAbs(value) {
		return fmt.Errorf("%s must be a non-empty relative path without parent segments", field)
	}
	if err := validateRuntimeCommandToken(value); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	for _, component := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == ".." {
			return fmt.Errorf("%s must not contain parent path segments", field)
		}
	}
	if clean := filepath.Clean(filepath.FromSlash(value)); clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must not escape the runtime directory", field)
	}
	return nil
}

// validateRuntimeSpecReferences is shared by Manager.Configure and the
// provider entry points. Configuration should fail before a malformed ref is
// persisted, while direct Provider.Stage calls must enforce the same contract
// even when they bypass the manager. Unknown/custom provider kinds still get
// the generic ref checks; git-like source types always require an exact ref.
func validateRuntimeSpecReferences(spec Spec, expectedKind string) error {
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType == "" {
		switch strings.ToLower(strings.TrimSpace(expectedKind)) {
		case "vllm":
			sourceType = inferVLLMSourceType(spec.Source)
		case "llamacpp":
			sourceType = inferLlamaSourceType(spec.Source)
		}
	}
	refRequired := sourceType == "git" || sourceType == "tag" || sourceType == "commit"
	if err := validateRuntimeRef(fmt.Sprintf("%s runtime ref", strings.TrimSpace(expectedKind)), spec.Ref, refRequired); err != nil {
		return err
	}
	if err := validateRuntimeRef(fmt.Sprintf("%s runtime commit", strings.TrimSpace(expectedKind)), spec.Commit, false); err != nil {
		return err
	}
	return nil
}

// validateRuntimeManifestReferences applies the same exact-ref contract to a
// persisted manifest while retaining compatibility with runtimes staged by
// older versions. The old Git manifest format sometimes recorded only the
// immutable commit in the version name (git-<commit>) or in Commit, not Ref.
// Those values are already immutable identities, so using them as the
// validation ref does not make a mutable branch/tag acceptable. New config
// and Stage inputs continue to use validateRuntimeSpecReferences directly and
// therefore still require an explicit Ref for Git-like sources.
func validateRuntimeManifestReferences(manifest Manifest, sourceType, source string) error {
	if strings.TrimSpace(source) == "" {
		source = manifest.Source
	}
	if strings.TrimSpace(sourceType) == "" {
		sourceType = ""
		switch strings.ToLower(strings.TrimSpace(manifest.Kind)) {
		case "vllm":
			sourceType = inferVLLMSourceType(source)
		case "llamacpp":
			sourceType = inferLlamaSourceType(source)
		}
	}

	ref := manifest.Ref
	if ref == "" && validGitCommit(manifest.Commit) {
		ref = manifest.Commit
	} else if ref == "" && manifest.Commit == "" {
		if commit, ok := gitCommitFromVersion(manifest.Version); ok {
			ref = commit
		}
	}
	return validateRuntimeSpecReferences(Spec{
		Kind:       manifest.Kind,
		SourceType: sourceType,
		Source:     source,
		Ref:        ref,
		Commit:     manifest.Commit,
	}, manifest.Kind)
}

func gitCommitFromVersion(version string) (string, bool) {
	if strings.TrimSpace(version) != version || !strings.HasPrefix(version, "git-") {
		return "", false
	}
	commit := strings.TrimPrefix(version, "git-")
	if !validGitCommit(commit) {
		return "", false
	}
	return strings.ToLower(commit), true
}

// validateRuntimeRef mirrors the restrictions enforced by git
// check-ref-format for the subset of refs accepted by managed providers. The
// value is eventually passed as a single argv item, but git/uv still parse
// option-like and ref-special strings themselves. Rejecting malformed refs at
// the provider boundary prevents option injection, ambiguous package specs,
// and refs that could escape the intended immutable version identity.
func validateRuntimeRef(field, ref string, required bool) error {
	if ref == "" {
		if required {
			return fmt.Errorf("%s requires an exact ref", field)
		}
		return nil
	}
	if strings.TrimSpace(ref) != ref {
		return fmt.Errorf("%s %q is not normalized", field, ref)
	}
	if len(ref) > 1024 {
		return fmt.Errorf("%s exceeds 1024 bytes", field)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("%s must not begin with '-'", field)
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return fmt.Errorf("%s %q is not a valid git ref", field, ref)
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
		return fmt.Errorf("%s %q is not a valid git ref", field, ref)
	}
	if strings.ContainsAny(ref, "~^:?*[\\") {
		return fmt.Errorf("%s %q is not a valid git ref", field, ref)
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || component == "." || component == ".." || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") {
			return fmt.Errorf("%s %q is not a valid git ref", field, ref)
		}
		for _, r := range component {
			if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return fmt.Errorf("%s contains whitespace or control characters", field)
			}
		}
	}
	return nil
}

// prepareProviderDestination validates and creates the candidate directory
// before any provider command runs. Manager.Stage supplies an already-created
// temporary directory, but the public Provider interface is also used by
// embedders and tests. Without this check a direct call could pass a symlinked
// destination and have uv/cmake write outside the manager-owned runtime root.
// Missing components are created only after rejectSymlinkPath has checked all
// existing parents; the final Lstat closes the common "destination is a file
// or link" mistake before a command is launched.
func prepareProviderDestination(destination string) error {
	if destination == "" {
		return errors.New("destination is required")
	}
	if strings.ContainsRune(destination, '\x00') || !filepath.IsAbs(destination) {
		return errors.New("destination must be an absolute path")
	}
	if err := rejectSymlinkPath(destination); err != nil {
		return err
	}
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("destination must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("destination must be a real directory")
	}
	return nil
}

func (p LlamaCPPProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	buildPath := filepath.Join(directory, "build")
	if err := rejectSymlinkPath(buildPath); err != nil {
		return fmt.Errorf("llama.cpp build path: %w", err)
	}
	if info, err := os.Lstat(buildPath); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		if err != nil {
			return err
		}
		return errors.New("llama.cpp runtime build path is not a real directory")
	}
	if err := verifyRuntimeMetadata(directory, manifest); err != nil {
		return err
	}
	return verifyLlamaCPPEntrypoint(directory)
}

// materializeLlamaCPPEntrypoint normalizes the output of the supported build
// drivers to the runtime layout consumed by CurrentLaunchBinding. Recent
// CMake builds use build/bin/llama-server, while legacy/custom builds can put
// the same binary under source. A custom builder may already have created the
// normalized path; in that case it is validated rather than overwritten.
func materializeLlamaCPPEntrypoint(ctx context.Context, directory string) error {
	target := filepath.Join(directory, "bin", "llama-server")
	if err := rejectSymlinkPath(target); err != nil {
		return fmt.Errorf("llama.cpp entrypoint path: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		return verifyLlamaCPPEntrypoint(directory)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat llama.cpp entrypoint: %w", err)
	}

	candidates := []string{
		"build/bin/llama-server",
		"source/build/bin/llama-server",
		"source/llama-server",
	}
	// Release archives (llama-bXXXX-bin-*.tar.gz) ship the prebuilt binary
	// under a versioned top-level directory. Accept exactly one such layout
	// so a stock release archive needs no custom build steps at all.
	if matches, globErr := filepath.Glob(filepath.Join(directory, "source", "*", "llama-server")); globErr == nil && len(matches) == 1 {
		if rel, relErr := filepath.Rel(directory, matches[0]); relErr == nil {
			candidates = append(candidates, filepath.ToSlash(rel))
		}
	}
	for _, source := range candidates {
		candidate := filepath.Join(directory, filepath.FromSlash(source))
		if err := rejectSymlinkPath(candidate); err != nil {
			return fmt.Errorf("llama.cpp entrypoint source: %w", err)
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat llama.cpp entrypoint source: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("llama.cpp entrypoint source is not a regular file")
		}
		if info.Mode().Perm()&0o111 == 0 {
			return errors.New("llama.cpp entrypoint source is not executable")
		}
		if err := copyRuntimeArtifacts(ctx, directory, []Artifact{{From: source, To: "bin/llama-server"}}, nil); err != nil {
			return fmt.Errorf("materialize llama.cpp entrypoint: %w", err)
		}
		if err := copyLlamaCPPSharedLibraries(ctx, directory, filepath.Dir(candidate)); err != nil {
			return fmt.Errorf("materialize llama.cpp libraries: %w", err)
		}
		return verifyLlamaCPPEntrypoint(directory)
	}
	return errors.New("llama.cpp build did not produce llama-server")
}

// copyLlamaCPPSharedLibraries brings the shared libraries produced next to the
// llama-server build output into bin/ so the materialized entrypoint stays
// self-contained: llama.cpp links llama-server against libllama/libggml
// objects through RUNPATH $ORIGIN, and both CMake builds and release archives
// ship them as symlink chains that must appear under every link name.
func copyLlamaCPPSharedLibraries(ctx context.Context, directory, sourceDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if rel, err := filepath.Rel(directory, sourceDir); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("llama.cpp library source %q escapes the runtime directory", sourceDir)
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !strings.Contains(name, ".so") && !strings.Contains(name, ".dylib") {
			continue
		}
		source := filepath.Join(sourceDir, name)
		// os.Stat follows the symlink chain and skips dangling links so every
		// link name lands in bin/ as a regular file the loader can find.
		info, statErr := os.Stat(source)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat llama.cpp library %s: %w", name, statErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		target := filepath.Join(directory, "bin", name)
		if err := rejectSymlinkPath(target); err != nil {
			return fmt.Errorf("llama.cpp library target: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("create llama.cpp library directory: %w", err)
		}
		content, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("open llama.cpp library %s: %w", name, err)
		}
		perm := info.Mode().Perm()
		if perm&0o444 == 0 {
			perm = 0o640
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			content.Close()
			return fmt.Errorf("create llama.cpp library %s: %w", name, err)
		}
		if _, err := io.Copy(out, content); err != nil {
			content.Close()
			out.Close()
			return fmt.Errorf("copy llama.cpp library %s: %w", name, err)
		}
		if err := content.Close(); err != nil {
			out.Close()
			return fmt.Errorf("close llama.cpp library %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close llama.cpp library %s: %w", name, err)
		}
	}
	return nil
}

func verifyLlamaCPPEntrypoint(directory string) error {
	entrypoint := filepath.Join(directory, "bin", "llama-server")
	if err := rejectSymlinkPath(entrypoint); err != nil {
		return fmt.Errorf("llama.cpp entrypoint path: %w", err)
	}
	info, err := os.Lstat(entrypoint)
	if err != nil {
		return fmt.Errorf("llama.cpp entrypoint: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("llama.cpp entrypoint is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("llama.cpp entrypoint is not executable")
	}
	return nil
}

func (p LlamaCPPProvider) Health(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Verify(ctx, manifest, directory); err != nil {
		return err
	}
	if err := validateRuntimeHealthPath(manifest.Metadata["verify.healthPath"]); err != nil {
		return err
	}
	return runRuntimeSmokeCommand(ctx, p.Run, directory, manifest.Metadata["verify.smokeCommand"])
}

// validateRuntimeHealthPath validates the optional HTTP path retained in a
// runtime manifest. The provider does not make a network request here—the
// runtime manager has no backend listener to probe—but rejecting absolute URLs,
// query/fragment injection and invisible characters keeps this value safe for
// a later adapter health probe and makes malformed manifests fail closed.
func validateRuntimeHealthPath(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\r\n") {
		return errors.New("runtime verify health path is invalid")
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("runtime verify health path is invalid")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, "/") || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("runtime verify health path must be an absolute path without query or fragment")
	}
	return nil
}

// parseRuntimeSmokeCommand implements a deliberately small shell-free argv
// grammar for the optional verify.smokeCommand setting. Whitespace separates
// arguments; single/double quotes preserve spaces and backslash escapes the
// next non-control character. Metacharacters are ordinary argument bytes and
// therefore cannot be interpreted as pipes, redirects or command substitution.
func parseRuntimeSmokeCommand(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > maxRuntimeSmokeCommandBytes || strings.ContainsRune(raw, '\x00') {
		return nil, errors.New("runtime smoke command is too long or contains NUL")
	}
	var args []string
	var token strings.Builder
	inToken := false
	var quote rune
	escaped := false
	flush := func() {
		if inToken {
			args = append(args, token.String())
			token.Reset()
			inToken = false
		}
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return nil, errors.New("runtime smoke command contains control characters")
		}
		if escaped {
			token.WriteRune(r)
			inToken = true
			escaped = false
			continue
		}
		if quote != 0 {
			switch r {
			case quote:
				quote = 0
				inToken = true
			case '\\':
				if quote == '"' {
					escaped = true
				} else {
					token.WriteRune(r)
				}
			default:
				token.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			inToken = true
		case '\\':
			escaped = true
			inToken = true
		default:
			if unicode.IsSpace(r) {
				flush()
				continue
			}
			token.WriteRune(r)
			inToken = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("runtime smoke command has an unterminated quote or escape")
	}
	flush()
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return nil, errors.New("runtime smoke command is empty")
	}
	return args, nil
}

func runRuntimeSmokeCommand(ctx context.Context, run CommandRunner, directory, raw string) error {
	args, err := parseRuntimeSmokeCommand(raw)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return nil
	}
	if run == nil {
		run = defaultCommandRunner
	}
	if _, err := runRuntimeCommand(ctx, run, directory, args[0], args[1:]...); err != nil {
		return fmt.Errorf("runtime smoke command: %w", err)
	}
	return nil
}

func writeRuntimeMetadata(directory string, metadata map[string]string) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(data)) > maxRuntimeManifestBytes {
		return fmt.Errorf("runtime metadata exceeds %d bytes", maxRuntimeManifestBytes)
	}
	return writeAtomicFile(filepath.Join(directory, "metadata.json"), data, 0o640)
}

func verifyRuntimeMetadata(directory string, manifest Manifest) error {
	metadataPath := filepath.Join(directory, "metadata.json")
	if err := rejectSymlinkPath(metadataPath); err != nil {
		return fmt.Errorf("runtime metadata path: %w", err)
	}
	file, err := os.Open(metadataPath)
	if err != nil {
		return fmt.Errorf("runtime metadata: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeManifestBytes+1))
	if err != nil {
		return fmt.Errorf("runtime metadata: %w", err)
	}
	if int64(len(data)) > maxRuntimeManifestBytes {
		return fmt.Errorf("runtime metadata exceeds %d bytes", maxRuntimeManifestBytes)
	}
	var metadata map[string]string
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	if metadata == nil {
		return errors.New("runtime metadata must be an object")
	}
	if len(metadata) > 256 {
		return errors.New("runtime metadata has too many entries")
	}
	for key, value := range metadata {
		if err := validateManifestText("metadata key", key, 256); err != nil {
			return err
		}
		if err := validateManifestText("metadata value", value, 16<<10); err != nil {
			return err
		}
	}
	// Verify may be called directly on a provider with a manifest loaded from
	// an older or operator-edited directory. Reapply the exact-ref contract
	// before consulting metadata so malformed git/tag/commit identities cannot
	// pass a filesystem-only health check. Older llama.cpp manifests did not
	// persist source/sourceType in metadata, so use the manifest source as the
	// fallback instead of treating a missing value as an implicit bundled
	// runtime (which would make an exact git ref appear optional).
	metadataSource := metadata["source"]
	if metadataSource == "" {
		metadataSource = manifest.Source
	}
	metadataSourceType := metadata["sourceType"]
	if metadataSourceType == "" {
		metadataSourceType = inferRuntimeSourceTypeForManifest(manifest.Kind, metadataSource)
	}
	if err := validateRuntimeManifestReferences(manifest, metadataSourceType, metadataSource); err != nil {
		return fmt.Errorf("runtime metadata references: %w", err)
	}
	if manifest.Fingerprint != "" && fingerprintMetadata(metadata) != manifest.Fingerprint {
		return errors.New("runtime metadata fingerprint mismatch")
	}
	if manifest.Checksum != "" {
		got := normalizeChecksum(metadata["sourceChecksum"])
		if got == "" {
			// Older staged versions only recorded checksum. Keep those manifests
			// readable while making new providers record the observed source hash.
			got = normalizeChecksum(metadata["checksum"])
		}
		if got == "" || got != normalizeChecksum(manifest.Checksum) {
			return errors.New("runtime checksum mismatch")
		}
	}
	// `verify.sha256` is an independent post-stage assertion. `source.checksum`
	// (represented by manifest.Checksum above) protects the configured source,
	// while this value is intended for operators who want the runtime metadata
	// to pin the exact artifact observed by the provider. Enforce it here rather
	// than merely carrying the field through the translator so a direct provider
	// caller and the Server-managed path have identical verification semantics.
	if configured := strings.TrimSpace(metadata["verify.sha256"]); configured != "" {
		expected := normalizeChecksum(configured)
		if len(expected) != sha256.Size*2 {
			return errors.New("runtime verification checksum must be a SHA-256 hex value")
		}
		if _, decodeErr := hex.DecodeString(expected); decodeErr != nil {
			return fmt.Errorf("runtime verification checksum: %w", decodeErr)
		}
		actual := normalizeChecksum(metadata["sourceChecksum"])
		if actual == "" {
			actual = normalizeChecksum(metadata["checksum"])
		}
		if actual == "" || actual != expected {
			return fmt.Errorf("runtime verification checksum mismatch: expected %s, got %s", expected, actual)
		}
	}
	return nil
}

func inferRuntimeSourceTypeForManifest(kind, source string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "vllm":
		return inferVLLMSourceType(source)
	case "llamacpp":
		return inferLlamaSourceType(source)
	default:
		return ""
	}
}

const defaultRuntimeDownloadBytes int64 = 8 << 30

// fileChecksum hashes an operator-selected local artifact without following a
// symlink (the caller has already checked the path boundary). It is kept
// separate from directoryChecksum because wheel/source archives are expected
// to be immutable single files.
func fileChecksum(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("artifact must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// directoryChecksum produces a stable SHA-256 for a source tree. File names,
// modes and bytes are included; symlinks are represented by their link text
// and are never followed. This lets a manifest detect a changed source tree
// while remaining deterministic across staging directories.
func directoryChecksum(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("source must be a real directory")
	}
	h := sha256.New()
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		h.Write([]byte(name))
		h.Write([]byte{0})
		mode := info.Mode()
		h.Write([]byte(mode.String()))
		h.Write([]byte{0})
		if mode&os.ModeSymlink != 0 {
			link, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			h.Write([]byte(link))
			h.Write([]byte{0})
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !mode.IsRegular() {
			return fmt.Errorf("unsupported source entry %s", name)
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyExpectedChecksum(expected, actual string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	expected = normalizeChecksum(expected)
	actual = normalizeChecksum(actual)
	if actual == "" || expected != actual {
		return fmt.Errorf("expected %s, got %s", expected, actual)
	}
	return nil
}

// downloadRuntimeArtifact fetches an HTTPS runtime artifact into a private
// temporary file, hashes it while streaming, then atomically renames it. The
// URL has already passed the provider's scheme/allowlist check; this helper
// still bounds the response and refuses redirects to an untrusted host.
func downloadRuntimeArtifact(ctx context.Context, client *http.Client, rawURL, destination string, maxBytes int64, allowlist []string) (string, string, error) {
	return downloadRuntimeArtifactWithProgress(ctx, client, rawURL, destination, maxBytes, allowlist, nil)
}

// downloadRuntimeArtifactWithProgress is the bounded downloader used by the
// managed providers.  The callback receives monotonic byte counters while the
// response is copied.  A missing Content-Length is represented by total=-1;
// callers can still expose the completed byte count without inventing a
// percentage.  Keeping the old downloadRuntimeArtifact wrapper preserves the
// small helper contract used by existing embedders and tests.
func downloadRuntimeArtifactWithProgress(ctx context.Context, client *http.Client, rawURL, destination string, maxBytes int64, allowlist []string, progress func(completed, total int64)) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", "", errors.New("runtime artifact URL must be HTTPS without credentials")
	}
	if !runtimeURLHostSafe(u, allowlist) || !sourceMatchesAllowlist(u.String(), allowlist) {
		return "", "", errors.New("runtime artifact URL is not allowed")
	}
	if maxBytes <= 0 {
		maxBytes = defaultRuntimeDownloadBytes
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	} else {
		// Do not mutate a caller-owned client. A shallow copy keeps its custom
		// transport/timeouts while allowing the runtime boundary to constrain
		// redirects independently of unrelated application traffic.
		copyClient := *client
		client = &copyClient
	}
	originalRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originalRedirect != nil {
			if err := originalRedirect(req, via); err != nil {
				return err
			}
		}
		// A caller-provided policy callback runs before the transport follows a
		// redirect and is allowed to mutate req.URL. Validate the final URL after
		// that callback as well as the initial target; otherwise a callback could
		// turn an otherwise safe redirect into a private/loopback request.
		if req == nil || req.URL == nil || !validRuntimeURL(req.URL.String(), allowlist) {
			return errors.New("runtime artifact redirect is not allowed")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("runtime artifact returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return "", "", fmt.Errorf("runtime artifact exceeds %d bytes", maxBytes)
	}
	if err := rejectSymlinkPath(destination); err != nil {
		return "", "", fmt.Errorf("runtime artifact destination: %w", err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(destination, ".runtime-download-*")
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	limited := io.LimitReader(resp.Body, maxBytes+1)
	reader := &progressReader{reader: limited, total: resp.ContentLength, progress: progress}
	written, copyErr := io.Copy(io.MultiWriter(tmp, h), reader)
	if copyErr != nil {
		return "", "", copyErr
	}
	if progress != nil {
		progress(written, resp.ContentLength)
	}
	if written > maxBytes {
		return "", "", fmt.Errorf("runtime artifact exceeds %d bytes", maxBytes)
	}
	if err := tmp.Sync(); err != nil {
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	artifactName := "runtime-artifact"
	urlPath := strings.ToLower(u.Path)
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar", ".zip", ".whl"} {
		if strings.HasSuffix(urlPath, suffix) {
			artifactName += suffix
			break
		}
	}
	final := filepath.Join(destination, artifactName)
	if err := os.Rename(tmpName, final); err != nil {
		return "", "", err
	}
	if err := syncDir(destination); err != nil {
		return "", "", err
	}
	removeTmp = false
	return final, hex.EncodeToString(h.Sum(nil)), nil
}

type progressReader struct {
	reader    io.Reader
	total     int64
	progress  func(completed, total int64)
	completed int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	if r == nil || r.reader == nil {
		return 0, io.EOF
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		r.completed = saturatingInt64Add(r.completed, int64(n))
		if r.progress != nil {
			r.progress(r.completed, r.total)
		}
	}
	return n, err
}

func saturatingInt64Add(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func progressFraction(completed, total int64, start, end float64) float64 {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > 1 {
		end = 1
	}
	if total <= 0 || completed <= 0 {
		return start
	}
	if completed > total {
		completed = total
	}
	fraction := float64(completed) / float64(total)
	return start + (end-start)*fraction
}

func formatProgressBytes(completed, total int64) string {
	if completed < 0 {
		completed = 0
	}
	if total > 0 {
		return fmt.Sprintf("%d/%d bytes", completed, total)
	}
	return fmt.Sprintf("%d bytes", completed)
}

// extractRuntimeArchive supports the release archive formats used by
// llama.cpp. It intentionally accepts regular files/directories only: archive
// symlinks and hard links are rejected to prevent an archive from escaping the
// staged source root during a later build.
func extractRuntimeArchive(ctx context.Context, archivePath, destination string, maxBytes int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxBytes <= 0 {
		maxBytes = defaultRuntimeDownloadBytes
	}
	if err := rejectSymlinkPath(archivePath); err != nil {
		return err
	}
	if err := rejectSymlinkPath(destination); err != nil {
		return fmt.Errorf("runtime archive destination: %w", err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	name := strings.ToLower(archivePath)
	if strings.HasSuffix(name, ".zip") {
		return extractZipArchive(ctx, archivePath, destination, maxBytes)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	var reader io.Reader = f
	if strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".tgz") {
		gz, gzipErr := gzip.NewReader(f)
		if gzipErr != nil {
			return gzipErr
		}
		defer gz.Close()
		reader = gz
	}
	return extractTarArchive(ctx, tar.NewReader(reader), destination, maxBytes)
}

// safeArchiveSymlink accepts only relative link targets that resolve inside
// the extraction destination: llama.cpp release archives ship shared-library
// symlink chains (libllama.so -> libllama.so.0 -> libllama.so.0.22.0), while
// an absolute or tree-escaping link would let a crafted archive plant a
// pointer outside the manager-owned directory.
func safeArchiveSymlink(destination, name, linkname string) error {
	if linkname == "" || filepath.IsAbs(linkname) {
		return fmt.Errorf("archive symlink %q has an unsafe target %q", name, linkname)
	}
	entry := filepath.Join(destination, filepath.FromSlash(name))
	resolved := filepath.Join(filepath.Dir(entry), filepath.FromSlash(linkname))
	rel, err := filepath.Rel(destination, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive symlink %q escapes the source root", name)
	}
	return nil
}

func extractTarArchive(ctx context.Context, tr *tar.Reader, destination string, maxBytes int64) error {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Name == "" || header.Name == "." || strings.ContainsRune(header.Name, '\x00') {
			return errors.New("archive entry has an invalid name")
		}
		target, err := safeArchiveTarget(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := rejectSymlinkPath(target); err != nil {
				return fmt.Errorf("archive directory %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := safeArchiveSymlink(destination, header.Name, header.Linkname); err != nil {
				return err
			}
			if err := rejectSymlinkPath(filepath.Dir(target)); err != nil {
				return fmt.Errorf("archive symlink %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("archive symlink %q: %w", header.Name, err)
			}
		case tar.TypeLink:
			source, err := safeArchiveTarget(destination, header.Linkname)
			if err != nil {
				return fmt.Errorf("archive hardlink %q: %w", header.Name, err)
			}
			if info, statErr := os.Lstat(source); statErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("archive hardlink %q references missing file %q", header.Name, header.Linkname)
			}
			if err := rejectSymlinkPath(filepath.Dir(target)); err != nil {
				return fmt.Errorf("archive hardlink %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return fmt.Errorf("archive hardlink %q: %w", header.Name, err)
			}
		case tar.TypeReg, tarTypeRegA:
			if header.Size < 0 || header.Size > maxBytes-total {
				return fmt.Errorf("archive exceeds %d bytes", maxBytes)
			}
			if err := rejectSymlinkPath(filepath.Dir(target)); err != nil {
				return fmt.Errorf("archive file %q: %w", header.Name, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			// Keep the archive's permission bits (prebuilt release archives
			// ship 0755 executables); fall back to a readable default when
			// the entry records no mode at all.
			perm := header.FileInfo().Mode().Perm()
			if perm&0o444 == 0 {
				perm = 0o640
			}
			file, createErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, perm)
			if createErr != nil {
				return createErr
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			syncErr := file.Sync()
			closeErr := file.Close()
			if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
				return err
			}
			total += header.Size
		default:
			return fmt.Errorf("archive entry %q has unsupported type", header.Name)
		}
	}
}

func extractZipArchive(ctx context.Context, archivePath, destination string, maxBytes int64) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	var total int64
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Name == "" || strings.ContainsRune(entry.Name, '\x00') {
			return errors.New("archive entry has an invalid name")
		}
		target, err := safeArchiveTarget(destination, entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %q is a link", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := rejectSymlinkPath(target); err != nil {
				return fmt.Errorf("archive directory %q: %w", entry.Name, err)
			}
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if entry.UncompressedSize64 > uint64(maxBytes-total) {
			return fmt.Errorf("archive exceeds %d bytes", maxBytes)
		}
		if err := rejectSymlinkPath(filepath.Dir(target)); err != nil {
			return fmt.Errorf("archive file %q: %w", entry.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		perm := mode.Perm()
		if perm&0o444 == 0 {
			perm = 0o640
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return openErr
		}
		file, createErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, perm)
		if createErr != nil {
			reader.Close()
			return createErr
		}
		_, copyErr := io.CopyN(file, reader, int64(entry.UncompressedSize64))
		closeReaderErr := reader.Close()
		syncErr := file.Sync()
		closeFileErr := file.Close()
		if err := errors.Join(copyErr, closeReaderErr, syncErr, closeFileErr); err != nil {
			return err
		}
		total += int64(entry.UncompressedSize64)
	}
	return nil
}

func safeArchiveTarget(destination, name string) (string, error) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return "", fmt.Errorf("archive entry %q escapes source root", name)
		}
	}
	clean := pathpkg.Clean(normalized)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, "/") || len(clean) >= 2 && clean[1] == ':' {
		return "", fmt.Errorf("archive entry %q escapes source root", name)
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes source root", name)
	}
	return target, nil
}

// collapseArchiveRoot handles GitHub's common single top-level directory
// layout (llama.cpp-master/...). It returns the directory containing
// CMakeLists.txt while leaving the extracted tree immutable.
func collapseArchiveRoot(root string) string {
	if _, err := os.Stat(filepath.Join(root, "CMakeLists.txt")); err == nil {
		return root
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return root
	}
	candidate := filepath.Join(root, entries[0].Name())
	if _, err := os.Stat(filepath.Join(candidate, "CMakeLists.txt")); err == nil {
		return candidate
	}
	return root
}

func normalizeChecksum(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
}

func cloneMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return make(map[string]string)
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		if strings.TrimSpace(key) == "" {
			continue
		}
		output[key] = value
	}
	return output
}

// sortedMapKeys returns deterministic keys for metadata/fingerprint fields.
// Build environment values themselves are never persisted; recording only the
// sorted key names keeps manifests useful for diagnostics without leaking
// potentially sensitive environment values or depending on Go map iteration
// order.
func sortedMapKeys(input map[string]string) []string {
	if len(input) == 0 {
		return nil
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		if strings.TrimSpace(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// buildArgs returns a defensive copy of a structured argv field. A runtime
// spec is often retained by the manager, so providers must not mutate the
// caller's slices while normalizing or appending arguments.
func buildArgs(spec Spec, key string) []string {
	values := spec.BuildArgs[key]
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func firstBuildArg(spec Spec, key string) string {
	values := buildArgs(spec, key)
	if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
		return strings.TrimSpace(values[0])
	}
	return strings.TrimSpace(spec.Build[key])
}

func cleanBuildArgs(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}

// runtimeSourcePath resolves a local path or a file:// URL. It intentionally
// rejects all other URL schemes so callers cannot accidentally treat a remote
// source as a filesystem path. The config validator applies the same file URL
// constraints; keeping the check here protects direct Spec users as well.
func runtimeSourcePath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err == nil && strings.EqualFold(u.Scheme, "file") {
		if u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", false
		}
		pathValue := u.Path
		if escaped := u.EscapedPath(); escaped != "" {
			if decoded, decodeErr := url.PathUnescape(escaped); decodeErr != nil {
				return "", false
			} else {
				pathValue = decoded
			}
		}
		if !filepath.IsAbs(pathValue) {
			return "", false
		}
		// Do not normalize away parent segments from a file URL. The config
		// validator and allowlist normally constrain the final path, but
		// rejecting traversal lexically here keeps a malformed direct Spec
		// from escaping an operator's intended source root.
		for _, segment := range strings.Split(filepath.ToSlash(pathValue), "/") {
			if segment == ".." {
				return "", false
			}
		}
		return filepath.Clean(pathValue), true
	}
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "git@") || strings.HasPrefix(raw, "git+") {
		return "", false
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	return filepath.Clean(raw), true
}

func inferVLLMSourceType(source string) string {
	source = strings.TrimSpace(source)
	switch {
	case source == "" || strings.EqualFold(source, "pypi") || strings.EqualFold(source, "bundled"):
		return source
	case strings.HasPrefix(source, "git+"):
		return "git"
	case strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "file://"):
		// Repository fields are translated to the source string before the
		// provider sees a Spec.  Keep the shorthand repository form useful even
		// when callers omit source.type: canonical GitHub repositories and URLs
		// ending in .git are Git sources, while wheel/archive URLs remain wheel
		// sources.  Explicit source.type still takes precedence.
		if parsed, err := url.Parse(source); err == nil && strings.EqualFold(parsed.Scheme, "https") {
			path := strings.ToLower(parsed.Path)
			if strings.HasSuffix(path, ".git") || (strings.EqualFold(parsed.Host, "github.com") && !strings.HasSuffix(path, ".whl") && !strings.HasSuffix(path, ".tar.gz") && !strings.HasSuffix(path, ".tgz") && !strings.HasSuffix(path, ".zip")) {
				return "git"
			}
		}
		return "wheel"
	default:
		return "local"
	}
}

func isRuntimeVersionCandidate(version, channel string) bool {
	original := version
	version = strings.TrimSpace(version)
	// Metadata comes from a remote index/release API and is later used both as
	// a path component and as a URL tag.  Do not silently normalize leading or
	// trailing whitespace: two visually identical tags could otherwise map to
	// different manifests, while URL delimiters could alter a release path when
	// a caller constructs a direct Spec without going through config validation.
	if version == "" || version != original || len(version) > 255 || strings.ContainsAny(version, "/\\\x00?#%") {
		return false
	}
	for _, r := range version {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	if !strings.EqualFold(strings.TrimSpace(channel), "prerelease") {
		lower := strings.ToLower(version)
		for _, marker := range []string{"dev", "alpha", "beta", "rc", "pre"} {
			if strings.Contains(lower, marker) {
				return false
			}
		}
	}
	return true
}

// compareRuntimeVersions provides a dependency-free ordering for the common
// PyPI/PEP 440 forms emitted by the vLLM index. Numeric runs are compared
// first; a stable release sorts after a prerelease with the same numeric
// prefix. The function intentionally does not claim to implement every PEP
// 440 rule, but it is deterministic and never treats arbitrary text as a
// higher version merely because it sorts lexically.
func compareRuntimeVersions(left, right string) int {
	numbers := func(value string) []int {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		var out []int
		current := 0
		inDigits := false
		seenDigits := false
		for _, r := range value {
			if r >= '0' && r <= '9' {
				current = current*10 + int(r-'0')
				inDigits = true
				seenDigits = true
				continue
			}
			if inDigits {
				out = append(out, current)
				current, inDigits = 0, false
			}
			// Letters before the first numeric run are common in llama.cpp tags
			// (for example b4200) and are only a prefix. Once a numeric core has
			// started, a letter denotes a qualifier (rc, beta, post, local build
			// metadata, ...); digits that follow belong to that qualifier rather
			// than the release core. Keeping them out prevents 0.7.0rc1 from
			// sorting after the final 0.7.0 release.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				if seenDigits {
					break
				}
			}
		}
		if inDigits {
			out = append(out, current)
		}
		return out
	}
	a, b := numbers(left), numbers(right)
	for i := 0; i < len(a) || i < len(b); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	aPre := !isRuntimeVersionCandidate(left, "stable")
	bPre := !isRuntimeVersionCandidate(right, "stable")
	if aPre != bPre {
		if aPre {
			return -1
		}
		return 1
	}
	return strings.Compare(strings.ToLower(strings.TrimSpace(left)), strings.ToLower(strings.TrimSpace(right)))
}

func inferLlamaSourceType(source string) string {
	source = strings.TrimSpace(source)
	if strings.HasPrefix(source, "https://") {
		if parsed, err := url.Parse(source); err == nil {
			path := strings.ToLower(parsed.Path)
			for _, suffix := range []string{".tar.gz", ".tgz", ".tar", ".zip"} {
				if strings.HasSuffix(path, suffix) || strings.Contains(path, "/releases/download/") {
					return "release"
				}
			}
		}
		return "git"
	}
	if strings.HasPrefix(source, "git+") || strings.HasPrefix(source, "git@") {
		return "git"
	}
	if strings.HasPrefix(source, "file://") || source != "" {
		return "local"
	}
	return "bundled"
}

func fingerprintMetadata(metadata map[string]string) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		h.Write([]byte(key))
		h.Write([]byte{0})
		h.Write([]byte(metadata[key]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ValidSourceURL reports whether a package index URL is accepted under the
// runtime source allowlist policy. It is exported so control-plane operations
// outside the built-in providers (for example the LMCache module install) can
// reuse the exact same URL safety checks.
func ValidSourceURL(raw string, allowlist []string) bool {
	return validRuntimeURL(raw, allowlist)
}

func validRuntimeURL(raw string, allowlist []string) bool {
	trimmed := strings.TrimSpace(raw)
	if raw != trimmed {
		return false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	if !runtimeURLTextSafe(u) {
		return false
	}
	if !runtimeURLHostSafe(u, allowlist) {
		return false
	}
	return sourceMatchesAllowlist(u.String(), allowlist)
}

// runtimeURLTextSafe rejects controls and invisible format characters even
// when they are hidden behind one or more percent-encoding layers. URL.Parse
// intentionally preserves those bytes in Path/RawQuery, but a later client or
// git implementation may decode them differently; validating all components
// here keeps direct provider callers on the same fail-closed boundary as the
// config loader.
func runtimeURLTextSafe(u *url.URL) bool {
	if u == nil {
		return false
	}
	for _, raw := range []string{u.Path, u.RawPath, u.RawQuery, u.Fragment} {
		decoded := raw
		for i := 0; i < 3; i++ {
			next, err := url.PathUnescape(decoded)
			if err != nil {
				return false
			}
			if next == decoded {
				break
			}
			decoded = next
		}
		for _, r := range decoded {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return false
			}
		}
	}
	return true
}

// guardedRuntimeClient copies an injected HTTP client and adds a redirect
// boundary for provider metadata checks. The initial URL is validated by the
// caller, but net/http follows redirects automatically unless CheckRedirect
// is set. Validate the post-callback URL as well: embedders may supply a
// callback that rewrites req.URL, and a public metadata endpoint must never be
// able to redirect a managed-runtime check into a loopback/private address.
func guardedRuntimeClient(source *http.Client, allowlist []string, defaultTimeout time.Duration) *http.Client {
	if source == nil {
		source = &http.Client{Timeout: defaultTimeout}
	} else {
		copyClient := *source
		source = &copyClient
	}
	originalRedirect := source.CheckRedirect
	source.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originalRedirect != nil {
			if err := originalRedirect(req, via); err != nil {
				return err
			}
		}
		if req == nil || req.URL == nil || !validRuntimeURL(req.URL.String(), allowlist) {
			return errors.New("runtime metadata redirect is not allowed")
		}
		return nil
	}
	return source
}

func validGitSource(raw string, allowlist []string) bool {
	trimmed := strings.TrimSpace(raw)
	if raw != trimmed {
		return false
	}
	raw = trimmed
	if strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !runtimeURLTextSafe(u) || !runtimeURLHostSafe(u, allowlist) {
			return false
		}
	} else if strings.HasPrefix(raw, "git@") {
		if !safeGitSSHHost(raw, allowlist) {
			return false
		}
	} else if strings.HasPrefix(raw, "file://") {
		path, ok := runtimeSourcePath(raw)
		pathRoots := filePathAllowlist(allowlist)
		if !ok || filepath.Clean(path) != path || rejectSymlinkPath(path) != nil || (len(allowlist) > 0 && len(pathRoots) == 0) || !pathAllowed(path, pathRoots) {
			return false
		}
		// File URLs are local source roots, so URL-host allowlist matching is
		// not applicable. The path allowlist above is the complete boundary.
		return true
	} else {
		return false
	}
	return sourceMatchesAllowlist(raw, allowlist)
}

func filePathAllowlist(allowlist []string) []string {
	paths := make([]string, 0, len(allowlist))
	for _, value := range allowlist {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if path, ok := runtimeSourcePath(value); ok {
			paths = append(paths, path)
			continue
		}
		if filepath.IsAbs(value) {
			paths = append(paths, filepath.Clean(value))
		}
	}
	return paths
}

// runtimeURLHostSafe blocks obvious local and link-local destinations before a
// provider opens an outbound request. Public HTTPS sources remain allowed when
// no allowlist is configured; an explicit matching allowlist entry can opt in
// to a private mirror for air-gapped deployments. This is deliberately a
// lexical guard: resolving arbitrary DNS names here would make discovery
// nondeterministic and would still be vulnerable to DNS rebinding between the
// check and the actual connection.
func runtimeURLHostSafe(u *url.URL, allowlist []string) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if host == "" {
		return false
	}
	restricted := false
	if ip, ok := runtimeIPLiteral(host); ok {
		restricted = restrictedRuntimeIP(ip)
	} else if candidates := numericIPv4Candidates(host); len(candidates) > 0 {
		// URL parsers and platform resolvers accept several legacy numeric IPv4
		// spellings (decimal, hexadecimal, octal, and abbreviated dotted forms).
		// Treat those spellings as IP literals before the request leaves the
		// process; otherwise 2130706433/0x7f000001 can bypass the lexical
		// loopback guard and resolve to 127.0.0.1. A leading-zero component is
		// ambiguous across parsers, so every valid interpretation is checked.
		for _, candidate := range candidates {
			if restrictedRuntimeIP(candidate) {
				restricted = true
				break
			}
		}
	} else {
		switch {
		case host == "localhost", strings.HasSuffix(host, ".localhost"), host == "ip6-localhost":
			restricted = true
		case host == "metadata", host == "metadata.google.internal", host == "instance-data":
			restricted = true
		case host == "host.docker.internal", host == "kubernetes.default.svc", strings.HasSuffix(host, ".cluster.local"), strings.HasSuffix(host, ".svc"):
			restricted = true
		}
	}
	if !restricted {
		return true
	}
	return len(allowlist) > 0 && sourceMatchesAllowlist(u.String(), allowlist)
}

func restrictedRuntimeIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		// net.IP.IsPrivate covers RFC 1918. CGNAT is not technically private,
		// but it is still not a public package source and should not be fetched
		// implicitly by a managed runtime.
		cgnat := ipv4[0] == 100 && ipv4[1] >= 64 && ipv4[1] <= 127
		return ipv4.IsLoopback() || ipv4.IsPrivate() || ipv4.IsLinkLocalUnicast() || ipv4.IsUnspecified() || ipv4.IsMulticast() || cgnat
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func safeGitSSHHost(raw string, allowlist []string) bool {
	withoutScheme := strings.TrimPrefix(raw, "git@")
	host := withoutScheme
	if strings.HasPrefix(host, "[") {
		// SSH URLs may use a bracketed IPv6 literal (git@[::1]:repo). The
		// first colon belongs to the address, not the host/path separator.
		close := strings.IndexByte(host, ']')
		if close < 0 {
			return false
		}
		if len(host) == close+1 || (host[close+1] != ':' && host[close+1] != '/') {
			return false
		}
		host = host[1:close]
	} else if index := strings.IndexAny(host, ":/"); index >= 0 {
		host = host[:index]
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	if ip, ok := runtimeIPLiteral(strings.Trim(host, "[]")); ok {
		if restrictedRuntimeIP(ip) {
			for _, allowed := range allowlist {
				if strings.EqualFold(strings.TrimSpace(allowed), raw) {
					return true
				}
			}
			return false
		}
	} else if candidates := numericIPv4Candidates(host); len(candidates) > 0 && anyRestrictedRuntimeIP(candidates) {
		for _, allowed := range allowlist {
			if strings.EqualFold(strings.TrimSpace(allowed), raw) {
				return true
			}
		}
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local") {
		for _, allowed := range allowlist {
			if strings.EqualFold(strings.TrimSpace(allowed), raw) {
				return true
			}
		}
		return false
	}
	return true
}

// runtimeIPLiteral recognizes IPv4/IPv6 literals, including IPv6 zone
// identifiers accepted by URL parsers (for example [fe80::1%25lo0]). A zone
// does not make a link-local or loopback address safe; strip it before applying
// the same private-destination policy used for ordinary literals.
func runtimeIPLiteral(host string) (net.IP, bool) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return nil, false
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		if zone == 0 || zone == len(host)-1 {
			return nil, false
		}
		host = host[:zone]
	}
	ip := net.ParseIP(host)
	return ip, ip != nil
}

// numericIPv4 recognizes the legacy integer forms accepted by common URL
// clients. It is intentionally limited to syntactically numeric hosts so
// ordinary DNS names are not treated as IP addresses; callers use the result
// only to apply the private/loopback destination guard.
func numericIPv4(host string) (net.IP, bool) {
	candidates := numericIPv4Candidates(host)
	if len(candidates) == 0 {
		return nil, false
	}
	return candidates[0], true
}

func anyRestrictedRuntimeIP(candidates []net.IP) bool {
	for _, candidate := range candidates {
		if restrictedRuntimeIP(candidate) {
			return true
		}
	}
	return false
}

// numericIPv4Candidates returns all interpretations of a legacy numeric IPv4
// spelling. The first candidate preserves numericIPv4's historical base-0
// result; decimal alternatives cover URL parsers that do not interpret
// leading-zero components as octal. Checking the full set prevents a private
// destination from being hidden behind parser-specific ambiguity.
func numericIPv4Candidates(host string) []net.IP {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "+- \t\r\n") {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil
	}
	values := make([][]uint64, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil
		}
		values[i] = parseNumericIPv4Component(part)
		if len(values[i]) == 0 {
			return nil
		}
	}
	candidates := make([]net.IP, 0, 16)
	var visit func(int, []uint64)
	visit = func(index int, selected []uint64) {
		if index == len(values) {
			if ip, ok := composeNumericIPv4(selected); ok {
				for _, existing := range candidates {
					if existing.Equal(ip) {
						return
					}
				}
				candidates = append(candidates, ip)
			}
			return
		}
		for _, value := range values[index] {
			visit(index+1, append(selected, value))
		}
	}
	visit(0, nil)
	return candidates
}

func parseNumericIPv4Component(part string) []uint64 {
	values := make([]uint64, 0, 2)
	// ParseUint with base 0 handles 0x-prefixed hexadecimal and a leading
	// zero octal form, while plain decimal remains the common case.
	if value, err := strconv.ParseUint(part, 0, 32); err == nil {
		values = append(values, value)
	}
	// A leading-zero component such as 08 is often interpreted as decimal by
	// URL parsers even though Go's base-0 parser rejects it as octal.
	if value, err := strconv.ParseUint(part, 10, 32); err == nil {
		for _, existing := range values {
			if existing == value {
				return values
			}
		}
		values = append(values, value)
	}
	return values
}

func composeNumericIPv4(values []uint64) (net.IP, bool) {
	if len(values) < 1 || len(values) > 4 {
		return nil, false
	}
	var number uint64
	switch len(values) {
	case 1:
		if values[0] > 0xffffffff {
			return nil, false
		}
		number = values[0]
	case 2:
		if values[0] > 0xff || values[1] > 0xffffff {
			return nil, false
		}
		number = values[0]<<24 | values[1]
	case 3:
		if values[0] > 0xff || values[1] > 0xff || values[2] > 0xffff {
			return nil, false
		}
		number = values[0]<<24 | values[1]<<16 | values[2]
	case 4:
		for _, value := range values {
			if value > 0xff {
				return nil, false
			}
		}
		number = values[0]<<24 | values[1]<<16 | values[2]<<8 | values[3]
	default:
		return nil, false
	}
	return net.IPv4(byte(number>>24), byte(number>>16), byte(number>>8), byte(number)), true
}

func pathAllowed(raw string, allowlist []string) bool {
	path, err := filepath.Abs(raw)
	if err != nil {
		return false
	}
	if len(allowlist) == 0 {
		return true
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	}
	for _, root := range allowlist {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if resolved, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
			rootAbs = resolved
		}
		rel, relErr := filepath.Rel(rootAbs, path)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sourceMatchesAllowlist(source string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, allowed := range allowlist {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		if strings.HasPrefix(source, "git@") || strings.HasPrefix(allowed, "git@") {
			if source == allowed || strings.HasPrefix(source, strings.TrimRight(allowed, "/")+"/") {
				return true
			}
			continue
		}
		sourceURL, sourceErr := url.Parse(source)
		allowedURL, allowedErr := url.Parse(allowed)
		if sourceErr != nil || allowedErr != nil || sourceURL.Scheme != allowedURL.Scheme || !strings.EqualFold(sourceURL.Host, allowedURL.Host) || sourceURL.User != nil || allowedURL.User != nil {
			continue
		}
		allowedPath := strings.TrimRight(allowedURL.Path, "/")
		if allowedPath == "" || sourceURL.Path == allowedPath || strings.HasPrefix(sourceURL.Path, allowedPath+"/") {
			return true
		}
	}
	return false
}
