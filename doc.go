/*
Package keelsql implements a SQL query engine over the keelstore key/value
store. This file exists to document the pieces the engine is assembled
from; the API itself is documented in keelsql.go.

# Layout

	lexer     SQL text  -> tokens, by hand, with positions
	ast       the syntax tree, and how it prints itself back as SQL
	parser    tokens    -> AST, recursive descent + precedence climbing
	types     values, the total order over them, three-valued logic
	keycodec  values    -> bytes, order-preserving for keys
	catalog   schemas, stored in keelstore under a reserved prefix
	storage   the seam to keelstore: readers, writers, the write buffer
	plan      AST       -> logical plan -> rewrite rules -> physical plan
	exec      physical plan -> volcano operators
	cmd/keelsql  the REPL

# The two-repo split

keelstore owns durability and ordering: a write-ahead log, a memtable, sorted
files on disk, compaction, snapshots. keelsql owns everything relational:
what a row is, what a table is, how a predicate becomes a byte range, how a
transaction's writes are held back until commit.

The interface between them is small on purpose — Put, Get, Delete, Snapshot
and a bounded iterator. Everything else in this repository is built on those
five operations.

# What is not here

No joins, no subqueries, no views, no window functions, no ALTER TABLE, no
multi-column indexes, no HAVING, no cross-process locking. The README lists
the limitations in full, including the ones that are subtle rather than
missing: the sort is in memory, and the isolation level is snapshot
isolation without write-write conflict detection.
*/
package keelsql
