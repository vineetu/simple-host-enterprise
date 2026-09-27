package db

import (
	"context"
	"database/sql"
	"time"
)

// IdleGrace is how long a site stays marked idle before it moves to
// Recently deleted, unless it is kept or used again first.
const IdleGrace = 30 * 24 * time.Hour

// siteLastUsed is when a site (alias s) was last used: created, deployed,
// its saved data written (both bump updated_at), or visited by a person.
const siteLastUsed = `GREATEST(s.created_at, s.updated_at, COALESCE(
	(SELECT max(a.last_seen_at) FROM site_daily_analytics a WHERE a.site_id = s.id AND a.pageviews > 0),
	s.created_at))`

// IdleSite is a site the idle cleanup has marked (or is acting on).
type IdleSite struct {
	ID            string
	OwnerID       string
	Owner         string
	Name          string
	ActiveVersion int
	IdleSince     time.Time
	LastUsed      time.Time
}

// DeleteOn is when the site moves to Recently deleted if nothing changes.
func (s IdleSite) DeleteOn() time.Time { return s.IdleSince.Add(IdleGrace) }

func scanIdleSites(rows *sql.Rows) ([]IdleSite, error) {
	defer rows.Close()
	var out []IdleSite
	for rows.Next() {
		var s IdleSite
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.Owner, &s.Name, &s.ActiveVersion, &s.IdleSince, &s.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkIdleSites marks every live site not kept and not used for idleFor,
// and returns them. Atomic per row, so replicas running it at once never
// mark (or report) a site twice.
func MarkIdleSites(ctx context.Context, q Querier, idleFor time.Duration) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE sites s SET idle_since = now()
		FROM users u
		WHERE u.id = s.user_id AND s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NULL
		  AND `+siteLastUsed+` < now() - $1 * interval '1 second'
		RETURNING s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed,
		int64(idleFor/time.Second))
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// ClearUsedIdleSites unmarks every marked site that has been used since it
// was marked (a visit, a deploy, a saved-data write, or a restore), and
// returns them.
func ClearUsedIdleSites(ctx context.Context, q Querier) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE sites s SET idle_since = NULL
		FROM users u, sites prev
		WHERE u.id = s.user_id AND prev.id = s.id AND s.idle_since IS NOT NULL AND s.deleted_at IS NULL
		  AND `+siteLastUsed+` > s.idle_since
		RETURNING s.id::text, s.user_id::text, u.username, s.name, s.active_version, prev.idle_since, `+siteLastUsed)
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// DueIdleSites lists the marked sites whose grace has run out, still
// unused and not kept: the ones to move to Recently deleted now.
func DueIdleSites(ctx context.Context, q Querier) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NOT NULL
		  AND s.idle_since < now() - $1 * interval '1 second'
		  AND `+siteLastUsed+` <= s.idle_since
		ORDER BY s.idle_since`, int64(IdleGrace/time.Second))
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// StillDueIdle re-checks, under the site's collaboration lock, that it is
// still due: nobody kept, used, deleted or moved it since DueIdleSites.
func StillDueIdle(ctx context.Context, q Querier, s IdleSite) (bool, error) {
	var due bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sites s
		WHERE s.id = $1::uuid AND s.user_id = $2::uuid AND s.name = $3
		  AND s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NOT NULL
		  AND s.idle_since < now() - $4 * interval '1 second'
		  AND `+siteLastUsed+` <= s.idle_since)`,
		s.ID, s.OwnerID, s.Name, int64(IdleGrace/time.Second)).Scan(&due)
	return due, err
}

// ClearIdleMark unmarks one site (after the cleanup deleted it).
func ClearIdleMark(ctx context.Context, q Querier, siteID string) error {
	_, err := q.ExecContext(ctx, `UPDATE sites SET idle_since = NULL WHERE id = $1::uuid`, siteID)
	return err
}

// ListIdleSites lists marked sites, soonest to go first: every one for
// actorID "" (the admin list), else those in actorID's own namespace and
// their teams' (the dashboard notice).
func ListIdleSites(ctx context.Context, q Querier, actorID string) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.deleted_at IS NULL AND s.idle_since IS NOT NULL AND NOT s.idle_keep
		  AND ($1 = '' OR s.user_id = $1::uuid OR EXISTS (
			SELECT 1 FROM team_members tm WHERE tm.team_id = s.user_id AND tm.user_id::text = $1))
		ORDER BY s.idle_since, u.username, s.name`, actorID)
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// SetSiteIdleKeep records Keep (or takes it back) for one site; keeping also
// unmarks it. It returns when the site had been marked (zero when it was
// not). It never touches updated_at: keeping is not using.
func SetSiteIdleKeep(ctx context.Context, q Querier, siteID string, keep bool) (wasIdleSince *time.Time, err error) {
	err = q.QueryRowContext(ctx, `
		UPDATE sites s SET idle_keep = $2, idle_since = CASE WHEN $2 THEN NULL ELSE s.idle_since END
		FROM (SELECT idle_since FROM sites WHERE id = $1::uuid) prev
		WHERE s.id = $1::uuid
		RETURNING prev.idle_since`, siteID, keep).Scan(&wasIdleSince)
	return wasIdleSince, err
}

// IdleNoticeRecipients is who is told a site was marked: the owner's
// address, or every enabled member's for a team.
func IdleNoticeRecipients(ctx context.Context, q Querier, ownerID string) ([]string, error) {
	return queryStrings(ctx, q, `
		SELECT DISTINCT u.email FROM users u
		WHERE u.disabled_at IS NULL AND COALESCE(u.email, '') <> '' AND (
			u.id = $1::uuid OR u.id IN (SELECT tm.user_id FROM team_members tm WHERE tm.team_id = $1::uuid))
		ORDER BY 1`, ownerID)
}
