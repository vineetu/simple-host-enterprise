package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 is a minimal in-memory stand-in for the three S3 operations this
// package calls, so the envelope, SSE-header and restore logic can be proven
// without a real bucket or a MinIO container (design 9.1/9.2's exit criteria
// are additionally proven by hand against MinIO on the local overlay).
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject
}

type fakeObject struct {
	body        []byte
	metadata    map[string]string
	sse         types.ServerSideEncryption
	sseKMSKeyID string
	contentType string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}}
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
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
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, fmt.Errorf("fakeS3: no such key %q", aws.ToString(in.Key))
	}
	return &s3.GetObjectOutput{
		Body:     io.NopCloser(bytes.NewReader(obj.body)),
		Metadata: maps.Clone(obj.metadata),
	}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
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
	contents := make([]types.Object, len(keys))
	for i, k := range keys {
		key := k
		contents[i] = types.Object{Key: &key}
	}
	return &s3.ListObjectsV2Output{Contents: contents}, nil
}
