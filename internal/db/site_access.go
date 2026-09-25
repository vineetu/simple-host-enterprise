package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// A site's access level (sites.access, migration 0033): who can open it.
const (
	AccessOnlyMe   = "only_me"  // the owner; for a team site, its members
	AccessSpecific = "specific" // plus named viewers, on the site's own host
	AccessCompany  = "company"  // any signed-in person with the link
	AccessListed   = "listed"   // company, plus the showcase and search
	AccessNetwork  = "network"  // anyone, no sign-in; only after an admin approves
)

// ValidAccessLevel reports whether level names one of the five levels.
func ValidAccessLevel(level string) bool {
	switch level {
	case AccessOnlyMe, AccessSpecific, AccessCompany, AccessListed, AccessNetwork:
		return true
	}
	return false
}

// ErrNoPendingRequest is an approve or decline for a site with no pending
// network-access request.
var ErrNoPendingRequest = errors.New("no pending network access request")

// ErrAlreadyNetwork is a network-access request for a site already open to
// the network.
var ErrAlreadyNetwork = errors.New("site is already open to the network")

// setSiteAccess sets a site's level, keeps sites.public equal to "listed or
// wider", and clears any pending network-access request. It returns the
// level the site had before. It never sets AccessNetwork for a caller that
// is not ApproveNetworkAccess: SetSiteAccess refuses it.
func setSiteAccess(ctx context.Context, q Querier, siteID, level string) (string, error) {
	var previous string
	if err := q.QueryRowContext(ctx, `SELECT access FROM sites WHERE id = $1::uuid FOR UPDATE`, siteID).Scan(&previous); err != nil {
		return "", err
	}
	_, err := q.ExecContext(ctx, `
		UPDATE sites
		SET access = $2, public = ($2 IN ('listed', 'network')),
		    network_requested_at = NULL, network_requested_by = NULL, network_request_reason = NULL
		WHERE id = $1::uuid`, siteID, level)
	return previous, err
}

// SetSiteAccess moves a site to any level but network, which only an
// admin's approval sets. Moving a site anywhere withdraws a pending
// network-access request, and moving a network site anywhere revokes its
// approval: going back up needs a new request.
func SetSiteAccess(ctx context.Context, tx *sql.Tx, siteID, level string) (previous string, err error) {
	if !ValidAccessLevel(level) || level == AccessNetwork {
		return "", errors.New("db: SetSiteAccess: invalid level " + level)
	}
	return setSiteAccess(ctx, tx, siteID, level)
}

// RequestNetworkAccess records a pending request to open a site to the
// network. The site keeps its current level until an admin approves. A
// second request replaces the first.
func RequestNetworkAccess(ctx context.Context, tx *sql.Tx, siteID, requestedBy, reason string) error {
	var access string
	if err := tx.QueryRowContext(ctx, `SELECT access FROM sites WHERE id = $1::uuid FOR UPDATE`, siteID).Scan(&access); err != nil {
		return err
	}
	if access == AccessNetwork {
		return ErrAlreadyNetwork
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE sites
		SET network_requested_at = now(), network_requested_by = $2::uuid, network_request_reason = $3
		WHERE id = $1::uuid`, siteID, requestedBy, reason)
	return err
}

// ApproveNetworkAccess opens a site with a pending request to the network.
func ApproveNetworkAccess(ctx context.Context, tx *sql.Tx, siteID string) (previous string, err error) {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT access, network_requested_at IS NOT NULL FROM sites WHERE id = $1::uuid FOR UPDATE`, siteID).Scan(&previous, &pending); err != nil {
		return "", err
	}
	if !pending {
		return "", ErrNoPendingRequest
	}
	_, err = setSiteAccess(ctx, tx, siteID, AccessNetwork)
	return previous, err
}

