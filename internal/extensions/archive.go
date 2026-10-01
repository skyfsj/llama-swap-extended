package extensions

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Extension archive (zip) helpers for import/export. The archive root holds a
// manifest.yaml next to the source tree; manifest.config is deliberately not
// exported — per-instance config values are not part of the extension, its
// settings declaration in index.js is.

const (
	// maxArchiveBytes bounds the compressed upload; the tree limits below
	// bound what it may expand to.
	maxArchiveBytes = 8 << 20
)

// archiveRejection reasons are user-facing: the import preview surfaces them.
var (
	ErrArchiveTooLarge     = errors.New("extension archive exceeds 8 MiB")
	ErrArchiveEmpty        = errors.New("extension archive has no files")
	ErrArchiveManifestPath = errors.New("archive path is not allowed")
	ErrArchiveDuplicate    = errors.New("archive contains a duplicate path")
	ErrArchiveSymlink      = errors.New("archive contains a symlink entry")
	ErrArchiveManifest     = errors.New("archive has no readable manifest.yaml")
	ErrArchiveSource       = errors.New("archive has no index.js source")
	ErrArchiveTooManyFiles = fmt.Errorf("archive contains more than %d files", MaxExtensionFiles)
)

// BuildExtensionArchive renders the export zip for a definition: sanitized
// manifest (config stripped) plus every source file. Declared empty
// directories are exported as explicit directory entries so the round trip
// preserves them.
func BuildExtensionArchive(d Definition) ([]byte, error) {
	manifest := d.Manifest
	manifest.Config = nil
	manifest.Directories = d.Directories
	manifestYAML, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	if err := writeArchiveFile(writer, "manifest.yaml", manifestYAML); err != nil {
		return nil, err
	}
	for _, directory := range d.Directories {
		if err := writeArchiveDir(writer, directory); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(d.Files))
	for name := range d.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeArchiveFile(writer, name, []byte(d.Files[name])); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// writeArchiveDir writes an explicit directory entry (trailing slash).
func writeArchiveDir(writer *zip.Writer, name string) error {
	if err := validateArchivePath(name); err != nil {
		return err
	}
	header := &zip.FileHeader{Name: name + "/", Method: zip.Deflate}
	header.SetMode(os.ModeDir | 0o700)
	_, err := writer.CreateHeader(header)
	return err
}

func writeArchiveFile(writer *zip.Writer, name string, content []byte) error {
	if err := validateArchivePath(name); err != nil {
		return err
	}
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = entry.Write(content)
	return err
}

// ParsedArchive is the result of reading an import archive before anything is
// saved. Diagnostics carry non-fatal observations (e.g. package.json without a
// lock file) so the preview can explain them.
type ParsedArchive struct {
	Manifest    Manifest
	Files       map[string]string
	Diagnostics []string
}

// ParseExtensionArchive reads an import archive and validates its shape.
// Rejections follow the same rules the archive writer produces: forward-only
// relative paths, no symlinks, no duplicates, managed file types, and the
// existing tree limits.
func ParseExtensionArchive(data []byte) (*ParsedArchive, error) {
	if len(data) > maxArchiveBytes+1 {
		return nil, ErrArchiveTooLarge
	}
	if len(data) > maxArchiveBytes {
		return nil, ErrArchiveTooLarge
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("extension archive is unreadable: %w", err)
	}
	files := map[string]string{}
	var manifestRaw []byte
	total := 0
	for _, entry := range reader.File {
		name := entry.Name
		// Explicit directory entries (trailing slash, dir mode) are validated
		// via the manifest's Directories list; skip them in the file loop.
		if entry.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			continue
		}
		if err := validateArchivePath(name); err != nil {
			return nil, err
		}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return nil, ErrArchiveSymlink
		}
		if entry.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			// Explicit directory entries are validated via the manifest's
			// Directories list; skip them in the file loop.
			continue
		}
		if !entry.Mode().IsRegular() {
			return nil, ErrArchiveSymlink
		}
		if _, seen := files[name]; seen {
			return nil, fmt.Errorf("%w: %s", ErrArchiveDuplicate, name)
		}
		if name == "manifest.yaml" {
			manifestRaw, err = readArchiveEntry(entry, MaxExtensionTreeBytes)
			if err != nil {
				return nil, err
			}
			continue
		}
		content, err := readArchiveEntry(entry, MaxExtensionFileBytes)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > MaxExtensionTreeBytes {
			return nil, fmt.Errorf("archive source tree exceeds %d bytes", MaxExtensionTreeBytes)
		}
		files[name] = string(content)
	}
	if manifestRaw == nil {
		return nil, ErrArchiveManifest
	}
	if len(files) == 0 {
		return nil, ErrArchiveEmpty
	}
	if _, ok := files[EntryFile]; !ok {
		return nil, ErrArchiveSource
	}
	if len(files) > MaxExtensionFiles {
		return nil, ErrArchiveTooManyFiles
	}
	var manifest Manifest
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, fmt.Errorf("manifest.yaml is invalid: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := validateFileMap(files); err != nil {
		return nil, err
	}
	parsed := &ParsedArchive{Manifest: manifest, Files: files}
	parsed.Diagnostics = archiveDiagnostics(manifest, files)
	return parsed, nil
}

// archiveDiagnostics lists non-fatal observations about the parsed archive.
func archiveDiagnostics(manifest Manifest, files map[string]string) []string {
	var notes []string
	_, hasPackage := files["package.json"]
	_, hasLock := files["package-lock.json"]
	if hasPackage && !hasLock {
		notes = append(notes, "package.json without package-lock.json: dependencies are required to be locked")
	}
	if manifest.Enabled {
		notes = append(notes, "imported extensions start disabled and must be enabled explicitly")
	}
	return notes
}

func readArchiveEntry(entry *zip.File, limit int) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	// Compressed bytes are bounded by maxArchiveBytes already; this guards the
	// declared uncompressed size against a lying header.
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("archive entry %s exceeds %d bytes", entry.Name, limit)
	}
	return data, nil
}

// validateArchivePath is the path gate: archive entries are forward-only,
// slash-separated, non-absolute, and free of traversal or dot-leading
// segments — the same shape normalizeExtensionPath enforces for saves.
func validateArchivePath(name string) error {
	if name == "" || len(name) > MaxExtensionPathLength {
		return fmt.Errorf("%w: %q", ErrArchiveManifestPath, name)
	}
	if strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') || strings.HasPrefix(name, "/") {
		return fmt.Errorf("%w: %q", ErrArchiveManifestPath, name)
	}
	if path.IsAbs(name) {
		return fmt.Errorf("%w: %q", ErrArchiveManifestPath, name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: %q", ErrArchiveManifestPath, name)
		}
		if strings.HasPrefix(segment, ".") {
			return fmt.Errorf("%w: %q", ErrArchiveManifestPath, name)
		}
	}
	return nil
}
