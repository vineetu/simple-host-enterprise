package db

import (
	"context"

	"github.com/lib/pq"
)

// ReadyOwnerLabels lists the owner labels whose "*.<owner>.<base>"
// certificate is ready (migration 0042).
func ReadyOwnerLabels(ctx context.Context, q Querier) ([]string, error) {
	return queryStrings(ctx, q, `SELECT owner_label FROM owner_hosts WHERE ready ORDER BY owner_label`)
}

// OwnerLabelsWithSites lists the host label of every account or team that
// owns at least one site: the owners that need a certificate of their own.
func OwnerLabelsWithSites(ctx context.Context, q Querier) ([]string, error) {
	return queryStrings(ctx, q, `
		SELECT DISTINCT lower(replace(u.username, '.', '-'))
		FROM users u JOIN sites s ON s.user_id = u.id
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
