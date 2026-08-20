package types

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestValueKindsAndAccessors(t *testing.T) {
	cases := []struct {
		value Value
		kind  Kind
		text  string
	}{
		{Null(), KindNull, "NULL"},
		{Bool(true), KindBool, "true"},
		{Bool(false), KindBool, "false"},
		{Int(-7), KindInt, "-7"},
		{Float(2.5), KindFloat, "2.5"},
		{Float(3), KindFloat, "3.0"},
		{Text("hi"), KindText, "hi"},
	}
	for _, c := range cases {
		if got := c.value.Kind(); got != c.kind {
			t.Errorf("%v: kind = %v, want %v", c.text, got, c.kind)
		}
		if got := c.value.String(); got != c.text {
			t.Errorf("String() = %q, want %q", got, c.text)
		}
	}
	if !Null().IsNull() {
		t.Error("Null().IsNull() = false")
	}
	if Int(0).IsNull() {
		t.Error("Int(0) reports itself as NULL")
	}
}

func TestValueSQLQuotesStrings(t *testing.T) {
	if got := Text("it's").SQL(); got != "'it''s'" {
		t.Errorf("SQL() = %s, want 'it''s'", got)
	}
	if got := Int(3).SQL(); got != "3" {
		t.Errorf("SQL() = %s, want 3", got)
	}
	if got := Null().SQL(); got != "NULL" {
		t.Errorf("SQL() = %s, want NULL", got)
	}
}

