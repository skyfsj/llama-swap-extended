package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

const bundledLlamaCPPPathEnv = "LLAMA_SWAP_BUNDLED_LLAMA_CPP_PATH"

// discoverBundledLlamaCPP finds only image/runtime locations owned by the
// llama-swap distribution. It intentionally does not walk PATH: registering
// an arbitrary executable found in an operator's environment would turn a
// discovery convenience into an implicit code-selection policy.
func discoverBundledLlamaCPP() (runtimeManager.Spec, bool) {
	candidates := make([]string, 0, 4)
	if configured := strings.TrimSpace(os.Getenv(bundledLlamaCPPPathEnv)); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		"/opt/llama-swap/runtimes/llamacpp",
		"/usr/local/bin",
		"/app",
	)
	return discoverBundledLlamaCPPFromCandidates(candidates)
}

func discoverBundledLlamaCPPFromCandidates(candidates []string) (runtimeManager.Spec, bool) {
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		if !filepath.IsAbs(candidate) {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if !bundledLlamaCPPDirectory(candidate) {
			continue
		}
		return runtimeManager.Spec{
			Name:       "llamacpp",
			Kind:       "llamacpp",
			Mode:       runtimeManager.RuntimeModeNative,
			Version:    "bundled",
			SourceType: "bundled",
			Source:     candidate,
			Metadata: map[string]string{
				"mode":       runtimeManager.RuntimeModeNative,
				"sourceType": "bundled",
				"builtin":    "true",
			},
		}, true
	}
	return runtimeManager.Spec{}, false
}

func bundledLlamaCPPDirectory(root string) bool {
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	for _, relative := range []string{"bin/llama-server", "llama-server"} {
		entrypoint := filepath.Join(root, filepath.FromSlash(relative))
		entryInfo, statErr := os.Lstat(entrypoint)
		if statErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() || entryInfo.Mode().Perm()&0o111 == 0 {
			continue
		}
		if err := rejectBundledRuntimePath(entrypoint); err == nil {
			return true
		}
	}
	return false
}

func rejectBundledRuntimePath(path string) error {
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || strings.ContainsRune(clean, '\x00') {
		return errors.New("invalid bundled runtime path")
	}
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if trustedBundledRuntimeAlias(current) {
				parent := filepath.Dir(current)
				if parent == current {
					return nil
				}
				continue
			}
			return errors.New("bundled runtime path contains symlink")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func trustedBundledRuntimeAlias(path string) bool {
	if stdruntime.GOOS != "darwin" {
		return false
	}
	clean := filepath.Clean(path)
	if clean != "/var" && clean != "/tmp" && clean != "/etc" {
		return false
	}
	target, err := os.Readlink(clean)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(string(filepath.Separator), target)
	}
	return filepath.Clean(target) == filepath.Join("/private", clean)
}

func hasConfiguredLlamaCPPRuntime(runtimes map[string]config.RuntimeConfig) bool {
	for name, value := range runtimes {
		if strings.EqualFold(strings.TrimSpace(name), "llamacpp") || strings.EqualFold(strings.TrimSpace(value.Kind), "llamacpp") {
			return true
		}
	}
	return false
}

// registerDiscoveredBundledLlamaCPP keeps the generated definition separate
// from YAML so an image upgrade cannot overwrite an operator's explicit
// runtime. It returns whether a persistent current pointer was created and
// therefore whether the local router needs a launch-config refresh.
func registerDiscoveredBundledLlamaCPP(manager *runtimeManager.Manager, cfg config.Config) (bool, error) {
	if manager == nil || hasConfiguredLlamaCPPRuntime(cfg.Runtimes) {
		return false, nil
	}
	spec, found := discoverBundledLlamaCPP()
	if !found {
		return false, nil
	}
	if err := manager.Register(spec.Name, spec.Kind); err != nil {
		return false, err
	}
	if err := manager.Configure(spec.Name, spec, runtimeManager.UpdatePolicy{Policy: "manual"}); err != nil {
		return false, err
	}
	_, seeded, err := manager.SeedIfMissing(context.Background(), spec)
	return seeded, err
}
