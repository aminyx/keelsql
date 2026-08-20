package keycodec

import (
	"fmt"

	"github.com/aminyx/keelsql/types"
)

// RowFormat is the version byte at the front of every encoded row. A stored
// row that starts with anything else was written by a different version of
// keelsql and is refused rather than misread.
const RowFormat byte = 0x01

// EncodeRow appends the encoding of a whole row to dst: a format byte
// followed by every column value in the table's declared column order,
// using the same self-delimiting value encoding as the key codec.
//
// The primary key is stored in the row as well as in the key. That costs a
// few bytes per row and buys a decoder that never has to know which column
// is the key.
func EncodeRow(dst []byte, row []types.Value) []byte {
	dst = append(dst, RowFormat)
	return EncodeAll(dst, row...)
}

// DecodeRow reads a row of n columns.
func DecodeRow(src []byte, n int) ([]types.Value, error) {
	body, err := rowBody(src)
	if err != nil {
		return nil, err
	}
	return DecodeAll(body, n)
}

// DecodeRowMasked reads only the columns whose entry in want is true and
// leaves the rest as NULL. It is the physical half of projection pruning:
// the planner works out which columns a query actually reads and the scan
// skips over the others instead of decoding them.
//
// want must have one entry per column. A nil want decodes everything.
func DecodeRowMasked(src []byte, want []bool) ([]types.Value, error) {
	if want == nil {
		return nil, fmt.Errorf("keycodec: DecodeRowMasked needs a mask")
	}
	body, err := rowBody(src)
	if err != nil {
		return nil, err
	}
	out := make([]types.Value, len(want))
	rest := body
	for i, w := range want {
		if w {
			v, tail, err := Decode(rest)
			if err != nil {
				return nil, fmt.Errorf("field %d: %w", i, err)
			}
			out[i], rest = v, tail
			continue
		}
		tail, err := Skip(rest)
		if err != nil {
			return nil, fmt.Errorf("field %d: %w", i, err)
		}
		rest = tail
	}
	return out, nil
}

func rowBody(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("%w: empty row", ErrCorrupt)
	}
	if src[0] != RowFormat {
		return nil, fmt.Errorf("%w: row format 0x%02x, want 0x%02x", ErrCorrupt, src[0], RowFormat)
	}
	return src[1:], nil
}
