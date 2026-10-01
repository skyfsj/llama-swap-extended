package extensions

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// CheckDiagnostic is one syntax or resolution problem found in a draft tree.
// Line and Column are 1-based; Length is the number of bytes of the offending
// token, which the editor turns into a highlight range.
type CheckDiagnostic struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Length   int    `json:"length"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// virtualWorkingDir is a stand-in absolute working directory: nothing is read
// from it and every file is served from the in-memory tree, but esbuild refuses
// a relative one.
const virtualWorkingDir = "/virtual-extension"

// CheckTree checks a draft tree in memory and reports what esbuild finds. Two
// passes run: every file is parsed on its own, so a syntax error in a module the
// entrypoint does not import yet is still reported, and the tree is bundled to
// catch unresolved relative imports. Neither pass touches the disk, so a draft
// can be checked before it is saved. Dependencies are deliberately not
// installed: a bare import is marked external so an extension that declares
// packages does not look broken, and those packages are resolved when the
// extension is saved.
func CheckTree(files map[string]string) []CheckDiagnostic {
	if len(files) == 0 {
		return []CheckDiagnostic{{Path: EntryFile, Severity: "error", Message: "no files to check"}}
	}
	seen := map[string]bool{}
	diagnostics := make([]CheckDiagnostic, 0, len(files))
	add := func(candidates []CheckDiagnostic) {
		for _, candidate := range candidates {
			key := fmt.Sprintf("%s:%d:%d:%s", candidate.Path, candidate.Line, candidate.Column, candidate.Message)
			if seen[key] {
				continue
			}
			seen[key] = true
			diagnostics = append(diagnostics, candidate)
		}
	}
	for _, name := range sortedTreePaths(files) {
		add(CheckFile(name, files[name]))
	}
	add(checkBundle(files))
	return diagnostics
}

// bundleTree bundles a tree in memory and returns the resulting script. It
// shares its resolver with the checker, so anything the checker accepts also
// bundles: bare specifiers stay external.
func bundleTree(files map[string]string) (string, error) {
	if _, ok := files[EntryFile]; !ok {
		return "", fmt.Errorf("bundle: %s is missing", EntryFile)
	}
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{EntryFile}, AbsWorkingDir: virtualWorkingDir, Bundle: true, Write: false,
		Platform: api.PlatformNeutral, Format: api.FormatIIFE, GlobalName: "__llamaSwapExtension",
		LogLevel: api.LogLevelSilent, LogLimit: 100,
		Plugins: []api.Plugin{{
			Name:  "llama-swap-virtual-tree",
			Setup: virtualTreeSetup(files),
		}},
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("bundle: %s", bundleErrorText(result.Errors))
	}
	if len(result.OutputFiles) != 1 {
		return "", errors.New("bundle: no JavaScript output")
	}
	return string(result.OutputFiles[0].Contents), nil
}

// checkBundle resolves the tree's imports in memory, which is what reports a
// relative path that points at nothing.
func checkBundle(files map[string]string) []CheckDiagnostic {
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{EntryFile}, AbsWorkingDir: virtualWorkingDir, Bundle: true, Write: false,
		Platform: api.PlatformNeutral, Format: api.FormatIIFE, GlobalName: "__llamaSwapExtension",
		LogLevel: api.LogLevelSilent, LogLimit: 100,
		Plugins: []api.Plugin{{
			Name:  "llama-swap-virtual-tree",
			Setup: virtualTreeSetup(files),
		}},
	})
	diagnostics := make([]CheckDiagnostic, 0, len(result.Errors)+len(result.Warnings))
	for _, message := range result.Errors {
		diagnostics = append(diagnostics, checkDiagnostic(message, "error"))
	}
	for _, message := range result.Warnings {
		diagnostics = append(diagnostics, checkDiagnostic(message, "warning"))
	}
	return diagnostics
}

// virtualNamespace is the plugin namespace the tree is served under. It tells
// esbuild the resolved paths are tree keys rather than file-system paths, which
// is what keeps the whole check off the disk.
const virtualNamespace = "llama-swap-tree"

// virtualTreeSetup serves imports from the in-memory tree. Nothing is read from
// the working directory.
func virtualTreeSetup(files map[string]string) func(api.PluginBuild) {
	return func(build api.PluginBuild) {
		build.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			if args.Kind != api.ResolveEntryPoint && !strings.HasPrefix(args.Path, ".") && !strings.HasPrefix(args.Path, "/") {
				// A bare specifier is a dependency installed when the extension
				// is saved, so it is left external on purpose.
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			}
			if resolved, ok := matchTreePath(files, path.Join(path.Dir(args.Importer), args.Path)); ok {
				return api.OnResolveResult{Path: resolved, Namespace: virtualNamespace}, nil
			}
			// Not found: falling through lets esbuild report the import position
			// itself, which is more useful than an error raised here.
			return api.OnResolveResult{}, nil
		})
		build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: virtualNamespace}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
			data, ok := files[args.Path]
			if !ok {
				return api.OnLoadResult{}, nil
			}
			return api.OnLoadResult{Contents: &data, Loader: loaderFor(args.Path)}, nil
		})
	}
}

// CheckFile reports the problems of a single file, for a draft that is not part
// of a tree yet.
func CheckFile(filePath, source string) []CheckDiagnostic {
	result := api.Transform(source, api.TransformOptions{
		Loader: loaderFor(filePath), Sourcemap: api.SourceMapNone, Charset: api.CharsetUTF8,
		LogLevel: api.LogLevelSilent, Sourcefile: filePath,
	})
	diagnostics := make([]CheckDiagnostic, 0, len(result.Errors))
	for _, message := range result.Errors {
		diagnostics = append(diagnostics, checkDiagnostic(message, "error"))
	}
	return diagnostics
}

func checkDiagnostic(message api.Message, severity string) CheckDiagnostic {
	diagnostic := CheckDiagnostic{Severity: severity, Message: message.Text, Path: EntryFile}
	if message.Location != nil {
		diagnostic.Path = checkFilePath(message.Location.File)
		diagnostic.Line = message.Location.Line
		// esbuild columns are 0-based, the editor expects 1-based.
		diagnostic.Column = message.Location.Column + 1
		diagnostic.Length = message.Location.Length
	}
	if diagnostic.Path == "" || diagnostic.Path == "<stdin>" || diagnostic.Path == "<stdout>" {
		diagnostic.Path = EntryFile
	}
	return diagnostic
}

// checkFilePath strips the plugin namespace and the virtual working directory
// from a reported location so the editor can match it against a tree key.
func checkFilePath(location string) string {
	name := strings.TrimPrefix(location, virtualNamespace+":")
	if name == "" {
		return EntryFile
	}
	if index := strings.LastIndex(name, virtualWorkingDir+"/"); index >= 0 {
		name = name[index+len(virtualWorkingDir)+1:]
	}
	return strings.TrimPrefix(name, "/")
}

func loaderFor(name string) api.Loader {
	if path.Ext(name) == ".json" {
		return api.LoaderJSON
	}
	return api.LoaderJS
}

// matchTreePath resolves a candidate to a tree entry, trying the file itself and
// then the index and extension conventions before giving up.
func matchTreePath(files map[string]string, candidate string) (string, bool) {
	cleaned := strings.TrimPrefix(path.Clean(candidate), "./")
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "", false
	}
	if files == nil {
		return cleaned, true
	}
	if _, ok := files[cleaned]; ok {
		return cleaned, true
	}
	for _, suffix := range []string{"/index.js", ".js", ".json"} {
		if _, ok := files[cleaned+suffix]; ok {
			return cleaned + suffix, true
		}
	}
	return "", false
}
