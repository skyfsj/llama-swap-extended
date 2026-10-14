package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/hw"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// This file wires power statistics together:
//
//   - resolveModelGPUs maps a model to the GPU cards it is configured to
//     occupy (launch.gpus for native processes, container.gpus for containers)
//     so energy can be attributed without querying inside the inference
//     process.
//   - powerOccupancyTracker mirrors running-model state into persistent
//     occupancy intervals; the metrics middleware already records requests,
//     so no request-path code changes.
//   - runPowerFlusher persists aggregated power samples from the perf
//     monitor's ring buffers, which are memory-only and otherwise lost.
//
// The daemon restart gap is deliberate: a model that was running before a
// restart gets its occupancy interval opened when the tracker first sees it
// running again, and sample coverage resumes from the new process.

// powerTrackerDebounce coalesces status-change bursts (start/stop storms,
// swap chains) into one reconcile pass.
const powerTrackerDebounce = time.Second

// powerSampleGapGuard skips integrating across sample gaps longer than this,
// so missing data is never extrapolated as consumption.
const powerSampleGapGuard = 3 * time.Minute

// gpuStatKey mirrors perf's energy-accumulator keying: a UUID when the
// backend reports one, the index otherwise. Occupancy intervals and power
// sample rows share this key space.
func gpuStatKey(id int, uuid string) string {
	if trimmed := strings.TrimSpace(uuid); trimmed != "" {
		return trimmed
	}
	return fmt.Sprintf("index-%d", id)
}

// resolveModelGPUs returns the GPU keys a model is configured to occupy. An
// empty result means attribution is not possible (no GPU configured, a
// non-NVIDIA card, or a value that does not resolve against the hardware
// snapshot); the caller logs that once per model.
func resolveModelGPUs(cfg config.Config, hardware *hw.HardwareSnapshot, modelID string) []string {
	modelConfig, ok := cfg.Models[modelID]
	if !ok {
		return nil
	}

	var selections []string
	// CUDA_VISIBLE_DEVICES in the model environment is the single persisted GPU
	// selection; a legacy launch block is folded into it at config load. Accept
	// the block directly too, for configs built in memory without the loader.
	selections = append(selections, config.CUDAVisibleDevicesFromEnvironment(modelConfig.Env)...)
	if launch := modelConfig.Backend.Launch; launch != nil {
		selections = append(selections, launch.GPUs...)
	}
	if containerGPUs := strings.TrimSpace(modelConfig.Backend.Container.GPUs); containerGPUs != "" {
		selections = append(selections, parseContainerGPUSelections(containerGPUs, hardware)...)
	}

	keys := make([]string, 0, len(selections))
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		key := resolveGPUSelectionKey(hardware, selection)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// resolveGPUSelectionKey maps one configured GPU selection (a card UUID or an
// index) to the stable stat key used by power rows. Selections that do not
// resolve against the snapshot are dropped.
func resolveGPUSelectionKey(hardware *hw.HardwareSnapshot, selection string) string {
	selection = strings.TrimSpace(selection)
	if selection == "" {
		return ""
	}
	if hardware == nil {
		// Without a snapshot the configured value is trusted verbatim.
		return selection
	}
	accelerator := findLaunchAccelerator(hardware, selection)
	if accelerator == nil {
		return ""
	}
	if accelerator.UUID != nil && strings.TrimSpace(*accelerator.UUID) != "" {
		return *accelerator.UUID
	}
	return gpuStatKey(accelerator.Index, "")
}

// parseContainerGPUSelections interprets a docker --gpus value into
// per-card selections. "all" expands to every NVIDIA card in the snapshot;
// "device=0,1" and bare lists pass through as UUID-or-index selections.
func parseContainerGPUSelections(value string, hardware *hw.HardwareSnapshot) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.EqualFold(value, "all") {
		all := []string{}
		if hardware != nil {
			for index := range hardware.Accelerators {
				accelerator := &hardware.Accelerators[index]
				if accelerator.Vendor == nil || !strings.EqualFold(*accelerator.Vendor, "NVIDIA") {
					continue
				}
				if accelerator.UUID != nil && strings.TrimSpace(*accelerator.UUID) != "" {
					all = append(all, *accelerator.UUID)
					continue
				}
				// Index-only cards must still be attributed (and guarded by
				// the thermal monitor); fall back to their stat key form.
				all = append(all, gpuStatKey(accelerator.Index, ""))
			}
		}
		return all
	}
	value = strings.TrimPrefix(value, "device=")
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// powerOccupancyTracker mirrors running-model state into persistent per-GPU
// occupancy intervals. Intervals open when a model is first seen running and
// close when it is no longer running, including at shutdown.
type powerOccupancyTracker struct {
	mu       sync.Mutex
	timer    *time.Timer
	occupied map[string]map[string]struct{}
	warned   map[string]struct{}
}

func newPowerOccupancyTracker() *powerOccupancyTracker {
	return &powerOccupancyTracker{
		occupied: make(map[string]map[string]struct{}),
		warned:   make(map[string]struct{}),
	}
}

// scheduleReconcile coalesces status-change notifications into one pass.
func (t *powerOccupancyTracker) scheduleReconcile(s *Server) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		return
	}
	t.timer = time.AfterFunc(powerTrackerDebounce, func() {
		t.mu.Lock()
		t.timer = nil
		t.mu.Unlock()
		t.reconcile(s)
	})
}

