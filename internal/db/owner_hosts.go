package db

import (
	"context"
	"time"

	"github.com/lib/pq"
)

// ReadyOwnerLabels lists the owner labels whose "*.<owner>.<base>"
// certificate is ready (migration 0042).
func ReadyOwnerLabels(ctx context.Context, q Querier) ([]string, error) {
	return queryStrings(ctx, q, `SELECT owner_label FROM owner_hosts WHERE ready ORDER BY owner_label`)
}

// OwnerLabelsWithSites lists the host label of every account or team that
// owns at least one site, and every owner label a moved or renamed site
// still redirects from (site_redirects): the owners that need a certificate
// of their own, so an old address keeps answering over TLS.
func OwnerLabelsWithSites(ctx context.Context, q Querier) ([]string, error) {
	return queryStrings(ctx, q, `
		SELECT lower(replace(u.username, '.', '-'))
		FROM users u JOIN sites s ON s.user_id = u.id
		UNION
		SELECT owner_label FROM site_redirects
		ORDER BY 1`)
}

// SetOwnerHostReady records whether an owner label's certificate is ready.
func SetOwnerHostReady(ctx context.Context, q Querier, label string, ready bool) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO owner_hosts (owner_label, ready, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (owner_label) DO UPDATE SET ready = EXCLUDED.ready, updated_at = now()
		WHERE owner_hosts.ready IS DISTINCT FROM EXCLUDED.ready`, label, ready)
	return err
}

// DeleteOwnerHostsExcept forgets every owner label not in keep.
func DeleteOwnerHostsExcept(ctx context.Context, q Querier, keep []string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM owner_hosts WHERE NOT (owner_label = ANY($1))`, pq.Array(keep))
	return err
}

// OwnerHostWait is an owner whose "*.<owner>.<base>" certificate is not
// ready yet, and since when: when the reconciler last recorded it not
// ready, or, when it has not seen the owner yet, their first site.
type OwnerHostWait struct {
	Label string
	Since time.Time
}

// OwnerHostReadiness counts the owners that need a certificate (the same
// set OwnerLabelsWithSites gives the reconciler) whose certificate is
// ready, and lists the rest longest-waiting first.
func OwnerHostReadiness(ctx context.Context, q Querier) (ready int, waiting []OwnerHostWait, err error) {
	rows, err := q.QueryContext(ctx, `
		WITH owners AS (
			SELECT lower(replace(u.username, '.', '-')) AS label, min(s.created_at) AS first_site
			FROM users u JOIN sites s ON s.user_id = u.id
			GROUP BY 1
			UNION ALL
			SELECT owner_label, min(created_at) FROM site_redirects GROUP BY 1
		), needed AS (
			SELECT label, min(first_site) AS first_site FROM owners GROUP BY label
		)
		SELECT n.label, COALESCE(h.ready, false), COALESCE(h.updated_at, n.first_site)
		FROM needed n LEFT JOIN owner_hosts h ON h.owner_label = n.label
		ORDER BY 3, 1`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w OwnerHostWait
		var ok bool
		if err := rows.Scan(&w.Label, &ok, &w.Since); err != nil {
			return 0, nil, err
		}
		if ok {
			ready++
		} else {
			waiting = append(waiting, w)
		}
	}
	return ready, waiting, rows.Err()
}
