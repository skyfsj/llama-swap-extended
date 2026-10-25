-- +goose Up
-- Every read of the power statistics filters power_samples on ts_end: the report
-- window, the calendar totals and the retention prune all do. Without an index on
-- that column each of them scans the table (or the whole source range) from the
-- beginning, so the energy panel gets slower the longer the server has been
-- collecting samples.
--
-- The shape puts source first so the existing per-source queries keep their
-- ordering, and ts_end second so the window predicate is a range rather than a
-- filter over every row of the source.
CREATE INDEX idx_power_samples_source_ts_end ON power_samples (source, ts_end);

-- The GPU energy totals group by source and filter on ts_end, which is exactly
-- this index's shape.
CREATE INDEX idx_model_gpu_intervals_ended_ts ON model_gpu_intervals (ended_ts);

-- The interval report asks for intervals that are open or that end after the
-- window start; neither the gpu nor the model index leads with ended_ts, so a
-- query without a model or gpu filter matched neither.
CREATE INDEX idx_model_gpu_intervals_started_ts ON model_gpu_intervals (started_ts);

-- +goose Down
DROP INDEX idx_model_gpu_intervals_started_ts;
DROP INDEX idx_model_gpu_intervals_ended_ts;
DROP INDEX idx_power_samples_source_ts_end;
