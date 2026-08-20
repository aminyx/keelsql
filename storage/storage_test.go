package storage

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func collect(t *testing.T, it Iterator) string {
	t.Helper()
	defer it.Close()
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Key())+"="+string(it.Value()))
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	return strings.Join(parts, " ")
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	m := NewMemory()
	if _, err := m.Get([]byte("nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a missing key = %v, want ErrNotFound", err)
	}
	if err := m.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	v, err := m.Get([]byte("a"))
	if err != nil || string(v) != "1" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if m.Len() != 1 {
		t.Errorf("Len = %d, want 1", m.Len())
	}
	if err := m.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get([]byte("a")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestMemoryScanIsOrderedAndHalfOpen(t *testing.T) {
	m := NewMemory()
	for _, k := range []string{"d", "a", "c", "b"} {
		m.Put([]byte(k), []byte(strings.ToUpper(k)))
	}
	if got := collect(t, m.Scan(nil, nil)); got != "a=A b=B c=C d=D" {
		t.Errorf("full scan = %q", got)
	}
	if got := collect(t, m.Scan([]byte("b"), []byte("d"))); got != "b=B c=C" {
		t.Errorf("bounded scan = %q, want the upper bound excluded", got)
	}
	if got := collect(t, m.Scan([]byte("c"), nil)); got != "c=C d=D" {
		t.Errorf("open-ended scan = %q", got)
	}
}

func TestMemoryStoreCopiesValues(t *testing.T) {
	m := NewMemory()
	value := []byte("original")
	m.Put([]byte("k"), value)
	value[0] = 'X'
	got, _ := m.Get([]byte("k"))
	if string(got) != "original" {
		t.Errorf("the store kept a reference to the caller's slice: %q", got)
	}
}

func TestBufferRecordsLatestWritePerKey(t *testing.T) {
	b := NewBuffer()
	b.Put([]byte("a"), []byte("1"))
	b.Put([]byte("a"), []byte("2"))
	b.Put([]byte("b"), []byte("3"))
	b.Delete([]byte("b"))

	if b.Len() != 2 {
		t.Errorf("Len = %d, want 2", b.Len())
	}
	e, ok := b.Lookup([]byte("a"))
	if !ok || string(e.Value) != "2" {
		t.Errorf("Lookup(a) = %v, %v", e, ok)
	}
	e, ok = b.Lookup([]byte("b"))
	if !ok || !e.Deleted {
		t.Errorf("Lookup(b) = %v, %v; want a tombstone", e, ok)
	}
	if _, ok := b.Lookup([]byte("zz")); ok {
		t.Error("Lookup of an untouched key should report false")
	}
}

func TestBufferEntriesAreSorted(t *testing.T) {
	b := NewBuffer()
	for _, k := range []string{"c", "a", "b"} {
		b.Put([]byte(k), nil)
	}
	entries := b.Entries()
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].Key, entries[i].Key) >= 0 {
			t.Fatalf("entries are not sorted: %q then %q", entries[i-1].Key, entries[i].Key)
		}
	}
	b.Reset()
	if b.Len() != 0 || len(b.Entries()) != 0 {
		t.Error("Reset should empty the buffer")
	}
}

// TestBufferEntriesSurviveLaterWrites is the aliasing rule the overlay scan
// depends on: an iterator holds a slice of the entries it started with, so
// rebuilding the sorted list must not rewrite the old array.
func TestBufferEntriesSurviveLaterWrites(t *testing.T) {
	b := NewBuffer()
	b.Put([]byte("a"), []byte("1"))
	before := b.Entries()

	b.Put([]byte("b"), []byte("2"))
	b.Entries() // forces a rebuild

	if string(before[0].Key) != "a" || string(before[0].Value) != "1" {
		t.Errorf("the earlier slice was rewritten: %v", before[0])
	}
}

func TestOverlayReadsSeeBufferedWrites(t *testing.T) {
	base := NewMemory()
	base.Put([]byte("a"), []byte("base"))
	base.Put([]byte("b"), []byte("base"))

	o := NewOverlay(base, NewBuffer())
	o.Put([]byte("a"), []byte("buffered"))
	o.Delete([]byte("b"))
	o.Put([]byte("c"), []byte("new"))

	v, err := o.Get([]byte("a"))
	if err != nil || string(v) != "buffered" {
		t.Errorf("Get(a) = %q, %v; want the buffered value", v, err)
	}
	if _, err := o.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(b) = %v, want ErrNotFound after a buffered delete", err)
	}
	if v, err := o.Get([]byte("c")); err != nil || string(v) != "new" {
		t.Errorf("Get(c) = %q, %v", v, err)
	}

	// The base is untouched until the buffer is applied.
	if v, _ := base.Get([]byte("a")); string(v) != "base" {
		t.Errorf("the base changed underneath: %q", v)
	}
}

func TestOverlayScanMerges(t *testing.T) {
	base := NewMemory()
	for _, k := range []string{"a", "c", "e"} {
		base.Put([]byte(k), []byte("base"))
	}
	o := NewOverlay(base, NewBuffer())
	o.Put([]byte("b"), []byte("buf")) // between two base keys
	o.Put([]byte("c"), []byte("buf")) // shadows a base key
	o.Delete([]byte("e"))             // hides a base key
	o.Put([]byte("f"), []byte("buf")) // after every base key
	o.Put([]byte("0"), []byte("buf")) // before every base key

	got := collect(t, o.Scan(nil, nil))
	want := "0=buf a=base b=buf c=buf f=buf"
	if got != want {
		t.Errorf("merged scan = %q\n            want %q", got, want)
	}
}

func TestOverlayScanRespectsBounds(t *testing.T) {
	base := NewMemory()
	base.Put([]byte("a"), []byte("base"))
	base.Put([]byte("d"), []byte("base"))
	o := NewOverlay(base, NewBuffer())
	o.Put([]byte("b"), []byte("buf"))
	o.Put([]byte("e"), []byte("buf"))

	if got := collect(t, o.Scan([]byte("b"), []byte("e"))); got != "b=buf d=base" {
		t.Errorf("bounded merged scan = %q", got)
	}
}

func TestOverlayScanKeysStayValidAcrossNext(t *testing.T) {
	base := NewMemory()
	base.Put([]byte("a"), []byte("base"))
	base.Put([]byte("c"), []byte("base"))
	o := NewOverlay(base, NewBuffer())
	o.Put([]byte("b"), []byte("buf"))

	it := o.Scan(nil, nil)
	defer it.Close()

	var keys [][]byte
	for it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	if len(keys) != 3 {
		t.Fatalf("got %d keys", len(keys))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(keys[i]) != want {
			t.Errorf("key %d = %q, want %q", i, keys[i], want)
		}
	}
}

func TestErrNotFoundWrapsKeelstore(t *testing.T) {
	if ErrNotFound.Error() == "" {
		t.Error("ErrNotFound should have a message")
	}
	wrapped := errors.Unwrap(ErrNotFound)
	if wrapped == nil {
		t.Fatal("ErrNotFound should unwrap to keelstore's sentinel")
	}
	if !strings.Contains(wrapped.Error(), "keelstore") {
		t.Errorf("unwrapped to %v", wrapped)
	}
}
