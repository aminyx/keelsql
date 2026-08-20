package keycodec

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/aminyx/keelsql/types"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	values := []types.Value{
		types.Null(),
		types.Bool(false),
		types.Bool(true),
		types.Int(0),
		types.Int(1),
		types.Int(-1),
		types.Int(math.MaxInt64),
		types.Int(math.MinInt64),
		types.Float(0),
		types.Float(-0.5),
		types.Float(math.Inf(1)),
		types.Float(math.Inf(-1)),
		types.Text(""),
		types.Text("hello"),
		types.Text("with\x00zero"),
		types.Text("\x00\x00"),
		types.Text("héllo, 世界"),
	}
	for _, v := range values {
		encoded := Encode(nil, v)
		got, rest, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode(%v): %v", v, err)
		}
		if len(rest) != 0 {
			t.Errorf("Decode(%v) left %d bytes", v, len(rest))
		}
		if !types.Equal(got, v) {
			t.Errorf("round trip of %v gave %v", v, got)
		}
	}
}

func TestTagsMatchTheDocumentedLayout(t *testing.T) {
	cases := []struct {
		value types.Value
		tag   byte
		size  int
	}{
		{types.Null(), TagNull, 1},
		{types.Bool(true), TagBool, 2},
		{types.Int(1), TagInt, 9},
		{types.Float(1), TagFloat, 9},
		{types.Text("ab"), TagText, 5}, // tag + 2 bytes + 0x00 0x01
	}
	for _, c := range cases {
		encoded := Encode(nil, c.value)
		if encoded[0] != c.tag {
			t.Errorf("%v: tag = 0x%02x, want 0x%02x", c.value, encoded[0], c.tag)
		}
		if len(encoded) != c.size {
			t.Errorf("%v: %d bytes, want %d", c.value, len(encoded), c.size)
		}
	}
}

// TestOrderPreservationProperty is the property the whole storage layout
// rests on: sorting encoded values as byte strings must produce the same
// order as sorting the values themselves. If it ever fails, a range scan
// silently returns the wrong rows.
func TestOrderPreservationProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20260821))

	for round := 0; round < 200; round++ {
		values := make([]types.Value, 60)
		for i := range values {
			values[i] = randomValue(rng)
		}

		encoded := make([][]byte, len(values))
		for i, v := range values {
			encoded[i] = Encode(nil, v)
		}

		// Sort the encodings as raw bytes...
		byBytes := append([][]byte(nil), encoded...)
		sort.Slice(byBytes, func(i, j int) bool { return bytes.Compare(byBytes[i], byBytes[j]) < 0 })

		// ...and sort the values by the logical order, then encode them.
		byValue := append([]types.Value(nil), values...)
		sort.SliceStable(byValue, func(i, j int) bool { return types.Order(byValue[i], byValue[j]) < 0 })

		for i := range byValue {
			want := Encode(nil, byValue[i])
			if !bytes.Equal(byBytes[i], want) {
				got, _, _ := Decode(byBytes[i])
				t.Fatalf("round %d position %d: byte order gave %v, value order gave %v",
					round, i, got, byValue[i])
			}
		}
	}
}

// TestOrderPreservationForNegativeIntegers pins the case the sign-bit flip
// exists for: two's complement bytes sort negatives above positives.
func TestOrderPreservationForNegativeIntegers(t *testing.T) {
	ints := []int64{math.MinInt64, -1 << 40, -256, -2, -1, 0, 1, 2, 256, 1 << 40, math.MaxInt64}
	for i := 1; i < len(ints); i++ {
		lo := Encode(nil, types.Int(ints[i-1]))
		hi := Encode(nil, types.Int(ints[i]))
		if bytes.Compare(lo, hi) >= 0 {
			t.Errorf("encoding of %d does not sort before %d", ints[i-1], ints[i])
		}
	}
}

// TestOrderPreservationForFloats covers the other sign trick: negative
// IEEE-754 values run backwards until every bit is flipped.
func TestOrderPreservationForFloats(t *testing.T) {
	floats := []float64{
		math.Inf(-1), -1e300, -1, -0.5, -math.SmallestNonzeroFloat64,
		0, math.SmallestNonzeroFloat64, 0.5, 1, 1e300, math.Inf(1),
	}
	for i := 1; i < len(floats); i++ {
		lo := Encode(nil, types.Float(floats[i-1]))
		hi := Encode(nil, types.Float(floats[i]))
		if bytes.Compare(lo, hi) >= 0 {
			t.Errorf("encoding of %v does not sort before %v", floats[i-1], floats[i])
		}
	}
}

