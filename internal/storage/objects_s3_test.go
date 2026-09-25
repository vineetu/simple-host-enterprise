package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is an in-memory stand-in for the s3API subset this package calls,
// so the envelope, SSE-header, prefix and error mapping logic can be proven
// without a real bucket (objects_minio_test.go covers a real one).
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	objects  map[string]fakeObject
	pageSize int  // ListObjectsV2 page size; 0 means unlimited
	denyGet  bool // GetObject answers AccessDenied, as after a policy removal
}

type fakeObject struct {
	body        []byte
	metadata    map[string]string
	sse         types.ServerSideEncryption
	sseKMSKeyID string
	contentType string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{bucket: "test-bucket", objects: map[string]fakeObject{}}
}

func (f *fakeS3) checkBucket(bucket *string) error {
	if aws.ToString(bucket) != f.bucket {
		return &types.NoSuchBucket{Message: aws.String(aws.ToString(bucket))}
	}
	return nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if in.ContentLength != nil && *in.ContentLength != int64(len(body)) {
		return nil, fmt.Errorf("fakeS3: content length %d, body %d", *in.ContentLength, len(body))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[aws.ToString(in.Key)] = fakeObject{
		body:        body,
		metadata:    maps.Clone(in.Metadata),
		sse:         in.ServerSideEncryption,
		sseKMSKeyID: aws.ToString(in.SSEKMSKeyId),
		contentType: aws.ToString(in.ContentType),
	}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	if f.denyGet {
		return nil, errors.New("api error AccessDenied: Access Denied")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{Message: aws.String("no such key " + aws.ToString(in.Key))}
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.body)),
		ContentLength: aws.Int64(int64(len(obj.body))),
		ContentType:   aws.String(obj.contentType),
		Metadata:      maps.Clone(obj.metadata),
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, aws.ToString(in.Key)) // S3 deletes are idempotent
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	sourceBucket, sourceKey, ok := strings.Cut(aws.ToString(in.CopySource), "/")
	if !ok || sourceBucket != f.bucket {
		return nil, fmt.Errorf("fakeS3: bad copy source %q", aws.ToString(in.CopySource))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	source, ok := f.objects[sourceKey]
	if !ok {
		return nil, &types.NoSuchKey{Message: aws.String("no such key " + sourceKey)}
	}
	// MetadataDirective defaults to COPY: body, metadata and content type
	// travel; the destination's encryption is what the request asks for.
	f.objects[aws.ToString(in.Key)] = fakeObject{
		body:        bytes.Clone(source.body),
		metadata:    maps.Clone(source.metadata),
		sse:         in.ServerSideEncryption,
		sseKMSKeyID: aws.ToString(in.SSEKMSKeyId),
		contentType: source.contentType,
	}
	return &s3.CopyObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if in.ContinuationToken != nil {
		start, _ = strconv.Atoi(*in.ContinuationToken)
	}
	end := len(keys)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
	}
	out := &s3.ListObjectsV2Output{}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(k), Size: aws.Int64(int64(len(f.objects[k].body)))})
	}
	if end < len(keys) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if err := f.checkBucket(in.Bucket); err != nil {
		return nil, err
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) raw(t *testing.T, key string) fakeObject {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	if !ok {
		t.Fatalf("fakeS3 has no object %q", key)
	}
	return obj
}

func TestS3ObjectsPutAppliesSSE(t *testing.T) {
	cases := []struct {
		name, sse, kmsKey string
		wantSSE           types.ServerSideEncryption
		wantKMSKey        string
	}{
		{name: "default", wantSSE: types.ServerSideEncryptionAes256},
		{name: "aes256 ignores kms key", sse: "AES256", kmsKey: "ignored", wantSSE: types.ServerSideEncryptionAes256},
		{name: "kms with key", sse: "aws:kms", kmsKey: "arn:aws:kms:k", wantSSE: types.ServerSideEncryptionAwsKms, wantKMSKey: "arn:aws:kms:k"},
		{name: "kms default key", sse: "aws:kms", wantSSE: types.ServerSideEncryptionAwsKms},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse, err := parseSSE(tc.sse)
			if err != nil {
				t.Fatal(err)
			}
			fake := newFakeS3()
			objects := newS3Objects(fake, fake.bucket, "", sse, tc.kmsKey, nil)
			ctx := context.Background()
			if err := objects.Put(ctx, "sites/a", []byte("body"), "text/plain"); err != nil {
				t.Fatal(err)
			}
			if err := objects.Copy(ctx, "sites/a", "sites/b"); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"sites/a", "sites/b"} {
				obj := fake.raw(t, key)
				if obj.sse != tc.wantSSE || obj.sseKMSKeyID != tc.wantKMSKey {
					t.Errorf("%s: sse=%q kms=%q, want %q %q", key, obj.sse, obj.sseKMSKeyID, tc.wantSSE, tc.wantKMSKey)
				}
			}
			if obj := fake.raw(t, "sites/a"); obj.contentType != "text/plain" || string(obj.body) != "body" {
				t.Errorf("plain object stored as %q %q", obj.contentType, obj.body)
			}
		})
	}
	if _, err := parseSSE("aws:kms:dsse"); err == nil {
		t.Error("parseSSE accepted an unsupported mode")
	}
}

