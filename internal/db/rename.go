package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
	oldLabel := strings.ReplaceAll(strings.ToLower(oldName), ".", "-")
	// Before any check: a sign-in or another rename taking either label
	// waits for this transaction, then sees what it did (migration 0057).
	if err := LockOwnerLabels(ctx, tx, oldLabel, newName); err != nil {
		return "", nil, err
	}
	var taken, erased, heldByOther, legacyTeam bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM users WHERE lower(replace(username, '.', '-')) = $1 AND id <> $2::uuid),
			EXISTS (SELECT 1 FROM erased_owner_labels WHERE owner_label = $1),
			EXISTS (SELECT 1 FROM renamed_owner_labels WHERE owner_label = $1 AND user_id IS DISTINCT FROM $2::uuid),
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
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username = $2 WHERE id = $1::uuid`, userID, newName); err != nil {
		if isUniqueViolationErr(err) {
			return "", nil, errRenameUniqueRaced
		}
		return "", nil, fmt.Errorf("rename person: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM renamed_owner_labels WHERE owner_label = $1 AND user_id = $2::uuid`, newName, userID); err != nil {
		return "", nil, fmt.Errorf("release held label: %w", err)
	}
	// DO NOTHING, not DO UPDATE: the application role has no UPDATE on the
	// table, and Postgres checks that privilege for any DO UPDATE. The old
	// label cannot be held already: the trigger refuses a held username.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO renamed_owner_labels (owner_label, user_id) VALUES ($1, $2::uuid)
		ON CONFLICT (owner_label) DO NOTHING`, oldLabel, userID); err != nil {
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

// LockOwnerLabels takes the transaction-scoped lock every change to which
// labels are taken or held takes (owner_label_lock, migration 0057; the
// users triggers take it on every insert and username change), in a fixed
// order so two callers cannot deadlock.
func LockOwnerLabels(ctx context.Context, tx *sql.Tx, labels ...string) error {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	for i, label := range sorted {
		if i > 0 && label == sorted[i-1] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `SELECT owner_label_lock($1)`, label); err != nil {
			return fmt.Errorf("lock owner label: %w", err)
		}
	}
	return nil
}

// OwnerLabelHistory is label followed by every label the namespace that
// has it now held before an admin renamed it, so a person's Visitors
// history (access_log is keyed by label) carries across a rename. Just
// label when nobody has it or it was never renamed.
func OwnerLabelHistory(ctx context.Context, q Querier, label string) ([]string, error) {
	out := []string{label}
	rows, err := q.QueryContext(ctx, `
		SELECT h.owner_label FROM renamed_owner_labels h
		JOIN users u ON u.id = h.user_id
		WHERE lower(replace(u.username, '.', '-')) = $1
		ORDER BY h.renamed_at DESC, h.owner_label`, label)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var held string
		if err := rows.Scan(&held); err != nil {
			return nil, err
		}
		out = append(out, held)
	}
	return out, rows.Err()
}
