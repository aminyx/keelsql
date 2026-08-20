package keycodec

import (
	"testing"

	"github.com/aminyx/keelsql/types"
)

var row = []types.Value{
	types.Int(1234567),
	types.Text("a reasonably long string value for a row"),
	types.Float(3.14159),
	types.Null(),
	types.Bool(true),
}

func BenchmarkEncodeRow(b *testing.B) {
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = EncodeRow(buf[:0], row)
	}
	_ = buf
}

func BenchmarkDecodeRow(b *testing.B) {
	blob := EncodeRow(nil, row)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeRow(blob, len(row)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeRowMasked is projection pruning measured: the same row,
// decoding one column instead of five.
func BenchmarkDecodeRowMasked(b *testing.B) {
	blob := EncodeRow(nil, row)
	mask := []bool{true, false, false, false, false}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeRowMasked(blob, mask); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeKey(b *testing.B) {
	buf := make([]byte, 0, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = Encode(buf[:0], types.Int(int64(i)))
	}
	_ = buf
}
