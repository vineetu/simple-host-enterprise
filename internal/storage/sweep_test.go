package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lib/pq"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/migrate"
)

// sweepTestDB applies every migration to a fresh throwaway database on the
// server MIGRATE_TEST_DSN names (the internal/db assetsTestDB pattern), or
// skips when it is unset.
func sweepTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse MIGRATE_TEST_DSN: %v", err)
	}
	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	rand.Read(suffix)
	name := "storage_sweep_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN.String())
		if err != nil {
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	testDSN := *parsed
	testDSN.Path = "/" + name
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := migrate.Apply(context.Background(), database, 0, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

func queuedKeys(t *testing.T, database *sql.DB) map[string]int {
	t.Helper()
	rows, err := database.Query(`SELECT object_key FROM storage_retired`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		out[key]++
	}
	return out
}

func TestSweepDeletesDueObjects(t *testing.T) {
	database := sweepTestDB(t)
	ctx := context.Background()
	memory := NewMemoryObjects()
	recording := newRecordingObjects(memory)
	store, _ := newTestStore(t, recording, nil, 1<<30)

	put := func(key string) {
		t.Helper()
		if err := memory.Put(ctx, key, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	retire := func(key string, grace time.Duration) {
		t.Helper()
		if err := db.RetireObjects(ctx, database, key, grace); err != nil {
			t.Fatalf("RetireObjects(%q): %v", key, err)
		}
	}
	has := func(key string) bool {
		_, err := memory.Get(ctx, key, 1<<20)
		return err == nil
	}

	dueKey := "sites/" + testSiteA + "/v1.tar.gz"
	keptSibling := "sites/" + testSiteA + "/v2.tar.gz"
	prefix := "sites/" + testSiteB + "/"
	prefixed := []string{prefix + "v1.tar.gz", prefix + "v2.tar.gz", prefix + "assets/" + testSiteC}
	notDue := "sites/" + testSiteC + "/v1.tar.gz"
	outOfScope := "other/x"
	dotted := "sites/../other/y"
	failing := "sites/" + testSiteC + "/v2.tar.gz"
	for _, key := range append([]string{dueKey, keptSibling, notDue, outOfScope, dotted, failing}, prefixed...) {
		put(key)
	}
	// More due entries than one sweep batch, to cover the batch loop.
	var bulk []string
	for version := 10; version < 10+sweepBatch+7; version++ {
		key := "sites/" + testSiteA + "/v" + strconv.Itoa(version) + ".tar.gz"
		bulk = append(bulk, key)
		put(key)
		retire(key, 0)
	}
	retire(dueKey, 0)
	retire(prefix, 0)
	retire(notDue, time.Hour)
	retire(outOfScope, 0)
	retire(dotted, 0)
	retire(failing, 0)

	// A bucket that is down deletes nothing and loses nothing; only the two
	// out-of-scope entries, which never touch the bucket, are dropped.
	down := errors.New("bucket down")
	memory.Fail = down
	if n, err := store.Sweep(ctx, database); err != nil || n != 2 {
		t.Fatalf("Sweep with bucket down = %d, %v; want 2, nil", n, err)
	}
	memory.Fail = nil
	if got := len(queuedKeys(t, database)); got != len(bulk)+4 {
		t.Fatalf("queue holds %d entries after a failed sweep, want %d", got, len(bulk)+4)
	}
	// Failed entries were pushed back rather than retried at once; make them
	// due again for the rest of the test.
	makeDue := func(except string) {
		t.Helper()
		if _, err := database.ExecContext(ctx, `UPDATE storage_retired SET retire_after = now() - interval '1 second' WHERE object_key <> $1`, except); err != nil {
			t.Fatal(err)
		}
	}
	makeDue(notDue)

	recording.failDeletes[failing] = true
	n, err := store.Sweep(ctx, database)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// Every due entry but the failing one completes.
	if want := len(bulk) + 2; n != want {
		t.Fatalf("Sweep completed %d entries, want %d", n, want)
	}
	for _, key := range append(append([]string{dueKey}, prefixed...), bulk...) {
		if has(key) {
			t.Errorf("due object %s survived the sweep", key)
		}
	}
	for _, key := range []string{keptSibling, notDue, outOfScope, dotted, failing} {
		if !has(key) {
			t.Errorf("object %s was deleted but should not have been", key)
		}
	}
	queued := queuedKeys(t, database)
	if len(queued) != 2 || queued[notDue] != 1 || queued[failing] != 1 {
		t.Fatalf("queue after sweep = %v, want only %s and %s", queued, notDue, failing)
	}

	// The failing entry was pushed to the back of the queue, not left at the
	// front where it would block every sweep.
	var deferred bool
	if err := database.QueryRowContext(ctx, `SELECT retire_after > now() + interval '10 minutes' FROM storage_retired WHERE object_key = $1`, failing).Scan(&deferred); err != nil || !deferred {
		t.Fatalf("failing entry deferred = %t, %v; want true", deferred, err)
	}

	// Once the delete succeeds the entry completes.
	delete(recording.failDeletes, failing)
	makeDue(notDue)
	if n, err := store.Sweep(ctx, database); err != nil || n != 1 {
		t.Fatalf("retry sweep = %d, %v; want 1", n, err)
	}
	if has(failing) {
		t.Fatal("retried object survived")
	}
	if queued := queuedKeys(t, database); len(queued) != 1 || queued[notDue] != 1 {
		t.Fatalf("queue after retry = %v", queued)
	}
}
