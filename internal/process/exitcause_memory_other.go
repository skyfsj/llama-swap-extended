//go:build !linux

package process

// memoryPressureNote returns nothing on platforms without the cgroup v2
// counters it reads on Linux; the exit reason alone still gets reported.
func memoryPressureNote() string { return "" }

// memoryKillNote returns nothing on platforms without cgroup v2 counters.
func memoryKillNote() string { return "" }
