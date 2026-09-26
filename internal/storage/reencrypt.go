package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ErrNoEnvelopeKey is returned by Reencrypt when BACKUP_ENVELOPE_KEY is unset.
var ErrNoEnvelopeKey = errors.New("BACKUP_ENVELOPE_KEY is not set: there is no envelope key to re-encrypt under, so there is nothing to do")

// ReencryptOptions configures Reencrypt.
type ReencryptOptions struct {
	// DryRun reads and decrypts every object that would be rewritten but
	// writes nothing.
	DryRun bool
	// Concurrency is how many objects are worked at once; below 1 means 1.
	Concurrency int
	// VersionCommitted reports whether a committed versions row names
	// siteID's version. Required: see Reencrypt for why.
	VersionCommitted func(ctx context.Context, siteID string, version int) (bool, error)
	// Logf receives one line per skipped or failed object (and, under
	// DryRun, per object that would be rewritten), and the periodic
	// progress line.
	Logf func(format string, args ...any)
	// ProgressEvery is how many scanned objects pass between progress lines;
	// 0 means 100.
	ProgressEvery int
}

// ReencryptStats counts what Reencrypt did. Rewritten counts objects that
// would be rewritten under DryRun.
type ReencryptStats struct {
	Scanned, Rewritten, Current, Skipped, Failed int
}

func (s ReencryptStats) String() string {
	return fmt.Sprintf("%d scanned, %d rewritten, %d already current, %d skipped, %d failed", s.Scanned, s.Rewritten, s.Current, s.Skipped, s.Failed)
}

// Reencrypt rewrites every object the store keeps (everything under sites/:
// version archives sites/<id>/v<N>.tar.gz and assets sites/<id>/assets/<id>,
// see keys.go; the store writes nothing else) so it is wrapped under the
// first configured envelope key in the key-bound form (format 2). Afterwards
// no object needs any other key, so retired keys can be removed. Objects
// written with no envelope (BACKUP_ENVELOPE_PLAINTEXT_ALLOWED) are encrypted
// the same way.
//
// It is idempotent and resumable: an object already in format 2 under the
// first key is recognised from its metadata alone (a HEAD, no body download)
// and left alone, so a second run rewrites nothing and a run cut short is
// simply run again. A rewrite goes through Put, the path every new write
// takes, so it is bound to the same object key. An object that does not
// decrypt is never overwritten: it is reported and counted as failed. A
// rewrite counts only once it has been read back, decrypts to the same
// SHA-256 and carries the first key in format 2.
//
// Running next to live servers is safe:
//
//   - Assets are keyed by a fresh random id per upload, so an asset key only
//     ever holds one plaintext; rewriting it writes the same bytes back. An
//     asset deleted between our read and our write is resurrected as an
//     unreferenced object: serving looks the (soft-deleted) row up first, so
//     it is never served, only stored.
//   - Version keys are NOT immutable until committed: a deploy uploads its
//     archive before its transaction commits, and a failed deploy's number is
//     allocated again to the next deploy, which overwrites the object. So a
//     version object is rewritten only when a committed versions row names
//     it, checked before it is read: from then on the key's content is final
//     (the number is never allocated again while the row exists). An
//     unreferenced version object (an in-flight deploy, a failed deploy's
//     leftover, or one queued for retirement after a prune or site delete)
//     is skipped with a reason; the server never reads it, and the retire
//     sweep deletes queued ones without needing to decrypt them.
//   - The pod-local cache holds plaintext (unpacked version files, asset
//     bytes checked against their recorded SHA-256), filled from a
//     decrypted Get, so a rewritten object changes nothing a replica serves.
func (o *S3Objects) Reencrypt(ctx context.Context, opts ReencryptOptions) (ReencryptStats, error) {
	if len(o.envelopeKeys) == 0 {
		return ReencryptStats{}, ErrNoEnvelopeKey
	}
	if opts.VersionCommitted == nil {
		return ReencryptStats{}, errors.New("reencrypt: VersionCommitted is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	every := opts.ProgressEvery
	if every <= 0 {
		every = 100
	}
	workers := max(opts.Concurrency, 1)

	listed, err := o.List(ctx, "sites/")
	if err != nil {
		return ReencryptStats{}, err
	}

	var (
		mu    sync.Mutex
		stats ReencryptStats
	)
	record := func(outcome reencryptOutcome, key, detail string) {
		mu.Lock()
		defer mu.Unlock()
		stats.Scanned++
		switch outcome {
		case reencryptRewritten:
			stats.Rewritten++
			if opts.DryRun {
				logf("reencrypt: would rewrite %s (%s)", key, detail)
			}
		case reencryptCurrent:
			stats.Current++
		case reencryptSkipped:
			stats.Skipped++
			logf("reencrypt: skipped %s: %s", key, detail)
		case reencryptFailed:
			stats.Failed++
			logf("reencrypt: FAILED %s: %s", key, detail)
		}
		if stats.Scanned%every == 0 {
			logf("reencrypt: %s", stats)
		}
	}

	keys := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for key := range keys {
				outcome, detail := o.reencryptOne(ctx, key, opts)
				record(outcome, key, detail)
			}
		})
	}
	for _, object := range listed {
		if ctx.Err() != nil {
			break
		}
		keys <- object.Key
	}
	close(keys)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	return stats, nil
}

