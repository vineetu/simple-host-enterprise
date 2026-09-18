ALTER TABLE sites ADD COLUMN IF NOT EXISTS uses_state           boolean NOT NULL DEFAULT false;
ALTER TABLE sites ADD COLUMN IF NOT EXISTS uses_versioned_state boolean NOT NULL DEFAULT false;
