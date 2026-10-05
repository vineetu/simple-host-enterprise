package db

import (
	"context"
	"database/sql"
)

type PersonPresentation struct {
	Home *string `json:"site"`
	Bio  string  `json:"bio"`
}
type ShowcasePreference struct {
	Pinned bool `json:"pinned"`
	Order  int  `json:"order"`
}

func GetPersonPresentation(ctx context.Context, q Querier, userID string) (PersonPresentation, error) {
	var p PersonPresentation
	var name sql.NullString
	err := q.QueryRowContext(ctx, `SELECT s.name, u.showcase_bio FROM users u
 LEFT JOIN sites s ON s.id=u.home_site AND s.user_id=u.id AND s.deleted_at IS NULL
 WHERE u.id=$1::uuid`, userID).Scan(&name, &p.Bio)
	if name.Valid {
		p.Home = &name.String
	}
	return p, err
}

// ServingPersonHome excludes disabled owners, unpublished, deleted and taken-down sites.
func ServingPersonHome(ctx context.Context, q Querier, username string) (string, error) {
	var name string
	err := q.QueryRowContext(ctx, `SELECT s.name FROM users u JOIN sites s ON s.id=u.home_site AND s.user_id=u.id
 WHERE u.username=$1 AND u.kind='person' AND u.disabled_at IS NULL AND s.deleted_at IS NULL
 AND s.active_version>0 AND COALESCE(s.access_decision,'')<>'restricted'`, username).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}
func SetPersonHome(ctx context.Context, q Querier, userID string, name *string) error {
	var id sql.NullString
	if name != nil {
		if err := q.QueryRowContext(ctx, `SELECT id::text FROM sites WHERE user_id=$1::uuid AND name=$2 AND deleted_at IS NULL FOR UPDATE`, userID, *name).Scan(&id); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx, `UPDATE users SET home_site=$2::uuid WHERE id=$1::uuid AND kind='person'`, userID, id)
	return err
}
func SetPersonBio(ctx context.Context, q Querier, userID, bio string) error {
	_, err := q.ExecContext(ctx, `UPDATE users SET showcase_bio=$2 WHERE id=$1::uuid AND kind='person'`, userID, bio)
	return err
}
func GetShowcasePreferences(ctx context.Context, q Querier, userID string) (map[string]ShowcasePreference, error) {
	rows, err := q.QueryContext(ctx, `SELECT name,showcase_pinned,showcase_order FROM sites WHERE user_id=$1::uuid AND deleted_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ShowcasePreference{}
	for rows.Next() {
		var name string
		var pref ShowcasePreference
		if err := rows.Scan(&name, &pref.Pinned, &pref.Order); err != nil {
			return nil, err
		}
		out[name] = pref
	}
	return out, rows.Err()
}
func SetShowcasePreference(ctx context.Context, q Querier, userID, name string, pinned *bool, order *int) (ShowcasePreference, error) {
	var pref ShowcasePreference
	err := q.QueryRowContext(ctx, `UPDATE sites SET showcase_pinned=COALESCE($3,showcase_pinned),showcase_order=COALESCE($4,showcase_order)
 WHERE user_id=$1::uuid AND name=$2 AND deleted_at IS NULL RETURNING showcase_pinned,showcase_order`, userID, name, pinned, order).Scan(&pref.Pinned, &pref.Order)
	return pref, err
}
