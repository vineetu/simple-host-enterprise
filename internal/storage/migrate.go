package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
)

// The helpers below serve the operator subcommands (restore and the one-time
// migrate-storage move off the old volume). They work on Objects directly,
// with no cache, and every upload is read back and checked before it counts.
// They never overwrite an object that is already there.

// CopyVersion copies one stored version to another site and version number,
// server side.
func CopyVersion(ctx context.Context, objects Objects, fromSiteID string, fromVersion int, toSiteID string, toVersion int) error {
	from, err := VersionKey(fromSiteID, fromVersion)
	if err != nil {
		return err
	}
	to, err := VersionKey(toSiteID, toVersion)
	if err != nil {
		return err
	}
	return objects.Copy(ctx, from, to)
}

// UploadVersionDir packs an unpacked version directory from the old volume
// and uploads it as siteID's version, then verifies the stored object. It
// refuses symlinks and anything but regular files and directories, as the
// old store did. It returns the version's total file bytes.
func UploadVersionDir(ctx context.Context, objects Objects, siteID string, version int, dir *os.Root) (int64, error) {
	var archive bytes.Buffer
	stats, err := writeVersionArchive(&archive, dir)
	if err != nil {
		return 0, err
	}
	return stats.bytes, uploadVerifiedVersion(ctx, objects, siteID, version, archive.Bytes(), stats)
}

// UploadVersionArchive uploads an already-packed version (the old volume's
// vN.tar.gz form) after checking it unpacks cleanly, then verifies the stored
// object. It returns the version's total file bytes.
func UploadVersionArchive(ctx context.Context, objects Objects, siteID string, version int, archive []byte) (int64, error) {
	stats, err := scanVersionArchive(bytes.NewReader(archive), nil)
	if err != nil {
		return 0, fmt.Errorf("invalid version archive: %w", err)
	}
	return stats.bytes, uploadVerifiedVersion(ctx, objects, siteID, version, archive, stats)
}

func uploadVerifiedVersion(ctx context.Context, objects Objects, siteID string, version int, archive []byte, want regularFileStats) error {
	key, err := VersionKey(siteID, version)
	if err != nil {
		return err
	}
	// An object already at the key is verified, never replaced: after
	// cutover it is live data (a re-run must not overwrite it or pile up
	// bucket versions), and before cutover it is this command's own earlier
	// upload.
	verify := func() error {
		stored, err := objects.Get(ctx, key, maxVersionObjectBytes)
		if err != nil {
			return err
		}
		got, err := scanVersionArchive(bytes.NewReader(stored), nil)
		if err != nil {
			return fmt.Errorf("stored %s: %w", key, err)
		}
		if got != want {
			return fmt.Errorf("stored %s has %d files / %d bytes, want %d / %d", key, got.count, got.bytes, want.count, want.bytes)
		}
		return nil
	}
	err = verify()
	if !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if err := objects.Put(ctx, key, archive, "application/gzip"); err != nil {
		return err
	}
	return verify()
}

// UploadAsset uploads one asset's bytes under its existing id after checking
// them against the SHA-256 its row recorded, then verifies the stored object.
func UploadAsset(ctx context.Context, objects Objects, siteID, assetID, contentType string, body, wantSHA256 []byte) error {
	key, err := AssetKey(siteID, assetID)
	if err != nil {
		return fmt.Errorf("asset %q: %w", assetID, err)
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], wantSHA256) {
		return fmt.Errorf("asset %s does not match its recorded sha256", assetID)
	}
	verify := func() error {
		stored, err := objects.Get(ctx, key, int64(len(body)))
		if err != nil {
			return err
		}
		if sum := sha256.Sum256(stored); !bytes.Equal(sum[:], wantSHA256) {
			return fmt.Errorf("stored %s does not match its recorded sha256", key)
		}
		return nil
	}
	// As for versions: an existing object is verified, never replaced.
	err = verify()
	if !errors.Is(err, ErrObjectNotFound) {
		return err
	}
	if err := objects.Put(ctx, key, body, contentType); err != nil {
		return err
	}
	return verify()
}
