-- Separate non-human traffic from readership in the daily rollup.
--
-- Views were counting anything that received HTML: the platform's own skill
-- tells every agent to fetch the deployed URL after each deploy, and each of
-- those fetches counted as both a view and — because a bare fetch carries no
-- cookie back — a brand new visitor.
--
-- Traffic is split rather than dropped. The headline numbers become readership;
-- the bot columns keep the rest, so any figure can still be explained and a
-- misclassification shows up instead of silently vanishing.
--
-- Additive and backfill-free: existing rows keep their totals in pageviews and
-- visits. Those pre-cutover figures still include agent traffic and cannot be
-- separated retroactively, because no user agent was ever recorded.

ALTER TABLE site_daily_analytics
  ADD COLUMN IF NOT EXISTS bot_pageviews BIGINT NOT NULL DEFAULT 0;

ALTER TABLE site_daily_analytics
  ADD COLUMN IF NOT EXISTS bot_visits BIGINT NOT NULL DEFAULT 0;
