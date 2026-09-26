package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// minioObjects connects to the S3-compatible endpoint in
// STORAGE_TEST_S3_ENDPOINT (MinIO in practice), creating the bucket if
// needed, under a random prefix that is emptied afterwards. Skips when the
// endpoint is unset. MinIO must run with MINIO_KMS_SECRET_KEY set, as
// deploy/components/minio does: every Put carries the SSE header, which
// MinIO refuses (501 NotImplemented) without a KMS backend.
func minioObjects(t *testing.T, envelope []EnvelopeKey) *S3Objects {
	t.Helper()
	endpoint := os.Getenv("STORAGE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("STORAGE_TEST_S3_ENDPOINT not set")
	}
	bucket := os.Getenv("STORAGE_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "test"
	}
	random := make([]byte, 6)
	rand.Read(random)
	ctx := context.Background()
	objects, err := NewS3Objects(ctx, S3Config{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		Bucket:          bucket,
		Prefix:          "storage-test-" + hex.EncodeToString(random),
		AccessKeyID:     os.Getenv("STORAGE_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("STORAGE_TEST_S3_SECRET_KEY"),
		EnvelopeKeys:    envelope,
	})
	if err != nil {
		t.Fatalf("NewS3Objects: %v", err)
	}
	if err := objects.Ping(ctx); err != nil {
		client := objects.client.(*s3.Client)
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			var owned *types.BucketAlreadyOwnedByYou
			if !errors.As(err, &owned) {
				t.Fatalf("create bucket %s: %v", bucket, err)
			}
		}
	}
	t.Cleanup(func() {
		listed, err := objects.List(context.Background(), "")
		if err != nil {
			t.Logf("cleanup list: %v", err)
			return
		}
		for _, object := range listed {
			objects.Delete(context.Background(), object.Key)
		}
	})
	return objects
}

func TestMinIOObjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envelope []EnvelopeKey
	}{
		{"plain", nil},
		{"enveloped", []EnvelopeKey{testKey("k1", 0x5a)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := minioObjects(t, tc.envelope)
			ctx := context.Background()
			if err := objects.Ping(ctx); err != nil {
				t.Fatalf("Ping: %v", err)
			}
			body := bytes.Repeat([]byte("minio round trip "), 1000)
			if err := objects.Put(ctx, "sites/a/v1.tar.gz", body, "application/gzip"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if err := objects.Put(ctx, "sites/a/assets/x", []byte("asset"), "text/plain"); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := objects.Get(ctx, "sites/a/v1.tar.gz", int64(len(body)))
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("Get = %d bytes, %v", len(got), err)
			}
			if _, err := objects.Get(ctx, "sites/a/v1.tar.gz", int64(len(body))-1); !errors.Is(err, ErrObjectTooLarge) {
				t.Fatalf("Get over bound = %v, want ErrObjectTooLarge", err)
			}
			if _, err := objects.Get(ctx, "sites/a/missing", 10); !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("Get missing = %v, want ErrObjectNotFound", err)
			}

			// What is actually stored: ciphertext only when enveloped.
			client := objects.client.(*s3.Client)
			raw, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(objects.bucket), Key: objects.key("sites/a/v1.tar.gz")})
			if err != nil {
				t.Fatal(err)
			}
			stored, _ := readBounded(raw.Body, 1<<20)
			raw.Body.Close()
			if enveloped := !bytes.Equal(stored, body); enveloped != (tc.envelope != nil) {
				t.Fatalf("stored body enveloped = %v, want %v", enveloped, tc.envelope != nil)
			}

			if err := objects.Copy(ctx, "sites/a/v1.tar.gz", "sites/b/v3.tar.gz"); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			if got, err := objects.Get(ctx, "sites/b/v3.tar.gz", 1<<20); err != nil || !bytes.Equal(got, body) {
				t.Fatalf("Get after Copy = %d bytes, %v", len(got), err)
			}
			if err := objects.Copy(ctx, "sites/a/missing", "sites/b/v4.tar.gz"); !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("Copy missing = %v, want ErrObjectNotFound", err)
			}

			listed, err := objects.List(ctx, "sites/a/")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(listed) != 2 || listed[0].Key != "sites/a/assets/x" || listed[1].Key != "sites/a/v1.tar.gz" || listed[1].Size != int64(len(stored)) {
				t.Fatalf("List = %v", listed)
			}
			for _, object := range listed {
				if strings.HasPrefix(object.Key, "storage-test-") {
					t.Fatalf("List did not strip the prefix: %v", listed)
				}
			}

			if err := objects.Delete(ctx, "sites/a/v1.tar.gz"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := objects.Delete(ctx, "sites/a/v1.tar.gz"); err != nil {
				t.Fatalf("Delete missing: %v", err)
			}
			if _, err := objects.Get(ctx, "sites/a/v1.tar.gz", 1<<20); !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("Get after Delete = %v", err)
			}
		})
	}
}

func TestMinIOStoreRoundTrip(t *testing.T) {
	objects := minioObjects(t, []EnvelopeKey{testKey("k1", 0x6b)})
	store, _ := newTestStore(t, objects, mapIndex{"alice/demo": {id: testSiteA, version: 2}}, 1<<30)
	ctx := context.Background()
	mustPutVersion(t, store, testSiteA, 2, map[string][]byte{
		"index.html":     []byte("<h1>from minio</h1>"),
		"css/site.css":   []byte("body{}"),
		"img/deep/a.bin": {0, 1, 2, 3},
	})
	lease, err := store.OpenCurrent(ctx, "alice", "demo")
	if err != nil {
		t.Fatalf("OpenCurrent: %v", err)
	}
	if got := readLeaseFile(t, lease, "img/deep/a.bin"); !bytes.Equal(got, []byte{0, 1, 2, 3}) {
		t.Fatalf("a.bin = %v", got)
	}
	lease.Close()

	content := append(append([]byte(nil), pngMagic...), "png body"...)
	stored, err := store.CreateAsset(ctx, testSiteA, "", bytes.NewReader(content), generousLimits)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	asset, err := store.OpenAsset(ctx, testSiteA, stored.ID, generousLimits.MaxFileBytes, stored.SHA256[:])
	if err != nil {
		t.Fatalf("OpenAsset: %v", err)
	}
	got, _ := readBounded(asset.File, 1<<20)
	asset.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("asset bytes differ")
	}
	if err := store.DeleteAsset(ctx, testSiteA, stored.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenAsset(ctx, testSiteA, testSiteC, generousLimits.MaxFileBytes, nil); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("OpenAsset unknown = %v, want ErrAssetNotFound", err)
	}
	usage, err := store.Usage(ctx)
	if err != nil || usage[testSiteA].VersionBytes[2] == 0 {
		t.Fatalf("Usage = %v, %v", usage, err)
	}
}