func TestCompareAcrossNumericKinds(t *testing.T) {
	cases := []struct {
		a, b Value
		want int
	}{
		{Int(1), Int(2), -1},
		{Int(2), Int(2), 0},
		{Int(3), Int(2), 1},
		{Int(2), Float(2.5), -1},
		{Float(2.5), Int(2), 1},
		{Float(2), Int(2), 0},
		{Text("a"), Text("b"), -1},
		{Text("b"), Text("b"), 0},
		{Bool(false), Bool(true), -1},
	}
	for _, c := range cases {
		got, err := Compare(c.a, c.b)
		if err != nil {
			t.Fatalf("Compare(%v, %v): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("Compare(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareRejectsMixedKinds(t *testing.T) {
	if _, err := Compare(Text("a"), Int(1)); !errors.Is(err, ErrType) {
		t.Errorf("Compare(TEXT, INT) error = %v, want ErrType", err)
	}
	if _, err := Compare(Null(), Int(1)); !errors.Is(err, ErrType) {
		t.Errorf("Compare(NULL, INT) error = %v, want ErrType", err)
	}
}

func TestOrderIsTotalAndPutsNullFirst(t *testing.T) {
	if Order(Null(), Int(-9000)) >= 0 {
		t.Error("NULL should order before every integer")
	}
	if Order(Null(), Null()) != 0 {
		t.Error("NULL should order equal to NULL")
	}
	if Order(Int(5), Text("a")) >= 0 {
		t.Error("INT should order before TEXT (kind order)")
	}
	if Order(Bool(true), Int(0)) >= 0 {
		t.Error("BOOL should order before INT (kind order)")
	}
}

func TestOrderPutsNaNLast(t *testing.T) {
	nan := Float(math.NaN())
	if Order(nan, Float(math.Inf(1))) <= 0 {
		t.Error("NaN should order after +Inf")
	}
	if Order(nan, nan) != 0 {
		t.Error("NaN should order equal to itself, to keep the order total")
	}
}

func TestEqualTreatsNullAsEqualToNull(t *testing.T) {
	if !Equal(Null(), Null()) {
		t.Error("Equal(NULL, NULL) = false")
	}
	if Equal(Int(1), Float(1)) {
		t.Error("Equal should distinguish INT 1 from FLOAT 1.0")
	}
	if !Equal(Text("x"), Text("x")) {
		t.Error("Equal(x, x) = false")
	}
}

func TestCoerceWidensIntToFloatOnly(t *testing.T) {
	v, err := Coerce(Int(3), KindFloat)
	if err != nil {
		t.Fatalf("Coerce(INT -> FLOAT): %v", err)
	}
	if v.Kind() != KindFloat || v.AsFloat() != 3 {
		t.Errorf("Coerce gave %v", v)
	}
	if _, err := Coerce(Float(1.5), KindInt); !errors.Is(err, ErrType) {
		t.Errorf("Coerce(FLOAT -> INT) error = %v, want ErrType", err)
	}
	if _, err := Coerce(Text("x"), KindInt); !errors.Is(err, ErrType) {
		t.Errorf("Coerce(TEXT -> INT) error = %v, want ErrType", err)
	}
	if v, err := Coerce(Null(), KindInt); err != nil || !v.IsNull() {
		t.Errorf("Coerce(NULL) = %v, %v; want NULL, nil", v, err)
	}
}

func TestArithmeticKeepsIntegersIntegral(t *testing.T) {
	sum, err := Add(Int(2), Int(3))
	if err != nil || sum.Kind() != KindInt || sum.AsInt() != 5 {
		t.Fatalf("2 + 3 = %v, %v", sum, err)
	}
	quot, err := Div(Int(7), Int(2))
	if err != nil || quot.Kind() != KindInt || quot.AsInt() != 3 {
		t.Fatalf("7 / 2 = %v, %v; want INT 3 (truncating division)", quot, err)
	}
	mixed, err := Mul(Int(2), Float(1.5))
	if err != nil || mixed.Kind() != KindFloat || mixed.AsFloat() != 3 {
		t.Fatalf("2 * 1.5 = %v, %v", mixed, err)
	}
	if _, err := Sub(Text("a"), Int(1)); !errors.Is(err, ErrType) {
		t.Errorf("'a' - 1 error = %v, want ErrType", err)
	}
}

func TestArithmeticPropagatesNull(t *testing.T) {
	for _, op := range []func(a, b Value) (Value, error){Add, Sub, Mul, Div, Mod} {
		v, err := op(Null(), Int(1))
		if err != nil || !v.IsNull() {
			t.Errorf("op(NULL, 1) = %v, %v; want NULL", v, err)
		}
		v, err = op(Int(1), Null())
		if err != nil || !v.IsNull() {
			t.Errorf("op(1, NULL) = %v, %v; want NULL", v, err)
		}
	}
}

func TestDivideByZeroIsAnError(t *testing.T) {
	if _, err := Div(Int(1), Int(0)); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("1 / 0 error = %v, want ErrDivideByZero", err)
	}
	if _, err := Div(Float(1), Float(0)); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("1.0 / 0.0 error = %v, want ErrDivideByZero", err)
	}
	if _, err := Mod(Int(1), Int(0)); !errors.Is(err, ErrDivideByZero) {
		t.Errorf("1 %% 0 error = %v, want ErrDivideByZero", err)
	}
}

func TestNegation(t *testing.T) {
	v, err := Neg(Int(5))
	if err != nil || v.AsInt() != -5 {
		t.Fatalf("-5 = %v, %v", v, err)
	}
	if v, err := Neg(Null()); err != nil || !v.IsNull() {
		t.Fatalf("-NULL = %v, %v", v, err)
	}
	if _, err := Neg(Text("x")); !errors.Is(err, ErrType) {
		t.Errorf("-'x' error = %v, want ErrType", err)
	}
}

func TestLikePatterns(t *testing.T) {
	cases := []struct {
		text, pattern string
		want          bool
	}{
		{"abc", "abc", true},
		{"abc", "ABC", false},
		{"abc", "a%", true},
		{"abc", "%c", true},
		{"abc", "%b%", true},
		{"abc", "a_c", true},
		{"abc", "a_", false},
		{"abc", "_bc", true},
		{"", "%", true},
		{"", "_", false},
		{"anything", "%", true},
		{"aaa", "%a%a%a%b", false},
		{"héllo", "h_llo", true},
		{"a%b", "a%b", true},
	}
	for _, c := range cases {
		if got := Like(c.text, c.pattern); got != c.want {
			t.Errorf("Like(%q, %q) = %v, want %v", c.text, c.pattern, got, c.want)
		}
	}
}

func TestLikeValueIsUnknownForNull(t *testing.T) {
	got, err := LikeValue(Null(), Text("%"))
	if err != nil || got != Unknown {
		t.Errorf("NULL LIKE '%%' = %v, %v; want UNKNOWN", got, err)
	}
	if _, err := LikeValue(Int(1), Text("%")); !errors.Is(err, ErrType) {
		t.Errorf("1 LIKE '%%' error = %v, want ErrType", err)
	}
}

func TestParseKindAcceptsAliases(t *testing.T) {
	cases := map[string]Kind{
		"int": KindInt, "INTEGER": KindInt, "BigInt": KindInt,
		"float": KindFloat, "REAL": KindFloat, "double": KindFloat,
		"text": KindText, "VARCHAR": KindText, "String": KindText,
		"bool": KindBool, "BOOLEAN": KindBool,
	}
	for name, want := range cases {
		got, ok := ParseKind(name)
		if !ok || got != want {
			t.Errorf("ParseKind(%q) = %v, %v; want %v", name, got, ok, want)
		}
	}
	if _, ok := ParseKind("blob"); ok {
		t.Error("ParseKind(blob) should fail")
	}
}

func TestKindMarshalsAsItsName(t *testing.T) {
	blob, err := json.Marshal(struct {
		K Kind `json:"k"`
	}{KindText})
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != `{"k":"TEXT"}` {
		t.Errorf("marshalled to %s", blob)
	}
	var back struct {
		K Kind `json:"k"`
	}
	if err := json.Unmarshal([]byte(`{"k":"FLOAT"}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.K != KindFloat {
		t.Errorf("unmarshalled to %v", back.K)
	}
	if err := json.Unmarshal([]byte(`{"k":"BLOB"}`), &back); err == nil {
		t.Error("unmarshalling an unknown kind should fail")
	}
}
