-- Per-owner-per-site-per-day AI token usage counters for the Claude Haiku
-- proxy (/api/claude/v1/messages). A usage "calculator", not a quota — rows
-- are written best-effort by the handler after each proxied request and never
-- gate anything. Day boundary is UTC.
--
-- site_name is the name (not sites.id) on purpose: usage history should
-- survive site deletion/re-creation. user_id cascades with the owning user.
--
-- NOTE: 0007 is reserved for the state-concurrency sites.state_version ALTER.
CREATE TABLE ai_usage (
    user_id       UUID   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    site_name     TEXT   NOT NULL,
    day           DATE   NOT NULL,
    requests      BIGINT NOT NULL DEFAULT 0,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, site_name, day)
);
