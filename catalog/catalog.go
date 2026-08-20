// Package catalog holds keelsql's schema information and keeps it in the
// same keelstore database as the data.
//
// A table's definition lives under the reserved key prefix 0x01, encoded as
// JSON. There is no separate metadata file and no second store: opening a
// keelsql database means opening keelstore and reading the catalog back out
// of it, which is why a schema created in one process is still there in the
// next.
package catalog

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// Catalog errors. They are sentinels so that callers can test for them with
// errors.Is; the wrapped message carries the name that was not found.
var (
	ErrTableNotFound  = errors.New("keelsql: no such table")
	ErrTableExists    = errors.New("keelsql: table already exists")
	ErrIndexNotFound  = errors.New("keelsql: no such index")
	ErrIndexExists    = errors.New("keelsql: index already exists")
	ErrColumnNotFound = errors.New("keelsql: no such column")
	ErrNoPrimaryKey   = errors.New("keelsql: table needs exactly one PRIMARY KEY column")
	ErrCorruptCatalog = errors.New("keelsql: corrupt catalog entry")
)

// A Column is one column of a table.
type Column struct {
	Name       string     `json:"name"`
	Type       types.Kind `json:"type"`
	NotNull    bool       `json:"not_null,omitempty"`
	PrimaryKey bool       `json:"primary_key,omitempty"`
}

// An Index is a secondary index over exactly one column.
type Index struct {
	Name   string `json:"name"`
	ID     uint32 `json:"id"`
	Column string `json:"column"`
	Pos    int    `json:"pos"` // the indexed column's position in a row
}

// A Table is a table's schema plus the identifiers that locate its rows and
// index entries in the key space.
type Table struct {
	Name        string   `json:"name"`
	ID          uint32   `json:"id"`
	Columns     []Column `json:"columns"`
	PK          int      `json:"pk"` // position of the primary key column
	Indexes     []Index  `json:"indexes,omitempty"`
	NextIndexID uint32   `json:"next_index_id"`
}

// PKColumn returns the primary key column.
func (t *Table) PKColumn() Column { return t.Columns[t.PK] }

// ColumnIndex returns the position of a named column.
func (t *Table) ColumnIndex(name string) (int, bool) {
	for i, c := range t.Columns {
		if c.Name == name {
			return i, true
		}
	}
	return 0, false
}

// ColumnNames returns every column name in declaration order.
func (t *Table) ColumnNames() []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

// FindIndex returns the index with the given name.
func (t *Table) FindIndex(name string) (*Index, bool) {
	for i := range t.Indexes {
		if t.Indexes[i].Name == name {
			return &t.Indexes[i], true
		}
	}
	return nil, false
}

// IndexOn returns an index over the named column, if one exists. When
// several indexes cover the same column the first one wins, which keeps
// planning deterministic.
func (t *Table) IndexOn(column string) (*Index, bool) {
	for i := range t.Indexes {
		if t.Indexes[i].Column == column {
			return &t.Indexes[i], true
		}
	}
	return nil, false
}

// Clone returns a deep copy, so that a failed DDL statement cannot leave a
// half-modified schema in memory.
func (t *Table) Clone() *Table {
	out := *t
	out.Columns = append([]Column(nil), t.Columns...)
	out.Indexes = append([]Index(nil), t.Indexes...)
	return &out
}