// TestOrderPreservationForStringPrefixes is the reason for the 0x00 0x01
// terminator: without it a prefix and its extension would be
// indistinguishable once a primary key is appended.
func TestOrderPreservationForStringPrefixes(t *testing.T) {
	strs := []string{"", "a", "a\x00", "a\x00b", "aa", "ab", "b", "\xff"}
	for i := 1; i < len(strs); i++ {
		lo := Encode(nil, types.Text(strs[i-1]))
		hi := Encode(nil, types.Text(strs[i]))
		if bytes.Compare(lo, hi) >= 0 {
			t.Errorf("encoding of %q does not sort before %q", strs[i-1], strs[i])
		}
	}
}

// TestCompositeKeysSortFieldByField checks that concatenating encodings —
// what an index entry does with the indexed value and the primary key —
// still sorts correctly.
func TestCompositeKeysSortFieldByField(t *testing.T) {
	keys := [][]byte{
		Key(types.Text("a"), types.Int(1)),
		Key(types.Text("a"), types.Int(2)),
		Key(types.Text("ab"), types.Int(1)),
		Key(types.Text("b"), types.Int(-5)),
	}
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Errorf("composite key %d does not sort before %d", i-1, i)
		}
	}
}

func TestSkipMatchesDecode(t *testing.T) {
	values := []types.Value{
		types.Null(), types.Bool(true), types.Int(-5), types.Float(1.5),
		types.Text("a\x00b"), types.Text(""),
	}
	blob := EncodeAll(nil, values...)
	rest := blob
	for i := range values {
		skipped, err := Skip(rest)
		if err != nil {
			t.Fatalf("Skip at %d: %v", i, err)
		}
		_, decoded, err := Decode(rest)
		if err != nil {
			t.Fatalf("Decode at %d: %v", i, err)
		}
		if !bytes.Equal(skipped, decoded) {
			t.Fatalf("Skip and Decode disagree at field %d", i)
		}
		rest = skipped
	}
	if len(rest) != 0 {
		t.Errorf("%d bytes left over", len(rest))
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	if _, _, err := Decode(nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Decode(nil) error = %v, want ErrCorrupt", err)
	}
	if _, _, err := Decode([]byte{0x99}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Decode(unknown tag) error = %v, want ErrCorrupt", err)
	}
	if _, _, err := Decode([]byte{TagInt, 1, 2}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Decode(truncated INT) error = %v, want ErrCorrupt", err)
	}
	if _, _, err := Decode([]byte{TagText, 'a'}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Decode(unterminated TEXT) error = %v, want ErrCorrupt", err)
	}
	if _, err := Skip([]byte{TagText, 'a', 0x00, 0x42}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Skip(bad escape) error = %v, want ErrCorrupt", err)
	}
}

func TestRowRoundTrip(t *testing.T) {
	row := []types.Value{types.Int(7), types.Text("ada"), types.Null(), types.Float(2.5)}
	blob := EncodeRow(nil, row)
	if blob[0] != RowFormat {
		t.Fatalf("row starts with 0x%02x, want the format byte", blob[0])
	}
	got, err := DecodeRow(blob, len(row))
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if !types.Equal(got[i], row[i]) {
			t.Errorf("column %d: got %v, want %v", i, got[i], row[i])
		}
	}
}

