CREATE TABLE IF NOT EXISTS site_file_downloads (
    site_id uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    path    text NOT NULL,
    day     date NOT NULL,
    downloads bigint NOT NULL DEFAULT 0,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, path, day)
);

CREATE INDEX IF NOT EXISTS idx_site_file_downloads_day ON site_file_downloads (day DESC);
