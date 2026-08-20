package keycodec

import (
	"encoding/binary"
	"fmt"

	"github.com/aminyx/keelsql/types"
)

// keelsql keeps everything — catalog, rows and index entries — in one flat
// keelstore key space, partitioned by a leading prefix byte:
//
//	0x01 0x74 <name>                          catalog entry for table <name>
//	0x01 0x73                                 the identifier counter
//	0x02 <table:4> <pk>                       a row
//	0x03 <table:4> <index:4> <value> <pk>     a secondary index entry
//
// Table and index identifiers are big-endian so that a table's rows are one
// contiguous byte range, which is what turns "scan table t" into a bounded
// keelstore iteration rather than a walk over the whole store. Names are
// only used in the catalog: renaming a table would not have to rewrite a
// single row (keelsql does not implement rename, but the layout allows it).
const (
	PrefixMeta  byte = 0x01
	PrefixData  byte = 0x02
	PrefixIndex byte = 0x03

	metaTable byte = 0x74 // 't'
	metaSeq   byte = 0x73 // 's'
)

// MetaTableKey is the catalog key holding the schema of a named table.
func MetaTableKey(name string) []byte {
	out := make([]byte, 0, 2+len(name))
	out = append(out, PrefixMeta, metaTable)
	return append(out, name...)
}

// MetaTablePrefix is the prefix every catalog table entry shares, used to
// list the catalog on open.
func MetaTablePrefix() []byte { return []byte{PrefixMeta, metaTable} }

// MetaTableName recovers the table name from a catalog key.
func MetaTableName(key []byte) (string, error) {
	if len(key) < 2 || key[0] != PrefixMeta || key[1] != metaTable {
		return "", fmt.Errorf("%w: not a catalog table key", ErrCorrupt)
	}
	return string(key[2:]), nil
}

// MetaSeqKey is the key of the counter that hands out table and index ids.
func MetaSeqKey() []byte { return []byte{PrefixMeta, metaSeq} }

// DataPrefix is the common prefix of every row in a table.
func DataPrefix(table uint32) []byte {
	out := make([]byte, 0, 5)
	out = append(out, PrefixData)
	return binary.BigEndian.AppendUint32(out, table)
}

// RowKey is the storage key of one row, identified by its primary key.
func RowKey(table uint32, pk types.Value) []byte {
	return Encode(DataPrefix(table), pk)
}

// RowKeyPK recovers the primary key from a row key.
func RowKeyPK(key []byte) (types.Value, error) {
	if len(key) < 5 || key[0] != PrefixData {
		return types.Value{}, fmt.Errorf("%w: not a row key", ErrCorrupt)
	}
	v, rest, err := Decode(key[5:])
	if err != nil {
		return types.Value{}, err
	}
	if len(rest) != 0 {
		return types.Value{}, fmt.Errorf("%w: %d trailing bytes in row key", ErrCorrupt, len(rest))
	}
	return v, nil
}

// IndexPrefix is the common prefix of every entry of one index.
func IndexPrefix(table, index uint32) []byte {
	out := make([]byte, 0, 9)
	out = append(out, PrefixIndex)
	out = binary.BigEndian.AppendUint32(out, table)
	return binary.BigEndian.AppendUint32(out, index)
}

// IndexKey is the storage key of one index entry. The indexed value comes
// first so that the entries sort by that value; the primary key is appended
// to keep entries unique when the indexed value repeats, and so that the
// row can be fetched without storing anything in the entry's value.
func IndexKey(table, index uint32, val, pk types.Value) []byte {
	return EncodeAll(IndexPrefix(table, index), val, pk)
}

// IndexEntry splits an index key back into the indexed value and the
// primary key it points at.
func IndexEntry(key []byte) (val, pk types.Value, err error) {
	if len(key) < 9 || key[0] != PrefixIndex {
		return types.Value{}, types.Value{}, fmt.Errorf("%w: not an index key", ErrCorrupt)
	}
	val, rest, err := Decode(key[9:])
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	pk, rest, err = Decode(rest)
	if err != nil {
		return types.Value{}, types.Value{}, err
	}
	if len(rest) != 0 {
		return types.Value{}, types.Value{}, fmt.Errorf("%w: %d trailing bytes in index key", ErrCorrupt, len(rest))
	}
	return val, pk, nil
}

// PrefixEnd returns the smallest key that sorts after every key beginning
// with prefix, which is what an exclusive upper bound needs to be. It works
// by incrementing the last byte below 0xFF and dropping the 0xFF tail; a
// prefix of nothing but 0xFF bytes has no successor, and nil means
// "unbounded above".
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
