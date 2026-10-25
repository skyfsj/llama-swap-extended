package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ContainerBinding is the immutable launch identity selected by a runtime's
// current pointer. The manager persists the image digest in the manifest; the
// server uses this value when it builds the private process configuration so a
// mutable tag can never silently switch underneath an active model.
type ContainerBinding struct {
	Version  string
	Engine   string
	Image    string
	Platform string
}

// CurrentContainerBinding reads the manifest selected by
// <root>/<name>/current. A missing current pointer is reported as found=false;
// callers can still construct a startup configuration, but must keep the
// engine's pull policy at "never" until an explicit Stage/Activate operation
// has established an immutable version.
func CurrentContainerBinding(root, name, kind string) (ContainerBinding, bool, error) {
	runtimeDir, kind, err := managedContainerRuntimeDir(root, name, kind)
	if err != nil {
		return ContainerBinding{}, false, err
	}
	currentPath := filepath.Join(runtimeDir, "current")
	if _, err := os.Lstat(currentPath); errors.Is(err, os.ErrNotExist) {
		return ContainerBinding{}, false, nil
	} else if err != nil {
		return ContainerBinding{}, false, err
	}
	current, err := readLink(currentPath)
	if err != nil {
		return ContainerBinding{}, false, fmt.Errorf("read managed runtime current pointer: %w", err)
	}
	if err := validVersionDir(runtimeDir, current); err != nil {
		return ContainerBinding{}, false, fmt.Errorf("managed runtime current version: %w", err)
	}
	binding, err := ContainerBindingForVersion(root, name, kind, current)
	if err != nil {
		return ContainerBinding{}, false, err
	}
	return binding, true, nil
}

// ContainerBindingForVersion resolves an immutable container launch identity
// from one already-staged version. It is used during an activation switch so
// a candidate process can be started from an explicit version rather than a
// mutable current symlink. The manifest is the authority for both the engine
// and image digest; declarative image tags are never used as a fallback here.
func ContainerBindingForVersion(root, name, kind, version string) (ContainerBinding, error) {
	runtimeDir, kind, err := managedContainerRuntimeDir(root, name, kind)
	if err != nil {
		return ContainerBinding{}, err
	}
	if err := validateVersion(version); err != nil {
		return ContainerBinding{}, err
	}
	if err := validVersionDir(runtimeDir, version); err != nil {
		return ContainerBinding{}, fmt.Errorf("managed runtime version: %w", err)
	}
	manifest, err := readManifest(filepath.Join(runtimeDir, "versions", version, "manifest.json"))
	if err != nil {
		return ContainerBinding{}, fmt.Errorf("read managed runtime manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return ContainerBinding{}, fmt.Errorf("managed runtime manifest: %w", err)
	}
	if manifest.Name != name || !strings.EqualFold(manifest.Kind, kind) {
		return ContainerBinding{}, errors.New("managed runtime manifest identity does not match configuration")
	}
	if manifestMode(manifest) != RuntimeModeContainer {
		return ContainerBinding{}, errors.New("managed runtime manifest is not a container")
	}
	image := strings.TrimSpace(manifest.Metadata["image"])
	if image == "" {
		image = strings.TrimSpace(manifest.Source)
	}
	if err := validateContainerImage(image, nil); err != nil {
		return ContainerBinding{}, fmt.Errorf("managed runtime image: %w", err)
	}
	digest := strings.ToLower(strings.TrimSpace(manifest.Metadata["imageDigest"]))
	if digest == "" {
		digest = imageDigest(image)
	}
	if !imageDigestPattern.MatchString(digest) {
		return ContainerBinding{}, errors.New("managed runtime container manifest has no valid immutable digest")
	}
	baseImage := image
	if at := strings.IndexByte(baseImage, '@'); at >= 0 {
		baseImage = baseImage[:at]
	}
	if strings.TrimSpace(baseImage) == "" {
		return ContainerBinding{}, errors.New("managed runtime container image is empty")
	}
	engine := normalizeContainerEngine(manifest.Metadata["engine"])
	if engine != "docker" && engine != "podman" {
		return ContainerBinding{}, fmt.Errorf("managed runtime container engine %q is unsupported", engine)
	}
	platform := strings.TrimSpace(manifest.Metadata["platform"])
	if platform != "" {
		if err := validateContainerPlatform(platform); err != nil {
			return ContainerBinding{}, fmt.Errorf("managed runtime container platform: %w", err)
		}
	}
	return ContainerBinding{Version: manifest.Version, Engine: engine, Image: baseImage + "@" + digest, Platform: platform}, nil
}

// CurrentManifest returns the manifest selected by <root>/<name>/current.
// A missing current pointer is reported as found=false so callers can still
// build a launch configuration before the first activation.
func CurrentManifest(root, name, kind string) (Manifest, bool, error) {
	runtimeDir, err := managedRuntimeLaunchBase(root, name)
	if err != nil {
		return Manifest{}, false, err
	}
	currentPath := filepath.Join(runtimeDir, "current")
	if _, err := os.Lstat(currentPath); errors.Is(err, os.ErrNotExist) {
		return Manifest{}, false, nil
	} else if err != nil {
		return Manifest{}, false, err
	}
	version, err := readLink(currentPath)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("read managed runtime current pointer: %w", err)
	}
	manifest, err := ManifestForVersion(root, name, kind, version)
	if err != nil {
		return Manifest{}, false, err
	}
	return manifest, true, nil
}

// ManifestForVersion resolves the immutable manifest of one staged version.
func ManifestForVersion(root, name, kind, version string) (Manifest, error) {
	runtimeDir, err := managedRuntimeLaunchBase(root, name)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateVersion(version); err != nil {
		return Manifest{}, err
	}
	if err := validVersionDir(runtimeDir, version); err != nil {
		return Manifest{}, fmt.Errorf("managed runtime version: %w", err)
	}
	manifest, err := readManifest(filepath.Join(runtimeDir, "versions", version, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read managed runtime manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("managed runtime manifest: %w", err)
	}
	if manifest.Name != name || !strings.EqualFold(manifest.Kind, strings.TrimSpace(kind)) {
		return Manifest{}, errors.New("managed runtime manifest identity does not match native configuration")
	}
	return manifest, nil
}

func managedContainerRuntimeDir(root, name, kind string) (string, string, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) || strings.ContainsRune(root, '\x00') {
		return "", "", errors.New("managed runtime root must be an absolute path")
	}
	if err := validateName(name); err != nil {
		return "", "", err
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "vllm" && kind != "llamacpp" {
		return "", "", fmt.Errorf("unsupported managed container runtime kind %q", kind)
	}
	runtimeDir := filepath.Join(filepath.Clean(root), name)
	if err := rejectSymlinkPath(runtimeDir); err != nil {
		return "", "", fmt.Errorf("managed runtime path: %w", err)
	}
	return runtimeDir, kind, nil
}
