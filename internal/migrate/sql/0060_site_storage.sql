-- simple-host: backward-compatible
-- Site storage with access presets (docs/designs/storage-presets.md): new
-- tables only, which older releases ignore.
--
-- A resource is one key-value namespace, one database of tables, or one file
-- bucket. Its access is a matrix of four actions, each one of five values;
-- for a database, settings.tables holds each table's columns and matrix.
-- The server validates every matrix before saving it; the CHECKs below are
-- the floor the database itself holds (R1: no anonymous edit or delete; R2:
-- add is never own).
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE site_storage_resources (
    site_id      uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    kind         text NOT NULL CHECK (kind IN ('kv', 'db', 'files')),
    preset       text NOT NULL,
    read_who     text NOT NULL CHECK (read_who IN ('nobody', 'owner', 'own', 'signed-in', 'anyone')),
    add_who      text NOT NULL CHECK (add_who IN ('nobody', 'owner', 'signed-in', 'anyone')),
    edit_who     text NOT NULL CHECK (edit_who IN ('nobody', 'owner', 'own', 'signed-in')),
    delete_who   text NOT NULL CHECK (delete_who IN ('nobody', 'owner', 'own', 'signed-in')),
    settings     jsonb NOT NULL DEFAULT '{}'::jsonb,
    bytes        bigint NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, name)
);

CREATE TABLE site_storage_kv (
    site_id      uuid NOT NULL,
    resource     text NOT NULL,
    key          text NOT NULL,
    value        jsonb NOT NULL,
    added_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, resource, key),
    FOREIGN KEY (site_id, resource) REFERENCES site_storage_resources(site_id, name) ON DELETE CASCADE
);

CREATE TABLE site_storage_rows (
    id           bigserial PRIMARY KEY,
    site_id      uuid NOT NULL,
    resource     text NOT NULL,
    table_name   text NOT NULL,
    data         jsonb NOT NULL,
    added_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz,
    FOREIGN KEY (site_id, resource) REFERENCES site_storage_resources(site_id, name) ON DELETE CASCADE
);
CREATE INDEX site_storage_rows_table_idx ON site_storage_rows (site_id, resource, table_name, id);

-- The bytes are a bucket object (sites/<site-id>/assets/<object_id>),
-- written through the asset code; replacing or deleting a file retires the
-- old object in the same transaction.
CREATE TABLE site_storage_files (
    site_id      uuid NOT NULL,
    resource     text NOT NULL,
    path         text NOT NULL,
    object_id    uuid NOT NULL,
    content_type text NOT NULL,
    size         bigint NOT NULL CHECK (size >= 0),
    sha256       bytea NOT NULL,
    added_by     uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (site_id, resource, path),
    FOREIGN KEY (site_id, resource) REFERENCES site_storage_resources(site_id, name) ON DELETE CASCADE
);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE site_storage_resources, site_storage_kv, site_storage_rows, site_storage_files TO simplehost_app;
GRANT USAGE, SELECT ON SEQUENCE site_storage_rows_id_seq TO simplehost_app;

COMMIT;
