package router

import (
	"context"
	"fmt"
	"slices"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

type Gpus struct {
	*baseRouter
}

func NewGpus(conf config.Config, proxylog, upstreamlog *logmon.Monitor) (*Gpus, error) {
	gpus := conf.Routing.Router.Settings.Gpus
	if gpus == nil {
		return nil, fmt.Errorf("gpus router requires a gpus configuration")
	}
	if err := config.ValidateGpus(gpus, conf.Models); err != nil {
		return nil, fmt.Errorf("validating gpus configuration: %w", err)
	}

	swapper := &gpusSwapper{
		gpus:   gpus,
		logger: proxylog,
	}

	// Build a process for every model in the config. Any model can run even
	// if it is not listed on a card; this mirrors NewMatrix.
	processes := make(map[string]process.Process, len(conf.Models))
	base, err := newBaseRouter("gpus", conf, processes, proxylog, swapper)
	if err != nil {
		return nil, fmt.Errorf("creating base router: %w", err)
	}
	base.setProcessFactory(func(ctx context.Context, modelID string, modelCfg config.ModelConfig) (process.Process, error) {
		return process.New(ctx, modelID, modelCfg, logmon.NewWriter(logmon.NewLinePrefixWriter("["+modelID+"] ", upstreamlog)), proxylog)
	})

	for mid, modelCfg := range conf.Models {
		procLog := logmon.NewWriter(logmon.NewLinePrefixWriter("["+mid+"] ", upstreamlog))
		p, err := process.New(base.procCtx, mid, modelCfg, procLog, proxylog)
		if err != nil {
			base.shutdownFn()
			base.procCancel()
			return nil, fmt.Errorf("creating process for %q: %w", mid, err)
		}
		processes[mid] = p
	}
	base.processMu.Lock()
	for mid := range processes {
		if modelCfg, ok := conf.Models[mid]; ok {
			base.processConfigs[mid] = modelCfg
			base.processRuntime[mid] = runtimeSnapshot(conf, modelCfg)
		}
	}
	base.processMu.Unlock()

	r := &Gpus{baseRouter: base}
	go base.run()
	return r, nil
}

// Reconfigure keeps the gpus router's process registry and scheduler alive
// while applying a new planner/topology through the base run loop. Candidate
// group and matrix routing is also accepted so an operator can switch router
// kinds without recreating the HTTP server.
func (r *Gpus) Reconfigure(conf config.Config) error {
	planner, modelIDs, err := plannerForConfig(conf, r.logger)
	if err != nil {
		return err
	}
	return r.baseRouter.Reconfigure(conf, planner, modelIDs)
}

// gpusSwapper decides evictions from per-card model occupancy: every running
// model that shares a card with the target is evicted, everything else is
// left running.
//
// The scheduler drives planners from a single event-loop goroutine and calls
// OnSwapStart with the same target and running set it just gave EvictionFor,
// so the last decision is cached and reused instead of recomputing per swap.
// The cache is only valid under that single-goroutine access pattern.
type gpusSwapper struct {
	gpus   *config.GpusConfig
	logger *logmon.Monitor

	lastTarget  string
	lastRunning []string
	lastEvict   []string
	lastValid   bool
}

func (p *gpusSwapper) solve(target string, running []string) []string {
	if p.lastValid && p.lastTarget == target && slices.Equal(p.lastRunning, running) {
		return p.lastEvict
	}
	var evict []string
	for _, model := range running {
		// The target never evicts itself: a ready target is part of the
		// running set, and SharesCard(model, model) is true, so without this
		// skip every request to an already-loaded model would "evict" — i.e.
		// stop and restart — its own backend.
		if model == target {
			continue
		}
		if p.gpus.SharesCard(model, target) {
			evict = append(evict, model)
		}
	}
	p.lastTarget = target
	p.lastRunning = slices.Clone(running)
	p.lastEvict = evict
	p.lastValid = true
	return evict
}

func (p *gpusSwapper) EvictionFor(target string, running []string) []string {
	return p.solve(target, running)
}

func (p *gpusSwapper) OnSwapStart(target string, running []string) {
	evict := p.solve(target, running)
	switch {
	case len(evict) > 0:
		p.logger.Infof("gpus: model=%s cards=%v evict=%v", target, p.gpus.CardsOf(target), evict)
	case len(running) == 0:
		p.logger.Infof("gpus: model=%s starting (no models running)", target)
	default:
		p.logger.Debugf("gpus: model=%s running alongside %v", target, running)
	}
}
