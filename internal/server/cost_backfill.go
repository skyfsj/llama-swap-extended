package server

import (
	"context"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store"
)

// costBackfillPassTimeout bounds one full backfill pass so a pathologically
// large backlog cannot keep a goroutine alive past a reasonable maintenance
// window. The cursor is persisted per batch, so an expired pass resumes where
// it stopped on the next trigger.
const costBackfillPassTimeout = 30 * time.Minute

// startCostBackfill kicks off a background pass when the feature is enabled.
// Calls are cheap when a pass is already running (single-flight) or when the
// store is missing, so every successful pricing sync can fire it unconditionally.
func (s *Server) startCostBackfill() {
	if s == nil || s.store == nil {
		return
	}
	if !s.currentConfig().EffectivePricing().ModelsDev.CostBackfillEnabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.shutdownCtx, costBackfillPassTimeout)
		defer cancel()
		s.backfillEstimatedCosts(ctx)
	}()
}

// backfillEstimatedCosts prices the activity rows that were recorded before a
// price was available. Cost used to be computed once at record time and frozen
// at zero when the catalog had no entry yet, so the aggregated "estimated
// cost" column only reflected the few recent requests that happened to find a
// price — this pass recomputes the unpriced rows from the current catalog and
// persists the result, in resumable batches. Rows that still have no price are
// skipped (they keep the "—" display) and retried on the next pass, after the
// next sync potentially adds their catalog entry.
func (s *Server) backfillEstimatedCosts(ctx context.Context) int64 {
	if s == nil || s.store == nil || s.metrics == nil {
		return 0
	}
	if !s.costBackfillRunning.CompareAndSwap(false, true) {
		return 0
	}
	defer s.costBackfillRunning.Store(false)

	cursor, err := s.store.GetCostBackfillCursor(ctx)
	if err != nil && s.proxylog != nil {
		s.proxylog.Warnf("cost backfill: read cursor: %v", err)
		cursor = 0
	}
	var priced int64
	for {
		select {
		case <-ctx.Done():
			return priced
		default:
		}
		rows, err := s.store.NextUnpricedActivity(ctx, cursor, 1000)
		if err != nil {
			if s.proxylog != nil {
				s.proxylog.Warnf("cost backfill: read batch: %v", err)
			}
			return priced
		}
		if len(rows) == 0 {
			if err := s.store.ClearCostBackfillCursor(ctx); err != nil && s.proxylog != nil {
				s.proxylog.Warnf("cost backfill: clear cursor: %v", err)
			}
			return priced
		}
		updates := make([]store.CostBackfillUpdate, 0, len(rows))
		for _, row := range rows {
			cursor = row.ID
			price, ok := s.metrics.lookupPrice(row.Model, row.Provider)
			if !ok {
				continue
			}
			cost := estimatedCostUSD(price, row.ReqPath,
				row.Input, row.Cached, row.CacheCreation, row.Output, row.Reasoning)
			updates = append(updates, store.CostBackfillUpdate{
				ID:        row.ID,
				Cost:      cost,
				TsCreated: row.TsCreated,
				Model:     row.Model,
				KeyID:     row.KeyID,
				ReqPath:   row.ReqPath,
			})
		}
		if err := s.store.ApplyEstimatedCostBatch(ctx, updates); err != nil {
			if s.proxylog != nil {
				s.proxylog.Warnf("cost backfill: apply batch: %v", err)
			}
			return priced
		}
		priced += int64(len(updates))
		if err := s.store.SetCostBackfillCursor(ctx, cursor); err != nil && s.proxylog != nil {
			s.proxylog.Warnf("cost backfill: persist cursor: %v", err)
		}
	}
}

// costBackfillIdle reports whether no pass is in flight. Tests use it to wait
// for a triggered pass without polling the database.
func (s *Server) costBackfillIdle() bool {
	return s == nil || !s.costBackfillRunning.Load()
}
