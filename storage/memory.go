package storage

import (
	"bytes"
	"sort"
	"sync"
)

// Memory is an in-memory ReadWriter with the same semantics as the
// keelstore adapter: keys are ordered, Scan is half-open, and a missing key
// returns ErrNotFound.
//
// It exists so that the layers above storage — the catalog, the planner,
// the operators — can be tested without a database on disk, and so that a
// program embedding keelsql can exercise it without one either. It is not
// durable and makes no attempt to be.
type Memory struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{m: map[string][]byte{}} }

// Get returns the value stored at key.
func (s *Memory) Get(key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[string(key)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// Put stores a value.
func (s *Memory) Put(key, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[string(key)] = append([]byte(nil), value...)
	return nil
}

// Delete removes a key.
func (s *Memory) Delete(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, string(key))
	return nil
}

// Len reports how many keys are stored.
func (s *Memory) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}

// Scan iterates [start, end). The matching entries are copied out up front,
// so writing to the store while the iterator is live is safe — and matches
// keelstore, whose iterator reads a snapshot.
func (s *Memory) Scan(start, end []byte) Iterator {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []Entry
	for k, v := range s.m {
		key := []byte(k)
		if start != nil && bytes.Compare(key, start) < 0 {
			continue
		}
		if end != nil && bytes.Compare(key, end) >= 0 {
			continue
		}
		entries = append(entries, Entry{Key: key, Value: append([]byte(nil), v...)})
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].Key, entries[j].Key) < 0
	})
	return &sliceIter{entries: entries, pos: -1}
}

type sliceIter struct {
	entries []Entry
	pos     int
}

func (s *sliceIter) Next() bool {
	s.pos++
	return s.pos < len(s.entries)
}

func (s *sliceIter) Key() []byte {
	if s.pos < 0 || s.pos >= len(s.entries) {
		return nil
	}
	return s.entries[s.pos].Key
}

func (s *sliceIter) Value() []byte {
	if s.pos < 0 || s.pos >= len(s.entries) {
		return nil
	}
	return s.entries[s.pos].Value
}

func (s *sliceIter) Err() error   { return nil }
func (s *sliceIter) Close() error { s.entries = nil; return nil }