// reconcile aligns open occupancy intervals with the currently running
// models. It is safe to call repeatedly; the flusher also calls it every
// tick as a safety net for missed events.
func (t *powerOccupancyTracker) reconcile(s *Server) {
	if s == nil || s.store == nil || s.local == nil {
		return
	}
	running := s.local.RunningModels()
	cfg := s.currentConfig()
	now := time.Now()
	ctx := s.shutdownCtx

	t.mu.Lock()
	defer t.mu.Unlock()

	for modelID := range running {
		if _, tracked := t.occupied[modelID]; tracked {
			continue
		}
		keys := resolveModelGPUs(cfg, s.hardware, modelID)
		if len(keys) == 0 {
			if _, warned := t.warned[modelID]; !warned {
				t.warned[modelID] = struct{}{}
				if s.proxylog != nil {
					s.proxylog.Warnf("power stats: model %q has no resolvable GPU configuration, skipping energy attribution", modelID)
				}
			}
			continue
		}
		delete(t.warned, modelID)
		opened := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			if err := s.store.OpenModelGpuInterval(ctx, store.ModelGpuIntervalRow{
				ModelID: modelID,
				GpuUUID: key,
				Started: now,
			}); err != nil {
				if s.proxylog != nil {
					s.proxylog.Warnf("power stats: open occupancy interval for %q on %s: %v", modelID, key, err)
				}
				continue
			}
			opened[key] = struct{}{}
		}
		if len(opened) > 0 {
			t.occupied[modelID] = opened
		}
	}

	for modelID := range t.occupied {
		if _, stillRunning := running[modelID]; stillRunning {
			continue
		}
		if _, err := s.store.CloseModelGpuIntervals(ctx, modelID, now); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("power stats: close occupancy intervals for %q: %v", modelID, err)
		}
		delete(t.occupied, modelID)
	}
}

// closeAll closes every open interval; used at shutdown so a restart does not
// inherit stale open intervals. The shutdownCtx is already cancelled when the
// flusher reaches this path, so the update runs on a non-cancellable context.
func (t *powerOccupancyTracker) closeAll(s *Server) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s == nil || s.store == nil || len(t.occupied) == 0 {
		return
	}
	closeCtx := context.WithoutCancel(s.shutdownCtx)
	if _, err := s.store.CloseModelGpuIntervals(closeCtx, "", time.Now()); err != nil && s.proxylog != nil {
		s.proxylog.Warnf("power stats: close occupancy intervals at shutdown: %v", err)
	}
	t.occupied = make(map[string]map[string]struct{})
}

