package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestExpiredAPIKeyIsRefused(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	user, err := CreateOIDCUser(ctx, database, "ci", "sub-ci", "ci@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	live := HashAPIKey("shk_live")
	if _, err := CreateAPIKey(ctx, database, user.ID, "live", live, KeyPrefix(live), time.Now().Add(time.Hour), APIKeyScopeFull); err != nil {
		t.Fatal(err)
	}
	expired := HashAPIKey("shk_expired")
	if _, err := CreateAPIKey(ctx, database, user.ID, "expired", expired, KeyPrefix(expired), time.Now().Add(-time.Second), APIKeyScopeFull); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := GetUserByAPIKeyHash(ctx, database, live); err != nil {
		t.Fatalf("live key: %v", err)
	}
	if _, _, _, err := GetUserByAPIKeyHash(ctx, database, expired); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired key: err = %v, want sql.ErrNoRows", err)
	}
}
