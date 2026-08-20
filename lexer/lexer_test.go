package lexer

import (
	"strings"
	"testing"
)

// kinds strips the EOF token and renders the rest as "Kind(text)" strings,
// which makes an expected token stream readable in a table.
func kinds(t *testing.T, src string) []string {
	t.Helper()
	tokens, err := Tokenize(src)
	if err != nil {
		t.Fatalf("Tokenize(%q): %v", src, err)
	}
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens[:len(tokens)-1] {
		out = append(out, tok.String())
	}
	return out
}

func TestKeywordsAreCaseInsensitiveAndUpperCased(t *testing.T) {
	got := kinds(t, "SeLeCt * FrOm users")
	want := []string{"keyword(SELECT)", "operator(*)", "keyword(FROM)", "identifier(users)"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestIdentifiersAreLowerCasedUnlessQuoted(t *testing.T) {
	got := kinds(t, `MixedCase "MixedCase" "with space"`)
	want := []string{`identifier(mixedcase)`, `identifier(MixedCase)`, `identifier(with space)`}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNumbers(t *testing.T) {
	cases := []struct{ src, want string }{
		{"0", "integer(0)"},
		{"42", "integer(42)"},
		{"1.5", "float(1.5)"},
		{"1e3", "float(1e3)"},
		{"1.5E-3", "float(1.5E-3)"},
		{"1e+3", "float(1e+3)"},
		{"10.", "integer(10) operator(.)"}, // a trailing dot is not part of the number
	}
	for _, c := range cases {
		if got := strings.Join(kinds(t, c.src), " "); got != c.want {
			t.Errorf("Tokenize(%q) = %s, want %s", c.src, got, c.want)
		}
	}
}

func TestStringEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`'plain'`, "plain"},
		{`'it''s'`, "it's"},
		{`'it\'s'`, "it's"},
		{`'a\nb'`, "a\nb"},
		{`'a\tb'`, "a\tb"},
		{`'a\\b'`, `a\b`},
		{`'a\qb'`, `a\qb`}, // an unknown escape keeps the backslash
		{`''`, ""},
		{`'%_'`, "%_"},
	}
	for _, c := range cases {
		tokens, err := Tokenize(c.src)
		if err != nil {
			t.Fatalf("Tokenize(%s): %v", c.src, err)
		}
		if tokens[0].Kind != String {
			t.Fatalf("Tokenize(%s) gave %v", c.src, tokens[0].Kind)
		}
		if tokens[0].Text != c.want {
			t.Errorf("Tokenize(%s) = %q, want %q", c.src, tokens[0].Text, c.want)
		}
	}
}

func TestOperatorsAreScannedLongestFirst(t *testing.T) {
	got := strings.Join(kinds(t, "<= >= <> != == < > = + - * / % ( ) , ; ."), " ")
	want := "operator(<=) operator(>=) operator(<>) operator(!=) operator(==) " +
		"operator(<) operator(>) operator(=) operator(+) operator(-) operator(*) " +
		"operator(/) operator(%) operator(() operator()) operator(,) operator(;) operator(.)"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestComments(t *testing.T) {
	got := strings.Join(kinds(t, "SELECT -- a line comment\n1 /* a block\ncomment */ + 2"), " ")
	want := "keyword(SELECT) integer(1) operator(+) integer(2)"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestPositionsTrackLinesAndColumns(t *testing.T) {
	tokens, err := Tokenize("SELECT a\nFROM t")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		index      int
		line, col  int
		offsetText string
	}{
		{0, 1, 1, "SELECT"},
		{1, 1, 8, "a"},
		{2, 2, 1, "FROM"},
		{3, 2, 6, "t"},
	}
	for _, c := range cases {
		tok := tokens[c.index]
		if tok.Pos.Line != c.line || tok.Pos.Column != c.col {
			t.Errorf("%s at %s, want line %d, column %d", c.offsetText, tok.Pos, c.line, c.col)
		}
	}
}

func TestColumnsCountRunesNotBytes(t *testing.T) {
	tokens, err := Tokenize("SELECT 'héllo', x")
	if err != nil {
		t.Fatal(err)
	}
	// 'héllo' is 7 runes but 8 bytes; the comma after it is at column 15.
	comma := tokens[2]
	if !comma.Is(",") {
		t.Fatalf("token 2 is %v", comma)
	}
	if comma.Pos.Column != 15 {
		t.Errorf("comma at column %d, want 15", comma.Pos.Column)
	}
}

func TestLexicalErrors(t *testing.T) {
	cases := []struct{ src, contains string }{
		{"'unterminated", "unterminated string literal"},
		{`"unterminated`, "unterminated quoted identifier"},
		{`""`, "empty quoted identifier"},
		{"/* never closed", "unterminated block comment"},
		{"SELECT #", "unexpected character"},
		{"12abc", "invalid number"},
	}
	for _, c := range cases {
		_, err := Tokenize(c.src)
		if err == nil {
			t.Errorf("Tokenize(%q) succeeded, want an error", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Errorf("Tokenize(%q) error = %q, want it to mention %q", c.src, err, c.contains)
		}
		var lexErr *Error
		if !asLexError(err, &lexErr) {
			t.Errorf("Tokenize(%q) returned %T, want *lexer.Error", c.src, err)
		}
	}
}

func asLexError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestEOFRepeats(t *testing.T) {
	l := New("")
	for i := 0; i < 3; i++ {
		tok, err := l.Next()
		if err != nil {
			t.Fatal(err)
		}
		if tok.Kind != EOF {
			t.Fatalf("call %d gave %v, want EOF", i, tok)
		}
	}
}

func TestIsKeyword(t *testing.T) {
	if !IsKeyword("select") || !IsKeyword("SELECT") {
		t.Error("SELECT should be a keyword in any case")
	}
	// Type names and aggregate names are deliberately not reserved, so a
	// column may still be called "count" or "text".
	for _, word := range []string{"int", "text", "count", "sum", "avg", "min", "max"} {
		if IsKeyword(word) {
			t.Errorf("%q should not be reserved", word)
		}
	}
}

func TestTokenDescribe(t *testing.T) {
	tokens, err := Tokenize("SELECT a 'x' 1 ,")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`keyword SELECT`, `identifier "a"`, `string "x"`, `"1"`, `","`, "end of input"}
	for i, tok := range tokens {
		if got := tok.Describe(); got != want[i] {
			t.Errorf("token %d Describe() = %s, want %s", i, got, want[i])
		}
	}
}