// runPowerFlusher persists aggregated power samples from the perf monitor's
// ring buffers and maintains the occupancy tracker. Samples arrive on the
// perf monitor's cadence (default 5s); rows are written once per flush
// interval (default 60s) with the tariff price resolved at write time, so
// tariff edits never rewrite history.
func (s *Server) runPowerFlusher() {
	if s == nil || s.shutdownCtx == nil || s.store == nil || s.perf == nil {
		return
	}
	if s.currentConfig().Power.Disabled {
		return
	}
	flushInterval := s.currentConfig().Power.FlushInterval

	unsubscribe := event.On(func(e swaputil.ModelStatusesChangedEvent) {
		s.powerTracker.scheduleReconcile(s)
	})
	defer unsubscribe()

	// Close intervals left open by a crashed predecessor before reconciling:
	// an open row stretches its model's attribution coverage to every query,
	// so a stale sweep at startup is mandatory. Models that are genuinely
	// still running get fresh intervals from the reconcile below.
	if closed, err := s.store.CloseStaleModelGpuIntervals(s.shutdownCtx, time.Now()); err != nil {
		if s.proxylog != nil {
			s.proxylog.Warnf("power stats: stale occupancy sweep: %v", err)
		}
	} else if closed > 0 && s.proxylog != nil {
		s.proxylog.Infof("power stats: closed %d occupancy interval(s) left open by a previous process", closed)
	}

	// Catch models that are already running (preloaded, or started before the
	// flusher came up).
	s.powerTracker.reconcile(s)

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	cursors := newPowerCursors()
	cursors.arm(time.Now(), flushInterval)
	lastPrune := time.Now()
	for {
		select {
		case <-s.shutdownCtx.Done():
			flushCtx := context.WithoutCancel(s.shutdownCtx)
			s.flushPowerSamples(flushCtx, cursors)
			s.powerTracker.closeAll(s)
			return
		case <-ticker.C:
			// Pick up reload changes each tick: a disabled flag stops the
			// loop and a new flush interval resizes the ticker, so neither
			// setting goes stale for the process lifetime.
			power := s.currentConfig().Power
			if power.Disabled {
				return
			}
			if power.FlushInterval != flushInterval {
				flushInterval = power.FlushInterval
				ticker.Reset(flushInterval)
			}

			// Reconcile every tick too: it repairs any missed status event and
			// picks up GPU mapping changes from config reloads.
			s.powerTracker.reconcile(s)

			flushCtx, cancel := context.WithTimeout(s.shutdownCtx, 30*time.Second)
			s.flushPowerSamples(flushCtx, cursors)
			cancel()

			if time.Since(lastPrune) >= 24*time.Hour {
				lastPrune = time.Now()
				retention := s.currentConfig().Power.RetentionDays
				pruneCtx, pruneCancel := context.WithTimeout(s.shutdownCtx, time.Minute)
				if err := s.store.PrunePowerData(pruneCtx, time.Now().AddDate(0, 0, -retention)); err != nil && s.proxylog != nil {
					s.proxylog.Warnf("power stats: prune: %v", err)
				}
				pruneCancel()
			}
		}
	}
}

// flushPowerSamples aggregates every sample newer than each source's own
// cursor into one row per source and advances that source's cursor to its
// newest sample timestamp. Per-source cursors are required because sources
// sample at different cadences (GPU ~5s, IPMI 30s): a single global cursor
// set by the fastest source makes slower sources systematically lose the span
// between their own last sample and the cursor on every window. A source's
// cursor only advances after a successful insert, so a failed flush retries
// with that source's full window next tick.
func (s *Server) flushPowerSamples(ctx context.Context, cursors *powerCursors) {
	if s.perf == nil {
		return
	}
	_, gpuStats, nodeStats := s.perf.Current()

	rows := aggregatePowerRows(gpuStats, nodeStats, cursors, s.currentConfig().Power)
	if len(rows) == 0 {
		return
	}
	if err := s.store.InsertPowerSamples(ctx, rows); err != nil {
		if s.proxylog != nil {
			s.proxylog.Warnf("power stats: insert samples: %v", err)
		}
		return
	}
	for _, row := range rows {
		cursors.set(row.SourceID, row.End)
	}
}

// powerCursors holds the last-integrated timestamp per source key ("node" or
// a GPU stat key).
type powerCursors struct {
	mu       sync.Mutex
	byKey    map[string]time.Time
	defaults map[string]time.Time
}

func newPowerCursors() *powerCursors {
	return &powerCursors{byKey: map[string]time.Time{}, defaults: map[string]time.Time{}}
}

// arm seeds a source's cursor at process start: the flush interval lookback
// bounds how far back the first window reaches when no cursor exists yet.
func (c *powerCursors) arm(now time.Time, lookback time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaults["*"] = now.Add(-lookback)
}

