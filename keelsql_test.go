package keelsql_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aminyx/keelsql"
	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/exec"
	"github.com/aminyx/keelsql/types"
)

func open(t *testing.T) *keelsql.DB {
	t.Helper()
	db, err := keelsql.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *keelsql.DB, sql string) *keelsql.Result {
	t.Helper()
	res, err := db.Exec(sql)
	if err != nil {
		t.Fatalf("Exec(%q): %v", sql, err)
	}
	return res
}

func mustScript(t *testing.T, db *keelsql.DB, sql string) {
	t.Helper()
	if _, err := db.ExecScript(sql); err != nil {
		t.Fatalf("ExecScript: %v", err)
	}
}

// render turns a result into "a,b|c,d".
func render(res *keelsql.Result) string {
	rows := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		cells := make([]string, len(row))
		for j, v := range row {
			cells[j] = v.String()
		}
		rows[i] = strings.Join(cells, ",")
	}
	return strings.Join(rows, "|")
}

func query(t *testing.T, db *keelsql.DB, sql string) string {
	t.Helper()
	return render(mustExec(t, db, sql))
}

func seed(t *testing.T, db *keelsql.DB) {
	t.Helper()
	mustScript(t, db, `
		CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT);
		INSERT INTO users VALUES (1, 'ada', 99.5), (2, 'grace', 97.0), (3, 'alan', NULL);
	`)
}

// ---------------------------------------------------------------------
// basics
// ---------------------------------------------------------------------

func TestExecTagsAndCounts(t *testing.T) {
	db := open(t)
	cases := []struct {
		sql      string
		tag      string
		affected int64
	}{
		{"CREATE TABLE t (a INT PRIMARY KEY, b TEXT)", "CREATE TABLE", 0},
		{"INSERT INTO t VALUES (1, 'x'), (2, 'y')", "INSERT", 2},
		{"UPDATE t SET b = 'z' WHERE a = 1", "UPDATE", 1},
		{"DELETE FROM t WHERE a = 2", "DELETE", 1},
		{"CREATE INDEX i ON t (b)", "CREATE INDEX", 0},
		{"DROP INDEX i", "DROP INDEX", 0},
		{"DROP TABLE t", "DROP TABLE", 0},
	}
	for _, c := range cases {
		res := mustExec(t, db, c.sql)
		if res.Tag != c.tag {
			t.Errorf("%s: tag = %q, want %q", c.sql, res.Tag, c.tag)
		}
		if res.Affected != c.affected {
			t.Errorf("%s: affected = %d, want %d", c.sql, res.Affected, c.affected)
		}
	}
}

func TestSelectResultCarriesColumnNames(t *testing.T) {
	db := open(t)
	seed(t, db)

	res := mustExec(t, db, "SELECT id, name AS who, score * 2.0 FROM users WHERE id = 1")
	want := []string{"id", "who", "score * 2.0"}
	if strings.Join(res.Columns, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", res.Columns, want)
	}
	if render(res) != "1,ada,199.0" {
		t.Errorf("row = %q", render(res))
	}
	if res.Affected != 1 {
		t.Errorf("a SELECT should report its row count, got %d", res.Affected)
	}
}

func TestSelectStar(t *testing.T) {
	db := open(t)
	seed(t, db)
	res := mustExec(t, db, "SELECT * FROM users ORDER BY id")
	if strings.Join(res.Columns, ",") != "id,name,score" {
		t.Errorf("columns = %v", res.Columns)
	}
	if render(res) != "1,ada,99.5|2,grace,97.0|3,alan,NULL" {
		t.Errorf("rows = %q", render(res))
	}
}

