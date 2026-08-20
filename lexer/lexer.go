package lexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Lexer walks a SQL string one token at a time.
//
// It is a plain hand-rolled scanner: a byte cursor, a switch on the current
// byte, and one small routine per token shape. Nothing about SQL's lexical
// grammar needs more than that, and doing it by hand is what keeps the
// positions exact.
type Lexer struct {
	src  string
	pos  int
	line int
	col  int
}

// New returns a lexer over src.
func New(src string) *Lexer {
	return &Lexer{src: src, line: 1, col: 1}
}

// Tokenize runs the lexer to completion and returns every token, including
// the final EOF token.
func Tokenize(src string) ([]Token, error) {
	l := New(src)
	var out []Token
	for {
		tok, err := l.Next()
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		if tok.Kind == EOF {
			return out, nil
		}
	}
}

// Next returns the next token. At the end of the input it returns an EOF
// token, repeatedly, so a caller may keep asking.
func (l *Lexer) Next() (Token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return Token{}, err
	}
	start := l.mark()
	if l.pos >= len(l.src) {
		return Token{Kind: EOF, Text: "", Pos: start}, nil
	}

	c := l.src[l.pos]
	switch {
	case c == '\'':
		return l.lexString(start)
	case c == '"':
		return l.lexQuotedIdent(start)
	case isDigit(c):
		return l.lexNumber(start)
	case isIdentStart(c):
		return l.lexWord(start)
	}
	return l.lexPunct(start)
}

// ---------------------------------------------------------------------
// scanning primitives
// ---------------------------------------------------------------------

func (l *Lexer) mark() Pos { return Pos{Line: l.line, Column: l.col, Offset: l.pos} }

// advance consumes one byte, tracking line and column. Column counts runes
// rather than bytes so that a caret under a multi-byte character lands in
// the right place.
func (l *Lexer) advance() byte {
	c := l.src[l.pos]
	l.pos++
	switch {
	case c == '\n':
		l.line++
		l.col = 1
	case c < utf8.RuneSelf || c >= 0xC0:
		// A single-byte rune or the first byte of a multi-byte one.
		l.col++
	}
	return c
}

func (l *Lexer) peek() byte {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *Lexer) peekAt(n int) byte {
	if l.pos+n >= len(l.src) {
		return 0
	}
	return l.src[l.pos+n]
}

func (l *Lexer) errorf(pos Pos, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Pos: pos}
}

func (l *Lexer) skipSpaceAndComments() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			l.advance()

		case c == '-' && l.peekAt(1) == '-':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.advance()
			}

		case c == '/' && l.peekAt(1) == '*':
			start := l.mark()
			l.advance()
			l.advance()
			closed := false
			for l.pos < len(l.src) {
				if l.src[l.pos] == '*' && l.peekAt(1) == '/' {
					l.advance()
					l.advance()
					closed = true
					break
				}
				l.advance()
			}
			if !closed {
				return l.errorf(start, "unterminated block comment")
			}

		default:
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// token shapes
// ---------------------------------------------------------------------

// lexString reads a single-quoted literal. Two escape conventions are
// accepted, because both are common and neither is ambiguous: the SQL
// standard's doubled quote ('it”s') and C-style backslash escapes
// ('it\'s', '\n', '\t', '\\'). A backslash before anything else is a
// literal backslash.
func (l *Lexer) lexString(start Pos) (Token, error) {
	l.advance() // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.advance()
		switch c {
		case '\'':
			if l.peek() == '\'' {
				l.advance()
				sb.WriteByte('\'')
				continue
			}
			return Token{Kind: String, Text: sb.String(), Pos: start}, nil

		case '\\':
			if l.pos >= len(l.src) {
				return Token{}, l.errorf(start, "unterminated string literal")
			}
			e := l.advance()
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '0':
				sb.WriteByte(0)
			case '\\', '\'', '"':
				sb.WriteByte(e)
			default:
				sb.WriteByte('\\')
				sb.WriteByte(e)
			}

		default:
			sb.WriteByte(c)
		}
	}
	return Token{}, l.errorf(start, "unterminated string literal")
}

// lexQuotedIdent reads a "double quoted" identifier, whose spelling is
// preserved exactly. A doubled quote inside stands for one quote.
func (l *Lexer) lexQuotedIdent(start Pos) (Token, error) {
	l.advance()
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.advance()
		if c != '"' {
			sb.WriteByte(c)
			continue
		}
		if l.peek() == '"' {
			l.advance()
			sb.WriteByte('"')
			continue
		}
		if sb.Len() == 0 {
			return Token{}, l.errorf(start, "empty quoted identifier")
		}
		return Token{Kind: Ident, Text: sb.String(), Pos: start}, nil
	}
	return Token{}, l.errorf(start, "unterminated quoted identifier")
}

// lexNumber reads an integer or a float. A float needs a leading digit:
// keelsql spells one half as 0.5, never .5, so that '.' is unambiguously a
// qualified-name separator.
func (l *Lexer) lexNumber(start Pos) (Token, error) {
	kind := Int
	for isDigit(l.peek()) {
		l.advance()
	}
	if l.peek() == '.' && isDigit(l.peekAt(1)) {
		kind = Float
		l.advance()
		for isDigit(l.peek()) {
			l.advance()
		}
	}
	if c := l.peek(); c == 'e' || c == 'E' {
		next, after := l.peekAt(1), l.peekAt(2)
		if isDigit(next) || ((next == '+' || next == '-') && isDigit(after)) {
			kind = Float
			l.advance()
			l.advance()
			for isDigit(l.peek()) {
				l.advance()
			}
		}
	}
	text := l.src[start.Offset:l.pos]
	if isIdentStart(l.peek()) {
		return Token{}, l.errorf(start, "invalid number %q", text+string(l.peek()))
	}
	return Token{Kind: kind, Text: text, Pos: start}, nil
}

// lexWord reads a bare word and decides whether it is a keyword or an
// identifier. Keywords are upper-cased and identifiers are lower-cased, so
// that both compare case-insensitively from here on; a quoted identifier is
// the way to keep an unusual spelling.
func (l *Lexer) lexWord(start Pos) (Token, error) {
	for isIdentPart(l.peek()) {
		l.advance()
	}
	word := l.src[start.Offset:l.pos]
	if upper := strings.ToUpper(word); keywords[upper] {
		return Token{Kind: Keyword, Text: upper, Pos: start}, nil
	}
	return Token{Kind: Ident, Text: strings.ToLower(word), Pos: start}, nil
}

// operators lists the multi-byte operators, longest first so that the
// scanner never splits '<=' into '<' and '='.
var operators = []string{"<=", ">=", "<>", "!=", "==", "||"}

func (l *Lexer) lexPunct(start Pos) (Token, error) {
	for _, op := range operators {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.advance()
			l.advance()
			return Token{Kind: Punct, Text: op, Pos: start}, nil
		}
	}
	c := l.advance()
	switch c {
	case '=', '<', '>', '+', '-', '*', '/', '%', '(', ')', ',', ';', '.':
		return Token{Kind: Punct, Text: string(c), Pos: start}, nil
	}
	r, _ := utf8.DecodeRuneInString(l.src[start.Offset:])
	if unicode.IsPrint(r) {
		return Token{}, l.errorf(start, "unexpected character %q", r)
	}
	return Token{}, l.errorf(start, "unexpected byte 0x%02x", c)
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || isAlpha(c) || c >= utf8.RuneSelf }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }
func isAlpha(c byte) bool      { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
