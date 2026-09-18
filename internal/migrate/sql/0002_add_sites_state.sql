ALTER TABLE sites ADD COLUMN state jsonb NOT NULL DEFAULT '{}'::jsonb;