// DeclineNetworkAccess clears a pending request; the site's level is
// unchanged.
func DeclineNetworkAccess(ctx context.Context, tx *sql.Tx, siteID string) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE sites
		SET network_requested_at = NULL, network_requested_by = NULL, network_request_reason = NULL
		WHERE id = $1::uuid AND network_requested_at IS NOT NULL`, siteID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNoPendingRequest
	}
	return nil
}

// NetworkOpen reports whether a site is open to anonymous visitors. A site
// that no longer exists is not.
func NetworkOpen(ctx context.Context, q Querier, siteID string) (bool, error) {
	var open bool
	err := q.QueryRowContext(ctx, `SELECT access = 'network' FROM sites WHERE id = $1::uuid`, siteID).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return open, err
}

// NetworkAccessEntry is a site with a pending network-access request, or one
// already open to the network, for the admin page and the admin API.
type NetworkAccessEntry struct {
	SiteID      string
	Owner       string
	SiteName    string
	Access      string
	RequestedBy string
	Reason      string
	RequestedAt *time.Time
}

// ListNetworkAccess returns every pending request (oldest first) and every
// site already open to the network.
func ListNetworkAccess(ctx context.Context, q Querier) ([]NetworkAccessEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, owner.username, s.name, s.access,
		       COALESCE(requester.username, ''), COALESCE(s.network_request_reason, ''), s.network_requested_at
		FROM sites s
		JOIN users owner ON owner.id = s.user_id
		LEFT JOIN users requester ON requester.id = s.network_requested_by
		WHERE s.network_requested_at IS NOT NULL OR s.access = 'network'
		ORDER BY s.network_requested_at ASC NULLS LAST, owner.username, s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NetworkAccessEntry
	for rows.Next() {
		var e NetworkAccessEntry
		if err := rows.Scan(&e.SiteID, &e.Owner, &e.SiteName, &e.Access, &e.RequestedBy, &e.Reason, &e.RequestedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxStateHistory is how many saved states each site keeps.
const MaxStateHistory = 20

// StateHistoryEntry is one retained saved state.
type StateHistoryEntry struct {
	ID           int64
	StateVersion int64
	WrittenBy    string
	CreatedAt    time.Time
	Bytes        int
	State        json.RawMessage
}

// RecordStateHistory copies a site's current saved state into its history
// and prunes the history to MaxStateHistory, in the caller's transaction,
// right after the write that produced it.
func RecordStateHistory(ctx context.Context, tx *sql.Tx, siteID, writtenBy string) error {
	var by any
	if writtenBy != "" {
		by = writtenBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO site_state_history (site_id, state_version, state, written_by)
		SELECT id, state_version, state, $2::uuid FROM sites WHERE id = $1::uuid`, siteID, by); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		DELETE FROM site_state_history
		WHERE site_id = $1::uuid
		  AND id NOT IN (SELECT id FROM site_state_history WHERE site_id = $1::uuid ORDER BY id DESC LIMIT $2)`,
		siteID, MaxStateHistory)
	return err
}

// ListStateHistory returns a site's retained saved states, newest first,
// without their contents.
func ListStateHistory(ctx context.Context, q Querier, siteID string) ([]StateHistoryEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT h.id, h.state_version, COALESCE(u.username, ''), h.created_at, octet_length(h.state::text)
		FROM site_state_history h
		LEFT JOIN users u ON u.id = h.written_by
		WHERE h.site_id = $1::uuid
		ORDER BY h.id DESC`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateHistoryEntry
	for rows.Next() {
		var e StateHistoryEntry
		if err := rows.Scan(&e.ID, &e.StateVersion, &e.WrittenBy, &e.CreatedAt, &e.Bytes); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetStateHistory returns one retained saved state with its contents.
// sql.ErrNoRows covers an id that is not this site's.
func GetStateHistory(ctx context.Context, q Querier, siteID string, id int64) (StateHistoryEntry, error) {
	var e StateHistoryEntry
	err := q.QueryRowContext(ctx, `
		SELECT h.id, h.state_version, COALESCE(u.username, ''), h.created_at, octet_length(h.state::text), h.state
		FROM site_state_history h
		LEFT JOIN users u ON u.id = h.written_by
		WHERE h.site_id = $1::uuid AND h.id = $2`, siteID, id).Scan(
		&e.ID, &e.StateVersion, &e.WrittenBy, &e.CreatedAt, &e.Bytes, &e.State)
	return e, err
}

// RestoreStateHistory makes a retained saved state current again as a new
// write: the state version moves forward, so a page holding the old version
// gets a conflict rather than overwriting the restore, and the restore
// itself lands in the history. sql.ErrNoRows covers an id that is not this
// site's.
func RestoreStateHistory(ctx context.Context, tx *sql.Tx, siteID string, id int64, actorID string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `
		UPDATE sites s
		SET state = h.state, state_version = s.state_version + 1, updated_at = now()
		FROM site_state_history h
		WHERE s.id = $1::uuid AND h.site_id = s.id AND h.id = $2
		RETURNING s.state_version`, siteID, id).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, RecordStateHistory(ctx, tx, siteID, actorID)
}
