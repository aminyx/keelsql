# Changelog

All notable changes to keelsql are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-08-21

First release: a working SQL query engine over
[keelstore](https://github.com/aminyx/keelstore).

### Added

- **Lexer.** Hand-written scanner with byte, line and rune-column positions
  for every token. Case-insensitive keywords, `"quoted"` identifiers,
  string literals with both SQL (`''`) and C (`\n`, `\'`) escapes, integer
  and floating-point literals with exponents, `--` and `/* */` comments.
- **Parser.** Recursive descent with one token of lookahead and precedence
  climbing for expressions — no yacc, no parser generator. Supports
  `CREATE TABLE`, `DROP TABLE`, `CREATE INDEX`, `DROP INDEX`, `INSERT`,
  `SELECT`, `UPDATE`, `DELETE`, `EXPLAIN`, `BEGIN`, `COMMIT` and
  `ROLLBACK`. Errors name what was expected and where: `expected FROM,
  found keyword WHERE` at line 1, column 10.
- **Order-preserving key encoding.** Values encode to bytes whose
  lexicographic order matches their SQL order: sign-bit flips for integers,
  bit inversion for negative floats, and `0x00 0xFF` escaping with a
  `0x00 0x01` terminator for strings. Documented byte by byte in the README
  and pinned by a randomised property test.
- **Row encoding.** A format byte plus self-delimiting field encodings,
  with a masked decoder that skips columns the query does not read.
- **Catalog.** Table and index definitions stored as JSON inside keelstore
  under a reserved key prefix, reloaded on open, so a schema survives a
  restart. Identifiers are handed out by a persisted counter.
- **Secondary indexes.** `CREATE INDEX idx ON t (col)` builds entries from
  the rows already present and every subsequent insert, update and delete
  maintains them through the same write path as the row itself.
- **Planner.** Logical plan followed by rewrite rules: predicate pushdown
  into a bounded key range, index selection under a cost-lite model,
  projection pruning, limit pushdown into a top-N sort, and sort
  elimination when the chosen scan already produces the requested order.
  `EXPLAIN` prints the physical plan as a tree.
- **Executor.** Volcano iterators — `Next() (Row, bool, error)` — for
  SeqScan, RangeScan, IndexScan, Filter, Project, Sort (in memory, with a
  bounded heap for top-N), Limit/Offset, Distinct and hash aggregation with
  `COUNT`, `SUM`, `AVG`, `MIN`, `MAX` and optional `GROUP BY`.
- **Three-valued logic.** Kleene semantics throughout `WHERE`: a comparison
  with NULL is UNKNOWN, `NOT UNKNOWN` is UNKNOWN, and only TRUE keeps a
  row. `IS NULL`, `IN`, `BETWEEN` and `LIKE` all follow the same rules.
- **Transactions.** `BEGIN`/`COMMIT`/`ROLLBACK` over a keelstore snapshot
  and an in-memory write buffer, with statements inside a transaction
  seeing their own uncommitted writes. Snapshot isolation without
  write-write conflict detection: last writer wins, documented rather than
  overstated.
- **`keelsql` REPL.** Reads a script from standard input or `-c`, prints
  results as an ASCII table with per-statement timings, and supports
  `.tables`, `.schema`, `.explain on|off`, `.timer on|off` and `.help`.
  The `-echo` flag reproduces an interactive transcript from a pipe.
- **Tests.** 171 test functions and 13 benchmarks, all offline, including
  an order-preservation property test over random values and a
  differential test that runs 3000 random statements against two keelsql
  databases (one indexed, one not) and a reference model written in plain
  Go, demanding that all three agree.
- **No CI.** `make check` is the gate, run by the committed
  `.githooks/pre-commit` hook. `make race` runs the suite under the race
  detector inside a pinned Docker image, for machines without a C
  toolchain.

[0.1.0]: https://github.com/aminyx/keelsql/releases/tag/v0.1.0
