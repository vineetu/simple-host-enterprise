-- simple-host: backward-compatible
-- Cross-replica create response replay; older releases ignore this table.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE create_idempotency (
    account_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_key text NOT NULL,
    body_hash bytea NOT NULL,
    status integer NOT NULL DEFAULT 0,
    headers jsonb NOT NULL DEFAULT '{}',
    body bytea NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL DEFAULT (now() + interval '24 hours'),
    PRIMARY KEY (account_id, request_key)
);
CREATE INDEX create_idempotency_expiry ON create_idempotency (expires_at);
GRANT SELECT, INSERT, UPDATE, DELETE ON create_idempotency TO simplehost_app;
COMMIT;
