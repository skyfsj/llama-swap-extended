package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LaunchBinding describes the executable and PATH entry selected from a
// managed runtime's durable current pointer. The returned executable retains
// the literal current path on purpose: a process created after an atomic
// runtime activation follows the new version, while an already-running
// process continues using the binary it originally exec'd.
type LaunchBinding struct {
	Executable string
	BinDir     string
	// LibraryDirs contains directories that must be visible to the dynamic
	// loader when the executable is started. llama.cpp's CMake build keeps
	// shared libraries next to the generated build/bin entrypoint; the
	// managed stable bin path is materialized separately, so callers must
	// retain both locations in LD_LIBRARY_PATH.
	LibraryDirs []string
}

// CurrentLaunchBinding converts the documented native managed-runtime command
// forms into an executable below <root>/<runtime>/current. It only constructs
// a path; a runtime can be staged after llama-swap starts, so existence is
// deliberately checked by exec at model load time rather than preventing the
// control plane from coming up.
//
// Structured backend argv is an explicit security boundary. Do not accept an
// arbitrary program name here: a runtime reference must select the actual
// vLLM interpreter/entrypoint or llama.cpp server binary, never an ambient
// host executable found through PATH.
func CurrentLaunchBinding(root, name, kind, command string) (LaunchBinding, error) {
	base, err := managedRuntimeLaunchBase(root, name)
	if err != nil {
		return LaunchBinding{}, err
	}
	return launchBindingAt(filepath.Join(base, "current"), kind, command)
}

// CurrentVersionLaunchBinding resolves current through its validated runtime
// pointer when one exists. Unlike CurrentLaunchBinding, it returns found=false
// before the first activation so server startup can still accept a declarative
// managed runtime that has not been staged yet. Once active, new processes are
// pinned to the selected version instead of inheriting future pointer swaps.
func CurrentVersionLaunchBinding(root, name, kind, command string) (LaunchBinding, bool, error) {
	base, err := managedRuntimeLaunchBase(root, name)
	if err != nil {
		return LaunchBinding{}, false, err
	}
	currentPath := filepath.Join(base, "current")
	if _, err := os.Lstat(currentPath); errors.Is(err, os.ErrNotExist) {
		return LaunchBinding{}, false, nil
	} else if err != nil {
		return LaunchBinding{}, false, err
	}
	version, err := readLink(currentPath)
	if err != nil {
		return LaunchBinding{}, false, fmt.Errorf("read managed runtime current pointer: %w", err)
	}
	binding, err := LaunchBindingForVersion(root, name, kind, command, version)
	if err != nil {
		return LaunchBinding{}, false, err
	}
	return binding, true, nil
}

// LaunchBindingForVersion resolves a native launch directly from an immutable
// staged version. Runtime activation uses it for candidate and rollback
// processes so a pointer change cannot make an old-process rollback execute
// the newly selected binary through current.
func LaunchBindingForVersion(root, name, kind, command, version string) (LaunchBinding, error) {
	base, err := managedRuntimeLaunchBase(root, name)
	if err != nil {
		return LaunchBinding{}, err
	}
	if err := validateVersion(version); err != nil {
		return LaunchBinding{}, err
	}
	if err := validVersionDir(base, version); err != nil {
		return LaunchBinding{}, fmt.Errorf("managed runtime version: %w", err)
	}
	manifest, err := readManifest(filepath.Join(base, "versions", version, "manifest.json"))
	if err != nil {
		return LaunchBinding{}, fmt.Errorf("read managed runtime manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return LaunchBinding{}, fmt.Errorf("managed runtime manifest: %w", err)
	}
	if manifest.Name != name || !strings.EqualFold(manifest.Kind, strings.TrimSpace(kind)) || manifestMode(manifest) != RuntimeModeNative {
		return LaunchBinding{}, errors.New("managed runtime manifest identity does not match native configuration")
	}
	return launchBindingAt(filepath.Join(base, "versions", version), kind, command)
}

func managedRuntimeLaunchBase(root, name string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
		return "", errors.New("managed runtime root must be an absolute path")
	}
	if err := validateName(name); err != nil {
		return "", err
	}
	base := filepath.Clean(root)
	runtimeDir := filepath.Join(base, name)
	rel, err := filepath.Rel(base, runtimeDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("managed runtime path escapes root")
	}
	return runtimeDir, nil
}

func launchBindingAt(runtimePath, kind, command string) (LaunchBinding, error) {
	command = strings.TrimSpace(command)
	if command == "" || command != filepath.Base(command) || strings.ContainsRune(command, '\x00') {
		return LaunchBinding{}, errors.New("managed runtime command must be a simple executable name")
	}

	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "vllm":
		binDir := filepath.Join(runtimePath, ".venv", "bin")
		switch command {
		case "vllm":
			return LaunchBinding{Executable: filepath.Join(binDir, "vllm"), BinDir: binDir}, nil
		case "python", "python3":
			return LaunchBinding{Executable: filepath.Join(binDir, "python"), BinDir: binDir}, nil
		default:
			return LaunchBinding{}, fmt.Errorf("vLLM managed runtime command %q is unsupported; use vllm or python", command)
		}
	case "lmcache":
		// The standalone LMCache server runs from its own version-managed
		// virtualenv, never from a vLLM venv. The console script is the only
		// supported launch form.
		if command != "lmcache" {
			return LaunchBinding{}, fmt.Errorf("LMCache managed runtime command %q is unsupported; use lmcache", command)
		}
		binDir := filepath.Join(runtimePath, ".venv", "bin")
		return LaunchBinding{Executable: filepath.Join(binDir, "lmcache"), BinDir: binDir}, nil
	case "llamacpp":
		if command != "llama-server" {
			return LaunchBinding{}, fmt.Errorf("llama.cpp managed runtime command %q is unsupported; use llama-server", command)
		}
		binDir := filepath.Join(runtimePath, "bin")
		return LaunchBinding{
			Executable:  filepath.Join(binDir, "llama-server"),
			BinDir:      binDir,
			LibraryDirs: []string{binDir, filepath.Join(runtimePath, "build", "bin")},
		}, nil
	default:
		return LaunchBinding{}, fmt.Errorf("unsupported managed runtime kind %q", kind)
	}
}