func (c *powerCursors) get(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ts, ok := c.byKey[key]; ok {
		return ts
	}
	if ts, ok := c.defaults["*"]; ok {
		return ts
	}
	return time.Time{}
}

func (c *powerCursors) set(key string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[key] = ts
}

// aggregatePowerRows folds raw samples into one PowerSampleRow per source.
// Each source integrates from its own cursor: the window's first sample
// covers [cursor, ts), so consecutive windows stay contiguous per source.
// Samples at or before the cursor were already integrated by an earlier
// flush — skipping them keeps both the energy sum and the cursor
// continuation exact.
func aggregatePowerRows(gpuStats []perf.GpuStat, nodeStats []perf.NodeStat, cursors *powerCursors, power config.PowerConfig) []store.PowerSampleRow {
	type accumulation struct {
		sourceName     string
		cursor         time.Time
		last           time.Time
		previous       time.Time
		energyWh       float64
		integratedSecs float64
		minWatts       float64
		maxWatts       float64
		samples        int
	}
	accumulations := map[string]*accumulation{}
	accumulate := func(key string, name string, ts time.Time, watts float64) {
		cursor := cursors.get(key)
		// The perf ring holds about an hour of samples but the flush window
		// is one flushInterval wide. Samples at or before the cursor were
		// already integrated by an earlier flush.
		if !ts.After(cursor) {
			return
		}
		acc := accumulations[key]
		if acc == nil {
			acc = &accumulation{
				sourceName: name,
				cursor:     cursor,
				last:       ts,
				previous:   cursor,
				minWatts:   -1,
			}
			accumulations[key] = acc
		}
		if watts <= 0 {
			return
		}
		// Rectangular integration from the previous sample; the window's
		// first sample covers [cursor, ts). The gap guard skips integrating
		// across longer outages so missing data is never extrapolated; the
		// skipped seconds are excluded from the average's denominator too,
		// keeping AvgWatts a true sample-weighted mean.
		if gap := ts.Sub(acc.previous); gap > 0 {
			if gap <= powerSampleGapGuard {
				acc.energyWh += watts * gap.Hours()
				acc.integratedSecs += gap.Seconds()
			}
		}
		acc.previous = ts
		if ts.After(acc.last) {
			acc.last = ts
		}
		acc.samples++
		if acc.minWatts < 0 || watts < acc.minWatts {
			acc.minWatts = watts
		}
		if watts > acc.maxWatts {
			acc.maxWatts = watts
		}
	}

	for _, stat := range gpuStats {
		accumulate(gpuStatKey(stat.ID, stat.UUID), stat.Name, stat.Timestamp, stat.PowerDrawW)
	}
	for _, stat := range nodeStats {
		accumulate("node", "", stat.Timestamp, stat.PowerW)
	}

	rows := make([]store.PowerSampleRow, 0, len(accumulations))
	for key, acc := range accumulations {
		if acc.samples == 0 {
			continue
		}
		if acc.integratedSecs <= 0 {
			continue
		}
		source := "gpu"
		if key == "node" {
			source = "node"
		}
		price, tariffName := power.TariffPriceFor(acc.cursor)
		rows = append(rows, store.PowerSampleRow{
			Start:       acc.cursor,
			End:         acc.last,
			Source:      source,
			SourceID:    key,
			SourceName:  acc.sourceName,
			AvgWatts:    acc.energyWh / (acc.integratedSecs / 3600),
			MinWatts:    acc.minWatts,
			MaxWatts:    acc.maxWatts,
			Samples:     acc.samples,
			EnergyWh:    acc.energyWh,
			PricePerKwh: price,
			TariffName:  tariffName,
		})
	}
	return rows
}

// powerSourceStatus reports which power collectors have data, for the API
// response and the panel's source badges.
func (s *Server) powerSourceStatus() (gpu bool, ipmi bool) {
	if s.perf == nil {
		return false, false
	}
	_, gpuStats, _ := s.perf.Current()
	for _, stat := range gpuStats {
		if stat.PowerDrawW > 0 {
			gpu = true
			break
		}
	}
	return gpu, s.perf.NodePowerAvailable()
}
