//go:build linux

package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func attachExtensionCgroup(pid, memoryMiB int) func() {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return func() {}
	}
	var relative string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			relative = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if relative == "" {
		return func() {}
	}
	root := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(relative, "/"))
	dir := filepath.Join(root, fmt.Sprintf("llama-swap-ext-%d", pid))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return func() {}
	}
	cleanup := func() { _ = os.Remove(dir) }
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(fmt.Sprintf("%d", memoryMiB<<20)), 0o600); err != nil {
		cleanup()
		return func() {}
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte("32"), 0o600); err != nil {
		cleanup()
		return func() {}
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(fmt.Sprintf("%d", pid)), 0o600); err != nil {
		cleanup()
		return func() {}
	}
	return cleanup
}
