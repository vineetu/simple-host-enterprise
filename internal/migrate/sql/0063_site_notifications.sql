-- simple-host: backward-compatible
-- Additive notification subscriptions, outbox, leases, and shared budgets.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE IF NOT EXISTS site_notifications (
 id TEXT PRIMARY KEY,
 site_id UUID NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
 site_owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source TEXT NOT NULL,
 grant_id UUID REFERENCES oauth_grants(id) ON DELETE CASCADE,
 app_name TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '',
 url TEXT NOT NULL, events TEXT[] NOT NULL, storage TEXT NOT NULL DEFAULT '',
 args_canonical TEXT NOT NULL DEFAULT '', secret TEXT NOT NULL,
 prev_secret TEXT NOT NULL DEFAULT '', prev_secret_until TIMESTAMPTZ,
 auth_header_name TEXT NOT NULL DEFAULT '', auth_header_value TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'active', verified_at TIMESTAMPTZ, expires_at TIMESTAMPTZ,
 window_start TIMESTAMPTZ, window_attempts INT NOT NULL DEFAULT 0, window_failures INT NOT NULL DEFAULT 0,
 failing_since TIMESTAMPTZ, last_attempt_at TIMESTAMPTZ, last_success_at TIMESTAMPTZ,
 last_result TEXT NOT NULL DEFAULT '', suspended_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS site_notifications_site ON site_notifications(site_id);
CREATE INDEX IF NOT EXISTS site_notifications_owner ON site_notifications(owner_id);
CREATE INDEX IF NOT EXISTS site_notifications_grant ON site_notifications(grant_id);
CREATE TABLE IF NOT EXISTS site_events (
 seq BIGSERIAL PRIMARY KEY, event_id TEXT UNIQUE NOT NULL,
 site_id UUID NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
 name TEXT NOT NULL, storage TEXT NOT NULL, data JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS site_events_site_seq ON site_events(site_id,seq);
CREATE INDEX IF NOT EXISTS site_events_created ON site_events(created_at);
CREATE TABLE IF NOT EXISTS site_event_deliveries (
 event_seq BIGINT REFERENCES site_events(seq) ON DELETE CASCADE,
 subscription_id TEXT REFERENCES site_notifications(id) ON DELETE CASCADE,
 state TEXT NOT NULL DEFAULT 'pending', attempts INT NOT NULL DEFAULT 0,
 send_started BOOLEAN NOT NULL DEFAULT false,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(), lease_until TIMESTAMPTZ,
 last_attempt_at TIMESTAMPTZ, last_http INT, last_category TEXT NOT NULL DEFAULT '', duration_ms INT,
 PRIMARY KEY(event_seq,subscription_id)
);
CREATE INDEX IF NOT EXISTS site_event_deliveries_due ON site_event_deliveries(next_attempt_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS site_event_deliveries_subscription ON site_event_deliveries(subscription_id,event_seq);

CREATE TABLE notification_budgets (
 key TEXT PRIMARY KEY, window_start TIMESTAMPTZ NOT NULL DEFAULT now(), used INT NOT NULL DEFAULT 0
);
GRANT SELECT, INSERT, UPDATE, DELETE ON site_notifications, site_events, site_event_deliveries, notification_budgets TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE site_events_seq_seq TO simplehost_app;
COMMIT;
