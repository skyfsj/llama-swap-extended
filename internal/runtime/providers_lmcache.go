package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// defaultLMCachePython is the documented LMCache baseline interpreter.
	// The published wheel requires Python >=3.10,<3.14; uv resolves the exact
	// interpreter and a mismatch fails the install with the package's own
	// error instead of being guessed here.
	defaultLMCachePython = "3.11"

	// defaultLMCacheHealthTimeout bounds the temporary server start plus
	// /healthcheck probe during Verify/Health. A cold GPU host imports torch
	// before the management endpoint answers, so the default is generous.
	defaultLMCacheHealthTimeout = 90 * time.Second

	// lmcacheProbeTimeout bounds the import probe executed against a staged
	// interpreter. The probe imports lmcache, which loads torch, so it can be
	// slow on a cold cache.
	lmcacheProbeTimeout = 120 * time.Second
)

// LMCacheProvider stages an isolated LMCache server virtualenv under
// <destination>/.venv. The server runtime is intentionally separate from the
// vLLM virtualenvs: the standalone lmcache server runs from its own
// current/previous-managed version, while each vLLM version keeps the pinned
// connector package it needs to import at process start. Source types are
// constrained by the same allowlist and SSRF rules as the vLLM provider.
type LMCacheProvider struct {
	UVPath   string
	Python   string
	IndexURL string
	// UpdateURL overrides the PyPI JSON endpoint used by CheckForUpdate. It is
	// primarily useful for controlled mirrors and deterministic tests; when
	// empty IndexURL is converted to its mirror JSON endpoint, or the public
	// lmcache endpoint is used when no index is configured.
	UpdateURL       string
	SourceAllowlist []string
	// Client is injectable for air-gapped deployments and deterministic tests.
	// A nil client uses a bounded default timeout.
	Client *http.Client
	Run    CommandRunner
	RunEnv EnvCommandRunner
	// ServerHealthProbe replaces the default temporary-server /healthcheck
	// probe. Tests inject a fake; production runs the real console script.
	ServerHealthProbe func(ctx context.Context, directory string, timeout time.Duration) error
}

// LatestVersion resolves the newest published stable release of a package
// from the PyPI JSON endpoint (p.UpdateURL, the configured index mirror, or
// pypi.org when no mirror is configured). It is the single place where an
// empty version pin is turned into a concrete one, so the server venv always
// records the exact release it staged.
func (p LMCacheProvider) LatestVersion(ctx context.Context, packageName string) (string, error) {
	return p.LatestVersionForChannel(ctx, packageName, "stable")
}

// LatestVersionForChannel resolves the newest published release accepted by
// the requested update channel. Keep LatestVersion stable by default for
// callers that do not have an LMCache policy, while lifecycle callers pass the
// configured channel explicitly.
func (p LMCacheProvider) LatestVersionForChannel(ctx context.Context, packageName, channel string) (string, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		channel = "stable"
	}
	return p.latestVersion(ctx, packageName, channel)
}

func (p LMCacheProvider) latestVersion(ctx context.Context, packageName, channel string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.TrimSpace(packageName)
	if name == "" {
		name = "lmcache"
	}
	endpoint := strings.TrimSpace(p.UpdateURL)
	if endpoint == "" {
		endpoint = lmcacheMetadataEndpoint(p.IndexURL, name)
		if endpoint == "" {
			endpoint = "https://pypi.org/pypi/" + name + "/json"
		}
	}
	metadataAllowlist := append([]string(nil), p.SourceAllowlist...)
	if !validLMCacheMetadataURL(endpoint, p.IndexURL, name, p.SourceAllowlist) {
		return "", errors.New("lmcache update URL is not allowed")
	}
	// A simple-index path is often the only path granted to a private mirror.
	// The derived JSON endpoint is still constrained to the same validated host,
	// but add that exact endpoint to the redirect boundary so a harmless mirror
	// redirect does not get rejected merely because its path is outside /simple.
	if !validRuntimeURL(endpoint, p.SourceAllowlist) {
		metadataAllowlist = append(metadataAllowlist, endpoint)
	}
	client := guardedRuntimeClient(p.Client, metadataAllowlist, 30*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("lmcache package metadata returned HTTP %d", resp.StatusCode)
	}
	var metadata struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := decodeBoundedRuntimeJSON(resp.Body, maxRuntimeMetadataBytes, &metadata); err != nil {
		return "", fmt.Errorf("decode lmcache package metadata: %w", err)
	}
	latest := ""
	if infoVersion := strings.TrimSpace(metadata.Info.Version); isRuntimeVersionCandidate(infoVersion, channel) {
		latest = infoVersion
	}
	if len(metadata.Releases) > 0 {
		for version := range metadata.Releases {
			if !isRuntimeVersionCandidate(version, channel) {
				continue
			}
			if latest == "" || compareRuntimeVersions(version, latest) > 0 {
				latest = version
			}
		}
	}
	if latest == "" || !isRuntimeVersionCandidate(latest, channel) {
		return "", fmt.Errorf("no %s %s release published", strings.TrimSpace(channel), name)
	}
	return latest, nil
}

