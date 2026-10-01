package runtime

import (
	"context"
	"strings"
	"testing"
)

// The dedicated server venv is launched through the lmcache console script,
// whose import chain requires packages the distribution metadata does not
// declare. Stage must therefore install the CLI dependencies alongside the
// lmcache requirement.
func TestRuntime_LMCacheStageInstallsCLIDependencies(t *testing.T) {
	destination := t.TempDir()
	var calls []string
	probe := []byte(`{"importable": true, "lmcache": "0.5.4", "pythonVersion": "3.11.16"}` + "\n")
	fake := func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if strings.HasSuffix(name, "python") && len(args) > 0 && args[0] == "-c" {
			return probe, nil
		}
		return []byte("ok"), nil
	}
	provider := LMCacheProvider{
		Run: fake,
		RunEnv: func(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, error) {
			return fake(ctx, dir, name, args...)
		},
	}
	spec := Spec{
		Name: "lmcache", Kind: "lmcache", Mode: RuntimeModeNative,
		SourceType: "pypi", Source: "pypi", Version: "0.5.4",
		BuildArgs: map[string][]string{"package": {"lmcache"}, "python": {"3.11"}},
	}
	if _, err := provider.Stage(context.Background(), spec, destination); err != nil {
		t.Fatalf("Stage() = %v", err)
	}
	install := ""
	for _, call := range calls {
		if strings.Contains(call, "pip install") {
			install = call
		}
	}
	for _, want := range []string{"lmcache==0.5.4", "openai"} {
		if !strings.Contains(install, want) {
			t.Fatalf("pip install call %q missing %q; calls: %v", install, want, calls)
		}
	}
}
