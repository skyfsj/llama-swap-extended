package extensions

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testDefinition() Definition {
	return Definition{
		Manifest: Manifest{ID: "zippy", Name: "Zippy", Enabled: true, Priority: 10, Permissions: Permissions{NetworkHosts: []string{"api.example.com"}}, Config: map[string]any{"secret": "value"}},
		Files: map[string]string{
			"index.js":          "export default { onRequest(ctx, request) { return request; } };\n",
			"lib/util.js":       "export const answer = 42;\n",
			"package.json":      `{"name":"zippy"}`,
			"package-lock.json": `{"lockfileVersion":3}`,
		},
	}
}

func TestExtensionArchiveRoundTrip(t *testing.T) {
	data, err := BuildExtensionArchive(testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExtensionArchive(data)
	if err != nil {
		t.Fatalf("re-import of our own export failed: %v", err)
	}
	if parsed.Manifest.ID != "zippy" || parsed.Manifest.Name != "Zippy" {
		t.Fatalf("manifest = %+v", parsed.Manifest)
	}
	// config values never travel with the archive.
	if len(parsed.Manifest.Config) != 0 {
		t.Fatalf("config leaked into import: %+v", parsed.Manifest.Config)
	}
	if len(parsed.Files) != len(testDefinition().Files) {
		t.Fatalf("files = %v", parsed.Files)
	}
	if parsed.Files["lib/util.js"] != "export const answer = 42;\n" {
		t.Fatalf("nested file content wrong: %q", parsed.Files["lib/util.js"])
	}
}

func buildArchive(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.symlink {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if entry.symlink {
			_, _ = file.Write([]byte("../../etc/passwd"))
			continue
		}
		_, _ = file.Write([]byte(entry.content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type archiveEntry struct {
	name    string
	content string
	symlink bool
}

const okManifest = "id: evil\nenabled: false\n"

func TestExtensionArchiveRejectsAttacks(t *testing.T) {
	cases := []struct {
		name    string
		entries []archiveEntry
		want    string
	}{
		{"traversal", []archiveEntry{{name: "../evil.js", content: "x"}, {name: "index.js", content: "x"}, {name: "manifest.yaml", content: okManifest}}, "archive path is not allowed"},
		{"absolute", []archiveEntry{{name: "/etc/passwd", content: "x"}, {name: "index.js", content: "x"}, {name: "manifest.yaml", content: okManifest}}, "archive path is not allowed"},
		{"symlink", []archiveEntry{{name: "lib", content: "x", symlink: true}, {name: "index.js", content: "x"}, {name: "manifest.yaml", content: okManifest}}, "symlink"},
		{"backslash", []archiveEntry{{name: "lib\\evil.js", content: "x"}, {name: "index.js", content: "x"}, {name: "manifest.yaml", content: okManifest}}, "archive path is not allowed"},
		{"duplicate", []archiveEntry{{name: "index.js", content: "a"}, {name: "index.js", content: "b"}, {name: "manifest.yaml", content: okManifest}}, "duplicate path"},
		{"no manifest", []archiveEntry{{name: "index.js", content: "x"}}, "no readable manifest.yaml"},
		{"no entry", []archiveEntry{{name: "manifest.yaml", content: okManifest}, {name: "lib/util.js", content: "x"}}, "no index.js source"},
		{"bad manifest", []archiveEntry{{name: "index.js", content: "x"}, {name: "manifest.yaml", content: "id: [unclosed"}}, "manifest.yaml is invalid"},
		{"invalid id", []archiveEntry{{name: "index.js", content: "x"}, {name: "manifest.yaml", content: "id: \"Bad ID\"\nenabled: false\n"}}, "invalid characters"},
		{"dot file", []archiveEntry{{name: ".hidden.js", content: "x"}, {name: "index.js", content: "x"}, {name: "manifest.yaml", content: okManifest}}, "archive path is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseExtensionArchive(buildArchive(t, tc.entries))
			if err == nil {
				t.Fatal("import accepted an invalid archive")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestExtensionArchiveImportSavesThroughManager(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	// Prepare an archive from a definition saved by the manager, then import it
	// under a fresh id to check the save path end to end.
	seed := Definition{Manifest: Manifest{ID: "seed", Enabled: false}, Files: map[string]string{"index.js": "export default { onRequest(ctx, request) { return request; } };\n"}}
	if _, err := m.Save(context.Background(), seed, ""); err != nil {
		t.Fatal(err)
	}
	stored, err := m.Get("seed")
	if err != nil {
		t.Fatal(err)
	}
	data, err := BuildExtensionArchive(stored)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExtensionArchive(data)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Manifest.ID = "imported"
	// The import handler forces enabled=false before Save; the archive itself
	// carries whatever the source manifest declared, which the round trip here
	// must preserve (seed was disabled).
	saved, err := m.Save(context.Background(), Definition{Manifest: parsed.Manifest, Files: parsed.Files}, "")
	if err != nil {
		t.Fatalf("save imported: %v", err)
	}
	if saved.Manifest.Enabled {
		t.Fatal("archive round trip flipped the enabled flag")
	}
	if saved.Manifest.ID != "imported" {
		t.Fatalf("saved id = %q", saved.Manifest.ID)
	}
	if _, err := os.Stat(filepath.Join(root, "imported", "index.js")); err != nil {
		t.Fatalf("imported extension missing: %v", err)
	}
}

func TestExtensionArchiveDirectoriesSurvive(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{
		Manifest:    Manifest{ID: "dircetory", Enabled: false},
		Files:       map[string]string{"index.js": "export default { onRequest(ctx, request) { return request; } };\n"},
		Directories: []string{"data/cache", "logs"},
	}
	saved, err := m.Save(context.Background(), definition, "")
	if err != nil {
		t.Fatalf("save with directories: %v", err)
	}
	if len(saved.Directories) != 2 {
		t.Fatalf("saved directories = %v", saved.Directories)
	}
	for _, directory := range []string{"data/cache", "logs"} {
		if info, err := os.Stat(filepath.Join(root, "dircetory", filepath.FromSlash(directory))); err != nil || !info.IsDir() {
			t.Fatalf("directory %s not materialized: %v", directory, err)
		}
	}
	// Read-back keeps them, and the reload cycle (directoryETag comparison)
	// stays stable so the 2s reloader does not thrash.
	stored, err := m.Get("dircetory")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Directories) != 2 {
		t.Fatalf("read-back directories = %v", stored.Directories)
	}
	item, _ := m.Compiled("dircetory")
	if item.ETag != stored.ETag {
		t.Fatalf("etag drift after reload-safe save: %s vs %s", item.ETag, stored.ETag)
	}
	// Removing a directory via a save without it must delete the folder.
	definition.Directories = []string{"logs"}
	if _, err := m.Save(context.Background(), definition, stored.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "dircetory", "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed directory still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dircetory", "logs")); err != nil {
		t.Fatalf("kept directory removed: %v", err)
	}
}

func TestExtensionArchiveDirectoriesRoundTrip(t *testing.T) {
	definition := Definition{
		Manifest:    Manifest{ID: "dirzip", Enabled: false},
		Files:       map[string]string{"index.js": "export default {};\n"},
		Directories: []string{"data/cache"},
	}
	data, err := BuildExtensionArchive(definition)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseExtensionArchive(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Manifest.Directories) != 1 || parsed.Manifest.Directories[0] != "data/cache" {
		t.Fatalf("round trip lost directories: %v", parsed.Manifest.Directories)
	}
}
