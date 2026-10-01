package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	runtimeManager "github.com/mostlygeek/llama-swap/internal/runtime"
)

// lmcacheSwitchState records what the pre-activation hook observed so the
// post-activation hook can restore the right serving state: whether the
// server was running before the switch and which version was current then.
// `set` marks that the pre-activation hook actually ran for this switch:
// the manager skips it for an idempotent re-activation of the current
// version, in which case the post hook must not touch the process.
type lmcacheSwitchState struct {
	set        bool
	wasRunning bool
	preVersion string
}

// switchRuntimeBefore is the Runtime Manager's pre-activation hook, invoked
// for every runtime switch. Only the derived lmcache runtime needs process
// coordination — refuse the switch while models depend on the server, stop
// the serving process so the activation health probe can bind its ports,
// and park the state machine in UPDATING. Model runtimes are no-ops here;
// their processes are restarted by the post-activation hook.
func (s *Server) switchRuntimeBefore(ctx context.Context, runtimeName, from, to string) error {
	if runtimeName != config.LMCacheRuntimeName {
		return nil
	}
	if s.lmcacheMod == nil {
		return nil
	}
	return s.lmcacheMod.beforeRuntimeSwitch(from)
}

// beforeRuntimeSwitch guards a pointer switch of the lmcache runtime. It is
// the single dependency guard shared by every update path (automatic loop
// and explicit API operations): while any model still uses the server the
// activation is refused with the model list, so an in-flight KV cache can
// never be dropped by a version switch. A passed guard stops the running
// server and marks the module UPDATING until the post-activation hook
// restores service on the winning version.
func (svc *lmcacheService) beforeRuntimeSwitch(from string) error {
	cfg := svc.lifecycleConfig()
	if err := svc.requireNoUsers(cfg); err != nil {
		return err
	}
	proc := svc.proc.Status()
	svc.switchMu.Lock()
	svc.switchState = lmcacheSwitchState{set: true, wasRunning: proc.Running, preVersion: from}
	svc.switchMu.Unlock()
	if proc.Running {
		if err := svc.stopServer(); err != nil {
			return fmt.Errorf("stop lmcache server before version switch: %w", err)
		}
	}
	// UPDATING outlives the stopped process: a crash between stop and
	// restart must not masquerade as a healthy STOPPED server.
	svc.proc.setExternalState(LMCacheStateUpdating)
	return nil
}

// lmcacheSwitchAfter is the post-pointer hook for the derived runtime,
// reached on success, on compensation after a failed activation, and on an
// explicit rollback. `to` is the version that must serve afterwards: the
// new version on success, the restored old version on compensation. The
// server is (re)started from that version's venv so the running process
// always matches the durable pointer; a failed restart is an explicit error
// and leaves the old version intact (fail-closed, no half upgrade).
func (s *Server) lmcacheSwitchAfter(ctx context.Context, from, to string) error {
	if s.lmcacheMod == nil {
		return nil
	}
	return s.lmcacheMod.afterRuntimeSwitch(ctx, from, to)
}

func (svc *lmcacheService) afterRuntimeSwitch(ctx context.Context, from, to string) error {
	svc.switchMu.Lock()
	pre := svc.switchState
	svc.switchMu.Unlock()
	if !pre.set {
		// The pre-activation hook never ran for this switch: the manager
		// skips it for an idempotent re-activation of the current version,
		// so the pointer change took the server neither down nor onto a
		// new version — the serving process is untouched.
		return nil
	}
	if to == pre.preVersion && !pre.wasRunning {
		// Compensation for a switch that never took the server down: the
		// old version is back in charge and the server stays stopped.
		svc.switchMu.Lock()
		svc.switchState = lmcacheSwitchState{}
		svc.switchMu.Unlock()
		return nil
	}
	cfg := svc.lifecycleConfig()
	if !cfg.LMCache.Server.Enabled {
		// Pointer changes remain durable while the server is disabled, but a
		// version operation must not surprise the operator by starting it.
		svc.proc.setExternalState(LMCacheStateStopped)
		svc.switchMu.Lock()
		svc.switchState = lmcacheSwitchState{}
		svc.switchMu.Unlock()
		return nil
	}
	if err := svc.startServerAtVersion(ctx, cfg, to); err != nil {
		// Keep the pre-switch snapshot for Manager's compensation hook. If the
		// candidate failed its real health check, compensation invokes this
		// method again with the old pointer and restarts that exact version.
		return err
	}
	svc.switchMu.Lock()
	svc.switchState = lmcacheSwitchState{}
	svc.switchMu.Unlock()
	return nil
}