func TestDecodeRowMaskedSkipsUnwantedColumns(t *testing.T) {
	row := []types.Value{types.Int(7), types.Text("a long string value"), types.Float(2.5)}
	blob := EncodeRow(nil, row)

	got, err := DecodeRowMasked(blob, []bool{true, false, true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("masked decode returned %d columns", len(got))
	}
	if got[0].AsInt() != 7 || got[2].AsFloat() != 2.5 {
		t.Errorf("wanted columns decoded wrongly: %v", got)
	}
	if !got[1].IsNull() {
		t.Errorf("skipped column should come back as NULL, got %v", got[1])
	}
}

func TestDecodeRowRejectsWrongFormatAndArity(t *testing.T) {
	blob := EncodeRow(nil, []types.Value{types.Int(1)})
	if _, err := DecodeRow(blob, 2); !errors.Is(err, ErrCorrupt) {
		t.Errorf("decoding 1 column as 2 gave %v, want ErrCorrupt", err)
	}
	bad := append([]byte(nil), blob...)
	bad[0] = 0x42
	if _, err := DecodeRow(bad, 1); !errors.Is(err, ErrCorrupt) {
		t.Errorf("wrong format byte gave %v, want ErrCorrupt", err)
	}
	if _, err := DecodeRowMasked(blob, nil); err == nil {
		t.Error("DecodeRowMasked(nil mask) should fail")
	}
}

func TestKeyLayout(t *testing.T) {
	rowKey := RowKey(3, types.Int(9))
	if rowKey[0] != PrefixData {
		t.Errorf("row key prefix = 0x%02x, want 0x%02x", rowKey[0], PrefixData)
	}
	if !bytes.HasPrefix(rowKey, DataPrefix(3)) {
		t.Error("row key should start with its table prefix")
	}
	pk, err := RowKeyPK(rowKey)
	if err != nil || pk.AsInt() != 9 {
		t.Fatalf("RowKeyPK = %v, %v", pk, err)
	}

	idxKey := IndexKey(3, 1, types.Text("x"), types.Int(9))
	if idxKey[0] != PrefixIndex {
		t.Errorf("index key prefix = 0x%02x", idxKey[0])
	}
	val, pk, err := IndexEntry(idxKey)
	if err != nil {
		t.Fatal(err)
	}
	if val.AsText() != "x" || pk.AsInt() != 9 {
		t.Errorf("IndexEntry = %v, %v", val, pk)
	}

	meta := MetaTableKey("users")
	name, err := MetaTableName(meta)
	if err != nil || name != "users" {
		t.Fatalf("MetaTableName = %q, %v", name, err)
	}
	if !bytes.HasPrefix(meta, MetaTablePrefix()) {
		t.Error("catalog key should start with the catalog prefix")
	}
	if _, err := MetaTableName([]byte{0x02, 0x03}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("MetaTableName on a row key gave %v", err)
	}
	if _, err := RowKeyPK([]byte{0x01}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("RowKeyPK on a catalog key gave %v", err)
	}
	if _, _, err := IndexEntry([]byte{0x02}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("IndexEntry on a row key gave %v", err)
	}
}

func TestTablesOccupyDisjointRanges(t *testing.T) {
	a, b := DataPrefix(1), DataPrefix(2)
	endA := PrefixEnd(a)
	if bytes.Compare(endA, b) > 0 {
		t.Error("table 1's range should end at or before table 2's start")
	}
	row := RowKey(1, types.Text("zzzz"))
	if bytes.Compare(row, endA) >= 0 {
		t.Error("a row of table 1 escaped table 1's range")
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct {
		in, want []byte
	}{
		{[]byte{0x01}, []byte{0x02}},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
		{[]byte{0x01, 0x02, 0xFF, 0xFF}, []byte{0x01, 0x03}},
		{[]byte{0xFF, 0xFF}, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("PrefixEnd(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func randomValue(rng *rand.Rand) types.Value {
	switch rng.Intn(5) {
	case 0:
		return types.Null()
	case 1:
		return types.Bool(rng.Intn(2) == 0)
	case 2:
		switch rng.Intn(4) {
		case 0:
			return types.Int(math.MinInt64)
		case 1:
			return types.Int(math.MaxInt64)
		case 2:
			return types.Int(int64(rng.Intn(41) - 20))
		default:
			return types.Int(rng.Int63() - (1 << 62))
		}
	case 3:
		switch rng.Intn(4) {
		case 0:
			return types.Float(0)
		case 1:
			return types.Float(math.Inf(rng.Intn(2)*2 - 1))
		case 2:
			return types.Float(float64(rng.Intn(21)-10) / 2)
		default:
			return types.Float((rng.Float64() - 0.5) * 1e12)
		}
	default:
		alphabet := "ab\x00\xff é"
		n := rng.Intn(5)
		out := make([]byte, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, alphabet[rng.Intn(len(alphabet))])
		}
		return types.Text(string(out))
	}
}
