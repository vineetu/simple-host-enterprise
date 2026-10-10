package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/vsriram/simple-host/internal/storagepolicy"
)

// Site storage (migration 0060, docs/designs/storage-presets.md): key-value
// namespaces, databases of tables and file buckets, each with an access
// matrix. Every statement here is fixed text; table and column names a page
// or owner chooses only ever travel as bound parameters (a table_name value,
// a jsonb key), never as SQL identifiers.

// ErrStorageNotFound is a missing resource, key, row or file.
var ErrStorageNotFound = errors.New("not found")

// StorageTable is one table of a database: its matrix and its columns.
type StorageTable struct {
	Preset  string                          `json:"preset"`
	Base    string                          `json:"base,omitempty"`
	Access  storagepolicy.Matrix            `json:"access"`
	Columns map[string]storagepolicy.Column `json:"columns"`
}

// StorageSettings is site_storage_resources.settings.
type StorageSettings struct {
	// Base is the preset a custom matrix started from.
	Base   string                  `json:"base,omitempty"`
	Tables map[string]StorageTable `json:"tables,omitempty"`
}

// StorageResource is one row of site_storage_resources.
type StorageResource struct {
	SiteID    string
	Name      string
	Kind      string
	Preset    string
	Access    storagepolicy.Matrix
	Settings  StorageSettings
	Bytes     int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const storageResourceColumns = `site_id::text, name, kind, preset, read_who, add_who, edit_who, delete_who, settings, bytes, created_at, updated_at`

func scanStorageResource(row interface{ Scan(...any) error }) (StorageResource, error) {
	var r StorageResource
	var settings []byte
	var read, add, edit, del string
	if err := row.Scan(&r.SiteID, &r.Name, &r.Kind, &r.Preset, &read, &add, &edit, &del, &settings, &r.Bytes, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return StorageResource{}, err
	}
	r.Access = storagepolicy.Matrix{Read: storagepolicy.Who(read), Add: storagepolicy.Who(add), Edit: storagepolicy.Who(edit), Delete: storagepolicy.Who(del)}
	if err := json.Unmarshal(settings, &r.Settings); err != nil {
		return StorageResource{}, err
	}
	return r, nil
}

// ListStorageResources returns a site's resources by name.
func ListStorageResources(ctx context.Context, q Querier, siteID string) ([]StorageResource, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+storageResourceColumns+` FROM site_storage_resources WHERE site_id = $1::uuid ORDER BY name`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageResource
	for rows.Next() {
		r, err := scanStorageResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetStorageResource returns one resource, or ErrStorageNotFound.
func GetStorageResource(ctx context.Context, q Querier, siteID, name string) (StorageResource, error) {
	r, err := scanStorageResource(q.QueryRowContext(ctx, `SELECT `+storageResourceColumns+` FROM site_storage_resources WHERE site_id = $1::uuid AND name = $2`, siteID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return StorageResource{}, ErrStorageNotFound
	}
	return r, err
}

// LockSiteStorage serialises every storage write of one site, across
// replicas, for the rest of tx: the add-or-edit decision, the byte limits
// and reference checks all read what the write then changes.
func LockSiteStorage(ctx context.Context, tx *sql.Tx, siteID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('site-storage' || chr(31) || $1::uuid::text, 0))`, siteID)
	return err
}

// PutStorageResource creates or replaces a resource's settings, keeping its
// data and byte count.
func PutStorageResource(ctx context.Context, tx *sql.Tx, r StorageResource) (created bool, err error) {
	settings, err := json.Marshal(r.Settings)
	if err != nil {
		return false, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO site_storage_resources (site_id, name, kind, preset, read_who, add_who, edit_who, delete_who, settings)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		ON CONFLICT (site_id, name) DO UPDATE SET
			preset = EXCLUDED.preset, read_who = EXCLUDED.read_who, add_who = EXCLUDED.add_who,
			edit_who = EXCLUDED.edit_who, delete_who = EXCLUDED.delete_who,
			settings = EXCLUDED.settings, updated_at = now()
		RETURNING xmax = 0`,
		r.SiteID, r.Name, r.Kind, r.Preset, string(r.Access.Read), string(r.Access.Add), string(r.Access.Edit), string(r.Access.Delete), string(settings)).Scan(&created)
	return created, err
}

// DeleteStorageResource deletes a resource and all its data, and returns the
// ids of its files' objects for the caller to retire in the same tx.
func DeleteStorageResource(ctx context.Context, tx *sql.Tx, siteID, name string) ([]string, error) {
	objects, err := storageFileObjects(ctx, tx, `SELECT object_id::text FROM site_storage_files WHERE site_id = $1::uuid AND resource = $2`, siteID, name)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM site_storage_resources WHERE site_id = $1::uuid AND name = $2`, siteID, name)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrStorageNotFound
	}
	return objects, nil
}

// DeleteStorageTableRows deletes every row of one table (a table removed
// from a database's settings) and returns how many bytes they held.
func DeleteStorageTableRows(ctx context.Context, tx *sql.Tx, siteID, resource, table string) (int64, error) {
	var freed int64
	err := tx.QueryRowContext(ctx, `
		WITH gone AS (
			DELETE FROM site_storage_rows WHERE site_id = $1::uuid AND resource = $2 AND table_name = $3
			RETURNING octet_length(data::text) AS n)
		SELECT COALESCE(sum(n), 0) FROM gone`, siteID, resource, table).Scan(&freed)
	return freed, err
}

func storageFileObjects(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AddStorageBytes adjusts a resource's byte count by delta.
func AddStorageBytes(ctx context.Context, tx *sql.Tx, siteID, name string, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE site_storage_resources SET bytes = GREATEST(bytes + $3, 0) WHERE site_id = $1::uuid AND name = $2`, siteID, name, delta)
	return err
}

// SiteStorageBytes returns the bytes a site's data resources (KV and
// databases) and its file buckets hold.
func SiteStorageBytes(ctx context.Context, q Querier, siteID string) (data, files int64, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT COALESCE(sum(bytes) FILTER (WHERE kind <> 'files'), 0), COALESCE(sum(bytes) FILTER (WHERE kind = 'files'), 0)
		FROM site_storage_resources WHERE site_id = $1::uuid`, siteID).Scan(&data, &files)
	return data, files, err
}

// CountStorageResources returns how many resources a site has.
func CountStorageResources(ctx context.Context, q Querier, siteID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM site_storage_resources WHERE site_id = $1::uuid`, siteID).Scan(&n)
	return n, err
}

