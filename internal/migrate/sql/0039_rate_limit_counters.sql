-- simple-host: backward-compatible
-- Marked compatible because it only adds a table (and its index and grant)
-- that no older code reads or writes, so a rollback runs unchanged against
-- the schema this leaves.
--
-- Fixed-window counters for the security-relevant rate limits (sign-in,
-- session hand-off, API key mint, the connector's token and registration
-- endpoints), shared by every replica so N pods do not mean N times the
-- budget. One row per (hashed policy+caller key, window). key is a sha256
-- hex digest: no raw address or identifier is stored. window_start is
-- computed by the database (date_bin over now()), so pods with skewed clocks
-- agree on the window. Rows are pruned opportunistically by the server once
-- their window is long over (internal/ratelimit/shared.go).

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS rate_limit_counters (
    key          text        NOT NULL,
    window_start timestamptz NOT NULL,
    count        integer     NOT NULL,
    PRIMARY KEY (key, window_start)
);

CREATE INDEX IF NOT EXISTS rate_limit_counters_window_start_idx
    ON rate_limit_counters (window_start);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE rate_limit_counters TO simplehost_app;

COMMIT;
