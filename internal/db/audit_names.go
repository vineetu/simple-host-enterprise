package db

import (
	"context"

	"github.com/lib/pq"
)

// AuditNames resolves the ids on a page of audit events to names: every
// user or team id to its username, every site id to its name. Deleted sites
// in their recovery window still resolve; an id that no longer exists (a
// purged site, an erased person) is simply absent, and the reader shows the
// id instead.
func AuditNames(ctx context.Context, q Querier, userIDs, siteIDs []string) (users, sites map[string]string, err error) {
	users, sites = map[string]string{}, map[string]string{}
	if len(userIDs) > 0 {
		rows, err := q.QueryContext(ctx, `SELECT id::text, username FROM users WHERE id::text = ANY($1)`, pq.Array(userIDs))
		if err != nil {
			return nil, nil, err
		}
		if err := scanPairs(rows, users); err != nil {
			return nil, nil, err
		}
	}
	if len(siteIDs) > 0 {
		rows, err := q.QueryContext(ctx, `SELECT id::text, name FROM sites WHERE id::text = ANY($1)`, pq.Array(siteIDs))
		if err != nil {
			return nil, nil, err
		}
		if err := scanPairs(rows, sites); err != nil {
			return nil, nil, err
		}
	}
	return users, sites, nil
}

// TeamMemberIDs returns, for each of teamIDs, the ids of its members.
func TeamMemberIDs(ctx context.Context, q Querier, teamIDs []string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	if len(teamIDs) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT team_id::text, user_id::text FROM team_members WHERE team_id::text = ANY($1)`, pq.Array(teamIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var team, user string
		if err := rows.Scan(&team, &user); err != nil {
			return nil, err
		}
		if out[team] == nil {
			out[team] = map[string]bool{}
		}
		out[team][user] = true
	}
	return out, rows.Err()
}

// SiteIDForAuditFilter is the id of ownerID's site of that name for an
// audit-log filter: the live one, or else one in its recovery window, so a
// deleted site's history can still be searched. sql.ErrNoRows when neither.
func SiteIDForAuditFilter(ctx context.Context, q Querier, ownerID, name string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id::text FROM sites WHERE user_id = $1::uuid AND name = $2
		ORDER BY deleted_at NULLS FIRST LIMIT 1`, ownerID, name).Scan(&id)
	return id, err
}

func scanPairs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}, into map[string]string) error {
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		into[k] = v
	}
	return rows.Err()
}