// StorageEntryMeta is what every entry carries besides its value.
type StorageEntryMeta struct {
	AddedBy         string // users.id, "" when unknown (nobody signed in, or erased)
	AddedByUsername string
	CreatedAt       time.Time
	UpdatedAt       *time.Time
}

// StorageKV is one key. Stored is the bytes it counts toward the site's
// limit: the key plus the value as Postgres stores it (octet_length of the
// jsonb text), the one measure every add, edit and delete uses, so the count
// cannot drift (a value's text can grow when stored: 1e9 becomes 1000000000).
type StorageKV struct {
	Key    string
	Value  json.RawMessage
	Stored int64
	StorageEntryMeta
}

// ListPage bounds a listing. OnlyAddedBy, when set, keeps only entries that
// person added (an own read).
type ListPage struct {
	Prefix      string
	After       string
	Limit       int
	OnlyAddedBy string
}

func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListStorageKV lists keys in order, after page.After.
func ListStorageKV(ctx context.Context, q Querier, siteID, resource string, page ListPage) ([]StorageKV, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT k.key, k.value, octet_length(k.key) + octet_length(k.value::text), COALESCE(k.added_by::text, ''), COALESCE(u.username, ''), k.created_at, k.updated_at
		FROM site_storage_kv k LEFT JOIN users u ON u.id = k.added_by
		WHERE k.site_id = $1::uuid AND k.resource = $2
		  AND left(k.key, length($3)) = $3 AND k.key COLLATE "C" > $4
		  AND ($5::uuid IS NULL OR k.added_by = $5::uuid)
		ORDER BY k.key COLLATE "C" LIMIT $6`,
		siteID, resource, page.Prefix, page.After, nullableUUID(page.OnlyAddedBy), page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageKV
	for rows.Next() {
		var e StorageKV
		var updated time.Time
		if err := rows.Scan(&e.Key, &e.Value, &e.Stored, &e.AddedBy, &e.AddedByUsername, &e.CreatedAt, &updated); err != nil {
			return nil, err
		}
		e.UpdatedAt = &updated
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetStorageKV returns one key, or ErrStorageNotFound.
func GetStorageKV(ctx context.Context, q Querier, siteID, resource, key string) (StorageKV, error) {
	var e StorageKV
	var updated time.Time
	err := q.QueryRowContext(ctx, `
		SELECT k.key, k.value, octet_length(k.key) + octet_length(k.value::text), COALESCE(k.added_by::text, ''), COALESCE(u.username, ''), k.created_at, k.updated_at
		FROM site_storage_kv k LEFT JOIN users u ON u.id = k.added_by
		WHERE k.site_id = $1::uuid AND k.resource = $2 AND k.key = $3`, siteID, resource, key).
		Scan(&e.Key, &e.Value, &e.Stored, &e.AddedBy, &e.AddedByUsername, &e.CreatedAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return StorageKV{}, ErrStorageNotFound
	}
	e.UpdatedAt = &updated
	return e, err
}

// InsertStorageKV adds a key and returns its stored size.
func InsertStorageKV(ctx context.Context, tx *sql.Tx, siteID, resource, key string, value json.RawMessage, addedBy string) (int64, error) {
	var stored int64
	err := tx.QueryRowContext(ctx, `INSERT INTO site_storage_kv (site_id, resource, key, value, added_by) VALUES ($1::uuid, $2, $3, $4::jsonb, $5::uuid)
		RETURNING octet_length(key) + octet_length(value::text)`,
		siteID, resource, key, string(value), nullableUUID(addedBy)).Scan(&stored)
	return stored, err
}

// UpdateStorageKV changes a key's value, keeping who added it. With
// onlyAddedBy set the key must have been added by that person; zero rows is
// ErrStorageNotFound either way.
func UpdateStorageKV(ctx context.Context, tx *sql.Tx, siteID, resource, key string, value json.RawMessage, onlyAddedBy string) (int64, error) {
	var stored int64
	err := tx.QueryRowContext(ctx, `
		UPDATE site_storage_kv SET value = $4::jsonb, updated_at = now()
		WHERE site_id = $1::uuid AND resource = $2 AND key = $3 AND ($5::uuid IS NULL OR added_by = $5::uuid)
		RETURNING octet_length(key) + octet_length(value::text)`,
		siteID, resource, key, string(value), nullableUUID(onlyAddedBy)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrStorageNotFound
	}
	return stored, err
}

// DeleteStorageKV deletes a key, under the same onlyAddedBy rule.
func DeleteStorageKV(ctx context.Context, tx *sql.Tx, siteID, resource, key, onlyAddedBy string) error {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM site_storage_kv
		WHERE site_id = $1::uuid AND resource = $2 AND key = $3 AND ($4::uuid IS NULL OR added_by = $4::uuid)`,
		siteID, resource, key, nullableUUID(onlyAddedBy))
	return oneRow(res, err)
}