func TestS3ObjectsEnvelope(t *testing.T) {
	fake := newFakeS3()
	keys := []EnvelopeKey{testKey("k1", 0x11)}
	objects := newS3Objects(fake, fake.bucket, "backups", types.ServerSideEncryptionAes256, "", keys)
	ctx := context.Background()
	plaintext := []byte("secret site archive bytes")

	if err := objects.Put(ctx, "sites/x/v1.tar.gz", plaintext, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	obj := fake.raw(t, "backups/sites/x/v1.tar.gz")
	if bytes.Contains(obj.body, plaintext) || obj.contentType != "application/octet-stream" || obj.metadata[metaEnvelopeKeyID] != "k1" {
		t.Fatalf("stored object is not enveloped: type=%q meta=%v", obj.contentType, obj.metadata)
	}
	got, err := objects.Get(ctx, "sites/x/v1.tar.gz", int64(len(plaintext)))
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("Get = %q, %v", got, err)
	}

	if err := objects.Copy(ctx, "sites/x/v1.tar.gz", "sites/y/v7.tar.gz"); err != nil {
		t.Fatal(err)
	}
	got, err = objects.Get(ctx, "sites/y/v7.tar.gz", 1<<20)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("Get after Copy = %q, %v", got, err)
	}

	// Without the key configured, the object cannot be read at all.
	keyless := newS3Objects(fake, fake.bucket, "backups", types.ServerSideEncryptionAes256, "", nil)
	if _, err := keyless.Get(ctx, "sites/x/v1.tar.gz", 1<<20); err == nil {
		t.Fatal("Get of an enveloped object succeeded with no key configured")
	}
	// A plain object is still readable by an envelope-configured client.
	if err := keyless.Put(ctx, "sites/plain", []byte("plain"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if got, err := objects.Get(ctx, "sites/plain", 100); err != nil || string(got) != "plain" {
		t.Fatalf("Get plain = %q, %v", got, err)
	}
}

func TestS3ObjectsGetErrors(t *testing.T) {
	for _, keys := range [][]EnvelopeKey{nil, {testKey("k1", 0x22)}} {
		fake := newFakeS3()
		objects := newS3Objects(fake, fake.bucket, "", "", "", keys)
		ctx := context.Background()
		if _, err := objects.Get(ctx, "sites/missing", 10); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("envelope=%v: missing key error = %v, want ErrObjectNotFound", keys != nil, err)
		}
		body := bytes.Repeat([]byte("x"), 100)
		if err := objects.Put(ctx, "sites/big", body, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.Get(ctx, "sites/big", 99); !errors.Is(err, ErrObjectTooLarge) {
			t.Errorf("envelope=%v: over-bound error = %v, want ErrObjectTooLarge", keys != nil, err)
		}
		if got, err := objects.Get(ctx, "sites/big", 100); err != nil || !bytes.Equal(got, body) {
			t.Errorf("envelope=%v: at-bound Get = %d bytes, %v", keys != nil, len(got), err)
		}
		if err := objects.Delete(ctx, "sites/big"); err != nil {
			t.Fatal(err)
		}
		if err := objects.Delete(ctx, "sites/big"); err != nil {
			t.Errorf("deleting a missing key: %v", err)
		}
		if _, err := objects.Get(ctx, "sites/big", 100); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Get after Delete = %v", err)
		}
		if err := objects.Copy(ctx, "sites/missing", "sites/to"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("Copy of missing source = %v, want ErrObjectNotFound", err)
		}
	}
}

