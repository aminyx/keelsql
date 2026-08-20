// Package keelsql is a small SQL query engine built on top of keelstore.
//
// keelstore is an LSM-tree key/value store: it knows about durability,
// ordering and snapshots, and nothing about tables. keelsql is the layer
// that turns that into a database — a lexer, a hand-written parser, a
// logical plan, a cost-lite optimiser and a volcano-style executor over an
// order-preserving key encoding. Between them they are a complete, if
// small, relational database.
//
//	db, err := keelsql.Open("data", nil)
//	if err != nil {
//	        log.Fatal(err)
//	}
//	defer db.Close()
//
//	db.Exec("CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL)")
//	db.Exec("INSERT INTO users VALUES (1, 'ada'), (2, 'grace')")
//
//	res, err := db.Exec("SELECT name FROM users WHERE id = 2")
//	// res.Rows[0][0] is 'grace'
//
// The engine is deliberately a subset of SQL: one table per query, no
// joins, no subqueries, no views. What it does implement, it implements
// properly — three-valued logic, index maintenance, snapshot reads and an
// EXPLAIN that reflects the plan that actually ran.
package keelsql

import (
	"fmt"
	"sync"

	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelstore"
)

// Options configures a database.
type Options struct {
	// Store is passed straight through to keelstore. A nil value uses
	// keelstore's defaults.
	Store *keelstore.Options
}

// A DB is an open keelsql database: a keelstore instance plus the catalog
// read out of it.
//
// A DB is safe for concurrent use. Reads take a keelstore snapshot and
// never block; writers serialise with each other only at commit.
type DB struct {
	store *keelstore.DB
	cat   *catalog.Catalog
	plan  *plan.Planner

	// commitMu serialises the application of a write set against the
	// taking of a snapshot. keelstore has no atomic batch write, so a
	// commit is a sequence of single-key writes; holding this lock while
	// they run, and taking it to read while a snapshot is created, is what
	// stops another transaction from starting inside that sequence and
	// seeing half of it. DDL holds it for writing too.
	commitMu sync.RWMutex
}

// Open opens or creates a database in dir.
func Open(dir string, opts *Options) (*DB, error) {
	var storeOpts *keelstore.Options
	if opts != nil {
		storeOpts = opts.Store
	}
	store, err := keelstore.Open(dir, storeOpts)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Load(storage.Store{DB: store})
	if err != nil {
		store.Close()
		return nil, err
	}
	return &DB{store: store, cat: cat, plan: plan.New(cat)}, nil
}

// Close closes the underlying store.
func (db *DB) Close() error { return db.store.Close() }

// Path is the directory the database lives in.
func (db *DB) Path() string { return db.store.Path() }

// Catalog exposes the schema, which is what the CLI's .tables and .schema
// commands read.
func (db *DB) Catalog() *catalog.Catalog { return db.cat }

// Store exposes the underlying keelstore database. It is here so that a
// program can ask the storage layer for its statistics, or compact it, or
// otherwise use it as the key/value store it still is.
func (db *DB) Store() *keelstore.DB { return db.store }

// Conn returns a session. A session is where an explicit transaction lives:
// BEGIN on one session does not affect another.
func (db *DB) Conn() *Conn { return &Conn{db: db} }

// Exec runs one statement on a throwaway session and returns its result,
// with any rows materialised. It is the convenient entry point; use Conn
// when a transaction has to span several calls.
func (db *DB) Exec(sql string) (*Result, error) { return db.Conn().Exec(sql) }

// ExecScript runs a semicolon-separated script on one session, so that a
// script may contain BEGIN and COMMIT.
func (db *DB) ExecScript(sql string) ([]*Result, error) { return db.Conn().ExecScript(sql) }

// Query runs one statement and streams the rows back. The caller must close
// the returned Rows, which releases the snapshot the query is reading.
func (db *DB) Query(sql string) (*Rows, error) { return db.Conn().Query(sql) }

// ---------------------------------------------------------------------
// transactions
// ---------------------------------------------------------------------

// A Tx is an open transaction: a keelstore snapshot to read from and a
// write buffer to collect changes in.
//
// # Isolation
//
// keelsql provides snapshot isolation *without* write-write conflict
// detection. Every statement in a transaction reads the database as it was
// when the transaction began, plus that transaction's own uncommitted
// writes. Commit then applies the write set unconditionally.
//
// The honest consequence is last-writer-wins: if two transactions read the
// same row and both update it, the second commit silently overwrites the
// first, and neither is told. That is weaker than serializable and weaker
// than the snapshot isolation of a database that aborts on conflict. It is
// what the implementation actually does, so it is what the documentation
// says.
//
// Commit is atomic with respect to other keelsql readers in the same
// process, because a snapshot cannot be taken while a write set is being
// applied. It is *not* crash-atomic: keelstore exposes single-key writes
// and no batch, so a process that dies part-way through a commit leaves
// part of the write set behind.
type Tx struct {
	db   *DB
	snap *keelstore.Snapshot
	buf  *storage.Buffer
	rw   *storage.Overlay
	done bool
}

// begin starts a transaction. The snapshot is taken under the read side of
// commitMu, so it never lands in the middle of another transaction's
// commit.
func (db *DB) begin() *Tx {
	db.commitMu.RLock()
	snap := db.store.Snapshot()
	db.commitMu.RUnlock()

	buf := storage.NewBuffer()
	return &Tx{
		db:   db,
		snap: snap,
		buf:  buf,
		rw:   storage.NewOverlay(storage.Snapshot{Snap: snap}, buf),
	}
}

// Commit applies the write set and releases the snapshot.
func (tx *Tx) Commit() error {
	if tx.done {
		return fmt.Errorf("keelsql: transaction is already finished")
	}
	tx.done = true
	defer tx.snap.Release()

	entries := tx.buf.Entries()
	if len(entries) == 0 {
		return nil
	}

	tx.db.commitMu.Lock()
	defer tx.db.commitMu.Unlock()
	for _, e := range entries {
		var err error
		if e.Deleted {
			err = tx.db.store.Delete(e.Key)
		} else {
			err = tx.db.store.Put(e.Key, e.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Rollback discards the write set. Nothing was written to keelstore, so
// there is nothing to undo: the buffer is simply dropped.
func (tx *Tx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	tx.buf.Reset()
	tx.snap.Release()
	return nil
}

// Writes reports how many keys the transaction has buffered. Tests use it;
// so does the CLI, to show that a rollback really discarded something.
func (tx *Tx) Writes() int { return tx.buf.Len() }
