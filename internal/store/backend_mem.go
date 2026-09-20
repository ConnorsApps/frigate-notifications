package store

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// memBackend is the fail-open fallback: correct within one process lifetime,
// forgotten on restart.
type memBackend struct {
	mu   sync.Mutex
	vals map[string]memEntry
}

type memEntry struct {
	val     string
	expires time.Time
}

func newMemBackend() *memBackend {
	return &memBackend{vals: make(map[string]memEntry)}
}

// getLocked reads a key, treating an expired entry as absent. Expiry is
// checked on read rather than swept on a timer: the key set is small and
// bounded by TTL-sized bursts.
func (m *memBackend) getLocked(key string, now time.Time) (string, bool) {
	e, ok := m.vals[key]
	if !ok {
		return "", false
	}
	if !e.expires.IsZero() && now.After(e.expires) {
		delete(m.vals, key)
		return "", false
	}
	return e.val, true
}

func (m *memBackend) SetNX(_ context.Context, key, val string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if _, exists := m.getLocked(key, now); exists {
		return false, nil
	}
	m.vals[key] = memEntry{val: val, expires: now.Add(ttl)}
	m.sweepLocked(now)
	return true, nil
}

func (m *memBackend) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.getLocked(key, time.Now())
	return v, ok, nil
}

func (m *memBackend) Set(_ context.Context, key, val string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[key] = memEntry{val: val, expires: time.Now().Add(ttl)}
	return nil
}

func (m *memBackend) Del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, key)
	return nil
}

func (m *memBackend) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var n int64
	if v, ok := m.getLocked(key, now); ok {
		n, _ = strconv.ParseInt(v, 10, 64)
	}
	n++
	m.vals[key] = memEntry{val: strconv.FormatInt(n, 10), expires: now.Add(ttl)}
	return n, nil
}

// sweepLocked drops expired keys so a long-lived process doesn't accumulate
// them. Cheap because the map only ever holds a few hundred entries.
func (m *memBackend) sweepLocked(now time.Time) {
	if len(m.vals) < 512 {
		return
	}
	for k, e := range m.vals {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(m.vals, k)
		}
	}
}
