BEGIN;

CREATE TABLE site_collaborators (
    site_id    uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       text NOT NULL DEFAULT 'editor',
    added_by   uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT site_collaborators_pkey PRIMARY KEY (site_id, user_id),
    CONSTRAINT site_collaborators_role_check CHECK (role = 'editor')
);

CREATE INDEX site_collaborators_user_site_idx
    ON site_collaborators (user_id, site_id);

ALTER TABLE versions
    ADD COLUMN uploaded_by uuid REFERENCES users(id) ON DELETE SET NULL;

UPDATE versions AS version
SET uploaded_by = site.user_id
FROM sites AS site
WHERE site.id = version.site_id;

COMMIT;
