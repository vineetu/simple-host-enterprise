package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Backup uploads gzipped tarballs of version directories to an S3-compatible
// bucket. The client is constructed once at process start. Expiry of old
// backups is a bucket lifecycle rule the installer sets; not server-managed.
type Backup struct {
	client       s3API
	bucket       string
	prefix       string
	sse          types.ServerSideEncryption
	sseKMSKeyID  string
	envelopeKeys []EnvelopeKey
}

// s3API is the subset of *s3.Client this package calls. Depending on an
// interface rather than the concrete SDK type lets tests substitute an
// in-memory fake instead of a real bucket or a MinIO container: the envelope
// and restore logic below is what needs proving, not the AWS SDK's own wire
// format.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// EnvelopeKey is one named 32-byte AES-256 key for the optional client-side
// backup envelope (design 9.1). The first key in Backup.envelopeKeys wraps
// every new object; every key is tried, by id, to unwrap an existing one, so
// key rotation is: add the new key second, deploy, swap the order, deploy,
// remove the old key.
type EnvelopeKey struct {
	ID  string
	Key []byte
}

// NewBackup constructs the object-storage client for the configured endpoint.
// Any store that speaks the S3 API is acceptable — Amazon S3, Google Cloud
// Storage, Oracle Object Storage, MinIO and the rest: the endpoint is explicit,
// path-style addressing is used so a bucket does not need its own DNS name, and
// the key pair is optional.
//
// Omitting the key pair falls through to the AWS SDK's default credential
// chain, which reads IRSA and EKS Pod Identity. That is an AWS-only path: the
// chain has no idea what a GKE or AKS workload identity is, so on those
// platforms supply the static key pair your provider issues (GCS calls them
// HMAC keys, Oracle calls them Customer Secret Keys). An empty bucket disables
// backups and returns nil, which every method treats as "nothing to do".
func NewBackup(ctx context.Context, cfg BackupConfig) (*Backup, error) {
	if cfg.Bucket == "" {
		return nil, nil
	}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load s3 config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})
	sse, err := parseSSE(cfg.SSE)
	if err != nil {
		return nil, err
	}
	return newBackup(client, cfg.Bucket, cfg.Prefix, sse, cfg.SSEKMSKeyID, cfg.EnvelopeKeys), nil
}

func newBackup(client s3API, bucket, prefix string, sse types.ServerSideEncryption, sseKMSKeyID string, envelopeKeys []EnvelopeKey) *Backup {
	return &Backup{
		client:       client,
		bucket:       bucket,
		prefix:       strings.TrimSuffix(prefix, "/") + "/",
		sse:          sse,
		sseKMSKeyID:  sseKMSKeyID,
		envelopeKeys: envelopeKeys,
	}
}

func parseSSE(value string) (types.ServerSideEncryption, error) {
	switch value {
	case "", string(types.ServerSideEncryptionAes256):
		return types.ServerSideEncryptionAes256, nil
	case string(types.ServerSideEncryptionAwsKms):
		return types.ServerSideEncryptionAwsKms, nil
	default:
		return "", fmt.Errorf("unsupported server-side-encryption mode %q", value)
	}
}

// BackupConfig is the storage-facing subset of the server configuration,
// repeated here so this package does not import config.
type BackupConfig struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	// SSE is the x-amz-server-side-encryption header value: "AES256" (or
	// empty, which means the same thing) or "aws:kms".
	SSE string
	// SSEKMSKeyID is sent as x-amz-server-side-encryption-aws-kms-key-id when
	// SSE is "aws:kms"; ignored otherwise.
	SSEKMSKeyID string
	// EnvelopeKeys is the optional client-side envelope. Empty disables it.
	EnvelopeKeys []EnvelopeKey
}

// Object metadata keys for the client-side envelope. Stored without the
// "x-amz-meta-" prefix the SDK adds and strips automatically.
const (
	metaEnvelopeKeyID   = "sh-envelope-key-id"
	metaEnvelopeWrapNC  = "sh-envelope-wrap-nonce"
	metaEnvelopeWrapKey = "sh-envelope-wrapped-key"
	metaEnvelopeDataNC  = "sh-envelope-data-nonce"

	dataKeyLength = 32 // AES-256
)

// BackupVersion tarballs the on-disk version directory in memory and
// uploads it to S3 with a timestamped key. Designed to be called as a
// fire-and-forget goroutine after a successful upload; failures are
// returned (so the caller can log) but never propagate to the user.
//
// The compressed backup can approach the 500 MiB extracted-site limit and this
// method buffers it in memory. Callers must apply their process-wide backup
// memory admission gate before invoking it. Same-region S3 PutObject completes
// in seconds; no need for multipart streaming.
func (b *Backup) BackupVersion(ctx context.Context, siteDir, username, siteName string, version int) error {
	if b == nil {
		return nil
	}

	versionDir := filepath.Join(siteDir, username, siteName, fmt.Sprintf("v%d", version))
	if _, err := os.Lstat(versionDir); errors.Is(err, os.ErrNotExist) {
		archivePath := versionDir + ".tar.gz"
		if info, archiveErr := os.Lstat(archivePath); archiveErr == nil && info.Mode().IsRegular() {
			// A background backup must not materialize an archive and inflate the
			// disk it is meant to protect. The durable archive is skipped instead.
			return nil
		}
	}
	body, err := buildTarGz(versionDir)
	if err != nil {
		return fmt.Errorf("build tar.gz for %s/%s/v%d: %w", username, siteName, version, err)
	}

	key := fmt.Sprintf("%s%s/%s/v%d-%s.tar.gz",
		b.prefix,
		username,
		siteName,
		version,
		time.Now().UTC().Format("20060102T150405Z"),
	)
	if err := b.put(ctx, key, body, "application/gzip"); err != nil {
		return fmt.Errorf("put s3 object %s: %w", key, err)
	}
	return nil
}

