//go:build !linux

package extensions

func attachExtensionCgroup(pid, memoryMiB int) func() { return func() {} }
