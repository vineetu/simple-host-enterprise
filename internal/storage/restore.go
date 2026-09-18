package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/tarball"
)

// ErrNoBackup means no object matches the requested version, or backups are
// not configured at all (b is nil).
var ErrNoBackup = errors.New("no matching backup object")

// versionObjectPrefix is the common prefix of every backup object
// BackupVersion writes for one version: "<prefix><owner>/<site>/vN-". The
// timestamp suffix after it sorts lexicographically because BackupVersion
// formats it as a fixed-width UTC stamp, so the greatest key under this
// prefix is always the most recent backup of that version.
func versionObjectPrefix(prefix, owner, site string, version int) string {
	return fmt.Sprintf("%s%s/%s/v%d-", prefix, owner, site, version)
}

func assetsObjectPrefix(prefix, owner, site string) string {
	return fmt.Sprintf("%s%s/%s/assets/", prefix, owner, site)
}

// listKeys pages through every object under prefix.
func (b *Backup) listKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}

// latestVersionKey finds the most recent backup object for one version.
func (b *Backup) latestVersionKey(ctx context.Context, owner, site string, version int) (string, error) {
	keys, err := b.listKeys(ctx, versionObjectPrefix(b.prefix, owner, site, version))
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("%w: %s/%s v%d", ErrNoBackup, owner, site, version)
	}
	sort.Strings(keys)
	return keys[len(keys)-1], nil
}

// fetchAndUnwrap downloads one object and, when its metadata carries the
// client-side envelope, decrypts it. A plain object (no envelope at write
// time) is returned as-is. Decrypting an enveloped object with no matching
// configured key fails closed with the key id the object needs, which is
// what an operator needs to see to fix the secret rather than the data.
func (b *Backup) fetchAndUnwrap(ctx context.Context, key string) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if !isEnveloped(out.Metadata) {
		return body, nil
	}
	plaintext, err := unwrapObject(body, out.Metadata, b.envelopeKeys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return plaintext, nil
}

// RestoreVersion rebuilds one version from its most recent backup object into
// a target site directory, which may name a different owner or site than the
// one the backup was taken from (design 10.5: "restore ... is how any
// environment gets a copy of another's content"). It returns the object key
// it restored from. When setCurrent is true, the restored version also
// becomes the target site's serving version, through the same atomic
// current-symlink swap a normal deploy uses.
func (b *Backup) RestoreVersion(ctx context.Context, disk *DiskStorage, sourceOwner, sourceSite string, version int, targetOwner, targetSite string, setCurrent bool) (string, error) {
	if b == nil {
		return "", errors.New("backups are not configured")
	}
	key, err := b.latestVersionKey(ctx, sourceOwner, sourceSite, version)
	if err != nil {
		return "", err
	}
	body, err := b.fetchAndUnwrap(ctx, key)
	if err != nil {
		return "", err
	}
	files, err := tarball.ExtractBytes(body, "restore.tar.gz")
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", key, err)
	}
	if err := tarball.ValidateExtensions(files); err != nil {
		return "", fmt.Errorf("validate %s: %w", key, err)
	}
	if err := disk.WriteFiles(targetOwner, targetSite, version, files); err != nil {
		return "", fmt.Errorf("write %s/%s v%d: %w", targetOwner, targetSite, version, err)
	}
	if setCurrent {
		if err := disk.SetCurrentVersion(targetOwner, targetSite, version); err != nil {
			return "", fmt.Errorf("set current for %s/%s: %w", targetOwner, targetSite, err)
		}
	}
	return key, nil
}

// BackupAssets uploads every regular file under <siteDir>/<owner>/<site>/assets
// to the bucket, under the same prefix, SSE header and envelope as a version
// backup. It is safe to run repeatedly: every file is re-uploaded each time
// (no incremental diff), which keeps the sync logic simple and matches asset
// volumes small enough that a full resync is cheap. Returns the number of
// files uploaded. A missing assets directory is not an error: most sites have
// none, and Phase 3 is what starts creating one.
func (b *Backup) BackupAssets(ctx context.Context, siteDir, owner, site string) (int, error) {
	if b == nil {
		return 0, errors.New("backups are not configured")
	}
	assetsDir := filepath.Join(siteDir, owner, site, "assets")
	info, err := os.Stat(assetsDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", assetsDir, err)
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", assetsDir)
	}

	uploaded := 0
	err = filepath.Walk(assetsDir, func(path string, fileInfo os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fileInfo.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(assetsDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if canonical, err := safepath.CanonicalRelativePath(rel, false); err != nil || canonical != rel {
			return fmt.Errorf("refuse to back up unsafe asset path %q", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		key := assetsObjectPrefix(b.prefix, owner, site) + rel
		if err := b.put(ctx, key, data, "application/octet-stream"); err != nil {
			return fmt.Errorf("put %s: %w", key, err)
		}
		uploaded++
		return nil
	})
	if err != nil {
		return uploaded, err
	}
	return uploaded, nil
}

// RestoreAssets downloads every object backed up for one site's assets
// directory into <targetDir>/assets/<relative path>, unwrapping the envelope
// where the object carries one. Returns the number of files restored.
func (b *Backup) RestoreAssets(ctx context.Context, sourceOwner, sourceSite, targetDir string) (int, error) {
	if b == nil {
		return 0, errors.New("backups are not configured")
	}
	prefix := assetsObjectPrefix(b.prefix, sourceOwner, sourceSite)
	keys, err := b.listKeys(ctx, prefix)
	if err != nil {
		return 0, err
	}
	assetsDir := filepath.Join(targetDir, "assets")
	restored := 0
	for _, key := range keys {
		rel := strings.TrimPrefix(key, prefix)
		if canonical, err := safepath.CanonicalRelativePath(rel, false); err != nil || canonical != rel {
			return restored, fmt.Errorf("refuse to restore unsafe asset key %q", key)
		}
		body, err := b.fetchAndUnwrap(ctx, key)
		if err != nil {
			return restored, err
		}
		destination := filepath.Join(assetsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return restored, fmt.Errorf("create directory for %s: %w", rel, err)
		}
		if err := os.WriteFile(destination, body, 0o644); err != nil {
			return restored, fmt.Errorf("write %s: %w", destination, err)
		}
		restored++
	}
	return restored, nil
}
