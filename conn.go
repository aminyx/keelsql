package keelsql

import (
	"errors"
	"fmt"

	"github.com/aminyx/keelsql/ast"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/exec"
	"github.com/aminyx/keelsql/parser"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// ErrNoTransaction is returned by COMMIT or ROLLBACK outside a transaction.
var ErrNoTransaction = errors.New("keelsql: no transaction is open")

// ErrTransactionOpen is returned by BEGIN inside a transaction; keelsql has
// no nested transactions.
var ErrTransactionOpen = errors.New("keelsql: a transaction is already open")

// ErrDDLInTransaction is returned when a schema change is attempted inside
// an explicit transaction. Schema changes bypass the write buffer — they
// have to update the in-memory catalog as well as the store — so keelsql
// refuses to pretend they can be rolled back.
var ErrDDLInTransaction = errors.New("keelsql: DDL is not allowed inside a transaction")

// A Conn is a session: one client's view of the database, and the place an
// explicit transaction lives between BEGIN and COMMIT.
//
// A Conn is not safe for concurrent use. Open one per goroutine; they share
// the DB underneath.
type Conn struct {
	db *DB
	tx *Tx
}

// A Result is what one statement produced.
type Result struct {
	// Tag names the statement: SELECT, INSERT, CREATE TABLE, EXPLAIN…
	Tag string
	// Columns and Rows are set for SELECT and EXPLAIN.
	Columns []string
	Rows    [][]types.Value
	// Affected is the number of rows an INSERT, UPDATE or DELETE changed.
	Affected int64
	// Plan holds the rendered plan tree of an EXPLAIN.
	Plan string
}

// InTx reports whether an explicit transaction is open on this session.
func (c *Conn) InTx() bool { return c.tx != nil }

// Tx returns the open transaction, or nil.
func (c *Conn) Tx() *Tx { return c.tx }

// Exec parses and runs exactly one statement, materialising any rows.
func (c *Conn) Exec(sql string) (*Result, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return c.run(stmt)
}

// ExecScript runs a semicolon-separated script and returns one result per
// statement. It stops at the first error.
func (c *Conn) ExecScript(sql string) ([]*Result, error) {
	stmts, err := parser.ParseMany(sql)
	if err != nil {
		return nil, err
	}
	out := make([]*Result, 0, len(stmts))
	for _, stmt := range stmts {
		res, err := c.run(stmt)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// RunStatement executes a statement that has already been parsed. The REPL
// uses it so that it can inspect a statement — to print its plan first when
// .explain is on — without parsing the text twice.
func (c *Conn) RunStatement(stmt ast.Statement) (*Result, error) { return c.run(stmt) }

// Query runs one statement and streams its rows. The caller must close the
// returned Rows: until then it holds the snapshot it is reading.
func (c *Conn) Query(sql string) (*Rows, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	sel, ok := stmt.(*ast.Select)
	if !ok {
		return nil, fmt.Errorf("keelsql: Query wants a SELECT, got %T", stmt)
	}
	return c.query(sel)
}

// ---------------------------------------------------------------------
// statement dispatch
// ---------------------------------------------------------------------

func (c *Conn) run(stmt ast.Statement) (*Result, error) {
	switch s := stmt.(type) {
	case *ast.Begin:
		if c.tx != nil {
			return nil, ErrTransactionOpen
		}
		c.tx = c.db.begin()
		return &Result{Tag: "BEGIN"}, nil

	case *ast.Commit:
		if c.tx == nil {
			return nil, ErrNoTransaction
		}
		tx := c.tx
		c.tx = nil
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &Result{Tag: "COMMIT"}, nil

	case *ast.Rollback:
		if c.tx == nil {
			return nil, ErrNoTransaction
		}
		tx := c.tx
		c.tx = nil
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		return &Result{Tag: "ROLLBACK"}, nil

	case *ast.Explain:
		return c.explain(s)

	case *ast.Select:
		return c.selectRows(s)

	case *ast.CreateTable, *ast.DropTable, *ast.CreateIndex, *ast.DropIndex:
		return c.ddl(stmt)
	}
	return c.dml(stmt)
}

func (c *Conn) explain(s *ast.Explain) (*Result, error) {
	p, err := c.db.plan.Plan(s.Stmt)
	if err != nil {
		return nil, err
	}
	tree := plan.Explain(p)
	res := &Result{Tag: "EXPLAIN", Plan: tree, Columns: []string{"plan"}}
	for _, line := range splitLines(tree) {
		res.Rows = append(res.Rows, []types.Value{types.Text(line)})
	}
	return res, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// selectRows runs a query and materialises it.
func (c *Conn) selectRows(s *ast.Select) (*Result, error) {
	rows, err := c.query(s)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &Result{Tag: "SELECT", Columns: rows.Columns()}
	for rows.Next() {
		res.Rows = append(res.Rows, append([]types.Value(nil), rows.Row()...))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Affected = int64(len(res.Rows))
	return res, nil
}

// query builds the streaming cursor. Outside an explicit transaction it
// opens an implicit one, which the cursor owns and finishes on Close — that
// is what keeps a long scan reading a stable snapshot.
func (c *Conn) query(s *ast.Select) (*Rows, error) {
	p, err := c.db.plan.Plan(s)
	if err != nil {
		return nil, err
	}

	tx, owned := c.tx, false
	if tx == nil {
		tx, owned = c.db.begin(), true
	}
	op, err := exec.Build(p, tx.rw)
	if err != nil {
		if owned {
			tx.Rollback()
		}
		return nil, err
	}
	return &Rows{cols: exec.Columns(p), op: op, tx: tx, owned: owned}, nil
}

// dml runs an INSERT, UPDATE or DELETE. Outside an explicit transaction it
// wraps the statement in one, so a statement that fails half way through
// leaves nothing behind.
func (c *Conn) dml(stmt ast.Statement) (*Result, error) {
	p, err := c.db.plan.Plan(stmt)
	if err != nil {
		return nil, err
	}

	tx, owned := c.tx, false
	if tx == nil {
		tx, owned = c.db.begin(), true
	}
	affected, err := exec.RunDML(p, tx.rw)
	if err != nil {
		if owned {
			tx.Rollback()
		} else {
			// Inside an explicit transaction the statement's partial
			// writes stay in the buffer; the client decides whether to
			// ROLLBACK. Report the error and leave the choice to them.
			_ = affected
		}
		return nil, err
	}
	if owned {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: dmlTag(stmt), Affected: affected}, nil
}

func dmlTag(stmt ast.Statement) string {
	switch stmt.(type) {
	case *ast.Insert:
		return "INSERT"
	case *ast.Update:
		return "UPDATE"
	case *ast.Delete:
		return "DELETE"
	}
	return "OK"
}

// ---------------------------------------------------------------------
// DDL
// ---------------------------------------------------------------------

// ddl runs a schema change. It holds the write side of commitMu for the
// whole statement and writes straight through to keelstore, so that the
// in-memory catalog and the stored catalog change together and no snapshot
// can be taken in between.
func (c *Conn) ddl(stmt ast.Statement) (*Result, error) {
	if c.tx != nil {
		return nil, ErrDDLInTransaction
	}
	p, err := c.db.plan.Plan(stmt)
	if err != nil {
		if isExistenceSkip(stmt, err) {
			return &Result{Tag: ddlTag(stmt)}, nil
		}
		return nil, err
	}

	db := c.db
	db.commitMu.Lock()
	defer db.commitMu.Unlock()
	rw := storage.Store{DB: db.store}

	switch n := p.(type) {
	case *plan.CreateTable:
		if db.cat.Has(n.Def.Name) {
			if n.IfNotExists {
				return &Result{Tag: "CREATE TABLE"}, nil
			}
			return nil, fmt.Errorf("%w: %s", catalog.ErrTableExists, n.Def.Name)
		}
		if _, err := db.cat.Create(rw, n.Def); err != nil {
			return nil, err
		}
		return &Result{Tag: "CREATE TABLE"}, nil

	case *plan.DropTable:
		t, err := db.cat.Get(n.Name)
		if err != nil {
			if n.IfExists && errors.Is(err, catalog.ErrTableNotFound) {
				return &Result{Tag: "DROP TABLE"}, nil
			}
			return nil, err
		}
		if err := exec.DropTableData(rw, t); err != nil {
			return nil, err
		}
		if err := db.cat.Drop(rw, n.Name); err != nil {
			return nil, err
		}
		return &Result{Tag: "DROP TABLE"}, nil

	case *plan.CreateIndex:
		if _, _, err := db.cat.FindIndex(n.Name); err == nil {
			if n.IfNotExists {
				return &Result{Tag: "CREATE INDEX"}, nil
			}
			return nil, fmt.Errorf("%w: %s", catalog.ErrIndexExists, n.Name)
		}
		column := n.Table.Columns[n.Column].Name
		table, idx, err := db.cat.AddIndex(rw, n.Table.Name, n.Name, column)
		if err != nil {
			return nil, err
		}
		if err := exec.BuildIndex(rw, table, idx); err != nil {
			return nil, err
		}
		return &Result{Tag: "CREATE INDEX"}, nil

	case *plan.DropIndex:
		table, idx, err := db.cat.FindIndex(n.Name)
		if err != nil {
			if n.IfExists && errors.Is(err, catalog.ErrIndexNotFound) {
				return &Result{Tag: "DROP INDEX"}, nil
			}
			return nil, err
		}
		if err := exec.DropIndexEntries(rw, table, idx); err != nil {
			return nil, err
		}
		if _, _, err := db.cat.RemoveIndex(rw, n.Name); err != nil {
			return nil, err
		}
		return &Result{Tag: "DROP INDEX"}, nil
	}
	return nil, fmt.Errorf("keelsql: %T is not a schema statement", p)
}

// isExistenceSkip lets `DROP TABLE IF EXISTS t` and
// `CREATE INDEX IF NOT EXISTS i ON missing (c)` succeed quietly when
// planning already failed because the object is not there.
func isExistenceSkip(stmt ast.Statement, err error) bool {
	if !errors.Is(err, catalog.ErrTableNotFound) && !errors.Is(err, catalog.ErrIndexNotFound) {
		return false
	}
	switch s := stmt.(type) {
	case *ast.DropTable:
		return s.IfExists
	case *ast.DropIndex:
		return s.IfExists
	case *ast.CreateIndex:
		return s.IfNotExists
	}
	return false
}

func ddlTag(stmt ast.Statement) string {
	switch stmt.(type) {
	case *ast.CreateTable:
		return "CREATE TABLE"
	case *ast.DropTable:
		return "DROP TABLE"
	case *ast.CreateIndex:
		return "CREATE INDEX"
	case *ast.DropIndex:
		return "DROP INDEX"
	}
	return "OK"
}

// ---------------------------------------------------------------------
// streaming rows
// ---------------------------------------------------------------------

// Rows is a streaming query result, in the style of database/sql:
//
//	for rows.Next() { use(rows.Row()) }
//	if err := rows.Err(); err != nil { … }
//	rows.Close()
//
// Nothing is materialised: each call to Next pulls one row through the
// operator tree.
type Rows struct {
	cols   []string
	op     exec.Operator
	tx     *Tx
	owned  bool
	row    []types.Value
	err    error
	closed bool
}

// Columns returns the result's column names.
func (r *Rows) Columns() []string { return r.cols }

// Next advances to the next row and reports whether one was available.
func (r *Rows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	row, ok, err := r.op.Next()
	if err != nil {
		r.err = err
		return false
	}
	if !ok {
		return false
	}
	r.row = row
	return true
}

// Row returns the current row. It is only valid until the next call to
// Next.
func (r *Rows) Row() []types.Value { return r.row }

// Err returns the error that stopped the iteration, if any.
func (r *Rows) Err() error { return r.err }

// Close releases the operator tree and, if the query opened its own
// transaction, that transaction's snapshot.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.op.Close()
	if r.owned {
		if rollbackErr := r.tx.Rollback(); err == nil {
			err = rollbackErr
		}
	}
	return err
}
