// Package storage is the seam between keelsql and keelstore.
//
// Everything above it — the catalog, the executor, the index maintenance —
// reads and writes through two small interfaces: a Reader that can fetch a
// key and iterate a range, and a Writer that can put and delete. keelstore
// implements the reader twice (the live database and a snapshot of it), and
// a transaction's uncommitted write set implements both, layered over a
// snapshot.
//
// That layering is what lets a statement inside a transaction see its own
// uncommitted writes while still reading a stable snapshot of everything
// else.
package storage

import (
	"bytes"
	"errors"
	"sort"

	"github.com/aminyx/keelstore"
)

// ErrNotFound is returned by Get when a key is absent. It wraps
// keelstore.ErrNotFound so that either sentinel matches with errors.Is.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "keelsql: key not found" }

// Unwrap makes errors.Is(err, keelstore.ErrNotFound) succeed as well.
func (errNotFound) Unwrap() error { return keelstore.ErrNotFound }

// An Iterator walks keys in ascending byte order. It follows the same
// call-Next-before-reading shape as keelstore's own iterator.
type Iterator interface {
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

// A Reader is a point-lookup and range-scan source.
type Reader interface {
	// Get returns the value stored at key, or ErrNotFound.
	Get(key []byte) ([]byte, error)
	// Scan iterates the half-open range [start, end). A nil bound is
	// unbounded on that side.
	Scan(start, end []byte) Iterator
}

// A Writer accepts single-key mutations.
type Writer interface {
	Put(key, value []byte) error
	Delete(key []byte) error
}

// A ReadWriter is both.
type ReadWriter interface {
	Reader
	Writer
}

// ---------------------------------------------------------------------
// keelstore adapters
// ---------------------------------------------------------------------

// Store adapts a live keelstore database.
type Store struct{ DB *keelstore.DB }

// Get reads through the database.
func (s Store) Get(key []byte) ([]byte, error) { return translate(s.DB.Get(key)) }

// Scan iterates the database.
func (s Store) Scan(start, end []byte) Iterator {
	return &storeIter{it: s.DB.NewIterator(&keelstore.IterOptions{Start: start, End: end})}
}

// Put writes through to keelstore.
func (s Store) Put(key, value []byte) error { return s.DB.Put(key, value) }

// Delete writes a tombstone through to keelstore.
func (s Store) Delete(key []byte) error { return s.DB.Delete(key) }

// Snapshot adapts a keelstore snapshot: a stable view of the database as of
// the moment it was taken.
type Snapshot struct{ Snap *keelstore.Snapshot }

// Get reads through the snapshot.
func (s Snapshot) Get(key []byte) ([]byte, error) { return translate(s.Snap.Get(key)) }

// Scan iterates the snapshot.
func (s Snapshot) Scan(start, end []byte) Iterator {
	return &storeIter{it: s.Snap.NewIterator(&keelstore.IterOptions{Start: start, End: end})}
}

func translate(v []byte, err error) ([]byte, error) {
	if errors.Is(err, keelstore.ErrNotFound) {
		return nil, ErrNotFound
	}
	return v, err
}

// storeIter copies each key and value out of keelstore's iterator. The
// iterator hands back slices into a block it owns, so anything kept past
// the next call to Next has to be copied.
type storeIter struct {
	it    *keelstore.Iterator
	key   []byte
	value []byte
}

func (s *storeIter) Next() bool {
	if !s.it.Next() {
		return false
	}
	s.key = append(s.key[:0], s.it.Key()...)
	s.value = append(s.value[:0], s.it.Value()...)
	return true
}

func (s *storeIter) Key() []byte   { return s.key }
func (s *storeIter) Value() []byte { return s.value }
func (s *storeIter) Err() error    { return s.it.Error() }
func (s *storeIter) Close() error  { return s.it.Close() }

// ---------------------------------------------------------------------
// write buffer
// ---------------------------------------------------------------------

// An Entry is one buffered mutation.
type Entry struct {
	Key     []byte
	Value   []byte
	Deleted bool
}

// A Buffer is a transaction's uncommitted write set: the newest mutation
// per key, in memory, with deletions recorded as tombstones rather than as
// removals, because a delete has to hide a row that exists in the snapshot
// underneath.
type Buffer struct {
	m      map[string]Entry
	sorted []Entry
	dirty  bool
}

// NewBuffer returns an empty write buffer.
func NewBuffer() *Buffer { return &Buffer{m: map[string]Entry{}} }

// Len reports how many keys the buffer holds.
func (b *Buffer) Len() int { return len(b.m) }

// Put records a write.
func (b *Buffer) Put(key, value []byte) error {
	b.m[string(key)] = Entry{
		Key:   append([]byte(nil), key...),
		Value: append([]byte(nil), value...),
	}
	b.dirty = true
	return nil
}

// Delete records a tombstone.
func (b *Buffer) Delete(key []byte) error {
	b.m[string(key)] = Entry{Key: append([]byte(nil), key...), Deleted: true}
	b.dirty = true
	return nil
}

// Lookup returns the buffered entry for a key, if there is one.
func (b *Buffer) Lookup(key []byte) (Entry, bool) {
	e, ok := b.m[string(key)]
	return e, ok
}

// Entries returns every mutation in ascending key order. Commit applies
// them in this order, which keeps the sequence of keelstore writes
// deterministic.
func (b *Buffer) Entries() []Entry {
	if b.dirty {
		// A fresh slice every time, never a reused one: a scan started
		// earlier in the statement still holds a sub-slice of the previous
		// array and must not see it rewritten underneath.
		b.sorted = make([]Entry, 0, len(b.m))
		for _, e := range b.m {
			b.sorted = append(b.sorted, e)
		}
		sort.Slice(b.sorted, func(i, j int) bool {
			return bytes.Compare(b.sorted[i].Key, b.sorted[j].Key) < 0
		})
		b.dirty = false
	}
	return b.sorted
}

// Reset empties the buffer.
func (b *Buffer) Reset() {
	b.m = map[string]Entry{}
	b.sorted = nil
	b.dirty = false
}

// rangeEntries returns the buffered entries inside [start, end).
func (b *Buffer) rangeEntries(start, end []byte) []Entry {
	all := b.Entries()
	lo := sort.Search(len(all), func(i int) bool {
		return start == nil || bytes.Compare(all[i].Key, start) >= 0
	})
	hi := len(all)
	if end != nil {
		hi = sort.Search(len(all), func(i int) bool {
			return bytes.Compare(all[i].Key, end) >= 0
		})
	}
	if lo > hi {
		return nil
	}
	return all[lo:hi]
}

// ---------------------------------------------------------------------
// overlay
// ---------------------------------------------------------------------

// An Overlay reads a write buffer layered on top of a base reader. Writes
// go to the buffer; reads see the buffer first and the base underneath.
type Overlay struct {
	Base Reader
	Buf  *Buffer
}

// NewOverlay layers buf over base.
func NewOverlay(base Reader, buf *Buffer) *Overlay { return &Overlay{Base: base, Buf: buf} }

// Put buffers a write.
func (o *Overlay) Put(key, value []byte) error { return o.Buf.Put(key, value) }

// Delete buffers a tombstone.
func (o *Overlay) Delete(key []byte) error { return o.Buf.Delete(key) }

// Get consults the buffer first: an uncommitted write shadows the snapshot,
// and an uncommitted delete hides it.
func (o *Overlay) Get(key []byte) ([]byte, error) {
	if e, ok := o.Buf.Lookup(key); ok {
		if e.Deleted {
			return nil, ErrNotFound
		}
		return e.Value, nil
	}
	return o.Base.Get(key)
}

// Scan merges the buffer's entries with the base iterator, in key order,
// letting the buffer win on ties and skipping tombstones.
func (o *Overlay) Scan(start, end []byte) Iterator {
	return &overlayIter{
		base:     o.Base.Scan(start, end),
		buf:      o.Buf.rangeEntries(start, end),
		needBase: true,
	}
}

type overlayIter struct {
	base      Iterator
	buf       []Entry
	bi        int
	needBase  bool
	baseValid bool
	key       []byte
	value     []byte

	// Separate scratch space for rows copied out of the base iterator.
	// key and value alias the buffer's own slices when the buffer wins,
	// so they must never be reused as an append target.
	scratchKey []byte
	scratchVal []byte
}

func (m *overlayIter) Next() bool {
	for {
		if m.needBase {
			m.baseValid = m.base.Next()
			m.needBase = false
		}
		bufValid := m.bi < len(m.buf)
		switch {
		case !m.baseValid && !bufValid:
			return false

		case bufValid && (!m.baseValid || bytes.Compare(m.buf[m.bi].Key, m.base.Key()) <= 0):
			e := m.buf[m.bi]
			m.bi++
			if m.baseValid && bytes.Equal(e.Key, m.base.Key()) {
				m.needBase = true // the buffered version replaces it
			}
			if e.Deleted {
				continue
			}
			m.key, m.value = e.Key, e.Value
			return true

		default:
			m.scratchKey = append(m.scratchKey[:0], m.base.Key()...)
			m.scratchVal = append(m.scratchVal[:0], m.base.Value()...)
			m.key, m.value = m.scratchKey, m.scratchVal
			m.needBase = true
			return true
		}
	}
}

func (m *overlayIter) Key() []byte   { return m.key }
func (m *overlayIter) Value() []byte { return m.value }
func (m *overlayIter) Err() error    { return m.base.Err() }
func (m *overlayIter) Close() error  { return m.base.Close() }
