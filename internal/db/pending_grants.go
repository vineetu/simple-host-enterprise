package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/lib/pq"
)

// Pending grants: a site viewer or team member named by company email before
// that person has an account. Each is a row keyed by the lower-cased address
// (migration 0045) that becomes the real grant at the person's first sign-in
// with a verified matching email (ConvertPendingGrants). Rows go with their
// site or team (ON DELETE CASCADE).

// ErrAmbiguousEmail refuses an email that more than one account carries:
// which of them was meant is not something to guess.
var ErrAmbiguousEmail = errors.New("more than one account has that email")

// IsEmailName reports whether a name given where a username is accepted is
// an email address rather than a username. Usernames never contain "@".
func IsEmailName(name string) bool {
	return strings.Contains(name, "@")
}

// splitEmailNames separates the email addresses from the usernames in an
// already normalized batch, keeping each list's order.
func splitEmailNames(names []string) (usernames, emails []string) {
	for _, name := range names {
		if IsEmailName(name) {
			emails = append(emails, name)
		} else {
			usernames = append(usernames, name)
		}
	}
	return usernames, emails
}

const resolveEmailsQuery = `
	SELECT lower(email), min(username), count(*)::int
	FROM users
	WHERE lower(email) = ANY($1::text[])
	  AND kind = 'person'
	  AND email_source = 'claimed'
	GROUP BY lower(email)
`

// resolveEmails maps each address that one person's account carries to that
// account's username, counting only addresses the identity provider vouched
// for at a sign-in (email_source 'claimed'; never an inferred or
// reset-request guess); the addresses no account carries come back as
// pending, in their given order. An address two accounts carry is refused.
func resolveEmails(ctx context.Context, q Querier, emails []string) (usernames map[string]string, pending []string, err error) {
	usernames = make(map[string]string, len(emails))
	if len(emails) == 0 {
		return usernames, nil, nil
	}
	rows, err := q.QueryContext(ctx, resolveEmailsQuery, pq.Array(emails))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email, username string
		var count int
		if err := rows.Scan(&email, &username, &count); err != nil {
			return nil, nil, fmt.Errorf("resolve emails: %w", err)
		}
		if count > 1 {
			return nil, nil, ErrAmbiguousEmail
		}
		usernames[email] = username
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("resolve emails: %w", err)
	}
	for _, email := range emails {
		if _, ok := usernames[email]; !ok {
			pending = append(pending, email)
		}
	}
	return usernames, pending, nil
}

// resolveEmailNames replaces every address in a normalized batch that
// belongs to an account with that account's username, and returns the
// addresses nobody has signed in with yet separately.
func resolveEmailNames(ctx context.Context, q Querier, names []string) (usernames, pending []string, err error) {
	usernames, emails := splitEmailNames(names)
	byEmail, pending, err := resolveEmails(ctx, q, emails)
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[string]bool, len(usernames))
	for _, u := range usernames {
		seen[u] = true
	}
	for _, email := range emails {
		if u, ok := byEmail[email]; ok && !seen[u] {
			usernames = append(usernames, u)
			seen[u] = true
		}
	}
	return usernames, pending, nil
}

const addPendingSiteViewersQuery = `
	INSERT INTO pending_site_viewers (site_id, email, added_by)
	SELECT $1::uuid, requested.email, $3::uuid
	FROM unnest($2::text[]) AS requested(email)
	ON CONFLICT (site_id, email) DO NOTHING
`

const removePendingSiteViewerQuery = `
	DELETE FROM pending_site_viewers WHERE site_id = $1::uuid AND email = $2
`

const addPendingTeamMembersQuery = `
	INSERT INTO pending_team_members (team_id, email, added_by)
	SELECT $1::uuid, requested.email, $3::uuid
	FROM unnest($2::text[]) AS requested(email)
	ON CONFLICT (team_id, email) DO NOTHING
`

const countPendingTeamMembersQuery = `
	SELECT count(*)::int FROM pending_team_members WHERE team_id = $1::uuid AND email = ANY($2::text[])
`

const removePendingTeamMemberQuery = `
	DELETE FROM pending_team_members WHERE team_id = $1::uuid AND email = $2
`

