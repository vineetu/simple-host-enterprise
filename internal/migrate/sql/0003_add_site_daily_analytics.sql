CREATE TABLE IF NOT EXISTS site_daily_analytics (
    site_id uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    day date NOT NULL,
    pageviews bigint NOT NULL DEFAULT 0,
    visits bigint NOT NULL DEFAULT 0,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, day)
);

CREATE INDEX IF NOT EXISTS idx_site_daily_analytics_day
    ON site_daily_analytics (day DESC);