// lmcacheMetadataEndpoint derives the JSON metadata location used by a PyPI
// mirror. An empty index intentionally returns an empty string so callers can
// make the explicit decision to use public PyPI; a configured but malformed
// index is returned unchanged and is rejected by the normal URL validation.
func lmcacheMetadataEndpoint(indexURL, packageName string) string {
	indexURL = strings.TrimSpace(indexURL)
	if indexURL == "" {
		return ""
	}
	u, err := url.Parse(indexURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return indexURL
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/simple") {
		path = strings.TrimSuffix(path, "/simple")
	}
	path = strings.TrimRight(path, "/") + "/pypi/" + url.PathEscape(strings.TrimSpace(packageName)) + "/json"
	u.Path = path
	u.RawPath = ""
	return u.String()
}

// validLMCacheMetadataURL accepts the ordinary runtime URL boundary and, for
// a configured simple-index mirror, the derived JSON path on that same
// already-validated host. The latter is deliberately exact: it does not turn
// an allowlisted index into permission to query an arbitrary second host.
func validLMCacheMetadataURL(endpoint, indexURL, packageName string, allowlist []string) bool {
	if validRuntimeURL(endpoint, allowlist) {
		return true
	}
	indexURL = strings.TrimSpace(indexURL)
	if indexURL == "" || endpoint != lmcacheMetadataEndpoint(indexURL, packageName) || !validRuntimeURL(indexURL, allowlist) {
		return false
	}
	index, indexErr := url.Parse(indexURL)
	metadata, metadataErr := url.Parse(endpoint)
	if indexErr != nil || metadataErr != nil || index == nil || metadata == nil {
		return false
	}
	return metadata.Scheme == "https" && metadata.Host != "" && metadata.User == nil &&
		strings.EqualFold(index.Host, metadata.Host) && runtimeURLTextSafe(metadata)
}