// RemovePendingTeamMember deletes a team's pending grant for one address,
// reporting whether there was one. The caller holds LockTeam.
func RemovePendingTeamMember(ctx context.Context, q Querier, teamID, email string) (bool, error) {
	result, err := q.ExecContext(ctx, removePendingTeamMemberQuery, teamID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return false, fmt.Errorf("remove pending team member: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remove pending team member: %w", err)
	}
	return affected > 0, nil
}

// ConvertedGrant is one pending grant turned into the real one at sign-in:
// a site viewer (SiteID and its OwnerID set) or a team membership (TeamID).
type ConvertedGrant struct {
	SiteID  string
	OwnerID string
	TeamID  string
}

const takePendingSiteViewersQuery = `
	DELETE FROM pending_site_viewers p
	USING sites s
	WHERE p.email = $1 AND s.id = p.site_id
	RETURNING p.site_id::text, s.user_id::text, p.added_by::text
`

const takePendingTeamMembersQuery = `
	DELETE FROM pending_team_members
	WHERE email = $1
	RETURNING team_id::text, added_by::text
`

const convertSiteViewerQuery = `
	INSERT INTO site_viewers (site_id, principal_id, added_by)
	VALUES ($1::uuid, $2::uuid, $3::uuid)
	ON CONFLICT (site_id, principal_id) DO NOTHING
`

const convertTeamMemberQuery = `
	INSERT INTO team_members (team_id, user_id, added_by)
	VALUES ($1::uuid, $2::uuid, $3::uuid)
	ON CONFLICT (team_id, user_id) DO NOTHING
`

// ConvertPendingGrants turns every pending grant for email into the real
// grant for userID and deletes the pending rows, in tx. The caller must have
// the provider's word that userID holds email (a verified email claim at
// sign-in); on top of that, nothing converts unless email is plain ASCII
// (grant emails are, and Unicode case folding must never make two addresses
// meet) and is the address userID's account now holds from a claim (not one
// the sign-in's refresh refused because another account holds it). Matching
// is exact on the lower-cased address. Each pending grant was counted against its site's or team's cap
// when it was added, so converting one never exceeds it.
func ConvertPendingGrants(ctx context.Context, tx *sql.Tx, userID, email string) ([]ConvertedGrant, error) {
	email = strings.TrimSpace(email)
	if email == "" || strings.IndexFunc(email, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		return nil, nil
	}
	email = strings.ToLower(email)
	var holds bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1::uuid AND email = $2 AND email_source = 'claimed')`, userID, email).Scan(&holds); err != nil {
		return nil, fmt.Errorf("check account email: %w", err)
	}
	if !holds {
		return nil, nil
	}
	type taken struct {
		grant   ConvertedGrant
		addedBy sql.NullString
	}
	var all []taken

	rows, err := tx.QueryContext(ctx, takePendingSiteViewersQuery, email)
	if err != nil {
		return nil, fmt.Errorf("take pending site viewers: %w", err)
	}
	for rows.Next() {
		var t taken
		if err := rows.Scan(&t.grant.SiteID, &t.grant.OwnerID, &t.addedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("take pending site viewers: %w", err)
		}
		all = append(all, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, takePendingTeamMembersQuery, email)
	if err != nil {
		return nil, fmt.Errorf("take pending team members: %w", err)
	}
	for rows.Next() {
		var t taken
		if err := rows.Scan(&t.grant.TeamID, &t.addedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("take pending team members: %w", err)
		}
		all = append(all, t)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	converted := make([]ConvertedGrant, 0, len(all))
	for _, t := range all {
		var addedBy any
		if t.addedBy.Valid {
			addedBy = t.addedBy.String
		}
		if t.grant.SiteID != "" {
			_, err = tx.ExecContext(ctx, convertSiteViewerQuery, t.grant.SiteID, userID, addedBy)
		} else {
			_, err = tx.ExecContext(ctx, convertTeamMemberQuery, t.grant.TeamID, userID, addedBy)
		}
		if err != nil {
			return nil, fmt.Errorf("convert pending grant: %w", err)
		}
		converted = append(converted, t.grant)
	}
	return converted, nil
}
