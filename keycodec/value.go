// Package keycodec turns SQL values into bytes.
//
// It provides two encodings, and the difference between them is the whole
// point of the package:
//
//   - The *key* encoding is order-preserving. For any two values a and b,
//     bytes.Compare(Encode(a), Encode(b)) has the same sign as
//     types.Order(a, b). keelstore stores keys in byte order, so an
//     ascending scan of encoded keys is an ascending scan of SQL values —
//     that is what makes a primary-key range scan and a secondary index
//     work without a sort.
//
//   - The *row* encoding is not ordered and does not have to be. It only
//     has to round-trip and to be skippable field by field, so that a query
//     that touches two columns of a ten-column table can walk past the
//     other eight without materialising them.
//
// Both encodings are self-delimiting: an encoded value knows where it ends,
// so values can be concatenated (an index entry is the indexed value
// followed by the primary key) and split apart again without any length
// prefixes.
//
// # Key encoding layout
//
//	kind    tag    payload
//	----    ---    ------------------------------------------------------
//	NULL    0x00   none
//	BOOL    0x10   1 byte: 0x00 false, 0x01 true
//	INT     0x20   8 bytes, big-endian, sign bit flipped
//	FLOAT   0x30   8 bytes, IEEE-754 bits, see below
//	TEXT    0x40   UTF-8 with 0x00 escaped as 0x00 0xFF, then 0x00 0x01
//
// The tags ascend in the same order as types.Kind, so values of different
// kinds sort by kind, with NULL first.
//
// Integers: two's complement does not sort correctly as unsigned bytes,
// because -1 is 0xFFFF…FF and 0 is 0x0000…00. Flipping the sign bit maps
// the signed range onto the unsigned range monotonically: -2^63 becomes
// 0x0000…00, -1 becomes 0x7FFF…FF, 0 becomes 0x8000…00 and 2^63-1 becomes
// 0xFFFF…FF. Big-endian bytes then compare in numeric order.
//
// Floats: IEEE-754 is already ordered within the positives, but negatives
// run backwards and the sign bit is inverted. For a non-negative float set
// the sign bit; for a negative float flip every bit. That maps -Inf to the
// smallest encoding and +Inf to the largest, with the ordering monotone
// across zero. (NaN encodes above +Inf, matching types.Order.)
//
// Strings: a plain byte comparison of UTF-8 is already lexicographic by
// code point, so the only problem is termination — without a terminator
// "a" and "ab" would be indistinguishable once a primary key is appended.
// A 0x00 terminator alone would break for strings that contain 0x00, so
// every 0x00 in the string becomes 0x00 0xFF and the terminator is
// 0x00 0x01. Since 0x01 < 0xFF, a string that is a prefix of another still
// sorts before it, and an embedded 0x00 still sorts after the end of a
// shorter string.
package keycodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/aminyx/keelsql/types"
)

// Tag bytes that introduce an encoded value. They are exported because the
// on-disk format is part of what this project documents.
const (
	TagNull  byte = 0x00
	TagBool  byte = 0x10
	TagInt   byte = 0x20
	TagFloat byte = 0x30
	TagText  byte = 0x40
)

const (
	escByte  byte = 0x00
	escLit   byte = 0xFF // 0x00 0xFF stands for a literal 0x00
	escTerm  byte = 0x01 // 0x00 0x01 ends a string
	signFlip      = uint64(1) << 63
)

// ErrCorrupt reports bytes that are not a valid encoding. It should only
// ever be seen if the store has been damaged or written by another program.
var ErrCorrupt = errors.New("keelsql: corrupt encoded value")

// Encode appends the order-preserving encoding of v to dst and returns the
// extended slice. Passing a nil dst allocates.
func Encode(dst []byte, v types.Value) []byte {
	switch v.Kind() {
	case types.KindNull:
		return append(dst, TagNull)

	case types.KindBool:
		b := byte(0)
		if v.AsBool() {
			b = 1
		}
		return append(dst, TagBool, b)

	case types.KindInt:
		dst = append(dst, TagInt)
		return binary.BigEndian.AppendUint64(dst, uint64(v.AsInt())^signFlip)

	case types.KindFloat:
		dst = append(dst, TagFloat)
		bits := math.Float64bits(v.AsFloat())
		if bits&signFlip != 0 {
			bits = ^bits // negative: reverse the whole range
		} else {
			bits |= signFlip // non-negative: lift above the negatives
		}
		return binary.BigEndian.AppendUint64(dst, bits)

	case types.KindText:
		dst = append(dst, TagText)
		for i := 0; i < len(v.AsText()); i++ {
			c := v.AsText()[i]
			if c == escByte {
				dst = append(dst, escByte, escLit)
				continue
			}
			dst = append(dst, c)
		}
		return append(dst, escByte, escTerm)
	}
	panic(fmt.Sprintf("keycodec: cannot encode kind %d", v.Kind()))
}

