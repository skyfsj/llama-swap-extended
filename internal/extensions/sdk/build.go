//go:build ignore

// Command sdk-build bundles internal/extensions/sdk/extension.ts into
// sdk/dist/extension.js with the vendored pure-Go esbuild port, so the built
// artifact is committed to the repository and `go build` alone produces a
// working binary. Run with: go run ./internal/extensions/sdk/build.go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	source := filepath.Join(root, "internal", "extensions", "sdk", "extension.ts")
	outDir := filepath.Join(root, "internal", "extensions", "sdk", "dist")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}
	// CJS: the extension bundle's `import ... from "@llama-swap/extension"`
	// becomes require_sdk() against this file, so module.exports must carry
	// the SDK functions.
	result := api.Build(api.BuildOptions{
		EntryPoints: []string{source},
		Outfile:     filepath.Join(outDir, "extension.js"),
		Bundle:      true,
		Write:       true,
		Platform:    api.PlatformNode,
		Format:      api.FormatCommonJS,
		Target:      api.ES2019,
		LogLevel:    api.LogLevelInfo,
	})
	if len(result.Errors) > 0 {
		for _, entry := range result.Errors {
			fmt.Fprintln(os.Stderr, entry.Text)
		}
		os.Exit(1)
	}
	if err := os.Chmod(filepath.Join(outDir, "extension.js"), 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("built internal/extensions/sdk/dist/extension.js")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