// TestReopenKeepsSchemaAndRows is the whole reason the catalog lives in the
// store: closing and reopening has to find everything again.
func TestReopenKeepsSchemaAndRows(t *testing.T) {
	dir := t.TempDir()

	db, err := keelsql.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecScript(`
		CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL);
		INSERT INTO users VALUES (1, 'ada'), (2, 'grace');
		CREATE INDEX idx_name ON users (name);
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := keelsql.Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	if got := query(t, again, "SELECT id, name FROM users"); got != "1,ada|2,grace" {
		t.Errorf("rows after reopen = %q", got)
	}
	table, err := again.Catalog().Get("users")
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Indexes) != 1 {
		t.Errorf("the index was lost: %v", table.Indexes)
	}
	// A new table must not land on the old one's key range.
	mustScript(t, again, "CREATE TABLE posts (id INT PRIMARY KEY, body TEXT); INSERT INTO posts VALUES (1, 'hello')")
	if got := query(t, again, "SELECT id, name FROM users"); got != "1,ada|2,grace" {
		t.Errorf("the new table trampled the old one: %q", got)
	}
}

func TestQueryStreamsRows(t *testing.T) {
	db := open(t)
	seed(t, db)

	rows, err := db.Query("SELECT id FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	if strings.Join(rows.Columns(), ",") != "id" {
		t.Errorf("columns = %v", rows.Columns())
	}
	var ids []string
	for rows.Next() {
		ids = append(ids, rows.Row()[0].String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "1,2,3" {
		t.Errorf("streamed %v", ids)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("a closed cursor should not produce rows")
	}

	if _, err := db.Query("INSERT INTO users VALUES (9, 'x', 1.0)"); err == nil {
		t.Error("Query should refuse a non-SELECT")
	}
}

func TestExplainReturnsThePlanTree(t *testing.T) {
	db := open(t)
	seed(t, db)

	res := mustExec(t, db, "EXPLAIN SELECT name FROM users WHERE id = 2")
	if res.Tag != "EXPLAIN" {
		t.Errorf("tag = %q", res.Tag)
	}
	if !strings.Contains(res.Plan, "RangeScan table=users pk=id range=[2, 2]") {
		t.Errorf("plan:\n%s", res.Plan)
	}
	if len(res.Rows) != strings.Count(res.Plan, "\n")+1 {
		t.Errorf("the plan should come back one line per row, got %d rows", len(res.Rows))
	}
}

// TestExplainProvesTheIndexIsUsed is the before-and-after the README shows.
func TestExplainProvesTheIndexIsUsed(t *testing.T) {
	db := open(t)
	seed(t, db)

	before := mustExec(t, db, "EXPLAIN SELECT id FROM users WHERE name = 'grace'")
	if !strings.Contains(before.Plan, "SeqScan") {
		t.Errorf("without an index the query should scan:\n%s", before.Plan)
	}

	mustExec(t, db, "CREATE INDEX idx_name ON users (name)")

	after := mustExec(t, db, "EXPLAIN SELECT id FROM users WHERE name = 'grace'")
	if !strings.Contains(after.Plan, "IndexScan table=users index=idx_name") {
		t.Errorf("with an index the query should use it:\n%s", after.Plan)
	}
	if strings.Contains(after.Plan, "Filter") {
		t.Errorf("the predicate should be absorbed by the index scan:\n%s", after.Plan)
	}
	if got := query(t, db, "SELECT id FROM users WHERE name = 'grace'"); got != "2" {
		t.Errorf("the query itself returned %q", got)
	}
}

// ---------------------------------------------------------------------
// three-valued logic, end to end
// ---------------------------------------------------------------------

func TestNullSemanticsEndToEnd(t *testing.T) {
	db := open(t)
	seed(t, db)

	cases := []struct{ sql, want string }{
		{"SELECT id FROM users WHERE score IS NULL", "3"},
		{"SELECT id FROM users WHERE score IS NOT NULL", "1|2"},
		{"SELECT id FROM users WHERE score = NULL", ""},
		{"SELECT id FROM users WHERE NULL = NULL", ""},
		{"SELECT id FROM users WHERE score > 0.0", "1|2"},
		{"SELECT id FROM users WHERE NOT (score > 0.0)", ""},
		{"SELECT id FROM users WHERE score > 0.0 OR id = 3", "1|2|3"},
		{"SELECT NULL = NULL FROM users WHERE id = 1", "NULL"},
		{"SELECT score IS NULL FROM users WHERE id = 3", "true"},
	}
	for _, c := range cases {
		if got := query(t, db, c.sql); got != c.want {
			t.Errorf("%s = %q, want %q", c.sql, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------
// transactions
// ---------------------------------------------------------------------

func TestRollbackDiscardsEverything(t *testing.T) {
	db := open(t)
	seed(t, db)

	conn := db.Conn()
	if _, err := conn.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("INSERT INTO users VALUES (4, 'linus', 1.0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("DELETE FROM users WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if !conn.InTx() || conn.Tx().Writes() == 0 {
		t.Fatal("the transaction should be holding buffered writes")
	}
	// Inside the transaction, the changes are visible.
	res, err := conn.Exec("SELECT id FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if render(res) != "2|3|4" {
		t.Errorf("inside the transaction: %q", render(res))
	}
	// Outside it, nothing has happened yet.
	if got := query(t, db, "SELECT id FROM users"); got != "1|2|3" {
		t.Errorf("outside the transaction: %q", got)
	}

	if _, err := conn.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if conn.InTx() {
		t.Error("ROLLBACK should close the transaction")
	}
	if got := query(t, db, "SELECT id FROM users"); got != "1|2|3" {
		t.Errorf("after rollback: %q", got)
	}
}

func TestCommitAppliesEverything(t *testing.T) {
	db := open(t)
	seed(t, db)

	conn := db.Conn()
	for _, sql := range []string{
		"BEGIN",
		"INSERT INTO users VALUES (4, 'linus', 1.0)",
		"UPDATE users SET name = 'ada l.' WHERE id = 1",
		"DELETE FROM users WHERE id = 3",
		"COMMIT",
	} {
		if _, err := conn.Exec(sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if got := query(t, db, "SELECT id, name FROM users"); got != "1,ada l.|2,grace|4,linus" {
		t.Errorf("after commit: %q", got)
	}
}

// TestSnapshotReadIsStable is the property snapshot isolation is named for:
// a transaction sees the database as it was when it began, however busy the
// rest of the process is.
func TestSnapshotReadIsStable(t *testing.T) {
	db := open(t)
	seed(t, db)

	reader := db.Conn()
	if _, err := reader.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	res, err := reader.Exec("SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if render(res) != "3" {
		t.Fatalf("first read: %q", render(res))
	}

	// Another session commits, twice.
	writer := db.Conn()
	if _, err := writer.Exec("INSERT INTO users VALUES (4, 'linus', 1.0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("DELETE FROM users WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	res, err = reader.Exec("SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if render(res) != "3" {
		t.Errorf("the snapshot moved: %q, want 3", render(res))
	}
	res, err = reader.Exec("SELECT id FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if render(res) != "1|2|3" {
		t.Errorf("the snapshot moved: %q", render(res))
	}

	if _, err := reader.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	if got := query(t, db, "SELECT id FROM users"); got != "2|3|4" {
		t.Errorf("after the reader finished: %q", got)
	}
}

// TestLastWriterWins documents the isolation level honestly: keelsql does
// not detect write-write conflicts, so the second commit silently wins.
func TestLastWriterWins(t *testing.T) {
	db := open(t)
	seed(t, db)

	a, b := db.Conn(), db.Conn()
	if _, err := a.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec("UPDATE users SET name = 'from a' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec("UPDATE users SET name = 'from b' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	// No conflict is reported: this is snapshot isolation without
	// write-write conflict detection.
	if _, err := b.Exec("COMMIT"); err != nil {
		t.Fatalf("the second commit should succeed under last-writer-wins: %v", err)
	}
	if got := query(t, db, "SELECT name FROM users WHERE id = 1"); got != "from b" {
		t.Errorf("the last writer should have won, got %q", got)
	}
}

func TestTransactionControlErrors(t *testing.T) {
	db := open(t)
	seed(t, db)
	conn := db.Conn()

	if _, err := conn.Exec("COMMIT"); !errors.Is(err, keelsql.ErrNoTransaction) {
		t.Errorf("COMMIT with no transaction gave %v", err)
	}
	if _, err := conn.Exec("ROLLBACK"); !errors.Is(err, keelsql.ErrNoTransaction) {
		t.Errorf("ROLLBACK with no transaction gave %v", err)
	}
	if _, err := conn.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec("BEGIN"); !errors.Is(err, keelsql.ErrTransactionOpen) {
		t.Errorf("a nested BEGIN gave %v", err)
	}
	if _, err := conn.Exec("CREATE TABLE inside (a INT PRIMARY KEY)"); !errors.Is(err, keelsql.ErrDDLInTransaction) {
		t.Errorf("DDL inside a transaction gave %v", err)
	}
	if _, err := conn.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	// After the rollback the same DDL works.
	if _, err := conn.Exec("CREATE TABLE inside (a INT PRIMARY KEY)"); err != nil {
		t.Errorf("DDL outside a transaction: %v", err)
	}
}

func TestSessionsHaveIndependentTransactions(t *testing.T) {
	db := open(t)
	seed(t, db)

	a, b := db.Conn(), db.Conn()
	if _, err := a.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	if b.InTx() {
		t.Error("BEGIN on one session should not affect another")
	}
	if _, err := b.Exec("INSERT INTO users VALUES (4, 'linus', 1.0)"); err != nil {
		t.Fatalf("the other session should still be in autocommit: %v", err)
	}
	if _, err := a.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if got := query(t, db, "SELECT id FROM users"); got != "1|2|3|4" {
		t.Errorf("got %q", got)
	}
}

// TestFailedStatementLeavesNothingBehind: an autocommit INSERT that fails
// half way through must not commit the rows it managed to write.
func TestFailedStatementLeavesNothingBehind(t *testing.T) {
	db := open(t)
	seed(t, db)

	_, err := db.Exec("INSERT INTO users VALUES (4, 'linus', 1.0), (1, 'clash', 2.0)")
	if !errors.Is(err, exec.ErrDuplicateKey) {
		t.Fatalf("error = %v, want a duplicate key", err)
	}
	if got := query(t, db, "SELECT id FROM users"); got != "1|2|3" {
		t.Errorf("the first row of the failed INSERT was committed: %q", got)
	}
}

// ---------------------------------------------------------------------
// DDL
// ---------------------------------------------------------------------

func TestDropTableRemovesItsData(t *testing.T) {
	db := open(t)
	seed(t, db)
	mustExec(t, db, "CREATE INDEX idx_name ON users (name)")
	mustExec(t, db, "DROP TABLE users")

	if _, err := db.Exec("SELECT * FROM users"); !errors.Is(err, catalog.ErrTableNotFound) {
		t.Errorf("querying a dropped table gave %v", err)
	}
	// Recreating it must not find the old rows.
	mustExec(t, db, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, score FLOAT)")
	if got := query(t, db, "SELECT id FROM users"); got != "" {
		t.Errorf("the recreated table still holds %q", got)
	}
}

func TestIfExistsAndIfNotExists(t *testing.T) {
	db := open(t)
	mustExec(t, db, "DROP TABLE IF EXISTS nothing")
	mustExec(t, db, "DROP INDEX IF EXISTS nothing")
	mustExec(t, db, "CREATE TABLE t (a INT PRIMARY KEY)")
	mustExec(t, db, "CREATE TABLE IF NOT EXISTS t (a INT PRIMARY KEY)")
	mustExec(t, db, "CREATE INDEX i ON t (a)")
	mustExec(t, db, "CREATE INDEX IF NOT EXISTS i ON t (a)")

	if _, err := db.Exec("CREATE TABLE t (a INT PRIMARY KEY)"); !errors.Is(err, catalog.ErrTableExists) {
		t.Errorf("recreating a table gave %v", err)
	}
	if _, err := db.Exec("CREATE INDEX i ON t (a)"); !errors.Is(err, catalog.ErrIndexExists) {
		t.Errorf("recreating an index gave %v", err)
	}
	if _, err := db.Exec("DROP TABLE nothing"); !errors.Is(err, catalog.ErrTableNotFound) {
		t.Errorf("dropping a missing table gave %v", err)
	}
	if _, err := db.Exec("DROP INDEX nothing"); !errors.Is(err, catalog.ErrIndexNotFound) {
		t.Errorf("dropping a missing index gave %v", err)
	}
}

func TestCatalogIsVisibleToTheCLICommands(t *testing.T) {
	db := open(t)
	seed(t, db)
	mustExec(t, db, "CREATE TABLE posts (id INT PRIMARY KEY, body TEXT)")

	if got := strings.Join(db.Catalog().Names(), ","); got != "posts,users" {
		t.Errorf(".tables would show %q", got)
	}
	table, err := db.Catalog().Get("users")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.SQL(), "id INT PRIMARY KEY") {
		t.Errorf(".schema would show:\n%s", table.SQL())
	}
	if db.Path() == "" {
		t.Error("Path should name the directory")
	}
	if db.Store() == nil {
		t.Error("Store should expose the underlying keelstore")
	}
}

// ---------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------

func TestErrorPaths(t *testing.T) {
	db := open(t)
	seed(t, db)

	cases := []struct{ sql, want string }{
		{"SELECT * FROM missing", "no such table: missing"},
		{"SELECT nosuch FROM users", "no such column: users.nosuch"},
		{"INSERT INTO users VALUES (1, 'clash', 1.0)", "duplicate primary key"},
		{"INSERT INTO users VALUES (9, NULL, 1.0)", "NOT NULL constraint violated"},
		{"INSERT INTO users VALUES ('x', 'y', 1.0)", "cannot store TEXT in a INT column"},
		{"INSERT INTO users VALUES (9, 'y')", "row 1 has 2 values"},
		{"UPDATE users SET score = 'x' WHERE id = 1", "cannot store TEXT in a FLOAT column"},
		{"SELECT name + 1 FROM users", "type mismatch"},
		{"SELECT id / 0 FROM users", "division by zero"},
		{"SELECT id FROM users WHERE name > 1", "cannot compare TEXT with INT"},
		{"SELECT id FROM users WHERE id", "not a condition"},
		{"SELCT 1", "syntax error"},
	}
	for _, c := range cases {
		_, err := db.Exec(c.sql)
		if err == nil {
			t.Errorf("Exec(%q) succeeded, want an error", c.sql)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Exec(%q)\n got %q\nwant it to contain %q", c.sql, err, c.want)
		}
	}
	// None of the failures should have changed anything.
	if got := query(t, db, "SELECT id, name FROM users"); got != "1,ada|2,grace|3,alan" {
		t.Errorf("the table changed: %q", got)
	}
}

func TestExecScriptStopsAtTheFirstError(t *testing.T) {
	db := open(t)
	seed(t, db)
	results, err := db.ExecScript(`
		INSERT INTO users VALUES (4, 'linus', 1.0);
		INSERT INTO users VALUES (4, 'again', 2.0);
		INSERT INTO users VALUES (5, 'never', 3.0);
	`)
	if err == nil {
		t.Fatal("expected the duplicate key to fail the script")
	}
	if len(results) != 1 {
		t.Errorf("got %d results before the error, want 1", len(results))
	}
	if got := query(t, db, "SELECT id FROM users"); got != "1|2|3|4" {
		t.Errorf("the statements after the error should not have run: %q", got)
	}
}

// ---------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------

func TestFormatTable(t *testing.T) {
	got := keelsql.FormatTable(
		[]string{"id", "name"},
		[][]types.Value{
			{types.Int(1), types.Text("ada")},
			{types.Int(2), types.Text("grace")},
			{types.Int(3), types.Null()},
		},
	)
	want := strings.Join([]string{
		"+----+-------+",
		"| id | name  |",
		"+----+-------+",
		"| 1  | ada   |",
		"| 2  | grace |",
		"| 3  | NULL  |",
		"+----+-------+",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatTableHandlesEdgeCases(t *testing.T) {
	if got := keelsql.FormatTable(nil, nil); got != "" {
		t.Errorf("no columns should format as nothing, got %q", got)
	}
	// A header wider than every value still lines up.
	got := keelsql.FormatTable([]string{"a long header"}, [][]types.Value{{types.Int(1)}})
	if !strings.Contains(got, "| 1             |") {
		t.Errorf("got\n%s", got)
	}
	// Runes, not bytes, decide the width.
	got = keelsql.FormatTable([]string{"x"}, [][]types.Value{{types.Text("héé")}})
	if !strings.Contains(got, "| héé |") {
		t.Errorf("got\n%s", got)
	}
}

// ---------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------

// TestConcurrentReadersAndWriters is what `make race` is pointed at. Each
// goroutine opens its own session, because a Conn holds the transaction and
// is not shared; the DB underneath is.
func TestConcurrentReadersAndWriters(t *testing.T) {
	db := open(t)
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, s TEXT NOT NULL)")
	mustExec(t, db, "CREATE INDEX idx_k ON t (k)")
	for i := 0; i < 50; i++ {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, 'seed')", i, i%5))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Writers, each on its own key range so they never collide.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			conn := db.Conn()
			for i := 0; i < 40; i++ {
				id := 1000 + w*100 + i
				if _, err := conn.Exec(fmt.Sprintf("INSERT INTO t VALUES (%d, %d, 'w')", id, i%5)); err != nil {
					errs <- err
					return
				}
				if _, err := conn.Exec(fmt.Sprintf("UPDATE t SET k = %d WHERE id = %d", i%7, id)); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}

	// Readers, in explicit transactions so they hold a snapshot open while
	// the writers commit underneath them.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := db.Conn()
			for i := 0; i < 40; i++ {
				if _, err := conn.Exec("BEGIN"); err != nil {
					errs <- err
					return
				}
				first, err := conn.Exec("SELECT COUNT(*) FROM t")
				if err != nil {
					errs <- err
					return
				}
				second, err := conn.Exec("SELECT COUNT(*) FROM t WHERE k >= 0")
				if err != nil {
					errs <- err
					return
				}
				if render(first) != render(second) {
					errs <- fmt.Errorf("the snapshot moved inside a transaction: %s then %s",
						render(first), render(second))
					return
				}
				if _, err := conn.Exec("COMMIT"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := query(t, db, "SELECT COUNT(*) FROM t"); got != "210" {
		t.Errorf("final row count = %s, want 210", got)
	}
}
