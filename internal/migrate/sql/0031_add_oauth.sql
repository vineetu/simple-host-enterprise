-- OAuth for /mcp: the authorization server an AI app (ChatGPT, Claude,
-- Copilot, Cursor, Codex) uses to connect on a person's behalf. The app
-- registers itself (RFC 7591), the person signs in through the company's
-- OIDC provider and allows it once, and the app then holds a short-lived
-- access token and a rotating refresh token.
--
-- Every secret here (client secret, code, token) is stored only as the
-- SHA-256 of a 256-bit random value, the same shape api_keys.key_hash uses.
-- Grants hang off users with ON DELETE CASCADE, and disabling a person
-- deletes their grants (db.SetUserDisabled), which takes every token with it.

BEGIN;

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE IF NOT EXISTS oauth_clients (
    client_id                  text PRIMARY KEY,
    client_secret_hash         bytea,
    client_name                text NOT NULL,
    redirect_uris              jsonb NOT NULL,
    token_endpoint_auth_method text NOT NULL DEFAULT 'none',
    created_at                 timestamptz NOT NULL DEFAULT now(),
    last_used_at               timestamptz,
    CONSTRAINT oauth_clients_auth_method_check
        CHECK (token_endpoint_auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
    CONSTRAINT oauth_clients_secret_shape
        CHECK ((token_endpoint_auth_method = 'none') = (client_secret_hash IS NULL))
);

-- One grant per "Allow". It is the refresh-token family: rotation stays
-- inside it, and a rotated refresh token presented again deletes it.
CREATE TABLE IF NOT EXISTS oauth_grants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id    text NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    resource     text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS oauth_grants_user_idx ON oauth_grants (user_id);
CREATE INDEX IF NOT EXISTS oauth_grants_client_idx ON oauth_grants (client_id);

-- Authorization codes: single use, one minute, bound to the client, the
-- redirect URI, the PKCE challenge and the resource. grant_id is set on
-- redemption so a second redemption can revoke what the first issued.
CREATE TABLE IF NOT EXISTS oauth_codes (
    code_hash      bytea PRIMARY KEY,
    client_id      text NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    user_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri   text NOT NULL,
    code_challenge text NOT NULL,
    resource       text NOT NULL,
    expires_at     timestamptz NOT NULL,
    used_at        timestamptz,
    grant_id       uuid REFERENCES oauth_grants(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS oauth_codes_expires_idx ON oauth_codes (expires_at);

-- used_at on a refresh token marks it rotated; presenting it again is reuse.
CREATE TABLE IF NOT EXISTS oauth_tokens (
    token_hash bytea PRIMARY KEY,
    grant_id   uuid NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    CONSTRAINT oauth_tokens_kind_check CHECK (kind IN ('access', 'refresh'))
);

CREATE INDEX IF NOT EXISTS oauth_tokens_grant_idx ON oauth_tokens (grant_id);
CREATE INDEX IF NOT EXISTS oauth_tokens_expires_idx ON oauth_tokens (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE oauth_clients, oauth_grants, oauth_codes, oauth_tokens TO simplehost_app;

COMMIT;
