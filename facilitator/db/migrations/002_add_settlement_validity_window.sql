-- Adds the authorization's EIP-3009 validity window to each settlement row,
-- and an index to find stale in_flight rows without a full table scan (spec
-- 0007 follow-up #197: the periodic sweep). Existing rows have no recorded
-- window (they predate this column); DEFAULT '0' leaves ValidBefore = "0",
-- which settle.ReconcileRow treats as "no recorded window" and skips —
-- logging a warning and leaving the row in_flight for manual/support
-- handling rather than guessing an outcome from data the row never had.
ALTER TABLE settlements
    ADD COLUMN valid_after  TEXT NOT NULL DEFAULT '0',
    ADD COLUMN valid_before TEXT NOT NULL DEFAULT '0';

-- Partial index: only in_flight rows are ever queried by StaleInFlight, and
-- the table is otherwise mostly settled/failed rows once a row terminates.
CREATE INDEX settlements_stale_in_flight_idx
    ON settlements (created_at)
    WHERE status = 'in_flight';
