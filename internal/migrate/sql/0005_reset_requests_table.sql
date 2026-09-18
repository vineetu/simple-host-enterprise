-- Reset request intake. Users submit via POST /api/reset-requests (driven by
-- the skill); admin approves/rejects from /admin. Approval invalidates the
-- user's current api_key; user then re-calls /api/auth to claim a fresh one.
CREATE TABLE reset_requests (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email       TEXT NOT NULL,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_at TIMESTAMPTZ,
    rejected_at TIMESTAMPTZ
);

-- Index for the admin dashboard's "pending requests" query.
CREATE INDEX reset_requests_open_idx
    ON reset_requests (created_at)
    WHERE approved_at IS NULL AND rejected_at IS NULL;

-- At most one OPEN request per user; collapses duplicate submissions so the
-- admin queue stays clean. Insertions use ON CONFLICT to no-op duplicates.
CREATE UNIQUE INDEX reset_requests_one_open_per_user
    ON reset_requests (user_id)
    WHERE approved_at IS NULL AND rejected_at IS NULL;
