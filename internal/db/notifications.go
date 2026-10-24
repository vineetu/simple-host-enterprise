package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"
)

var ErrNotificationCap = errors.New("too many notifications")
var ErrNotificationSite = errors.New("subscription site changed")

type Notification struct {
	ID              string     `json:"id"`
	SiteOwnerID     string     `json:"-"`
	SiteID          string     `json:"site_id"`
	OwnerID         string     `json:"owner_id"`
	Site            string     `json:"site"`
	Source          string     `json:"source"`
	GrantID         *string    `json:"grant_id"`
	AppName         string     `json:"app_name"`
	Label           string     `json:"label"`
	URL             string     `json:"url"`
	Events          []string   `json:"events"`
	Storage         string     `json:"storage"`
	ArgsCanonical   string     `json:"args_canonical"`
	Secret          string     `json:"secret"`
	PrevSecret      string     `json:"prev_secret"`
	PrevSecretUntil *time.Time `json:"prev_secret_until"`
	AuthHeaderName  string     `json:"auth_header_name"`
	AuthHeaderValue string     `json:"auth_header_value"`
	State           string     `json:"state"`
	VerifiedAt      *time.Time `json:"verified_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	FailingSince    *time.Time `json:"failing_since"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastResult      string     `json:"last_result"`
	CreatedAt       time.Time  `json:"created_at"`
}

const notificationColumns = `n.id,n.site_owner_id,n.site_id,n.owner_id,s.name,n.source,n.grant_id,n.app_name,n.label,n.url,n.events,n.storage,n.args_canonical,n.secret,n.prev_secret,n.prev_secret_until,n.auth_header_name,n.auth_header_value,n.state,n.verified_at,n.expires_at,n.failing_since,n.last_attempt_at,n.last_success_at,n.last_result,n.created_at`

