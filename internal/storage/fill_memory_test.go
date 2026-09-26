package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const bigObjectBytes = 32 << 20

// allocatedDuring reports the bytes allocated on the heap while fn ran. Tests
// in this package do not run in parallel, so the figure is fn's own.
func allocatedDuring(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenVersionStreamsLargeArchive(t *testing.T) {
	store, _ := newTestStore(t, NewMemoryObjects(), nil, 1<<30)
	// Random bytes do not compress, so the archive is as big as the file.
	content := randomBytes(t, bigObjectBytes)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"big.bin": content})

	var lease *VersionLease
	allocated := allocatedDuring(func() { lease = mustOpenVersion(t, store, testSiteA, 1) })
	defer lease.Close()
	if allocated > bigObjectBytes/4 {
		t.Fatalf("cache fill allocated %d bytes for a %d-byte archive; it should stream through a file", allocated, bigObjectBytes)
	}
	if got := readLeaseFile(t, lease, "big.bin"); !bytes.Equal(got, content) {
		t.Fatal("big.bin differs after the fill")
	}
}

func TestOpenAssetStreamsLargeObject(t *testing.T) {
	objects := NewMemoryObjects()
	store, _ := newTestStore(t, objects, nil, 1<<30)
	content := randomBytes(t, bigObjectBytes)
	id, err := newAssetID()
	if err != nil {
		t.Fatal(err)
	}
	key, err := AssetKey(testSiteA, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(context.Background(), key, content, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	// A wrong digest is still refused, and leaves nothing cached.
	wrong := sha256.Sum256([]byte("other"))
	if _, err := store.OpenAsset(context.Background(), testSiteA, id, bigObjectBytes, wrong[:]); err == nil {
		t.Fatal("OpenAsset accepted an object that does not match its recorded sha256")
	}
	// The size bound still holds.
	if _, err := store.OpenAsset(context.Background(), testSiteA, id, bigObjectBytes-1, sum[:]); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("over-bound OpenAsset = %v, want ErrObjectTooLarge", err)
	}

	var lease *AssetLease
	allocated := allocatedDuring(func() {
		lease, err = store.OpenAsset(context.Background(), testSiteA, id, bigObjectBytes, sum[:])
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if allocated > bigObjectBytes/4 {
		t.Fatalf("asset fill allocated %d bytes for a %d-byte object; it should stream to the cache file", allocated, bigObjectBytes)
	}
	got, err := io.ReadAll(lease.File)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("asset content differs after the fill (%v)", err)
	}
}

func TestS3GetToStreamsPlainObjects(t *testing.T) {
	fake := newFakeS3()
	objects := newS3Objects(fake, fake.bucket, "", "", "", nil)
	content := randomBytes(t, bigObjectBytes)
	if err := objects.Put(context.Background(), "sites/big", content, ""); err != nil {
		t.Fatal(err)
	}
	hasher := sha256.New()
	var written int64
	var err error
	allocated := allocatedDuring(func() {
		written, err = objects.GetTo(context.Background(), "sites/big", bigObjectBytes, hasher)
	})
	if err != nil || written != bigObjectBytes {
		t.Fatalf("GetTo = %d, %v", written, err)
	}
	if allocated > bigObjectBytes/4 {
		t.Fatalf("plain GetTo allocated %d bytes for a %d-byte object", allocated, bigObjectBytes)
	}
	if want := sha256.Sum256(content); !bytes.Equal(hasher.Sum(nil), want[:]) {
		t.Fatal("streamed body differs")
	}
	if _, err := objects.GetTo(context.Background(), "sites/big", bigObjectBytes-1, io.Discard); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("over-bound GetTo = %v, want ErrObjectTooLarge", err)
	}
}

func TestS3GetToEnvelopeBuffersOnce(t *testing.T) {
	fake := newFakeS3()
	keys := []EnvelopeKey{testKey("k1", 0x44)}
	objects := newS3Objects(fake, fake.bucket, "", types.ServerSideEncryptionAes256, "", keys)
	ctx := context.Background()
	content := randomBytes(t, bigObjectBytes)
	if err := objects.Put(ctx, "sites/big", content, ""); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.Grow(bigObjectBytes)
	var err error
	allocated := allocatedDuring(func() { _, err = objects.GetTo(ctx, "sites/big", bigObjectBytes, &out) })
	if err != nil || !bytes.Equal(out.Bytes(), content) {
		t.Fatalf("enveloped GetTo: %v", err)
	}
	// One buffer for the body, decrypted in place: well under two copies.
	if allocated > bigObjectBytes*3/2 {
		t.Fatalf("enveloped GetTo allocated %d bytes for a %d-byte object; want about one copy", allocated, bigObjectBytes)
	}
	if _, err := objects.GetTo(ctx, "sites/big", bigObjectBytes-1, io.Discard); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("over-bound enveloped GetTo = %v, want ErrObjectTooLarge", err)
	}
	// A plain object is still refused when the envelope is on.
	plain := newS3Objects(fake, fake.bucket, "", "", "", nil)
	if err := plain.Put(ctx, "sites/plain", []byte("plain"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.GetTo(ctx, "sites/plain", 100, io.Discard); err == nil {
		t.Fatal("enveloped GetTo accepted a plain object")
	}
	// Tampering still fails closed.
	stored := fake.objects["sites/big"]
	stored.body = append([]byte(nil), stored.body...)
	stored.body[10] ^= 0xFF
	fake.objects["sites/big"] = stored
	if _, err := objects.GetTo(ctx, "sites/big", bigObjectBytes, io.Discard); err == nil {
		t.Fatal("enveloped GetTo accepted a tampered body")
	}
}

func TestS3GetToEnvelopeWaitsForBudget(t *testing.T) {
	fake := newFakeS3()
	objects := newS3Objects(fake, fake.bucket, "", "", "", []EnvelopeKey{testKey("k1", 0x45)})
	objects.fillBudget = newMemoryBudget(1 << 20)
	ctx := context.Background()
	if err := objects.Put(ctx, "sites/obj", []byte("enveloped body"), ""); err != nil {
		t.Fatal(err)
	}
	// Another fill holds the whole budget.
	if err := objects.fillBudget.acquire(ctx, 1<<20); err != nil {
		t.Fatal(err)
	}

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := objects.GetTo(short, "sites/obj", 1<<10, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetTo with the budget taken = %v, want DeadlineExceeded", err)
	}

	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		_, err := objects.GetTo(ctx, "sites/obj", 1<<10, &out)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("GetTo finished while the budget was taken: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	objects.fillBudget.release(1 << 20)
	if err := <-done; err != nil || out.String() != "enveloped body" {
		t.Fatalf("GetTo after release = %q, %v", out.String(), err)
	}
	if !objects.fillBudget.tryAcquire(1 << 20) {
		t.Fatal("GetTo did not return its share of the budget")
	}
}

func TestMemoryBudgetBoundsConcurrency(t *testing.T) {
	const total, each, workers = 100, 40, 12
	budget := newMemoryBudget(total)
	var mu sync.Mutex
	var inUse, peak int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := budget.acquire(context.Background(), each); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inUse += each
			peak = max(peak, inUse)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inUse -= each
			mu.Unlock()
			budget.release(each)
		}()
	}
	wg.Wait()
	if peak > total || peak < each {
		t.Fatalf("peak in use = %d, want between %d and %d", peak, each, total)
	}
	// A request bigger than the whole budget still runs, alone.
	if err := budget.acquire(context.Background(), 10*total); err != nil {
		t.Fatal(err)
	}
	if budget.tryAcquire(1) {
		t.Fatal("budget handed out bytes while an oversized request held all of it")
	}
	budget.release(10 * total)
	if !budget.tryAcquire(total) {
		t.Fatal("budget not fully returned")
	}
}
