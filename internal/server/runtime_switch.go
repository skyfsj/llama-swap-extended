package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

// restartLockApplyWaitTimeout bounds how long a runtime switch hook waits for
// a config apply to finish before giving up. Package-level so tests can
// shorten it.
var restartLockApplyWaitTimeout = 5 * time.Minute

// switchRuntimeProcesses is installed as Runtime Manager's post-pointer hook.
// It intentionally reuses the existing router configuration/restart lifecycle:
// Reconfigure fences new requests, FIFO drains the old generation, and the
// router recreates the old explicit version if a candidate fails. The manager
// invokes this hook a second time with reversed versions after pointer rollback
// so the private process snapshot converges with durable current again.
func (s *Server) switchRuntimeProcesses(ctx context.Context, runtimeName, from, to string) error {
	if s == nil {
		return errors.New("server is nil")
	}
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return errors.New("runtime name is required")
	}
	if runtimeName == config.LMCacheRuntimeName {
		// The derived server runtime has no model processes to recreate; the
		// hook (re)starts the standalone server from the version the pointer
		// now names, or restores the previous serving state on compensation.
		return s.lmcacheSwitchAfter(ctx, from, to)
	}
	if strings.TrimSpace(from) == strings.TrimSpace(to) {
		return nil
	}
	operation := func() error {
		cfg := s.currentConfig()
		if _, ok := cfg.Runtimes[runtimeName]; !ok {
			return fmt.Errorf("runtime %q is no longer configured", runtimeName)
		}

		versions := map[string]string(nil)
		if target := strings.TrimSpace(to); target != "" {
			versions = map[string]string{runtimeName: target}
		}
		bound, err := bindManagedRuntimeLaunchConfigForVersions(cfg, versions, s.hardware)
		if err != nil {
			return fmt.Errorf("bind runtime %q version %q: %w", runtimeName, to, err)
		}
		local, ok := s.local.(interface{ Reconfigure(config.Config) error })
		if !ok {
			return errors.New("local router cannot reconfigure runtime processes")
		}
		if err := local.Reconfigure(bound); err != nil {
			return fmt.Errorf("publish runtime %q process binding: %w", runtimeName, err)
		}

		restarter, ok := s.local.(interface{ RestartModel(string) error })
		if !ok {
			return errors.New("local router cannot restart runtime processes")
		}
		for _, modelID := range runtimeReadyModels(cfg, s.local.RunningModels(), runtimeName) {
			err := restarter.RestartModel(modelID)
			if err != nil && !errors.Is(err, scheduler.ErrRestartNotPending) {
				return fmt.Errorf("restart model %q for runtime %q: %w", modelID, runtimeName, err)
			}
			if errors.Is(err, scheduler.ErrRestartNotPending) {
				// A failed candidate restart may already have recreated this exact
				// old version before Manager invokes its compensation hook. In that
				// case Reconfigure has no pending change and there is nothing left
				// to restart.
				continue
			}
			if err := s.waitRuntimeModelRestart(ctx, modelID); err != nil {
				return fmt.Errorf("restart model %q for runtime %q: %w", modelID, runtimeName, err)
			}
		}
		return nil
	}
	if s.configReconciler == nil {
		return operation()
	}
	// Bound the wait for a config apply to finish. The caller's context is
	// typically the manager's shutdown context without a deadline, and a
	// config apply can itself block on the manager lock from inside its apply
	// callback (e.g. the LMCache module sync staging a server runtime) while
	// this hook — running under that same manager lock inside Activate —
	// waits here. Without a bound that pair is a permanent deadlock; with
	// one, the activation fails, compensation restores the old pointer, and
	// the apply completes so the next automatic pass can retry.
	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, restartLockApplyWaitTimeout)
		defer cancel()
	}
	return s.configReconciler.WithRestartLock(waitCtx, operation)
}

func runtimeReadyModels(cfg config.Config, running map[string]process.ProcessState, runtimeName string) []string {
	models := make([]string, 0)
	for modelID, model := range cfg.Models {
		if strings.TrimSpace(model.Backend.Runtime) != runtimeName {
			continue
		}
		// A pinned model keeps its own version regardless of the current
		// pointer, so the switch changes nothing for it: no restart.
		if strings.TrimSpace(model.Backend.RuntimeVersion) != "" {
			continue
		}
		if running[modelID] == process.StateReady {
			models = append(models, modelID)
		}
	}
	sort.Strings(models)
	return models
}

func (s *Server) waitRuntimeModelRestart(ctx context.Context, modelID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	statuses, hasStatuses := s.local.(interface {
		ModelLifecycleStatuses() map[string]scheduler.ModelLifecycleStatus
	})
	if !hasStatuses {
		return errors.New("local router cannot report model restart status")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, pending := statuses.ModelLifecycleStatuses()[modelID]
		if !pending {
			if running := s.local.RunningModels(); running[modelID] == process.StateReady {
				return nil
			}
			return errors.New("model restart completed without a ready process")
		}
		if status.ConfigStatus == scheduler.ConfigStatusApplyFailed {
			if status.Error != "" {
				return errors.New(status.Error)
			}
			return errors.New("model restart failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