func scanNotification(row interface{ Scan(...any) error }) (Notification, error) {
	var n Notification
	err := row.Scan(&n.ID, &n.SiteOwnerID, &n.SiteID, &n.OwnerID, &n.Site, &n.Source, &n.GrantID, &n.AppName, &n.Label, &n.URL, pq.Array(&n.Events), &n.Storage, &n.ArgsCanonical, &n.Secret, &n.PrevSecret, &n.PrevSecretUntil, &n.AuthHeaderName, &n.AuthHeaderValue, &n.State, &n.VerifiedAt, &n.ExpiresAt, &n.FailingSince, &n.LastAttemptAt, &n.LastSuccessAt, &n.LastResult, &n.CreatedAt)
	return n, err
}
func GetNotification(ctx context.Context, q Querier, id string) (Notification, error) {
	return scanNotification(q.QueryRowContext(ctx, `SELECT `+notificationColumns+` FROM site_notifications n JOIN sites s ON s.id=n.site_id WHERE n.id=$1`, id))
}
func ListNotifications(ctx context.Context, database *sql.DB, owner, site string) ([]Notification, error) {
	rows, err := database.QueryContext(ctx, `SELECT `+notificationColumns+` FROM site_notifications n JOIN sites s ON s.id=n.site_id WHERE (s.user_id=$1 OR EXISTS(SELECT 1 FROM team_members tm WHERE tm.team_id=s.user_id AND tm.user_id=$1)) AND s.deleted_at IS NULL AND ($2='' OR n.site_id::text=$2) ORDER BY n.created_at DESC`, owner, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotificationSecrets uses the handler's notification AEAD without giving the DB a key.
type NotificationSecrets struct {
	Seal func(id, plaintext string) (string, error)
	Open func(id, stored string) (string, error)
}

// sealLegacy preserves ciphertext and empty values, including during rotation.
func sealLegacy(id, value string, seal func(string, string) (string, error)) (string, error) {
	if value == "" || strings.HasPrefix(value, "enc1:") {
		return value, nil
	}
	return seal(id, value)
}

// SaveNotification accepts plaintext secrets and stores only sealed values.
// Lock the site before counting, so concurrent REST and MCP creates share the cap.
func SaveNotification(ctx context.Context, database *sql.DB, n Notification, max int, cursor *int64, historyDays int, secrets NotificationSecrets, audit ...NotificationMutation) (bool, bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer rollbackNotificationMutation(tx, audit)
	if err = LockSiteStorage(ctx, tx, n.SiteID); err != nil {
		return false, false, err
	}
	var site string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM sites WHERE id=$1 FOR UPDATE`, n.SiteID).Scan(&site); err != nil {
		return false, false, err
	}
	var oldSite, oldState, oldSecret, oldPrev, oldAuth string
	err = tx.QueryRowContext(ctx, `SELECT site_id,state,secret,prev_secret,auth_header_value FROM site_notifications WHERE id=$1 FOR UPDATE`, n.ID).Scan(&oldSite, &oldState, &oldSecret, &oldPrev, &oldAuth)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	if oldSite != "" && oldSite != n.SiteID {
		return false, false, ErrNotificationSite
	}
	var exists bool
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_notifications WHERE id=$1), (SELECT count(*) FROM site_notifications WHERE site_id=$2)`, n.ID, n.SiteID).Scan(&exists, &count); err != nil {
		return false, false, err
	}
	if !exists && count >= max {
		return false, false, ErrNotificationCap
	}
	// Compare plaintext under the row lock: random nonces cannot indicate a change.
	changed := false
	if exists {
		// A secret this key cannot open (the key changed) is replaced, and no
		// unreadable previous secret is kept.
		plain, err := secrets.Open(n.ID, oldSecret)
		changed = err != nil || plain != n.Secret
		switch {
		case err != nil:
			oldPrev = ""
		case changed:
			oldPrev = oldSecret
		}
		n.AuthHeaderValue, err = sealLegacy(n.ID+":header", oldAuth, secrets.Seal)
		if err != nil {
			return false, false, err
		}
	} else {
		n.AuthHeaderValue, err = secrets.Seal(n.ID+":header", n.AuthHeaderValue)
		if err != nil {
			return false, false, err
		}
	}
	n.Secret, err = secrets.Seal(n.ID, n.Secret)
	if err != nil {
		return false, false, err
	}
	oldPrev, err = sealLegacy(n.ID, oldPrev, secrets.Seal)
	if err != nil {
		return false, false, err
	}
	saved, err := tx.ExecContext(ctx, `INSERT INTO site_notifications(id,site_owner_id,site_id,owner_id,source,grant_id,app_name,label,url,events,storage,args_canonical,secret,auth_header_name,auth_header_value,verified_at,expires_at)
 VALUES($1,(SELECT user_id FROM sites WHERE id=$2),$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
 ON CONFLICT(id) DO UPDATE SET prev_secret=$18,
 prev_secret_until=CASE WHEN $17 THEN now()+interval '24 hours' ELSE site_notifications.prev_secret_until END,
 secret=EXCLUDED.secret,auth_header_value=EXCLUDED.auth_header_value,expires_at=EXCLUDED.expires_at,verified_at=EXCLUDED.verified_at,grant_id=EXCLUDED.grant_id,app_name=EXCLUDED.app_name,state='active',suspended_at=NULL,failing_since=NULL,window_start=NULL,window_attempts=0,window_failures=0,updated_at=now() WHERE site_notifications.site_id=EXCLUDED.site_id`, n.ID, n.SiteID, n.OwnerID, n.Source, n.GrantID, n.AppName, n.Label, n.URL, pq.Array(n.Events), n.Storage, n.ArgsCanonical, n.Secret, n.AuthHeaderName, n.AuthHeaderValue, n.VerifiedAt, n.ExpiresAt, changed, oldPrev)
	if err != nil {
		return false, false, err
	}
	if rows, err := saved.RowsAffected(); err != nil {
		return false, false, err
	} else if rows == 0 {
		return false, false, ErrNotificationSite
	}
	if oldState == "paused" || oldState == "suspended" {
		_, err = tx.ExecContext(ctx, `UPDATE site_event_deliveries SET next_attempt_at=now() WHERE subscription_id=$1 AND state='pending' AND (lease_until IS NULL OR lease_until<=now())`, n.ID)
		if err != nil {
			return false, false, err
		}
	}
	truncated := false
	if cursor != nil {
		var oldest sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT min(seq) FROM site_events WHERE site_id=$1 AND created_at>=now()-$2*interval '1 day'`, n.SiteID, historyDays).Scan(&oldest); err != nil {
			return false, false, err
		}
		truncated = oldest.Valid && *cursor < oldest.Int64
		_, err = tx.ExecContext(ctx, `INSERT INTO site_event_deliveries(event_seq,subscription_id) SELECT seq,$1 FROM site_events WHERE site_id=$2 AND seq>$3 AND name=ANY($4) AND ($5='' OR storage=$5) AND created_at>=now()-$6*interval '1 day' ON CONFLICT DO NOTHING`, n.ID, n.SiteID, *cursor, pq.Array(n.Events), n.Storage, historyDays)
		if err != nil {
			return false, false, err
		}
	}
	if err := notificationRecord(tx, audit); err != nil {
		return false, false, err
	}
	return exists, truncated, commitNotificationMutation(tx, audit)
}

func DeleteNotification(ctx context.Context, q Querier, id string) (bool, error) {
	r, err := q.ExecContext(ctx, `DELETE FROM site_notifications WHERE id=$1`, id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// LockNotificationSite keeps mutation lock order consistent with subscription
// creation: site, subscription, and then audit chain.
func LockNotificationSite(ctx context.Context, tx *sql.Tx, id string) error {
	var site string
	return tx.QueryRowContext(ctx, `SELECT s.id FROM sites s JOIN site_notifications n ON n.site_id=s.id WHERE n.id=$1 FOR SHARE OF s`, id).Scan(&site)
}

func PauseNotification(ctx context.Context, database *sql.DB, id string, paused bool, audit ...NotificationMutation) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackNotificationMutation(tx, audit)
	if err = LockNotificationSite(ctx, tx, id); err != nil {
		return err
	}
	var oldState string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM site_notifications WHERE id=$1 FOR UPDATE`, id).Scan(&oldState); err != nil {
		return err
	}
	state := "active"
	if paused {
		state = "paused"
	}
	_, err = tx.ExecContext(ctx, `UPDATE site_notifications SET state=$2,updated_at=now(),suspended_at=NULL,failing_since=NULL,window_start=NULL,window_attempts=0,window_failures=0 WHERE id=$1`, id, state)
	if err != nil {
		return err
	}
	if !paused && (oldState == "paused" || oldState == "suspended") {
		_, err = tx.ExecContext(ctx, `UPDATE site_event_deliveries SET next_attempt_at=now() WHERE subscription_id=$1 AND state='pending' AND (lease_until IS NULL OR lease_until<=now())`, id)
		if err != nil {
			return err
		}
	}
	if err := notificationRecord(tx, audit); err != nil {
		return err
	}
	return commitNotificationMutation(tx, audit)
}

// RotateNotification accepts a plaintext replacement and keeps a readable current
// secret for the rotation grace period. Both signing secrets use the same AD.
func RotateNotification(ctx context.Context, database *sql.DB, id, secret string, secrets NotificationSecrets, audit ...NotificationMutation) (*time.Time, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackNotificationMutation(tx, audit)
	if err = LockNotificationSite(ctx, tx, id); err != nil {
		return nil, err
	}
	var old, header string
	if err = tx.QueryRowContext(ctx, `SELECT secret,auth_header_value FROM site_notifications WHERE id=$1 FOR UPDATE`, id).Scan(&old, &header); err != nil {
		return nil, err
	}
	if _, err := secrets.Open(id, old); err != nil {
		old = ""
	}
	old, err = sealLegacy(id, old, secrets.Seal)
	if err != nil {
		return nil, err
	}
	header, err = sealLegacy(id+":header", header, secrets.Seal)
	if err != nil {
		return nil, err
	}
	secret, err = secrets.Seal(id, secret)
	if err != nil {
		return nil, err
	}
	var until *time.Time
	err = tx.QueryRowContext(ctx, `UPDATE site_notifications SET prev_secret=$3,secret=$2,auth_header_value=$4,
 prev_secret_until=CASE WHEN $3='' THEN NULL ELSE now()+interval '24 hours' END,updated_at=now() WHERE id=$1 RETURNING prev_secret_until`, id, secret, old, header).Scan(&until)
	if err != nil {
		return nil, err
	}
	if err := notificationRecord(tx, audit); err != nil {
		return nil, err
	}
	return until, commitNotificationMutation(tx, audit)
}

func HasNotifications(ctx context.Context, q Querier, site string) (bool, error) {
	var yes bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_notifications n JOIN users u ON u.id=n.owner_id WHERE n.site_id=$1 AND n.state IN ('active','suspended') AND u.disabled_at IS NULL AND (n.expires_at IS NULL OR n.expires_at>now()))`, site).Scan(&yes)
	return yes, err
}

// EmitSiteEventTx commits the outbox with the visitor's mutation. The caller
// already holds the site storage lock; subscription creation locks that same row.
func EmitSiteEventTx(ctx context.Context, tx *sql.Tx, site, id, name, storage string, data []byte) error {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO site_events(site_id,event_id,name,storage,data) VALUES($1,$2,$3,$4,$5) RETURNING seq`, site, id, name, storage, data).Scan(&seq)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO site_event_deliveries(event_seq,subscription_id) SELECT $1,id FROM site_notifications WHERE site_id=$2 AND state IN ('active','suspended') AND $3=ANY(events) AND (storage='' OR storage=$4) AND (expires_at IS NULL OR expires_at>now())`, seq, site, name, storage)
	return err
}

type NotificationDelivery struct {
	Seq            int64
	SubscriptionID string
	Attempts       int
	LeaseUntil     time.Time
	LastAttemptAt  time.Time
	EventID        string
	Name           string
	Data           json.RawMessage
	CreatedAt      time.Time
}

func ClaimNotificationDelivery(ctx context.Context, database *sql.DB, lease time.Duration, maxAttempts, historyDays int, window time.Duration) (NotificationDelivery, error) {
	var d NotificationDelivery
	// Expired leases are failed attempts, even if their worker never finished.
	_, err := database.ExecContext(ctx, `WITH stale AS (
 SELECT d.event_seq,d.subscription_id FROM site_event_deliveries d JOIN site_notifications n ON n.id=d.subscription_id
 WHERE d.state='pending' AND d.lease_until<=now() FOR UPDATE OF n,d SKIP LOCKED
 ), expired AS (
 UPDATE site_event_deliveries d SET state=CASE WHEN attempts>=$1 THEN 'failed' ELSE state END,send_started=false,lease_until=NULL,last_category='timeout'
 FROM stale s WHERE d.event_seq=s.event_seq AND d.subscription_id=s.subscription_id RETURNING d.subscription_id,d.last_attempt_at
 ), counts AS (
 SELECT e.subscription_id,min(e.last_attempt_at) AS first_attempt,count(*) FILTER (WHERE e.last_attempt_at>=n.window_start) AS failures
 FROM expired e JOIN site_notifications n ON n.id=e.subscription_id GROUP BY e.subscription_id
 ) UPDATE site_notifications n SET window_failures=window_failures+counts.failures,
 failing_since=COALESCE(failing_since,counts.first_attempt),last_result='timeout' FROM counts WHERE n.id=counts.subscription_id`, maxAttempts)
	if err != nil {
		return d, err
	}
	err = database.QueryRowContext(ctx, `WITH due AS (
 SELECT d.event_seq,d.subscription_id FROM site_event_deliveries d JOIN site_notifications n ON n.id=d.subscription_id JOIN site_events e ON e.seq=d.event_seq
 WHERE d.state='pending' AND d.attempts<$2 AND e.created_at>=now()-$3*interval '1 day' AND d.next_attempt_at<=now() AND (d.lease_until IS NULL OR d.lease_until<=now()) AND n.state='active' AND (n.expires_at IS NULL OR n.expires_at>now())
 ORDER BY d.next_attempt_at,d.event_seq LIMIT 1 FOR UPDATE OF n,d SKIP LOCKED
 ), claimed AS (UPDATE site_event_deliveries d SET lease_until=now()+$1*interval '1 millisecond',attempts=attempts+1,last_attempt_at=now() FROM due WHERE d.event_seq=due.event_seq AND d.subscription_id=due.subscription_id RETURNING d.*),
 accounted AS (UPDATE site_notifications n SET
 window_attempts=CASE WHEN window_start IS NULL OR window_start<=now()-$4*interval '1 millisecond' THEN 1 ELSE window_attempts+1 END,
 window_failures=CASE WHEN window_start IS NULL OR window_start<=now()-$4*interval '1 millisecond' THEN 0 ELSE window_failures END,
 window_start=CASE WHEN window_start IS NULL OR window_start<=now()-$4*interval '1 millisecond' THEN now() ELSE window_start END,
 last_attempt_at=now()
 FROM claimed d WHERE n.id=d.subscription_id RETURNING n.id)
 SELECT d.event_seq,d.subscription_id,d.attempts,d.lease_until,d.last_attempt_at,e.event_id,e.name,e.data,e.created_at FROM claimed d JOIN site_events e ON e.seq=d.event_seq JOIN accounted n ON n.id=d.subscription_id`, lease.Milliseconds(), maxAttempts, historyDays, window.Milliseconds()).Scan(&d.Seq, &d.SubscriptionID, &d.Attempts, &d.LeaseUntil, &d.LastAttemptAt, &d.EventID, &d.Name, &d.Data, &d.CreatedAt)
	return d, err
}

// DeferNotificationDelivery returns a throttled claim without spending an attempt.
func DeferNotificationDelivery(ctx context.Context, database *sql.DB, d NotificationDelivery) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM site_notifications WHERE id=$1 FOR UPDATE`, d.SubscriptionID).Scan(&id); err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `UPDATE site_event_deliveries SET attempts=attempts-1,send_started=false,lease_until=NULL,next_attempt_at=now()+interval '2 seconds'
 WHERE event_seq=$1 AND subscription_id=$2 AND state='pending' AND lease_until=$3 AND lease_until>now()`, d.Seq, d.SubscriptionID, d.LeaseUntil)
	if err != nil {
		return err
	}
	rows, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE site_notifications SET window_attempts=GREATEST(0,window_attempts-1) WHERE id=$1 AND window_start<=$2`, id, d.LastAttemptAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func NotificationLeaseHeld(ctx context.Context, q Querier, d NotificationDelivery) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM site_event_deliveries WHERE event_seq=$1 AND subscription_id=$2 AND state='pending' AND lease_until=$3 AND lease_until>now())`, d.Seq, d.SubscriptionID, d.LeaseUntil).Scan(&held)
	return held, err
}
func NotificationCursor(ctx context.Context, q Querier, id, site string) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(max(seq),0) FROM site_events WHERE site_id=$2 AND seq<COALESCE((SELECT min(event_seq) FROM site_event_deliveries WHERE subscription_id=$1 AND state='pending'),9223372036854775807)`, id, site).Scan(&seq)
	return seq, err
}

// FinishNotificationDelivery checks the lease token so an overdue worker cannot
// overwrite an attempt claimed by another worker.
func FinishNotificationDelivery(ctx context.Context, database *sql.DB, d NotificationDelivery, state, category string, status, duration int, next time.Time, window time.Duration, minAttempts, failPercent int, failFor time.Duration) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM site_notifications WHERE id=$1 FOR UPDATE`, d.SubscriptionID).Scan(&id); err != nil {
		return false, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE site_event_deliveries SET state=$3,next_attempt_at=$4,send_started=false,lease_until=NULL,last_http=$5,last_category=$6,duration_ms=$7 WHERE event_seq=$1 AND subscription_id=$2 AND lease_until=$8`, d.Seq, d.SubscriptionID, state, next, status, category, duration, d.LeaseUntil)
	if err != nil {
		return false, err
	}
	rows, _ := r.RowsAffected()
	if rows == 0 {
		return false, nil
	}
	ignored := state == "skipped" || status == 410 || status == 413
	if ignored {
		_, err = tx.ExecContext(ctx, `UPDATE site_notifications SET last_result=$2 WHERE id=$1`, id, category)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE site_notifications SET
 window_failures=window_failures+CASE WHEN NOT $3 AND $2>=window_start THEN 1 ELSE 0 END,
 failing_since=CASE WHEN $3 THEN NULL ELSE COALESCE(failing_since,$2) END,
 last_success_at=CASE WHEN $3 THEN now() ELSE last_success_at END,last_result=$4 WHERE id=$1`, id, d.LastAttemptAt, state == "delivered", category)
	}
	if err != nil {
		return false, err
	}
	if ignored {
		return false, tx.Commit()
	}
	r, err = tx.ExecContext(ctx, `UPDATE site_notifications SET state='suspended',suspended_at=now(),updated_at=now() WHERE id=$1 AND state='active' AND ((window_start>now()-$5*interval '1 millisecond' AND window_attempts>=$2 AND window_failures::bigint*100>window_attempts::bigint*$3) OR failing_since<=now()-$4*interval '1 millisecond')`, id, minAttempts, failPercent, failFor.Milliseconds(), window.Milliseconds())
	if err != nil {
		return false, err
	}
	rows, _ = r.RowsAffected()
	return rows > 0, tx.Commit()
}
func SweepNotifications(ctx context.Context, q Querier, days int) error {
	_, err := q.ExecContext(ctx, `DELETE FROM site_events WHERE created_at<now()-$1*interval '1 day'`, days)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `DELETE FROM site_notifications WHERE expires_at<now()-$1*interval '1 day'`, days)
	return err
}
func NotificationHistory(ctx context.Context, database *sql.DB, id string, days int) ([]json.RawMessage, error) {
	rows, err := database.QueryContext(ctx, `SELECT jsonb_build_object('event_id',e.event_id,'name',e.name,'created_at',e.created_at,'state',d.state,'attempts',d.attempts,'last_attempt_at',d.last_attempt_at,'last_http',d.last_http,'result',d.last_category,'duration_ms',d.duration_ms) FROM site_event_deliveries d JOIN site_events e ON e.seq=d.event_seq WHERE subscription_id=$1 AND e.created_at>=now()-$2*interval '1 day' ORDER BY e.seq DESC LIMIT 200`, id, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, rows.Err()
}

// NotificationMutation supplies the recorder's transaction completion hooks
// without making the database package depend on the audit package.
type NotificationMutation struct {
	Record   func(*sql.Tx) error
	Commit   func(*sql.Tx) error
	Rollback func(*sql.Tx) error
}

func commitNotificationMutation(tx *sql.Tx, hooks []NotificationMutation) error {
	if len(hooks) > 0 {
		return hooks[0].Commit(tx)
	}
	return tx.Commit()
}
func rollbackNotificationMutation(tx *sql.Tx, hooks []NotificationMutation) {
	if len(hooks) > 0 {
		_ = hooks[0].Rollback(tx)
	} else {
		_ = tx.Rollback()
	}
}

func notificationRecord(tx *sql.Tx, callbacks []NotificationMutation) error {
	for _, f := range callbacks {
		if err := f.Record(tx); err != nil {
			return err
		}
	}
	return nil
}

// NotificationAllowed rechecks membership, site ownership, and the live grant
// immediately before each delivery. A transfer does not carry subscriptions.
func NotificationAllowed(ctx context.Context, q Querier, id string) (bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM site_notifications n JOIN sites s ON s.id=n.site_id
 JOIN users u ON u.id=n.owner_id JOIN users owner ON owner.id=s.user_id
 WHERE n.id=$1 AND u.disabled_at IS NULL AND owner.disabled_at IS NULL
 AND s.deleted_at IS NULL AND s.active_version>0 AND s.access<>'network'
 AND COALESCE(s.access_decision,'')<>'restricted' AND s.user_id=n.site_owner_id
 AND (s.user_id=n.owner_id OR EXISTS(SELECT 1 FROM team_members tm WHERE tm.team_id=s.user_id AND tm.user_id=n.owner_id))
 AND (n.source='owner' OR EXISTS(SELECT 1 FROM oauth_grants g WHERE g.id=n.grant_id AND g.user_id=n.owner_id
 AND EXISTS(SELECT 1 FROM oauth_tokens t WHERE t.grant_id=g.id AND t.expires_at>now())))
 )`, id).Scan(&allowed)
	return allowed, err
}

// NotificationBudget is a shared fixed window. Its caller chooses the
// transaction, so failed visitor saves roll back the event budget as well.
func NotificationBudget(ctx context.Context, q Querier, key string, cap int, window time.Duration) (bool, error) {
	sum := sha256.Sum256([]byte(key))
	key = hex.EncodeToString(sum[:])
	var used int
	err := q.QueryRowContext(ctx, `INSERT INTO notification_budgets(key,used) VALUES($1,1)
 ON CONFLICT(key) DO UPDATE SET
 used=CASE WHEN notification_budgets.window_start<=now()-$3*interval '1 millisecond' THEN 1 ELSE notification_budgets.used+1 END,
 window_start=CASE WHEN notification_budgets.window_start<=now()-$3*interval '1 millisecond' THEN now() ELSE notification_budgets.window_start END
 WHERE notification_budgets.window_start<=now()-$3*interval '1 millisecond' OR notification_budgets.used<$2
 RETURNING used`, key, cap, window.Milliseconds()).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AcquireNotificationSend serializes the short admission check by creator.
// Leased deliveries count across all replicas. Budgets include retry attempts.
func AcquireNotificationSend(ctx context.Context, database *sql.DB, n Notification, d NotificationDelivery, concurrency, minute, day, urlMinute int) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,41))`, n.OwnerID); err != nil {
		return false, err
	}
	var count int
	// Only admitted claims carry send_started. It is cleared with their lease.
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM site_event_deliveries d JOIN site_notifications n ON n.id=d.subscription_id WHERE n.owner_id=$1 AND d.state='pending' AND d.lease_until>now() AND d.send_started`, n.OwnerID).Scan(&count)
	if err != nil || count >= concurrency {
		return false, err
	}
	for _, b := range []struct {
		key    string
		cap    int
		window time.Duration
	}{
		{"minute:" + n.ID, minute, time.Minute}, {"day:" + n.ID, day, 24 * time.Hour}, {"url:" + n.URL, urlMinute, time.Minute},
	} {
		ok, e := NotificationBudget(ctx, tx, b.key, b.cap, b.window)
		if e != nil || !ok {
			return false, e
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE site_event_deliveries SET send_started=true WHERE event_seq=$1 AND subscription_id=$2 AND lease_until=$3 AND lease_until>now() AND NOT send_started`, d.Seq, d.SubscriptionID, d.LeaseUntil)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil || rows != 1 {
		return false, err
	}
	return true, tx.Commit()
}