// CheckForUpdate discovers the newest lmcache package version for a PyPI-backed
// runtime. Explicit update.version values remain authoritative. A Git runtime
// with an explicit trackRef may resolve that ref even before its first stage.
func (p LMCacheProvider) CheckForUpdate(ctx context.Context, current Manifest, desired Spec, policy UpdatePolicy) (Spec, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(desired.Version) != "" {
		return desired, desiredDiff(current, desired), nil
	}
	sourceType := strings.ToLower(strings.TrimSpace(desired.SourceType))
	if sourceType == "" {
		sourceType = inferLMCacheSourceType(desired.Source)
	}
	if sourceType == "git" && strings.TrimSpace(desired.Metadata["trackRef"]) != "" {
		return checkTrackedGitUpdate(ctx, p.Run, p.SourceAllowlist, current, desired)
	}
	if sourceType != "pypi" {
		return desired, desiredDiff(current, desired), nil
	}
	name := firstBuildArg(desired, "package")
	channel := strings.TrimSpace(policy.Channel)
	if channel == "" {
		channel = "stable"
	}
	latest, err := p.latestVersion(ctx, name, channel)
	if err != nil {
		return desired, false, err
	}
	// Automatic checks must never turn a stale or reordered package index into
	// a downgrade, mirroring the vLLM checker.
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

func (p LMCacheProvider) Stage(ctx context.Context, spec Spec, destination string) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateProviderStage(spec, "lmcache"); err != nil {
		return Manifest{}, err
	}
	if err := prepareProviderDestination(destination); err != nil {
		return Manifest{}, fmt.Errorf("lmcache stage destination: %w", err)
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
	python := p.Python
	if configured := firstBuildArg(spec, "python"); configured != "" {
		python = configured
	}
	if python == "" {
		python = defaultLMCachePython
	}
	driver := strings.ToLower(strings.TrimSpace(firstBuildArg(spec, "driver")))
	if driver == "" {
		driver = "uv"
	}
	if driver != "uv" && driver != "custom" {
		return Manifest{}, fmt.Errorf("lmcache build driver %q is unsupported", driver)
	}
	venv := filepath.Join(destination, ".venv")
	reportRuntimeProgress(ctx, "staging", 0.12, "creating LMCache virtualenv")
	if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, uv, "venv", "--python", python, venv); err != nil {
		return Manifest{}, fmt.Errorf("create lmcache virtualenv: %w", err)
	}
	packageName := "lmcache"
	if configured := firstBuildArg(spec, "package"); configured != "" {
		packageName = configured
	}
	source := strings.TrimSpace(spec.Source)
	sourceType := strings.ToLower(strings.TrimSpace(spec.SourceType))
	if sourceType == "" {
		sourceType = inferLMCacheSourceType(source)
	}
	if sourceType == "" {
		// Empty source means the default PyPI package, matching uv's normal
		// install behavior. Record that choice explicitly for manifests.
		sourceType = "pypi"
	}
	var packageSpec string
	var sourceChecksum string
	var sourceRoot string
	switch sourceType {
	case "pypi", "bundled":
		// The version label is an exact pin recorded in the manifest; the
		// package spec pins the same version so the staged venv cannot drift.
		// The package requirement itself must be a bare project name (optionally
		// with extras): the version is carried exclusively in spec.Version so a
		// direct caller cannot produce a double-pinned requirement.
		if strings.ContainsAny(packageName, "=<>~!") {
			return Manifest{}, fmt.Errorf("lmcache package %q must be a bare project name; the version belongs in the runtime version", packageName)
		}
		if strings.TrimSpace(spec.Version) == "" {
			return Manifest{}, errors.New("lmcache pypi source requires an explicit version")
		}
		packageSpec = packageName + "==" + spec.Version
	case "git":
		if spec.Ref == "" {
			return Manifest{}, errors.New("lmcache git source requires an exact ref")
		}
		gitSource := strings.TrimPrefix(source, "git+")
		if !validGitSource(gitSource, p.SourceAllowlist) {
			return Manifest{}, errors.New("lmcache git source is not allowed")
		}
		packageSpec = "git+" + gitSource + "@" + spec.Ref
		if driver == "custom" {
			sourceRoot = filepath.Join(destination, "source")
			reportRuntimeProgress(ctx, "downloading", 0.2, "cloning lmcache source")
			if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, "git", "clone", "--no-checkout", gitSource, sourceRoot); err != nil {
				return Manifest{}, fmt.Errorf("clone lmcache source: %w", err)
			}
			if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, sourceRoot, "git", "checkout", "--detach", spec.Ref); err != nil {
				return Manifest{}, fmt.Errorf("checkout lmcache ref: %w", err)
			}
		}
	case "wheel", "release":
		if source == "" {
			return Manifest{}, errors.New("lmcache wheel source is required")
		}
		if localPath, ok := runtimeSourcePath(source); ok {
			if err := rejectSymlinkPath(localPath); err != nil {
				return Manifest{}, fmt.Errorf("lmcache source path: %w", err)
			}
			info, statErr := os.Stat(localPath)
			if statErr != nil {
				return Manifest{}, fmt.Errorf("lmcache source path: %w", statErr)
			}
			if !info.Mode().IsRegular() {
				return Manifest{}, errors.New("lmcache wheel source must be a regular file")
			}
			if !pathAllowed(localPath, p.SourceAllowlist) {
				return Manifest{}, errors.New("lmcache source path is not allowed")
			}
			var checksumErr error
			sourceChecksum, checksumErr = fileChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash lmcache source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("lmcache source checksum: %w", err)
			}
			packageSpec = localPath
		} else if validRuntimeURL(source, p.SourceAllowlist) {
			reportRuntimeProgress(ctx, "downloading", 0.2, "downloading lmcache wheel")
			artifact, checksum, downloadErr := downloadRuntimeArtifactWithProgress(ctx, p.Client, source, destination, defaultRuntimeDownloadBytes, p.SourceAllowlist, func(completed, total int64) {
				progress := progressFraction(completed, total, 0.2, 0.4)
				reportRuntimeProgressBytes(ctx, "downloading", progress, completed, total, fmt.Sprintf("downloaded %s", formatProgressBytes(completed, total)))
			})
			if downloadErr != nil {
				return Manifest{}, fmt.Errorf("download lmcache wheel: %w", downloadErr)
			}
			sourceChecksum = checksum
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("lmcache source checksum: %w", err)
			}
			packageSpec = artifact
		} else {
			return Manifest{}, errors.New("lmcache source URL or path is not allowed")
		}
	case "local":
		localPath, ok := runtimeSourcePath(source)
		if !ok {
			return Manifest{}, errors.New("lmcache source path is not allowed")
		}
		if err := rejectSymlinkPath(localPath); err != nil {
			return Manifest{}, fmt.Errorf("lmcache source path: %w", err)
		}
		if !pathAllowed(localPath, p.SourceAllowlist) {
			return Manifest{}, errors.New("lmcache source path is not allowed")
		}
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			return Manifest{}, fmt.Errorf("lmcache source path: %w", statErr)
		}
		if info.IsDir() {
			var checksumErr error
			sourceChecksum, checksumErr = directoryChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash lmcache source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("lmcache source checksum: %w", err)
			}
			sourceRoot = filepath.Join(destination, "source")
			if err := os.MkdirAll(sourceRoot, 0o750); err != nil {
				return Manifest{}, fmt.Errorf("create lmcache source directory: %w", err)
			}
			if err := copyTree(ctx, localPath, sourceRoot); err != nil {
				return Manifest{}, fmt.Errorf("copy lmcache source: %w", err)
			}
			packageSpec = sourceRoot
		} else if info.Mode().IsRegular() {
			var checksumErr error
			sourceChecksum, checksumErr = fileChecksum(localPath)
			if checksumErr != nil {
				return Manifest{}, fmt.Errorf("hash lmcache source: %w", checksumErr)
			}
			if err := verifyExpectedChecksum(spec.Checksum, sourceChecksum); err != nil {
				return Manifest{}, fmt.Errorf("lmcache source checksum: %w", err)
			}
			packageSpec = localPath
		} else {
			return Manifest{}, errors.New("lmcache local source must be a directory or regular wheel file")
		}
	default:
		return Manifest{}, fmt.Errorf("unsupported lmcache source type %q", spec.SourceType)
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
	if driver == "custom" {
		if len(spec.BuildSteps) == 0 {
			return Manifest{}, errors.New("lmcache custom build requires at least one build step")
		}
		defaultDir := destination
		if sourceRoot != "" {
			defaultDir = sourceRoot
		}
		if configured := strings.TrimSpace(spec.Build["workDir"]); configured != "" {
			resolved, resolveErr := resolveRuntimeWorkDir(destination, expandRuntimeMacro(configured, vars))
			if resolveErr != nil {
				return Manifest{}, fmt.Errorf("lmcache custom workDir: %w", resolveErr)
			}
			defaultDir = resolved
		}
		if err := runStructuredBuildSteps(ctx, destination, defaultDir, spec.BuildSteps, spec.BuildEnv, vars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	} else {
		args := []string{"pip", "install", "--python", vars["PYTHON"]}
		if indexURL != "" {
			if !validRuntimeURL(indexURL, p.SourceAllowlist) {
				return Manifest{}, errors.New("lmcache package index URL is not allowed")
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
		args = append(args, lmcacheCLIDependencies...)
		reportRuntimeProgress(ctx, "building", 0.45, "installing lmcache and dependencies")
		if _, err := runRuntimeCommandEnv(ctx, run, runEnv, spec.BuildEnv, destination, uv, args...); err != nil {
			return Manifest{}, fmt.Errorf("install lmcache: %w", err)
		}
		if err := runStructuredBuildSteps(ctx, destination, destination, spec.BuildSteps, spec.BuildEnv, vars, run, runEnv); err != nil {
			return Manifest{}, err
		}
	}
	if err := copyRuntimeArtifacts(ctx, destination, spec.Artifacts, vars); err != nil {
		return Manifest{}, err
	}
	// The import probe is mandatory for lmcache: a staged environment that
	// cannot import lmcache is broken, and activation must not proceed. This
	// is stricter than the vLLM best-effort probe because the LMCache server
	// has no separate smoke command in the common deployment.
	probed, probeErr := probeLMCacheEnvironment(ctx, run, destination, filepath.Join(venv, "bin", "python"))
	if probeErr != nil {
		return Manifest{}, probeErr
	}
	metadata := cloneMetadata(spec.Metadata)
	metadata["mode"] = normalizeRuntimeMode(spec.Mode)
	metadata["sourceType"] = sourceType
	if len(spec.BuildEnv) > 0 {
		metadata["buildEnvKeys"] = strings.Join(sortedMapKeys(spec.BuildEnv), ",")
	}
	metadata["python"] = python
	metadata["pythonPath"] = python
	if probed["lmcache"] != "" {
		metadata["lmcache"] = probed["lmcache"]
	}
	metadata["uv"] = uv
	metadata["package"] = packageIdentity
	metadata["packageName"] = packageName
	metadata["source"] = source
	metadata["runtime"] = runtime.GOOS + "/" + runtime.GOARCH
	metadata["installedAt"] = time.Now().UTC().Format(time.RFC3339)
	if probed["pythonVersion"] != "" {
		metadata["pythonVersion"] = probed["pythonVersion"]
	}
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
	metadata["lockFingerprint"] = runtimeLockFingerprint(spec, packageIdentity, python, indexURL, nil, sourceChecksum)
	reportRuntimeProgress(ctx, "verifying", 0.8, "probing lmcache environment")
	if err := writeRuntimeMetadata(destination, metadata); err != nil {
		return Manifest{}, err
	}
	fingerprint := fingerprintMetadata(metadata)
	manifestSource := spec.Source
	if strings.TrimSpace(manifestSource) == "" {
		manifestSource = sourceType
		if strings.TrimSpace(manifestSource) == "" {
			manifestSource = "pypi"
		}
	}
	manifest := Manifest{Name: spec.Name, Version: spec.Version, Kind: "lmcache", Source: manifestSource, Ref: spec.Ref, Commit: spec.Commit, Checksum: spec.Checksum, Fingerprint: fingerprint, Metadata: metadata, InstalledAt: time.Now().UTC()}
	manifest.Python = python
	manifest.PythonVersion = strings.TrimSpace(probed["pythonVersion"])
	return manifest, nil
}

func (p LMCacheProvider) Verify(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if manifest.Kind != "" && manifest.Kind != "lmcache" {
		return fmt.Errorf("manifest kind %q is not lmcache", manifest.Kind)
	}
	venvPath := filepath.Join(directory, ".venv")
	if err := rejectSymlinkPath(venvPath); err != nil {
		return fmt.Errorf("lmcache virtualenv path: %w", err)
	}
	info, err := os.Lstat(venvPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("lmcache runtime has no .venv")
	}
	if err := verifyRuntimeMetadata(directory, manifest); err != nil {
		return err
	}
	if err := verifyLMCacheEntrypoints(directory); err != nil {
		return err
	}
	// Re-probe the interpreter: an operator-edited or partially upgraded venv
	// must not pass a filesystem-only check.
	run := p.Run
	if run == nil {
		run = defaultCommandRunner
	}
	probed, err := probeLMCacheEnvironment(ctx, run, directory, filepath.Join(venvPath, "bin", "python"))
	if err != nil {
		return err
	}
	if recorded := strings.TrimSpace(manifest.Metadata["lmcache"]); recorded != "" {
		if got := strings.TrimSpace(probed["lmcache"]); got != "" && got != recorded {
			return fmt.Errorf("lmcache version drift: manifest records %s but the venv reports %s", recorded, got)
		}
	}
	return nil
}

// verifyLMCacheEntrypoints checks the interpreter and the lmcache console
// script that CurrentVersionLaunchBinding resolves for this runtime kind.
func verifyLMCacheEntrypoints(directory string) error {
	binDir := filepath.Join(directory, ".venv", "bin")
	if err := rejectSymlinkPath(binDir); err != nil {
		return fmt.Errorf("lmcache virtualenv bin path: %w", err)
	}
	for _, name := range []string{"python", "lmcache"} {
		entrypoint := filepath.Join(binDir, name)
		info, err := os.Stat(entrypoint)
		if err != nil {
			return fmt.Errorf("lmcache entrypoint %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("lmcache entrypoint %s is not a regular file", name)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("lmcache entrypoint %s is not executable", name)
		}
	}
	return nil
}

func (p LMCacheProvider) Health(ctx context.Context, manifest Manifest, directory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.Verify(ctx, manifest, directory); err != nil {
		return err
	}
	if err := validateRuntimeHealthPath(manifest.Metadata["verify.healthPath"]); err != nil {
		return err
	}
	if raw := strings.TrimSpace(manifest.Metadata["verify.smokeCommand"]); raw != "" {
		if err := runRuntimeSmokeCommand(ctx, p.Run, directory, raw); err != nil {
			return err
		}
	}
	probe := p.ServerHealthProbe
	if probe == nil {
		probe = defaultLMCacheServerProbe
	}
	timeout := defaultLMCacheHealthTimeout
	if raw := strings.TrimSpace(manifest.Metadata["verify.healthTimeout"]); raw != "" {
		if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
			timeout = duration
		}
	}
	return probe(ctx, directory, timeout)
}

// probeLMCacheEnvironment imports lmcache through the staged interpreter and
// reports the installed version. It fails closed: a missing interpreter, a
// non-zero exit, or an unimportable package is an error, not a silent skip.
// Version metadata is tolerated as empty for dev builds without packaging
// metadata; importability is not.
func probeLMCacheEnvironment(ctx context.Context, run CommandRunner, directory, python string) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if run == nil || strings.TrimSpace(directory) == "" || strings.TrimSpace(python) == "" {
		return nil, errors.New("lmcache probe requires a command runner, directory, and python path")
	}
	const script = `import importlib.metadata as m, json, os, platform, sys
out = {"pythonVersion": platform.python_version(), "lmcache": "", "importable": False}
try:
    out["lmcache"] = m.version("lmcache")
except Exception:
    pass
# The runner merges stderr into the returned bytes, so import-time noise
# (banners, colored progress) would corrupt the JSON payload; silence it.
try:
    saved = sys.stderr
    sys.stderr = open(os.devnull, "w")
    try:
        import lmcache
        out["importable"] = True
    finally:
        sys.stderr = saved
except Exception:
    pass
print(json.dumps(out, sort_keys=True))`
	probeCtx, cancel := context.WithTimeout(ctx, lmcacheProbeTimeout)
	defer cancel()
	output, err := runRuntimeCommand(probeCtx, run, directory, python, "-c", script)
	if err != nil {
		return nil, fmt.Errorf("probe lmcache environment: %v: %s", err, tailRuntimeOutput(output, 400))
	}
	if len(output) > 8<<10 {
		return nil, errors.New("lmcache probe output is unexpectedly large")
	}
	payload := lmcacheProbeJSONLine(output)
	if payload == nil {
		return nil, fmt.Errorf("decode lmcache probe output: %s", tailRuntimeOutput(output, 200))
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("decode lmcache probe output: %w", err)
	}
	result := make(map[string]string)
	for _, key := range []string{"pythonVersion", "lmcache"} {
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
	if importable, ok := raw["importable"].(bool); !ok || !importable {
		return nil, errors.New("lmcache is not importable in the staged environment")
	}
	return result, nil
}

// lmcacheProbeJSONLine extracts the probe's JSON payload from the combined
// command output. The probe script prints exactly one JSON object as its
// final statement, but the runner merges stderr into the returned bytes, so
// import-time noise can precede the payload on a dirty interpreter.
func lmcacheProbeJSONLine(output []byte) []byte {
	var payload []byte
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		payload = line
	}
	return payload
}

func tailRuntimeOutput(output []byte, max int) string {
	output = bytes.TrimSpace(output)
	if len(output) <= max {
		return string(output)
	}
	return "…" + string(output[len(output)-max:])
}

// lmcacheCLIDependencies are extra packages the default pip install adds
// alongside the lmcache requirement. The lmcache console script imports
// `openai` unconditionally through its bench command (lmcache/cli/commands/
// bench/engine_bench/config.py) even though the distribution metadata never
// declares it, so a bare `uv pip install lmcache` leaves a venv where
// `lmcache server` crashes with ModuleNotFoundError. Only the dedicated
// server venv needs it: the per-model connector is imported by vLLM and does
// not touch the CLI import chain. Drop the entry once upstream declares the
// dependency in Requires-Dist.
var lmcacheCLIDependencies = []string{"openai"}

func inferLMCacheSourceType(source string) string {
	source = strings.TrimSpace(source)
	switch {
	case source == "" || strings.EqualFold(source, "pypi"):
		return source
	case strings.HasPrefix(source, "git+"):
		return "git"
	case strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "file://"):
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

// defaultLMCacheServerProbe starts a temporary lmcache server from the staged
// version and waits until the management /healthcheck endpoint reports
// healthy. The probe binds only to loopback on ephemeral ports with a minimal
// pinned-DRAM pool, so it cannot collide with or meaningfully load a running
// deployment. Any failure returns the server log tail for diagnosis.
func defaultLMCacheServerProbe(ctx context.Context, directory string, timeout time.Duration) error {
	err := defaultLMCacheServerProbeOnce(ctx, directory, timeout)
	if err == nil {
		return nil
	}
	// The ports are picked before the server starts, so a concurrent bind can
	// steal one and kill the probe instantly (a TOCTOU race, not a runtime
	// fault). One retry with fresh ports turns that race into a spurious
	// blip; genuine faults fail the second attempt too. The early-exit path
	// fails within seconds, so the retry adds almost no latency.
	var early *lmCacheProbeEarlyExitError
	if errors.As(err, &early) && (ctx == nil || ctx.Err() == nil) {
		return defaultLMCacheServerProbeOnce(ctx, directory, timeout)
	}
	return err
}

// lmCacheProbeEarlyExitError marks a probe attempt whose server process died
// before the health endpoint ever answered.
type lmCacheProbeEarlyExitError struct{ log string }

func (e *lmCacheProbeEarlyExitError) Error() string {
	return "lmcache health probe server exited early: " + e.log
}

func defaultLMCacheServerProbeOnce(ctx context.Context, directory string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultLMCacheHealthTimeout
	}
	zmqPort, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("lmcache health probe: %w", err)
	}
	httpPort, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("lmcache health probe: %w", err)
	}
	executable := filepath.Join(directory, ".venv", "bin", "lmcache")
	args := []string{
		"server",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(zmqPort),
		"--l1-size-gb", "1",
		"--eviction-policy", "LRU",
		"--chunk-size", "256",
		"--http-host", "127.0.0.1",
		"--http-port", strconv.Itoa(httpPort),
		"--instance-id", "llama-swap-health-probe",
	}
	logFile, err := os.CreateTemp(directory, ".health-probe-*.log")
	if err != nil {
		return fmt.Errorf("lmcache health probe log: %w", err)
	}
	logPath := logFile.Name()
	defer os.Remove(logPath)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = directory
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start lmcache health probe server: %w: %s", err, tailProbeLog(logPath, 200))
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
		logFile.Close()
	}()
	// waitDone guards the buffered channel: the early-exit branch may consume
	// waitCh before stopProbe runs, and a second select on the empty buffer
	// would block for the full escalation timeout.
	waitDone := false
	stopProbe := func() {
		if waitDone {
			return
		}
		if runtime.GOOS == "windows" {
			_ = cmd.Process.Kill()
		} else if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitCh:
			waitDone = true
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
			waitDone = true
		}
	}
	probeURL := fmt.Sprintf("http://127.0.0.1:%d/healthcheck", httpPort)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			stopProbe()
			return fmt.Errorf("lmcache health probe canceled: %w", ctx.Err())
		case <-waitCh:
			waitDone = true
			return &lmCacheProbeEarlyExitError{log: tailProbeLog(logPath, 200)}
		case <-time.After(250 * time.Millisecond):
		}
		resp, err := client.Get(probeURL)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "healthy") {
			stopProbe()
			return nil
		}
		lastErr = fmt.Errorf("healthcheck returned HTTP %d: %s", resp.StatusCode, tailRuntimeOutput(body, 200))
	}
	stopProbe()
	return fmt.Errorf("lmcache health probe did not become healthy within %s: %v: %s", timeout, lastErr, tailProbeLog(logPath, 200))
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	port, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("lmcache health probe: unexpected loopback address type")
	}
	return port.Port, nil
}

func tailProbeLog(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no server log)"
	}
	return tailRuntimeOutput(data, max)
}