// SQL renders the CREATE TABLE statement that would recreate the table.
// The CLI's .schema command prints it.
func (t *Table) SQL() string {
	parts := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		parts[i] = "  " + c.Name + " " + c.Type.String()
		if c.PrimaryKey {
			parts[i] += " PRIMARY KEY"
		} else if c.NotNull {
			parts[i] += " NOT NULL"
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE %s (\n%s\n);", t.Name, strings.Join(parts, ",\n"))
	for _, idx := range t.Indexes {
		fmt.Fprintf(&sb, "\nCREATE INDEX %s ON %s (%s);", idx.Name, t.Name, idx.Column)
	}
	return sb.String()
}

// ---------------------------------------------------------------------
// the catalog itself
// ---------------------------------------------------------------------

// A Catalog is the in-memory schema, kept in step with what is stored.
// It is safe for concurrent readers; writers are serialised by the database
// above it.
type Catalog struct {
	mu     sync.RWMutex
	tables map[string]*Table
	nextID uint32
}

// Load reads every catalog entry out of the store.
func Load(r storage.Reader) (*Catalog, error) {
	c := &Catalog{tables: map[string]*Table{}, nextID: 1}

	prefix := keycodec.MetaTablePrefix()
	it := r.Scan(prefix, keycodec.PrefixEnd(prefix))
	defer it.Close()
	for it.Next() {
		name, err := keycodec.MetaTableName(it.Key())
		if err != nil {
			return nil, err
		}
		var t Table
		if err := json.Unmarshal(it.Value(), &t); err != nil {
			return nil, fmt.Errorf("%w for table %q: %v", ErrCorruptCatalog, name, err)
		}
		if t.PK < 0 || t.PK >= len(t.Columns) {
			return nil, fmt.Errorf("%w for table %q: primary key position %d", ErrCorruptCatalog, name, t.PK)
		}
		c.tables[t.Name] = &t
	}
	if err := it.Err(); err != nil {
		return nil, err
	}

	seq, err := r.Get(keycodec.MetaSeqKey())
	switch {
	case errors.Is(err, storage.ErrNotFound):
		// A fresh database: identifiers start at 1.
	case err != nil:
		return nil, err
	case len(seq) != 4:
		return nil, fmt.Errorf("%w: identifier counter is %d bytes", ErrCorruptCatalog, len(seq))
	default:
		c.nextID = binary.BigEndian.Uint32(seq)
	}
	return c, nil
}

// Get returns a table by name.
func (c *Catalog) Get(name string) (*Table, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	return t, nil
}

// Has reports whether a table exists.
func (c *Catalog) Has(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.tables[name]
	return ok
}

// All returns every table, ordered by name.
func (c *Catalog) All() []*Table {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every table name, sorted.
func (c *Catalog) Names() []string {
	tables := c.All()
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = t.Name
	}
	return out
}

// FindIndex looks an index up by name across every table. Index names are
// unique database-wide, because DROP INDEX names one without a table.
func (c *Catalog) FindIndex(name string) (*Table, *Index, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, t := range c.tables {
		if idx, ok := t.FindIndex(name); ok {
			return t, idx, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
}

// Create defines a new table, assigning it an identifier and persisting it.
func (c *Catalog) Create(w storage.Writer, def *Table) (*Table, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[def.Name]; ok {
		return nil, fmt.Errorf("%w: %s", ErrTableExists, def.Name)
	}
	t := def.Clone()
	t.ID = c.nextID
	t.NextIndexID = 1
	if err := c.bumpID(w); err != nil {
		return nil, err
	}
	if err := persist(w, t); err != nil {
		return nil, err
	}
	c.tables[t.Name] = t
	return t, nil
}

// Drop forgets a table. Removing its rows and index entries is the caller's
// job; the catalog only owns the schema.
func (c *Catalog) Drop(w storage.Writer, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[name]; !ok {
		return fmt.Errorf("%w: %s", ErrTableNotFound, name)
	}
	if err := w.Delete(keycodec.MetaTableKey(name)); err != nil {
		return err
	}
	delete(c.tables, name)
	return nil
}

// AddIndex records a new index on a table and returns the stored copy. The
// entries themselves are built by the caller, which is what makes CREATE
// INDEX on a populated table work.
func (c *Catalog) AddIndex(w storage.Writer, table, name, column string) (*Table, *Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tables[table]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrTableNotFound, table)
	}
	for _, other := range c.tables {
		if _, exists := other.FindIndex(name); exists {
			return nil, nil, fmt.Errorf("%w: %s", ErrIndexExists, name)
		}
	}
	pos, ok := t.ColumnIndex(column)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s.%s", ErrColumnNotFound, table, column)
	}

	updated := t.Clone()
	idx := Index{Name: name, ID: updated.NextIndexID, Column: column, Pos: pos}
	updated.NextIndexID++
	updated.Indexes = append(updated.Indexes, idx)
	if err := persist(w, updated); err != nil {
		return nil, nil, err
	}
	c.tables[table] = updated
	stored, _ := updated.FindIndex(name)
	return updated, stored, nil
}

// RemoveIndex forgets an index. Deleting its entries is the caller's job.
func (c *Catalog) RemoveIndex(w storage.Writer, name string) (*Table, *Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tables {
		idx, ok := t.FindIndex(name)
		if !ok {
			continue
		}
		dropped := *idx
		updated := t.Clone()
		updated.Indexes = nil
		for _, keep := range t.Indexes {
			if keep.Name != name {
				updated.Indexes = append(updated.Indexes, keep)
			}
		}
		if err := persist(w, updated); err != nil {
			return nil, nil, err
		}
		c.tables[updated.Name] = updated
		return updated, &dropped, nil
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
}

func (c *Catalog) bumpID(w storage.Writer) error {
	next := make([]byte, 4)
	binary.BigEndian.PutUint32(next, c.nextID+1)
	if err := w.Put(keycodec.MetaSeqKey(), next); err != nil {
		return err
	}
	c.nextID++
	return nil
}

func persist(w storage.Writer, t *Table) error {
	blob, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return w.Put(keycodec.MetaTableKey(t.Name), blob)
}

// Define validates a parsed table definition and returns the schema to
// create. Exactly one column must be the primary key: keelsql stores a row
// under its key, so a table without one has nowhere to put its rows.
func Define(name string, cols []Column) (*Table, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("keelsql: table %s has no columns", name)
	}
	seen := map[string]bool{}
	pk := -1
	for i, c := range cols {
		if seen[c.Name] {
			return nil, fmt.Errorf("keelsql: duplicate column %q in table %s", c.Name, name)
		}
		seen[c.Name] = true
		if !c.PrimaryKey {
			continue
		}
		if pk >= 0 {
			return nil, fmt.Errorf("%w: %s declares %q and %q", ErrNoPrimaryKey, name, cols[pk].Name, c.Name)
		}
		pk = i
	}
	if pk < 0 {
		return nil, fmt.Errorf("%w: %s declares none", ErrNoPrimaryKey, name)
	}
	cols[pk].NotNull = true
	return &Table{Name: name, Columns: cols, PK: pk}, nil
}
