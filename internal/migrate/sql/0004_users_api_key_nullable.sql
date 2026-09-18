-- Make users.api_key nullable. NULL means "admin has approved a reset; user
-- can claim a fresh key by re-calling POST /api/auth." Existing rows
-- (all currently non-NULL) are untouched.
ALTER TABLE users ALTER COLUMN api_key DROP NOT NULL;
