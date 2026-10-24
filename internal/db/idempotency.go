package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
)

var ErrIdempotencyReused = errors.New("idempotency key reused with a different body")
var ErrIdempotencyFull = errors.New("idempotency capacity reached")

type CreateReplay struct {
	Status int
	Header http.Header
	Body   []byte
}

// ClaimCreate reserves a key before a mutation. A committed pending row remains
// pending after an uncertain failure, so another pod cannot repeat the mutation.
// The short transaction serializes admission to enforce both caps across pods.
func ClaimCreate(ctx context.Context, database *sql.DB, account, key string, hash []byte, perAccount, total int) (*CreateReplay, bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE create_idempotency IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM create_idempotency WHERE expires_at <= now()`); err != nil {
		return nil, false, err
	}
	var oldHash, header []byte
	var replay CreateReplay
	err = tx.QueryRowContext(ctx, `SELECT body_hash, status, headers, body FROM create_idempotency WHERE account_id=$1 AND request_key=$2`, account, key).Scan(&oldHash, &replay.Status, &header, &replay.Body)
	if err == nil {
		if !bytes.Equal(oldHash, hash) {
			return nil, false, ErrIdempotencyReused
		}
		if err = json.Unmarshal(header, &replay.Header); err != nil {
			return nil, false, err
		}
		return &replay, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var ownCount, totalCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE account_id=$1), count(*) FROM create_idempotency`, account).Scan(&ownCount, &totalCount); err != nil {
		return nil, false, err
	}
	if ownCount >= perAccount || totalCount >= total {
		return nil, false, ErrIdempotencyFull
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO create_idempotency(account_id,request_key,body_hash) VALUES($1,$2,$3)`, account, key, hash); err != nil {
		return nil, false, err
	}
	return nil, true, tx.Commit()
}

func CompleteCreate(ctx context.Context, database *sql.DB, account, key string, replay CreateReplay) error {
	if replay.Body == nil {
		replay.Body = []byte{}
	}
	header, err := json.Marshal(replay.Header)
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `UPDATE create_idempotency SET status=$3, headers=$4, body=$5, expires_at=now()+interval '24 hours' WHERE account_id=$1 AND request_key=$2 AND status=0`, account, key, replay.Status, string(header), replay.Body)
	return err
}