func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStorageNotFound
	}
	return nil
}

// StorageRow is one row of a table. Stored is octet_length(data::text), the
// row's count toward the site's limit (see StorageKV).
type StorageRow struct {
	ID     int64
	Data   json.RawMessage
	Stored int64
	StorageEntryMeta
}

// RowFilter is one equality filter: the column's value equals Value.
type RowFilter struct {
	Column string
	Value  json.RawMessage
}

// RowQuery bounds a row listing. Order is "id", "created_at", "updated_at"
// or a declared column.
type RowQuery struct {
	Filters     []RowFilter
	OnlyAddedBy string
	Order       string
	Desc        bool
	Limit       int
	Offset      int
}

const storageRowColumns = `r.id, r.data, octet_length(r.data::text), COALESCE(r.added_by::text, ''), COALESCE(u.username, ''), r.created_at, r.updated_at`

func scanStorageRow(row interface{ Scan(...any) error }) (StorageRow, error) {
	var r StorageRow
	err := row.Scan(&r.ID, &r.Data, &r.Stored, &r.AddedBy, &r.AddedByUsername, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListStorageRows lists one table's rows.
func ListStorageRows(ctx context.Context, q Querier, siteID, resource, table string, rq RowQuery) ([]StorageRow, error) {
	args := []any{siteID, resource, table, nullableUUID(rq.OnlyAddedBy)}
	var where strings.Builder
	for _, f := range rq.Filters {
		args = append(args, f.Column, string(f.Value))
		// The key and the value are both parameters: data -> $k = $v::jsonb.
		where.WriteString(" AND r.data -> $" + strconv.Itoa(len(args)-1) + " = $" + strconv.Itoa(len(args)) + "::jsonb")
	}
	dir := " ASC"
	if rq.Desc {
		dir = " DESC"
	}
	var order string
	switch rq.Order {
	case "", "id":
		order = "r.id" + dir
	case "created_at":
		order = "r.created_at" + dir + ", r.id" + dir
	case "updated_at":
		order = "r.updated_at" + dir + " NULLS LAST, r.id" + dir
	default:
		args = append(args, rq.Order)
		order = "r.data -> $" + strconv.Itoa(len(args)) + dir + " NULLS LAST, r.id" + dir
	}
	args = append(args, rq.Limit, rq.Offset)
	query := `SELECT ` + storageRowColumns + `
		FROM site_storage_rows r LEFT JOIN users u ON u.id = r.added_by
		WHERE r.site_id = $1::uuid AND r.resource = $2 AND r.table_name = $3
		  AND ($4::uuid IS NULL OR r.added_by = $4::uuid)` + where.String() + `
		ORDER BY ` + order + ` LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageRow
	for rows.Next() {
		r, err := scanStorageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetStorageRow returns one row, or ErrStorageNotFound.
func GetStorageRow(ctx context.Context, q Querier, siteID, resource, table string, id int64) (StorageRow, error) {
	r, err := scanStorageRow(q.QueryRowContext(ctx, `SELECT `+storageRowColumns+`
		FROM site_storage_rows r LEFT JOIN users u ON u.id = r.added_by
		WHERE r.site_id = $1::uuid AND r.resource = $2 AND r.table_name = $3 AND r.id = $4`, siteID, resource, table, id))
	if errors.Is(err, sql.ErrNoRows) {
		return StorageRow{}, ErrStorageNotFound
	}
	return r, err
}

// InsertStorageRow adds a row and returns it.
func InsertStorageRow(ctx context.Context, tx *sql.Tx, siteID, resource, table string, data json.RawMessage, addedBy string) (StorageRow, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO site_storage_rows (site_id, resource, table_name, data, added_by)
		VALUES ($1::uuid, $2, $3, $4::jsonb, $5::uuid) RETURNING id`,
		siteID, resource, table, string(data), nullableUUID(addedBy)).Scan(&id); err != nil {
		return StorageRow{}, err
	}
	return GetStorageRow(ctx, tx, siteID, resource, table, id)
}

// UpdateStorageRow replaces a row's data, under the onlyAddedBy rule, and
// returns its new stored size.
func UpdateStorageRow(ctx context.Context, tx *sql.Tx, siteID, resource, table string, id int64, data json.RawMessage, onlyAddedBy string) (int64, error) {
	var stored int64
	err := tx.QueryRowContext(ctx, `
		UPDATE site_storage_rows SET data = $5::jsonb, updated_at = now()
		WHERE site_id = $1::uuid AND resource = $2 AND table_name = $3 AND id = $4 AND ($6::uuid IS NULL OR added_by = $6::uuid)
		RETURNING octet_length(data::text)`,
		siteID, resource, table, id, string(data), nullableUUID(onlyAddedBy)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrStorageNotFound
	}
	return stored, err
}

// StripStorageColumns removes columns' values from every row of a table (a
// column the owner removed or retyped) and returns the bytes freed, so a
// column added back later starts empty.
func StripStorageColumns(ctx context.Context, tx *sql.Tx, siteID, resource, table string, columns []string) (int64, error) {
	if len(columns) == 0 {
		return 0, nil
	}
	var freed int64
	err := tx.QueryRowContext(ctx, `
		WITH before AS (
			SELECT id, octet_length(data::text) AS n FROM site_storage_rows
			WHERE site_id = $1::uuid AND resource = $2 AND table_name = $3 AND data ?| $4::text[]),
		after AS (
			UPDATE site_storage_rows r SET data = r.data - $4::text[]
			FROM before b WHERE r.id = b.id
			RETURNING r.id, octet_length(r.data::text) AS n)
		SELECT COALESCE(sum(b.n - a.n), 0) FROM before b JOIN after a ON a.id = b.id`,
		siteID, resource, table, pq.Array(columns)).Scan(&freed)
	return freed, err
}

// DeleteStorageRow deletes a row, under the onlyAddedBy rule.
func DeleteStorageRow(ctx context.Context, tx *sql.Tx, siteID, resource, table string, id int64, onlyAddedBy string) error {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM site_storage_rows
		WHERE site_id = $1::uuid AND resource = $2 AND table_name = $3 AND id = $4 AND ($5::uuid IS NULL OR added_by = $5::uuid)`,
		siteID, resource, table, id, nullableUUID(onlyAddedBy))
	return oneRow(res, err)
}

// StorageRowReferenced reports whether any row of table has column equal to
// id: whether deleting row id of the table column references would leave a
// dangling reference.
func StorageRowReferenced(ctx context.Context, q Querier, siteID, resource, table, column string, id int64) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM site_storage_rows
			WHERE site_id = $1::uuid AND resource = $2 AND table_name = $3 AND data -> $4 = to_jsonb($5::bigint))`,
		siteID, resource, table, column, id).Scan(&found)
	return found, err
}

