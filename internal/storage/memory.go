package storage

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
)

// Memory is an in-memory Storage for tests.
type Memory struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory { return &Memory{objs: map[string][]byte{}} }

// Stat implements Storage.
func (m *Memory) Stat(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

// Put implements Storage.
func (m *Memory) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = b
	return nil
}

// Get implements Storage.
func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Delete implements Storage.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// Ping implements Storage.
func (m *Memory) Ping(context.Context) error { return nil }

// Keys returns all stored keys, sorted. Test helper.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objs))
	for k := range m.objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Bytes returns an object's content. Test helper.
func (m *Memory) Bytes(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	return b, ok
}
