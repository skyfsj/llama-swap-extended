package process

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// DescribeExit renders how a managed process actually died.
//
// The raw Go error string is not enough to act on: "exit status 120" does not
// say whether the process chose that code, was killed by a signal, or ran out
// of memory. A pre-ready death is the single most common support question, so
// the lifecycle error must name the mechanism instead of forwarding the
// wrapper verbatim.
//
// The rendering is deliberately grammatical on its own ("killed by signal
// SIGKILL (9)") so callers can prefix it with context ("upstream command ...")
// without the sentence depending on words that follow.
func DescribeExit(err error) string {
	if err == nil {
		return "exited with code 0"
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err.Error()
	}
	return describeExitState(exitErr)
}

// DescribeExitWithMemory adds the memory picture observed at death. Memory
// exhaustion is the classic silent death — no traceback, no signal, and a
// process that simply stops writing output — so whenever such evidence is
// available it belongs next to the exit reason rather than requiring an
// operator to reconstruct it from cgroup counters after the fact.
//
// initiatedByLlamaSwap suppresses the OOM speculation. A SIGKILL we sent
// ourselves (start deadline, explicit stop, shutdown) is not evidence of
// memory pressure, and guessing "kernel OOM killer" there sends the reader
// chasing a memory problem that may not exist.
func DescribeExitWithMemory(err error, initiatedByLlamaSwap bool) string {
	description := DescribeExit(err)
	if initiatedByLlamaSwap {
		// Still surface hard memory evidence, but never the guess.
		if note := memoryKillNote(); note != "" {
			return fmt.Sprintf("%s (%s)", description, note)
		}
		return description
	}
	// A SIGKILL nobody here sent is, in practice, most often the kernel
	// reclaiming memory. Say so only on this branch: when llama-swap sent the
	// signal itself, that guess would be actively wrong.
	if strings.Contains(description, "SIGKILL") {
		description += " — a SIGKILL that llama-swap did not send is usually the kernel OOM killer"
	}
	if note := memoryPressureNote(); note != "" {
		return fmt.Sprintf("%s (%s)", description, note)
	}
	return description
}
