-- +goose Up
-- Every sortable column of the activity page needs an index whose shape matches
-- the ORDER BY the page emits: "<column> <dir>, id <dir>". Without it, SQLite
-- has to sort the filtered rows before it can apply LIMIT/OFFSET, so paging or
-- sorting a large table costs a full sort on every request — the reason a
-- request-records page gets slower the more records it holds.
--
-- The index is two columns on purpose. A third column between the sort column
-- and id cannot serve the ORDER BY: SQLite can only follow an index's contiguous
-- prefix, so "model_id DESC, id DESC" matches an index on (model_id, id) and not
-- one on (model_id, ts_created, id). That is why the existing
-- (model|key|session, ts_created, id) indexes, which serve the time filters well,
-- still force a sort when the page sorts by one of those columns. The new
-- indexes cover the sort; the old ones keep serving the filters.
--
-- req_path had no usable index at all (00017's lacks the id tiebreaker), and the
-- remaining columns below had none whatsoever.
CREATE INDEX idx_activity_req_path_sort
    ON activity (req_path, id);
CREATE INDEX idx_activity_model_sort
    ON activity (model_id, id);
CREATE INDEX idx_activity_key_sort
    ON activity (key_id, id);
CREATE INDEX idx_activity_session_sort
    ON activity (session_id, id);
CREATE INDEX idx_activity_status_sort
    ON activity (resp_status_code, id);
CREATE INDEX idx_activity_content_type_sort
    ON activity (resp_content_type, id);
CREATE INDEX idx_activity_cached_sort
    ON activity (cache_tokens, id);
CREATE INDEX idx_activity_input_sort
    ON activity (input_tokens, id);
CREATE INDEX idx_activity_output_sort
    ON activity (output_tokens, id);
CREATE INDEX idx_activity_drafted_sort
    ON activity (draft_tokens, id);
CREATE INDEX idx_activity_prompt_speed_sort
    ON activity (prompt_per_second, id);
CREATE INDEX idx_activity_gen_speed_sort
    ON activity (tokens_per_second, id);
CREATE INDEX idx_activity_duration_sort
    ON activity (duration_ms, id);
CREATE INDEX idx_activity_cost_sort
    ON activity (estimated_cost, id);

-- +goose Down
DROP INDEX idx_activity_cost_sort;
DROP INDEX idx_activity_duration_sort;
DROP INDEX idx_activity_gen_speed_sort;
DROP INDEX idx_activity_prompt_speed_sort;
DROP INDEX idx_activity_drafted_sort;
DROP INDEX idx_activity_output_sort;
DROP INDEX idx_activity_input_sort;
DROP INDEX idx_activity_cached_sort;
DROP INDEX idx_activity_content_type_sort;
DROP INDEX idx_activity_status_sort;
DROP INDEX idx_activity_session_sort;
DROP INDEX idx_activity_key_sort;
DROP INDEX idx_activity_model_sort;
DROP INDEX idx_activity_req_path_sort;
