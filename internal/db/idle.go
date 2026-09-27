package db

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/vsriram/simple-host/internal/oplimits"
	"time"
)

// IdleGrace is how long a site stays marked idle before it moves to
// Recently deleted, unless it is kept or used again first
// (IDLE_CLEANUP_GRACE_DAYS, 30 by default).
func IdleGrace() time.Duration { return oplimits.Get().IdleGrace() }

// siteLastUsed is when a site (alias s) was last used: created, deployed,
// its saved data read or written, or opened by anyone but a bot or a
// preview (sites.last_used_at, bumped at most hourly: TouchSiteUsed,
// MarkSiteUsed). A site that existed before last_used_at did starts at the
// time the column was added, so it is never judged idle on missing data.
const siteLastUsed = `GREATEST(s.created_at, s.updated_at, s.last_used_at)`

// SiteUseThrottle is how often a site's use is written: at most once per
// site in this long, however many visits and saved-data reads it gets.
const SiteUseThrottle = time.Hour

// MarkSiteUsed records that owner's site was just opened or its saved data
// read, unless that was already recorded within SiteUseThrottle. It updates
// the site row, so it waits for (and is seen by) the idle cleanup's check
// under LockSiteRow.
func MarkSiteUsed(ctx context.Context, q Querier, owner, site string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE sites s SET last_used_at = now()
		FROM users u
		WHERE u.id = s.user_id AND u.username = $1 AND s.name = $2 AND s.deleted_at IS NULL
		  AND s.last_used_at < now() - $3 * interval '1 second'`,
		owner, site, int64(SiteUseThrottle/time.Second))
	return err
}

// TouchSiteUsed records, in the caller's transaction, that a site was just
// used (a version deployed, live or held).
func TouchSiteUsed(ctx context.Context, q Querier, siteID string) error {
	_, err := q.ExecContext(ctx, `UPDATE sites SET last_used_at = now() WHERE id = $1::uuid`, siteID)
	return err
}

// LockSiteRow takes the site row's lock for the rest of the transaction, so
// a concurrent use (MarkSiteUsed, a saved-data write, Keep) either lands
// before the caller's next read or waits until it commits.
func LockSiteRow(ctx context.Context, q Querier, siteID string) error {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id::text FROM sites WHERE id = $1::uuid FOR UPDATE`, siteID).Scan(&id)
	return err
}

// IdleSite is a site the idle cleanup has marked (or is acting on).
type IdleSite struct {
	ID            string
	OwnerID       string
	Owner         string
	Name          string
	ActiveVersion int
	IdleSince     time.Time
	LastUsed      time.Time
	// deleteAt is the date stored when it was marked (NULL for marks from
	// before migration 0054).
	deleteAt sql.NullTime
}

// DeleteOn is when the site moves to Recently deleted if nothing changes:
// the date given when it was marked, so a later IDLE_CLEANUP_GRACE_DAYS
// change applies to new marks only.
func (s IdleSite) DeleteOn() time.Time {
	if s.deleteAt.Valid {
		return s.deleteAt.Time
	}
	return s.IdleSince.Add(IdleGrace())
}

// idleDue is true for a marked site (alias s) whose delete date has passed;
// $%d is IdleGrace in seconds, for marks from before the date was stored.
func idleDue(p int) string {
	return fmt.Sprintf(`COALESCE(s.idle_delete_at, s.idle_since + $%d * interval '1 second') < now()`, p)
}

