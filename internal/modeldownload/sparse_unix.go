//go:build !windows

package modeldownload

import (
	"os"
	"syscall"
)

// fileIsSparse reports whether path occupies fewer blocks than its size, which
// means it contains holes (unwritten ranges that read as NUL). A download part
// created by Truncate before any bytes were written is exactly that shape.
func fileIsSparse(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	// st_blocks counts 512-byte units on every platform Go supports.
	allocated := int64(stat.Blocks) * 512
	return allocated < info.Size(), nil
}
