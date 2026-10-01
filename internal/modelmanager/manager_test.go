package modelmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func nilModelManagerContext() context.Context { return nil }

func TestModelManager_ListDirectory(t *testing.T) {
	root := t.TempDir()
	modelPath := filepath.Join(root, "nested", "model-Q4.gguf")
	otherPath := filepath.Join(root, "other.safetensors")
	if err := os.MkdirAll(filepath.Dir(modelPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{modelPath, otherPath} {
		if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "local", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 2 || len(catalog.Data) != 2 {
		t.Fatalf("catalog = %+v, want two model files", catalog)
	}
	if catalog.Data[0].SourceType != config.ModelFileSourceDirectory {
		t.Fatalf("source type = %q", catalog.Data[0].SourceType)
	}
	if catalog.Data[0].RelativePath != "nested/model-Q4.gguf" {
		t.Fatalf("relative path = %q", catalog.Data[0].RelativePath)
	}

	catalog, err = m.List(context.Background(), ListOptions{SourceID: "local", Query: "safetensors", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 1 || catalog.Data[0].Name != "other.safetensors" {
		t.Fatalf("filtered catalog = %+v", catalog)
	}
}

func TestModelManager_ListAndDeleteHandleNilContextAndRejectSymlinkRoot(t *testing.T) {
	root := t.TempDir()
	modelPath := filepath.Join(root, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: config.ModelFileSourceDirectory, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if catalog, err := m.List(nilModelManagerContext(), ListOptions{SourceID: "local", Limit: 20}); err != nil || catalog.Total != 1 {
		t.Fatalf("nil-context catalog = %+v, err=%v", catalog, err)
	}
	if _, err := m.Delete(nilModelManagerContext(), "local", "missing", modelPath); err == nil {
		t.Fatal("missing catalog entry unexpectedly deleted")
	}

	link := filepath.Join(root, "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	symlinkManager, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"linked": {Type: config.ModelFileSourceDirectory, Path: link},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := symlinkManager.List(context.Background(), ListOptions{SourceID: "linked", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 0 || len(catalog.Sources) != 1 || catalog.Sources[0].Available || !strings.Contains(catalog.Sources[0].Error, "symlink") {
		t.Fatalf("symlink-root catalog = %+v", catalog)
	}
}

func TestModelManager_RejectsSymlinkedParentSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlinks require elevated privileges on Windows")
	}
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"linked": {Type: config.ModelFileSourceDirectory, Path: filepath.Join(linkedParent, "models")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "linked", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 0 || len(catalog.Sources) != 1 || catalog.Sources[0].Available || !strings.Contains(catalog.Sources[0].Error, "symlink") {
		t.Fatalf("symlinked parent source catalog = %+v", catalog)
	}
	if _, err := m.ResolveDownloadSource("linked"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked parent download source accepted: %v", err)
	}
}

func TestModelManager_ListHFCache(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, "models--acme--demo", "snapshots", "abc123")
	modelPath := filepath.Join(snapshot, "model.safetensors")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"hf": {Type: "hf_ceche", Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "hf", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 1 || len(catalog.Data) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	file := catalog.Data[0]
	if file.Repository != "acme/demo" || file.Revision != "abc123" {
		t.Fatalf("HF file metadata = %+v", file)
	}
}

func TestModelManager_ResolveMissingDefaultHFCache(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "hub")
	t.Setenv("HF_HUB_CACHE", cacheRoot)
	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"hf": {Type: config.ModelFileSourceHFCache},
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := m.ResolveDownloadSource("hf")
	if err != nil {
		t.Fatal(err)
	}
	if source.Path != cacheRoot || source.Type != config.ModelFileSourceHFCache {
		t.Fatalf("resolved source = %+v, want missing default cache %q", source, cacheRoot)
	}
}

func TestModelManager_ListModelScopeCache(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, "models", "Qwen--Qwen3-ASR-1.7B", "snapshots", "abc123")
	modelPath := filepath.Join(snapshot, "model-00001-of-00002.safetensors")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"modelscope": {Type: config.ModelFileSourceMSCache, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "modelscope", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Total != 1 || len(catalog.Data) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	file := catalog.Data[0]
	if file.Repository != "Qwen/Qwen3-ASR-1.7B" || file.Revision != "abc123" || file.SourceType != config.ModelFileSourceMSCache {
		t.Fatalf("ModelScope file metadata = %+v", file)
	}
}

func TestModelManager_ResolveMissingDefaultModelScopeCache(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "modelscope")
	t.Setenv("MODELSCOPE_CACHE", cacheRoot)
	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"modelscope": {Type: config.ModelFileSourceMSCache},
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := m.ResolveDownloadSourceForProvider("modelscope", config.ModelFileSourceMSCache)
	if err != nil {
		t.Fatal(err)
	}
	if source.Path != cacheRoot || source.Type != config.ModelFileSourceMSCache {
		t.Fatalf("resolved source = %+v, want missing default cache %q", source, cacheRoot)
	}
}

func TestModelManager_DeleteHFCacheSymlinkKeepsBlob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional Windows privileges")
	}
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "models--acme--demo")
	snapshot := filepath.Join(cacheRoot, "snapshots", "abc123")
	blob := filepath.Join(cacheRoot, "blobs", "blob-1")
	modelPath := filepath.Join(snapshot, "model.safetensors")
	if err := os.MkdirAll(filepath.Dir(modelPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "blob-1"), modelPath); err != nil {
		t.Fatal(err)
	}

	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"hf": {Type: config.ModelFileSourceHFCache, Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "hf", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 1 || !catalog.Data[0].Symlink {
		t.Fatalf("catalog = %+v, want one symlink", catalog)
	}
	if _, err := m.Delete(context.Background(), "hf", catalog.Data[0].ID, modelPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("HF blob was removed: %v", err)
	}
	if _, err := os.Lstat(modelPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot link stat = %v", err)
	}
}

func TestModelManager_Delete(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "delete-me.gguf")
	if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(config.ModelFilesConfig{Sources: map[string]config.ModelFileSourceConfig{
		"local": {Type: "directory", Path: root},
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := m.List(context.Background(), ListOptions{SourceID: "local", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Data) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	deleted, err := m.Delete(context.Background(), "local", catalog.Data[0].ID, path)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Path != path {
		t.Fatalf("deleted path = %q", deleted.Path)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat deleted path = %v", err)
	}
}

func TestModelManager_RegisteredModels(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "registered.gguf")
	modelDirectory := filepath.Join(root, "model-directory")
	if err := os.MkdirAll(modelDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"served":    {Cmd: "llama-server --model " + path},
		"backend":   {Backend: config.BackendConfig{Arguments: []string{"--model", path}}},
		"directory": {Cmd: "vllm --model " + modelDirectory},
		"other":     {Cmd: "llama-server --model " + filepath.Join(root, "other.gguf")},
	}}
	registered := RegisteredModels(cfg, path)
	if len(registered) != 2 || registered[0] != "backend" || registered[1] != "served" {
		t.Fatalf("registered = %v", registered)
	}
	directoryFile := filepath.Join(modelDirectory, "weights.safetensors")
	if err := os.WriteFile(directoryFile, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	registered = RegisteredModels(cfg, directoryFile)
	if len(registered) != 1 || registered[0] != "directory" {
		t.Fatalf("directory registered = %v", registered)
	}
}