// EncodeAll appends every value in order. Because each encoding is
// self-delimiting the concatenation is still order-preserving, compared
// lexicographically field by field.
func EncodeAll(dst []byte, vals ...types.Value) []byte {
	for _, v := range vals {
		dst = Encode(dst, v)
	}
	return dst
}

// Key is EncodeAll with a fresh slice.
func Key(vals ...types.Value) []byte { return EncodeAll(nil, vals...) }

// Decode reads one value from the front of src and returns it together with
// the bytes that follow it.
func Decode(src []byte) (types.Value, []byte, error) {
	if len(src) == 0 {
		return types.Value{}, nil, fmt.Errorf("%w: empty input", ErrCorrupt)
	}
	tag, body := src[0], src[1:]
	switch tag {
	case TagNull:
		return types.Null(), body, nil

	case TagBool:
		if len(body) < 1 {
			return types.Value{}, nil, fmt.Errorf("%w: truncated BOOL", ErrCorrupt)
		}
		return types.Bool(body[0] != 0), body[1:], nil

	case TagInt:
		if len(body) < 8 {
			return types.Value{}, nil, fmt.Errorf("%w: truncated INT", ErrCorrupt)
		}
		u := binary.BigEndian.Uint64(body) ^ signFlip
		return types.Int(int64(u)), body[8:], nil

	case TagFloat:
		if len(body) < 8 {
			return types.Value{}, nil, fmt.Errorf("%w: truncated FLOAT", ErrCorrupt)
		}
		bits := binary.BigEndian.Uint64(body)
		if bits&signFlip != 0 {
			bits &^= signFlip
		} else {
			bits = ^bits
		}
		return types.Float(math.Float64frombits(bits)), body[8:], nil

	case TagText:
		out := make([]byte, 0, len(body))
		for i := 0; i < len(body); i++ {
			if body[i] != escByte {
				out = append(out, body[i])
				continue
			}
			if i+1 >= len(body) {
				return types.Value{}, nil, fmt.Errorf("%w: TEXT ends inside an escape", ErrCorrupt)
			}
			switch body[i+1] {
			case escTerm:
				return types.Text(string(out)), body[i+2:], nil
			case escLit:
				out = append(out, 0x00)
				i++
			default:
				return types.Value{}, nil, fmt.Errorf("%w: bad TEXT escape 0x%02x", ErrCorrupt, body[i+1])
			}
		}
		return types.Value{}, nil, fmt.Errorf("%w: unterminated TEXT", ErrCorrupt)
	}
	return types.Value{}, nil, fmt.Errorf("%w: unknown tag 0x%02x", ErrCorrupt, tag)
}

// Skip advances past one encoded value without building it. It is what
// makes projection pruning worth doing: skipping a long string costs a scan
// for its terminator, not an allocation and a copy.
func Skip(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrCorrupt)
	}
	tag, body := src[0], src[1:]
	switch tag {
	case TagNull:
		return body, nil
	case TagBool:
		if len(body) < 1 {
			return nil, fmt.Errorf("%w: truncated BOOL", ErrCorrupt)
		}
		return body[1:], nil
	case TagInt, TagFloat:
		if len(body) < 8 {
			return nil, fmt.Errorf("%w: truncated 8-byte value", ErrCorrupt)
		}
		return body[8:], nil
	case TagText:
		for i := 0; i+1 < len(body); i++ {
			if body[i] != escByte {
				continue
			}
			switch body[i+1] {
			case escTerm:
				return body[i+2:], nil
			case escLit:
				i++
			default:
				return nil, fmt.Errorf("%w: bad TEXT escape 0x%02x", ErrCorrupt, body[i+1])
			}
		}
		return nil, fmt.Errorf("%w: unterminated TEXT", ErrCorrupt)
	}
	return nil, fmt.Errorf("%w: unknown tag 0x%02x", ErrCorrupt, tag)
}

// DecodeAll reads exactly n values and reports an error if the input holds
// a different number.
func DecodeAll(src []byte, n int) ([]types.Value, error) {
	out := make([]types.Value, 0, n)
	rest := src
	for i := 0; i < n; i++ {
		v, tail, err := Decode(rest)
		if err != nil {
			return nil, fmt.Errorf("field %d: %w", i, err)
		}
		out = append(out, v)
		rest = tail
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes after %d values", ErrCorrupt, len(rest), n)
	}
	return out, nil
}
