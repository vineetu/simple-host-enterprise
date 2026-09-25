-- The tables every later migration alters. The original instance created these
-- by hand; this file is that DDL as the code reads it
-- (plaintext api_key, no S3 columns beyond the version prefix the code still
-- carries), so a fresh database and the migration chain agree.

CREATE TABLE IF NOT EXISTS users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username   text UNIQUE NOT NULL,
    api_key    text UNIQUE NOT NULL,
    is_admin   boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sites (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           text NOT NULL,
    active_version integer NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

CREATE TABLE IF NOT EXISTS versions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    site_id        uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    version_number integer NOT NULL,
    s3_prefix      text NOT NULL,
    status         text NOT NULL DEFAULT 'uploading',
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (site_id, version_number)
);