// resolveServerExecutable finds the lmcache console script in the dedicated
// server runtime. An empty version follows the current pointer; a concrete
// version must be an installed one — the update hooks and the model
// runtimeVersion pin both fail closed on a missing version. The server never
// runs from a vLLM venv: the two dependency trees are isolated by design.
func resolveServerExecutable(cfg config.Config, version string) (string, string, error) {
	root := managedRuntimeRoot(cfg)
	var (
		binding runtimeManager.LaunchBinding
		found   bool
		err     error
	)
	if version == "" {
		binding, found, err = runtimeManager.CurrentVersionLaunchBinding(root, config.LMCacheRuntimeName, "lmcache", "lmcache")
		if err == nil && !found {
			return "", "", errors.New("lmcache server runtime is not staged; enable the LMCache module first")
		}
	} else {
		binding, err = runtimeManager.LaunchBindingForVersion(root, config.LMCacheRuntimeName, "lmcache", "lmcache", version)
		if err != nil {
			return "", "", fmt.Errorf("lmcache server runtime version %s is not installed: %w", version, err)
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve lmcache server runtime: %w", err)
	}
	if info, statErr := os.Stat(binding.Executable); statErr != nil || !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("lmcache executable not found in the dedicated server venv; enable the LMCache module first")
	}
	workDir := filepath.Dir(binding.BinDir)
	return binding.Executable, workDir, nil
}

// startServerAtVersion is startServer with the executable pinned to an
// installed version instead of the current pointer. It is used by the
// post-activation hooks so the serving process always starts from the
// version the pointer names, including the compensated rollback to the old
// one.
func (svc *lmcacheService) startServerAtVersion(ctx context.Context, cfg config.Config, version string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	eff := cfg.LMCache.Server.Effective()
	executable, workDir, err := resolveServerExecutable(cfg, version)
	if err != nil {
		svc.proc.recordStartFailure(err.Error())
		return err
	}
	args, err := lmcacheServerArgs(eff)
	if err != nil {
		svc.proc.recordStartFailure(err.Error())
		svc.publish("error", 0, "", err.Error())
		return err
	}
	if err := svc.prepareL3Runtime(eff); err != nil {
		svc.proc.recordStartFailure(err.Error())
		svc.publish("error", 0, "", err.Error())
		return err
	}
	logPath := filepath.Join(managedRuntimeRoot(cfg), lmcacheStateDir, "server.log")
	if stopErr := svc.stopServer(); stopErr != nil && svc.srv.proxylog != nil {
		svc.srv.proxylog.Warnf("lmcache server pre-start stop: %v", stopErr)
	}
	svc.publish("starting", 0.5, fmt.Sprintf("starting lmcache server from the dedicated server venv (version %s)", version), "")
	healthURL := fmt.Sprintf("http://%s:%d/healthcheck", eff.HTTPHost, eff.HTTPPort)
	if startErr := svc.proc.Start(executable, args, workDir, logPath, healthURL); startErr != nil {
		svc.proc.recordStartFailure(startErr.Error())
		svc.publish("error", 0, "", startErr.Error())
		return startErr
	}
	// The health watcher owns only the process state; settle the progress
	// stream here so the UI's bar does not stay pinned at this "starting"
	// phase after the version switch completes or fails.
	if err := svc.waitForServerSettled(ctx); err != nil {
		svc.publish("error", 0, "", tailOutput(err.Error(), lmcacheOutputTail))
		return err
	}
	svc.publish("idle", 1, "lmcache server running", "")
	return nil
}

// protectedRuntimeVersion answers the Runtime Manager's deletion-protection
// probe: does the server side hold a reference to this version that the
// manager's own state (current/previous/pinned pointers) cannot see? A
// protected version is kept on disk for the next prune pass.
func (s *Server) protectedRuntimeVersion(name, version string) (bool, string) {
	cfg := s.currentConfig()
	if name == config.LMCacheRuntimeName {
		if s.lmcacheMod == nil {
			return false, ""
		}
		if s.lmcacheMod.proc.Status().State == LMCacheStateUpdating {
			return true, "lmcache update in progress"
		}
		if models := s.lmcacheMod.inUseModels(cfg); len(models) > 0 {
			return true, "in use by models: " + strings.Join(models, ", ")
		}
		return false, ""
	}
	// Model runtimes (vllm, llama.cpp): a version that a model config pins
	// must never be pruned, even while no model is running.
	var pinners []string
	for id, model := range cfg.Models {
		if model.Backend.Runtime == name && strings.TrimSpace(model.Backend.RuntimeVersion) == version {
			pinners = append(pinners, id)
		}
	}
	if len(pinners) > 0 {
		sort.Strings(pinners)
		return true, "pinned by model: " + strings.Join(pinners, ", ")
	}
	return false, ""
}
