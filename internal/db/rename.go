package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// Why a person's address cannot take a new name. Each is refused before
// anything changes.
var (
	ErrRenameNotPerson   = errors.New("only a person's address can be renamed")
	ErrRenameSameName    = errors.New("that is already their name")
	ErrRenameNameTaken   = errors.New("another person or team has that name")
	ErrRenameNameHeld    = errors.New("that name is held: it belonged to someone else before")
	ErrRenameLegacyTeam  = errors.New("that name is a team's old address")
	ErrRenameTeamPrefix  = errors.New(`names beginning with "team-" are for teams`)
	errRenameUniqueRaced = errors.New("that name was taken while renaming")
)

// RenamedSite is one of the renamed person's sites, for the caller's
// follow-up work (its redirect was recorded; its search entry and bucket
// manifest follow).
type RenamedSite struct {
	ID      string
	Name    string
	Deleted bool
}

// RenamePerson gives the person userID the username newName, in tx, and
// keeps everything that pointed at the old one working:
//
//   - the old label is held (renamed_owner_labels), so nobody else signs in
//     as it and inherits the links; a label this person held before is theirs
//     to take back, and its hold is released;
//   - every site they own (Recently deleted included, so a restore still
//     redirects) records its old "<site part>.<old label>" address in
//     site_redirects, which the host gate follows only for people allowed
//     to open the site, and which keeps the old label's certificate managed;
//   - addresses under the new label that redirected to one of their sites
//     are cleared: a live site is served there now.
//
// sitePart maps a site name to its host label (handler.siteHostPart). The
// users row is updated first, so a move or team change touching the person
// waits for this transaction. The label is the username itself: the caller
// allows no dots.
func RenamePerson(ctx context.Context, tx *sql.Tx, userID, newName string, sitePart func(string) string) (oldName string, sites []RenamedSite, err error) {
	if strings.HasPrefix(newName, TeamPrefix) {
		return "", nil, ErrRenameTeamPrefix
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT username, kind FROM users WHERE id = $1::uuid FOR UPDATE`, userID).Scan(&oldName, &kind); err != nil {
		return "", nil, err
	}
	if kind != "person" {
		return "", nil, ErrRenameNotPerson
	}
	if oldName == newName {
		return "", nil, ErrRenameSameName
	}
	var taken, erased, heldByOther, legacyTeam bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM users WHERE lower(replace(username, '.', '-')) = $1 AND id <> $2::uuid),
			EXISTS (SELECT 1 FROM erased_owner_labels WHERE owner_label = $1),
			EXISTS (SELECT 1 FROM renamed_owner_labels WHERE owner_label = $1 AND user_id <> $2::uuid),
			EXISTS (SELECT 1 FROM users WHERE kind = 'team' AND username = 'team-' || $1)`,
		newName, userID).Scan(&taken, &erased, &heldByOther, &legacyTeam); err != nil {
		return "", nil, err
	}
	switch {
	case taken:
		return "", nil, ErrRenameNameTaken
	case erased || heldByOther:
		return "", nil, ErrRenameNameHeld
	case legacyTeam:
		return "", nil, ErrRenameLegacyTeam
	}
	oldLabel := strings.ReplaceAll(strings.ToLower(oldName), ".", "-")
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username = $2 WHERE id = $1::uuid`, userID, newName); err != nil {
		if isUniqueViolationErr(err) {
			return "", nil, errRenameUniqueRaced
		}
		return "", nil, fmt.Errorf("rename person: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM renamed_owner_labels WHERE owner_label = $1 AND user_id = $2::uuid`, newName, userID); err != nil {
		return "", nil, fmt.Errorf("release held label: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO renamed_owner_labels (owner_label, user_id) VALUES ($1, $2::uuid)
		ON CONFLICT (owner_label) DO UPDATE SET user_id = EXCLUDED.user_id, renamed_at = now()`, oldLabel, userID); err != nil {
		return "", nil, fmt.Errorf("hold old label: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id::text, name, deleted_at IS NOT NULL FROM sites WHERE user_id = $1::uuid ORDER BY name`, userID)
	if err != nil {
		return "", nil, fmt.Errorf("list sites: %w", err)
	}
	for rows.Next() {
		var s RenamedSite
		if err := rows.Scan(&s.ID, &s.Name, &s.Deleted); err != nil {
			rows.Close()
			return "", nil, err
		}
		sites = append(sites, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	for _, s := range sites {
		part := sitePart(s.Name)
		if _, err := tx.ExecContext(ctx, recordSiteRedirectQuery, oldLabel, part, s.ID); err != nil {
			return "", nil, fmt.Errorf("record site redirect: %w", err)
		}
		if _, err := tx.ExecContext(ctx, clearSiteRedirectQuery, newName, part); err != nil {
			return "", nil, fmt.Errorf("clear site redirect: %w", err)
		}
	}
	return oldName, sites, nil
}

// RenameFailed reports whether err is one of RenamePerson's refusals, and
// the words to show for it.
func RenameFailed(err error) (string, bool) {
	for _, known := range []error{ErrRenameNotPerson, ErrRenameSameName, ErrRenameNameTaken, ErrRenameNameHeld, ErrRenameLegacyTeam, ErrRenameTeamPrefix, errRenameUniqueRaced} {
		if errors.Is(err, known) {
			return known.Error(), true
		}
	}
	return "", false
}

func isUniqueViolationErr(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// RenamedOwner reports whose address label was before an admin renamed
// them: that person's current username. ok is false for a label nobody was
// renamed away from.
func RenamedOwner(ctx context.Context, q Querier, label string) (username string, ok bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT u.username FROM renamed_owner_labels h JOIN users u ON u.id = h.user_id
		WHERE h.owner_label = $1`, label).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return username, err == nil, err
}
