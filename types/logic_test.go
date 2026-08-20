package types

import (
	"errors"
	"testing"
)

// The three-valued truth tables, written out in full. They are the contract
// SQL's WHERE clause depends on, so they are pinned rather than derived.

func TestAndTruthTable(t *testing.T) {
	cases := []struct{ a, b, want Bool3 }{
		{True, True, True},
		{True, False, False},
		{True, Unknown, Unknown},
		{False, True, False},
		{False, False, False},
		{False, Unknown, False}, // false whatever the unknown turns out to be
		{Unknown, True, Unknown},
		{Unknown, False, False},
		{Unknown, Unknown, Unknown},
	}
	for _, c := range cases {
		if got := c.a.And(c.b); got != c.want {
			t.Errorf("%v AND %v = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOrTruthTable(t *testing.T) {
	cases := []struct{ a, b, want Bool3 }{
		{True, True, True},
		{True, False, True},
		{True, Unknown, True}, // true whatever the unknown turns out to be
		{False, True, True},
		{False, False, False},
		{False, Unknown, Unknown},
		{Unknown, True, True},
		{Unknown, False, Unknown},
		{Unknown, Unknown, Unknown},
	}
	for _, c := range cases {
		if got := c.a.Or(c.b); got != c.want {
			t.Errorf("%v OR %v = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNotTruthTable(t *testing.T) {
	cases := []struct{ a, want Bool3 }{
		{True, False},
		{False, True},
		{Unknown, Unknown}, // NOT does not resolve an unknown
	}
	for _, c := range cases {
		if got := c.a.Not(); got != c.want {
			t.Errorf("NOT %v = %v, want %v", c.a, got, c.want)
		}
	}
}

func TestOnlyTrueKeepsARow(t *testing.T) {
	if !True.IsTrue() {
		t.Error("TRUE should keep a row")
	}
	if False.IsTrue() || Unknown.IsTrue() {
		t.Error("only TRUE keeps a row; FALSE and UNKNOWN both drop it")
	}
}

func TestComparisonWithNullIsUnknown(t *testing.T) {
	for _, op := range []string{"=", "!=", "<>", "<", "<=", ">", ">="} {
		got, err := CompareOp(op, Null(), Null())
		if err != nil {
			t.Fatalf("NULL %s NULL: %v", op, err)
		}
		if got != Unknown {
			t.Errorf("NULL %s NULL = %v, want UNKNOWN", op, got)
		}
		got, err = CompareOp(op, Int(1), Null())
		if err != nil {
			t.Fatalf("1 %s NULL: %v", op, err)
		}
		if got != Unknown {
			t.Errorf("1 %s NULL = %v, want UNKNOWN", op, got)
		}
	}
}

func TestComparisonOperators(t *testing.T) {
	cases := []struct {
		op   string
		a, b Value
		want Bool3
	}{
		{"=", Int(1), Int(1), True},
		{"=", Int(1), Int(2), False},
		{"==", Int(1), Int(1), True},
		{"!=", Int(1), Int(2), True},
		{"<>", Int(1), Int(1), False},
		{"<", Int(1), Int(2), True},
		{"<=", Int(2), Int(2), True},
		{">", Int(3), Int(2), True},
		{">=", Int(1), Int(2), False},
		{"=", Text("a"), Text("a"), True},
	}
	for _, c := range cases {
		got, err := CompareOp(c.op, c.a, c.b)
		if err != nil {
			t.Fatalf("%v %s %v: %v", c.a, c.op, c.b, err)
		}
		if got != c.want {
			t.Errorf("%v %s %v = %v, want %v", c.a, c.op, c.b, got, c.want)
		}
	}
}

func TestBool3RoundTripsThroughValue(t *testing.T) {
	if v := Unknown.Value(); !v.IsNull() {
		t.Errorf("UNKNOWN.Value() = %v, want NULL", v)
	}
	if v := True.Value(); v.Kind() != KindBool || !v.AsBool() {
		t.Errorf("TRUE.Value() = %v", v)
	}
	if v := False.Value(); v.Kind() != KindBool || v.AsBool() {
		t.Errorf("FALSE.Value() = %v", v)
	}
}

func TestTruthReadsConditions(t *testing.T) {
	got, err := Truth(Null())
	if err != nil || got != Unknown {
		t.Errorf("Truth(NULL) = %v, %v; want UNKNOWN", got, err)
	}
	if got, err := Truth(Bool(true)); err != nil || got != True {
		t.Errorf("Truth(true) = %v, %v", got, err)
	}
	// keelsql does not treat 0 or '' as false.
	if _, err := Truth(Int(0)); !errors.Is(err, ErrType) {
		t.Errorf("Truth(0) error = %v, want ErrType", err)
	}
	if _, err := Truth(Text("")); !errors.Is(err, ErrType) {
		t.Errorf("Truth('') error = %v, want ErrType", err)
	}
}

func TestB3Lifting(t *testing.T) {
	if B3(true) != True || B3(false) != False {
		t.Error("B3 should map Go booleans onto TRUE and FALSE")
	}
	if True.String() != "TRUE" || False.String() != "FALSE" || Unknown.String() != "UNKNOWN" {
		t.Error("Bool3.String should spell the values the way SQL does")
	}
}