// StorageFile is one file's row; the bytes are the bucket object ObjectID.
type StorageFile struct {
	Path        string
	ObjectID    string
	ContentType string
	Size        int64
	SHA256      []byte
	StorageEntryMeta
}

const storageFileColumns = `f.path, f.object_id::text, f.content_type, f.size, f.sha256, COALESCE(f.added_by::text, ''), COALESCE(u.username, ''), f.created_at, f.updated_at`

func scanStorageFile(row interface{ Scan(...any) error }) (StorageFile, error) {
	var f StorageFile
	var updated time.Time
	err := row.Scan(&f.Path, &f.ObjectID, &f.ContentType, &f.Size, &f.SHA256, &f.AddedBy, &f.AddedByUsername, &f.CreatedAt, &updated)
	f.UpdatedAt = &updated
	return f, err
}

// ListStorageFiles lists files in path order, after page.After.
func ListStorageFiles(ctx context.Context, q Querier, siteID, resource string, page ListPage) ([]StorageFile, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+storageFileColumns+`
		FROM site_storage_files f LEFT JOIN users u ON u.id = f.added_by
		WHERE f.site_id = $1::uuid AND f.resource = $2
		  AND left(f.path, length($3)) = $3 AND f.path COLLATE "C" > $4
		  AND ($5::uuid IS NULL OR f.added_by = $5::uuid)
		ORDER BY f.path COLLATE "C" LIMIT $6`,
		siteID, resource, page.Prefix, page.After, nullableUUID(page.OnlyAddedBy), page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageFile
	for rows.Next() {
		f, err := scanStorageFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetStorageFile returns one file, or ErrStorageNotFound.
func GetStorageFile(ctx context.Context, q Querier, siteID, resource, path string) (StorageFile, error) {
	f, err := scanStorageFile(q.QueryRowContext(ctx, `SELECT `+storageFileColumns+`
		FROM site_storage_files f LEFT JOIN users u ON u.id = f.added_by
		WHERE f.site_id = $1::uuid AND f.resource = $2 AND f.path = $3`, siteID, resource, path))
	if errors.Is(err, sql.ErrNoRows) {
		return StorageFile{}, ErrStorageNotFound
	}
	return f, err
}

// InsertStorageFile adds a file.
func InsertStorageFile(ctx context.Context, tx *sql.Tx, siteID, resource string, f StorageFile, addedBy string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO site_storage_files (site_id, resource, path, object_id, content_type, size, sha256, added_by)
		VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8::uuid)`,
		siteID, resource, f.Path, f.ObjectID, f.ContentType, f.Size, f.SHA256, nullableUUID(addedBy))
	return err
}

// UpdateStorageFile points a path at new bytes, keeping who added it, under
// the onlyAddedBy rule.
func UpdateStorageFile(ctx context.Context, tx *sql.Tx, siteID, resource string, f StorageFile, onlyAddedBy string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE site_storage_files SET object_id = $4::uuid, content_type = $5, size = $6, sha256 = $7, updated_at = now()
		WHERE site_id = $1::uuid AND resource = $2 AND path = $3 AND ($8::uuid IS NULL OR added_by = $8::uuid)`,
		siteID, resource, f.Path, f.ObjectID, f.ContentType, f.Size, f.SHA256, nullableUUID(onlyAddedBy))
	return oneRow(res, err)
}

// DeleteStorageFile deletes a file's row, under the onlyAddedBy rule.
func DeleteStorageFile(ctx context.Context, tx *sql.Tx, siteID, resource, path, onlyAddedBy string) error {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM site_storage_files
		WHERE site_id = $1::uuid AND resource = $2 AND path = $3 AND ($4::uuid IS NULL OR added_by = $4::uuid)`,
		siteID, resource, path, nullableUUID(onlyAddedBy))
	return oneRow(res, err)
}
