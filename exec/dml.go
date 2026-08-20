package exec

import (
	"errors"
	"fmt"

	"github.com/aminyx/keelsql/catalog"
	"github.com/aminyx/keelsql/keycodec"
	"github.com/aminyx/keelsql/plan"
	"github.com/aminyx/keelsql/storage"
	"github.com/aminyx/keelsql/types"
)

// Constraint errors. They are sentinels so that a caller — or a test — can
// recognise them without matching on message text.
var (
	ErrDuplicateKey = errors.New("keelsql: duplicate primary key")
	ErrNotNull      = errors.New("keelsql: NOT NULL constraint violated")
	ErrNullKey      = errors.New("keelsql: primary key must not be NULL")
)

// A Mutator writes rows and keeps every index on the table in step with
// them. Nothing else in keelsql writes a row key, which is what makes
// "indexes are maintained transactionally" true by construction rather
// than by discipline: an index entry cannot be forgotten because the only
// path to the row also writes the entries.
type Mutator struct {
	rw    storage.ReadWriter
	table *catalog.Table
}

// NewMutator returns a mutator for one table.
func NewMutator(rw storage.ReadWriter, t *catalog.Table) *Mutator {
	return &Mutator{rw: rw, table: t}
}

// Insert validates a row and writes it together with its index entries.
func (m *Mutator) Insert(row Row) error {
	if err := m.validate(row); err != nil {
		return err
	}
	pk := row[m.table.PK]
	key := keycodec.RowKey(m.table.ID, pk)
	if _, err := m.rw.Get(key); err == nil {
		return fmt.Errorf("%w: %s.%s = %s", ErrDuplicateKey,
			m.table.Name, m.table.PKColumn().Name, pk.SQL())
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := m.rw.Put(key, keycodec.EncodeRow(nil, row)); err != nil {
		return err
	}
	return m.writeIndexes(row, pk, false)
}

// Update replaces oldRow with newRow, moving the row itself if the primary
// key changed and rewriting every index entry that pointed at it.
func (m *Mutator) Update(oldRow, newRow Row) error {
	if err := m.validate(newRow); err != nil {
		return err
	}
	oldPK, newPK := oldRow[m.table.PK], newRow[m.table.PK]

	if !types.Equal(oldPK, newPK) {
		newKey := keycodec.RowKey(m.table.ID, newPK)
		if _, err := m.rw.Get(newKey); err == nil {
			return fmt.Errorf("%w: %s.%s = %s", ErrDuplicateKey,
				m.table.Name, m.table.PKColumn().Name, newPK.SQL())
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := m.rw.Delete(keycodec.RowKey(m.table.ID, oldPK)); err != nil {
			return err
		}
	}

	if err := m.writeIndexes(oldRow, oldPK, true); err != nil {
		return err
	}
	if err := m.rw.Put(keycodec.RowKey(m.table.ID, newPK), keycodec.EncodeRow(nil, newRow)); err != nil {
		return err
	}
	return m.writeIndexes(newRow, newPK, false)
}

// Delete removes a row and its index entries.
func (m *Mutator) Delete(row Row) error {
	pk := row[m.table.PK]
	if err := m.writeIndexes(row, pk, true); err != nil {
		return err
	}
	return m.rw.Delete(keycodec.RowKey(m.table.ID, pk))
}

// writeIndexes adds or removes the index entries for one row. Every row has
// an entry in every index, NULLs included, so an index scan without bounds
// sees exactly what a table scan sees.
func (m *Mutator) writeIndexes(row Row, pk types.Value, remove bool) error {
	for _, idx := range m.table.Indexes {
		key := keycodec.IndexKey(m.table.ID, idx.ID, row[idx.Pos], pk)
		var err error
		if remove {
			err = m.rw.Delete(key)
		} else {
			err = m.rw.Put(key, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// validate applies the column constraints and the implicit conversions.
func (m *Mutator) validate(row Row) error {
	if len(row) != len(m.table.Columns) {
		return fmt.Errorf("keelsql: row has %d values, table %s has %d columns",
			len(row), m.table.Name, len(m.table.Columns))
	}
	for i, col := range m.table.Columns {
		v, err := types.Coerce(row[i], col.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", m.table.Name, col.Name, err)
		}
		row[i] = v
		if !v.IsNull() {
			continue
		}
		if col.PrimaryKey {
			return fmt.Errorf("%w: %s.%s", ErrNullKey, m.table.Name, col.Name)
		}
		if col.NotNull {
			return fmt.Errorf("%w: %s.%s", ErrNotNull, m.table.Name, col.Name)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// statement execution
// ---------------------------------------------------------------------

// RunDML executes an INSERT, UPDATE or DELETE plan and returns the number
// of rows it changed.
func RunDML(p plan.Plan, rw storage.ReadWriter) (int64, error) {
	switch n := p.(type) {
	case *plan.Insert:
		return runInsert(n, rw)
	case *plan.Update:
		return runUpdate(n, rw)
	case *plan.Delete:
		return runDelete(n, rw)
	}
	return 0, fmt.Errorf("keelsql: %T is not a DML statement", p)
}

func runInsert(n *plan.Insert, rw storage.ReadWriter) (int64, error) {
	m := NewMutator(rw, n.Table)
	var count int64
	for _, values := range n.Rows {
		row := make(Row, len(n.Table.Columns))
		for i := range row {
			row[i] = types.Null()
		}
		for i, e := range values {
			// VALUES entries are constant, so they evaluate without a row
			// and without an environment.
			v, err := Eval(e, nil, nil)
			if err != nil {
				return count, err
			}
			row[n.Cols[i]] = v
		}
		if err := m.Insert(row); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func runUpdate(n *plan.Update, rw storage.ReadWriter) (int64, error) {
	input, err := Build(n.Input, rw)
	if err != nil {
		return 0, err
	}
	defer input.Close()

	m := NewMutator(rw, n.Table)
	var count int64
	for {
		oldRow, ok, err := input.Next()
		if err != nil || !ok {
			return count, err
		}
		newRow := append(Row(nil), oldRow...)
		for _, a := range n.Set {
			// The right-hand sides all read the row as it was before this
			// statement, so `SET a = b, b = a` swaps rather than copies.
			v, err := Eval(a.Value, n.Env, oldRow)
			if err != nil {
				return count, err
			}
			newRow[a.Column] = v
		}
		if err := m.Update(oldRow, newRow); err != nil {
			return count, err
		}
		count++
	}
}

func runDelete(n *plan.Delete, rw storage.ReadWriter) (int64, error) {
	input, err := Build(n.Input, rw)
	if err != nil {
		return 0, err
	}
	defer input.Close()

	m := NewMutator(rw, n.Table)
	var count int64
	for {
		row, ok, err := input.Next()
		if err != nil || !ok {
			return count, err
		}
		if err := m.Delete(row); err != nil {
			return count, err
		}
		count++
	}
}

// ---------------------------------------------------------------------
// bulk maintenance
// ---------------------------------------------------------------------

// BuildIndex populates a new index from the rows already in the table, so
// that CREATE INDEX works on a table with data in it.
func BuildIndex(rw storage.ReadWriter, t *catalog.Table, idx *catalog.Index) error {
	prefix := keycodec.DataPrefix(t.ID)
	it := rw.Scan(prefix, keycodec.PrefixEnd(prefix))
	defer it.Close()

	type pending struct {
		key []byte
	}
	var writes []pending
	for it.Next() {
		row, err := DecodeStoredRow(t, it.Value())
		if err != nil {
			return err
		}
		writes = append(writes, pending{
			key: keycodec.IndexKey(t.ID, idx.ID, row[idx.Pos], row[t.PK]),
		})
	}
	if err := it.Err(); err != nil {
		return err
	}
	for _, w := range writes {
		if err := rw.Put(w.key, nil); err != nil {
			return err
		}
	}
	return nil
}

// DropIndexEntries removes every entry of an index.
func DropIndexEntries(rw storage.ReadWriter, t *catalog.Table, idx *catalog.Index) error {
	prefix := keycodec.IndexPrefix(t.ID, idx.ID)
	return deleteRange(rw, prefix, keycodec.PrefixEnd(prefix))
}

// DropTableData removes every row and index entry of a table.
func DropTableData(rw storage.ReadWriter, t *catalog.Table) error {
	prefix := keycodec.DataPrefix(t.ID)
	if err := deleteRange(rw, prefix, keycodec.PrefixEnd(prefix)); err != nil {
		return err
	}
	for i := range t.Indexes {
		if err := DropIndexEntries(rw, t, &t.Indexes[i]); err != nil {
			return err
		}
	}
	return nil
}

// deleteRange collects the keys first and deletes them afterwards, so that
// the deletions cannot disturb the iteration that found them.
func deleteRange(rw storage.ReadWriter, start, end []byte) error {
	it := rw.Scan(start, end)
	var keys [][]byte
	for it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	err := it.Err()
	if closeErr := it.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := rw.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// DecodeStoredRow decodes a complete stored row of a table.
func DecodeStoredRow(t *catalog.Table, blob []byte) (Row, error) {
	row, err := keycodec.DecodeRow(blob, len(t.Columns))
	if err != nil {
		return nil, fmt.Errorf("table %s: %w", t.Name, err)
	}
	return row, nil
}
