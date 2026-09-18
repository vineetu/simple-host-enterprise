package audit

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/lib/pq"
)

// partitionMonthsAhead is how far ahead Prune keeps partitions created,
// matching migration 0027's own bootstrap call
// (SELECT audit_ensure_partitions(2)) so the rolling schedule this
// establishes at deploy time and the one `simple-host prune` maintains
// every month agree.
const partitionMonthsAhead = 2

// monthlyPartitionPattern recognizes a partition audit_ensure_partitions
// created (migration 0027): "<table>_p<YYYY>_<MM>". Anything else under
// audit_events or access_log — today, only the "_default" catch-all — does
// not match and is never a candidate for Prune to touch; the default
// partition is not part of this date-bounded sweep (see migration 0027's
// comment on why it exists).
var monthlyPartitionPattern = regexp.MustCompile(`^(?:audit_events|access_log)_p(\d{4})_(\d{2})$`)

// PrunedPartition is one partition Prune dropped, or would drop under
// -dry-run.
type PrunedPartition struct {
	Table     string
	Partition string
	// MonthEnd is the exclusive upper bound of the partition's month: every
	// row it could hold has `at` < MonthEnd.
	MonthEnd time.Time
}

// PruneOptions configures one Prune run. AuditRetentionDays and
// AccessRetentionDays are design 8.2's AUDIT_RETENTION_DAYS (400) and
// ACCESS_LOG_RETENTION_DAYS (90) defaults, read from the environment by
// cmd/server/subcommands.go's prune subcommand — this package takes them as
// plain ints so it does not need to know how they were sourced.
type PruneOptions struct {
	AuditRetentionDays  int
	AccessRetentionDays int
	// DryRun lists qualifying partitions instead of dropping them.
	DryRun bool
	// Now overrides the current time for tests; zero means time.Now().
	Now time.Time
}

// PruneResult reports what Prune did (DryRun false) or would do (DryRun
// true) — never both; Dropped and WouldDrop are the same list under the
// two modes, named differently so a caller cannot mistake one for the
// other.
type PruneResult struct {
	Dropped   []PrunedPartition
	WouldDrop []PrunedPartition
}

// Prune drops (or, under DryRun, lists) partitions of audit_events and
// access_log wholly older than their configured retention (design 8.1,
// 8.2: "retention is a partition drop," design 9.3: "retention... [is] the
// owner role's work"). db must connect as the owning role — the
// application role has no DROP privilege on these tables at all (design
// 9.3), so this is never called with the server's own connection pool.
//
// Each call also runs audit_ensure_partitions first, so a monthly prune
// job is also what keeps the calendar ahead of the current month; a
// deployment that only ever runs `simple-host prune` (never a second,
// separate partition-creation job) still never runs out of partitions.
func Prune(ctx context.Context, db *sql.DB, opts PruneOptions) (PruneResult, error) {
	if db == nil {
		return PruneResult{}, fmt.Errorf("audit: Prune requires a non-nil db")
	}
	if opts.AuditRetentionDays <= 0 || opts.AccessRetentionDays <= 0 {
		return PruneResult{}, fmt.Errorf("audit: Prune requires positive retention days (audit=%d, access=%d)", opts.AuditRetentionDays, opts.AccessRetentionDays)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	if _, err := db.ExecContext(ctx, `SELECT audit_ensure_partitions($1)`, partitionMonthsAhead); err != nil {
		return PruneResult{}, fmt.Errorf("ensure partitions: %w", err)
	}

	var result PruneResult
	specs := []struct {
		table         string
		retentionDays int
	}{
		{"audit_events", opts.AuditRetentionDays},
		{"access_log", opts.AccessRetentionDays},
	}
	for _, spec := range specs {
		cutoff := now.AddDate(0, 0, -spec.retentionDays)
		partitions, err := listMonthlyPartitions(ctx, db, spec.table)
		if err != nil {
			return result, fmt.Errorf("list %s partitions: %w", spec.table, err)
		}
		for _, p := range partitions {
			// A partition is safe to drop only once every row it could
			// hold is already outside the retention window: its whole
			// range, up to MonthEnd, must be at or before cutoff.
			if p.monthEnd.After(cutoff) {
				continue
			}
			entry := PrunedPartition{Table: spec.table, Partition: p.name, MonthEnd: p.monthEnd}
			if opts.DryRun {
				result.WouldDrop = append(result.WouldDrop, entry)
				continue
			}
			if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+pq.QuoteIdentifier(p.name)); err != nil {
				return result, fmt.Errorf("drop partition %s: %w", p.name, err)
			}
			result.Dropped = append(result.Dropped, entry)
		}
	}
	return result, nil
}

type monthlyPartition struct {
	name     string
	monthEnd time.Time
}

// listMonthlyPartitions reads parent's current partitions from the catalog
// (pg_inherits, not information_schema: partition membership is exposed
// there, not in any information_schema view) and keeps only the ones whose
// name matches monthlyPartitionPattern.
func listMonthlyPartitions(ctx context.Context, db *sql.DB, parent string) ([]monthlyPartition, error) {
	const query = `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = $1::regclass
		ORDER BY c.relname
	`
	rows, err := db.QueryContext(ctx, query, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []monthlyPartition
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		m := monthlyPartitionPattern.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		year, err1 := strconv.Atoi(m[1])
		month, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil || month < 1 || month > 12 {
			continue
		}
		monthStart := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		out = append(out, monthlyPartition{name: name, monthEnd: monthStart.AddDate(0, 1, 0)})
	}
	return out, rows.Err()
}