func TestS3ObjectsPrefixAndList(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{
		{"backups/", "backups/"},
		{"backups", "backups/"},
		{"/backups/", "backups/"},
		{"a/b", "a/b/"},
		{"", ""},
		{"/", ""},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			fake := newFakeS3()
			fake.pageSize = 2
			objects := newS3Objects(fake, fake.bucket, tc.prefix, "", "", nil)
			ctx := context.Background()
			fake.objects["sites-outside"] = fakeObject{body: []byte("not ours")}
			names := []string{"sites/a/v1.tar.gz", "sites/a/v2.tar.gz", "sites/a/assets/x", "sites/b/v1.tar.gz", "sites/ab/v1.tar.gz"}
			for i, name := range names {
				if err := objects.Put(ctx, name, bytes.Repeat([]byte("z"), i+1), ""); err != nil {
					t.Fatal(err)
				}
				fake.raw(t, tc.want+name)
			}
			listed, err := objects.List(ctx, "sites/a/")
			if err != nil {
				t.Fatal(err)
			}
			want := []ObjectInfo{{"sites/a/assets/x", 3}, {"sites/a/v1.tar.gz", 1}, {"sites/a/v2.tar.gz", 2}}
			if fmt.Sprint(listed) != fmt.Sprint(want) {
				t.Fatalf("List = %v, want %v", listed, want)
			}
			all, err := objects.List(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			wantAll := len(names)
			if tc.want == "" {
				wantAll++ // the unprefixed bucket holds the stray object too
			}
			if len(all) != wantAll {
				t.Fatalf("List(\"\") = %v", all)
			}
		})
	}
}

func TestS3ObjectsPing(t *testing.T) {
	fake := newFakeS3()
	if err := newS3Objects(fake, fake.bucket, "", "", "", nil).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := newS3Objects(fake, "other", "", "", "", nil).Ping(context.Background()); err == nil {
		t.Fatal("Ping of a missing bucket succeeded")
	}
	// The bucket still answers HeadBucket but objects can no longer be read.
	fake.denyGet = true
	if err := newS3Objects(fake, fake.bucket, "p", "", "", nil).Ping(context.Background()); err == nil {
		t.Fatal("Ping without object read access succeeded")
	}
}

func TestStoreOverS3WithEnvelope(t *testing.T) {
	fake := newFakeS3()
	objects := newS3Objects(fake, fake.bucket, "p", types.ServerSideEncryptionAes256, "", []EnvelopeKey{testKey("k1", 0x33)})
	store, _ := newTestStore(t, objects, nil, 1<<30)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("<h1>enveloped</h1>")})
	lease := mustOpenVersion(t, store, testSiteA, 1)
	defer lease.Close()
	if got := readLeaseFile(t, lease, "index.html"); string(got) != "<h1>enveloped</h1>" {
		t.Fatalf("index.html = %q", got)
	}
}

func TestIsAWSEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"":                                   true,
		"https://s3.amazonaws.com":           true,
		"https://s3.us-east-1.amazonaws.com": true,
		"https://bucket.s3.eu-west-1.amazonaws.com:443":                true,
		"https://S3.US-WEST-2.AMAZONAWS.COM":                           true,
		"http://127.0.0.1:9000":                                        false,
		"http://minio:9000":                                            false,
		"https://storage.googleapis.com":                               false,
		"https://ns.compat.objectstorage.us-ashburn-1.oraclecloud.com": false,
		"https://amazonaws.com.evil.example":                           false,
		"https://evilamazonaws.com":                                    false,
		"https://s3.amazonaws.com@evil.example":                        false,
		"::not a url":                                                  false,
	} {
		if got := isAWSEndpoint(endpoint); got != want {
			t.Errorf("isAWSEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

// Amazon S3 itself keeps the SDK's default checksum behaviour and credential
// chain (IRSA, EKS Pod Identity) when no key pair is given; only other
// endpoints drop to "when required", which GCS and Oracle need.
func TestNewS3ObjectsChecksumModeByEndpoint(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		aws      bool
	}{
		{endpoint: "", aws: true},
		{endpoint: "https://s3.us-east-1.amazonaws.com", aws: true},
		{endpoint: "https://storage.googleapis.com", aws: false},
		{endpoint: "http://minio.simple-host.svc.cluster.local:9000", aws: false},
	} {
		objects, err := NewS3Objects(context.Background(), S3Config{Endpoint: test.endpoint, Region: "us-east-1", Bucket: "b", SSE: "aws:kms", SSEKMSKeyID: "arn:aws:kms:us-east-1:111122223333:key/k"})
		if err != nil {
			t.Fatalf("%q: %v", test.endpoint, err)
		}
		options := objects.client.(*s3.Client).Options()
		whenRequired := options.RequestChecksumCalculation == aws.RequestChecksumCalculationWhenRequired &&
			options.ResponseChecksumValidation == aws.ResponseChecksumValidationWhenRequired
		if whenRequired == test.aws {
			t.Errorf("%q: request=%v response=%v, want SDK default on AWS and WhenRequired elsewhere", test.endpoint, options.RequestChecksumCalculation, options.ResponseChecksumValidation)
		}
		if test.endpoint == "" && options.BaseEndpoint != nil {
			t.Errorf("AWS default endpoint was overridden: %v", *options.BaseEndpoint)
		}
		if objects.sse != "aws:kms" || objects.sseKMSKeyID == "" {
			t.Errorf("%q: SSE-KMS not carried: %q %q", test.endpoint, objects.sse, objects.sseKMSKeyID)
		}
	}
}