type reencryptOutcome int

const (
	reencryptRewritten reencryptOutcome = iota
	reencryptCurrent
	reencryptSkipped
	reencryptFailed
)

func (o *S3Objects) reencryptOne(ctx context.Context, key string, opts ReencryptOptions) (reencryptOutcome, string) {
	siteID, version, isAsset, ok := parseStoreKey(key)
	if !ok {
		return reencryptSkipped, "not an object the store writes"
	}
	metadata, err := o.head(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return reencryptSkipped, "deleted while running"
	}
	if err != nil {
		return reencryptFailed, err.Error()
	}
	if o.isCurrent(metadata) {
		return reencryptCurrent, ""
	}
	if !isAsset {
		committed, err := opts.VersionCommitted(ctx, siteID, version)
		if err != nil {
			return reencryptFailed, fmt.Sprintf("look up version row: %v", err)
		}
		if !committed {
			return reencryptSkipped, "no committed version row (an in-flight deploy, a failed deploy's leftover, or queued for retirement); never served"
		}
	}
	from := describeEnvelope(metadata)

	plaintext, err := o.Get(ctx, key, maxVersionObjectBytes)
	if errors.Is(err, ErrObjectNotFound) {
		return reencryptSkipped, "deleted while running"
	}
	if err != nil {
		// Never overwrite what could not be read: the old key (or the
		// plaintext allowance) is still needed for it.
		return reencryptFailed, fmt.Sprintf("%v; left untouched", err)
	}
	if opts.DryRun {
		return reencryptRewritten, from
	}
	want := sha256.Sum256(plaintext)
	if err := o.Put(ctx, key, plaintext, "application/octet-stream"); err != nil {
		return reencryptFailed, fmt.Sprintf("rewrite: %v", err)
	}
	stored, err := o.Get(ctx, key, maxVersionObjectBytes)
	if err != nil {
		return reencryptFailed, fmt.Sprintf("read back after rewrite: %v", err)
	}
	if got := sha256.Sum256(stored); !bytes.Equal(got[:], want[:]) {
		return reencryptFailed, "read back after rewrite: plaintext digest differs"
	}
	metadata, err = o.head(ctx, key)
	if err != nil {
		return reencryptFailed, fmt.Sprintf("read back after rewrite: %v", err)
	}
	if !o.isCurrent(metadata) {
		return reencryptFailed, "read back after rewrite: not under the first key in format 2 (" + describeEnvelope(metadata) + ")"
	}
	return reencryptRewritten, from
}

// isCurrent reports whether metadata says the object is wrapped under the
// first configured key in the key-bound format.
func (o *S3Objects) isCurrent(metadata map[string]string) bool {
	return metadata[metaEnvelopeKeyID] == o.envelopeKeys[0].ID && metadata[metaEnvelopeFormat] == envelopeFormatKeyBound
}

func describeEnvelope(metadata map[string]string) string {
	if !isEnveloped(metadata) {
		return "plaintext"
	}
	format := metadata[metaEnvelopeFormat]
	if format == "" {
		format = "1"
	}
	return fmt.Sprintf("key %q, format %s", metadata[metaEnvelopeKeyID], format)
}

// head returns an object's user metadata without its body.
func (o *S3Objects) head(ctx context.Context, key string) (map[string]string, error) {
	out, err := o.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(o.bucket), Key: o.key(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, fmt.Errorf("head %s: %w", key, err)
	}
	return out.Metadata, nil
}

// parseStoreKey recognises the two key shapes keys.go builds.
func parseStoreKey(key string) (siteID string, version int, isAsset bool, ok bool) {
	rest, found := strings.CutPrefix(key, "sites/")
	if !found || len(rest) < 37 || !isUUID(rest[:36]) || rest[36] != '/' {
		return "", 0, false, false
	}
	siteID, name := rest[:36], rest[37:]
	if assetID, found := strings.CutPrefix(name, "assets/"); found {
		return siteID, 0, true, isUUID(assetID)
	}
	if n, found := cutVersionArchiveName(name); found && "v"+strconv.Itoa(n)+".tar.gz" == name {
		return siteID, n, false, true
	}
	return "", 0, false, false
}
