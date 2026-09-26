package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"
)

// ErrObjectNotFound is returned by Objects.Get when no object has the key.
var ErrObjectNotFound = errors.New("object not found")

// ErrObjectTooLarge is returned by Objects.Get when the object is bigger than
// the caller's bound.
var ErrObjectTooLarge = errors.New("object exceeds the size bound")

// ObjectInfo is one listed object.
type ObjectInfo struct {
	Key  string
	Size int64
}

// Objects is the bucket, and the only thing the store keeps durable bytes in.
// Keys are relative to the configured prefix and are only ever built by this
// package (see keys.go), never taken from a request.
type Objects interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	// Get returns the whole object, refusing one larger than maxBytes.
	Get(ctx context.Context, key string, maxBytes int64) ([]byte, error)
	// GetTo writes the object to w, refusing one larger than maxBytes, and
	// returns the bytes written. Cache fills use it so a large object goes
	// to disk rather than into memory. On error w may hold a partial body,
	// which the caller must discard.
	GetTo(ctx context.Context, key string, maxBytes int64, w io.Writer) (int64, error)
	// Delete removes one object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Copy(ctx context.Context, from, to string) error
	// Ping proves the bucket is reachable with the configured credentials.
	Ping(ctx context.Context) error
}

// MemoryObjects is an in-process Objects for tests and local tools. It keeps
// every object in memory and never persists anything.
type MemoryObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	// Fail, when set, is returned by every call. Tests use it to simulate an
	// unreachable bucket.
	Fail error
}

// NewMemoryObjects returns an empty in-memory bucket.
func NewMemoryObjects() *MemoryObjects {
	return &MemoryObjects{objects: map[string][]byte{}}
}

func (m *MemoryObjects) Put(_ context.Context, key string, body []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.objects[key] = bytes.Clone(body)
	return nil
}

func (m *MemoryObjects) Get(_ context.Context, key string, maxBytes int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	body, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrObjectTooLarge
	}
	return bytes.Clone(body), nil
}

func (m *MemoryObjects) GetTo(_ context.Context, key string, maxBytes int64, w io.Writer) (int64, error) {
	m.mu.Lock()
	body, ok := m.objects[key]
	fail := m.Fail
	m.mu.Unlock()
	if fail != nil {
		return 0, fail
	}
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
	}
	if int64(len(body)) > maxBytes {
		return 0, ErrObjectTooLarge
	}
	// Stored bodies are never modified in place (Put and Copy store clones),
	// so writing from it outside the lock is safe and copies nothing.
	n, err := w.Write(body)
	return int64(n), err
}

func (m *MemoryObjects) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	delete(m.objects, key)
	return nil
}

func (m *MemoryObjects) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []ObjectInfo
	for key, body := range m.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, ObjectInfo{Key: key, Size: int64(len(body))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemoryObjects) Copy(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	body, ok := m.objects[from]
	if !ok {
		return fmt.Errorf("%w: %s", ErrObjectNotFound, from)
	}
	m.objects[to] = bytes.Clone(body)
	return nil
}

func (m *MemoryObjects) Ping(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Fail
}

// Keys returns a snapshot of every key, for tests.
func (m *MemoryObjects) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for key := range maps.Keys(m.objects) {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// readBounded reads at most maxBytes from body, failing if there is more.
func readBounded(body io.Reader, maxBytes int64) ([]byte, error) {
	return readBoundedSized(body, maxBytes, -1)
}

// readBoundedSized is readBounded with the expected length (the response's
// Content-Length, or -1 when unknown), so a known-size body is read into one
// exact allocation instead of a buffer that doubles as it grows.
func readBoundedSized(body io.Reader, maxBytes, expected int64) ([]byte, error) {
	if expected < 0 || expected > maxBytes {
		data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, ErrObjectTooLarge
		}
		return data, nil
	}
	data := make([]byte, expected)
	if _, err := io.ReadFull(body, data); err != nil {
		return nil, err
	}
	var extra [1]byte
	if n, _ := io.ReadFull(body, extra[:]); n != 0 {
		return nil, errors.New("body is longer than its declared length")
	}
	return data, nil
}
