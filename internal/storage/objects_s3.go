package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Config is the storage-facing subset of the server configuration,
// repeated here so this package does not import config.
type S3Config struct {
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
	// PlaintextAllowed lets an install with EnvelopeKeys still read objects
	// written before the envelope was turned on
	// (BACKUP_ENVELOPE_PLAINTEXT_ALLOWED). Otherwise such an object is
	// refused: with the envelope on, a plain object can only be one that
	// somebody with bucket access put there.
	PlaintextAllowed bool
}

// s3API is the subset of *s3.Client this package calls, so tests can
// substitute an in-memory fake for the SDK.
type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	CopyObject(ctx context.Context, params *s3.CopyObjectInput, optFns ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

// S3Objects is Objects on any bucket that speaks the S3 API — Amazon S3,
// Google Cloud Storage, Oracle Object Storage, MinIO and the rest. Every
// write carries the server-side-encryption header and, when configured, the
// client-side envelope.
type S3Objects struct {
	client       s3API
	bucket       string
	prefix       string
	sse          types.ServerSideEncryption
	sseKMSKeyID  string
	envelopeKeys []EnvelopeKey
	// plaintextAllowed: see S3Config.PlaintextAllowed.
	plaintextAllowed bool
}

// NewS3Objects constructs the client for the configured endpoint. The endpoint
// is explicit and path-style addressing is used, so a bucket does not need its
// own DNS name.
//
// Omitting the key pair falls through to the AWS SDK's default credential
// chain, which reads IRSA and EKS Pod Identity. That is an AWS-only path: on
// other platforms supply the static key pair your provider issues (GCS calls
// them HMAC keys, Oracle calls them Customer Secret Keys).
func NewS3Objects(ctx context.Context, cfg S3Config) (*S3Objects, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("a bucket is required")
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
		if !isAWSEndpoint(cfg.Endpoint) {
			// The SDK's default adds CRC checksums (and aws-chunked uploads)
			// that Google Cloud Storage and Oracle Object Storage reject.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})
	sse, err := parseSSE(cfg.SSE)
	if err != nil {
		return nil, err
	}
	objects := newS3Objects(client, cfg.Bucket, cfg.Prefix, sse, cfg.SSEKMSKeyID, cfg.EnvelopeKeys)
	objects.plaintextAllowed = cfg.PlaintextAllowed
	return objects, nil
}

func newS3Objects(client s3API, bucket, prefix string, sse types.ServerSideEncryption, sseKMSKeyID string, envelopeKeys []EnvelopeKey) *S3Objects {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Objects{
		client:       client,
		bucket:       bucket,
		prefix:       prefix,
		sse:          sse,
		sseKMSKeyID:  sseKMSKeyID,
		envelopeKeys: envelopeKeys,
	}
}

// isAWSEndpoint reports whether endpoint is Amazon S3 itself (or empty, which
// means the SDK's own AWS endpoint).
func isAWSEndpoint(endpoint string) bool {
	if endpoint == "" {
		return true
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com")
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

func (o *S3Objects) key(key string) *string { return aws.String(o.prefix + key) }

func (o *S3Objects) Put(ctx context.Context, key string, body []byte, contentType string) error {
	input := &s3.PutObjectInput{
		Bucket:               aws.String(o.bucket),
		Key:                  o.key(key),
		ServerSideEncryption: o.sse,
	}
	if o.sse == types.ServerSideEncryptionAwsKms && o.sseKMSKeyID != "" {
		input.SSEKMSKeyId = aws.String(o.sseKMSKeyID)
	}
	if len(o.envelopeKeys) > 0 {
		ciphertext, metadata, err := wrapObject(key, body, o.envelopeKeys[0])
		if err != nil {
			return fmt.Errorf("envelope-encrypt: %w", err)
		}
		input.Body = bytes.NewReader(ciphertext)
		input.ContentLength = aws.Int64(int64(len(ciphertext)))
		input.ContentType = aws.String("application/octet-stream")
		input.Metadata = metadata
	} else {
		input.Body = bytes.NewReader(body)
		input.ContentLength = aws.Int64(int64(len(body)))
		input.ContentType = aws.String(contentType)
	}
	if _, err := o.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (o *S3Objects) Get(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	out, err := o.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.bucket), Key: o.key(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > maxBytes+envelopeOverhead {
		return nil, ErrObjectTooLarge
	}
	body, err := readBounded(out.Body, maxBytes+envelopeOverhead)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	if isEnveloped(out.Metadata) {
		body, err = unwrapObject(key, body, out.Metadata, o.envelopeKeys)
		if err != nil {
			return nil, fmt.Errorf("unwrap %s: %w", key, err)
		}
	} else if len(o.envelopeKeys) > 0 && !o.plaintextAllowed {
		return nil, fmt.Errorf("get %s: object is not envelope-encrypted but BACKUP_ENVELOPE_KEY is set (BACKUP_ENVELOPE_PLAINTEXT_ALLOWED=true reads objects written before the key was added)", key)
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrObjectTooLarge
	}
	return body, nil
}

// envelopeOverhead is the GCM tag the envelope adds to a body.
const envelopeOverhead = 16

func (o *S3Objects) Delete(ctx context.Context, key string) error {
	_, err := o.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(o.bucket), Key: o.key(key)})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

func (o *S3Objects) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	var token *string
	for {
		page, err := o.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(o.bucket),
			Prefix:            o.key(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, object := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(object.Key), o.prefix)
			out = append(out, ObjectInfo{Key: key, Size: aws.ToInt64(object.Size)})
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// Copy is a server-side copy without the envelope. With it, the body is bound
// to its object key, so the copy is read and re-wrapped under the new key
// instead.
func (o *S3Objects) Copy(ctx context.Context, from, to string) error {
	if len(o.envelopeKeys) > 0 {
		body, err := o.Get(ctx, from, maxVersionObjectBytes)
		if err != nil {
			return err
		}
		return o.Put(ctx, to, body, "application/octet-stream")
	}
	input := &s3.CopyObjectInput{
		Bucket: aws.String(o.bucket),
		// Keys are built from UUIDs, version numbers and fixed words only
		// (keys.go), so the source needs no escaping.
		CopySource:           aws.String(o.bucket + "/" + o.prefix + from),
		Key:                  o.key(to),
		ServerSideEncryption: o.sse,
	}
	if o.sse == types.ServerSideEncryptionAwsKms && o.sseKMSKeyID != "" {
		input.SSEKMSKeyId = aws.String(o.sseKMSKeyID)
	}
	if _, err := o.client.CopyObject(ctx, input); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", ErrObjectNotFound, from)
		}
		return fmt.Errorf("copy %s to %s: %w", from, to, err)
	}
	return nil
}

// readyProbeKey is never written. Reading it answers NoSuchKey only to a
// caller allowed to read (and list) the bucket; without those permissions S3
// answers AccessDenied. HeadBucket alone can keep succeeding after the
// credentials lose object access, as a real bucket-policy removal showed.
const readyProbeKey = "readyz-probe"

func (o *S3Objects) Ping(ctx context.Context) error {
	if _, err := o.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(o.bucket)}); err != nil {
		return err
	}
	out, err := o.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.bucket), Key: o.key(readyProbeKey)})
	if err == nil {
		out.Body.Close()
		return nil
	}
	if isNotFound(err) {
		return nil
	}
	return fmt.Errorf("read probe object: %w", err)
}

func isNotFound(err error) bool {
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