func scanIdleSites(rows *sql.Rows) ([]IdleSite, error) {
	defer rows.Close()
	var out []IdleSite
	for rows.Next() {
		var s IdleSite
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.Owner, &s.Name, &s.ActiveVersion, &s.IdleSince, &s.LastUsed, &s.deleteAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// notRestricted keeps a site an admin has restricted (a takedown, often
// held as evidence) out of idle cleanup: it is never marked or deleted for
// disuse, and a mark it had is cleared.
const notRestricted = `s.access_decision IS DISTINCT FROM 'restricted'`

// markEligible is true for a live site (alias s) that is not kept, not
// marked, not restricted, and unused for $1 seconds.
const markEligible = `s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NULL AND ` + notRestricted + `
		  AND ` + siteLastUsed + ` < now() - $1 * interval '1 second'`

// IdleMarkCandidates lists the sites MarkIdleSite would mark now, the
// longest unused first; limit, when above 0, lists at most that many (the
// rest are marked on a later run). IdleSince is the listing time.
func IdleMarkCandidates(ctx context.Context, q Querier, idleFor time.Duration, limit int) ([]IdleSite, error) {
	var maxSites any // NULL: LIMIT NULL lists every idle site
	if limit > 0 {
		maxSites = limit
	}
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, now(), `+siteLastUsed+`, s.idle_delete_at
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE `+markEligible+`
		ORDER BY `+siteLastUsed+`, s.id
		LIMIT $2`,
		int64(idleFor/time.Second), maxSites)
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// MarkIdleSite marks one site if it is still eligible, and reports whether
// it did. Called one site per transaction, under the site's collaboration
// lock and row lock (LockSiteRow), the order every lifecycle change takes
// them in, so a run never holds one site's row while waiting for another's.
func MarkIdleSite(ctx context.Context, q Querier, siteID string, idleFor time.Duration) (IdleSite, bool, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE sites s SET idle_since = now(), idle_delete_at = now() + $3 * interval '1 second'
		FROM users u
		WHERE u.id = s.user_id AND s.id = $2::uuid AND `+markEligible+`
		RETURNING s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`, s.idle_delete_at`,
		int64(idleFor/time.Second), siteID, int64(IdleGrace()/time.Second))
	if err != nil {
		return IdleSite{}, false, err
	}
	sites, err := scanIdleSites(rows)
	if err != nil || len(sites) == 0 {
		return IdleSite{}, false, err
	}
	return sites[0], true, nil
}

// clearEligible is true for a marked site (alias s) that has been used
// since it was marked (a visit, a deploy, a saved-data write, or a
// restore), or that an admin has restricted since.
const clearEligible = `s.idle_since IS NOT NULL AND s.deleted_at IS NULL
		  AND (` + siteLastUsed + ` > s.idle_since OR s.access_decision = 'restricted')`

// IdleClearCandidates lists the marked sites ClearIdleSite would unmark now.
func IdleClearCandidates(ctx context.Context, q Querier) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`, s.idle_delete_at
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE `+clearEligible+`
		ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	return scanIdleSites(rows)
}

// ClearIdleSite unmarks one site if it is still eligible, and reports
// whether it did; locked the way MarkIdleSite is.
func ClearIdleSite(ctx context.Context, q Querier, siteID string) (IdleSite, bool, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE sites s SET idle_since = NULL
		FROM users u, sites prev
		WHERE u.id = s.user_id AND prev.id = s.id AND s.id = $1::uuid AND `+clearEligible+`
		RETURNING s.id::text, s.user_id::text, u.username, s.name, s.active_version, prev.idle_since, `+siteLastUsed+`, prev.idle_delete_at`, siteID)
	if err != nil {
		return IdleSite{}, false, err
	}
	sites, err := scanIdleSites(rows)
	if err != nil || len(sites) == 0 {
		return IdleSite{}, false, err
	}
	return sites[0], true, nil
}

// DueIdleSites lists the marked sites whose delete date (DeleteOn) has
// passed, still unused and not kept: the ones to move to Recently deleted
// now.
func DueIdleSites(ctx context.Context, q Querier) ([]IdleSite, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`, s.idle_delete_at
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NOT NULL AND `+notRestricted+`
		  AND `+idleDue(1)+`
		  AND `+siteLastUsed+` <= s.idle_since
		ORDER BY s.idle_since`, int64(IdleGrace()/time.Second))
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
		  AND s.deleted_at IS NULL AND NOT s.idle_keep AND s.idle_since IS NOT NULL AND `+notRestricted+`
		  AND `+idleDue(4)+`
		  AND `+siteLastUsed+` <= s.idle_since)`,
		s.ID, s.OwnerID, s.Name, int64(IdleGrace()/time.Second)).Scan(&due)
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
		SELECT s.id::text, s.user_id::text, u.username, s.name, s.active_version, s.idle_since, `+siteLastUsed+`, s.idle_delete_at
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.deleted_at IS NULL AND s.idle_since IS NOT NULL AND NOT s.idle_keep AND `+notRestricted+`
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
// not). It never touches updated_at: keeping is not using. sql.ErrNoRows
// when the site has been deleted.
func SetSiteIdleKeep(ctx context.Context, q Querier, siteID string, keep bool) (wasIdleSince *time.Time, err error) {
	err = q.QueryRowContext(ctx, `
		UPDATE sites s SET idle_keep = $2, idle_since = CASE WHEN $2 THEN NULL ELSE s.idle_since END
		FROM (SELECT idle_since FROM sites WHERE id = $1::uuid) prev
		WHERE s.id = $1::uuid AND s.deleted_at IS NULL
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