// put uploads body under key, applying the configured server-side-encryption
// header and, when configured, the client-side envelope. contentType is what
// gets sent when the envelope is off; enveloped bodies are opaque ciphertext
// and are sent as application/octet-stream regardless.
func (b *Backup) put(ctx context.Context, key string, body []byte, contentType string) error {
	input := &s3.PutObjectInput{
		Bucket:               aws.String(b.bucket),
		Key:                  aws.String(key),
		ServerSideEncryption: b.sse,
	}
	if b.sse == types.ServerSideEncryptionAwsKms && b.sseKMSKeyID != "" {
		input.SSEKMSKeyId = aws.String(b.sseKMSKeyID)
	}
	if len(b.envelopeKeys) > 0 {
		ciphertext, metadata, err := wrapObject(body, b.envelopeKeys[0])
		if err != nil {
			return fmt.Errorf("envelope-encrypt: %w", err)
		}
		input.Body = bytes.NewReader(ciphertext)
		input.ContentType = aws.String("application/octet-stream")
		input.Metadata = metadata
	} else {
		input.Body = bytes.NewReader(body)
		input.ContentType = aws.String(contentType)
	}
	_, err := b.client.PutObject(ctx, input)
	return err
}

// wrapObject encrypts plaintext under a fresh per-object data key with
// AES-256-GCM, then wraps that data key with the given envelope key (also
// AES-256-GCM, its own nonce). The wrapped key and both nonces travel as
// object metadata; the ciphertext is the object body. Unwrapping needs only
// the envelope key and this metadata, never the plaintext data key at rest
// anywhere.
func wrapObject(plaintext []byte, envelopeKey EnvelopeKey) ([]byte, map[string]string, error) {
	dataKey := make([]byte, dataKeyLength)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, nil, fmt.Errorf("generate data key: %w", err)
	}

	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return nil, nil, err
	}
	dataNonce := make([]byte, dataGCM.NonceSize())
	if _, err := rand.Read(dataNonce); err != nil {
		return nil, nil, fmt.Errorf("generate data nonce: %w", err)
	}
	ciphertext := dataGCM.Seal(nil, dataNonce, plaintext, nil)

	wrapGCM, err := newGCM(envelopeKey.Key)
	if err != nil {
		return nil, nil, err
	}
	wrapNonce := make([]byte, wrapGCM.NonceSize())
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, nil, fmt.Errorf("generate wrap nonce: %w", err)
	}
	// The envelope key id is bound in as GCM associated data so a wrapped key
	// only authenticates under the id it was actually wrapped with. Without
	// this, swapping the sh-envelope-key-id metadata field to name a
	// different configured key that happens to share the same raw key bytes
	// (e.g. a botched rotation entry) would unwrap silently under the wrong
	// id instead of failing.
	wrappedKey := wrapGCM.Seal(nil, wrapNonce, dataKey, []byte(envelopeKey.ID))

	metadata := map[string]string{
		metaEnvelopeKeyID:   envelopeKey.ID,
		metaEnvelopeWrapNC:  base64.StdEncoding.EncodeToString(wrapNonce),
		metaEnvelopeWrapKey: base64.StdEncoding.EncodeToString(wrappedKey),
		metaEnvelopeDataNC:  base64.StdEncoding.EncodeToString(dataNonce),
	}
	return ciphertext, metadata, nil
}

// unwrapObject reverses wrapObject. It looks up the envelope key named in the
// object's metadata among the configured keys (by id, so a rotation in
// progress unwraps objects wrapped under either the old or the new key), then
// decrypts the data key and the body in turn. AES-GCM's tag makes both steps
// fail closed on any corruption or tampering.
func unwrapObject(ciphertext []byte, metadata map[string]string, keys []EnvelopeKey) ([]byte, error) {
	keyID := metadata[metaEnvelopeKeyID]
	var envelopeKey *EnvelopeKey
	for i := range keys {
		if keys[i].ID == keyID {
			envelopeKey = &keys[i]
			break
		}
	}
	if envelopeKey == nil {
		return nil, fmt.Errorf("no configured envelope key matches object key id %q", keyID)
	}

	wrapNonce, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeWrapNC])
	if err != nil {
		return nil, fmt.Errorf("decode wrap nonce: %w", err)
	}
	wrappedKey, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeWrapKey])
	if err != nil {
		return nil, fmt.Errorf("decode wrapped key: %w", err)
	}
	dataNonce, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeDataNC])
	if err != nil {
		return nil, fmt.Errorf("decode data nonce: %w", err)
	}

	wrapGCM, err := newGCM(envelopeKey.Key)
	if err != nil {
		return nil, err
	}
	dataKey, err := wrapGCM.Open(nil, wrapNonce, wrappedKey, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("unwrap data key (key id %q): %w", keyID, err)
	}

	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataGCM.Open(nil, dataNonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt object body (key id %q): %w", keyID, err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm mode: %w", err)
	}
	return gcm, nil
}

// isEnveloped reports whether metadata carries the client-side envelope
// this package writes, as opposed to a plain object (no envelope configured
// at write time, or an object this package never wrote).
func isEnveloped(metadata map[string]string) bool {
	return metadata[metaEnvelopeKeyID] != ""
}

func buildTarGz(root string) ([]byte, error) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
