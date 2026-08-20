package keelsql_test

import (
	"fmt"
	"testing"

	"github.com/aminyx/keelsql"
	"github.com/aminyx/keelsql/parser"
)

// benchDB returns a database holding n rows of
// `CREATE TABLE bench (id INT PRIMARY KEY, k INT, s TEXT NOT NULL)`,
// with an index on k when indexed is set.
func benchDB(b *testing.B, n int, indexed bool) *keelsql.DB {
	b.Helper()
	db, err := keelsql.Open(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })

	if _, err := db.Exec("CREATE TABLE bench (id INT PRIMARY KEY, k INT, s TEXT NOT NULL)"); err != nil {
		b.Fatal(err)
	}
	conn := db.Conn()
	if _, err := conn.Exec("BEGIN"); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		sql := fmt.Sprintf("INSERT INTO bench VALUES (%d, %d, 'row %d')", i, i%100, i)
		if _, err := conn.Exec(sql); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := conn.Exec("COMMIT"); err != nil {
		b.Fatal(err)
	}
	if indexed {
		if _, err := db.Exec("CREATE INDEX idx_k ON bench (k)"); err != nil {
			b.Fatal(err)
		}
	}
	return db
}

func BenchmarkInsert(b *testing.B) {
	db := benchDB(b, 0, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec(fmt.Sprintf("INSERT INTO bench VALUES (%d, 1, 'x')", i)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPointLookup is the one the planner exists for: a predicate on
// the primary key becomes a bounded scan of a handful of keys, not a walk
// over the table.
func BenchmarkPointLookup(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := db.Exec(fmt.Sprintf("SELECT s FROM bench WHERE id = %d", i%5000))
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Rows) != 1 {
			b.Fatalf("got %d rows", len(res.Rows))
		}
	}
}

func BenchmarkIndexLookup(b *testing.B) {
	db := benchDB(b, 5000, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec(fmt.Sprintf("SELECT id FROM bench WHERE k = %d", i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFullScanLookup is the same query without an index: the number to
// compare BenchmarkIndexLookup against.
func BenchmarkFullScanLookup(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec(fmt.Sprintf("SELECT id FROM bench WHERE k = %d", i%100)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRangeScan(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo := i % 4000
		if _, err := db.Exec(fmt.Sprintf("SELECT id, s FROM bench WHERE id BETWEEN %d AND %d", lo, lo+100)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFullScan(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec("SELECT COUNT(*) FROM bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGroupBy(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec("SELECT k, COUNT(*), SUM(id) FROM bench GROUP BY k"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTopNSort(b *testing.B) {
	db := benchDB(b, 5000, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Exec("SELECT id FROM bench ORDER BY k DESC LIMIT 10"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParse(b *testing.B) {
	const sql = "SELECT a, b FROM t WHERE a > 1 AND b LIKE 'x%' ORDER BY b DESC LIMIT 10"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parser.Parse(sql); err != nil {
			b.Fatal(err)
		}
	}
}
